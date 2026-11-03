package data_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
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
		return err == nil && v.State == state
	})
	return v
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
		t.Fatal("aggregate is not fresh and ready", v)
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
	drift["metadata"].(map[string]any)["uid"] = "replacement-uid"
	api.Change("snats", drift, false)
	degraded := baseState(t, f, v.ID, biz.Degraded)
	if degraded.BaseConnectivity.State != "degraded" {
		t.Fatal("base dependency drift not aggregated", degraded)
	}
	api.Change("snats", original, false)
	baseState(t, f, v.ID, biz.Available)
	op, err := f.n.GetOperation(f.ctx, f.tenant, accepted.LastOperationID)
	if err != nil || op.State != biz.Succeeded {
		t.Fatal("history rewritten", op, err)
	}
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
func TestBaseConnectivityInitialSnatPendingKeepsCreateOpenThenWorkerRecovers(t *testing.T) {
	type bindingFacts struct {
		snat, eip  map[string]any
		statusCode int
	}
	captured := make(chan bindingFacts, 1)
	var armed, intercepted atomic.Bool
	var snatPosts atomic.Int32
	intercept := func(w http.ResponseWriter, r *http.Request, api *controlled.Server) bool {
		if !armed.Load() || r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/snats") {
			return false
		}
		snatPosts.Add(1)
		if !intercepted.CompareAndSwap(false, true) {
			return false
		}
		recorder := httptest.NewRecorder()
		api.Backend.ServeHTTP(recorder, r)
		var created map[string]any
		if recorder.Code >= 200 && recorder.Code < 300 && json.Unmarshal(recorder.Body.Bytes(), &created) == nil {
			metadata, _ := created["metadata"].(map[string]any)
			namespace, _ := metadata["namespace"].(string)
			name, _ := metadata["name"].(string)
			spec, _ := created["spec"].(map[string]any)
			eipName, _ := spec["eip"].(string)
			originalSnat := api.Object("snats", namespace, name)
			originalEIP := api.Object("eips", namespace, eipName)
			if originalSnat != nil && originalEIP != nil {
				pendingSnat := api.Object("snats", namespace, name)
				pendingSnat["status"].(map[string]any)["phase"] = "Pending"
				pendingEIP := api.Object("eips", namespace, eipName)
				pendingEIP["status"].(map[string]any)["phase"] = "Available"
				pendingEIP["status"].(map[string]any)["boundResource"] = nil
				api.Change("eips", pendingEIP, false)
				api.Change("snats", pendingSnat, false)
				captured <- bindingFacts{snat: originalSnat, eip: originalEIP, statusCode: recorder.Code}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(recorder.Code)
				_ = json.NewEncoder(w).Encode(api.Object("snats", namespace, name))
				return true
			}
		}
		captured <- bindingFacts{statusCode: recorder.Code}
		for key, values := range recorder.Header() {
			w.Header()[key] = values
		}
		w.WriteHeader(recorder.Code)
		_, _ = w.Write(recorder.Body.Bytes())
		return true
	}
	f, api, _, _ := newBaseKCFixture(t, intercept)
	armed.Store(true)
	accepted := createBaseVPC(t, f, "initial-snat-pending")
	eipID, snatID := baseIDs(t, f, accepted.ID)
	var facts *bindingFacts
	assertCreateOpen := func() {
		t.Helper()
		v, err := f.n.GetVPC(f.ctx, f.tenant, accepted.ID)
		if err != nil || v.State != biz.Provisioning || v.LastOperationID != accepted.LastOperationID || v.BaseConnectivity == nil || v.BaseConnectivity.State == "ready" {
			t.Fatal("unapplied initial SNAT completed VPC aggregate", v, err)
		}
		operation, err := f.n.GetOperation(f.ctx, f.tenant, accepted.LastOperationID)
		if err != nil || operation.State == biz.Succeeded || operation.CompletedAt != nil {
			t.Fatal("unapplied initial SNAT completed create operation", operation, err)
		}
		var active int
		if err := f.owner.QueryRow(f.ctx, `SELECT count(*) FROM network_eip_claims WHERE tenant_id=$1 AND eip_id=$2 AND snat_id=$3 AND released_at IS NULL`, f.tenant, eipID, snatID).Scan(&active); err != nil || active != 1 {
			t.Fatal("pending base binding released exclusive EIP claim", active, err)
		}
		actualEIP, actualSnat := baseIDs(t, f, accepted.ID)
		if actualEIP != eipID || actualSnat != snatID {
			t.Fatal("pending binding changed fixed base identities")
		}
	}
	f.drive(t, func() bool {
		assertCreateOpen()
		if facts == nil {
			select {
			case value := <-captured:
				facts = &value
			default:
				return false
			}
		}
		if facts.snat == nil || facts.eip == nil {
			t.Fatal("controlled pending-binding injection failed", facts.statusCode)
		}
		var observedPending bool
		err := f.owner.QueryRow(f.ctx, `SELECT s.purpose='intranet' AND s.state='provisioning' AND s.observed_at IS NOT NULL AND s.applied_enabled IS NULL AND s.reason='PROVIDER_NOT_READY' AND binding.provider_uid<>'' AND binding.pending_action='' AND b.provider_ready AND b.state='pending' AND b.reason='PROVIDER_NOT_READY' FROM network_snat_bindings s JOIN network_provider_bindings binding ON binding.tenant_id=s.tenant_id AND binding.snat_id=s.snat_id JOIN network_vpc_base_connectivity b ON b.tenant_id=s.tenant_id AND b.snat_id=s.snat_id WHERE s.tenant_id=$1 AND s.snat_id=$2`, f.tenant, snatID).Scan(&observedPending)
		if err != nil {
			t.Fatal(err)
		}
		return observedPending
	})
	// Repeated retries cannot turn the accepted provider object into a successful
	// VPC until the actual same-identity facts satisfy the binding contract.
	for i := 0; i < 12; i++ {
		if _, err := f.w.Step(f.ctx); err != nil {
			t.Fatal(err)
		}
		assertCreateOpen()
	}
	var bound bool
	if err := f.owner.QueryRow(f.ctx, `SELECT state='bound' FROM network_eip_claims WHERE tenant_id=$1 AND eip_id=$2 AND released_at IS NULL`, f.tenant, eipID).Scan(&bound); err != nil || bound {
		t.Fatal("unapplied binding claim incorrectly confirmed", bound, err)
	}
	namespace := facts.snat["metadata"].(map[string]any)["namespace"].(string)
	for _, target := range []struct {
		kind     string
		original map[string]any
	}{{"eips", facts.eip}, {"snats", facts.snat}} {
		metadata := target.original["metadata"].(map[string]any)
		name := metadata["name"].(string)
		current := api.Object(target.kind, namespace, name)
		currentMeta := current["metadata"].(map[string]any)
		if currentMeta["uid"] != metadata["uid"] || currentMeta["generation"] != metadata["generation"] || !reflect.DeepEqual(current["spec"], target.original["spec"]) {
			t.Fatal("pending observation altered accepted Provider identity or spec", target.kind)
		}
		// Restore only controlled current-generation status; do not create another
		// object, resubmit product intent, or manually wake/update database rows.
		current["status"] = target.original["status"]
		api.Change(target.kind, current, false)
	}
	current := baseState(t, f, accepted.ID, biz.Available)
	if current.LastOperationID != accepted.LastOperationID || current.BaseConnectivity == nil || current.BaseConnectivity.State != "ready" {
		t.Fatal("recovery did not complete the original aggregate", current)
	}
	operation, err := f.n.GetOperation(f.ctx, f.tenant, accepted.LastOperationID)
	if err != nil || operation.State != biz.Succeeded || operation.CompletedAt == nil {
		t.Fatal("same create operation did not recover", operation, err)
	}
	if snatPosts.Load() != 1 {
		t.Fatal("pending binding allocated another SNAT", snatPosts.Load())
	}
	for _, target := range []struct {
		id, kind string
		original map[string]any
	}{{eipID, "eips", facts.eip}, {snatID, "snats", facts.snat}} {
		metadata := target.original["metadata"].(map[string]any)
		var uid string
		if err := f.owner.QueryRow(f.ctx, `SELECT provider_uid FROM network_provider_bindings WHERE tenant_id=$1 AND coalesce(eip_id,snat_id)=$2`, f.tenant, target.id).Scan(&uid); err != nil || uid != metadata["uid"] {
			t.Fatal("recovery changed fixed provider UID", target.kind, uid, err)
		}
		api.Backend.Mu.Lock()
		creates := api.Backend.Creates[target.kind]
		api.Backend.Mu.Unlock()
		if creates != 1 {
			t.Fatal("pending binding caused duplicate allocation", target.kind, creates)
		}
	}
	if err := f.owner.QueryRow(f.ctx, `SELECT state='bound' FROM network_eip_claims WHERE tenant_id=$1 AND eip_id=$2 AND snat_id=$3 AND released_at IS NULL`, f.tenant, eipID, snatID).Scan(&bound); err != nil || !bound {
		t.Fatal("applied base binding did not confirm the original claim", bound, err)
	}
	if _, err = f.n.DeleteVPC(f.ctx, f.tenant, accepted.ID); err != nil {
		t.Fatal(err)
	}
	baseState(t, f, accepted.ID, biz.Deleted)
}
