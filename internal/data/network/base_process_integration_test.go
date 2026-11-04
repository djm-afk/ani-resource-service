package data_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	controlled "github.com/zhangzhe-ctrl/ani-resource-service/tests/net05a/provider"
	"github.com/zhangzhe-ctrl/ani-resource-service/tests/testenv"
)

// Critical boundaries execute the production service, durable PostgreSQL state,
// and actual KC adapter against a controlled HTTP API. They prove recovery of
// provider contracts only; they never assert live network traffic or kc health.
// Per-resource POST/late DELETE recovery is covered with fresh production
// runtimes in base_recovery_integration_test.go; zero-write cancellation has
// its own shorter actual-adapter test in base_connectivity_integration_test.go.
type baseProcessFault struct {
	method, kind               string
	delayed                    bool
	armed, captured            atomic.Bool
	reached, release, executed chan struct{}
	releaseOnce                sync.Once
	status                     atomic.Int32
}

func newBaseProcessFault(method, kind string, delayed bool) *baseProcessFault {
	return &baseProcessFault{method: method, kind: kind, delayed: delayed, reached: make(chan struct{}), release: make(chan struct{}), executed: make(chan struct{})}
}
func (f *baseProcessFault) unblock() { f.releaseOnce.Do(func() { close(f.release) }) }
func (f *baseProcessFault) intercept(w http.ResponseWriter, r *http.Request, api *controlled.Server) bool {
	match := r.Method == f.method && ((f.method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/"+f.kind)) || (f.method == http.MethodDelete && strings.Contains(r.URL.Path, "/"+f.kind+"/")))
	if !match || !f.armed.Load() || !f.captured.CompareAndSwap(false, true) {
		return false
	}
	// Copy before killing the service: a Provider may finish an accepted request
	// after the initiating connection and its context have disappeared.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		f.status.Store(-1)
		close(f.reached)
		close(f.executed)
		http.Error(w, "request read failed", http.StatusBadRequest)
		return true
	}
	detached := r.Clone(context.WithoutCancel(r.Context()))
	detached.Body = io.NopCloser(bytes.NewReader(body))
	recorder := httptest.NewRecorder()
	if f.delayed {
		close(f.reached)
		<-f.release
	}
	api.Backend.ServeHTTP(recorder, detached)
	f.status.Store(int32(recorder.Code))
	close(f.executed)
	if !f.delayed {
		close(f.reached)
		<-f.release
	}
	for key, values := range recorder.Header() {
		w.Header()[key] = values
	}
	w.WriteHeader(recorder.Code)
	_, _ = w.Write(recorder.Body.Bytes())
	return true
}
func waitBaseFault(t *testing.T, fault *baseProcessFault, process *networkProcess) {
	t.Helper()
	select {
	case <-fault.reached:
	case <-time.After(18 * time.Second):
		log, _ := os.ReadFile(process.logPath)
		t.Fatalf("service did not reach %s %s boundary: %s", fault.method, fault.kind, log)
	}
	if !fault.delayed && (fault.status.Load() < 200 || fault.status.Load() >= 300) {
		t.Fatal("controlled provider did not execute the intercepted operation", fault.status.Load())
	}
}
func waitBaseProcessState(t *testing.T, f *egressFixture, id string, state biz.ResourceState, process *networkProcess) biz.VPC {
	t.Helper()
	var current biz.VPC
	var lastErr error
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		current, lastErr = f.n.GetVPC(f.ctx, f.tenant, id)
		if lastErr == nil && baseFixtureProductsSettled(current, state) {
			return current
		}
		time.Sleep(20 * time.Millisecond)
	}
	log, _ := os.ReadFile(process.logPath)
	t.Fatalf("service did not reach %s; current=%+v err=%v log=%s", state, current, lastErr, log)
	return current
}
func baseProcessClaimCount(t *testing.T, f *egressFixture, eip string) int {
	t.Helper()
	var active int
	if err := f.owner.QueryRow(f.ctx, `SELECT count(*) FROM network_eip_claims WHERE tenant_id=$1 AND eip_id=$2 AND released_at IS NULL`, f.tenant, eip).Scan(&active); err != nil {
		t.Fatal(err)
	}
	return active
}
func baseProcessHistory(t *testing.T, f *egressFixture, ids []string) string {
	t.Helper()
	var snapshot string
	if err := f.owner.QueryRow(f.ctx, `SELECT coalesce(jsonb_agg(to_jsonb(o) ORDER BY operation_id),'[]'::jsonb)::text FROM network_operations o WHERE tenant_id=$1 AND operation_id::text=ANY($2::text[])`, f.tenant, ids).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot
}
func baseProcessCreationOperations(t *testing.T, f *egressFixture, vpc, eip, snat string) []string {
	t.Helper()
	rows, err := f.owner.Query(f.ctx, `SELECT operation_id::text FROM network_operations WHERE tenant_id=$1 AND (vpc_id=$2 OR eip_id=$3 OR snat_id=$4) AND kind IN ('create_vpc','create_eip','bind_snat') ORDER BY operation_id`, f.tenant, vpc, eip, snat)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if rows.Err() != nil || len(ids) != 3 {
		t.Fatal("creation operations were not accepted atomically", ids, rows.Err())
	}
	return ids
}
func baseProcessProviderIdentity(t *testing.T, f *egressFixture, api *controlled.Server, id, kind string) string {
	t.Helper()
	var ns, name, uid string
	if err := f.owner.QueryRow(f.ctx, `SELECT namespace,provider_name,provider_uid FROM network_provider_bindings WHERE tenant_id=$1 AND coalesce(vpc_id,subnet_id,eip_id,snat_id)=$2`, f.tenant, id).Scan(&ns, &name, &uid); err != nil {
		t.Fatal(err)
	}
	object := api.Object(kind, ns, name)
	if object == nil {
		return ""
	}
	metadata, _ := object["metadata"].(map[string]any)
	actual, _ := metadata["uid"].(string)
	if actual == "" {
		t.Fatal("Provider object has no identity", kind, id)
	}
	if uid != "" && actual != uid {
		t.Fatal("persisted Provider UID differs", kind, id, uid, actual)
	}
	return actual
}
func requireBaseProcessRequestCount(t *testing.T, api *controlled.Server, fault *baseProcessFault, method, kind string, expected int64) {
	t.Helper()
	counts, _ := api.Counts()
	actual := counts[method+"/"+kind]
	// The one held request executes directly against Backend and therefore is
	// deliberately outside Server's ordinary request accounting.
	if fault != nil && fault.captured.Load() && fault.method == method && fault.kind == kind {
		actual++
	}
	if actual != expected {
		t.Fatal("unexpected Provider HTTP request count", method, kind, actual, expected)
	}
}
func requireBaseProcessFixedIdentities(t *testing.T, f *egressFixture, api *controlled.Server, vpc, eip, snat string, prior map[string]string) {
	t.Helper()
	actualEIP, actualSnat := baseIDs(t, f, vpc)
	if actualEIP != eip || actualSnat != snat {
		t.Fatal("restart allocated new base child identities")
	}
	for _, target := range []struct{ id, kind string }{{vpc, "vpcs"}, {eip, "eips"}, {snat, "snats"}} {
		uid := baseProcessProviderIdentity(t, f, api, target.id, target.kind)
		if uid == "" {
			t.Fatal("ready base resource lacks Provider identity", target.kind)
		}
		if before := prior[target.kind]; before != "" && before != uid {
			t.Fatal("restart replaced the original Provider object", target.kind, before, uid)
		}
		api.Backend.Mu.Lock()
		creates := api.Backend.Creates[target.kind]
		api.Backend.Mu.Unlock()
		if creates != 1 {
			t.Fatal("recovery sent a duplicate Provider create", target.kind, creates)
		}
	}
	if baseProcessClaimCount(t, f, eip) != 1 {
		t.Fatal("ready base binding lost exclusive EIP claim")
	}
	var persisted int
	if err := f.owner.QueryRow(f.ctx, `SELECT count(*) FROM network_provider_bindings WHERE tenant_id=$1 AND coalesce(vpc_id,subnet_id,eip_id,snat_id)=ANY($2::text[]) AND provider_uid<>'' AND pending_action=''`, f.tenant, []string{vpc, eip, snat}).Scan(&persisted); err != nil || persisted != 3 {
		t.Fatal("ready identities are not durably recorded", persisted, err)
	}
}
func requireBaseProcessCleanup(t *testing.T, f *egressFixture, api *controlled.Server, vpc, eip, snat string) {
	t.Helper()
	if baseProcessClaimCount(t, f, eip) != 0 {
		t.Fatal("confirmed cleanup retained the EIP claim")
	}
	for _, target := range []struct{ id, kind string }{{vpc, "vpcs"}, {eip, "eips"}, {snat, "snats"}} {
		if baseProcessProviderIdentity(t, f, api, target.id, target.kind) != "" {
			t.Fatal("deleted VPC retained a base Provider object", target.kind)
		}
	}
	var live int
	if err := f.owner.QueryRow(f.ctx, `SELECT (SELECT count(*) FROM network_eips WHERE tenant_id=$1 AND eip_id=$2 AND state<>'deleted')+(SELECT count(*) FROM network_snat_bindings WHERE tenant_id=$1 AND snat_id=$3 AND state<>'deleted')`, f.tenant, eip, snat).Scan(&live); err != nil || live != 0 {
		t.Fatal("child resources did not reach deleted", live, err)
	}
}

