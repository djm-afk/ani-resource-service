package data_test

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	data "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network"
	controlled "github.com/zhangzhe-ctrl/ani-resource-service/tests/net05a/provider"
	"k8s.io/client-go/rest"
)

func awaitNET05A(t *testing.T, timeout time.Duration, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition did not converge")
}

func TestObjectWatchQueuesDirectNativeStatusReads(t *testing.T) {
	p, _ := database(t)
	ctx := context.Background()
	api := controlled.New()
	host := httptest.NewServer(api)
	defer host.Close()
	provider, err := data.NewKCProvider(p, &rest.Config{Host: host.URL, QPS: 1000, Burst: 1000})
	if err != nil {
		t.Fatal(err)
	}
	policy := biz.DefaultWorkerPolicy()
	policy.ObserveEvery = 20 * time.Millisecond
	worker, err := biz.NewWorker(p, provider, uuid.NewString(), policy)
	if err != nil {
		t.Fatal(err)
	}
	observer, err := provider.EnableObservation(policy.RequestTimeout)
	if err != nil {
		t.Fatal(err)
	}
	live, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- observer.Start(live) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	n := newNetwork(t, p, time.Minute)
	tenant := uuid.NewString()
	namespace := "tenant-" + tenant
	api.Change("namespaces", map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": namespace, "uid": uuid.NewString(), "labels": map[string]any{"network.ani.io/managed-by": "ani-network-service", "network.ani.io/tenant-id": tenant}}, "status": map[string]any{"phase": "Active"}}, false)
	v, err := n.CreateVPC(ctx, biz.CreateVPC{TenantID: tenant, Name: "watch", CIDR: "10.42.0.0/16", IdempotencyKey: "watch"})
	if err != nil {
		t.Fatal(err)
	}
	waitState := func(expected biz.ResourceState) {
		t.Helper()
		awaitNET05A(t, 3*time.Second, func() bool {
			if _, err := worker.Step(ctx); err != nil {
				t.Fatal(err)
			}
			v, err = n.GetVPC(ctx, tenant, v.ID)
			return err == nil && v.State == expected
		})
	}
	waitState(biz.Available)
	name := "vpc-" + v.ID[4:]
	changeReady := func(value string) {
		object := api.Object("vpcs", namespace, name)
		for _, raw := range object["status"].(map[string]any)["conditions"].([]any) {
			condition := raw.(map[string]any)
			if condition["type"] == "Ready" {
				condition["status"] = value
			}
		}
		api.Change("vpcs", object, false)
	}
	changeReady("False")
	waitState(biz.Degraded)
	changeReady("True")
	waitState(biz.Available)
	// Events only enqueue. Without a worker they cannot renew the projection.
	before := v
	changeReady("False")
	awaitNET05A(t, 3*time.Second, func() bool { return observer.Snapshot()["notifications_total"] > 0 })
	v, err = n.GetVPC(ctx, tenant, v.ID)
	if err != nil || v.State != before.State || v.Version != before.Version || !v.ObservedAt.Equal(*before.ObservedAt) || v.ObservationStale {
		t.Fatal("event changed an unapplied resource projection", v, err)
	}
	waitState(biz.Degraded)
}
