package data_test

import (
	"context"
	"fmt"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network"
	controlled "github.com/zhangzhe-ctrl/ani-resource-service/tests/net05a/provider"
	"github.com/zhangzhe-ctrl/ani-resource-service/tests/testenv"
	"k8s.io/client-go/rest"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func awaitNET05A(t *testing.T, timeout time.Duration, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("NET05A condition did not converge")
}
func TestNET05AWatchStatusReconnectAndUnappliedEvidence(t *testing.T) {
	p, _ := database(t)
	ctx := context.Background()
	api := controlled.New()
	host := httptest.NewServer(api)
	defer host.Close()
	provider, e := data.NewKCProvider(p, &rest.Config{Host: host.URL, QPS: 1000, Burst: 1000})
	if e != nil {
		t.Fatal(e)
	}
	policy := biz.DefaultWorkerPolicy()
	policy.ObserveEvery = 20 * time.Millisecond
	policy.StaleAfter = 150 * time.Millisecond
	worker, e := biz.NewWorker(p, provider, uuid.NewString(), policy)
	if e != nil {
		t.Fatal(e)
	}
	options := data.DefaultObservationOptions()
	options.AuditInterval = 60 * time.Millisecond
	options.AuditJitter = time.Millisecond
	options.FlushInterval = 10 * time.Millisecond
	observer, e := provider.EnableObservation(options)
	if e != nil {
		t.Fatal(e)
	}
	live, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- observer.Start(live) }()
	defer func() {
		cancel()
		if e := <-done; e != nil {
			t.Error(e)
		}
	}()
	awaitNET05A(t, 3*time.Second, func() bool { return observer.Snapshot()["source_synced"] == 1 })
	n := newNetwork(t, p, policy.StaleAfter)
	tenant := uuid.NewString()
	api.Change("namespaces", map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "tenant-" + tenant, "uid": uuid.NewString(), "labels": map[string]any{"network.ani.io/managed-by": "ani-network-service", "network.ani.io/tenant-id": tenant}}, "status": map[string]any{"phase": "Active"}}, false)
	v, e := n.CreateVPC(ctx, biz.CreateVPC{TenantID: tenant, Name: "watch", CIDR: "10.42.0.0/16", IdempotencyKey: "watch"})
	if e != nil {
		t.Fatal(e)
	}
	awaitNET05A(t, 3*time.Second, func() bool {
		if _, e := worker.Step(ctx); e != nil {
			t.Fatal(e)
		}
		v, e = n.GetVPC(ctx, tenant, v.ID)
		return e == nil && v.State == biz.Available
	})
	// Let a completed audit include the new durable binding before status-only events.
	time.Sleep(100 * time.Millisecond)
	ns := "tenant-" + tenant
	name := "vpc-" + v.ID[4:]
	object := api.Object("vpcs", ns, name)
	status := object["status"].(map[string]any)
	conditions := status["conditions"].([]any)
	conditions[2].(map[string]any)["status"] = "False"
	api.Change("vpcs", object, false)
	awaitNET05A(t, 3*time.Second, func() bool {
		if _, e := worker.Step(ctx); e != nil {
			t.Fatal(e)
		}
		v, e = n.GetVPC(ctx, tenant, v.ID)
		return e == nil && v.State == biz.Degraded
	})
	object = api.Object("vpcs", ns, name)
	object["status"].(map[string]any)["conditions"].([]any)[2].(map[string]any)["status"] = "True"
	api.Change("vpcs", object, false)
	awaitNET05A(t, 3*time.Second, func() bool {
		if _, e := worker.Step(ctx); e != nil {
			t.Fatal(e)
		}
		v, e = n.GetVPC(ctx, tenant, v.ID)
		return e == nil && v.State == biz.Available
	})
	before := v.ObservedAt
	version := v.Version
	// Watch and real audits continue, but no worker applies them to PostgreSQL.
	time.Sleep(200 * time.Millisecond)
	v, e = n.GetVPC(ctx, tenant, v.ID)
	if e != nil || !v.ObservationStale || v.Version != version || !v.ObservedAt.Equal(*before) {
		t.Fatalf("Watch renewed unapplied facts: %+v %v", v, e)
	}
	if _, e = n.CreateSubnet(ctx, biz.CreateSubnet{TenantID: tenant, VPCID: v.ID, Name: "stale", CIDR: "10.42.1.0/24", IdempotencyKey: "stale"}); biz.ReasonOf(e) != biz.ParentNotReady {
		t.Fatalf("stale admission accepted: %v", e)
	}
	for _, code := range []int{0, 410} {
		api.Disconnect(code)
		awaitNET05A(t, 8*time.Second, func() bool { r, _ := api.Counts(); return r["WATCH/vpcs"] >= int64(2+code/410) })
	}
}
func TestNET05AInitialForbiddenThrottledWatchDoesNotInventFreshness(t *testing.T) {
	for _, code := range []int{403, 429} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			p, _ := database(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			api := controlled.New()
			api.Fail("LIST", "vpcs", code, code)
			api.Fail("WATCH", "vpcs", code)
			host := httptest.NewServer(api)
			defer host.Close()
			provider, e := data.NewKCProvider(p, &rest.Config{Host: host.URL, QPS: 1000, Burst: 1000})
			if e != nil {
				t.Fatal(e)
			}
			options := data.DefaultObservationOptions()
			options.AuditInterval = 50 * time.Millisecond
			observer, e := provider.EnableObservation(options)
			if e != nil {
				t.Fatal(e)
			}
			done := make(chan error, 1)
			go func() { done <- observer.Start(ctx) }()
			defer func() { cancel(); <-done }()
			awaitNET05A(t, 8*time.Second, func() bool { return observer.Snapshot()["source_synced"] == 1 })
			awaitNET05A(t, 3*time.Second, func() bool { r, _ := api.Counts(); return r["LIST/vpcs"] >= 3 })
		})
	}
}

