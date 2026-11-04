package data_test

import (
	"context"
	"encoding/json"
	"github.com/google/uuid"
	networkv1 "github.com/zhangzhe-ctrl/ani-resource-service/api/network/v1"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/service/network"
	controlled "github.com/zhangzhe-ctrl/ani-resource-service/tests/net05a/provider"
	"k8s.io/client-go/tools/clientcmd"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

type lbAdmissionFixture struct {
	kubeconfig, runtimeDSN string
	f                      *egressFixture
	api                    *controlled.Server
	lbs                    *biz.LoadBalancers
	rpc                    *service.TenantLoadBalancerService
	vpc                    biz.VPC
	subnet, backendSubnet  biz.Subnet
	request                biz.CreateLoadBalancer
}

// All parents are created by product use cases and the actual KC adapter. The
// controlled API plays the instance-owner/Kubernetes side, not Network admission.
func newLBAdmissionFixture(t *testing.T, interceptors ...func(http.ResponseWriter, *http.Request, *controlled.Server) bool) *lbAdmissionFixture {
	t.Helper()
	var intercept func(http.ResponseWriter, *http.Request, *controlled.Server) bool
	if len(interceptors) > 0 {
		intercept = interceptors[0]
	}
	f, api, db, kube := newBaseKCFixture(t, intercept)
	vpc := baseState(t, f, createBaseVPC(t, f, "lb-parent").ID, biz.Available)
	makeSubnet := func(name, cidr string) biz.Subnet {
		baseState(t, f, vpc.ID, biz.Available)
		s, err := f.n.CreateSubnet(f.ctx, biz.CreateSubnet{TenantID: f.tenant, VPCID: vpc.ID, Name: name, CIDR: cidr, IdempotencyKey: name})
		if err != nil {
			t.Fatal(err)
		}
		return driveSubnet(t, f.n, f.w, s, biz.Available)
	}
	entry, backend := makeSubnet("entry", "10.42.1.0/24"), makeSubnet("backend", "10.42.2.0/24")
	config, err := clientcmd.BuildConfigFromFlags("", kube)
	if err != nil {
		t.Fatal(err)
	}
	// Parent/attachment setup uses the actual adapter without testing throttling.
	config.QPS, config.Burst = 1000, 1000
	provider, err := data.NewKCProvider(f.p, config)
	if err != nil {
		t.Fatal(err)
	}
	attachments := biz.NewAttachments(f.p)
	a, err := attachments.Prepare(f.ctx, biz.PrepareAttachment{TenantID: f.tenant, VPCID: vpc.ID, SubnetID: backend.ID, InstanceID: "lb-backend", Slot: "primary", RequestKey: "backend", SubmissionID: uuid.NewString(), Generation: 1, ClusterID: "test-cluster", Namespace: "tenant-" + f.tenant})
	if err != nil {
		t.Fatal(err)
	}
	podUID, nicUID, ipUID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	pod := attachmentPod(a, "lb-backend", podUID)
	pod["status"] = map[string]any{"podIP": "10.42.2.2"}
	api.Change("pods", pod, false)
	owner := func(kind, apiVersion, name, id string) []any {
		return []any{map[string]any{"kind": kind, "apiVersion": apiVersion, "name": name, "uid": id}}
	}
	api.Change("vnics", map[string]any{"apiVersion": "networking.kubercloud.com/v1", "kind": "VNic", "metadata": map[string]any{"namespace": a.Namespace, "name": "lb-backend-nic", "uid": nicUID, "ownerReferences": owner("Pod", "v1", "lb-backend", podUID)}, "spec": map[string]any{"type": "VETH", "subnet": a.Plan.PrimaryNetworkRef}}, false)
	api.Change("vnicips", map[string]any{"apiVersion": "networking.kubercloud.com/v1", "kind": "VNicIP", "metadata": map[string]any{"namespace": a.Namespace, "name": "lb-backend-ip", "uid": ipUID, "ownerReferences": owner("VNic", "networking.kubercloud.com/v1", "lb-backend-nic", nicUID)}, "spec": map[string]any{"subnet": a.Plan.PrimaryNetworkRef, "vNic": "lb-backend-nic", "ipAddress": "10.42.2.2"}, "status": map[string]any{"vNic": "lb-backend-nic"}}, false)
	a, err = attachments.Confirm(f.ctx, biz.ConfirmAttachment{TenantID: a.TenantID, AttachmentID: a.ID, ExpectedVersion: a.Version, ClusterID: a.ClusterID, Namespace: a.Namespace, PodName: "lb-backend", PodUID: podUID})
	if err != nil {
		t.Fatal(err)
	}
	consumer := &attachmentConsumer{value: consumerFor(a, "open", "")}
	consumer.value.PodUIDs = []string{podUID}
	policy := biz.DefaultWorkerPolicy()
	policy.ObserveEvery = time.Millisecond
	worker, err := biz.NewAttachmentWorker(f.p, consumer, provider, uuid.NewString(), policy)
	if err != nil {
		t.Fatal(err)
	}
	awaitNET05A(t, 3*time.Second, func() bool {
		if _, err := worker.Step(f.ctx); err != nil {
			t.Fatal(err)
		}
		v, err := attachments.Get(f.ctx, f.tenant, a.ID)
		return err == nil && v.State == biz.Attached && len(v.CurrentIPs) == 1
	})
	// A diagnostic record is independent of admission and object reconciliation.
	if _, err = f.owner.Exec(f.ctx, `INSERT INTO network_lb_capabilities(cluster_id,ready,reason,observed_at) VALUES('test-cluster',true,'',clock_timestamp())`); err != nil {
		t.Fatal(err)
	}
	lbs, err := biz.NewLoadBalancers(f.p, biz.ContextEgressAuthorization{}, []byte(strings.Repeat("c", 32)))
	if err != nil {
		t.Fatal(err)
	}
	healthPort := uint32(8080)
	request := biz.CreateLoadBalancer{VPCID: vpc.ID, SubnetID: entry.ID, Exposure: "private", Flavor: "small", PrivateIP: "10.42.1.100", IdempotencyKey: "create", LoadBalancerMutableInput: biz.LoadBalancerMutableInput{Health: biz.LoadBalancerHealthInput{Port: &healthPort}, Name: "lb", Backends: []biz.LoadBalancerBackendInput{{SubnetID: backend.ID, Address: "10.42.2.2", Port: 8080}}}}
	return &lbAdmissionFixture{f: f, api: api, lbs: lbs, rpc: service.NewTenantLoadBalancerService(lbs), vpc: vpc, subnet: entry, backendSubnet: backend, request: request, kubeconfig: kube, runtimeDSN: db.RuntimeDSN}
}
func TestLBAdmissionAtomicIdentityReplayAndParentOccupancy(t *testing.T) {
	f := newLBAdmissionFixture(t)
	r, err := f.lbs.Create(f.f.ctx, f.request)
	if err != nil {
		t.Fatal(err)
	}
	if r.LoadBalancer.State != biz.Provisioning || r.LoadBalancer.DesiredVersion != 1 || r.LoadBalancer.AppliedVersion != 0 || r.Operation.ResourceID != r.LoadBalancer.ID || len(r.LoadBalancer.Backends) != 1 || r.LoadBalancer.Backends[0].AttachmentID == "" {
		t.Fatalf("invalid atomic receipt: %+v", r)
	}
	var refs, vips, operations int
	for table, out := range map[string]*int{"network_lb_subnet_refs": &refs, "network_lb_vip_intents": &vips, "network_operations": &operations} {
		if err = f.f.owner.QueryRow(f.f.ctx, "SELECT count(*) FROM "+table+" WHERE tenant_id=$1 AND lb_id=$2", f.f.tenant, r.LoadBalancer.ID).Scan(out); err != nil {
			t.Fatal(err)
		}
	}
	if refs != 2 || vips != 1 || operations != 1 {
		t.Fatalf("missing atomic occupancy: %d %d %d", refs, vips, operations)
	}
	if _, err = f.f.n.DeleteSubnet(f.f.ctx, f.f.tenant, f.subnet.ID); biz.ReasonOf(err) != biz.ResourceInUse {
		t.Fatal("entry not occupied", err)
	}
	if _, err = f.f.n.DeleteSubnet(f.f.ctx, f.f.tenant, f.backendSubnet.ID); biz.ReasonOf(err) != biz.ResourceInUse {
		t.Fatal("backend subnet not occupied", err)
	}
	if _, err = f.f.n.DeleteVPC(f.f.ctx, f.f.tenant, f.vpc.ID); biz.ReasonOf(err) != biz.ResourceInUse {
		t.Fatal("VPC not occupied", err)
	}
	// Corrupt dynamic readiness only in this controlled DB; permanent replay
	// must return its original receipt without rechecking these mutable facts.
	if _, err = f.f.owner.Exec(f.f.ctx, `UPDATE network_vpcs SET state='degraded' WHERE tenant_id=$1 AND vpc_id=$2`, f.f.tenant, f.vpc.ID); err != nil {
		t.Fatal(err)
	}
	replay, err := f.lbs.Create(f.f.ctx, f.request)
	if err != nil {
		t.Fatal(err)
	}
	x, _ := json.Marshal(r)
	y, _ := json.Marshal(replay)
	if string(x) != string(y) {
		t.Fatal("replay changed accepted resource/operation")
	}
	f.request.Name = "changed"
	if _, err = f.lbs.Create(f.f.ctx, f.request); biz.ReasonOf(err) != biz.IdempotencyConflict {
		t.Fatal(err)
	}
	other := biz.WithEgressCaller(context.Background(), biz.EgressCaller{TenantID: uuid.NewString()})
	if _, err = f.lbs.Get(other, "", r.LoadBalancer.ID); biz.ReasonOf(err) != biz.ResourceNotFound {
		t.Fatal("cross-tenant GET", err)
	}
	if _, err = f.lbs.GetOperation(other, "", r.Operation.ID); biz.ReasonOf(err) != biz.ResourceNotFound {
		t.Fatal("cross-tenant operation", err)
	}
	if _, err = f.rpc.GetLoadBalancerOperation(f.f.ctx, &networkv1.GetLoadBalancerOperationRequest{OperationId: r.Operation.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err = f.lbs.GetOperation(f.f.ctx, "", f.vpc.LastOperationID); biz.ReasonOf(err) != biz.ResourceNotFound {
		t.Fatal("non-LB operation exposed", err)
	}
	d, err := f.lbs.Delete(f.f.ctx, "", r.LoadBalancer.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d.LoadBalancer.State != biz.Deleted || d.Operation.State != biz.Succeeded {
		t.Fatal("never-dispatched product deletion did not complete atomically", d)
	}
	again, err := f.lbs.Delete(f.f.ctx, "", r.LoadBalancer.ID)
	if err != nil || d.Operation.ID != again.Operation.ID {
		t.Fatal("delete lost identity", err)
	}
	if _, err = f.lbs.Update(f.f.ctx, biz.UpdateLoadBalancer{ID: r.LoadBalancer.ID, ExpectedVersion: d.LoadBalancer.Version, IdempotencyKey: "closed", LoadBalancerMutableInput: f.request.LoadBalancerMutableInput}); biz.ReasonOf(err) != biz.ResourceBusy {
		t.Fatal("deletion did not close update", err)
	}
	if err = f.f.owner.QueryRow(f.f.ctx, `SELECT count(*) FROM network_lb_vip_intents WHERE tenant_id=$1 AND lb_id=$2 AND released_at IS NULL`, f.f.tenant, r.LoadBalancer.ID).Scan(&vips); err != nil || vips != 0 {
		t.Fatal("completed never-dispatched deletion retained VIP reservation", vips, err)
	}
}
func TestLBAdmissionRejectsCIDROnlyBackendAndDuplicateVIP(t *testing.T) {
	f := newLBAdmissionFixture(t)
	bad := f.request
	bad.Backends = slicesCloneLBInputs(f.request.Backends)
	bad.Backends[0].Address = "10.42.2.222"
	if _, err := f.lbs.Create(f.f.ctx, bad); biz.ReasonOf(err) != biz.BackendIdentityMismatch {
		t.Fatal("unallocated CIDR address accepted", err)
	}
	const concurrency = 5
	results := make(chan biz.LoadBalancerResult, concurrency)
	errs := make(chan error, concurrency)
	var group sync.WaitGroup
	for j := 0; j < concurrency; j++ {
		group.Add(1)
		go func() { defer group.Done(); v, err := f.lbs.Create(f.f.ctx, f.request); results <- v; errs <- err }()
	}
	group.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	id := ""
	for r := range results {
		if id == "" {
			id = r.LoadBalancer.ID
		}
		if id != r.LoadBalancer.ID {
			t.Fatal("concurrent key generated two LBs")
		}
	}
	r := f.request
	r.IdempotencyKey = "same-vip"
	if _, err := f.lbs.Create(f.f.ctx, r); biz.ReasonOf(err) != biz.VIPInUse {
		t.Fatal("VIP collision accepted", err)
	}
	var count int
	if err := f.f.owner.QueryRow(f.f.ctx, `SELECT count(*) FROM network_load_balancers WHERE tenant_id=$1`, f.f.tenant).Scan(&count); err != nil || count != 1 {
		t.Fatal("failed admission left orphan LB", count, err)
	}
}
func slicesCloneLBInputs(v []biz.LoadBalancerBackendInput) []biz.LoadBalancerBackendInput {
	return append([]biz.LoadBalancerBackendInput{}, v...)
}

func setLBCurrentBackendAddresses(t *testing.T, f *lbAdmissionFixture, lbID, attachmentID string, addresses []string) {
	t.Helper()
	if _, err := f.f.owner.Exec(f.f.ctx, `UPDATE network_attachments SET current_ips=$3 WHERE tenant_id=$1 AND attachment_id=$2`, f.f.tenant, attachmentID, addresses); err != nil {
		t.Fatal(err)
	}
	if err := f.f.p.NotifyResource(f.f.ctx, f.f.tenant, lbID); err != nil {
		t.Fatal(err)
	}
}
