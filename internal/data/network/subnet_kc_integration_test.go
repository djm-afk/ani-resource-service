package data_test

import (
	"context"
	"errors"
	networkv1 "github.com/zhangzhe-ctrl/ani-resource-service/api/network/v1"
	service "github.com/zhangzhe-ctrl/ani-resource-service/internal/service/network"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/tests/testenv"
	"k8s.io/client-go/rest"
)

func TestSubnetKCDeletionCompletesWhenOriginalObjectDisappears(t *testing.T) {
	p, _ := database(t)
	n := newNetwork(t, p, time.Minute)
	api := &testenv.KC{}
	host := httptest.NewServer(api)
	defer host.Close()
	provider, err := data.NewKCProvider(p, &rest.Config{Host: host.URL, QPS: 1000, Burst: 1000})
	if err != nil {
		t.Fatal(err)
	}
	policy := biz.DefaultWorkerPolicy()
	policy.ObserveEvery = 100 * time.Millisecond
	policy.RetryMin = time.Millisecond
	w, err := biz.NewWorker(p, provider, uuid.NewString(), policy)
	if err != nil {
		t.Fatal(err)
	}
	tenant := uuid.NewString()
	v := availableVPC(t, n, w, tenant, "parent")
	ctx := context.Background()
	accepted, err := n.CreateSubnet(ctx, biz.CreateSubnet{TenantID: tenant, VPCID: v.ID, Name: "http", CIDR: "10.42.1.0/24", IdempotencyKey: "http"})
	if err != nil {
		t.Fatal(err)
	}
	current := driveSubnet(t, n, w, accepted, biz.Available)
	if current.Gateway != "10.42.1.1" {
		t.Fatal("wrong gateway", current)
	}
	api.Mu.Lock()
	var object map[string]any
	var key string
	for k, o := range api.Objects {
		if o["kind"] == "Subnet" {
			key, object = k, o
		}
	}
	metadata := object["metadata"].(map[string]any)
	namespace := metadata["namespace"].(string)
	providerName := metadata["name"].(string)
	spec := object["spec"].(map[string]any)
	if spec["gatewayIP"] != "10.42.1.1" || spec["type"] != "VPC" {
		t.Fatal("incorrect serialized subnet", spec)
	}
	api.Mu.Unlock()
	if _, err := n.DeleteSubnet(ctx, tenant, current.ID); err != nil {
		t.Fatal(err)
	}
	// KCN owns its descendants. Res completes this deletion on the original
	// Subnet's absence, even while a provider-owned child remains.
	api.Mu.Lock()
	delete(api.Objects, key)
	api.Objects["vnicips/"+namespace+"/residual"] = map[string]any{"apiVersion": "networking.kubercloud.com/v1", "kind": "VNicIP", "metadata": map[string]any{"name": "residual", "namespace": namespace}, "spec": map[string]any{"subnet": providerName}}
	api.Mu.Unlock()
	driveSubnet(t, n, w, current, biz.Deleted)
	api.Mu.Lock()
	defer api.Mu.Unlock()
	if api.Objects["vnicips/"+namespace+"/residual"] == nil || api.Deletes["vnicips"] != 0 || api.Deletes["subnets"] != 0 || api.Invalid != "" {
		t.Fatal("Res operated on provider cleanup", api.Deletes, api.Invalid)
	}

}

