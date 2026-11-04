package data_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network"
	"k8s.io/client-go/rest"
)

func newNetwork(t *testing.T, repository *data.Postgres, _ time.Duration) *biz.Network {
	t.Helper()
	n, err := biz.NewNetwork(repository, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	return n
}
func create(t *testing.T, n *biz.Network, name string) biz.VPC {
	t.Helper()
	v, err := n.CreateVPC(context.Background(), biz.CreateVPC{TenantID: "7a7750cf-73b0-49c5-a3b1-4dba42689401", Name: name, CIDR: "10.42.0.0/16", IdempotencyKey: name})
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func runStep(t *testing.T, w *biz.Worker) {
	t.Helper()
	if _, err := w.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func fastPolicy() biz.WorkerPolicy {
	p := biz.DefaultWorkerPolicy()
	p.RequestTimeout = 100 * time.Millisecond
	p.Lease = 350 * time.Millisecond
	p.ObserveEvery = 5 * time.Millisecond
	p.RetryMin = 5 * time.Millisecond
	p.RetryMax = 10 * time.Millisecond
	return p
}

func TestUnknownPOSTRetriesTheSameNameWhileOriginalUIDIsUnknown(t *testing.T) {
	repository, _ := database(t)
	network := newNetwork(t, repository, time.Minute)
	api := &controlledKC{}
	arrived := make(chan struct{})
	release := make(chan struct{})
	var posts atomic.Int32
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && len(r.URL.Path) > 5 && r.URL.Path[len(r.URL.Path)-5:] == "/vpcs" {
			if posts.Add(1) == 1 {
				close(arrived)
				<-release // The first response outlives the caller; later POSTs use the same name.
			}
		}
		api.ServeHTTP(w, r)
	}))
	defer host.Close()
	defer close(release)
	provider, err := data.NewKCProvider(repository, &rest.Config{Host: host.URL})
	if err != nil {
		t.Fatal(err)
	}
	policy := fastPolicy()
	worker, err := biz.NewWorker(repository, provider, uuid.NewString(), policy)
	if err != nil {
		t.Fatal(err)
	}
	accepted := create(t, network, "late")
	runStep(t, worker)
	select {
	case <-arrived:
	default:
		t.Fatal("no actual HTTP POST reached the server")
	}
	op, err := network.GetOperation(context.Background(), accepted.TenantID, accepted.LastOperationID)
	if err != nil || op.State != biz.Retrying || op.Reason != biz.ProviderUnknown {
		t.Fatalf("unknown POST lost: %+v %v", op, err)
	}
	if _, err := network.DeleteVPC(context.Background(), accepted.TenantID, accepted.ID); biz.ReasonOf(err) != biz.ResourceBusy {
		t.Fatalf("unknown create allowed deletion: %v", err)
	}
	// A new worker retries the same durable name after a current GET404.
	replacement, err := biz.NewWorker(repository, provider, uuid.NewString(), policy)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(15 * time.Millisecond)
	runStep(t, replacement)
	if posts.Load() != 2 {
		t.Fatal("unknown create did not retry its fixed provider name")
	}
	release <- struct{}{}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		runStep(t, replacement)
		v, err := network.GetVPC(context.Background(), accepted.TenantID, accepted.ID)
		if err != nil {
			t.Fatal(err)
		}
		if v.State == biz.Available {
			if v.LastOperationID != accepted.LastOperationID || posts.Load() != 2 {
				t.Fatal("late recovery changed durable identity")
			}
			t.Logf("late POST: %s op=%s retrying -> available; external POSTs=%d", v.ID, v.LastOperationID, posts.Load())
			return
		}
	}
	t.Fatal("late external success was never recovered")
}

func TestLeaseExpiryRejectsOldEpochWritesAndRecoversPendingIdentity(t *testing.T) {
	repository, _ := database(t)
	n := newNetwork(t, repository, time.Minute)
	v := create(t, n, "epoch")
	ctx := context.Background()
	first, found, err := repository.Claim(ctx, uuid.NewString(), 40*time.Millisecond)
	if err != nil || !found {
		t.Fatalf("first claim: %v %v", found, err)
	}
	if err := repository.BeginMutation(ctx, first, "create", ""); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)
	second, found, err := repository.Claim(ctx, uuid.NewString(), time.Second)
	if err != nil || !found || second.Epoch <= first.Epoch {
		t.Fatalf("expired claim not recovered: %+v %v", second, err)
	}
	if err := repository.Finish(ctx, first, biz.Progress{State: biz.Available, OperationState: biz.Succeeded, Identity: "old-uid", Observed: true, ClearPending: true, NextDelay: time.Second}); !errors.Is(err, biz.ErrLeaseLost) {
		t.Fatalf("old epoch wrote: %v", err)
	}
	if err := repository.BeginMutation(ctx, first, "create", ""); !errors.Is(err, biz.ErrLeaseLost) {
		t.Fatalf("old epoch started mutation: %v", err)
	}
	if second.PendingAction != "create" || second.Resource.ID != v.ID {
		t.Fatalf("pending request identity lost: %+v", second)
	}
	if err := repository.Finish(ctx, second, biz.Progress{State: biz.Provisioning, OperationState: biz.Blocked, Reason: biz.ProviderUnknown, NextDelay: time.Second}); err != nil {
		t.Fatal(err)
	}
	got, err := n.GetVPC(ctx, v.TenantID, v.ID)
	if err != nil || got.State != biz.Provisioning {
		t.Fatalf("stale result changed resource: %+v %v", got, err)
	}
}

