package instance

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/instance"
	networkbiz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
)

type adapterNetworkRepository struct {
	networkbiz.NetworkRepository
	subnet   networkbiz.Subnet
	vpc      networkbiz.VPC
	err      error
	getCalls int
}

func (r *adapterNetworkRepository) GetSubnet(context.Context, string, string) (networkbiz.Subnet, error) {
	r.getCalls++
	return r.subnet, r.err
}
func (r *adapterNetworkRepository) GetVPC(context.Context, string, string) (networkbiz.VPC, error) {
	return r.vpc, r.err
}

type adapterAttachmentRepository struct {
	value   networkbiz.Attachment
	err     error
	prepare networkbiz.PrepareAttachment
	confirm networkbiz.ConfirmAttachment
	release networkbiz.ReleaseAttachment
}

func (r *adapterAttachmentRepository) PrepareAttachment(_ context.Context, v networkbiz.PrepareAttachment) (networkbiz.Attachment, error) {
	r.prepare = v
	return r.value, r.err
}
func (r *adapterAttachmentRepository) GetAttachment(context.Context, string, string) (networkbiz.Attachment, error) {
	return r.value, r.err
}
func (r *adapterAttachmentRepository) ConfirmAttachment(_ context.Context, v networkbiz.ConfirmAttachment) (networkbiz.Attachment, error) {
	r.confirm = v
	return r.value, r.err
}
func (r *adapterAttachmentRepository) ReleaseAttachment(_ context.Context, v networkbiz.ReleaseAttachment) (networkbiz.Attachment, error) {
	r.release = v
	return r.value, r.err
}
func (r *adapterAttachmentRepository) ListAttachmentConsumers(context.Context, string, networkbiz.AttachmentConsumerFilter) ([]networkbiz.AttachmentConsumerRow, int64, error) {
	return nil, 0, r.err
}
func instanceNetworkFixture(t *testing.T) (*InstanceNetwork, biz.Instance, *adapterNetworkRepository, *adapterAttachmentRepository) {
	t.Helper()
	i := configuredInstance(t)
	i.Spec.Network = &biz.NetworkSelection{VPCID: "vpc_" + strings.Repeat("a", 32), SubnetID: "subnet_" + strings.Repeat("b", 32)}
	i.Network = &biz.NetworkRuntime{SubmissionID: "22222222-2222-4222-8222-222222222222", RequestKey: "primary-stable", Generation: 1, SnapshotRevision: 4, State: "open", DesiredRunState: "running", AttachmentID: "att_" + strings.Repeat("c", 32), AttachmentVersion: 7, BindingRevision: "binding-original", PrimaryNetworkRef: i.Spec.Namespace + "/subnet-original"}
	i.PodName = "instance-0"
	i.PodUID = "pod-current"
	now := time.Now().UTC()
	repo := &adapterNetworkRepository{subnet: networkbiz.Subnet{ID: i.Spec.Network.SubnetID, TenantID: i.TenantID, VPCID: i.Spec.Network.VPCID, State: networkbiz.Available, ObservedAt: &now}, vpc: networkbiz.VPC{ID: i.Spec.Network.VPCID, TenantID: i.TenantID, State: networkbiz.Available, ObservedAt: &now}}
	uc, err := networkbiz.NewNetwork(repo, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	attachments := &adapterAttachmentRepository{value: networkbiz.Attachment{ID: i.Network.AttachmentID, TenantID: i.TenantID, InstanceID: i.ID, VPCID: i.Spec.Network.VPCID, SubnetID: i.Spec.Network.SubnetID, SubmissionID: i.Network.SubmissionID, Generation: 1, ClusterID: i.Spec.ClusterID, Namespace: i.Spec.Namespace, WorkloadMode: "controller_managed", State: networkbiz.Attached, Version: 7, SnapshotRevision: 4, PodName: i.PodName, PodUID: i.PodUID, BindingRevision: i.Network.BindingRevision, ObservedAt: &now}}
	attachments.value.Plan = &networkbiz.PodPrimaryPlan{FormatVersion: 1, WorkloadMode: "controller_managed", Namespace: i.Spec.Namespace, PrimaryNetworkRef: i.Network.PrimaryNetworkRef, TenantID: i.TenantID, InstanceID: i.ID, AttachmentID: i.Network.AttachmentID, SubmissionID: i.Network.SubmissionID, Generation: 1}
	adapter, err := NewInstanceNetwork(uc, networkbiz.NewAttachments(attachments), i.Spec.ClusterID, "tenant-")
	if err != nil {
		t.Fatal(err)
	}
	return adapter, i, repo, attachments
}

func TestInstanceNetworkAdapterUsesStableSubmissionAndSnapshotFences(t *testing.T) {
	adapter, i, _, repo := instanceNetworkFixture(t)
	ctx := context.Background()
	if err := adapter.ValidateSelection(ctx, i.TenantID, i.Spec.ClusterID, i.Spec.Namespace, *i.Spec.Network); err != nil {
		t.Fatal(err)
	}
	binding, err := adapter.Prepare(ctx, i)
	if err != nil || binding.AttachmentID != i.Network.AttachmentID || binding.PrimaryNetworkRef != i.Network.PrimaryNetworkRef || binding.Version != 7 {
		t.Fatal("real Network usecase mapping", binding, err)
	}
	if repo.prepare.RequestKey != i.Network.RequestKey || repo.prepare.SubmissionID != i.Network.SubmissionID || repo.prepare.Generation != 1 || repo.prepare.WorkloadMode != "controller_managed" || repo.prepare.TenantID != i.TenantID {
		t.Fatal("adapter invented mutable network intent", repo.prepare)
	}
	o, err := adapter.Observe(ctx, i)
	if err != nil || o.State != "attached" || o.PodUID != i.PodUID || o.SnapshotRevision != 4 || o.Version != 7 {
		t.Fatal("attachment observation lost fences", o, err)
	}
	if err := adapter.Confirm(ctx, i); err != nil {
		t.Fatal(err)
	}
	if repo.confirm.ExpectedSnapshotRevision != i.Network.SnapshotRevision || repo.confirm.ExpectedVersion != 7 || repo.confirm.PodUID != i.PodUID {
		t.Fatal("Confirm omitted persisted revision or Pod", repo.confirm)
	}
	i.Network.FinalizationID = "33333333-3333-4333-8333-333333333333"
	i.Network.State = "closed"
	if done, err := adapter.Release(ctx, i); err != nil || done {
		t.Fatal(err)
	}
	if repo.release.FinalizationID != i.Network.FinalizationID || repo.release.ExpectedVersion != 7 {
		t.Fatal("Release changed stable finalization", repo.release)
	}
}
func TestInstanceNetworkAdapterRejectsScopeAndMapsDomainErrors(t *testing.T) {
	adapter, i, network, attachments := instanceNetworkFixture(t)
	ctx := context.Background()
	if err := adapter.ValidateSelection(ctx, i.TenantID, "22222222-2222-4222-8222-222222222222", i.Spec.Namespace, *i.Spec.Network); !errors.Is(err, biz.ErrNetworkConflict) {
		t.Fatal("foreign placement accepted", err)
	}
	if network.getCalls != 0 {
		t.Fatal("placement error queried network resources")
	}
	network.subnet.ObservedAt = nil
	if err := adapter.ValidateSelection(ctx, i.TenantID, i.Spec.ClusterID, i.Spec.Namespace, *i.Spec.Network); err != nil {
		t.Fatal("management selection depends on observation age", err)
	}
	old := time.Now().Add(-time.Hour)
	attachments.value.ObservedAt = &old
	if _, err := adapter.Observe(ctx, i); err != nil {
		t.Fatal("stored management result depends on observation age", err)
	}
	attachments.err = networkbiz.Fail(networkbiz.ResourceNotFound, "private other tenant detail")
	if _, err := adapter.Observe(ctx, i); !errors.Is(err, biz.ErrNetworkNotFound) || strings.Contains(err.Error(), "private other tenant detail") {
		t.Fatal("unsafe domain error mapping", err)
	}
	i.Network.AttachmentID = ""
	if _, err := adapter.Observe(ctx, i); !errors.Is(err, biz.ErrNetworkUnavailable) {
		t.Fatal("unacknowledged Prepare treated as no resource", err)
	}
}

type instanceNetworkReader struct {
	value biz.Instance
	err   error
}

func (r instanceNetworkReader) Get(context.Context, string, string) (biz.Instance, error) {
	return r.value, r.err
}

type fallbackNetworkConsumer struct{ calls int }

func (f *fallbackNetworkConsumer) GetSubmission(context.Context, networkbiz.Attachment) (networkbiz.ConsumerSubmission, error) {
	f.calls++
	return networkbiz.ConsumerSubmission{ProtocolVersion: 1}, nil
}
func TestInstanceNetworkConsumerRoutesOnlyDurableManagedIdentity(t *testing.T) {
	_, i, _, attachments := instanceNetworkFixture(t)
	set := biz.WorkloadReference{APIVersion: "apps/v1", Kind: "StatefulSet", Namespace: i.Spec.Namespace, Name: i.Spec.NotebookName, UID: "set-uid"}
	i.Network.CurrentPods = []biz.PodReference{{Reference: biz.WorkloadReference{APIVersion: "v1", Kind: "Pod", Namespace: i.Spec.Namespace, Name: i.PodName, UID: i.PodUID}, Owner: set}}
	fallback := &fallbackNetworkConsumer{}
	consumer, err := NewInstanceNetworkConsumer(instanceNetworkReader{value: i}, fallback)
	if err != nil {
		t.Fatal(err)
	}
	a := attachments.value
	c, err := consumer.GetSubmission(context.Background(), a)
	if err != nil || c.ProtocolVersion != 2 || c.SnapshotRevision != 4 || len(c.CurrentPods) != 1 || c.CurrentPods[0].Reference.UID != i.PodUID || fallback.calls != 0 {
		t.Fatal("managed snapshot did not come from Instance persistence", c, err)
	}
	a.WorkloadMode = "single_pod"
	c, err = consumer.GetSubmission(context.Background(), a)
	if err != nil || c.ProtocolVersion != 1 || fallback.calls != 1 {
		t.Fatal("existing consumer was replaced", c, err)
	}
	a = attachments.value
	a.SubmissionID = "44444444-4444-4444-8444-444444444444"
	if _, err = consumer.GetSubmission(context.Background(), a); err == nil || fallback.calls != 1 {
		t.Fatal("foreign managed submission routed or exposed", err)
	}
	i.Network.AttachmentID = ""
	consumer, err = NewInstanceNetworkConsumer(instanceNetworkReader{value: i}, fallback)
	if err != nil {
		t.Fatal(err)
	}
	a = attachments.value
	if _, err = consumer.GetSubmission(context.Background(), a); !errors.Is(err, biz.ErrNetworkUnavailable) {
		t.Fatal("lost Prepare acknowledgement fabricated absence", err)
	}
}
