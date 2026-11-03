package biz

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"
)

func managedSnapshotFixture() (Attachment, ConsumerSubmission) {
	a := Attachment{ID: "att_11111111111111111111111111111111", TenantID: "11111111-1111-4111-8111-111111111111", InstanceID: "instance", SubmissionID: "22222222-2222-4222-8222-222222222222", Generation: 1, ClusterID: "cluster", Namespace: "tenant", State: Reserved, WorkloadMode: "controller_managed"}
	root := WorkloadReference{APIVersion: "kubeflow.org/v1", Kind: "Notebook", Namespace: a.Namespace, Name: "instance", UID: "notebook-uid"}
	set := WorkloadReference{APIVersion: "apps/v1", Kind: "StatefulSet", Namespace: a.Namespace, Name: "instance", UID: "set-uid"}
	pod := WorkloadReference{APIVersion: "v1", Kind: "Pod", Namespace: a.Namespace, Name: "instance-0", UID: "pod-1"}
	c := ConsumerSubmission{ProtocolVersion: 2, TenantID: a.TenantID, InstanceID: a.InstanceID, SubmissionID: a.SubmissionID, Generation: a.Generation, AttachmentID: a.ID, ClusterID: a.ClusterID, Namespace: a.Namespace, State: "open", SnapshotRevision: 1, DesiredRunState: "running", RootController: root, Controllers: []ControllerReference{{Reference: set, Owner: root}}, CurrentPods: []PodReference{{Reference: pod, Owner: set}}}
	return a, c
}

func TestManagedConsumerSnapshotRejectsRollbackAndSameRevisionMutation(t *testing.T) {
	a, c := managedSnapshotFixture()
	if !validConsumer(a, c) {
		t.Fatal("valid Notebook submission rejected")
	}
	a.SnapshotRevision = c.SnapshotRevision
	a.ConsumerSnapshot = &c
	a.SnapshotHash = ConsumerSnapshotHash(c)
	a.RootController = c.RootController
	old := c
	old.SnapshotRevision = 0
	if status := CompareConsumerSnapshot(a, old); status != SnapshotStale {
		t.Fatal("older snapshot was not ignored", status)
	}
	changed := c
	changed.DesiredRunState = "stopped"
	if status := CompareConsumerSnapshot(a, changed); status != SnapshotConflict {
		t.Fatal("same revision different content accepted", status)
	}
	closing := c
	closing.State = "closing"
	closing.FinalizationID = "33333333-3333-4333-8333-333333333333"
	closing.SnapshotRevision++
	a.ConsumerSnapshot = &closing
	a.SnapshotRevision = closing.SnapshotRevision
	a.SnapshotHash = ConsumerSnapshotHash(closing)
	a.FinalizationID = closing.FinalizationID
	reopened := c
	reopened.SnapshotRevision = closing.SnapshotRevision + 1
	if status := CompareConsumerSnapshot(a, reopened); status != SnapshotConflict {
		t.Fatal("closing reopened by newer open snapshot", status)
	}
}

type managedWorkRepository struct {
	work     AttachmentWork
	progress AttachmentProgress
}

func (r *managedWorkRepository) ClaimAttachment(context.Context, string, time.Duration) (AttachmentWork, bool, error) {
	return r.work, true, nil
}
func (r *managedWorkRepository) FinishAttachment(_ context.Context, _ AttachmentWork, p AttachmentProgress) error {
	r.progress = p
	return nil
}

type managedProvider struct{ value AttachmentObservation }

func (p managedProvider) ObserveAttachment(context.Context, AttachmentWork) (AttachmentObservation, error) {
	return p.value, nil
}

type managedConsumer struct{ value ConsumerSubmission }

func (c managedConsumer) GetSubmission(context.Context, Attachment) (ConsumerSubmission, error) {
	return c.value, nil
}