func TestConcurrentAcceptanceAndWorkersUseTheSameDatabaseAuthority(t *testing.T) {
	repository, owner := database(t)
	n := newNetwork(t, repository, time.Minute)
	ctx := context.Background()
	input := biz.CreateVPC{TenantID: uuid.NewString(), Name: "concurrent", CIDR: "10.42.0.0/16", IdempotencyKey: "same"}
	const count = 12
	values := make(chan biz.VPC, count)
	failures := make(chan error, count)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range count {
		wg.Go(func() {
			<-start
			v, e := n.CreateVPC(ctx, input)
			if e != nil {
				failures <- e
			} else {
				values <- v
			}
		})
	}
	close(start)
	wg.Wait()
	close(failures)
	close(values)
	for err := range failures {
		t.Fatal(err)
	}
	var identity string
	for v := range values {
		if identity == "" {
			identity = v.ID
		}
		if v.ID != identity {
			t.Fatal("concurrent key was accepted more than once")
		}
	}
	var resources, operations, receipts, histories int
	if err := owner.QueryRow(ctx, `SELECT (SELECT count(*) FROM network_vpcs),(SELECT count(*) FROM network_operations),(SELECT count(*) FROM network_idempotency),(SELECT count(*) FROM network_resource_history)`).Scan(&resources, &operations, &receipts, &histories); err != nil {
		t.Fatal(err)
	}
	if resources != 1 || operations != 1 || receipts != 1 || histories != 1 {
		t.Fatalf("T1 duplicated records: %d %d %d %d", resources, operations, receipts, histories)
	}
	input.TenantID = uuid.NewString()
	other, err := n.CreateVPC(ctx, input)
	if err != nil || other.ID == identity {
		t.Fatalf("cross-tenant key reused: %+v %v", other, err)
	}
	provider := &memoryProvider{}
	workerrors := make(chan error, count)
	for range count {
		wg.Go(func() {
			w, e := biz.NewWorker(repository, provider, uuid.NewString(), biz.DefaultWorkerPolicy())
			if e == nil {
				_, e = w.Step(ctx)
			}
			if e != nil {
				workerrors <- e
			}
		})
	}
	wg.Wait()
	close(workerrors)
	for err := range workerrors {
		t.Fatal(err)
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.objects) != 2 {
		t.Fatalf("workers did not converge both tenants: %d", len(provider.objects))
	}
}

// Observation age does not change native resource state or trigger provider work.
func TestPureQueriesKeepProjectionAndSuccessfulOperationHistory(t *testing.T) {
	repository, owner := database(t)
	n := newNetwork(t, repository, 30*time.Millisecond)
	v := create(t, n, "stale")
	api := &controlledKC{}
	var requests atomic.Int32
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); api.ServeHTTP(w, r) }))
	defer host.Close()
	provider, err := data.NewKCProvider(repository, &rest.Config{Host: host.URL})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := biz.NewWorker(repository, provider, uuid.NewString(), fastPolicy())
	if err != nil {
		t.Fatal(err)
	}
	runStep(t, worker)
	before, err := n.GetVPC(context.Background(), v.TenantID, v.ID)
	if err != nil || before.State != biz.Available {
		t.Fatalf("initial readiness: %+v %v", before, err)
	}
	var history int
	ctx := context.Background()
	if err := owner.QueryRow(ctx, "SELECT count(*) FROM network_resource_history").Scan(&history); err != nil {
		t.Fatal(err)
	}
	count := requests.Load()
	time.Sleep(45 * time.Millisecond)
	for range 5 {
		got, err := n.GetVPC(ctx, v.TenantID, v.ID)
		if err != nil || got.ObservationStale || got.Version != before.Version || got.State != biz.Available {
			t.Fatalf("query changed an unchanged successful observation: %+v %v", got, err)
		}
		if _, err := n.ListVPCs(ctx, biz.ListVPCs{TenantID: v.TenantID}); err != nil {
			t.Fatal(err)
		}
		if _, err := n.GetOperation(ctx, v.TenantID, v.LastOperationID); err != nil {
			t.Fatal(err)
		}
	}
	var historyAfter int
	if err := owner.QueryRow(ctx, "SELECT count(*) FROM network_resource_history").Scan(&historyAfter); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != count || historyAfter != history {
		t.Fatal("query performed provider work or wrote history")
	}
	api.mu.Lock()
	api.object["status"] = map[string]any{}
	api.mu.Unlock()
	if err := repository.NotifyResource(ctx, v.TenantID, v.ID); err != nil {
		t.Fatal(err)
	}
	runStep(t, worker)
	got, err := n.GetVPC(ctx, v.TenantID, v.ID)
	if err != nil || got.State != biz.Available || got.Reason != biz.ProviderStatusUnreported {
		t.Fatalf("unreported native Ready did not preserve resource state: %+v %v", got, err)
	}
	op, err := n.GetOperation(ctx, v.TenantID, v.LastOperationID)
	if err != nil || op.State != biz.Succeeded {
		t.Fatalf("historic operation rewritten: %+v %v", op, err)
	}
	t.Log(fmt.Sprintf("query-only phase kept version %d and %d HTTP requests; worker then recorded an unreported native status", before.Version, count))
}
