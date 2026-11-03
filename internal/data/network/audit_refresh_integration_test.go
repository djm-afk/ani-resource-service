package data_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network"
	"k8s.io/client-go/tools/clientcmd"
)

// Keep the periodic audit out of these scenarios. Ordinary reads must wait;
// explicit critical observations collect a complete audit after the event.
func startSlowAudit(t *testing.T, provider *data.KCProvider) *data.KCObservation {
	t.Helper()
	options := data.DefaultObservationOptions()
	options.AuditInterval = time.Hour
	options.AuditJitter = time.Millisecond
	options.FlushInterval = 10 * time.Millisecond
	observer, err := provider.EnableObservation(options)
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
	awaitNET05A(t, 3*time.Second, func() bool { return observer.Snapshot()["source_synced"] == 1 })
	return observer
}

func TestEgressWaitsForSharedAuditAndCriticalReadPreservesAppliedFacts(t *testing.T) {
	f, api, _, kube := newBaseKCFixture(t, nil)
	vpc := baseState(t, f, createBaseVPC(t, f, "refresh").ID, biz.Available)
	eipID, snatID := baseIDs(t, f, vpc.ID)
	target := biz.ProviderTarget{TenantID: f.tenant, ResourceID: snatID, Kind: "snat", VPCID: vpc.ID, Egress: &biz.EgressWorkSpec{EIPID: eipID, DesiredEnabled: true}, Direct: true}
	if err := f.owner.QueryRow(f.ctx, `SELECT binding_id,provider_uid FROM network_provider_bindings WHERE tenant_id=$1 AND snat_id=$2`, f.tenant, snatID).Scan(&target.BindingID, &target.KnownIdentity); err != nil {
		t.Fatal(err)
	}
	config, err := clientcmd.BuildConfigFromFlags("", kube)
	if err != nil {
		t.Fatal(err)
	}
	config.QPS, config.Burst = 1000, 1000
	provider, err := data.NewKCProvider(f.p, config)
	if err != nil {
		t.Fatal(err)
	}
	observer := startSlowAudit(t, provider)
	first, err := provider.Observe(f.ctx, target)
	if err != nil || !first.Ready || first.Egress == nil || first.Egress.AppliedEnabled == nil || !*first.Egress.AppliedEnabled {
		t.Fatalf("initial facts: %+v %v", first, err)
	}
	target.Direct = false
	ns, name := "tenant-"+f.tenant, strings.Replace(snatID, "_", "-", 1)
	before := observer.Snapshot()["notifications_total"]
	changed := api.Object("snats", ns, name)
	changed["metadata"].(map[string]any)["annotations"] = map[string]any{"test-observation": "renewed"}
	changedAt := time.Now()
	api.Change("snats", changed, false)
	awaitNET05A(t, 3*time.Second, func() bool { return observer.Snapshot()["notifications_total"] > before })
	// Exactly one caller attempt. Retrying this assertion would hide the bug.
	next, err := provider.Observe(f.ctx, target)
	if err == nil || !next.Proof.CollectedAt.IsZero() {
		t.Fatalf("ordinary observation consumed invalid proof: %+v %v", next, err)
	}
	target.Direct = true
	next, err = provider.Observe(f.ctx, target)
	if err != nil || !next.Ready || next.Egress == nil || next.Egress.AppliedEnabled == nil || !*next.Egress.AppliedEnabled || next.Proof.CollectedAt.Before(changedAt) {
		t.Fatalf("invalid cached audit leaked into SNAT status instead of fresh collection: %+v %v", next, err)
	}
	before = observer.Snapshot()["notifications_total"]
	changed = api.Object("snats", ns, name)
	changed["metadata"].(map[string]any)["uid"] = uuid.NewString()
	api.Change("snats", changed, false)
	awaitNET05A(t, 3*time.Second, func() bool { return observer.Snapshot()["notifications_total"] > before })
	_, err = provider.Observe(f.ctx, target)
	var failure *biz.ProviderError
	if !errors.As(err, &failure) || failure.Kind != biz.ProviderConflict {
		t.Fatalf("fresh read did not reject replacement UID: %v", err)
	}
}

