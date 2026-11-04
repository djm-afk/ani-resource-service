package biz

import (
	"context"
	"errors"
	"testing"
	"time"
)

type retainedClosureProvider struct {
	value AttachmentObservation
	fresh bool
}

func (p *retainedClosureProvider) ObserveAttachment(_ context.Context, w AttachmentWork) (AttachmentObservation, error) {
	p.fresh = w.RequireFreshRelations
	return p.value, nil
}

func TestReleasedProtocolRecoveryUsesLaterSharedEvidenceAndKeepsFences(t *testing.T) {
	for _, mode := range []string{"vm_managed", "controller_managed"} {
		for _, scenario := range []string{"valid", "before-violation", "uncovered-generation", "dependencies", "pending-candidate", "new-revision", "changed-finalization", "changed-identity"} {
			t.Run(mode+"/"+scenario, func(t *testing.T) {
				a, c := vmSnapshotFixture()
				if mode == "controller_managed" {
					a, c = managedSnapshotFixture()
				}
				now := time.Now()
				closed := now.Add(-time.Hour)
				c.State, c.DesiredRunState, c.ClosedAt = "closed", "deleted", &closed
				c.FinalizationID = "33333333-3333-4333-8333-333333333333"
				c.RetiredPods, c.CurrentPods = c.CurrentPods, nil
				a.State, a.RootController, a.FinalizationID = Released, c.RootController, c.FinalizationID
				a.SnapshotRevision, a.SnapshotHash = c.SnapshotRevision, ConsumerSnapshotHash(c)
				accepted := c
				a.ConsumerSnapshot = &accepted
				a.UpdatedAt = now.Add(-10 * time.Second)
				proof := ObservationProof{CollectedAt: now.Add(-time.Second), CoveredGeneration: 2, Hash: "absence"}
				o := AttachmentObservation{Relations: []byte("[]"), Proof: proof}
				switch scenario {
				case "before-violation":
					o.Proof.CollectedAt = a.UpdatedAt.Add(-time.Second)
				case "uncovered-generation":
					o.Proof.CoveredGeneration = 1
				case "dependencies":
					o.HasDependencies = true
				case "pending-candidate":
					o.PendingCandidate = true
				case "new-revision":
					c.SnapshotRevision++
				case "changed-finalization":
					c.FinalizationID = "55555555-5555-4555-8555-555555555555"
				case "changed-identity":
					c.RootController.UID = "replacement"
				}
				r := &managedWorkRepository{work: AttachmentWork{Attachment: a, ProtocolBlocked: true, Now: now, Relations: []byte("[]"), Requirement: ObservationRequirement{RequestedGeneration: 2, AppliedAt: now.Add(-20 * time.Second), Hash: "absence"}}}
				provider := &retainedClosureProvider{value: o}
				policy := DefaultWorkerPolicy()
				w, err := NewAttachmentWorker(r, provider, managedConsumer{c}, "44444444-4444-4444-8444-444444444444", policy)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = w.Step(context.Background()); err != nil {
					t.Fatal(err)
				}
				if provider.fresh {
					t.Fatal("released protocol retry demanded a per-Claim complete collection")
				}
				if scenario == "valid" {
					if r.progress.State != Released || r.progress.ProtocolBlocked || !r.progress.ProtocolRecovered || r.progress.Reason != "" {
						t.Fatal("unchanged accepted closure with newer shared absence did not recover", r.progress)
					}
				} else if !r.progress.ProtocolBlocked || r.progress.ProtocolRecovered || !r.progress.Backoff || r.progress.NextDelay < policy.StaleAfter {
					t.Fatal("unsafe recovery or hot protocol retry", r.progress)
				}
			})
		}
	}
}

type unavailableRetainedOwner struct{}

func (unavailableRetainedOwner) GetSubmission(context.Context, Attachment) (ConsumerSubmission, error) {
	return ConsumerSubmission{}, errors.New("owner unavailable")
}

func TestReleasedProtocolRecoveryRequiresCurrentOwner(t *testing.T) {
	a, c := vmSnapshotFixture()
	now := time.Now()
	c.State, c.DesiredRunState, c.ClosedAt = "closed", "deleted", &now
	c.FinalizationID = "33333333-3333-4333-8333-333333333333"
	c.RetiredPods, c.CurrentPods = c.CurrentPods, nil
	a.State, a.RootController, a.FinalizationID = Released, c.RootController, c.FinalizationID
	a.SnapshotRevision, a.SnapshotHash, a.ConsumerSnapshot = c.SnapshotRevision, ConsumerSnapshotHash(c), &c
	r := &managedWorkRepository{work: AttachmentWork{Attachment: a, ProtocolBlocked: true, Now: now, Relations: []byte("[]")}}
	p := &retainedClosureProvider{value: AttachmentObservation{Relations: []byte("[]"), Proof: ObservationProof{CollectedAt: now}}}
	w, err := NewAttachmentWorker(r, p, unavailableRetainedOwner{}, "44444444-4444-4444-8444-444444444444", DefaultWorkerPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !r.progress.ProtocolBlocked || r.progress.ProtocolRecovered || p.fresh {
		t.Fatal("owner error cleared retained protocol fence or forced full audit", r.progress)
	}
}
