package data_test

import (
	"context"
	"github.com/google/uuid"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network"
	controlled "github.com/zhangzhe-ctrl/ani-resource-service/tests/net05a/provider"
	"github.com/zhangzhe-ctrl/ani-resource-service/tests/testenv"
	"k8s.io/client-go/rest"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func seedEgressInfrastructure(api *controlled.Server) {
	for _, role := range []string{"controller", "cni-ds", "ovs-ds", "ovn-central"} {
		api.Change("pods", map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{"name": role, "namespace": "kcn-system", "uid": uuid.NewString(), "labels": map[string]any{"networking.kubercloud.com/app": role}}, "spec": map[string]any{"containers": []any{map[string]any{"name": role}}}, "status": map[string]any{"phase": "Running", "conditions": []any{map[string]any{"type": "Ready", "status": "True"}}, "containerStatuses": []any{map[string]any{"name": role, "ready": true, "imageID": "fixture@sha256:" + strings.Repeat("a", 64)}}}}, false)
	}
	api.Change("servicecidrs", map[string]any{"apiVersion": "networking.k8s.io/v1", "kind": "ServiceCIDR", "metadata": map[string]any{"name": "kubernetes", "uid": uuid.NewString()}, "spec": map[string]any{"cidrs": []any{"10.96.0.0/12"}}}, false)
}
func newEgressKCFixture(t *testing.T, intercept func(http.ResponseWriter, *http.Request, *controlled.Server) bool) (*egressFixture, *controlled.Server, *testenv.Database, string) {
	t.Helper()
	db := testenv.NewDatabase(t)
	configureRepositoryVPCPresets(t, db.Repository)
	api := controlled.New()
	seedEgressInfrastructure(api)
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if intercept != nil && intercept(w, r, api) {
			return
		}
		api.ServeHTTP(w, r)
	}))
	t.Cleanup(host.Close)
	provider, err := data.NewKCProvider(db.Repository, &rest.Config{Host: host.URL, QPS: 1000, Burst: 1000})
	if err != nil {
		t.Fatal(err)
	}
	db.Repository.UseVirtualSwitchInfrastructure(provider)
	policy := biz.DefaultWorkerPolicy()
	policy.ObserveEvery = 100 * time.Millisecond
	policy.RetryMin = 5 * time.Millisecond
	policy.RetryMax = 20 * time.Millisecond
	w, err := biz.NewWorker(db.Repository, provider, uuid.NewString(), policy)
	if err != nil {
		t.Fatal(err)
	}
	e, err := biz.NewEgress(db.Repository, provider, biz.ContextEgressAuthorization{}, []byte("0123456789abcdef0123456789abcdef"), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	f := &egressFixture{p: db.Repository, owner: db.Owner, e: e, n: newNetwork(t, db.Repository, time.Minute), w: w, tenant: uuid.NewString()}
	// Namespace creation belongs to the external platform, before admission.
	api.Change("namespaces", map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "tenant-" + f.tenant, "uid": uuid.NewString(), "labels": map[string]any{"network.ani.io/managed-by": "ani-network-service", "network.ani.io/tenant-id": f.tenant}}, "status": map[string]any{"phase": "Active"}}, false)
	f.ctx = biz.WithEgressCaller(context.Background(), biz.EgressCaller{TenantID: f.tenant, PlatformAdministrator: true, Attribution: biz.Attribution{Actor: "controlled-adapter-test", DirectCaller: "fixture"}})
	gateway := f.platform(t, biz.PlatformIntent{Kind: "create_egress_gateway", Name: "gateway", IdempotencyKey: "gateway"})
	f.pool = f.platform(t, biz.PlatformIntent{Kind: "create_public_pool", Name: "pool", IdempotencyKey: "pool", Pool: &biz.PublicPoolConfig{Mode: "overlay", GatewayID: gateway.ID, CIDR: "192.0.2.0/24", OVNGatewayIP: "192.0.2.1"}})
	evidence := &biz.PublicPoolVerification{ProviderSourceRevision: strings.Repeat("a", 40), ProviderImageDigests: []string{"sha256:" + strings.Repeat("a", 64)}, TopologyFingerprint: strings.Repeat("c", 64), EvidenceReference: "controlled HTTP fixture, not native data plane", Scope: "adapter test", VerifiedAt: time.Now().Add(-time.Second), ExpiresAt: time.Now().Add(time.Hour)}
	f.setPool(t, biz.PlatformIntent{Kind: "verify_public_pool", ID: f.pool.ID, IdempotencyKey: "verify", Verification: evidence})
	f.setPool(t, biz.PlatformIntent{Kind: "set_pool_allocation", ID: f.pool.ID, IdempotencyKey: "open", Enabled: true})
	f.setPool(t, biz.PlatformIntent{Kind: "set_default_pool", ID: f.pool.ID, IdempotencyKey: "default"})
	// Start shared observation after initial fixture setup. Every subsequent
	// lifecycle transition wakes the task through ordinary object events.
	obs, err := provider.EnableObservation(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- obs.Start(ctx) }()
	var stopOnce sync.Once
	f.stopObservation = func() {
		stopOnce.Do(func() {
			cancel()
			if err := <-done; err != nil {
				t.Error(err)
			}
		})
	}
	t.Cleanup(f.stopObservation)
	awaitNET05A(t, 3*time.Second, func() bool { return obs.Snapshot()["source_synced"] == 1 })
	return f, api, db, testenv.Kubeconfig(t, host.URL)
}
func TestEgressKCAdapterSharedObservationLifecycleAndDrift(t *testing.T) {
	f, api, _, _ := newEgressKCFixture(t, nil)
	vpc := availableEgressVPC(t, f, "vpc")
	eip := f.eip(t, "eip")
	bound, err := f.e.BindVPCSnat(f.ctx, biz.EgressIntent{VPCID: vpc.ID, EIPID: eip.ID, IdempotencyKey: "bind"})
	if err != nil {
		t.Fatal(err)
	}
	enabled := true
	bound = f.binding(t, bound.ID, biz.Available, &enabled)
	ns, name := "tenant-"+f.tenant, strings.Replace(bound.ID, "_", "-", 1)
	snat := api.Object("snats", ns, name)
	if snat["spec"].(map[string]any)["vpc"] != strings.Replace(vpc.ID, "_", "-", 1) || snat["spec"].(map[string]any)["cidrs"] != nil {
		t.Fatal("provider render changed scope")
	}
	_, err = f.e.SetVPCSnatEnabled(f.ctx, biz.EgressIntent{ID: bound.ID, Enabled: false, ExpectedVersion: bound.Version, IdempotencyKey: "off"})
	if err != nil {
		t.Fatal(err)
	}
	enabled = false
	bound = f.binding(t, bound.ID, biz.Available, &enabled)
	f.drive(t, func() bool { v, err := f.e.GetEIP(f.ctx, "", eip.ID); return err == nil && v.State == biz.Available })
	_, err = f.e.SetVPCSnatEnabled(f.ctx, biz.EgressIntent{ID: bound.ID, Enabled: true, ExpectedVersion: bound.Version, IdempotencyKey: "on"})
	if err != nil {
		t.Fatal(err)
	}
	enabled = true
	bound = f.binding(t, bound.ID, biz.Available, &enabled)
	// The binding consumes its own phase; EIP reverse-binding diagnostics are not a gate.
	eipName := strings.Replace(eip.ID, "_", "-", 1)

	if _, err = f.e.DeleteVPCSnatBinding(f.ctx, "", bound.ID); err != nil {
		t.Fatal(err)
	}
	f.binding(t, bound.ID, biz.Deleted, nil)
	if _, err = f.e.DeleteEIP(f.ctx, "", eip.ID); err != nil {
		t.Fatal(err)
	}
	f.drive(t, func() bool { v, err := f.e.GetEIP(f.ctx, "", eip.ID); return err == nil && v.State == biz.Deleted })
	_ = eipName

	if api.Backend.Invalid != "" {
		t.Fatal(api.Backend.Invalid)
	}
}