func TestBaseConnectivityServiceProcessesRecoverCriticalDurableBoundaries(t *testing.T) {
	if os.Getenv("NETWORK_TEST_ADMIN_DSN") == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "network-base-recovery")
	build := exec.Command("go", "build", "-race", "-trimpath", "-o", binary, "./cmd/ani-resource-service")
	build.Dir = root
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build base recovery service: %v\n%s", err, output)
	}
	t.Run("partial_unknown_EIP_termination_before_late_success", func(t *testing.T) {
		fault := newBaseProcessFault(http.MethodPost, "eips", true)
		f, api, db, kube := newBaseKCFixture(t, fault.intercept)
		t.Cleanup(fault.unblock)
		start := func() *networkProcess {
			return startNetworkProcess(t, root, binary, db.RuntimeDSN, kube, testenv.SigningKey(), "base")
		}
		first := start()
		fault.armed.Store(true)
		accepted := createBaseVPC(t, f, "terminate-unknown-eip")
		eip, snat := baseIDs(t, f, accepted.ID)
		waitBaseFault(t, fault, first)
		first.kill(t)
		vpcUID := baseProcessProviderIdentity(t, f, api, accepted.ID, "vpcs")
		if vpcUID == "" || baseProcessProviderIdentity(t, f, api, eip, "eips") != "" || baseProcessProviderIdentity(t, f, api, snat, "snats") != "" {
			t.Fatal("fault is not the intended VPC-created/EIP-unknown/SNAT-unsent boundary")
		}
		deleting, err := f.n.DeleteVPC(f.ctx, f.tenant, accepted.ID)
		if err != nil {
			t.Fatal(err)
		}
		if baseProcessClaimCount(t, f, eip) != 1 {
			t.Fatal("termination admission released an unresolved base EIP claim")
		}
		var dispatched bool
		var pending string
		if err := f.owner.QueryRow(f.ctx, `SELECT create_dispatched,pending_action FROM network_provider_bindings WHERE tenant_id=$1 AND eip_id=$2`, f.tenant, eip).Scan(&dispatched, &pending); err != nil || !dispatched || pending != "create" {
			t.Fatal("termination erased unknown EIP write evidence", dispatched, pending, err)
		}
		if currentEIP, currentSnat := baseIDs(t, f, accepted.ID); currentEIP != eip || currentSnat != snat {
			t.Fatal("termination changed fixed child identities")
		}
		// No service process is alive while the controlled Provider finishes the
		// original POST. Cleanup must recover this late UID after restart.
		fault.unblock()
		select {
		case <-fault.executed:
		case <-time.After(3 * time.Second):
			t.Fatal("late EIP success did not materialize after termination")
		}
		if fault.status.Load() < 200 || fault.status.Load() >= 300 {
			t.Fatal("late EIP request failed", fault.status.Load())
		}
		lateUID := baseProcessProviderIdentity(t, f, api, eip, "eips")
		if lateUID == "" {
			t.Fatal("late EIP has no stable Provider identity")
		}
		if baseProcessClaimCount(t, f, eip) != 1 {
			t.Fatal("claim released before process could confirm cleanup")
		}
		second := start()
		deleted := waitBaseProcessState(t, f, accepted.ID, biz.Deleted, second)
		if deleted.LastOperationID != deleting.LastOperationID {
			t.Fatal("partial termination replaced the delete operation")
		}
		requireBaseProcessCleanup(t, f, api, accepted.ID, eip, snat)
		if currentEIP, currentSnat := baseIDs(t, f, accepted.ID); currentEIP != eip || currentSnat != snat {
			t.Fatal("late-success cleanup changed fixed child identities")
		}
		for _, target := range []struct{ id, uid string }{{accepted.ID, vpcUID}, {eip, lateUID}} {
			var persistedUID, action string
			if err := f.owner.QueryRow(f.ctx, `SELECT provider_uid,pending_action FROM network_provider_bindings WHERE tenant_id=$1 AND coalesce(vpc_id,subnet_id,eip_id,snat_id)=$2`, f.tenant, target.id).Scan(&persistedUID, &action); err != nil || persistedUID != target.uid || action != "" {
				t.Fatal("cleanup failed to retain exact late-success identity", target.id, persistedUID, target.uid, action, err)
			}
		}
		var unsent bool
		if err := f.owner.QueryRow(f.ctx, `SELECT NOT create_dispatched AND provider_uid='' AND pending_action='' FROM network_provider_bindings WHERE tenant_id=$1 AND snat_id=$2`, f.tenant, snat).Scan(&unsent); err != nil || !unsent {
			t.Fatal("never-sent SNAT cancellation evidence lost", unsent, err)
		}
		for _, kind := range []string{"vpcs", "eips"} {
			requireBaseProcessRequestCount(t, api, fault, http.MethodPost, kind, 1)
			requireBaseProcessRequestCount(t, api, fault, http.MethodDelete, kind, 1)
		}
		requireBaseProcessRequestCount(t, api, fault, http.MethodPost, "snats", 0)
		requireBaseProcessRequestCount(t, api, fault, http.MethodDelete, "snats", 0)
		createOperation, err := f.n.GetOperation(f.ctx, f.tenant, accepted.LastOperationID)
		if err != nil || createOperation.State != biz.OpFailed || createOperation.Reason != "CREATE_TERMINATED" {
			t.Fatal("partial termination did not preserve failed create conclusion", createOperation, err)
		}
		deleteOperation, err := f.n.GetOperation(f.ctx, f.tenant, deleting.LastOperationID)
		if err != nil || deleteOperation.State != biz.Succeeded {
			t.Fatal("late-success cleanup did not complete delete operation", deleteOperation, err)
		}
		replay := createBaseVPC(t, f, "terminate-unknown-eip")
		if replay.ID != accepted.ID || replay.LastOperationID != accepted.LastOperationID || replay.State != biz.Provisioning {
			t.Fatal("termination changed immutable acceptance replay", replay)
		}
		second.stop(t)
		t.Logf("controlled_process_boundary=partial_unknown_EIP_termination vpc=%s eip=%s late_uid=%s snat=%s snat_create_requests=0 data_plane=not_verified", accepted.ID, eip, lateUID, snat)
	})
	for _, kind := range []string{"snats"} {
		t.Run(kind+"_DELETE_success_response_lost", func(t *testing.T) {
			fault := newBaseProcessFault(http.MethodDelete, kind, false)
			f, api, db, kube := newBaseKCFixture(t, fault.intercept)
			t.Cleanup(fault.unblock)
			start := func() *networkProcess {
				return startNetworkProcess(t, root, binary, db.RuntimeDSN, kube, testenv.SigningKey(), "base")
			}
			first := start()
			accepted := createBaseVPC(t, f, "delete-"+kind)
			eip, snat := baseIDs(t, f, accepted.ID)
			waitBaseProcessState(t, f, accepted.ID, biz.Available, first)
			requireBaseProcessFixedIdentities(t, f, api, accepted.ID, eip, snat, nil)
			for _, kind := range []string{"vpcs", "eips", "snats"} {
				requireBaseProcessRequestCount(t, api, fault, http.MethodPost, kind, 1)
			}
			operations := baseProcessCreationOperations(t, f, accepted.ID, eip, snat)
			history := baseProcessHistory(t, f, operations)
			fault.armed.Store(true)
			deleting, err := f.n.DeleteVPC(f.ctx, f.tenant, accepted.ID)
			if err != nil {
				t.Fatal(err)
			}
			waitBaseFault(t, fault, first)
			first.kill(t)
			active := baseProcessClaimCount(t, f, eip)
			if kind == "snats" && active != 1 {
				t.Fatal("unknown SNAT deletion released its exclusive claim", active)
			}
			if kind != "snats" && active != 0 {
				t.Fatal("confirmed SNAT release retained its claim into later cleanup", kind, active)
			}
			if after := baseProcessHistory(t, f, operations); after != history {
				t.Fatal("deletion admission rewrote successful creation operations")
			}
			fault.unblock()
			second := start()
			current := waitBaseProcessState(t, f, accepted.ID, biz.Deleted, second)
			if current.LastOperationID != deleting.LastOperationID {
				t.Fatal("restart replaced deletion operation")
			}
			requireBaseProcessCleanup(t, f, api, accepted.ID, eip, snat)
			for _, resource := range []string{"snats", "eips", "vpcs"} {
				api.Backend.Mu.Lock()
				count := api.Backend.Deletes[resource]
				api.Backend.Mu.Unlock()
				if count != 1 {
					t.Fatal("delete response loss issued redundant Provider DELETE", resource, count)
				}
				requireBaseProcessRequestCount(t, api, fault, http.MethodDelete, resource, 1)
			}
			if after := baseProcessHistory(t, f, operations); after != history {
				t.Fatal("delete recovery rewrote successful creation operations")
			}
			op, err := f.n.GetOperation(f.ctx, f.tenant, deleting.LastOperationID)
			if err != nil || op.State != biz.Succeeded {
				t.Fatal("delete operation did not complete", op, err)
			}
			second.stop(t)
			t.Logf("controlled_process_boundary=%s_DELETE_response_lost vpc=%s claim_cleanup=pass data_plane=not_verified", kind, accepted.ID)
		})
	}
}
