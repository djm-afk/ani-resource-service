//go:build instanceintegration

package vm

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	"github.com/zhangzhe-ctrl/ani-resource-service/tests/testenv"
)

// cloneAdmissionSource creates one ordinary managed VM and returns the facts a
// clone target must freeze. Operations left behind by earlier cases are pushed
// out of the claim window first, so Claim always returns the create this case
// just accepted. The create is driven to a terminal state and the source counts
// as free from that point on.
func cloneAdmissionSource(t *testing.T, ctx context.Context, db *testenv.Database, p *Postgres) biz.VM {
	t.Helper()
	if _, e := db.Owner.Exec(ctx, "UPDATE vm.operations SET next_attempt_at=now()+interval '1 day' WHERE state IN ('accepted','running','unknown')"); e != nil {
		t.Fatal(e)
	}
	tenant, id, op := uuid.NewString(), uuid.NewString(), uuid.NewString()
	spec := biz.Spec{Canonical: []byte(`{"fixture":"clone-source"}`), Digest: stringDigest('b'), Name: "source",
		Namespace: "tenant-a", NamespaceUID: uuid.NewString(),
		InitialDisks: []biz.Disk{biz.NewInitialDisk(id, "system", "system", "system", 8)}}
	if _, e := p.AcceptCreate(ctx, biz.Acceptance{TenantID: tenant, VMID: id, OperationID: op, RequestDigest: stringDigest('a'), Spec: spec}); e != nil {
		t.Fatal(e)
	}
	create, v, e := p.Claim(ctx, uuid.NewString(), time.Minute)
	if e != nil {
		t.Fatal(e)
	}
	if create.ID != op {
		t.Fatalf("claimed %s instead of the accepted create %s", create.ID, op)
	}
	v.Native = biz.ObjectRef{Kind: "VirtualMachine", Namespace: "tenant-a", Name: "vm-source", UID: "source-uid"}
	create.State = "succeeded"
	if e = p.Save(ctx, create, v); e != nil {
		t.Fatal(e)
	}
	return v
}

// cloneTargetAcceptance builds the canonical acceptance of a clone target.
func cloneTargetAcceptance(source biz.VM) biz.Acceptance {
	id := uuid.NewString()
	return biz.Acceptance{
		TenantID: source.TenantID, VMID: id, OperationID: uuid.NewString(), RequestDigest: stringDigest('c'),
		Spec: biz.Spec{
			Canonical: []byte(`{"fixture":"clone-target"}`), Digest: stringDigest('d'), Name: "target",
			Namespace: "tenant-a", NamespaceUID: uuid.NewString(),
			InitialDisks: []biz.Disk{biz.NewInitialDisk(id, "system", "system", "system", 8)},
			CloneSource: &biz.CloneSource{
				SourceVMID: source.ID, SourceName: source.Name, SourceVersion: source.Version,
				Native: source.Native,
				Disks:  []biz.CloneDisk{{SourceDiskID: source.Disks[0].ID, VolumeName: source.Disks[0].VolumeName, Role: "system", Name: "system", SizeGB: 8}},
			},
		},
	}
}

// TestCloneAdmissionRejectsAnyNonTerminalSourceOperation covers R6: the source
// mutex is the same business non-terminal rule as an ordinary command. A worker
// lease only schedules execution, and Save clears the SQL lease column at the end
// of every tick while the operation is still running, so neither an empty nor an
// expired lease may open the source.
func TestCloneAdmissionRejectsAnyNonTerminalSourceOperation(t *testing.T) {
	ctx := context.Background()
	db := testenv.NewDatabase(t)
	if e := Migrate(ctx, db.Owner.Config().ConnString(), db.RuntimeRole); e != nil {
		t.Fatal(e)
	}
	p, e := OpenPostgres(ctx, db.RuntimeDSN)
	if e != nil {
		t.Fatal(e)
	}
	defer p.Close()

	for _, test := range []struct {
		name  string
		state string
		lease string // SQL expression for lease_until; empty means NULL
	}{
		{name: "accepted", state: "accepted"},
		{name: "running without lease", state: "running"},
		{name: "running with expired lease", state: "running", lease: "now()-interval '1 second'"},
		{name: "running with live lease", state: "running", lease: "now()+interval '1 minute'"},
		{name: "unknown without lease", state: "unknown"},
		{name: "unknown with live lease", state: "unknown", lease: "now()+interval '1 minute'"},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := cloneAdmissionSource(t, ctx, db, p)
			op, e := p.AcceptCommand(ctx, biz.Command{TenantID: source.TenantID, VMID: source.ID, Action: "start", Key: "source-change", RequestDigest: stringDigest('e'), Intent: []byte(`{"expected_version":1}`), ExpectedVersion: source.Version})
			if e != nil {
				t.Fatal(e)
			}
			lease := "NULL"
			if test.lease != "" {
				lease = test.lease
			}
			if _, e = db.Owner.Exec(ctx, "UPDATE vm.operations SET state=$3,lease_until="+lease+" WHERE tenant_id=$1 AND id=$2", source.TenantID, op.ID, test.state); e != nil {
				t.Fatal(e)
			}
			// AcceptCommand bumped the business version, so the clone must freeze
			// the current one or it would fail on version instead of the mutex.
			current, e := p.Get(ctx, source.TenantID, source.ID)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = p.AcceptCreate(ctx, cloneTargetAcceptance(current)); !errors.Is(e, biz.ErrBusy) {
				t.Fatalf("clone admitted while the source change was %s (%s): %v", test.state, test.name, e)
			}
		})
	}

	t.Run("terminal source operation admits", func(t *testing.T) {
		source := cloneAdmissionSource(t, ctx, db, p)
		op, e := p.AcceptCommand(ctx, biz.Command{TenantID: source.TenantID, VMID: source.ID, Action: "start", Key: "source-change", RequestDigest: stringDigest('e'), Intent: []byte(`{"expected_version":1}`), ExpectedVersion: source.Version})
		if e != nil {
			t.Fatal(e)
		}
		if _, e = db.Owner.Exec(ctx, "UPDATE vm.operations SET state='succeeded',lease_until=NULL WHERE tenant_id=$1 AND id=$2", source.TenantID, op.ID); e != nil {
			t.Fatal(e)
		}
		current, e := p.Get(ctx, source.TenantID, source.ID)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = p.AcceptCreate(ctx, cloneTargetAcceptance(current)); e != nil {
			t.Fatalf("terminal source change blocked the clone: %v", e)
		}
		held, e := p.SourceHeld(ctx, source.TenantID, source.ID)
		if e != nil || !held {
			t.Fatalf("clone did not take HELD: %v %v", held, e)
		}
	})
}

