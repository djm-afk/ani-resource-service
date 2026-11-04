package data_test

import (
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
			http.Error(w, "read failure", http.StatusServiceUnavailable)
			return true
		}
		return false
	})
	vpc := baseState(t, f, createBaseVPC(t, f, "snat-read-facts").ID, biz.Available)
	_, snatID := baseIDs(t, f, vpc.ID)
	baseFactsDirectWorker(t, f, kube)
	projection := func() string {
		t.Helper()
		var value string
		if err := f.owner.QueryRow(f.ctx, `SELECT jsonb_build_array(state,reason,applied_enabled,observed_at)::text FROM network_snat_bindings WHERE tenant_id=$1 AND snat_id=$2`, f.tenant, snatID).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	before := projection()
	fail.Store(true)
	baseFactsStep(t, f, snatID)
	if projection() != before {
		t.Fatal("read failure changed confirmed SNAT state/reason/applied/time")
	}
	var observationError string
	if err := f.owner.QueryRow(f.ctx, `SELECT observation_error FROM network_reconciliations WHERE tenant_id=$1 AND snat_id=$2`, f.tenant, snatID).Scan(&observationError); err != nil || observationError != string(biz.ProviderUnavailable) {
		t.Fatal(observationError, err)
	}
	current, err := f.n.GetVPC(f.ctx, f.tenant, vpc.ID)
	if err != nil || current.State != biz.Available || current.BaseConnectivity == nil || current.BaseConnectivity.State != "ready" || !current.BaseConnectivity.ObservationStale {
		t.Fatal(current, err)
	}
	fail.Store(false)
	baseFactsStep(t, f, snatID)
	if err := f.owner.QueryRow(f.ctx, `SELECT observation_error FROM network_reconciliations WHERE tenant_id=$1 AND snat_id=$2`, f.tenant, snatID).Scan(&observationError); err != nil || observationError != "" {
		t.Fatal("successful current read did not clear diagnostic", observationError, err)
	}
}

func TestVPCReadFailureDoesNotRewriteManagedEgressProjection(t *testing.T) {
	var fail atomic.Bool
	f, _, _, kube := newBaseKCFixture(t, func(w http.ResponseWriter, r *http.Request, _ *controlled.Server) bool {
		if fail.Load() && r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/vpcs/") {
			http.Error(w, "read failure", http.StatusServiceUnavailable)
			return true
		}
		return false
	})
	vpc := baseState(t, f, createBaseVPC(t, f, "vpc-read-facts").ID, biz.Available)
	baseFactsDirectWorker(t, f, kube)
	var before, after string
	query := `SELECT to_jsonb(b)::text FROM network_vpc_base_connectivity b WHERE tenant_id=$1 AND vpc_id=$2`
	if err := f.owner.QueryRow(f.ctx, query, f.tenant, vpc.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	fail.Store(true)
	baseFactsStep(t, f, vpc.ID)
	if err := f.owner.QueryRow(f.ctx, query, f.tenant, vpc.ID).Scan(&after); err != nil || before != after {
		t.Fatal("VPC read error rewrote child relationship", before, after, err)
	}
	current, err := f.n.GetVPC(f.ctx, f.tenant, vpc.ID)
	if err != nil || current.State != vpc.State || current.Reason != vpc.Reason || !current.ObservedAt.Equal(*vpc.ObservedAt) || !current.ObservationStale {
		t.Fatal(current, err)
	}
}

func TestManagedChildStatusDoesNotFanOutParentHealthWork(t *testing.T) {
	f, _, _, kube := newBaseKCFixture(t, nil)
	vpc := baseState(t, f, createBaseVPC(t, f, "no-health-fanout").ID, biz.Available)
	eipID, snatID := baseIDs(t, f, vpc.ID)
	baseFactsDirectWorker(t, f, kube)
	var before, after int64
	query := `SELECT requested_generation FROM network_reconciliations WHERE tenant_id=$1 AND vpc_id=$2`
	if err := f.owner.QueryRow(f.ctx, query, f.tenant, vpc.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	baseFactsStep(t, f, eipID)
	baseFactsStep(t, f, snatID)
	if err := f.owner.QueryRow(f.ctx, query, f.tenant, vpc.ID).Scan(&after); err != nil || before != after {
		t.Fatal("child reads queued unrelated parent health checks", before, after, err)
	}
}
