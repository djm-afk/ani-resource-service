package biz

import (
	"context"
	"errors"
	"testing"
	"time"
)

func managedSnapshotFixture() (Attachment, ConsumerSubmission) {
	a := Attachment{ID: "att_11111111111111111111111111111111", TenantID: "11111111-1111-4111-8111-111111111111", InstanceID: "instance", SubmissionID: "22222222-2222-4222-8222-222222222222", Generation: 1, ClusterID: "cluster", Namespace: "tenant", State: Reserved, WorkloadMode: "controller_managed"}
	pod := WorkloadReference{APIVersion: "v1", Kind: "Pod", Namespace: a.Namespace, Name: "instance-0", UID: "pod-1"}
	c := ConsumerSubmission{ProtocolVersion: 2, TenantID: a.TenantID, InstanceID: a.InstanceID, SubmissionID: a.SubmissionID, Generation: a.Generation, AttachmentID: a.ID, ClusterID: a.ClusterID, Namespace: a.Namespace, State: "open", SnapshotRevision: 1, DesiredRunState: "running", CurrentPods: []PodReference{{Reference: pod}}}
	return a, c
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

type managedConsumer struct {
	value ConsumerSubmission
	err   error
	calls int
}

func (c *managedConsumer) GetSubmission(context.Context, Attachment) (ConsumerSubmission, error) {
	c.calls++
	return c.value, c.err
}

type managedAddressReader struct {
	value   AttachmentAddressObservation
	err     error
	calls   int
	current Attachment
}

func (p *managedAddressReader) ObserveAttachmentAddresses(_ context.Context, a Attachment, _ ConsumerSubmission) (AttachmentAddressObservation, error) {
	p.calls++
	p.current = a
	return p.value, p.err
}
func runCurrentAttachment(t *testing.T, a Attachment, c *managedConsumer, p *managedAddressReader) AttachmentProgress {
	t.Helper()
	r := &managedWorkRepository{work: AttachmentWork{Attachment: a, Now: time.Now()}}
	w, err := NewAttachmentWorker(r, c, p, "44444444-4444-4444-8444-444444444444", DefaultWorkerPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	return r.progress
}

func TestManagedSnapshotOrdersOnlyCurrentManagementRevision(t *testing.T) {
	a, c := managedSnapshotFixture()
	a.SnapshotRevision = 2
	a.ConsumerSnapshot = &c
	if got := CompareConsumerSnapshot(a, c); got != SnapshotStale {
		t.Fatal("old revision accepted", got)
	}
	c.SnapshotRevision = 2
	c.DesiredRunState = "stopped"
	c.CurrentPods = nil
	if got := CompareConsumerSnapshot(a, c); got != SnapshotAccepted {
		t.Fatal("current owner output required a historical hash", got)
	}
	closing := c
	closing.State, closing.FinalizationID = "closing", "33333333-3333-4333-8333-333333333333"
	a.ConsumerSnapshot, a.FinalizationID = &closing, closing.FinalizationID
	c.SnapshotRevision++
	if CompareConsumerSnapshot(a, c) != SnapshotConflict {
		t.Fatal("closed intent was reopened")
	}
}

func TestManagedAttachmentUsesCurrentPodWithoutAddressGate(t *testing.T) {
	a, c := managedSnapshotFixture()
	reader := &managedAddressReader{}
	got := runCurrentAttachment(t, a, &managedConsumer{value: c}, reader)
	if got.State != Attached || got.PodUID != c.CurrentPods[0].Reference.UID || !got.Observed || got.Reason != "" {
		t.Fatal(got)
	}
	if reader.calls != 1 || got.Addresses == nil || len(got.Addresses.CurrentIPs) != 0 {
		t.Fatal("empty address output blocked management attachment", got)
	}
}

func TestManagedAttachmentReadErrorPreservesCurrentFacts(t *testing.T) {
	a, c := managedSnapshotFixture()
	a.State, a.PodName, a.PodUID = Attached, "instance-0", "pod-1"
	a.CurrentIPs = []string{"10.42.1.42"}
	a.Reason = Reason("reported-reason")
	reader := &managedAddressReader{}
	got := runCurrentAttachment(t, a, &managedConsumer{err: errors.New("consumer unavailable")}, reader)
	if got.State != a.State || got.Reason != a.Reason || got.PodUID != a.PodUID || got.Addresses != nil || got.Observed || got.ObservationError != ConsumerUnavailable || reader.calls != 0 {
		t.Fatal(got)
	}
	reader.value = AttachmentAddressObservation{CurrentIPs: a.CurrentIPs}
	reader.err = errors.New("Kubernetes unavailable")
	got = runCurrentAttachment(t, a, &managedConsumer{value: c}, reader)
	if got.State != Attached || got.Reason != "" || got.ObservationError != ProviderUnavailable || got.Addresses.CurrentIPs[0] != a.CurrentIPs[0] {
		t.Fatal("address read error changed management state", got)
	}
}

func TestManagedAttachmentReplacesOnlyTheCurrentPod(t *testing.T) {
	a, c := managedSnapshotFixture()
	a.State, a.PodName, a.PodUID = Attached, "old-0", "old-uid"
	a.CurrentVNicName, a.CurrentVNicUID, a.CurrentMACAddress = "old-nic", "old-nic-uid", "02:00:00:00:00:01"
	a.CurrentIPs = []string{"10.42.1.42"}
	reader := &managedAddressReader{}
	got := runCurrentAttachment(t, a, &managedConsumer{value: c}, reader)
	if got.State != Attached || got.PodUID != "pod-1" || reader.current.CurrentVNicName != "" || len(reader.current.CurrentIPs) != 0 {
		t.Fatal("new current Pod inherited old output", got, reader.current)
	}
	c.SnapshotRevision++
	c.DesiredRunState, c.CurrentPods = "stopped", nil
	reader.calls = 0
	got = runCurrentAttachment(t, a, &managedConsumer{value: c}, reader)
	if got.State != Reserved || got.PodUID != "" || reader.calls != 0 || got.Addresses == nil || len(got.Addresses.CurrentIPs) != 0 {
		t.Fatal("stopped consumer retained current output", got)
	}
}

func TestManagedAttachmentProtocolErrorCanRecoverFromCurrentSnapshot(t *testing.T) {
	a, c := managedSnapshotFixture()
	wrong := c
	wrong.TenantID = "different-tenant"
	got := runCurrentAttachment(t, a, &managedConsumer{value: wrong}, &managedAddressReader{})
	if got.Reason != AttachmentProtocol || got.State != Reserved {
		t.Fatal(got)
	}
	a.Reason = got.Reason
	got = runCurrentAttachment(t, a, &managedConsumer{value: c}, &managedAddressReader{})
	if got.State != Attached || got.Reason != "" {
		t.Fatal("old protocol reason permanently fenced the consumer", got)
	}
}