func TestNET05AAttachmentUsesSharedIndexedAuditAndRetainsOrphan(t *testing.T) {
	f := newAttachmentFixture(t)
	ctx := context.Background()
	a := f.prepare(t)
	ctl := controlled.New()
	ctl.Backend = f.api
	pod := attachmentPod(a, "owned-pod", uuid.NewString())
	uid := pod["metadata"].(map[string]any)["uid"].(string)
	ctl.Change("pods", pod, false)
	nicUID := uuid.NewString()
	nic := map[string]any{"apiVersion": "networking.kubercloud.com/v1", "kind": "VNic", "metadata": map[string]any{"namespace": a.Namespace, "name": "nic", "uid": nicUID, "ownerReferences": []any{map[string]any{"kind": "Pod", "apiVersion": "v1", "uid": uid, "name": "owned-pod"}}}, "spec": map[string]any{"type": "VETH", "subnet": a.Plan.PrimaryNetworkRef}}
	ip := map[string]any{"apiVersion": "networking.kubercloud.com/v1", "kind": "VNicIP", "metadata": map[string]any{"namespace": a.Namespace, "name": "ip", "uid": uuid.NewString(), "ownerReferences": []any{map[string]any{"kind": "VNic", "apiVersion": "networking.kubercloud.com/v1", "uid": nicUID, "name": "nic"}}}, "spec": map[string]any{"vNic": "nic", "subnet": a.Plan.PrimaryNetworkRef}}
	ctl.Change("vnics", nic, false)
	ctl.Change("vnicips", ip, false)
	host := httptest.NewServer(ctl)
	defer host.Close()
	provider, e := data.NewKCProvider(f.p, &rest.Config{Host: host.URL, QPS: 1000, Burst: 1000})
	if e != nil {
		t.Fatal(e)
	}
	options := data.DefaultObservationOptions()
	options.AuditInterval = time.Hour
	options.AuditJitter = time.Millisecond
	options.FlushInterval = 10 * time.Millisecond
	observer, e := provider.EnableObservation(options)
	if e != nil {
		t.Fatal(e)
	}
	live, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- observer.Start(live) }()
	defer func() { cancel(); <-done }()
	awaitNET05A(t, 3*time.Second, func() bool { return observer.Snapshot()["source_synced"] == 1 })
	time.Sleep(50 * time.Millisecond)
	f.consumer.mu.Lock()
	f.consumer.err = nil
	f.consumer.value = consumerFor(a, "open", "")
	f.consumer.value.PodUIDs = []string{uid}
	f.consumer.mu.Unlock()
	policy := biz.DefaultWorkerPolicy()
	policy.ObserveEvery = 5 * time.Millisecond
	f.worker, e = biz.NewAttachmentWorker(f.p, provider, f.consumer, uuid.NewString(), policy)
	if e != nil {
		t.Fatal(e)
	}
	awaitNET05A(t, 3*time.Second, func() bool { a = f.step(t, a.ID); return a.State == biz.Attached })
	before, _ := ctl.Counts()
	for i := 0; i < 20; i++ {
		a = f.step(t, a.ID)
		if a.State != biz.Attached {
			t.Fatal(a)
		}
	}
	after, _ := ctl.Counts()
	for _, gvr := range []string{"pods", "vnics", "vnicips", "eips"} {
		if after["LIST/"+gvr] != before["LIST/"+gvr] {
			t.Fatalf("per-attachment full scan remains: %s %d -> %d", gvr, before["LIST/"+gvr], after["LIST/"+gvr])
		}
	}
	eip := map[string]any{"apiVersion": "networking.kubercloud.com/v1", "kind": "EIP", "metadata": map[string]any{"namespace": "external-owner", "name": "unlabelled-eip", "uid": uuid.NewString()}, "spec": map[string]any{"subnet": a.Plan.PrimaryNetworkRef}}
	ctl.Change("eips", eip, false)
	finalization := uuid.NewString()
	f.consumer.mu.Lock()
	f.consumer.value = consumerFor(a, "closed", finalization)
	now := time.Now()
	f.consumer.value.ClosedAt = &now
	f.consumer.value.PodUIDs = []string{uid}
	f.consumer.mu.Unlock()
	ctl.Change("pods", pod, true)
	ctl.Change("vnics", nic, true)
	awaitNET05A(t, 3*time.Second, func() bool { a = f.step(t, a.ID); return a.State == biz.Releasing && a.Reason == biz.CleanupPending })
	ctl.Change("vnicips", ip, true)
	for i := 0; i < 5; i++ {
		a = f.step(t, a.ID)
		if a.State != biz.Releasing || a.Reason != biz.CleanupPending {
			t.Fatalf("unlabelled EIP hidden from release: %+v", a)
		}
	}
	ctl.Change("eips", eip, true)
	awaitNET05A(t, 3*time.Second, func() bool { a = f.step(t, a.ID); return a.State == biz.Released })
	if a.Plan != nil {
		t.Fatal("released plan revived")
	}
}

