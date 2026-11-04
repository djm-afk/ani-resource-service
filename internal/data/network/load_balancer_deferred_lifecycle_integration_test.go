package data_test

import (
	"context"
	"errors"
	"github.com/google/uuid"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	data "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/tools/clientcmd"
	"reflect"
	"strings"
	"testing"
	"time"
)

type deferredLBBinding struct{ id, namespace, name, uid string }

func deferredBinding(t *testing.T, f *lbAdmissionFixture, id string) deferredLBBinding {
	t.Helper()
	var binding deferredLBBinding
	if err := f.f.owner.QueryRow(f.f.ctx, `SELECT binding_id,namespace,provider_name,provider_uid FROM network_provider_bindings WHERE tenant_id=$1 AND lb_id=$2 AND resource_kind='load_balancer'`, f.f.tenant, id).Scan(&binding.id, &binding.namespace, &binding.name, &binding.uid); err != nil {
		t.Fatal(err)
	}
	return binding
}

func deferredListenerInput(listener biz.LoadBalancerListener, backends []biz.LoadBalancerBackendInput) biz.LoadBalancerListenerInput {
	h := listener.Health
	return biz.LoadBalancerListenerInput{ID: listener.ID, Name: listener.Name, Protocol: listener.Protocol, Port: &listener.Port, Parameters: listener.Parameters, Backends: backends, Health: biz.LoadBalancerHealthInput{Protocol: h.Protocol, Path: h.Path, Hostname: h.Hostname, Port: &h.Port, IntervalSeconds: &h.IntervalSeconds, TimeoutSeconds: &h.TimeoutSeconds, UnhealthyThreshold: &h.UnhealthyThreshold, HealthyThreshold: &h.HealthyThreshold}}
}

func deferredRetainMembers(listener biz.LoadBalancerListener) []biz.LoadBalancerBackendInput {
	var members []biz.LoadBalancerBackendInput
	for _, backend := range listener.Backends {
		weight := backend.Weight
		members = append(members, biz.LoadBalancerBackendInput{ID: backend.ID, SubnetID: backend.SubnetID, Address: backend.Address, Port: backend.Port, Weight: &weight})
	}
	return members
}

func deferredMutationCount(controller *lbControllerFixture) int {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	count := 0
	for _, request := range controller.requests {
		if strings.HasPrefix(request, "POST/") || strings.HasPrefix(request, "PATCH/") || strings.HasPrefix(request, "DELETE/") {
			count++
		}
	}
	return count
}

func deferredVersions(t *testing.T, lb biz.LoadBalancer, desired, target, applied int64) {
	t.Helper()
	if lb.DesiredVersion != desired || lb.ProviderTargetVersion != target || lb.AppliedVersion != applied {
		t.Fatalf("versions desired/target/applied=%d/%d/%d; want=%d/%d/%d", lb.DesiredVersion, lb.ProviderTargetVersion, lb.AppliedVersion, desired, target, applied)
	}
}

func deferredConfigured(t *testing.T, f *lbAdmissionFixture, id string, target int64) biz.LoadBalancer {
	t.Helper()
	var lb biz.LoadBalancer
	f.f.drive(t, func() bool {
		var err error
		lb, err = f.lbs.Get(f.f.ctx, "", id)
		return err == nil && lb.State == biz.Available && lb.ConfigurationState == "configured" && lb.AppliedVersion == target
	})
	return lb
}

