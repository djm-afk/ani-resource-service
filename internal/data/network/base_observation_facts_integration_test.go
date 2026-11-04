package data_test

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	data "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network"
	controlled "github.com/zhangzhe-ctrl/ani-resource-service/tests/net05a/provider"
)

// Keep the production Claim/Finish fences and provider identity checks. Only
// select a due task in this fixture's exclusive database; do not edit facts.
func baseFactsStep(t *testing.T, f *egressFixture, id string) {
	t.Helper()
	for _, statement := range []string{
		`UPDATE network_reconciliations SET next_run_at='infinity'`,
		`UPDATE network_platform_reconciliations SET next_run_at='infinity'`,
	} {
		if _, err := f.owner.Exec(f.ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.owner.Exec(f.ctx, `UPDATE network_reconciliations SET next_run_at=clock_timestamp(),retry_not_before='1970-01-01 UTC' WHERE tenant_id=$1 AND coalesce(vpc_id,subnet_id,eip_id,snat_id,lb_id,vnic_id)=$2`, f.tenant, id); err != nil {
		t.Fatal(err)
	}
	if worked, err := f.w.Step(f.ctx); err != nil || !worked {
		t.Fatal("selected worker step", worked, err)
	}
}

func baseFactsDirectWorker(t *testing.T, f *egressFixture, kube string) {
	t.Helper()
	f.stopObservation()
	provider, err := data.OpenKCProvider(f.p, kube, data.KCClientPolicy{QPS: 1000, Burst: 1000})
	if err != nil {
		t.Fatal(err)
	}
	f.p.UseEgressInfrastructure(provider)
	policy := biz.DefaultWorkerPolicy()
	policy.ObserveEvery = 100 * time.Millisecond
	policy.RetryMin, policy.RetryMax = time.Millisecond, 10*time.Millisecond
	f.w, err = biz.NewWorker(f.p, provider, uuid.NewString(), policy)
	if err != nil {
		t.Fatal(err)
	}
}

func TestBaseObservationFailurePreservesConfirmedSnatAppliedFacts(t *testing.T) {
	var fail atomic.Bool
	f, _, _, kube := newBaseKCFixture(t, func(w http.ResponseWriter, r *http.Request, _ *controlled.Server) bool {
		if fail.Load() && r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/snats/") {
			http.Error(w, "temporary SNAT read failure", http.StatusServiceUnavailable)
			return true
		}
		return false
	})
	vpc := baseState(t, f, createBaseVPC(t, f, "snat-read-facts").ID, biz.Available)
	_, snatID := baseIDs(t, f, vpc.ID)
	baseFactsDirectWorker(t, f, kube)
	var before time.Time
	if err := f.owner.QueryRow(f.ctx, `SELECT observed_at FROM network_snat_bindings WHERE tenant_id=$1 AND snat_id=$2`, f.tenant, snatID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	assertPreserved := func(reason biz.Reason) {
		t.Helper()
		var preserved bool
		if err := f.owner.QueryRow(f.ctx, `SELECT state='available' AND reason=$3 AND applied_enabled IS TRUE AND observed_at=$4 FROM network_snat_bindings WHERE tenant_id=$1 AND snat_id=$2`, f.tenant, snatID, string(reason), before).Scan(&preserved); err != nil || !preserved {
			t.Fatal("unsuccessful observation erased or renewed confirmed SNAT facts", preserved, err)
		}
	}
	fail.Store(true)
	baseFactsStep(t, f, snatID)
	assertPreserved(biz.ProviderUnavailable)
	// The dependency gate skips the provider call altogether. It must preserve
	// exactly the same known result, without making its old evidence fresh.
	f.p.UseBaseConnectivityFreshness(time.Nanosecond)
	baseFactsStep(t, f, snatID)
	assertPreserved(biz.BaseConnectivityNotReady)
}

func TestBaseObservationVPCReadFailureRetainsOnlyUnexpiredOriginalProof(t *testing.T) {
	var fail atomic.Bool
	f, api, _, kube := newBaseKCFixture(t, func(w http.ResponseWriter, r *http.Request, _ *controlled.Server) bool {
		if fail.Load() && r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/vpcs/") {
			http.Error(w, "temporary VPC read failure", http.StatusServiceUnavailable)
			return true
		}
		return false
	})
	vpc := baseState(t, f, createBaseVPC(t, f, "vpc-read-facts").ID, biz.Available)
	baseFactsDirectWorker(t, f, kube)
	var providerAt, aggregateAt time.Time
	if err := f.owner.QueryRow(f.ctx, `SELECT provider_observed_at,observed_at FROM network_vpc_base_connectivity WHERE tenant_id=$1 AND vpc_id=$2`, f.tenant, vpc.ID).Scan(&providerAt, &aggregateAt); err != nil {
		t.Fatal(err)
	}
	fail.Store(true)
	baseFactsStep(t, f, vpc.ID)
	var preserved bool
	if err := f.owner.QueryRow(f.ctx, `SELECT state='ready' AND provider_ready AND provider_observed_at=$3 AND observed_at=$4 FROM network_vpc_base_connectivity WHERE tenant_id=$1 AND vpc_id=$2`, f.tenant, vpc.ID, providerAt, aggregateAt).Scan(&preserved); err != nil || !preserved {
		t.Fatal("temporary read discarded fresh prior VPC readiness", preserved, err)
	}
	// Time expiry still closes dependent admission; the failed read must not
	// prolong either timestamp or continue claiming current provider readiness.
	f.p.UseBaseConnectivityFreshness(time.Nanosecond)
	baseFactsStep(t, f, vpc.ID)
	if err := f.owner.QueryRow(f.ctx, `SELECT state='degraded' AND NOT provider_ready AND provider_observed_at=$3 AND observed_at IS NULL FROM network_vpc_base_connectivity WHERE tenant_id=$1 AND vpc_id=$2`, f.tenant, vpc.ID, providerAt).Scan(&preserved); err != nil || !preserved {
		t.Fatal("expired VPC proof retained readiness", preserved, err)
	}
	// A current same-name/different-UID object is never eligible for fallback.
	f.p.UseBaseConnectivityFreshness(time.Minute)
	fail.Store(false)
	baseFactsStep(t, f, vpc.ID)
	name := strings.Replace(vpc.ID, "_", "-", 1)
	object := api.Object("vpcs", "tenant-"+f.tenant, name)
	object["metadata"].(map[string]any)["uid"] = uuid.NewString()
	api.Change("vpcs", object, false)
	baseFactsStep(t, f, vpc.ID)
	if err := f.owner.QueryRow(context.Background(), `SELECT NOT provider_ready AND observed_at IS NULL FROM network_vpc_base_connectivity WHERE tenant_id=$1 AND vpc_id=$2`, f.tenant, vpc.ID).Scan(&preserved); err != nil || !preserved {
		t.Fatal("replacement identity reused last readiness", preserved, err)
	}
}

func TestBaseObservationChildFreshnessRecoveryWakesParentOnce(t *testing.T) {
	f, _, _, kube := newBaseKCFixture(t, nil)
	vpc := baseState(t, f, createBaseVPC(t, f, "child-freshness-recovery").ID, biz.Available)
	eipID, _ := baseIDs(t, f, vpc.ID)
	baseFactsDirectWorker(t, f, kube)
	f.p.UseBaseConnectivityFreshness(200 * time.Millisecond)
	var childBefore int64
	if err := f.owner.QueryRow(f.ctx, `SELECT requested_generation FROM network_reconciliations WHERE tenant_id=$1 AND eip_id=$2`, f.tenant, eipID).Scan(&childBefore); err != nil {
		t.Fatal(err)
	}
	time.Sleep(220 * time.Millisecond)
	baseFactsStep(t, f, vpc.ID)
	var childAfter int64
	if err := f.owner.QueryRow(f.ctx, `SELECT requested_generation FROM network_reconciliations WHERE tenant_id=$1 AND eip_id=$2`, f.tenant, eipID).Scan(&childAfter); err != nil || childAfter != childBefore+1 {
		t.Fatal("timestamp-only VPC proof recovery did not wake child", childBefore, childAfter, err)
	}
	var degraded bool
	if err := f.owner.QueryRow(f.ctx, `SELECT state='degraded' FROM network_vpc_base_connectivity WHERE tenant_id=$1 AND vpc_id=$2`, f.tenant, vpc.ID).Scan(&degraded); err != nil || !degraded {
		t.Fatal("fixture did not reach expired child proof", degraded, err)
	}
	generation := func() int64 {
		t.Helper()
		var v int64
		if err := f.owner.QueryRow(f.ctx, `SELECT requested_generation FROM network_reconciliations WHERE tenant_id=$1 AND vpc_id=$2`, f.tenant, vpc.ID).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	before := generation()
	baseFactsStep(t, f, eipID)
	if after := generation(); after != before+1 {
		t.Fatalf("timestamp-only recovery did not wake parent: before=%d after=%d", before, after)
	}
	// Fresh-to-fresh observations do not form an audit -> child -> parent loop.
	baseFactsStep(t, f, eipID)
	if after := generation(); after != before+1 {
		t.Fatalf("stable freshness repeatedly woke parent: before=%d after=%d", before, after)
	}
}