type net05aConsumerFunc func(context.Context, biz.Attachment) (biz.ConsumerSubmission, error)

func (f net05aConsumerFunc) GetSubmission(ctx context.Context, a biz.Attachment) (biz.ConsumerSubmission, error) {
	return f(ctx, a)
}

type net05aAttachmentProviderFunc func(context.Context, biz.AttachmentWork) (biz.AttachmentObservation, error)

func (f net05aAttachmentProviderFunc) ObserveAttachment(ctx context.Context, w biz.AttachmentWork) (biz.AttachmentObservation, error) {
	return f(ctx, w)
}

func TestNET05AReleaseAuditMustStartAfterOwnerClosureResponse(t *testing.T) {
	for _, inflight := range []bool{false, true} {
		t.Run(strconv.FormatBool(inflight), func(t *testing.T) {
			f := newAttachmentFixture(t)
			a := f.prepare(t)
			ctl := controlled.New()
			ctl.Backend = f.api
			hold, captured := make(chan struct{}), make(chan struct{}, 1)
			var release sync.Once
			defer release.Do(func() { close(hold) })
			host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if inflight && r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/eips") {
					response := httptest.NewRecorder()
					ctl.ServeHTTP(response, r)
					select {
					case captured <- struct{}{}:
					default:
					}
					select {
					case <-hold:
					case <-r.Context().Done():
						return
					}
					for key, values := range response.Header() {
						w.Header()[key] = values
					}
					w.WriteHeader(response.Code)
					_, _ = w.Write(response.Body.Bytes())
					return
				}
				ctl.ServeHTTP(w, r)
			}))
			defer host.Close()
			provider, err := data.NewKCProvider(f.p, &rest.Config{Host: host.URL, QPS: 1000, Burst: 1000})
			if err != nil {
				t.Fatal(err)
			}
			// Leave the Watch delivery lifecycle stopped. Complete HTTP audits still
			// run, reproducing a replica whose Watch has not delivered the last writes.
			if _, err = provider.EnableObservation(data.DefaultObservationOptions()); err != nil {
				t.Fatal(err)
			}
			podUID, nicUID := uuid.NewString(), uuid.NewString()
			nic := map[string]any{"apiVersion": "networking.kubercloud.com/v1", "kind": "VNic", "metadata": map[string]any{"namespace": a.Namespace, "name": "last-nic", "uid": nicUID, "ownerReferences": []any{map[string]any{"kind": "Pod", "apiVersion": "v1", "uid": podUID, "name": "last-pod"}}}, "spec": map[string]any{"type": "VETH", "subnet": a.Plan.PrimaryNetworkRef}}
			ip := map[string]any{"apiVersion": "networking.kubercloud.com/v1", "kind": "VNicIP", "metadata": map[string]any{"namespace": a.Namespace, "name": "last-ip", "uid": uuid.NewString(), "ownerReferences": []any{map[string]any{"kind": "VNic", "apiVersion": "networking.kubercloud.com/v1", "uid": nicUID, "name": "last-nic"}}}, "spec": map[string]any{"vNic": "last-nic", "subnet": a.Plan.PrimaryNetworkRef}}
			auditDone := make(chan error, 1)
			first := true
			closed := consumerFor(a, "closed", uuid.NewString())
			closed.PodUIDs = []string{podUID}
			consumer := net05aConsumerFunc(func(ctx context.Context, _ biz.Attachment) (biz.ConsumerSubmission, error) {
				if first {
					first = false
					// This runs inside GetSubmission, after the real worker Claim. Capture
					// an empty shared view before the owner finishes its last submission.
					work := biz.AttachmentWork{Attachment: a, Plan: *a.Plan, Now: time.Now(), Relations: []byte("[]")}
					collect := func() error {
						// This audit has its own lifecycle; owner RPC completion must not cancel it.
						auditCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
						defer cancel()
						empty, e := provider.ObserveAttachment(auditCtx, work)
						if e == nil && empty.HasDependencies {
							return fmt.Errorf("pre-closure view is not empty")
						}
						return e
					}
					if inflight {
						go func() { auditDone <- collect() }()
						select {
						case <-captured:
						case <-ctx.Done():
							return biz.ConsumerSubmission{}, ctx.Err()
						}
					} else if e := collect(); e != nil {
						t.Fatal(e)
					}
					ctl.Change("vnics", nic, false)
					ctl.Change("vnicips", ip, false)
					// The owner has deleted its Pod and closed durably; derived network
					// relations still await cleanup by their infrastructure owner.
					now := time.Now()
					closed.ClosedAt = &now
				}
				return closed, nil
			})
			policy := biz.DefaultWorkerPolicy()
			policy.ObserveEvery = 5 * time.Millisecond
			policy.RetryMin = time.Millisecond
			checkedProvider := net05aAttachmentProviderFunc(func(ctx context.Context, work biz.AttachmentWork) (biz.AttachmentObservation, error) {
				// Entry here proves the owner's closed response has returned. Allow the old
				// complete collection to return only now; its start time remains too old.
				if inflight {
					release.Do(func() { close(hold) })
				}
				return provider.ObserveAttachment(ctx, work)
			})
			f.worker, err = biz.NewAttachmentWorker(f.p, checkedProvider, consumer, uuid.NewString(), policy)
			if err != nil {
				t.Fatal(err)
			}
			a = f.step(t, a.ID)
			if inflight {
				if e := <-auditDone; e != nil {
					t.Fatal(e)
				}
				if a.State == biz.Released {
					t.Fatalf("in-flight pre-closure audit released relations: %+v", a)
				}
				awaitNET05A(t, 3*time.Second, func() bool { a = f.step(t, a.ID); return a.State == biz.Releasing && a.Reason == biz.CleanupPending })
			}
			if a.State != biz.Releasing || a.Reason != biz.CleanupPending {
				t.Fatalf("pre-closure empty view released live relations: %+v", a)
			}
			ctl.Change("vnics", nic, true)
			a = f.step(t, a.ID)
			if a.State != biz.Releasing || a.Reason != biz.CleanupPending {
				t.Fatalf("orphan IP did not retain release fence: %+v", a)
			}
			ctl.Change("vnicips", ip, true)
			awaitNET05A(t, 3*time.Second, func() bool { a = f.step(t, a.ID); return a.State == biz.Released })
		})
	}
}

