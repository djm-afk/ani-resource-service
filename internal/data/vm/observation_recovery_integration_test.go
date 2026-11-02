//go:build instanceintegration

package vm

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
)

func TestVMObservationTemporaryFailureRetainsFreshFactsUntilExpiry(t *testing.T) {
	for _, readErr := range []error{biz.ErrObservationUnavailable, context.DeadlineExceeded} {
		t.Run(readErr.Error(), func(t *testing.T) {
			p, _, v := observationRegisteredVM(t)
			ctx := context.Background()
			// Leave less than one failure-backoff interval of factual validity.
			v.ObservedAt = time.Now().Add(-50 * time.Second).UTC()
			v.Reason = "VMI_RUNNING"
			if _, err := p.pool.Exec(ctx, `UPDATE vm.virtual_machines SET current_configuration=$3 WHERE tenant_id=$1 AND id=$2`, v.TenantID, v.ID, encode(v)); err != nil {
				t.Fatal(err)
			}
			work, _, err := p.ClaimObservation(ctx, uuid.NewString(), 15*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if err = p.SaveObservation(ctx, work, biz.Observation{}, readErr); err != nil {
				t.Fatal(err)
			}
			current, err := p.Get(ctx, v.TenantID, v.ID)
			if err != nil || current.Observed != "Running" || current.Reason != v.Reason || !current.ObservedAt.Equal(v.ObservedAt) || len(current.Addresses) != 1 {
				t.Fatalf("fresh facts changed: %+v %v", current, err)
			}
			var due, earliest time.Time
			if err = p.pool.QueryRow(ctx, `SELECT next_attempt_at,not_before FROM vm.observation_work WHERE tenant_id=$1 AND vm_id=$2`, v.TenantID, v.ID).Scan(&due, &earliest); err != nil {
				t.Fatal(err)
			}
			expiry := v.ObservedAt.Add(time.Minute)
			if due.After(expiry.Add(time.Millisecond)) || earliest.After(expiry.Add(time.Millisecond)) || due.Before(expiry.Add(-time.Second)) {
				t.Fatalf("expiry hidden behind backoff: due=%v earliest=%v expiry=%v", due, earliest, expiry)
			}
			// Advance just the fixture evidence clock and scheduler to represent
			// the due expiry retry, without a minute-long wall-clock sleep.
			v.ObservedAt = time.Now().Add(-61 * time.Second).UTC()
			if _, err = p.pool.Exec(ctx, `UPDATE vm.virtual_machines SET current_configuration=$3 WHERE tenant_id=$1 AND id=$2`, v.TenantID, v.ID, encode(v)); err != nil {
				t.Fatal(err)
			}
			if _, err = p.pool.Exec(ctx, `UPDATE vm.observation_work SET next_attempt_at=now(),not_before=now() WHERE tenant_id=$1 AND vm_id=$2`, v.TenantID, v.ID); err != nil {
				t.Fatal(err)
			}
			work, _, err = p.ClaimObservation(ctx, uuid.NewString(), 15*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if err = p.SaveObservation(ctx, work, biz.Observation{}, readErr); err != nil {
				t.Fatal(err)
			}
			current, err = p.Get(ctx, v.TenantID, v.ID)
			if err != nil || current.Observed != "Unknown" || current.Reason != observationFailureReason(readErr) || !current.ObservedAt.Equal(v.ObservedAt) || len(current.Addresses) != 0 {
				t.Fatalf("expired facts retained: %+v %v", current, err)
			}
			var failures int
			if err = p.pool.QueryRow(ctx, `SELECT failures FROM vm.observation_work WHERE tenant_id=$1 AND vm_id=$2`, v.TenantID, v.ID).Scan(&failures); err != nil || failures != 2 {
				t.Fatalf("failure backoff reset: %d %v", failures, err)
			}
		})
	}
}

func TestVMObservationHardFailureDoesNotRetainFreshFacts(t *testing.T) {
	for _, readErr := range []error{biz.ErrOwnership, biz.ErrUnknown, biz.ErrDependency, biz.ErrInvalid} {
		t.Run(readErr.Error(), func(t *testing.T) {
			p, _, v := observationRegisteredVM(t)
			ctx := context.Background()
			work, _, err := p.ClaimObservation(ctx, uuid.NewString(), 15*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if err = p.SaveObservation(ctx, work, biz.Observation{}, readErr); err != nil {
				t.Fatal(err)
			}
			current, err := p.Get(ctx, v.TenantID, v.ID)
			if err != nil || current.Observed != "Unknown" || current.Reason != observationFailureReason(readErr) || !current.ObservedAt.Equal(v.ObservedAt) || len(current.Addresses) != 0 {
				t.Fatalf("hard failure retained facts: %+v %v", current, err)
			}
			if _, _, err = p.ClaimObservation(ctx, uuid.NewString(), 15*time.Second); !errors.Is(err, biz.ErrNotFound) {
				t.Fatalf("hard failure bypassed backoff: %v", err)
			}
		})
	}
}

func TestVMObservationVerifiedRecoveryWakesOnlyExactNativeVM(t *testing.T) {
	for _, state := range []string{"Running", "Stopped"} {
		t.Run(state, func(t *testing.T) {
			p, _, v := observationRegisteredVM(t)
			ctx := context.Background()
			other := v
			other.TenantID, other.ID, other.CreateOperationID, other.LastOperationID = uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
			other.Native.UID = uuid.NewString()
			if _, err := p.pool.Exec(ctx, `INSERT INTO vm.virtual_machines(tenant_id,id,create_operation_id,request_digest,spec_digest,initial_spec,current_configuration,version) VALUES($1,$2,$3,$4,$5,$6,$7,1)`, other.TenantID, other.ID, other.CreateOperationID, stringDigest('d'), other.Spec.Digest, encode(other.Spec), encode(other)); err != nil {
				t.Fatal(err)
			}
			v.ObservedAt = time.Now().Add(-61 * time.Second).UTC()
			if _, err := p.pool.Exec(ctx, `UPDATE vm.virtual_machines SET current_configuration=$3 WHERE tenant_id=$1 AND id=$2`, v.TenantID, v.ID, encode(v)); err != nil {
				t.Fatal(err)
			}
			work, _, err := p.ClaimObservation(ctx, uuid.NewString(), 15*time.Second)
			// The initial registration may choose the other row first. Only the
			// scheduling fixture is changed so the intended target claims next.
			if err != nil {
				t.Fatal(err)
			}
			if work.VMID != v.ID {
				if err = p.SaveObservation(ctx, work, biz.Observation{}, biz.ErrObservationUnavailable); err != nil {
					t.Fatal(err)
				}
				work, _, err = p.ClaimObservation(ctx, uuid.NewString(), 15*time.Second)
				if err != nil || work.VMID != v.ID {
					t.Fatalf("target claim: %+v %v", work, err)
				}
			}
			if err = p.SaveObservation(ctx, work, biz.Observation{}, biz.ErrObservationUnavailable); err != nil {
				t.Fatal(err)
			}
			if _, err = p.pool.Exec(ctx, `UPDATE vm.observation_work SET failures=4,not_before=now()+interval '240 seconds',next_attempt_at=now()+interval '240 seconds'`); err != nil {
				t.Fatal(err)
			}
			if err = p.NotifyAllObservations(ctx); err != nil {
				t.Fatal(err)
			}
			if _, _, err = p.ClaimObservation(ctx, uuid.NewString(), 15*time.Second); !errors.Is(err, biz.ErrNotFound) {
				t.Fatalf("ordinary rescan bypassed backoff: %v", err)
			}
			if err = p.RecoverObservations(ctx, []biz.ObservationRecovery{{Native: v.Native, ObservedAt: time.Now().UTC()}}); err != nil {
				t.Fatal(err)
			}
			var targetWait, otherWait float64
			if err = p.pool.QueryRow(ctx, `SELECT EXTRACT(EPOCH FROM not_before-now())::double precision FROM vm.observation_work WHERE tenant_id=$1 AND vm_id=$2`, v.TenantID, v.ID).Scan(&targetWait); err != nil {
				t.Fatal(err)
			}
			if err = p.pool.QueryRow(ctx, `SELECT EXTRACT(EPOCH FROM not_before-now())::double precision FROM vm.observation_work WHERE tenant_id=$1 AND vm_id=$2`, other.TenantID, other.ID).Scan(&otherWait); err != nil {
				t.Fatal(err)
			}
			if targetWait > 1.05 || otherWait < 235 {
				t.Fatalf("native UID or tenant isolation lost: target=%v other=%v", targetWait, otherWait)
			}
			observer, err := biz.NewObserver(p, observationRead(func(_ context.Context, vm biz.VM) (biz.Observation, error) {
				return biz.Observation{VM: vm.Native, State: state, Running: state == "Running", Stopped: state == "Stopped", ObservedAt: time.Now().UTC()}, nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(2 * time.Second)
			for {
				worked, err := observer.Step(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if worked {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("verified recovery remained behind failure backoff")
				}
				time.Sleep(20 * time.Millisecond)
			}
			current, err := p.Get(ctx, v.TenantID, v.ID)
			if err != nil || current.Observed != state || !current.ObservedAt.After(v.ObservedAt) {
				t.Fatalf("verified recovery not saved: %+v %v", current, err)
			}
			var failures int
			if err = p.pool.QueryRow(ctx, `SELECT failures FROM vm.observation_work WHERE tenant_id=$1 AND vm_id=$2`, v.TenantID, v.ID).Scan(&failures); err != nil || failures != 0 {
				t.Fatalf("successful recovery did not reset failures: %d %v", failures, err)
			}
		})
	}
}

func TestVMObservationRecoveryRejectsUntrustedProof(t *testing.T) {
	for _, mutation := range []string{"older", "equal", "expired", "future", "uid", "namespace", "name", "kind", "closing", "deleted", "ownership", "invalid", "healthy", "batch-limit"} {
		t.Run(mutation, func(t *testing.T) {
			p, _, v := observationRegisteredVM(t)
			ctx := context.Background()
			work, _, err := p.ClaimObservation(ctx, uuid.NewString(), 15*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			readErr := biz.ErrObservationUnavailable
			if mutation == "ownership" {
				readErr = biz.ErrOwnership
			}
			if mutation == "invalid" {
				readErr = biz.ErrInvalid
			}
			if err = p.SaveObservation(ctx, work, biz.Observation{}, readErr); err != nil {
				t.Fatal(err)
			}
			proof := biz.ObservationRecovery{Native: v.Native, ObservedAt: time.Now().UTC()}
			switch mutation {
			case "older":
				proof.ObservedAt = v.ObservedAt.Add(-time.Second)
			case "equal":
				proof.ObservedAt = v.ObservedAt
			case "expired":
				proof.ObservedAt = time.Now().Add(-time.Minute - time.Second)
			case "future":
				proof.ObservedAt = time.Now().Add(time.Minute)
			case "uid":
				proof.Native.UID = uuid.NewString()
			case "namespace":
				proof.Native.Namespace += "-other"
			case "name":
				proof.Native.Name += "-other"
			case "kind":
				proof.Native.Kind = "VirtualMachineInstance"
			case "closing":
				_, err = p.AcceptDelete(ctx, biz.Acceptance{TenantID: v.TenantID, VMID: v.ID, OperationID: uuid.NewString(), CreateOperationID: v.CreateOperationID, ExpectedVersion: v.Version, RequestDigest: stringDigest('d')})
			case "deleted":
				_, err = p.pool.Exec(ctx, `UPDATE vm.virtual_machines SET current_configuration=jsonb_set(current_configuration,'{Observed}','"Deleted"') WHERE tenant_id=$1 AND id=$2`, v.TenantID, v.ID)
			case "healthy":
				_, err = p.pool.Exec(ctx, `UPDATE vm.observation_work SET failures=0 WHERE tenant_id=$1 AND vm_id=$2`, v.TenantID, v.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			var beforeGeneration, afterGeneration int64
			var beforeDue, afterDue, beforeMinimum, afterMinimum time.Time
			readSchedule := func(g *int64, due, minimum *time.Time) {
				t.Helper()
				if err := p.pool.QueryRow(ctx, `SELECT notification_generation,next_attempt_at,not_before FROM vm.observation_work WHERE tenant_id=$1 AND vm_id=$2`, v.TenantID, v.ID).Scan(g, due, minimum); err != nil {
					t.Fatal(err)
				}
			}
			readSchedule(&beforeGeneration, &beforeDue, &beforeMinimum)
			proofs := []biz.ObservationRecovery{proof}
			if mutation == "batch-limit" {
				proofs = make([]biz.ObservationRecovery, 129)
				for i := range proofs {
					proofs[i] = proof
				}
			}
			err = p.RecoverObservations(ctx, proofs)
			if mutation == "kind" || mutation == "batch-limit" {
				if !errors.Is(err, biz.ErrInvalid) {
					t.Fatalf("invalid recovery accepted: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			readSchedule(&afterGeneration, &afterDue, &afterMinimum)
			if beforeGeneration != afterGeneration || !beforeDue.Equal(afterDue) || !beforeMinimum.Equal(afterMinimum) {
				t.Fatalf("untrusted proof released backoff: %v %v => %v %v", beforeGeneration, beforeDue, afterGeneration, afterDue)
			}
		})
	}
}

func TestVMObservationContinuousFailureRetainsBoundedBackoff(t *testing.T) {
	p, _, v := observationRegisteredVM(t)
	ctx := context.Background()
	v.ObservedAt = time.Now().Add(-2 * time.Minute).UTC()
	if _, err := p.pool.Exec(ctx, `UPDATE vm.virtual_machines SET current_configuration=$3 WHERE tenant_id=$1 AND id=$2`, v.TenantID, v.ID, encode(v)); err != nil {
		t.Fatal(err)
	}
	for i, expected := range []float64{30, 60, 120, 240, 300, 300} {
		if _, err := p.pool.Exec(ctx, `UPDATE vm.observation_work SET not_before=now(),next_attempt_at=now() WHERE tenant_id=$1 AND vm_id=$2`, v.TenantID, v.ID); err != nil {
			t.Fatal(err)
		}
		work, _, err := p.ClaimObservation(ctx, uuid.NewString(), 15*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if err = p.SaveObservation(ctx, work, biz.Observation{}, biz.ErrObservationUnavailable); err != nil {
			t.Fatal(err)
		}
		if err = p.NotifyAllObservations(ctx); err != nil {
			t.Fatal(err)
		}
		var wait float64
		if err = p.pool.QueryRow(ctx, `SELECT EXTRACT(EPOCH FROM not_before-now())::double precision FROM vm.observation_work WHERE tenant_id=$1 AND vm_id=$2`, v.TenantID, v.ID).Scan(&wait); err != nil || wait < expected-1 || wait > expected+1 {
			t.Fatalf("failure %d lost bounded backoff: %v expected=%v err=%v", i+1, wait, expected, err)
		}
	}
}

func TestVMObservationRecoveryFencesAlreadyClaimedFailure(t *testing.T) {
	p, _, v := observationRegisteredVM(t)
	ctx := context.Background()
	work, _, err := p.ClaimObservation(ctx, uuid.NewString(), 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.SaveObservation(ctx, work, biz.Observation{}, biz.ErrObservationUnavailable); err != nil {
		t.Fatal(err)
	}
	if _, err = p.pool.Exec(ctx, `UPDATE vm.observation_work SET not_before=now(),next_attempt_at=now() WHERE tenant_id=$1 AND vm_id=$2`, v.TenantID, v.ID); err != nil {
		t.Fatal(err)
	}
	// This read began before the shared audit recovered and is still in flight.
	work, _, err = p.ClaimObservation(ctx, uuid.NewString(), 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.RecoverObservations(ctx, []biz.ObservationRecovery{{Native: v.Native, ObservedAt: time.Now().UTC()}}); err != nil {
		t.Fatal(err)
	}
	if err = p.SaveObservation(ctx, work, biz.Observation{}, biz.ErrOwnership); !errors.Is(err, biz.ErrLease) {
		t.Fatalf("pre-recovery read accepted: %v", err)
	}
	current, err := p.Get(ctx, v.TenantID, v.ID)
	if err != nil || current.Observed != v.Observed || !current.ObservedAt.Equal(v.ObservedAt) || len(current.Addresses) != 1 {
		t.Fatalf("old failure replaced recovered generation: %+v %v", current, err)
	}
	var failures int
	var wait float64
	if err = p.pool.QueryRow(ctx, `SELECT failures,EXTRACT(EPOCH FROM next_attempt_at-now())::double precision FROM vm.observation_work WHERE tenant_id=$1 AND vm_id=$2`, v.TenantID, v.ID).Scan(&failures, &wait); err != nil || failures != 1 || wait > 1.05 {
		t.Fatalf("old failure restored backoff: failures=%d wait=%v err=%v", failures, wait, err)
	}
}
