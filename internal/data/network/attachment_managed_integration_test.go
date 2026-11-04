package data_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
)

func managedConsumerFor(a biz.Attachment, uid string, revision int64) biz.ConsumerSubmission {
	c := biz.ConsumerSubmission{ProtocolVersion: 2, TenantID: a.TenantID, InstanceID: a.InstanceID, SubmissionID: a.SubmissionID, Generation: a.Generation, AttachmentID: a.ID, ClusterID: a.ClusterID, Namespace: a.Namespace, State: "open", SnapshotRevision: revision, DesiredRunState: "running"}
	if uid != "" {
		c.CurrentPods = []biz.PodReference{{Reference: biz.WorkloadReference{APIVersion: "v1", Kind: "Pod", Namespace: a.Namespace, Name: "notebook-0", UID: uid}}}
	}
	return c
}
func managedPodObject(a biz.Attachment, uid string) map[string]any {
	pod := attachmentPod(a, "notebook-0", uid)
	pod["status"] = map[string]any{"podIP": "10.42.1.2"}
	return pod
}
func setManagedConsumer(f attachmentFixture, c biz.ConsumerSubmission) {
	f.consumer.mu.Lock()
	defer f.consumer.mu.Unlock()
	f.consumer.err, f.consumer.value = nil, c
}

func TestManagedAttachmentPGCurrentPodConfirmationAndReplacement(t *testing.T) {
	f := newAttachmentFixture(t)
	f.r.WorkloadMode = "controller_managed"
	a := f.prepare(t)
	c := managedConsumerFor(a, "pod-1", 1)
	setManagedConsumer(f, c)
	// No Notebook, StatefulSet, VNic or VNicIP exists in this fixture.
	a = f.step(t, a.ID)
	if a.State != biz.Attached || a.PodUID != "pod-1" || a.SnapshotRevision != 1 || len(a.CurrentIPs) != 0 {
		t.Fatal(a)
	}
	confirm := biz.ConfirmAttachment{TenantID: a.TenantID, AttachmentID: a.ID, ExpectedVersion: a.Version, ExpectedSnapshotRevision: c.SnapshotRevision, ClusterID: a.ClusterID, Namespace: a.Namespace, PodName: "notebook-0", PodUID: "pod-1"}
	a, err := f.a.Confirm(context.Background(), confirm)
	if err != nil || a.State != biz.Attached || a.ConfirmedPodUID != "pod-1" {
		t.Fatal(a, err)
	}
	c.SnapshotRevision++
	c.CurrentPods[0].Reference.UID = "pod-2"
	setManagedConsumer(f, c)
	a = f.step(t, a.ID)
	if a.State != biz.Attached || a.PodUID != "pod-2" || a.ConfirmUID != "" {
		t.Fatal("old confirmation stuck to replacement", a)
	}
	if _, err = f.a.Confirm(context.Background(), confirm); biz.ReasonOf(err) != biz.VersionConflict {
		t.Fatal("old revision confirmed new current Pod", err)
	}
}

func TestManagedAttachmentPGReadErrorPreservesStateAndAddresses(t *testing.T) {
	f := newAttachmentFixture(t)
	f.r.WorkloadMode = "controller_managed"
	a := f.prepare(t)
	c := managedConsumerFor(a, "pod-1", 1)
	f.api.Mu.Lock()
	f.api.Objects["pods/"+a.Namespace+"/notebook-0"] = managedPodObject(a, "pod-1")
	f.api.Mu.Unlock()
	setManagedConsumer(f, c)
	a = f.step(t, a.ID)
	if len(a.CurrentIPs) != 1 {
		t.Fatal(a)
	}
	f.consumer.mu.Lock()
	f.consumer.err = errors.New("owner unavailable")
	f.consumer.mu.Unlock()
	before := a
	a = f.step(t, a.ID)
	var diagnostic string
	if err := f.owner.QueryRow(context.Background(), `SELECT observation_error FROM network_attachments WHERE tenant_id=$1 AND attachment_id=$2`, a.TenantID, a.ID).Scan(&diagnostic); err != nil {
		t.Fatal(err)
	}
	if a.State != before.State || a.Reason != before.Reason || a.PodUID != before.PodUID || len(a.CurrentIPs) != 1 || a.CurrentIPs[0] != before.CurrentIPs[0] || diagnostic != string(biz.ConsumerUnavailable) || !a.ObservedAt.Equal(*before.ObservedAt) {
		t.Fatal(a, diagnostic)
	}
}

func TestManagedAttachmentPGStopAndClosedDoNotAuditResidualResources(t *testing.T) {
	f := newAttachmentFixture(t)
	f.r.WorkloadMode = "controller_managed"
	a := f.prepare(t)
	c := managedConsumerFor(a, "pod-1", 1)
	f.api.Mu.Lock()
	f.api.Objects["pods/"+a.Namespace+"/notebook-0"] = managedPodObject(a, "pod-1")
	f.api.Objects["vnicips/"+a.Namespace+"/old"] = map[string]any{"apiVersion": "networking.kubercloud.com/v1", "kind": "VNicIP", "metadata": map[string]any{"name": "old", "namespace": a.Namespace, "uid": "old-ip"}}
	f.api.Mu.Unlock()
	setManagedConsumer(f, c)
	a = f.step(t, a.ID)
	c.SnapshotRevision++
	c.DesiredRunState, c.CurrentPods = "stopped", nil
	setManagedConsumer(f, c)
	a = f.step(t, a.ID)
	if a.State != biz.Reserved || a.PodUID != "" || len(a.CurrentIPs) != 0 {
		t.Fatal(a)
	}
	c.SnapshotRevision++
	c.State, c.DesiredRunState, c.FinalizationID = "closed", "deleted", uuid.NewString()
	setManagedConsumer(f, c)
	a = f.step(t, a.ID)
	if a.State != biz.Released || a.Plan != nil || a.ReleasedAt == nil {
		t.Fatal(a)
	}
	if _, err := f.n.DeleteSubnet(context.Background(), a.TenantID, a.SubnetID); err != nil {
		t.Fatal("released management slot retained a provider cleanup gate", err)
	}
}