func TestLBWaitsForSharedAuditAndCriticalReadRejectsReusedBackendIdentity(t *testing.T) {
	f := newLBAdmissionFixture(t)
	created, err := f.lbs.Create(f.f.ctx, f.request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.f.owner.Exec(f.f.ctx, `UPDATE network_reconciliations SET next_run_at=clock_timestamp()-interval '1 minute' WHERE tenant_id=$1 AND lb_id=$2`, f.f.tenant, created.LoadBalancer.ID); err != nil {
		t.Fatal(err)
	}
	var work biz.Work
	for attempt := 0; attempt < 12; attempt++ {
		var found bool
		work, found, err = f.f.p.Claim(f.f.ctx, uuid.NewString(), time.Minute)
		if err != nil || !found {
			t.Fatalf("claim %v %v", found, err)
		}
		if work.Resource.ID == created.LoadBalancer.ID {
			break
		}
	}
	if work.Resource.ID != created.LoadBalancer.ID {
		t.Fatal("LB work not selected")
	}
	// Exercise the sustained observation path; creation already uses a fresh
	// collection. This test calls the adapter only, without finishing the work.
	work.ActiveOperation = false
	expected := seedLBInstallation(f.api)
	config, err := clientcmd.BuildConfigFromFlags("", f.kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	config.QPS, config.Burst = 1000, 1000
	provider, err := data.NewKCProvider(f.f.p, config)
	if err != nil {
		t.Fatal(err)
	}
	if err = provider.ConfigureLoadBalancer(expected); err != nil {
		t.Fatal(err)
	}
	observer := startSlowAudit(t, provider)
	// Start with an explicit fresh collection, then use the cached read path.
	initialWork := work
	initialWork.ActiveOperation = true
	first, err := provider.ObserveLoadBalancer(f.f.ctx, initialWork)
	if err != nil || len(first.Members) != 1 || !first.Members[0].Eligible {
		t.Fatalf("initial member %+v %v", first.Members, err)
	}
	before := observer.Snapshot()["notifications_total"]
	ip := f.api.Object("vnicips", "tenant-"+f.f.tenant, "lb-backend-ip")
	ip["metadata"].(map[string]any)["annotations"] = map[string]any{"test-observation": "renewed"}
	changedAt := time.Now()
	f.api.Change("vnicips", ip, false)
	awaitNET05A(t, 3*time.Second, func() bool { return observer.Snapshot()["notifications_total"] > before })
	next, err := provider.ObserveLoadBalancer(f.f.ctx, work)
	if err == nil || !next.Proof.CollectedAt.IsZero() {
		t.Fatalf("ordinary LB observation consumed invalid proof: %+v %v", next, err)
	}
	work.ActiveOperation = true
	next, err = provider.ObserveLoadBalancer(f.f.ctx, work)
	if err != nil || len(next.Members) != 1 || !next.Members[0].Eligible || next.Proof.CollectedAt.Before(changedAt) {
		t.Fatalf("invalid cached LB audit did not refresh in same call: %+v %v", next.Members, err)
	}
	before = observer.Snapshot()["notifications_total"]
	ip = f.api.Object("vnicips", "tenant-"+f.f.tenant, "lb-backend-ip")
	ip["metadata"].(map[string]any)["uid"] = uuid.NewString()
	f.api.Change("vnicips", ip, false)
	awaitNET05A(t, 3*time.Second, func() bool { return observer.Snapshot()["notifications_total"] > before })
	next, err = provider.ObserveLoadBalancer(f.f.ctx, work)
	if err != nil || len(next.Members) != 1 || next.Members[0].Eligible || next.Members[0].Reason != biz.BackendIdentityMismatch {
		t.Fatalf("replacement backend was not rejected by refreshed audit: %+v %v", next.Members, err)
	}
}