func deferredClaimSnapshot(t *testing.T, f *lbAdmissionFixture, id string) biz.Work {
	t.Helper()
	// Only the scheduler's due time is advanced in this isolated test database.
	// Identity, pending actions and the lease itself still come from real Claim.
	if _, err := f.f.owner.Exec(f.f.ctx, `UPDATE network_reconciliations SET next_run_at=clock_timestamp()-interval '1 minute' WHERE tenant_id=$1 AND lb_id=$2`, f.f.tenant, id); err != nil {
		t.Fatal(err)
	}
	var snapshot biz.Work
	awaitNET05A(t, 3*time.Second, func() bool {
		work, found, err := f.f.p.Claim(f.f.ctx, uuid.NewString(), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if found && work.Resource.ID == id {
			snapshot = work
			return true
		}
		return false
	})
	return snapshot
}

func restartDeferredWorker(t *testing.T, f *lbAdmissionFixture) {
	t.Helper()
	f.f.stopObservation()
	repository := lbRecoveryRepository(t, f)
	config, err := clientcmd.BuildConfigFromFlags("", f.kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	config.QPS, config.Burst = 1000, 1000
	provider, err := data.NewKCProvider(repository, config)
	if err != nil {
		t.Fatal(err)
	}
	repository.UseVirtualSwitchInfrastructure(provider)
	observer, err := provider.EnableObservation(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- observer.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	policy := biz.DefaultWorkerPolicy()
	policy.ObserveEvery, policy.RetryMin, policy.RetryMax = 100*time.Millisecond, 5*time.Millisecond, 20*time.Millisecond
	worker, err := biz.NewWorker(repository, provider, uuid.NewString(), policy)
	if err != nil {
		t.Fatal(err)
	}
	f.f.p, f.f.w = repository, worker
	f.lbs, err = biz.NewLoadBalancers(repository, biz.ContextEgressAuthorization{}, []byte(strings.Repeat("c", 32)))
	if err != nil {
		t.Fatal(err)
	}
	awaitNET05A(t, 3*time.Second, func() bool { return observer.Snapshot()["source_synced"] == 1 })
}

// The business chain is production authorization, admission, PostgreSQL, lease
// worker and KC adapter. Only Kubernetes/controllers and the business instance
// owner are protocol fixtures; configured state does not assert live traffic.
func TestLBDeferredActualAdapterLifecycleRestartAndReactivation(t *testing.T) {
	controller := &lbControllerFixture{}
	f := newLBAdmissionFixture(t, controller.http)
	eip := f.f.eip(t, "deferred-entry-eip")
	request := deferredEmptyLBRequest(f)
	request.Exposure, request.PublicEIPID = "public_private", eip.ID
	before := deferredMutationCount(controller)
	created, err := f.lbs.Create(f.f.ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	lb := created.LoadBalancer
	deferredVersions(t, lb, 1, 0, 0)
	if created.Operation.State != biz.Succeeded || lb.ConfigurationState != "waiting_for_backends" || lb.ObservedAt != nil || deferredMutationCount(controller) != before {
		t.Fatal("empty create did not save without LB Provider writes", created)
	}
	initial := deferredBinding(t, f, lb.ID)
	if initial.uid != "" || f.api.Object("gateways", initial.namespace, initial.name) != nil {
		t.Fatal("empty creation created a running Gateway", initial)
	}
	claim := func(want string, released bool) {
		t.Helper()
		var state string
		var actualReleased bool
		if err := f.f.owner.QueryRow(f.f.ctx, `SELECT state,released_at IS NOT NULL FROM network_eip_claims WHERE tenant_id=$1 AND lb_id=$2`, f.f.tenant, lb.ID).Scan(&state, &actualReleased); err != nil || state != want || actualReleased != released {
			t.Fatal("EIP claim fact", state, actualReleased, want, released, err)
		}
	}
	occupancy := func(vips, entry, backend int) {
		t.Helper()
		var actualVIP, actualEntry, actualBackend int
		if err := f.f.owner.QueryRow(f.f.ctx, `SELECT (SELECT count(*) FROM network_lb_vip_intents WHERE tenant_id=$1 AND lb_id=$2 AND released_at IS NULL),(SELECT count(*) FROM network_lb_subnet_refs WHERE tenant_id=$1 AND lb_id=$2 AND subnet_id=$3 AND released_at IS NULL),(SELECT count(*) FROM network_lb_subnet_refs WHERE tenant_id=$1 AND lb_id=$2 AND subnet_id=$4 AND released_at IS NULL)`, f.f.tenant, lb.ID, f.subnet.ID, f.backendSubnet.ID).Scan(&actualVIP, &actualEntry, &actualBackend); err != nil || actualVIP != vips || actualEntry != entry || actualBackend != backend {
			t.Fatal("product reservation facts", actualVIP, actualEntry, actualBackend, vips, entry, backend, err)
		}
	}
	operation := func(id string) {
		t.Helper()
		op, err := f.lbs.GetOperation(f.f.ctx, "", id)
		if err != nil || op.State != biz.Succeeded {
			t.Fatal("operation did not complete from real configuration/release facts", op, err)
		}
	}
	update := func(key string, listeners []biz.LoadBalancerListenerInput) biz.LoadBalancerResult {
		t.Helper()
		accepted, err := f.lbs.Update(f.f.ctx, biz.UpdateLoadBalancer{ID: lb.ID, ExpectedVersion: lb.Version, IdempotencyKey: key, LoadBalancerMutableInput: biz.LoadBalancerMutableInput{Listeners: listeners, UpdateMask: []string{"listeners"}}})
		if err != nil {
			t.Fatal(key, err)
		}
		return accepted
	}
	claim("reserved", false)
	occupancy(1, 1, 0)
	firstID := lb.Listeners[0].ID
	first := deferredListenerInput(lb.Listeners[0], f.request.Backends)
	activated := update("first-backend", []biz.LoadBalancerListenerInput{first})
	lb = deferredConfigured(t, f, lb.ID, 2)
	deferredVersions(t, lb, 2, 2, 2)
	operation(activated.Operation.ID)
	claim("bound", false)
	occupancy(1, 1, 1)
	running := deferredBinding(t, f, lb.ID)
	if running.id != initial.id || running.uid == "" || lb.Listeners[0].ID != firstID {
		t.Fatal("first activation changed product identity or did not record actual Gateway UID", running, lb)
	}
	oldMemberID := lb.Listeners[0].Backends[0].ID
	gateway := f.api.Object("gateways", running.namespace, running.name)
	if gateway == nil || len(gateway["spec"].(map[string]any)["listeners"].([]any)) != 1 {
		t.Fatal("activation did not expose one actual listener", gateway)
	}
	first = deferredListenerInput(lb.Listeners[0], deferredRetainMembers(lb.Listeners[0]))
	secondPort := uint32(8081)
	second := biz.LoadBalancerListenerInput{Name: "second", Protocol: "HTTP", Port: &secondPort}
	before = deferredMutationCount(controller)
	mixed := update("add-waiting-listener", []biz.LoadBalancerListenerInput{first, second})
	lb = mixed.LoadBalancer
	deferredVersions(t, lb, 3, 2, 2)
	operation(mixed.Operation.ID)
	if deferredMutationCount(controller) != before || !reflect.DeepEqual(gateway, f.api.Object("gateways", running.namespace, running.name)) || lb.Listeners[1].DeploymentState != "waiting_for_backends" {
		t.Fatal("mixed empty listener changed the running Gateway or did not wait", lb)
	}
	secondID := lb.Listeners[1].ID
	second = deferredListenerInput(lb.Listeners[1], deferredRetainMembers(lb.Listeners[0]))
	shared := update("activate-shared-listener", []biz.LoadBalancerListenerInput{first, second})
	lb = deferredConfigured(t, f, lb.ID, 4)
	operation(shared.Operation.ID)
	deferredVersions(t, lb, 4, 4, 4)
	if lb.Listeners[0].Backends[0].ID != lb.Listeners[1].Backends[0].ID {
		t.Fatal("same endpoint was not preserved as a shared member", lb.Listeners)
	}
	var activeBackends int
	if err = f.f.owner.QueryRow(f.f.ctx, `SELECT count(*) FROM network_lb_components WHERE tenant_id=$1 AND lb_id=$2 AND kind='backend' AND deleted_at IS NULL`, f.f.tenant, lb.ID).Scan(&activeBackends); err != nil || activeBackends != 1 {
		t.Fatal("shared listeners created duplicate Backend components", activeBackends, err)
	}
	first = deferredListenerInput(lb.Listeners[0], nil)
	second = deferredListenerInput(lb.Listeners[1], deferredRetainMembers(lb.Listeners[1]))
	partial := update("clear-first-listener", []biz.LoadBalancerListenerInput{first, second})
	lb = deferredConfigured(t, f, lb.ID, 5)
	operation(partial.Operation.ID)
	deferredVersions(t, lb, 5, 5, 5)
	remaining := f.api.Object("gateways", running.namespace, running.name)
	if remaining == nil || remaining["metadata"].(map[string]any)["uid"] != running.uid || len(remaining["spec"].(map[string]any)["listeners"].([]any)) != 1 || remaining["spec"].(map[string]any)["listeners"].([]any)[0].(map[string]any)["name"] != "second" {
		t.Fatal("partial clear removed other listener or replaced Gateway", remaining)
	}
	if lb.Listeners[0].DeploymentState != "waiting_for_backends" || lb.Listeners[1].DeploymentState != "configured" {
		t.Fatal("partial clear deployment states", lb.Listeners)
	}
	occupancy(1, 1, 1)
	var oldBackend, remainingRoute string
	if err = f.f.owner.QueryRow(f.f.ctx, `SELECT provider_name FROM network_lb_components WHERE tenant_id=$1 AND lb_id=$2 AND kind='backend' AND deleted_at IS NULL`, f.f.tenant, lb.ID).Scan(&oldBackend); err != nil {
		t.Fatal(err)
	}
	if f.api.Object("backends", running.namespace, oldBackend) == nil {
		t.Fatal("partial clear deleted the shared Backend")
	}
	if err = f.f.owner.QueryRow(f.f.ctx, `SELECT provider_name FROM network_lb_components WHERE tenant_id=$1 AND lb_id=$2 AND listener_id=$3 AND kind='route' AND deleted_at IS NULL`, f.f.tenant, lb.ID, secondID).Scan(&remainingRoute); err != nil {
		t.Fatal(err)
	}
	oldWork := deferredClaimSnapshot(t, f, lb.ID)
	controller.mu.Lock()
	controller.holdCleanup = true
	controller.mu.Unlock()
	second = deferredListenerInput(lb.Listeners[1], nil)
	cleared := update("clear-last-listener", []biz.LoadBalancerListenerInput{first, second})
	f.f.drive(t, func() bool {
		route := f.api.Object("httproutes", running.namespace, remainingRoute)
		return route != nil && route["metadata"].(map[string]any)["deletionTimestamp"] != nil
	})
	lb, err = f.lbs.Get(f.f.ctx, "", lb.ID)
	if err != nil || lb.AppliedVersion != 5 || lb.ConfigurationState == "waiting_for_backends" || lb.Listeners[1].DeploymentState != "removing" {
		t.Fatal("finalizing route falsely completed empty configuration", lb, err)
	}
	claim("bound", false)
	occupancy(1, 1, 1)
	restartDeferredWorker(t, f)
	// The external controller completes its own finalizer after the real worker
	// restart. Resource never removes the Provider finalizer to force progress.
	route := f.api.Object("httproutes", running.namespace, remainingRoute)
	f.api.Change("httproutes", route, true)
	controller.mu.Lock()
	controller.holdCleanup = false
	controller.mu.Unlock()
	f.f.drive(t, func() bool {
		lb, err = f.lbs.Get(f.f.ctx, "", lb.ID)
		return err == nil && lb.ConfigurationState == "waiting_for_backends" && lb.AppliedVersion == 6
	})
	deferredVersions(t, lb, 6, 6, 6)
	operation(cleared.Operation.ID)
	claim("reserved", false)
	occupancy(1, 1, 0)
	if lb.State != biz.Provisioning || lb.Listeners[0].ID != firstID || lb.Listeners[1].ID != secondID || f.api.Object("gateways", running.namespace, running.name) != nil || f.api.Object("backends", running.namespace, oldBackend) != nil || f.api.Object("services", running.namespace, running.name) != nil || f.api.Object("endpointslices", running.namespace, running.name+"-slice") != nil {
		t.Fatal("complete cleanup did not preserve product identity and remove old runtime", lb)
	}
	var retiredUID, retiredOperation string
	var cleanupTarget int64
	if err = f.f.owner.QueryRow(f.f.ctx, `SELECT provider_uid,cleanup_target_version,operation_id FROM network_lb_retired_bindings WHERE tenant_id=$1 AND lb_id=$2 AND binding_id=$3`, f.f.tenant, lb.ID, running.id).Scan(&retiredUID, &cleanupTarget, &retiredOperation); err != nil || retiredUID != running.uid || cleanupTarget != 6 || retiredOperation != cleared.Operation.ID {
		t.Fatal("durable retirement fact missing or not scoped to old Gateway/cleanup", retiredUID, cleanupTarget, retiredOperation, err)
	}
	first = deferredListenerInput(lb.Listeners[0], f.request.Backends)
	second = deferredListenerInput(lb.Listeners[1], nil)
	resumed := update("reactivate-after-cleanup", []biz.LoadBalancerListenerInput{first, second})
	newBinding := deferredBinding(t, f, lb.ID)
	if newBinding.id == running.id || newBinding.name == running.name || newBinding.uid != "" {
		t.Fatal("reactivation reused the retired Provider incarnation", newBinding, running)
	}
	// GatewayNamespace creates a Service with this name. Validate against the
	// actual Kubernetes Service name contract, which the protocol fixture does
	// not enforce. Keep running so the product is cleaned even on this failure.
	if problems := validation.IsDNS1035Label(newBinding.name); len(problems) != 0 {
		t.Error("reactivated Gateway cannot name its generated Service", newBinding.name, problems)
	}
	lb = deferredConfigured(t, f, lb.ID, 7)
	operation(resumed.Operation.ID)
	deferredVersions(t, lb, 7, 7, 7)
	newBinding = deferredBinding(t, f, lb.ID)
	if newBinding.uid == "" || newBinding.uid == running.uid || lb.Listeners[0].ID != firstID || lb.Listeners[0].Backends[0].ID == oldMemberID {
		t.Fatal("reactivation revived old Provider/member identities", newBinding, lb)
	}
	if err = f.f.p.Finish(f.f.ctx, oldWork, biz.Progress{State: biz.Available, Identity: running.uid}); !errors.Is(err, biz.ErrLeaseLost) {
		t.Fatal("old lease wrote into the new incarnation", err)
	}
	claim("bound", false)
	occupancy(1, 1, 1)
	deleted, err := f.lbs.Delete(f.f.ctx, "", lb.ID)
	if err != nil {
		t.Fatal(err)
	}
	lb = lbState(t, f, lb.ID, biz.Deleted)
	operation(deleted.Operation.ID)
	// Claim release is recorded by released_at; its last binding state remains.
	claim("bound", true)
	occupancy(0, 0, 0)
	if f.api.Object("gateways", newBinding.namespace, newBinding.name) != nil || f.api.Object("pods", newBinding.namespace, "lb-backend") == nil {
		t.Fatal("product deletion leaked Gateway or deleted business owner Pod")
	}
	if vpc, err := f.f.n.GetVPC(f.f.ctx, f.f.tenant, f.vpc.ID); err != nil || vpc.State == biz.Deleted {
		t.Fatal("LB deletion changed independent VPC", vpc, err)
	}
	if survivingEIP, err := f.f.e.GetEIP(f.f.ctx, "", eip.ID); err != nil || survivingEIP.State == biz.Deleted {
		t.Fatal("LB deletion changed independent EIP", survivingEIP, err)
	}
	t.Log("controlled empty-save/activate/mixed/shared/partial-clear/full-clear/restart/reactivate/delete chain completed; live VIP traffic remains a separate acceptance")
}

// P1 uses the existing production lifecycle without any retirement-table
// shortcut: save empty intent, activate it, then delete the product normally.
func TestLBDeferredFirstActivationThenProductDelete(t *testing.T) {
	controller := &lbControllerFixture{}
	f := newLBAdmissionFixture(t, controller.http)
	before := deferredMutationCount(controller)
	created, err := f.lbs.Create(f.f.ctx, deferredEmptyLBRequest(f))
	if err != nil {
		t.Fatal(err)
	}
	lb := created.LoadBalancer
	deferredVersions(t, lb, 1, 0, 0)
	if created.Operation.State != biz.Succeeded || lb.ConfigurationState != "waiting_for_backends" || lb.ObservedAt != nil || deferredMutationCount(controller) != before {
		t.Fatal("empty configuration did not save without external LB mutations", created)
	}
	initial := deferredBinding(t, f, lb.ID)
	if initial.uid != "" || f.api.Object("gateways", initial.namespace, initial.name) != nil {
		t.Fatal("empty configuration has a running Gateway", initial)
	}
	backend := f.api.Object("pods", initial.namespace, "lb-backend")
	if backend == nil {
		t.Fatal("missing external business backend fixture")
	}
	backendUID := backend["metadata"].(map[string]any)["uid"]
	activated, err := f.lbs.Update(f.f.ctx, biz.UpdateLoadBalancer{ID: lb.ID, ExpectedVersion: lb.Version, IdempotencyKey: "minimal-first-backend", LoadBalancerMutableInput: biz.LoadBalancerMutableInput{Listeners: []biz.LoadBalancerListenerInput{deferredListenerInput(lb.Listeners[0], f.request.Backends)}, UpdateMask: []string{"listeners"}}})
	if err != nil {
		t.Fatal(err)
	}
	lb = deferredConfigured(t, f, lb.ID, 2)
	deferredVersions(t, lb, 2, 2, 2)
	op, err := f.lbs.GetOperation(f.f.ctx, "", activated.Operation.ID)
	if err != nil || op.State != biz.Succeeded || deferredMutationCount(controller) <= before {
		t.Fatal("first activation did not complete through the actual adapter", op, err)
	}
	running := deferredBinding(t, f, lb.ID)
	if running.id != initial.id || running.uid == "" || f.api.Object("gateways", running.namespace, running.name) == nil || f.api.Object("services", running.namespace, running.name) == nil {
		t.Fatal("first activation did not preserve product placement and create a real Gateway/Service", running)
	}
	deleted, err := f.lbs.Delete(f.f.ctx, "", lb.ID)
	if err != nil {
		t.Fatal(err)
	}
	lb = lbState(t, f, lb.ID, biz.Deleted)
	op, err = f.lbs.GetOperation(f.f.ctx, "", deleted.Operation.ID)
	if err != nil || op.State != biz.Succeeded {
		t.Fatal("product deletion did not complete from Provider release observations", op, err)
	}
	var active int
	if err = f.f.owner.QueryRow(f.f.ctx, `SELECT (SELECT count(*) FROM network_lb_vip_intents WHERE tenant_id=$1 AND lb_id=$2 AND released_at IS NULL)+(SELECT count(*) FROM network_lb_subnet_refs WHERE tenant_id=$1 AND lb_id=$2 AND released_at IS NULL)+(SELECT count(*) FROM network_lb_components WHERE tenant_id=$1 AND lb_id=$2 AND deleted_at IS NULL)`, f.f.tenant, lb.ID).Scan(&active); err != nil || active != 0 {
		t.Fatal("product deletion retained VIP/subnet/component occupancy", active, err)
	}
	backend = f.api.Object("pods", running.namespace, "lb-backend")
	if backend == nil || backend["metadata"].(map[string]any)["uid"] != backendUID || f.api.Object("gateways", running.namespace, running.name) != nil || f.api.Object("services", running.namespace, running.name) != nil {
		t.Fatal("product deletion changed the business backend or retained its LB data plane")
	}
}
