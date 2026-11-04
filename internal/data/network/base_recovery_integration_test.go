package data_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	data "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network"
	controlled "github.com/zhangzhe-ctrl/ani-resource-service/tests/net05a/provider"
	"github.com/zhangzhe-ctrl/ani-resource-service/tests/testenv"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// baseRecoveryRuntime owns the production objects that must not survive a
// recovery boundary. Only the external KC HTTP API and the database survive.
// The critical SIGKILL boundaries remain in base_process_integration_test.go.
type baseRecoveryRuntime struct {
	f        *egressFixture
	db       *testenv.Database
	host     string
	provider *data.KCProvider
	cancel   context.CancelFunc
	done     chan error
}

func baseRecoveryPolicy() biz.WorkerPolicy {
	policy := biz.DefaultWorkerPolicy()
	policy.RequestTimeout, policy.Lease = time.Second, 4*time.Second
	policy.ObserveEvery = 50 * time.Millisecond
	policy.RetryMin, policy.RetryMax = 5*time.Millisecond, 20*time.Millisecond
	return policy
}

func (r *baseRecoveryRuntime) construct(t *testing.T, repository *data.Postgres) {
	t.Helper()
	provider, err := data.NewKCProvider(repository, &rest.Config{Host: r.host, QPS: 1000, Burst: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.ConfigureVPCCIDRPresets([]string{"10.42.0.0/16", "10.61.0.0/16", "10.62.0.0/16"}); err != nil {
		t.Fatal(err)
	}
	worker, err := biz.NewWorker(repository, provider, uuid.NewString(), baseRecoveryPolicy())
	if err != nil {
		t.Fatal(err)
	}
	egress, err := biz.NewEgress(repository, provider, biz.ContextEgressAuthorization{}, []byte("0123456789abcdef0123456789abcdef"), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	r.provider = provider
	r.f.p, r.f.e, r.f.n, r.f.w = repository, egress, newNetwork(t, repository, time.Minute), worker
}

func (r *baseRecoveryRuntime) observe(t *testing.T) {
	t.Helper()
	observer, err := r.provider.EnableObservation(baseRecoveryPolicy().RequestTimeout)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel, r.done = cancel, make(chan error, 1)
	go func() { r.done <- observer.Start(ctx) }()
	awaitNET05A(t, 3*time.Second, func() bool { return observer.Snapshot()["source_synced"] == 1 })
}

func (r *baseRecoveryRuntime) close(t *testing.T) {
	t.Helper()
	if r.cancel != nil {
		r.cancel()
		if err := <-r.done; err != nil {
			t.Error(err)
		}
		r.cancel, r.done = nil, nil
	}
	if r.f.p != nil {
		r.f.p.Close()
	}
	r.f.p, r.f.e, r.f.n, r.f.w, r.provider = nil, nil, nil, nil, nil
}

func (r *baseRecoveryRuntime) reopen(t *testing.T) {
	t.Helper()
	if r.f.p != nil || r.provider != nil || r.f.w != nil {
		t.Fatal("recovery must discard the old repository, provider, observer and worker")
	}
	repository, err := data.OpenPostgres(r.f.ctx, r.db.RuntimeDSN, data.Placement{ClusterID: "test-cluster", NamespacePrefix: "tenant-"})
	if err != nil {
		t.Fatal(err)
	}
	r.construct(t, repository)
	r.observe(t)
}

func newBaseRecoveryFixture(t *testing.T, fault *baseProcessFault) (*baseRecoveryRuntime, *controlled.Server) {
	t.Helper()
	f, api, db, kube := newBaseKCFixture(t, fault.intercept)
	config, err := clientcmd.BuildConfigFromFlags("", kube)
	if err != nil {
		t.Fatal(err)
	}
	// The shared fixture supplies real platform admission and configuration.
	// Discard its observer/worker before installing the bounded test runtime.
	f.stopObservation()
	runtime := &baseRecoveryRuntime{f: f, db: db, host: config.Host}
	runtime.construct(t, db.Repository)
	t.Cleanup(func() { runtime.close(t) })
	// Release the held external HTTP request before any runtime/server cleanup.
	t.Cleanup(fault.unblock)
	runtime.observe(t)
	return runtime, api
}

func runBaseWorkerToFault(t *testing.T, f *egressFixture, fault *baseProcessFault) {
	t.Helper()
	ctx, cancel := context.WithTimeout(f.ctx, 8*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		for ctx.Err() == nil {
			if _, err := f.w.Step(ctx); err != nil {
				done <- err
				return
			}
			if fault.captured.Load() {
				done <- nil
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
		done <- ctx.Err()
	}()
	select {
	case <-fault.reached:
	case err := <-done:
		t.Fatal("production worker did not reach the HTTP fault", err)
	}
	if fault.status.Load() < 200 || fault.status.Load() >= 300 {
		t.Fatal("controlled Provider did not accept the held request", fault.status.Load())
	}
	// The HTTP call really times out and the production Step/Finish returns;
	// no direct SQL pending marker or forced lease release is involved.
	if err := <-done; err != nil {
		t.Fatal("production worker failed at the held-response boundary", err)
	}
}

func waitBaseRecoveryState(t *testing.T, f *egressFixture, id string, expected biz.ResourceState) biz.VPC {
	t.Helper()
	var current biz.VPC
	ctx, cancel := context.WithTimeout(f.ctx, 10*time.Second)
	defer cancel()
	for ctx.Err() == nil {
		if _, err := f.w.Step(ctx); err != nil {
			t.Fatal(err)
		}
		var err error
		current, err = f.n.GetVPC(ctx, f.tenant, id)
		if err == nil && baseFixtureProductsSettled(current, expected) {
			return current
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("fresh production runtime did not reach %s: %+v", expected, current)
	return current
}

func waitBaseHeldRequest(t *testing.T, fault *baseProcessFault) {
	t.Helper()
	fault.unblock()
	select {
	case <-fault.executed:
	case <-time.After(3 * time.Second):
		t.Fatal("detached Provider request did not complete")
	}
}

func TestBaseConnectivityFreshRuntimeRecoversPerResourceUnknownCreate(t *testing.T) {
	for _, kind := range []string{"vpcs", "eips", "snats"} {
		t.Run(kind+"_POST_success_response_lost", func(t *testing.T) {
			fault := newBaseProcessFault(http.MethodPost, kind, false)
			runtime, api := newBaseRecoveryFixture(t, fault)
			f := runtime.f
			fault.armed.Store(true)
			key := fmt.Sprintf("restart-create-%s", kind)
			accepted := createBaseVPC(t, f, key)
			eip, snat := baseIDs(t, f, accepted.ID)
			runBaseWorkerToFault(t, f, fault)
			if baseProcessClaimCount(t, f, eip) != 1 {
				t.Fatal("unknown create released the accepted EIP claim")
			}
			target := map[string]string{"vpcs": accepted.ID, "eips": eip, "snats": snat}[kind]
			var dispatched bool
			var pending string
			if err := f.owner.QueryRow(f.ctx, `SELECT create_dispatched,pending_action FROM network_provider_bindings WHERE tenant_id=$1 AND coalesce(vpc_id,subnet_id,eip_id,snat_id)=$2`, f.tenant, target).Scan(&dispatched, &pending); err != nil || !dispatched || pending != "create" {
				t.Fatal("unknown create was not durably fenced", dispatched, pending, err)
			}
			prior := map[string]string{}
			for _, item := range []struct{ id, kind string }{{accepted.ID, "vpcs"}, {eip, "eips"}, {snat, "snats"}} {
				prior[item.kind] = baseProcessProviderIdentity(t, f, api, item.id, item.kind)
			}
			runtime.close(t)
			waitBaseHeldRequest(t, fault)
			runtime.reopen(t)
			current := waitBaseRecoveryState(t, f, accepted.ID, biz.Available)
			if current.LastOperationID != accepted.LastOperationID {
				t.Fatal("new runtime changed the parent creation operation")
			}
			requireBaseProcessFixedIdentities(t, f, api, accepted.ID, eip, snat, prior)
			for _, resource := range []string{"vpcs", "eips", "snats"} {
				requireBaseProcessRequestCount(t, api, fault, http.MethodPost, resource, 1)
			}
			operations := baseProcessCreationOperations(t, f, accepted.ID, eip, snat)
			history := baseProcessHistory(t, f, operations)
			replay := createBaseVPC(t, f, key)
			if replay.ID != accepted.ID || replay.LastOperationID != accepted.LastOperationID || replay.State != biz.Provisioning || !replay.CreatedAt.Equal(accepted.CreatedAt) {
				t.Fatal("new runtime changed the immutable creation receipt", replay)
			}
			deleting, err := f.n.DeleteVPC(f.ctx, f.tenant, accepted.ID)
			if err != nil {
				t.Fatal(err)
			}
			deleted := waitBaseRecoveryState(t, f, accepted.ID, biz.Deleted)
			if deleted.LastOperationID != deleting.LastOperationID {
				t.Fatal("cleanup operation identity drift")
			}
			requireBaseProcessCleanup(t, f, api, accepted.ID, eip, snat)
			if after := baseProcessHistory(t, f, operations); after != history {
				t.Fatal("cleanup rewrote successful parent/child creation operations")
			}
			for _, resource := range []string{"vpcs", "eips", "snats"} {
				requireBaseProcessRequestCount(t, api, fault, http.MethodDelete, resource, 1)
			}
		})
	}
}

func TestBaseConnectivityFreshRuntimeRecoversLateDeleteResponseLoss(t *testing.T) {
	for _, kind := range []string{"eips", "vpcs"} {
		t.Run(kind+"_DELETE_success_response_lost", func(t *testing.T) {
			fault := newBaseProcessFault(http.MethodDelete, kind, false)
			runtime, api := newBaseRecoveryFixture(t, fault)
			f := runtime.f
			key := "restart-delete-" + kind
			accepted := createBaseVPC(t, f, key)
			eip, snat := baseIDs(t, f, accepted.ID)
			waitBaseRecoveryState(t, f, accepted.ID, biz.Available)
			requireBaseProcessFixedIdentities(t, f, api, accepted.ID, eip, snat, nil)
			for _, resource := range []string{"vpcs", "eips", "snats"} {
				requireBaseProcessRequestCount(t, api, fault, http.MethodPost, resource, 1)
			}
			operations := baseProcessCreationOperations(t, f, accepted.ID, eip, snat)
			history := baseProcessHistory(t, f, operations)
			fault.armed.Store(true)
			deleting, err := f.n.DeleteVPC(f.ctx, f.tenant, accepted.ID)
			if err != nil {
				t.Fatal(err)
			}
			runBaseWorkerToFault(t, f, fault)
			if active := baseProcessClaimCount(t, f, eip); active != 0 {
				t.Fatal("confirmed SNAT release retained its claim into later cleanup", kind, active)
			}
			target := map[string]string{"eips": eip, "vpcs": accepted.ID}[kind]
			var action, uid string
			if err := f.owner.QueryRow(f.ctx, `SELECT pending_action,provider_uid FROM network_provider_bindings WHERE tenant_id=$1 AND coalesce(vpc_id,subnet_id,eip_id,snat_id)=$2`, f.tenant, target).Scan(&action, &uid); err != nil || action != "delete" || uid == "" {
				t.Fatal("unknown delete lost durable identity/pending evidence", action, uid, err)
			}
			if after := baseProcessHistory(t, f, operations); after != history {
				t.Fatal("delete admission rewrote successful creation operations")
			}
			runtime.close(t)
			waitBaseHeldRequest(t, fault)
			runtime.reopen(t)
			current := waitBaseRecoveryState(t, f, accepted.ID, biz.Deleted)
			if current.LastOperationID != deleting.LastOperationID {
				t.Fatal("fresh runtime replaced deletion operation")
			}
			requireBaseProcessCleanup(t, f, api, accepted.ID, eip, snat)
			if afterEIP, afterSNAT := baseIDs(t, f, accepted.ID); afterEIP != eip || afterSNAT != snat {
				t.Fatal("delete recovery changed fixed child identities")
			}
			var persistedUID, pending string
			if err := f.owner.QueryRow(f.ctx, `SELECT provider_uid,pending_action FROM network_provider_bindings WHERE tenant_id=$1 AND coalesce(vpc_id,subnet_id,eip_id,snat_id)=$2`, f.tenant, target).Scan(&persistedUID, &pending); err != nil || persistedUID != uid || pending != "" {
				t.Fatal("confirmed delete lost its exact durable identity", persistedUID, uid, pending, err)
			}
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
			replay := createBaseVPC(t, f, key)
			if replay.ID != accepted.ID || replay.LastOperationID != accepted.LastOperationID || replay.State != biz.Provisioning {
				t.Fatal("delete recovery changed immutable creation receipt", replay)
			}
		})
	}
}
