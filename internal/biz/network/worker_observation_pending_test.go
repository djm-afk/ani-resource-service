package biz

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestWorkerSharedObservationWaitDoesNotInstallFailureBackoff(t *testing.T) {
	pending := &ProviderError{Kind: ProviderTemporary, Cause: fmt.Errorf("audit view: %w", ErrObservationPending)}
	genuineFailure := &ProviderError{Kind: ProviderTemporary, Cause: errors.New("Kubernetes GET deadline exceeded")}
	untypedWait := &ProviderError{Kind: ProviderTemporary, Cause: errors.New("shared observation is pending")}
	for _, tc := range []struct {
		name        string
		kind        string
		state       ResourceState
		active      bool
		pending     string
		failure     error
		wantBackoff bool
	}{
		{name: "available EIP local wait", kind: "eip", state: Available, failure: pending},
		{name: "degraded EIP local wait", kind: "eip", state: Degraded, failure: pending},
		{name: "available SNAT local wait", kind: "snat", state: Available, failure: pending},
		{name: "degraded SNAT local wait", kind: "snat", state: Degraded, failure: pending},
		{name: "genuine provider failure", kind: "eip", state: Available, failure: genuineFailure, wantBackoff: true},
		{name: "error text is not a local wait", kind: "snat", state: Available, failure: untypedWait, wantBackoff: true},
		{name: "active operation", kind: "snat", state: Available, active: true, failure: pending, wantBackoff: true},
		{name: "pending mutation", kind: "eip", state: Available, pending: "create", failure: pending, wantBackoff: true},
		{name: "active deletion", kind: "eip", state: Deleting, failure: pending, wantBackoff: true},
		{name: "deleted tombstone", kind: "eip", state: Deleted, failure: pending, wantBackoff: true},
		{name: "provisioning", kind: "snat", state: Provisioning, failure: pending, wantBackoff: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			collected := now.Add(-33 * time.Second)
			r := &tombstoneBudgetRepository{work: Work{
				Resource:      ResourceWork{Kind: tc.kind, State: tc.state, ObservedAt: &collected},
				KnownIdentity: "original-provider-uid", ActiveOperation: tc.active, PendingAction: tc.pending,
				Attempt: 30001, Now: now, Requirement: ObservationRequirement{PreviousCollectedAt: collected},
			}}
			policy := DefaultWorkerPolicy()
			w, err := NewWorker(r, failedObservationProvider{err: tc.failure}, "44444444-4444-4444-8444-444444444444", policy)
			if err != nil {
				t.Fatal(err)
			}
			if worked, err := w.Step(context.Background()); !worked || err != nil {
				t.Fatal("observation was not durably completed", worked, err)
			}
			p := r.progress
			if p.Backoff != tc.wantBackoff {
				t.Fatalf("failure backoff = %t, want %t; progress=%+v", p.Backoff, tc.wantBackoff, p)
			}
			if !tc.wantBackoff && p.NextDelay != policy.ObserveEvery {
				t.Fatalf("shared wait hot-polled or deferred past normal observation cadence: %s", p.NextDelay)
			}
			if tc.wantBackoff && p.NextDelay <= policy.ObserveEvery {
				t.Fatalf("genuine failure or unfinished lifecycle lost its existing retry backoff: %s", p.NextDelay)
			}
			if p.State != tc.state || p.Reason != ProviderUnavailable || !errors.Is(p.ProviderError, tc.failure) || p.Identity != r.work.KnownIdentity {
				t.Fatalf("failed read changed resource identity/state or lost its cause: %+v", p)
			}
			if p.Observed || p.Proof != (ObservationProof{}) || p.Egress != nil || p.VNic != nil || p.ClearPending || r.mutation != "" {
				t.Fatalf("local wait produced new facts, cleared a mutation, or attempted a write: %+v, mutation=%s", p, r.mutation)
			}
			if !r.work.Resource.ObservedAt.Equal(now.Add(-33 * time.Second)) {
				t.Fatal("failed read renewed the original observation timestamp")
			}
			if tc.active && p.OperationState != Retrying {
				t.Fatal("active operation no longer remains retrying", p.OperationState)
			}
		})
	}
}
