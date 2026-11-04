package data_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network"
	"k8s.io/client-go/tools/clientcmd"
)

func TestBaseAdmissionRequiresDefaultWithoutLeavingAnAcceptedIntent(t *testing.T) {
	f := newEgressFixture(t)
	if _, err := f.p.SetNewVPCBaseConnectivity(f.ctx, true); err != nil {
		t.Fatal(err)
	}
	if _, err := f.n.CreateVPC(f.ctx, biz.CreateVPC{TenantID: f.tenant, Name: "no-default", CIDR: "10.42.0.0/16", IdempotencyKey: "no-default"}); biz.ReasonOf(err) != biz.BaseConnectivityNotReady {
		t.Fatal("missing default did not fail closed", err)
	}
	var n int
	if err := f.owner.QueryRow(f.ctx, `SELECT count(*) FROM network_vpcs WHERE tenant_id=$1`, f.tenant).Scan(&n); err != nil || n != 0 {
		t.Fatal("failed admission left parent intent", n, err)
	}
}

func TestBaseConcurrentWorkersDoNotAllocateTwiceOrLoseDeletionFences(t *testing.T) {
	f, api, _, kube := newBaseKCFixture(t, nil)
	config, err := clientcmd.BuildConfigFromFlags("", kube)
	if err != nil {
		t.Fatal(err)
	}
	config.QPS, config.Burst = 1000, 1000
	provider, err := data.NewKCProvider(f.p, config)
	if err != nil {
		t.Fatal(err)
	}
	policy := biz.DefaultWorkerPolicy()
	policy.ObserveEvery = 100 * time.Millisecond
	policy.RetryMin = 5 * time.Millisecond
	policy.RetryMax = 20 * time.Millisecond
	second, err := biz.NewWorker(f.p, provider, uuid.NewString(), policy)
	if err != nil {
		t.Fatal(err)
	}
	v := createBaseVPC(t, f, "concurrent-workers")
	ctx, cancel := context.WithCancel(f.ctx)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, worker := range []*biz.Worker{f.w, second} {
		wg.Add(1)
		go func(w *biz.Worker) {
			defer wg.Done()
			for ctx.Err() == nil {
				if _, err := w.Step(ctx); err != nil && ctx.Err() == nil {
					errs <- err
					return
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(3 * time.Millisecond):
				}
			}
		}(worker)
	}
	defer func() { cancel(); wg.Wait() }()
	awaitNET05A(t, 8*time.Second, func() bool {
		value, err := f.n.GetVPC(f.ctx, f.tenant, v.ID)
		return err == nil && baseFixtureProductsSettled(value, biz.Available)
	})
	e, s := baseIDs(t, f, v.ID)
	var eipOp string
	if err := f.owner.QueryRow(f.ctx, `SELECT last_operation_id FROM network_eips WHERE tenant_id=$1 AND eip_id=$2`, f.tenant, e).Scan(&eipOp); err != nil {
		t.Fatal(err)
	}
	if _, err := f.n.GetOperation(f.ctx, f.tenant, eipOp); biz.ReasonOf(err) != biz.ResourceNotFound {
		t.Fatal("system child operation escaped by guessed ID", err)
	}
	if _, err := f.n.DeleteVPC(f.ctx, f.tenant, v.ID); err != nil {
		t.Fatal(err)
	}
	awaitNET05A(t, 8*time.Second, func() bool {
		value, err := f.n.GetVPC(f.ctx, f.tenant, v.ID)
		return err == nil && baseFixtureProductsSettled(value, biz.Deleted)
	})
	cancel()
	wg.Wait()
	select {
	case err := <-errs:
		t.Fatal("worker lost durable progress", err)
	default:
	}
	api.Backend.Mu.Lock()
	ec, sc := api.Backend.Creates["eips"], api.Backend.Creates["snats"]
	api.Backend.Mu.Unlock()
	if ec != 1 || sc != 1 {
		t.Fatal("concurrent workers duplicated allocation", ec, sc)
	}
	requireBaseProcessCleanup(t, f, api, v.ID, e, s)
}

func TestBaseBackfillRechecksAllocationSettingAfterPlatformLockWait(t *testing.T) {
	f, _ := newBackfillFixture(t)
	availableVPC(t, f.n, f.w, f.tenant, "locked-admission")
	plan := planBackfill(t, f)
	resumeBackfill(t, f, plan)
	blocker, err := f.owner.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(f.ctx)
	var blockerPID int
	if err = blocker.QueryRow(f.ctx, `SELECT pg_backend_pid() FROM pg_advisory_xact_lock(hashtextextended('platform-cluster:test-cluster',0))`).Scan(&blockerPID); err != nil {
		t.Fatal(err)
	}
	type answer struct {
		result data.BaseBackfillAdmission
		err    error
	}
	answers := make(chan answer, 1)
	go func() { result, err := f.p.AdmitNextBaseBackfill(f.ctx, plan.RunID); answers <- answer{result, err} }()
	awaitNET05A(t, 5*time.Second, func() bool {
		var waiting bool
		err := f.owner.QueryRow(f.ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE locktype='advisory' AND NOT granted AND database=(SELECT oid FROM pg_database WHERE datname=current_database()) AND $1=ANY(pg_blocking_pids(pid)))`, blockerPID).Scan(&waiting)
		return err == nil && waiting
	})
	if _, err = blocker.Exec(f.ctx, `UPDATE network_public_pools SET allocation_enabled=false WHERE resource_id=$1`, f.pool.ID); err != nil {
		t.Fatal(err)
	}
	if err = blocker.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-answers:
		if got.err != nil || got.result.State != "blocked" {
			t.Fatal("closed allocation admitted after lock wait", got.result, got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("dispatcher failed to resume after lock release")
	}
	var count int
	if err = f.owner.QueryRow(f.ctx, `SELECT count(*) FROM network_operations WHERE kind='ensure_vpc_base_connectivity'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("closed allocation left an operation", count, err)
	}
	saved, err := f.p.GetBaseBackfill(f.ctx, plan.RunID)
	if err != nil || !saved.Paused || saved.Candidates[0].State != "pending" {
		t.Fatal("blocked fixed plan was not paused intact", saved, err)
	}
}
