package data_test

import (
	"context"
	"github.com/google/uuid"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network"
	"k8s.io/client-go/tools/clientcmd"
	"testing"
	"time"
)

type lbKnownOwners map[string]biz.Attachment

func (o lbKnownOwners) GetSubmission(_ context.Context, a biz.Attachment) (biz.ConsumerSubmission, error) {
	known, ok := o[a.ID]
	if !ok {
		return biz.ConsumerSubmission{}, biz.Fail(biz.ResourceNotFound, "unknown controlled owner submission")
	}
	c := consumerFor(known, "open", "")
	c.PodUIDs = []string{known.PodUID}
	return c, nil
}
func addLBBackend(t *testing.T, f *lbAdmissionFixture, subnet biz.Subnet, name, address string) biz.LoadBalancerBackendInput {
	t.Helper()
	attachments := biz.NewAttachments(f.f.p)
	a, err := attachments.Prepare(f.f.ctx, biz.PrepareAttachment{TenantID: f.f.tenant, VPCID: f.vpc.ID, SubnetID: subnet.ID, InstanceID: name, Slot: "primary", RequestKey: name, SubmissionID: uuid.NewString(), Generation: 1, ClusterID: "test-cluster", Namespace: "tenant-" + f.f.tenant})
	if err != nil {
		t.Fatal(err)
	}
	podUID, nicUID, ipUID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	pod := attachmentPod(a, name, podUID)
	pod["status"] = map[string]any{"podIP": address}
	f.api.Change("pods", pod, false)
	owner := func(kind, version, name, id string) []any {
		return []any{map[string]any{"kind": kind, "apiVersion": version, "name": name, "uid": id}}
	}
	f.api.Change("vnics", map[string]any{"apiVersion": "networking.kubercloud.com/v1", "kind": "VNic", "metadata": map[string]any{"namespace": a.Namespace, "name": name + "-nic", "uid": nicUID, "ownerReferences": owner("Pod", "v1", name, podUID)}, "spec": map[string]any{"type": "VETH", "subnet": a.Plan.PrimaryNetworkRef}}, false)
	f.api.Change("vnicips", map[string]any{"apiVersion": "networking.kubercloud.com/v1", "kind": "VNicIP", "metadata": map[string]any{"namespace": a.Namespace, "name": name + "-ip", "uid": ipUID, "ownerReferences": owner("VNic", "networking.kubercloud.com/v1", name+"-nic", nicUID)}, "spec": map[string]any{"subnet": a.Plan.PrimaryNetworkRef, "vNic": name + "-nic", "ipAddress": address}, "status": map[string]any{"vNic": name + "-nic"}}, false)
	a, err = attachments.Confirm(f.f.ctx, biz.ConfirmAttachment{TenantID: a.TenantID, AttachmentID: a.ID, ExpectedVersion: a.Version, ClusterID: a.ClusterID, Namespace: a.Namespace, PodName: name, PodUID: podUID})
	if err != nil {
		t.Fatal(err)
	}
	var originalID string
	if err = f.f.owner.QueryRow(f.f.ctx, `SELECT attachment_id FROM network_attachments WHERE tenant_id=$1 AND instance_id='lb-backend'`, f.f.tenant).Scan(&originalID); err != nil {
		t.Fatal(err)
	}
	original, err := attachments.Get(f.f.ctx, f.f.tenant, originalID)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", f.kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := data.NewKCProvider(f.f.p, cfg)
	if err != nil {
		t.Fatal(err)
	}
	policy := biz.DefaultWorkerPolicy()
	policy.ObserveEvery = time.Millisecond
	worker, err := biz.NewAttachmentWorker(f.f.p, lbKnownOwners{a.ID: a, original.ID: original}, provider, uuid.NewString(), policy)
	if err != nil {
		t.Fatal(err)
	}
	awaitNET05A(t, 3*time.Second, func() bool {
		if _, err := worker.Step(f.f.ctx); err != nil {
			t.Fatal(err)
		}
		observed, err := attachments.Get(f.f.ctx, f.f.tenant, a.ID)
		return err == nil && observed.State == biz.Attached && len(observed.CurrentIPs) == 1
	})
	return biz.LoadBalancerBackendInput{SubnetID: subnet.ID, Address: address, Port: 8080}
}
func TestLBUpdateRetainsRemovedSubnetUntilRouteAndBackendCleanup(t *testing.T) {
	c := &lbControllerFixture{}
	f := newLBAdmissionFixture(t, c.http)
	replacement := addLBBackend(t, f, f.subnet, "replacement-backend", "10.42.1.2")
	created, err := f.lbs.Create(f.f.ctx, f.request)
	if err != nil {
		t.Fatal(err)
	}
	lb := lbState(t, f, created.LoadBalancer.ID, biz.Available)
	var ns, oldBackend, route string
	if err = f.f.owner.QueryRow(f.f.ctx, `SELECT namespace,provider_name FROM network_lb_components WHERE tenant_id=$1 AND lb_id=$2 AND kind='backend'`, f.f.tenant, lb.ID).Scan(&ns, &oldBackend); err != nil {
		t.Fatal(err)
	}
	if err = f.f.owner.QueryRow(f.f.ctx, `SELECT provider_name FROM network_lb_components WHERE tenant_id=$1 AND lb_id=$2 AND kind='route'`, f.f.tenant, lb.ID).Scan(&route); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.holdCleanup = true
	c.mu.Unlock()
	updated, err := f.lbs.Update(f.f.ctx, biz.UpdateLoadBalancer{ID: lb.ID, ExpectedVersion: lb.Version, IdempotencyKey: "replace-set", LoadBalancerMutableInput: biz.LoadBalancerMutableInput{Health: f.request.Health, Name: "replacement", Backends: []biz.LoadBalancerBackendInput{replacement}}})
	if err != nil {
		t.Fatal(err)
	}
	f.f.drive(t, func() bool {
		obj := f.api.Object("backends", ns, oldBackend)
		return obj != nil && obj["metadata"].(map[string]any)["deletionTimestamp"] != nil
	})
	old := f.api.Object("backends", ns, oldBackend)
	refs := f.api.Object("httproutes", ns, route)["spec"].(map[string]any)["rules"].([]any)[0].(map[string]any)["backendRefs"].([]any)
	if len(refs) != 1 || refs[0].(map[string]any)["name"] == oldBackend {
		t.Fatal("old backend deleted before new Route was observed", refs)
	}
	var occupied int
	if err = f.f.owner.QueryRow(f.f.ctx, `SELECT count(*) FROM network_lb_subnet_refs WHERE tenant_id=$1 AND lb_id=$2 AND subnet_id=$3 AND released_at IS NULL`, f.f.tenant, lb.ID, f.backendSubnet.ID).Scan(&occupied); err != nil || occupied != 1 {
		t.Fatal("finalizing backend released subnet", occupied, err)
	}
	lb, err = f.lbs.Get(f.f.ctx, "", lb.ID)
	if err != nil || lb.AppliedVersion != 1 || lb.DesiredVersion != 2 {
		t.Fatal("partial update lost old applied version", lb, err)
	}
	if _, err = f.f.n.DeleteSubnet(f.f.ctx, f.f.tenant, f.backendSubnet.ID); biz.ReasonOf(err) != biz.ResourceInUse {
		t.Fatal("backend subnet deleted early", err)
	}
	// Only the simulated owner completes its finalizer; Network never removes it.
	f.api.Change("backends", old, true)
	c.mu.Lock()
	c.holdCleanup = false
	c.mu.Unlock()
	f.f.drive(t, func() bool {
		lb, err = f.lbs.Get(f.f.ctx, "", lb.ID)
		return err == nil && lb.AppliedVersion == 2 && lb.ConfigurationState == "configured"
	})
	if err = f.f.owner.QueryRow(f.f.ctx, `SELECT count(*) FROM network_lb_subnet_refs WHERE tenant_id=$1 AND lb_id=$2 AND subnet_id=$3 AND released_at IS NULL`, f.f.tenant, lb.ID, f.backendSubnet.ID).Scan(&occupied); err != nil || occupied != 0 {
		t.Fatal("retired backend subnet leaked", occupied, err)
	}
	op, err := f.lbs.GetOperation(f.f.ctx, "", updated.Operation.ID)
	if err != nil || op.State != biz.Succeeded {
		t.Fatal(op, err)
	}
	if _, err = f.lbs.Delete(f.f.ctx, "", lb.ID); err != nil {
		t.Fatal(err)
	}
	lbState(t, f, lb.ID, biz.Deleted)
	if f.api.Object("pods", ns, "lb-backend") == nil || f.api.Object("pods", ns, "replacement-backend") == nil {
		t.Fatal("LB took over instance-owner Pods")
	}
}

// Controlled owner receipts remain independent of the Attachment being queried.
type lbFinalizedOwners map[string]biz.ConsumerSubmission

func (o lbFinalizedOwners) GetSubmission(_ context.Context, a biz.Attachment) (biz.ConsumerSubmission, error) {
	value, ok := o[a.ID]
	if !ok {
		return biz.ConsumerSubmission{}, biz.Fail(biz.ResourceNotFound, "unknown controlled finalization")
	}
	return value, nil
}