func TestNET05ACriticalAuditFollowsCollectionStartedBeforeItsBoundary(t *testing.T) {
	f := newAttachmentFixture(t)
	a := f.prepare(t)
	ctl := controlled.New()
	ctl.Backend = f.api
	hold, captured := make(chan struct{}), make(chan struct{})
	var first, release sync.Once
	defer release.Do(func() { close(hold) })
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pause := false
		if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/eips") {
			first.Do(func() { pause = true })
		}
		if !pause {
			ctl.ServeHTTP(w, r)
			return
		}
		response := httptest.NewRecorder()
		ctl.ServeHTTP(response, r)
		close(captured)
		select {
		case <-hold:
		case <-r.Context().Done():
			return
		}
		for key, values := range response.Header() {
			w.Header()[key] = values
		}
		w.WriteHeader(response.Code)
		_, _ = w.Write(response.Body.Bytes())
	}))
	defer host.Close()
	provider, err := data.NewKCProvider(f.p, &rest.Config{Host: host.URL, QPS: 1000, Burst: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = provider.EnableObservation(data.DefaultObservationOptions()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	work := biz.AttachmentWork{Attachment: a, Plan: *a.Plan, Now: time.Now(), Relations: []byte("[]")}
	done := make(chan error, 1)
	go func() { _, err := provider.ObserveAttachment(ctx, work); done <- err }()
	select {
	case <-captured:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// The held complete view predates both the new relation and the critical
	// caller's database-time boundary. Joining it must lead to a later complete
	// collection within this call, rather than permanent retry under contention.
	ctl.Change("eips", map[string]any{"apiVersion": "networking.kubercloud.com/v1", "kind": "EIP", "metadata": map[string]any{"name": "late-eip", "namespace": a.Namespace, "uid": uuid.NewString()}, "spec": map[string]any{"subnet": a.Plan.PrimaryNetworkRef}}, false)
	work.RequireFreshRelations = true
	go func() { time.Sleep(50 * time.Millisecond); release.Do(func() { close(hold) }) }()
	observed, err := provider.ObserveAttachment(ctx, work)
	if err != nil || !observed.HasDependencies {
		t.Fatalf("critical caller must collect after its boundary and retain late EIP: observation=%+v err=%v", observed, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestNET05AOldPaginatedListCannotOverwriteNewWatchFact(t *testing.T) {
	p, _ := database(t)
	ctx := context.Background()
	api := controlled.New()
	host := httptest.NewServer(api)
	defer host.Close()
	provider, e := data.NewKCProvider(p, &rest.Config{Host: host.URL, QPS: 1000, Burst: 1000})
	if e != nil {
		t.Fatal(e)
	}
	policy := biz.DefaultWorkerPolicy()
	policy.ObserveEvery = 10 * time.Millisecond
	worker, e := biz.NewWorker(p, provider, uuid.NewString(), policy)
	if e != nil {
		t.Fatal(e)
	}
	n := newNetwork(t, p, time.Minute)
	tenant := uuid.NewString()
	api.Change("namespaces", map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "tenant-" + tenant, "uid": uuid.NewString(), "labels": map[string]any{"network.ani.io/managed-by": "ani-network-service", "network.ani.io/tenant-id": tenant}}, "status": map[string]any{"phase": "Active"}}, false)
	v := availableVPC(t, n, worker, tenant, "old-list")
	opts := data.DefaultObservationOptions()
	opts.AuditInterval = 80 * time.Millisecond
	opts.AuditJitter = time.Millisecond
	opts.FlushInterval = 10 * time.Millisecond
	observer, e := provider.EnableObservation(opts)
	if e != nil {
		t.Fatal(e)
	}
	live, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- observer.Start(live) }()
	defer func() { cancel(); <-done }()
	awaitNET05A(t, 3*time.Second, func() bool { return observer.Snapshot()["source_synced"] == 1 })
	time.Sleep(120 * time.Millisecond)
	// Capture the old LIST, hold its response, then deliver a newer status-only
	// event. Finish must use a direct verified fact; old audit publication later
	// cannot erase it even though its resource lease/PG version is valid.
	hold, captured := make(chan struct{}), make(chan struct{}, 1)
	api.SetListHold(hold, captured)
	select {
	case <-captured:
	case <-time.After(3 * time.Second):
		t.Fatal("old LIST not captured")
	}
	ns, name := "tenant-"+tenant, "vpc-"+v.ID[4:]
	obj := api.Object("vpcs", ns, name)
	obj["status"].(map[string]any)["conditions"].([]any)[2].(map[string]any)["status"] = "False"
	api.Change("vpcs", obj, false)
	if e = p.NotifyResource(ctx, tenant, v.ID); e != nil {
		t.Fatal(e)
	}
	awaitNET05A(t, 3*time.Second, func() bool {
		if _, e = worker.Step(ctx); e != nil {
			t.Fatal(e)
		}
		v, e = n.GetVPC(ctx, tenant, v.ID)
		return e == nil && v.State == biz.Degraded
	})
	api.SetListHold(nil, nil)
	close(hold)
	for i := 0; i < 10; i++ {
		time.Sleep(15 * time.Millisecond)
		if _, e = worker.Step(ctx); e != nil {
			t.Fatal(e)
		}
		v, e = n.GetVPC(ctx, tenant, v.ID)
		if e != nil || v.State != biz.Degraded {
			t.Fatalf("old LIST overwrote Watch: %+v %v", v, e)
		}
	}
}

func TestNET05ASilentWatchAndReplicaWithOlderAuditCannotRegress(t *testing.T) {
	p, _ := database(t)
	ctx := context.Background()
	fresh, old := controlled.New(), controlled.New()
	old.Backend = fresh.Backend
	host1, host2 := httptest.NewServer(fresh), httptest.NewServer(old)
	defer host1.Close()
	defer host2.Close()
	makeWorker := func(host string) (*data.KCProvider, *biz.Worker) {
		provider, e := data.NewKCProvider(p, &rest.Config{Host: host, QPS: 1000, Burst: 1000})
		if e != nil {
			t.Fatal(e)
		}
		policy := biz.DefaultWorkerPolicy()
		policy.ObserveEvery = 15 * time.Millisecond
		w, e := biz.NewWorker(p, provider, uuid.NewString(), policy)
		if e != nil {
			t.Fatal(e)
		}
		return provider, w
	}
	stops := []func(){}
	defer func() {
		for _, stop := range stops {
			stop()
		}
	}()
	provider1, worker1 := makeWorker(host1.URL)
	provider2, worker2 := makeWorker(host2.URL)
	n := newNetwork(t, p, time.Minute)
	tenant := uuid.NewString()
	fresh.Change("namespaces", map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "tenant-" + tenant, "uid": uuid.NewString(), "labels": map[string]any{"network.ani.io/managed-by": "ani-network-service", "network.ani.io/tenant-id": tenant}}, "status": map[string]any{"phase": "Active"}}, false)
	v := availableVPC(t, n, worker1, tenant, "silent-replica")
	start := func(provider *data.KCProvider) *data.KCObservation {
		options := data.DefaultObservationOptions()
		options.AuditInterval = 80 * time.Millisecond
		options.AuditJitter = time.Millisecond
		options.FlushInterval = 10 * time.Millisecond
		o, e := provider.EnableObservation(options)
		if e != nil {
			t.Fatal(e)
		}
		live, cancel := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- o.Start(live) }()
		stops = append(stops, func() { cancel(); <-done })
		awaitNET05A(t, 3*time.Second, func() bool { return o.Snapshot()["source_synced"] == 1 })
		return o
	}
	observer1, observer2 := start(provider1), start(provider2)
	time.Sleep(120 * time.Millisecond)
	// The second source's stream stays open but receives no new events. Its
	// independent audit is held at an older collection, while another replica
	// commits the new fact. No arbitrary RV numeric comparison is involved.
	hold, captured := make(chan struct{}), make(chan struct{}, 1)
	old.SetListHold(hold, captured)
	defer func() { old.SetListHold(nil, nil); close(hold) }()
	select {
	case <-captured:
	case <-time.After(3 * time.Second):
		t.Fatal("old audit not captured")
	}
	obj := fresh.Object("vpcs", "tenant-"+tenant, "vpc-"+v.ID[4:])
	obj["status"].(map[string]any)["conditions"].([]any)[2].(map[string]any)["status"] = "False"
	fresh.Change("vpcs", obj, false)
	awaitNET05A(t, 3*time.Second, func() bool { runStep(t, worker1); v, _ = n.GetVPC(ctx, tenant, v.ID); return v.State == biz.Degraded })
	before, _ := old.Counts()
	if e := p.NotifyResource(ctx, tenant, v.ID); e != nil {
		t.Fatal(e)
	}
	runStep(t, worker2)
	after, _ := old.Counts()
	actual, e := n.GetVPC(ctx, tenant, v.ID)
	if e != nil || actual.State != biz.Degraded || after["GET/vpcs"] <= before["GET/vpcs"] {
		t.Fatalf("older replica did not directly verify: %+v %v", actual, e)
	}
	if observer2.Snapshot()["source_synced"] != 1 {
		t.Fatal("silent connection was not kept open")
	}
	// Drop Watch events on the first source too. Periodic complete LIST must
	// recover the next status without a notification or resync freshness trick.
	fresh.Blackhole(true)
	obj = fresh.Object("vpcs", "tenant-"+tenant, "vpc-"+v.ID[4:])
	obj["status"].(map[string]any)["conditions"].([]any)[2].(map[string]any)["status"] = "True"
	fresh.Change("vpcs", obj, false)
	awaitNET05A(t, 3*time.Second, func() bool {
		runStep(t, worker1)
		actual, _ = n.GetVPC(ctx, tenant, v.ID)
		return actual.State == biz.Available
	})
	if observer1.Snapshot()["source_synced"] != 1 {
		t.Fatal("silent Watch was restarted instead of audited")
	}
	// Same name, different UID is a conflict, never authorization to rebuild.
	obj = fresh.Object("vpcs", "tenant-"+tenant, "vpc-"+v.ID[4:])
	obj["metadata"].(map[string]any)["uid"] = uuid.NewString()
	fresh.Change("vpcs", obj, false)
	awaitNET05A(t, 3*time.Second, func() bool {
		runStep(t, worker1)
		actual, _ = n.GetVPC(ctx, tenant, v.ID)
		return actual.Reason == biz.ProviderOwnership
	})
}

