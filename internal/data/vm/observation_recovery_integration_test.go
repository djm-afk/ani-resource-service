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