func TestSubnetKCIPRangesDetailsAndFences(t *testing.T) {
	p, _ := database(t)
	n := newNetwork(t, p, time.Minute)
	api := &testenv.KC{}
	var unavailable atomic.Bool
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if unavailable.Load() && strings.Contains(r.URL.Path, "/subnets/") {
			w.WriteHeader(503)
			return
		}
		api.ServeHTTP(w, r)
	}))
	defer host.Close()
	provider, err := data.NewKCProvider(p, &rest.Config{Host: host.URL, QPS: 1000, Burst: 1000})
	if err != nil {
		t.Fatal(err)
	}
	policy := biz.DefaultWorkerPolicy()
	policy.ObserveEvery = time.Hour
	policy.RetryMin = time.Millisecond
	worker, err := biz.NewWorker(p, provider, uuid.NewString(), policy)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tenant := uuid.NewString()
	vpc := availableVPC(t, n, worker, tenant, "ranges-parent")
	accepted, err := n.CreateSubnet(ctx, biz.CreateSubnet{TenantID: tenant, VPCID: vpc.ID, Name: "ranges", CIDR: "10.42.1.0/24", IdempotencyKey: "ranges"})
	if err != nil {
		t.Fatal(err)
	}
	current := driveSubnet(t, n, worker, accepted, biz.Available)
	rpc := service.NewNetworkService(n)
	details := func() *networkv1.Subnet {
		t.Helper()
		reply, e := rpc.GetSubnet(ctx, &networkv1.GetSubnetRequest{TenantId: tenant, SubnetId: current.ID})
		if e != nil {
			t.Fatal(e)
		}
		return reply.Subnet
	}
	if got := details(); got.AvailableIpRange != nil || got.UsedIpRange != nil {
		t.Fatal("missing status invented ranges", got)
	}
	var object map[string]any
	api.Mu.Lock()
	for _, o := range api.Objects {
		if o["kind"] == "Subnet" {
			object = o
		}
	}
	api.Mu.Unlock()
	metadata := object["metadata"].(map[string]any)
	labels := metadata["labels"].(map[string]any)
	statusFields := object["status"].(map[string]any)
	refresh := func(change func()) {
		t.Helper()
		api.Mu.Lock()
		change()
		api.Mu.Unlock()
		if e := p.NotifyResource(ctx, tenant, current.ID); e != nil {
			t.Fatal(e)
		}
		if _, e := worker.Step(ctx); e != nil {
			t.Fatal(e)
		}
	}
	refresh(func() {
		statusFields["v4availableIPrange"] = "10.42.1.20-10.42.1.254"
		statusFields["v4usingIPrange"] = "10.42.1.2,10.42.1.4-10.42.1.19"
	})
	before := details()
	if before.AvailableIpRange == nil || before.GetAvailableIpRange() != "10.42.1.20-10.42.1.254" || before.UsedIpRange == nil || before.GetUsedIpRange() != "10.42.1.2,10.42.1.4-10.42.1.19" {
		t.Fatal("ranges not persisted to detail", before)
	}
	refresh(func() {
		statusFields["v4availableIPrange"] = "10.42.1.21-10.42.1.254"
		statusFields["v4usingIPrange"] = "10.42.1.2-10.42.1.20"
	})
	if got := details(); got.GetAvailableIpRange() != "10.42.1.21-10.42.1.254" || got.GetUsedIpRange() != "10.42.1.2-10.42.1.20" || got.State != before.State || !got.ObservedAt.AsTime().After(before.ObservedAt.AsTime()) {
		t.Fatal("range-only refresh lost", got)
	}
	refresh(func() { statusFields["v4availableIPrange"] = ""; statusFields["v4usingIPrange"] = "" })
	if got := details(); got.AvailableIpRange == nil || got.UsedIpRange == nil || got.GetAvailableIpRange() != "" || got.GetUsedIpRange() != "" {
		t.Fatal("empty range lost presence", got)
	}
	refresh(func() {
		delete(statusFields, "v4availableIPrange")
		statusFields["v4usingIPrange"] = int64(123)
		statusFields["v4availableIPs"] = int64(999)
	})
	if got := details(); got.AvailableIpRange != nil || got.UsedIpRange != nil {
		t.Fatal("new missing/invalid ranges retained old values", got)
	}
	refresh(func() { statusFields["v4availableIPrange"] = int64(-1); statusFields["v4usingIPrange"] = 1.5 })
	if got := details(); got.AvailableIpRange != nil || got.UsedIpRange != nil {
		t.Fatal("non-string ranges accepted", got)
	}
	refresh(func() {
		statusFields["v4availableIPrange"] = "10.42.1.30-10.42.1.254"
		statusFields["v4usingIPrange"] = "10.42.1.2-10.42.1.29"
	})
	before = details()
	oldUID := metadata["uid"]
	refresh(func() {
		metadata["uid"] = uuid.NewString()
		statusFields["v4availableIPrange"] = "10.42.1.2-10.42.1.254"
	})
	if got := details(); got.GetAvailableIpRange() != "10.42.1.30-10.42.1.254" || !got.ObservedAt.AsTime().Equal(before.ObservedAt.AsTime()) {
		t.Fatal("foreign UID renewed ranges", got)
	}
	time.Sleep(policy.RetryMin)
	refresh(func() { metadata["uid"] = oldUID; labels["network.ani.io/tenant-id"] = uuid.NewString() })
	if got := details(); got.GetAvailableIpRange() != "10.42.1.30-10.42.1.254" || !got.ObservedAt.AsTime().Equal(before.ObservedAt.AsTime()) {
		t.Fatal("foreign tenant renewed ranges", got)
	}
	time.Sleep(policy.RetryMin)
	refresh(func() { labels["network.ani.io/tenant-id"] = tenant; statusFields["observedGeneration"] = int64(0) })
	if got := details(); got.GetAvailableIpRange() != "10.42.1.2-10.42.1.254" || got.UsedIpRange == nil {
		t.Fatal("generation hid directly reported ranges", got)
	}
	time.Sleep(policy.RetryMin)
	refresh(func() {
		statusFields["observedGeneration"] = int64(1)
		statusFields["v4availableIPrange"] = "10.42.1.30-10.42.1.254"
	})
	before = details()
	unavailable.Store(true)
	refresh(func() {})
	if got := details(); got.GetAvailableIpRange() != "10.42.1.30-10.42.1.254" || !got.ObservedAt.AsTime().Equal(before.ObservedAt.AsTime()) {
		t.Fatal("read failure refreshed ranges", got)
	}
	staleNetwork, e := biz.NewNetwork(p, []byte("0123456789abcdef0123456789abcdef"))
	if e != nil {
		t.Fatal(e)
	}
	stale, e := service.NewNetworkService(staleNetwork).GetSubnet(ctx, &networkv1.GetSubnetRequest{TenantId: tenant, SubnetId: current.ID})
	if e != nil || !stale.Subnet.ObservationStale || stale.Subnet.GetAvailableIpRange() != "10.42.1.30-10.42.1.254" {
		t.Fatal("read error lost its diagnostic or previous ranges", stale, e)
	}
	if _, e := rpc.GetSubnet(ctx, &networkv1.GetSubnetRequest{TenantId: uuid.NewString(), SubnetId: current.ID}); status.Code(e) != codes.NotFound {
		t.Fatal("cross-tenant detail", e)
	}
	unavailable.Store(false)
	time.Sleep(policy.RetryMin)
	if e := p.NotifyResource(ctx, tenant, current.ID); e != nil {
		t.Fatal(e)
	}
	work, found, e := p.Claim(ctx, uuid.NewString(), time.Minute)
	if e != nil || !found || work.Resource.ID != current.ID {
		t.Fatal("range fence claim", work, found, e)
	}
	empty := ""
	progress := biz.Progress{State: work.Resource.State, Observed: true, Identity: work.KnownIdentity, NextDelay: time.Hour, SubnetIPRanges: biz.SubnetIPRanges{Available: &empty, Used: &empty}}
	oldWork := work
	oldWork.Epoch--
	if e := p.Finish(ctx, oldWork, progress); !errors.Is(e, biz.ErrLeaseLost) {
		t.Fatal("older lease accepted", e)
	}
	if got := details(); got.GetAvailableIpRange() != "10.42.1.30-10.42.1.254" {
		t.Fatal("fenced completion changed ranges", got)
	}
}