func TestNET05APostgresOutageAndLostMemoryHintsRestartDurableIntents(t *testing.T) {
	fixture := testenv.NewDatabase(t)
	p, db := fixture.Repository, fixture.Owner
	configureRepositoryVPCPresets(t, p)
	ctx := context.Background()
	api := controlled.New()
	host := httptest.NewServer(api)
	defer host.Close()
	provider, e := data.NewKCProvider(p, &rest.Config{Host: host.URL, QPS: 1000, Burst: 1000})
	if e != nil {
		t.Fatal(e)
	}
	policy := biz.DefaultWorkerPolicy()
	policy.ObserveEvery = 20 * time.Millisecond
	policy.RetryMin = 20 * time.Millisecond
	policy.RetryMax = 40 * time.Millisecond
	worker, e := biz.NewWorker(p, provider, uuid.NewString(), policy)
	if e != nil {
		t.Fatal(e)
	}
	n := newNetwork(t, p, time.Minute)
	tenant := uuid.NewString()
	api.Change("namespaces", map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "tenant-" + tenant, "uid": uuid.NewString(), "labels": map[string]any{"network.ani.io/managed-by": "ani-network-service", "network.ani.io/tenant-id": tenant}}, "status": map[string]any{"phase": "Active"}}, false)
	v := availableVPC(t, n, worker, tenant, "before-outage")
	missing, e := n.CreateVPC(ctx, biz.CreateVPC{TenantID: tenant, Name: "no-cr-yet", CIDR: "10.0.0.0/16", IdempotencyKey: "no-cr-yet"})
	if e != nil {
		t.Fatal(e)
	}
	opts := data.DefaultObservationOptions()
	opts.AuditInterval = 70 * time.Millisecond
	opts.AuditJitter = time.Millisecond
	opts.FlushInterval = 10 * time.Millisecond
	observer, e := provider.EnableObservation(opts)
	if e != nil {
		t.Fatal(e)
	}
	live, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- observer.Start(live) }()
	stopped := false
	defer func() {
		cancel()
		if !stopped {
			<-done
		}
	}()
	awaitNET05A(t, 3*time.Second, func() bool { return observer.Snapshot()["source_synced"] == 1 })
	admin, e := pgxpool.New(ctx, os.Getenv("NETWORK_TEST_ADMIN_DSN"))
	if e != nil {
		t.Fatal(e)
	}
	defer admin.Close()
	role := pgx.Identifier{fixture.RuntimeRole}.Sanitize()
	if _, e = admin.Exec(ctx, "ALTER ROLE "+role+" NOLOGIN"); e != nil {
		t.Fatal(e)
	}
	restored := false
	defer func() {
		if !restored {
			_, _ = admin.Exec(ctx, "ALTER ROLE "+role+" LOGIN")
		}
	}()
	if _, e = admin.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE usename=$1`, fixture.RuntimeRole); e != nil {
		t.Fatal(e)
	}
	var before int64
	if e = db.QueryRow(ctx, `SELECT requested_generation FROM network_reconciliations WHERE tenant_id=$1 AND vpc_id=$2`, tenant, v.ID).Scan(&before); e != nil {
		t.Fatal(e)
	}
	obj := api.Object("vpcs", "tenant-"+tenant, "vpc-"+v.ID[4:])
	obj["status"].(map[string]any)["conditions"].([]any)[2].(map[string]any)["status"] = "False"
	api.Change("vpcs", obj, false)
	time.Sleep(180 * time.Millisecond)
	if e = p.NotifyResource(ctx, tenant, v.ID); biz.ReasonOf(e) != biz.DependencyUnavailable {
		t.Fatalf("outage did not reject persistence: %v", e)
	}
	var after int64
	if e = db.QueryRow(ctx, `SELECT requested_generation FROM network_reconciliations WHERE tenant_id=$1 AND vpc_id=$2`, tenant, v.ID).Scan(&after); e != nil || after != before {
		t.Fatalf("failed bridge invented durable notification %d %d %v", before, after, e)
	}
	// Stop and discard the entire old observer, including its in-memory retry.
	cancel()
	<-done
	stopped = true
	p.Close()
	if _, e = admin.Exec(ctx, "ALTER ROLE "+role+" LOGIN"); e != nil {
		t.Fatal(e)
	}
	restored = true
	replacement, e := data.OpenPostgres(ctx, fixture.RuntimeDSN, data.Placement{ClusterID: "test-cluster", NamespacePrefix: "tenant-"})
	if e != nil {
		t.Fatal(e)
	}
	defer replacement.Close()
	// Startup restores policy and external facts on each new repository;
	// neither lives in the discarded observer or the persistent database.
	configureRepositoryVPCPresets(t, replacement)
	provider, e = data.NewKCProvider(replacement, &rest.Config{Host: host.URL, QPS: 1000, Burst: 1000})
	if e != nil {
		t.Fatal(e)
	}
	observer, e = provider.EnableObservation(opts)
	if e != nil {
		t.Fatal(e)
	}
	restarted, stop := context.WithCancel(ctx)
	finished := make(chan error, 1)
	go func() { finished <- observer.Start(restarted) }()
	defer func() { stop(); <-finished }()
	worker, e = biz.NewWorker(replacement, provider, uuid.NewString(), policy)
	if e != nil {
		t.Fatal(e)
	}
	n = newNetwork(t, replacement, time.Minute)
	awaitNET05A(t, 5*time.Second, func() bool {
		runStep(t, worker)
		old, _ := n.GetVPC(ctx, tenant, v.ID)
		newValue, _ := n.GetVPC(ctx, tenant, missing.ID)
		return old.State == biz.Degraded && newValue.State == biz.Available
	})
	api.Backend.Mu.Lock()
	posts := api.Backend.Creates["vpcs"]
	api.Backend.Mu.Unlock()
	if posts != 2 {
		t.Fatalf("missing durable intent recreated %d times", posts)
	}
}