func TestManagedAttachmentStopStartReplacesCurrentWithoutPermanentConflict(t *testing.T) {
	a, c := managedSnapshotFixture()
	a.State = Attached
	a.PodName = "instance-0"
	a.PodUID = "pod-1"
	a.ConfirmUID = "pod-1"
	a.ConsumerSnapshot = &c
	a.SnapshotRevision = 1
	a.SnapshotHash = ConsumerSnapshotHash(c)
	a.RootController = c.RootController
	old := c.CurrentPods[0]
	c.SnapshotRevision = 2
	c.DesiredRunState = "stopped"
	c.RetiredPods = []PodReference{old}
	c.CurrentPods = nil
	r := &managedWorkRepository{work: AttachmentWork{Attachment: a, Now: time.Now()}}
	policy := DefaultWorkerPolicy()
	w, err := NewAttachmentWorker(r, managedProvider{}, managedConsumer{c}, "44444444-4444-4444-8444-444444444444", policy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.progress.ProtocolBlocked || r.progress.PodUID != "" || r.progress.State != Reserved {
		t.Fatal("Stop retained current address eligibility or permanently blocked", r.progress)
	}
	a.PodName = ""
	a.PodUID = ""
	a.ConfirmUID = ""
	a.State = Reserved
	a.ConsumerSnapshot = &c
	a.SnapshotRevision = 2
	a.SnapshotHash = ConsumerSnapshotHash(c)
	c.SnapshotRevision = 3
	c.DesiredRunState = "running"
	next := old
	next.Reference.UID = "pod-2"
	c.CurrentPods = []PodReference{next}
	r.work.Attachment = a
	w, err = NewAttachmentWorker(r, managedProvider{AttachmentObservation{Exists: true, PodName: "instance-0", PodUID: "pod-2", VerifiedCurrent: true}}, managedConsumer{c}, "44444444-4444-4444-8444-444444444444", policy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.progress.ProtocolBlocked || r.progress.PodUID != "pod-2" || r.progress.State != Attached {
		t.Fatal("Start rejected a verified replacement", r.progress)
	}
}

func TestManagedProtocolRecoveryKeepsFrozenIdentityAndConfirmFence(t *testing.T) {
	for _, name := range []string{"verified", "missing-confirm", "wrong-confirm-revision", "wrong-confirm-uid", "old-snapshot", "new-snapshot", "same-version-content", "root-replaced", "closing", "no-address", "pending-candidate"} {
		t.Run(name, func(t *testing.T) {
			a, frozen := managedSnapshotFixture()
			a.ConsumerSnapshot = &frozen
			a.SnapshotRevision = frozen.SnapshotRevision
			a.SnapshotHash = ConsumerSnapshotHash(frozen)
			a.RootController = frozen.RootController
			a.ConfirmedSnapshotRevision = frozen.SnapshotRevision
			a.ConfirmedPodUID = "pod-1"
			c := frozen
			o := AttachmentObservation{Exists: true, PodName: "instance-0", PodUID: "pod-1", VerifiedCurrent: true}
			switch name {
			case "missing-confirm":
				a.ConfirmedSnapshotRevision = 0
			case "wrong-confirm-revision":
				a.ConfirmedSnapshotRevision++
			case "wrong-confirm-uid":
				a.ConfirmedPodUID = "old-pod"
			case "old-snapshot":
				c.SnapshotRevision--
			case "new-snapshot":
				c.SnapshotRevision++
			case "same-version-content":
				c.DesiredRunState = "stopped"
			case "root-replaced":
				c.RootController.UID = "different-root"
			case "closing":
				c.State, c.DesiredRunState, c.FinalizationID = "closing", "deleted", "33333333-3333-4333-8333-333333333333"
				c.SnapshotRevision++
			case "no-address":
				o.VerifiedCurrent = false
			case "pending-candidate":
				o.PendingCandidate = true
			}
			r := &managedWorkRepository{work: AttachmentWork{Attachment: a, ProtocolBlocked: true, Now: time.Now()}}
			w, err := NewAttachmentWorker(r, managedProvider{o}, managedConsumer{c}, "44444444-4444-4444-8444-444444444444", DefaultWorkerPolicy())
			if err != nil {
				t.Fatal(err)
			}
			if _, err = w.Step(context.Background()); err != nil {
				t.Fatal(err)
			}
			if name == "verified" {
				if r.progress.ProtocolBlocked || !r.progress.ProtocolRecovered || r.progress.State != Attached {
					t.Fatal("complete unchanged proof did not recover", r.progress)
				}
			} else if !r.progress.ProtocolBlocked || r.progress.ProtocolRecovered {
				t.Fatal("unproven retry cleared the protocol fence", r.progress)
			}
		})
	}
}

func TestManagedConsumerOptionalManualReferencesPreserveSnapshotIdentity(t *testing.T) {
	for _, explicitNull := range []bool{false, true} {
		name := "omitted"
		if explicitNull {
			name = "null"
		}
		t.Run(name, func(t *testing.T) {
			a, c := managedSnapshotFixture()
			raw, e := json.Marshal(c)
			if e != nil {
				t.Fatal(e)
			}
			if explicitNull {
				raw = append([]byte(`{"ManualVNics":null,`), raw[1:]...)
			}
			var persisted ConsumerSubmission
			if e = json.Unmarshal(raw, &persisted); e != nil {
				t.Fatal(e)
			}
			h := sha256.Sum256(raw)
			a.SnapshotHash = hex.EncodeToString(h[:])
			a.SnapshotRevision = c.SnapshotRevision
			a.ConsumerSnapshot = &persisted
			a.RootController = c.RootController
			a.State = Attached
			a.ConfirmedSnapshotRevision = c.SnapshotRevision
			a.ConfirmedPodUID = c.CurrentPods[0].Reference.UID
			if CompareConsumerSnapshot(a, c) != SnapshotAccepted || !ManagedProtocolRecoveryMatches(a, c, "instance-0", a.ConfirmedPodUID) {
				t.Fatal("absent optional VNIC reference invalidated the unchanged consumer snapshot")
			}
			changed := c
			changed.DesiredRunState = "stopped"
			if CompareConsumerSnapshot(a, changed) != SnapshotConflict {
				t.Fatal("same-revision run-state change accepted")
			}
			changed = c
			changed.ManualVNics = []ManualVNicReference{{VNicID: "vnic_11111111111111111111111111111111", ProviderUID: "new-uid", ProviderName: "manual"}}
			if CompareConsumerSnapshot(a, changed) != SnapshotConflict {
				t.Fatal("same-revision manual VNIC change accepted")
			}
			changed = c
			changed.CurrentPods = append([]PodReference(nil), c.CurrentPods...)
			changed.CurrentPods[0].Reference.UID = "replacement-pod"
			if CompareConsumerSnapshot(a, changed) != SnapshotConflict {
				t.Fatal("same-revision endpoint replacement accepted")
			}
		})
	}
}
