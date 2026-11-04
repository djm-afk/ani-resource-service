package data_test

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	data "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network"
	controlled "github.com/zhangzhe-ctrl/ani-resource-service/tests/net05a/provider"
	"k8s.io/client-go/tools/clientcmd"
)

func TestOptionalLBAuditTimeoutKeepsCompleteBaseEvidence(t *testing.T) {
	var armed atomic.Bool
	var optionalReads atomic.Int32
	f, api, _, kube := newBaseKCFixture(t, func(w http.ResponseWriter, r *http.Request, _ *controlled.Server) bool {
		if armed.Load() && r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/replicasets") && r.URL.Query().Get("watch") != "true" {
			optionalReads.Add(1)
			<-r.Context().Done()
			return true
		}
		return false
	})
	vpc := baseState(t, f, createBaseVPC(t, f, "optional-lb-timeout").ID, biz.Available)
	eipID, snatID := baseIDs(t, f, vpc.ID)
	f.stopObservation()
	config, err := clientcmd.BuildConfigFromFlags("", kube)
	if err != nil {
		t.Fatal(err)
	}
	config.QPS, config.Burst = 1000, 1000
	provider, err := data.NewKCProvider(f.p, config)
	if err != nil {
		t.Fatal(err)
	}
	if err = provider.ConfigureLoadBalancer(seedLBInstallation(api)); err != nil {
		t.Fatal(err)
	}
	options := data.DefaultObservationOptions()
	options.AuditTimeout = 500 * time.Millisecond
	observer, err := provider.EnableObservation(options)
	if err != nil {
		t.Fatal(err)
	}
	// Use the real critical Egress path to request one full collection. No
	// informer or periodic audit runs concurrently with this controlled fault.
	target := biz.ProviderTarget{TenantID: f.tenant, ResourceID: snatID, Kind: "snat", VPCID: vpc.ID, Egress: &biz.EgressWorkSpec{EIPID: eipID, DesiredEnabled: true}, Direct: true}
	if err = f.owner.QueryRow(f.ctx, `SELECT binding_id,provider_uid FROM network_provider_bindings WHERE tenant_id=$1 AND snat_id=$2`, f.tenant, snatID).Scan(&target.BindingID, &target.KnownIdentity); err != nil {
		t.Fatal(err)
	}
	armed.Store(true)
	started := time.Now()
	ctx, cancel := context.WithTimeout(f.ctx, time.Second)
	defer cancel()
	observation, err := provider.Observe(ctx, target)
	if err != nil || !observation.Ready || observation.Egress == nil || observation.Egress.AppliedEnabled == nil || !*observation.Egress.AppliedEnabled {
		t.Fatalf("optional ReplicaSet timeout discarded complete base proof: %+v %v", observation, err)
	}
	if optionalReads.Load() != 1 || time.Since(started) >= options.AuditTimeout || observation.Proof.CollectedAt.Before(started) {
		t.Fatalf("optional audit exceeded or renewed the base proof: reads=%d elapsed=%s proof=%+v", optionalReads.Load(), time.Since(started), observation.Proof)
	}
	measurements := observer.Snapshot()
	if measurements["audit_objects_eips"] == 0 || measurements["audit_failures_total"] != 0 || measurements["optional_audit_failures_total"] != 1 {
		t.Fatalf("mandatory and optional failure scopes were mixed: %+v", measurements)
	}
	if _, present := measurements["audit_objects_gateways"]; present {
		t.Fatal("incomplete LB source scope was exposed as complete cleanup evidence")
	}
	var ready bool
	var reason string
	if err = f.owner.QueryRow(f.ctx, `SELECT ready,reason FROM network_lb_capabilities WHERE cluster_id='test-cluster'`).Scan(&ready, &reason); err != nil || ready || reason != "PROVIDER_UNAVAILABLE" {
		t.Fatalf("LB capability did not retain the optional failure: ready=%v reason=%s err=%v", ready, reason, err)
	}
	// Mandatory-source failure must still reject the next critical observation,
	// without pretending the previous audit was collected again.
	before := measurements["requests_LIST_eips_total"]
	api.Fail("LIST", "vpcs", http.StatusForbidden)
	next, err := provider.Observe(ctx, target)
	if err == nil || !next.Proof.CollectedAt.IsZero() {
		t.Fatalf("mandatory source failure returned usable proof: %+v %v", next, err)
	}
	if after := observer.Snapshot(); after["audit_failures_total"] != 1 || after["requests_LIST_eips_total"] != before {
		t.Fatalf("mandatory failure did not stop the full collection: %+v", after)
	}
}
