package data_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	controlled "github.com/zhangzhe-ctrl/ani-resource-service/tests/net05a/provider"
	"github.com/zhangzhe-ctrl/ani-resource-service/tests/testenv"
)

func newBaseKCFixture(t *testing.T, intercept func(http.ResponseWriter, *http.Request, *controlled.Server) bool) (*egressFixture, *controlled.Server, *testenv.Database, string) {
	t.Helper()
	f, api, db, kube := newEgressKCFixture(t, func(w http.ResponseWriter, r *http.Request, api *controlled.Server) bool {
		if intercept != nil && intercept(w, r, api) {
			return true
		}
		return intranetProviderInterceptor(w, r, api)
	})
	uid := seedIntranetInfrastructure(api)
	intent := intranetIntent("base-pool", "10.232.254.0/24", "10.232.254.1")
	intent.Pool.DefaultVPCUID = uid
	pool := f.platform(t, intent)
	setIntranetPool(t, f, biz.PlatformIntent{Kind: "set_intranet_pool_auto_allocation", ID: pool.ID, Enabled: true, IdempotencyKey: "base-auto-enable"})
	configureVPCPresetFixture(t, f, api, kube)
	return f, api, db, kube
}
func createBaseVPC(t *testing.T, f *egressFixture, key string) biz.VPC {
	t.Helper()
	v, err := f.n.CreateVPC(f.ctx, biz.CreateVPC{TenantID: f.tenant, Name: key, CIDR: "10.42.0.0/16", IdempotencyKey: key})
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func baseState(t *testing.T, f *egressFixture, id string, state biz.ResourceState) biz.VPC {
	t.Helper()
	var v biz.VPC
	f.drive(t, func() bool {
		var err error
		v, err = f.n.GetVPC(f.ctx, f.tenant, id)
		return err == nil && baseFixtureProductsSettled(v, state)
	})
	return v
}

// Lifecycle fixtures wait for both independently managed product records.
// Runtime VPC status remains independent; tests below check that explicitly.
func baseFixtureProductsSettled(v biz.VPC, state biz.ResourceState) bool {
	if v.State != state {
		return false
	}
	if v.BaseConnectivity != nil {
		if state == biz.Available {
			return v.BaseConnectivity.State == "ready"
		}
		if state == biz.Deleted {
			return v.BaseConnectivity.State == "deleted"
		}
	}
	return true
}
func baseIDs(t *testing.T, f *egressFixture, id string) (string, string) {
	t.Helper()
	var e, s string
	if err := f.owner.QueryRow(f.ctx, `SELECT eip_id,snat_id FROM network_vpc_base_connectivity WHERE tenant_id=$1 AND vpc_id=$2`, f.tenant, id).Scan(&e, &s); err != nil {
		t.Fatal(err)
	}
	return e, s
}
func TestBaseConnectivityActualAdapterLifecycleObservationAndPureReads(t *testing.T) {
	f, api, _, _ := newBaseKCFixture(t, nil)
	accepted := createBaseVPC(t, f, "base-lifecycle")
	if accepted.BaseConnectivity == nil || accepted.State != biz.Provisioning {
		t.Fatal("missing atomic base receipt", accepted)
	}
	v := baseState(t, f, accepted.ID, biz.Available)
	if v.BaseConnectivity == nil || v.BaseConnectivity.State != "ready" || v.BaseConnectivity.ObservationStale {
		t.Fatal("managed base projection is not ready or has a read error", v)
	}
	e, s := baseIDs(t, f, v.ID)
	var ns, snatName, vpcName, eipName string
	if err := f.owner.QueryRow(f.ctx, `SELECT b.namespace,b.provider_name,v.provider_name,e.provider_name FROM network_provider_bindings b JOIN network_provider_bindings v ON v.tenant_id=b.tenant_id AND v.vpc_id=$3 JOIN network_provider_bindings e ON e.tenant_id=b.tenant_id AND e.eip_id=$4 WHERE b.tenant_id=$1 AND b.snat_id=$2`, f.tenant, s, v.ID, e).Scan(&ns, &snatName, &vpcName, &eipName); err != nil {
		t.Fatal(err)
	}
	snat := api.Object("snats", ns, snatName)
	if snat["spec"].(map[string]any)["vpc"] != vpcName {
		t.Fatal("SNAT Provider vpc reference must be a short name", snat)
	}
	eip := api.Object("eips", ns, eipName)
	if eip["status"].(map[string]any)["boundResource"].(map[string]any)["vpc"] != ns+"/"+vpcName {
		t.Fatal("boundResource requires qualified VPC")
	}
	if _, err := f.e.GetEIP(f.ctx, "", e); biz.ReasonOf(err) != biz.ResourceNotFound {
		t.Fatal("system EIP escaped")
	}
	if _, err := f.e.GetVPCSnat(f.ctx, "", s, false); biz.ReasonOf(err) != biz.ResourceNotFound {
		t.Fatal("system SNAT escaped")
	}
	var before, after string
	query := `SELECT jsonb_build_object('vpc',to_jsonb(v),'base',to_jsonb(b),'op',to_jsonb(o))::text FROM network_vpcs v JOIN network_vpc_base_connectivity b USING(tenant_id,vpc_id) JOIN network_operations o ON o.tenant_id=v.tenant_id AND o.operation_id=$3 WHERE v.tenant_id=$1 AND v.vpc_id=$2`
	if err := f.owner.QueryRow(f.ctx, query, f.tenant, v.ID, accepted.LastOperationID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err := f.n.GetVPC(f.ctx, f.tenant, v.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := f.n.ListVPCs(f.ctx, biz.ListVPCs{TenantID: f.tenant, Limit: 10}); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.owner.QueryRow(f.ctx, query, f.tenant, v.ID, accepted.LastOperationID).Scan(&after); err != nil || after != before {
		t.Fatal("reads mutated persistent product state", err)
	}
	original := api.Object("snats", ns, snatName)
	drift := api.Object("snats", ns, snatName)
	drift["status"].(map[string]any)["phase"] = "Disabled"
	api.Change("snats", drift, false)
	f.drive(t, func() bool {
		current, err := f.n.GetVPC(f.ctx, f.tenant, v.ID)
		return err == nil && current.State == biz.Available && current.BaseConnectivity != nil && current.BaseConnectivity.State == "degraded"
	})
	op, err := f.n.GetOperation(f.ctx, f.tenant, accepted.LastOperationID)
	if err != nil || op.State != biz.Succeeded {
		t.Fatal("child projection rewrote VPC history", op, err)
	}
	api.Change("snats", original, false)
	baseState(t, f, v.ID, biz.Available)
	// Switching/closing platform allocation cannot move an accepted base address.
	var pool string
	if err = f.owner.QueryRow(f.ctx, `SELECT pool_id FROM network_vpc_base_connectivity WHERE tenant_id=$1 AND vpc_id=$2`, f.tenant, v.ID).Scan(&pool); err != nil {
		t.Fatal(err)
	}
	setIntranetPool(t, f, biz.PlatformIntent{Kind: "set_intranet_pool_allocation", ID: pool, IdempotencyKey: "close-allocated", Enabled: false})
	if _, err = f.n.DeleteVPC(f.ctx, f.tenant, v.ID); err != nil {
		t.Fatal(err)
	}
	baseState(t, f, v.ID, biz.Deleted)
	var active int
	if err = f.owner.QueryRow(f.ctx, `SELECT count(*) FROM network_eip_claims WHERE tenant_id=$1 AND eip_id=$2 AND released_at IS NULL`, f.tenant, e).Scan(&active); err != nil || active != 0 {
		t.Fatal("base claim retained after confirmed cleanup", active, err)
	}
	for _, target := range []struct{ kind, name string }{{"snats", snatName}, {"eips", eipName}, {"vpcs", vpcName}} {
		if api.Object(target.kind, ns, target.name) != nil {
			t.Fatal("cleanup left owned CR", target)
		}
	}
}
func TestBaseConnectivityTerminationBeforeAnyProviderCreate(t *testing.T) {
	f, api, _, _ := newBaseKCFixture(t, nil)
	v := createBaseVPC(t, f, "never-dispatched")
	e, s := baseIDs(t, f, v.ID)
	if _, err := f.n.DeleteVPC(f.ctx, f.tenant, v.ID); err != nil {
		t.Fatal(err)
	}
	baseState(t, f, v.ID, biz.Deleted)
	requireBaseProcessCleanup(t, f, api, v.ID, e, s)
	for _, kind := range []string{"vpcs", "eips", "snats"} {
		api.Backend.Mu.Lock()
		creates, deletes := api.Backend.Creates[kind], api.Backend.Deletes[kind]
		api.Backend.Mu.Unlock()
		if creates != 0 || deletes != 0 {
			t.Fatal("never-dispatched termination wrote Provider resources", kind, creates, deletes)
		}
		requireBaseProcessRequestCount(t, api, nil, http.MethodPost, kind, 0)
		requireBaseProcessRequestCount(t, api, nil, http.MethodDelete, kind, 0)
	}
	var retired int
	if err := f.owner.QueryRow(f.ctx, `SELECT count(*) FROM network_provider_bindings WHERE tenant_id=$1 AND coalesce(vpc_id,subnet_id,eip_id,snat_id)=ANY($2::text[]) AND NOT create_dispatched AND provider_uid='' AND pending_action=''`, f.tenant, []string{v.ID, e, s}).Scan(&retired); err != nil || retired != 3 {
		t.Fatal("never-dispatched evidence lost", retired, err)
	}
	var unscheduled int
	if err := f.owner.QueryRow(f.ctx, `SELECT count(*) FROM network_reconciliations WHERE tenant_id=$1 AND (eip_id=$2 OR snat_id=$3) AND retired AND next_run_at='infinity' AND processed_generation=requested_generation`, f.tenant, e, s).Scan(&unscheduled); err != nil || unscheduled != 2 {
		t.Fatal("cancelled children still scheduled", unscheduled, err)
	}
	operations := baseProcessCreationOperations(t, f, v.ID, e, s)
	history := baseProcessHistory(t, f, operations)
	for i := 0; i < 8; i++ {
		if _, err := f.w.Step(f.ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.owner.QueryRow(f.ctx, `SELECT count(*) FROM network_reconciliations WHERE tenant_id=$1 AND (eip_id=$2 OR snat_id=$3) AND retired AND next_run_at='infinity'`, f.tenant, e, s).Scan(&unscheduled); err != nil || unscheduled != 2 {
		t.Fatal("retired cancellation was reawakened", unscheduled, err)
	}
	op, err := f.n.GetOperation(f.ctx, f.tenant, v.LastOperationID)
	if err != nil || op.State != biz.OpFailed || op.Reason != "CREATE_TERMINATED" {
		t.Fatal("create termination result", op, err)
	}
	if after := baseProcessHistory(t, f, operations); after != history {
		t.Fatal("retired cancellation rewrote parent/child operation history")
	}
	replay := createBaseVPC(t, f, "never-dispatched")
	if replay.ID != v.ID || replay.LastOperationID != v.LastOperationID || replay.State != biz.Provisioning {
		t.Fatal("termination changed the immutable acceptance receipt", replay)
	}
}

func TestBaseConnectivityPinsPoolAndChildrenAcrossRetryAndDefaultSwitch(t *testing.T) {
	f, api, _, _ := newBaseKCFixture(t, nil)
	v := createBaseVPC(t, f, "pinned")
	e, s := baseIDs(t, f, v.ID)
	intent := intranetIntent("next-intranet", "10.233.254.0/24", "10.233.254.1")
	intent.Pool.DefaultVPCUID = api.Object("vpcs", "kcn-system", "kcn-cluster")["metadata"].(map[string]any)["uid"].(string)
	next := readyIntranetPool(t, f, f.platform(t, intent))
	setIntranetPool(t, f, biz.PlatformIntent{Kind: "set_default_intranet_pool", ID: next.ID, IdempotencyKey: "switch"})
	replay := createBaseVPC(t, f, "pinned")
	if replay.ID != v.ID {
		t.Fatal("replay chose a new VPC")
	}
	a, b := baseIDs(t, f, v.ID)
	if a != e || b != s {
		t.Fatal("replay replaced child identities")
	}
	var pool string
	if err := f.owner.QueryRow(f.ctx, `SELECT pool_id FROM network_vpc_base_connectivity WHERE tenant_id=$1 AND vpc_id=$2`, f.tenant, v.ID).Scan(&pool); err != nil || pool == next.ID {
		t.Fatal("accepted operation changed pool", err)
	}
	baseState(t, f, v.ID, biz.Available)
	setIntranetPool(t, f, biz.PlatformIntent{Kind: "set_intranet_pool_allocation", ID: next.ID, IdempotencyKey: "disable-new", Enabled: false})
	if _, err := f.n.CreateVPC(context.Background(), biz.CreateVPC{TenantID: f.tenant, Name: "closed", CIDR: "10.61.0.0/16", IdempotencyKey: "closed"}); biz.ReasonOf(err) != biz.BaseConnectivityNotReady {
		t.Fatal("closed pool accepted allocation", err)
	}
	var name string
	if err := f.owner.QueryRow(f.ctx, `SELECT provider_name FROM network_provider_bindings WHERE tenant_id=$1 AND eip_id=$2`, f.tenant, e).Scan(&name); err != nil || name != strings.Replace(e, "_", "-", 1) {
		t.Fatal("fixed provider name drift", err)
	}
}

// The Provider successfully creates the original system SNAT object but cannot
// apply its binding yet. This exercises the initial aggregate gate, not the
// already-available resource drift or response-loss paths.
func TestPendingManagedSnatDoesNotBlockNativeVPCReady(t *testing.T) {
	var armed atomic.Bool
	var posts atomic.Int32
	f, api, _, _ := newBaseKCFixture(t, func(w http.ResponseWriter, r *http.Request, api *controlled.Server) bool {
		if !armed.Load() || r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/snats") {
			return false
		}
		posts.Add(1)
		recorder := httptest.NewRecorder()
		api.Backend.ServeHTTP(recorder, r)
		var object map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &object); err != nil {
			t.Error(err)
			return false
		}
		object["status"].(map[string]any)["phase"] = "Pending"
		api.Change("snats", object, false)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(recorder.Code)
		_ = json.NewEncoder(w).Encode(object)
		return true
	})
	armed.Store(true)
	accepted := createBaseVPC(t, f, "native-ready-independent")
	_, snatID := baseIDs(t, f, accepted.ID)
	f.drive(t, func() bool {
		current, err := f.n.GetVPC(f.ctx, f.tenant, accepted.ID)
		var observed bool
		if err == nil {
			err = f.owner.QueryRow(f.ctx, `SELECT observed_at IS NOT NULL FROM network_snat_bindings WHERE tenant_id=$1 AND snat_id=$2`, f.tenant, snatID).Scan(&observed)
		}
		return err == nil && observed && current.State == biz.Available && current.BaseConnectivity != nil && current.BaseConnectivity.State == "pending"
	})
	operation, err := f.n.GetOperation(f.ctx, f.tenant, accepted.LastOperationID)
	if err != nil || operation.State != biz.Succeeded || operation.CompletedAt == nil {
		t.Fatal("child pending blocked native VPC operation", operation, err)
	}
	var namespace, name string
	if err = f.owner.QueryRow(f.ctx, `SELECT namespace,provider_name FROM network_provider_bindings WHERE tenant_id=$1 AND snat_id=$2`, f.tenant, snatID).Scan(&namespace, &name); err != nil {
		t.Fatal(err)
	}
	object := api.Object("snats", namespace, name)
	object["status"].(map[string]any)["phase"] = "Bound"
	api.Change("snats", object, false)
	baseState(t, f, accepted.ID, biz.Available)
	after, err := f.n.GetOperation(f.ctx, f.tenant, accepted.LastOperationID)
	if err != nil || !after.UpdatedAt.Equal(operation.UpdatedAt) || posts.Load() != 1 {
		t.Fatal("child recovery changed history or redeclared object", after, posts.Load(), err)
	}
}
