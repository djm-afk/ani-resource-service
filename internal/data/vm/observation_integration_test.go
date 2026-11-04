//go:build instanceintegration

package vm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	migrations "github.com/zhangzhe-ctrl/ani-resource-service/migrations/vm"
	"github.com/zhangzhe-ctrl/ani-resource-service/tests/testenv"
)

type observationRead func(context.Context, biz.VM) (biz.Observation, error)

func (f observationRead) ObserveState(ctx context.Context, v biz.VM) (biz.Observation, error) {
	return f(ctx, v)
}

func observationRepository(t *testing.T) (*Postgres, *testenv.Database, biz.Acceptance) {
	t.Helper()
	ctx := context.Background()
	db := testenv.NewDatabase(t)
	if err := Migrate(ctx, db.Owner.Config().ConnString(), db.RuntimeRole); err != nil {
		t.Fatal(err)
	}
	p, err := OpenPostgres(ctx, db.RuntimeDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	a := biz.Acceptance{TenantID: uuid.NewString(), VMID: uuid.NewString(), OperationID: uuid.NewString(), RequestDigest: stringDigest('a'), Spec: biz.Spec{Canonical: []byte(`{"observation":"native-boundary"}`), Digest: stringDigest('b'), Name: "observation", Namespace: "tenant-observation", StartAfterCreate: true}}
	return p, db, a
}

func observationRegisteredVM(t *testing.T) (*Postgres, *testenv.Database, biz.VM) {
	t.Helper()
	p, db, a := observationRepository(t)
	ctx := context.Background()
	if _, err := p.AcceptCreate(ctx, a); err != nil {
		t.Fatal(err)
	}
	o, v, err := p.Claim(ctx, uuid.NewString(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	v.Native.UID = uuid.NewString()
	v.Observed, v.ObservedAt = "Running", time.Now().Add(-time.Second).UTC()
	v.Network = biz.NetworkPlan{AttachmentID: uuid.NewString(), SnapshotRevision: 1, Consumer: &biz.NetworkConsumerState{State: "open", DesiredRunState: "running"}}
	v.Addresses = []biz.Address{{Address: "10.1.2.3"}}
	o.State = "succeeded"
	if err = p.Save(ctx, o, v); err != nil {
		t.Fatal(err)
	}
	return p, db, v
}

func stoppedObservation(v biz.VM) biz.Observation {
	return biz.Observation{VM: v.Native, Stopped: true, State: "Stopped", ObservedAt: time.Now().UTC()}
}

func TestVMIndependentObservationAfterSuccessfulStart(t *testing.T) {
	p, _, a := observationRepository(t)
	ctx := context.Background()
	if _, err := p.AcceptCreate(ctx, a); err != nil {
		t.Fatal(err)
	}
	// Only KubeVirt and Network boundaries are replaced. The actual create,
	// checkpoints, power worker and PostgreSQL operation run to completion.
	native := &powerVMNative{ref: biz.ObjectRef{Kind: "VirtualMachine", Namespace: a.Spec.Namespace, Name: "vm-" + a.VMID, UID: uuid.NewString()}, starts: 1}
	worker, err := biz.NewWorker(p, native, powerVMNetwork{})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(7 * time.Second)
	for {
		if _, err = worker.Step(ctx); err != nil {
			t.Fatal(err)
		}
		o, err := p.GetOperation(ctx, a.TenantID, a.VMID, a.OperationID)
		if err != nil {
			t.Fatal(err)
		}
		if o.State == "succeeded" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("create/start did not complete: %+v", o)
		}
		time.Sleep(20 * time.Millisecond)
	}
	before, err := p.Get(ctx, a.TenantID, a.VMID)
	if err != nil || before.Observed != "Running" {
		t.Fatalf("start projection: %+v %v", before, err)
	}
	native.vmi = "" // guest shutdown, with no platform command or open page
	observer, err := biz.NewObserver(p, observationRead(func(_ context.Context, v biz.VM) (biz.Observation, error) {
		return stoppedObservation(v), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if worked, err := observer.Step(ctx); err != nil || !worked {
		t.Fatalf("idle VM was not independently observed: worked=%v err=%v", worked, err)
	}
	after, err := p.Get(ctx, a.TenantID, a.VMID)
	if err != nil || after.Observed != "Stopped" || !after.ObservedAt.After(before.ObservedAt) || after.Version != before.Version || after.Desired != "Running" || after.LastOperationID != before.LastOperationID || after.Network.AttachmentID != before.Network.AttachmentID {
		t.Fatalf("guest shutdown did not update only facts: %+v %v", after, err)
	}
	if o, err := p.GetOperation(ctx, a.TenantID, a.VMID, a.OperationID); err != nil || o.State != "succeeded" {
		t.Fatalf("observer modified completed operation: %+v %v", o, err)
	}
	if worked, err := worker.Step(ctx); err != nil || worked {
		t.Fatalf("guest shutdown replayed old start: %v %v", worked, err)
	}
	items, total, err := p.List(ctx, a.TenantID, biz.ListFilter{State: "stopped"})
	if err != nil || total != 1 || len(items) != 1 || items[0].ID != a.VMID || items[0].Desired != "Running" {
		t.Fatalf("guest shutdown disappeared from stopped filter: total=%d items=%+v err=%v", total, items, err)
	}
}

func TestVMObservationPreservesConcurrentNetworkAndNewerOperationFacts(t *testing.T) {
	p, _, v := observationRegisteredVM(t)
	ctx := context.Background()
	w, snapshot, err := p.ClaimObservation(ctx, uuid.NewString(), 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	n := snapshot.Network
	n.SnapshotRevision++
	n.Consumer = &biz.NetworkConsumerState{State: "open", DesiredRunState: "running"}
	if err = p.UpdateNetwork(ctx, snapshot, n); err != nil {
		t.Fatal(err)
	}
	if err = p.SaveObservation(ctx, w, stoppedObservation(snapshot), nil); err != nil {
		t.Fatal(err)
	}
	latest, err := p.Get(ctx, v.TenantID, v.ID)
	if err != nil || latest.Network.SnapshotRevision != 2 || latest.Network.Consumer == nil || latest.Network.Consumer.State != "open" || latest.Observed != "Stopped" || len(latest.Addresses) != 0 || latest.Version != v.Version {
		t.Fatalf("observation replaced concurrent network: %+v %v", latest, err)
	}
	// A user operation claims older facts, then an independent observer sees a
	// later guest shutdown. Saving the operation must preserve that newer fact.
	command, err := p.AcceptCommand(ctx, biz.Command{TenantID: v.TenantID, VMID: v.ID, Action: "start", Key: "after-observation", ExpectedVersion: latest.Version, RequestDigest: stringDigest('c'), Intent: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	op, old, err := p.Claim(ctx, uuid.NewString(), time.Minute)
	if err != nil || op.ID != command.ID {
		t.Fatalf("operation claim: %+v %v", op, err)
	}
	// Make recovery due without sleeping; this changes only the scheduler.
	if _, err = p.pool.Exec(ctx, "UPDATE vm.observation_work SET not_before=now(),next_attempt_at=now() WHERE tenant_id=$1 AND vm_id=$2", v.TenantID, v.ID); err != nil {
		t.Fatal(err)
	}
	w, _, err = p.ClaimObservation(ctx, uuid.NewString(), 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.SaveObservation(ctx, w, stoppedObservation(old), nil); err != nil {
		t.Fatal(err)
	}
	op.State = "succeeded"
	old.Observed = "Running"
	if err = p.Save(ctx, op, old); err != nil {
		t.Fatal(err)
	}
	latest, err = p.Get(ctx, v.TenantID, v.ID)
	if err != nil || latest.Observed != "Stopped" || latest.Version != old.Version || latest.LastOperationID != command.ID {
		t.Fatalf("old operation overwrote newer observation: %+v %v", latest, err)
	}
}

func TestVMObservationFencesIntentIdentityAndNotifications(t *testing.T) {
	for _, mutation := range []string{"version", "native-uid", "closing", "notification", "lease"} {
		t.Run(mutation, func(t *testing.T) {
			p, _, v := observationRegisteredVM(t)
			ctx := context.Background()
			w, snapshot, err := p.ClaimObservation(ctx, uuid.NewString(), 15*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			switch mutation {
			case "version":
				_, err = p.AcceptCommand(ctx, biz.Command{TenantID: v.TenantID, VMID: v.ID, Action: "start", Key: "new-intent", ExpectedVersion: v.Version, RequestDigest: stringDigest('c'), Intent: []byte(`{}`)})
			case "native-uid":
				_, err = p.pool.Exec(ctx, `UPDATE vm.virtual_machines SET current_configuration=jsonb_set(current_configuration,'{Native,UID}',$3::jsonb) WHERE tenant_id=$1 AND id=$2`, v.TenantID, v.ID, encode(uuid.NewString()))
			case "closing":
				_, err = p.AcceptDelete(ctx, biz.Acceptance{TenantID: v.TenantID, VMID: v.ID, OperationID: uuid.NewString(), CreateOperationID: v.CreateOperationID, ExpectedVersion: v.Version, RequestDigest: stringDigest('d')})
			case "notification":
				err = p.NotifyObservation(ctx, v.Native)
			case "lease":
				_, err = p.pool.Exec(ctx, `UPDATE vm.observation_work SET lease_until=now()-interval '1 second' WHERE tenant_id=$1 AND vm_id=$2`, v.TenantID, v.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			err = p.SaveObservation(ctx, w, stoppedObservation(snapshot), nil)
			if !errors.Is(err, biz.ErrVersion) && !errors.Is(err, biz.ErrLease) {
				t.Fatalf("late observation accepted: %v", err)
			}
			current, err := p.Get(ctx, v.TenantID, v.ID)
			if err != nil || current.Observed != "Running" || !current.ObservedAt.Equal(v.ObservedAt) {
				t.Fatalf("fenced observation changed facts: %+v %v", current, err)
			}
		})
	}
}

func TestVMObservationFailureBackoffCannotBeBypassedByEvents(t *testing.T) {
	p, _, v := observationRegisteredVM(t)
	ctx := context.Background()
	w, _, err := p.ClaimObservation(ctx, uuid.NewString(), 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.SaveObservation(ctx, w, biz.Observation{}, context.DeadlineExceeded); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		if err = p.NotifyObservation(ctx, v.Native); err != nil {
			t.Fatal(err)
		}
	}
	if err = p.NotifyAllObservations(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err = p.ClaimObservation(ctx, uuid.NewString(), 15*time.Second); !errors.Is(err, biz.ErrNotFound) {
		t.Fatalf("watch storm bypassed durable backoff: %v", err)
	}
	current, err := p.Get(ctx, v.TenantID, v.ID)
	if err != nil || current.Observed != v.Observed || current.Reason != v.Reason || !current.ObservedAt.Equal(v.ObservedAt) || len(current.Addresses) != 1 {
		t.Fatalf("temporary failure replaced still-fresh facts: %+v %v", current, err)
	}
	var wait time.Duration
	var failures int
	if err = p.pool.QueryRow(ctx, `SELECT (EXTRACT(EPOCH FROM not_before-now())*1000000000)::bigint,failures FROM vm.observation_work WHERE tenant_id=$1 AND vm_id=$2`, v.TenantID, v.ID).Scan(&wait, &failures); err != nil || wait < 25*time.Second || failures != 1 {
		t.Fatalf("backoff lost: %v failures=%d err=%v", wait, failures, err)
	}
}

func TestVMObservationFailureSurvivesOlderOperationSave(t *testing.T) {
	p, _, v := observationRegisteredVM(t)
	ctx := context.Background()
	accepted, err := p.AcceptCommand(ctx, biz.Command{TenantID: v.TenantID, VMID: v.ID, Action: "start", Key: "observation-failure-race", ExpectedVersion: v.Version, RequestDigest: stringDigest('f'), Intent: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	op, old, err := p.Claim(ctx, uuid.NewString(), time.Minute)
	if err != nil || op.ID != accepted.ID {
		t.Fatalf("operation claim: %+v %v", op, err)
	}
	work, _, err := p.ClaimObservation(ctx, uuid.NewString(), 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.SaveObservation(ctx, work, biz.Observation{}, biz.ErrUnknown); err != nil {
		t.Fatal(err)
	}
	// This checkpoint has no new native observation. Its captured Running
	// projection must not overwrite a newer failure with the same evidence time.
	if err = p.Checkpoint(ctx, op, old); err != nil {
		t.Fatal(err)
	}
	current, err := p.Get(ctx, v.TenantID, v.ID)
	if err != nil || current.Observed != "Unknown" || current.Reason != "VM_OBSERVATION_UNAVAILABLE" || !current.ObservedAt.Equal(v.ObservedAt) || len(current.Addresses) != 0 {
		t.Fatalf("old operation revived unavailable observation: %+v %v", current, err)
	}
	// Real newer operation evidence still wins immediately; the observer's
	// failure/backoff does not become a new operation or state-update gate.
	old.Observed, old.Reason, old.ObservedAt = "Running", "", time.Now().UTC()
	op.State = "succeeded"
	if err = p.Save(ctx, op, old); err != nil {
		t.Fatal(err)
	}
	current, err = p.Get(ctx, v.TenantID, v.ID)
	if err != nil || current.Observed != "Running" || !current.ObservedAt.Equal(old.ObservedAt) {
		t.Fatalf("failure blocked newer operation evidence: %+v %v", current, err)
	}
}

func TestVMBlockedObservationDoesNotBlockCommandOrOperationWorker(t *testing.T) {
	p, _, v := observationRegisteredVM(t)
	ctx := context.Background()
	entered, release := make(chan struct{}), make(chan struct{})
	observer, err := biz.NewObserver(p, observationRead(func(ctx context.Context, _ biz.VM) (biz.Observation, error) {
		close(entered)
		select {
		case <-release:
			return biz.Observation{}, biz.ErrDependency
		case <-ctx.Done():
			return biz.Observation{}, ctx.Err()
		}
	}))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := observer.Step(ctx); done <- err }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("observation source not entered")
	}
	defer close(release)
	start := time.Now()
	command, err := p.AcceptCommand(ctx, biz.Command{TenantID: v.TenantID, VMID: v.ID, Action: "start", Key: "during-slow-observer", ExpectedVersion: v.Version, RequestDigest: stringDigest('e'), Intent: []byte(`{}`)})
	if err != nil {
		t.Fatalf("observer blocked acceptance: %v", err)
	}
	native := &powerVMNative{ref: v.Native, vmi: uuid.NewString()}
	worker, err := biz.NewWorker(p, native, powerVMNetwork{})
	if err != nil {
		t.Fatal(err)
	}
	if worked, err := worker.Step(ctx); err != nil || !worked {
		t.Fatalf("observer blocked operation worker: %v %v", worked, err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("read-only observation delayed independent operation: %v", time.Since(start))
	}
	if saved, err := p.GetOperation(ctx, v.TenantID, v.ID, command.ID); err != nil || saved.State != "succeeded" {
		t.Fatalf("operation did not finish while observer blocked: %+v %v", saved, err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("observer did not respect two-second source deadline")
	}
}

func TestVMObservationMigrationBackfillsExistingRegistrations(t *testing.T) {
	ctx := context.Background()
	db := testenv.NewDatabase(t)
	for i, name := range []string{"001_vm.sql", "002_guest_initialization.sql"} {
		body, err := migrations.Files.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = db.Owner.Exec(ctx, string(body)); err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(body)
		if _, err = db.Owner.Exec(ctx, "INSERT INTO vm.schema_version(version,checksum) VALUES($1,$2)", i+1, hex.EncodeToString(hash[:])); err != nil {
			t.Fatal(err)
		}
	}
	v := biz.VM{TenantID: uuid.NewString(), ID: uuid.NewString(), CreateOperationID: uuid.NewString(), Version: 1, Native: biz.ObjectRef{Kind: "VirtualMachine", Namespace: "tenant-existing", Name: "existing", UID: uuid.NewString()}, Desired: "Running", Observed: "Running", ObservedAt: time.Now().Add(-time.Minute).UTC()}
	v.Spec = biz.Spec{Canonical: []byte(`{}`), Digest: stringDigest('b')}
	if _, err := db.Owner.Exec(ctx, `INSERT INTO vm.virtual_machines(tenant_id,id,create_operation_id,request_digest,spec_digest,initial_spec,current_configuration,version) VALUES($1,$2,$3,$4,$5,$6,$7,1)`, v.TenantID, v.ID, v.CreateOperationID, stringDigest('a'), v.Spec.Digest, encode(v.Spec), encode(v)); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, db.Owner.Config().ConnString(), db.RuntimeRole); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, db.Owner.Config().ConnString(), db.RuntimeRole); err != nil {
		t.Fatalf("explicit migration replay changed checksum: %v", err)
	}
	p, err := OpenPostgres(ctx, db.RuntimeDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	work, snapshot, err := p.ClaimObservation(ctx, uuid.NewString(), 15*time.Second)
	if err != nil || work.VMID != v.ID || snapshot.Native != v.Native {
		t.Fatalf("existing registration was not backfilled: %+v %+v %v", work, snapshot, err)
	}
	if err = p.SaveObservation(ctx, work, stoppedObservation(snapshot), nil); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = p.pool.QueryRow(ctx, "SELECT count(*) FROM vm.observation_work WHERE tenant_id=$1 AND vm_id=$2", v.TenantID, v.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("migration duplicated observation work: %d %v", count, err)
	}
}

func TestVMObservationEvidenceTimeAndExpiredLeaseRecovery(t *testing.T) {
	p, _, v := observationRegisteredVM(t)
	ctx := context.Background()
	work, snapshot, err := p.ClaimObservation(ctx, uuid.NewString(), 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.pool.Exec(ctx, "UPDATE vm.observation_work SET lease_until=now()-interval '1 second' WHERE tenant_id=$1 AND vm_id=$2", v.TenantID, v.ID); err != nil {
		t.Fatal(err)
	}
	reclaimed, _, err := p.ClaimObservation(ctx, uuid.NewString(), 15*time.Second)
	if err != nil || reclaimed.Lease == work.Lease {
		t.Fatalf("crashed observation did not recover independently: %+v %v", reclaimed, err)
	}
	if err = p.SaveObservation(ctx, work, stoppedObservation(snapshot), nil); !errors.Is(err, biz.ErrLease) {
		t.Fatalf("expired observer overwrote its successor: %v", err)
	}
	old := stoppedObservation(snapshot)
	old.ObservedAt = v.ObservedAt.Add(-time.Second)
	if err = p.SaveObservation(ctx, reclaimed, old, nil); err != nil {
		t.Fatal(err)
	}
	current, err := p.Get(ctx, v.TenantID, v.ID)
	if err != nil || current.Observed != "Running" || !current.ObservedAt.Equal(v.ObservedAt) || len(current.Addresses) != 1 {
		t.Fatalf("old cache result renewed or replaced newer facts: %+v %v", current, err)
	}
	if _, err = p.pool.Exec(ctx, "UPDATE vm.observation_work SET not_before=now(),next_attempt_at=now() WHERE tenant_id=$1 AND vm_id=$2", v.TenantID, v.ID); err != nil {
		t.Fatal(err)
	}
	work, _, err = p.ClaimObservation(ctx, uuid.NewString(), 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	old.ObservedAt = time.Now().Add(-2 * time.Minute)
	if err = p.SaveObservation(ctx, work, old, nil); err != nil {
		t.Fatal(err)
	}
	current, err = p.Get(ctx, v.TenantID, v.ID)
	if err != nil || current.Observed != "Unknown" || !current.ObservedAt.Equal(v.ObservedAt) {
		t.Fatalf("stale cache produced fresh evidence: %+v %v", current, err)
	}
}