// TestCloneAdmissionKeepsOneHeldPerSource keeps the single-HELD guarantee under
// two concurrent acceptances of the same free source.
func TestCloneAdmissionKeepsOneHeldPerSource(t *testing.T) {
	ctx := context.Background()
	db := testenv.NewDatabase(t)
	if e := Migrate(ctx, db.Owner.Config().ConnString(), db.RuntimeRole); e != nil {
		t.Fatal(e)
	}
	p, e := OpenPostgres(ctx, db.RuntimeDSN)
	if e != nil {
		t.Fatal(e)
	}
	defer p.Close()
	source := cloneAdmissionSource(t, ctx, db, p)
	const count = 4
	var wg sync.WaitGroup
	results := make(chan error, count)
	wg.Add(count)
	for i := 0; i < count; i++ {
		a := cloneTargetAcceptance(source)
		go func() {
			defer wg.Done()
			_, e := p.AcceptCreate(ctx, a)
			results <- e
		}()
	}
	wg.Wait()
	close(results)
	admitted := 0
	for e := range results {
		if e == nil {
			admitted++
			continue
		}
		if !errors.Is(e, biz.ErrBusy) {
			t.Fatalf("unexpected admission error: %v", e)
		}
	}
	if admitted != 1 {
		t.Fatalf("admitted %d clones of one source", admitted)
	}
	var held int
	if e = p.pool.QueryRow(ctx, "SELECT count(*) FROM vm.clone_source_references WHERE tenant_id=$1 AND source_vm_id=$2 AND state='HELD'", source.TenantID, source.ID).Scan(&held); e != nil || held != 1 {
		t.Fatalf("HELD rows = %d: %v", held, e)
	}
}

// TestCloneCreateLeaseActiveReadsSQLColumn covers R8: the manual release guard
// must read the scheduling column Save clears, not the JSON record copy.
func TestCloneCreateLeaseActiveReadsSQLColumn(t *testing.T) {
	ctx := context.Background()
	db := testenv.NewDatabase(t)
	if e := Migrate(ctx, db.Owner.Config().ConnString(), db.RuntimeRole); e != nil {
		t.Fatal(e)
	}
	p, e := OpenPostgres(ctx, db.RuntimeDSN)
	if e != nil {
		t.Fatal(e)
	}
	defer p.Close()
	source := cloneAdmissionSource(t, ctx, db, p)
	target := cloneTargetAcceptance(source)
	created, e := p.AcceptCreate(ctx, target)
	if e != nil {
		t.Fatal(e)
	}
	if active, e := p.CloneCreateLeaseActive(ctx, source.TenantID, target.VMID, created.ID); e != nil || active {
		t.Fatalf("unclaimed create reported an active lease: %v %v", active, e)
	}
	if _, e = db.Owner.Exec(ctx, "UPDATE vm.operations SET lease_until=now()+interval '1 minute' WHERE tenant_id=$1 AND id=$2", source.TenantID, created.ID); e != nil {
		t.Fatal(e)
	}
	if active, e := p.CloneCreateLeaseActive(ctx, source.TenantID, target.VMID, created.ID); e != nil || !active {
		t.Fatalf("live SQL lease was not reported: %v %v", active, e)
	}
	// A stale JSON copy must not resurrect a lease the scheduler already cleared.
	if _, e = db.Owner.Exec(ctx, "UPDATE vm.operations SET lease_until=NULL, record=jsonb_set(record,'{LeaseUntil}','\"2099-01-01T00:00:00Z\"') WHERE tenant_id=$1 AND id=$2", source.TenantID, created.ID); e != nil {
		t.Fatal(e)
	}
	if active, e := p.CloneCreateLeaseActive(ctx, source.TenantID, target.VMID, created.ID); e != nil || active {
		t.Fatalf("JSON record copy was used as the lease fact: %v %v", active, e)
	}
}
