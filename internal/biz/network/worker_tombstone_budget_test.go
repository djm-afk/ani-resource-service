package biz

import (
	"context"
	"errors"
	"testing"
	"time"
)

type tombstoneBudgetRepository struct {
	work     Work
	progress Progress
	mutation string
}

func (r *tombstoneBudgetRepository) Claim(context.Context, string, time.Duration) (Work, bool, error) {
	return r.work, true, nil
}
func (r *tombstoneBudgetRepository) BeginMutation(_ context.Context, _ Work, action, _ string) error {
	r.mutation = action
	return nil
}
func (r *tombstoneBudgetRepository) Finish(_ context.Context, _ Work, p Progress) error {
	r.progress = p
	return nil
}

type tombstoneBudgetProvider struct {
	ResourceProvider
	value   ProviderObservation
	target  ProviderTarget
	deletes int
}

func (p *tombstoneBudgetProvider) Observe(_ context.Context, target ProviderTarget) (ProviderObservation, error) {
	p.target = target
	return p.value, nil
}
func (p *tombstoneBudgetProvider) Delete(context.Context, ProviderTarget) error {
	p.deletes++
	return nil
}

func TestWorkerCleanTombstoneUsesSharedEvidenceAndSlowerRecovery(t *testing.T) {
	for _, kind := range []string{"vpc", "subnet", "eip", "snat", "vlan", "egress_gateway", "public_pool"} {
		t.Run(kind, func(t *testing.T) {
			r := &tombstoneBudgetRepository{work: Work{Resource: ResourceWork{Kind: kind, State: Deleted}, KnownIdentity: "original-uid", Now: time.Now()}}
			p := &tombstoneBudgetProvider{}
			policy := DefaultWorkerPolicy()
			w, err := NewWorker(r, p, "44444444-4444-4444-8444-444444444444", policy)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = w.Step(context.Background()); err != nil {
				t.Fatal(err)
			}
			if p.target.Direct || !p.target.Deleting || !p.target.Tombstone || r.progress.State != Deleted || r.progress.NextDelay < policy.StaleAfter || p.deletes != 0 {
				t.Fatalf("clean tombstone requested a new per-resource audit or hot poll: target=%+v progress=%+v deletes=%d", p.target, r.progress, p.deletes)
			}
		})
	}
}

func TestWorkerTombstoneKeepsUnknownMutationAndLateObjectCleanup(t *testing.T) {
	for _, name := range []string{"unknown-create", "pending-delete", "late-original-uid", "dependencies", "replacement-uid"} {
		t.Run(name, func(t *testing.T) {
			r := &tombstoneBudgetRepository{work: Work{Resource: ResourceWork{Kind: "subnet", State: Deleted}, KnownIdentity: "original-uid", Now: time.Now()}}
			p := &tombstoneBudgetProvider{}
			switch name {
			case "unknown-create":
				r.work.PendingAction = "create"
			case "pending-delete":
				r.work.PendingAction = "delete"
			case "late-original-uid":
				p.value = ProviderObservation{Exists: true, Identity: "original-uid"}
			case "dependencies":
				p.value.HasDependencies = true
			case "replacement-uid":
				p.value = ProviderObservation{Exists: true, Identity: "replacement-uid"}
			}
			w, err := NewWorker(r, p, "44444444-4444-4444-8444-444444444444", DefaultWorkerPolicy())
			if err != nil {
				t.Fatal(err)
			}
			if _, err = w.Step(context.Background()); err != nil {
				t.Fatal(err)
			}
			if r.work.PendingAction != "" && (!p.target.Direct || p.target.Tombstone) {
				t.Fatal("unknown mutation reused ordinary tombstone evidence")
			}
			switch name {
			case "unknown-create":
				if r.progress.ClearPending || r.progress.Reason != ProviderUnknown || r.progress.OperationState != Blocked {
					t.Fatal("absence retired an unknown create", r.progress)
				}
			case "late-original-uid":
				if r.mutation != "delete" || p.deletes != 1 || r.progress.ClearPending || r.progress.Reason != CleanupPending {
					t.Fatal("late original object escaped durable cleanup", r.progress, r.mutation, p.deletes)
				}
			case "dependencies", "replacement-uid":
				if p.deletes != 0 || r.progress.Reason == "" {
					t.Fatal("unsafe tombstone dependency or replacement was ignored", r.progress, p.deletes)
				}
			}
		})
	}
}

type tombstoneBudgetConsumer struct{ calls int }

func (c *tombstoneBudgetConsumer) GetSubmission(context.Context, Attachment) (ConsumerSubmission, error) {
	c.calls++
	return ConsumerSubmission{}, errors.New("owner must not be repeatedly queried after persisted closure")
}

func TestManagedReleasedTombstoneRetainsClosureAndChecksLateRelations(t *testing.T) {
	for _, late := range []bool{false, true} {
		a, closed := vmSnapshotFixture()
		now := time.Now()
		closed.State, closed.DesiredRunState = "closed", "deleted"
		closed.FinalizationID = "33333333-3333-4333-8333-333333333333"
		closed.ClosedAt = &now
		closed.RetiredPods, closed.CurrentPods = closed.CurrentPods, nil
		a.State, a.FinalizationID, a.RootController = Released, closed.FinalizationID, closed.RootController
		a.SnapshotRevision, a.SnapshotHash, a.ConsumerSnapshot = closed.SnapshotRevision, ConsumerSnapshotHash(closed), &closed
		r := &managedWorkRepository{work: AttachmentWork{Attachment: a, Now: now, Relations: []byte("[]")}}
		owner := &tombstoneBudgetConsumer{}
		provider := managedProvider{value: AttachmentObservation{HasDependencies: late, Relations: []byte("[]"), Proof: ObservationProof{CollectedAt: now}}}
		policy := DefaultWorkerPolicy()
		w, err := NewAttachmentWorker(r, provider, owner, "44444444-4444-4444-8444-444444444444", policy)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = w.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
		if owner.calls != 0 || r.progress.State != Released || !r.progress.Observed {
			t.Fatal("released tombstone lost durable closure or skipped relationship observation", owner.calls, r.progress)
		}
		if late {
			if !r.progress.ProtocolBlocked || r.progress.Reason != AttachmentProtocol {
				t.Fatal("late relation failed to fence parent/address reuse", r.progress)
			}
		} else if r.progress.ProtocolBlocked || r.progress.NextDelay < policy.StaleAfter {
			t.Fatal("clean released tombstone stayed in the hot poll loop", r.progress)
		}
	}
}
