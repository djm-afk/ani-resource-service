package data_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	networkv1 "github.com/zhangzhe-ctrl/ani-resource-service/api/network/v1"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	service "github.com/zhangzhe-ctrl/ani-resource-service/internal/service/network"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func listTotal(t *testing.T, reply proto.Message, want int64) {
	t.Helper()
	m := reply.ProtoReflect()
	f := m.Descriptor().Fields().ByName("total")
	if f == nil {
		t.Fatalf("%s is missing total", m.Descriptor().FullName())
	}
	if got := m.Get(f).Int(); got != want {
		t.Fatalf("%s total=%d, want %d", m.Descriptor().FullName(), got, want)
	}
	body, err := protojson.MarshalOptions{EmitUnpopulated: true}.Marshal(reply)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err = json.Unmarshal(body, &value); err != nil {
		t.Fatal(err)
	}
	if value["total"] != strconv.FormatInt(want, 10) {
		t.Fatalf("wire JSON total=%v, want %d", value["total"], want)
	}
}

func TestVPCListTotalAcrossPagesAndFilters(t *testing.T) {
	repo, owner := database(t)
	n := newNetwork(t, repo, time.Minute)
	s := service.NewNetworkService(n)
	ctx := context.Background()
	tenant, other := uuid.NewString(), uuid.NewString()
	var deleted string
	for i, name := range []string{"alpha", "beta", "gamma", "removed"} {
		v, err := n.CreateVPC(ctx, biz.CreateVPC{TenantID: tenant, Name: name, CIDR: "10.42.0.0/16", IdempotencyKey: name})
		if err != nil {
			t.Fatal(err)
		}
		if i == 3 {
			deleted = v.ID
		}
	}
	if _, err := n.CreateVPC(ctx, biz.CreateVPC{TenantID: other, Name: "alpha", CIDR: "10.42.0.0/16", IdempotencyKey: "other"}); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(ctx, `UPDATE network_vpcs SET state='deleted' WHERE tenant_id=$1 AND vpc_id=$2`, tenant, deleted); err != nil {
		t.Fatal(err)
	}
	first, err := s.ListVPCs(ctx, &networkv1.ListVPCsRequest{TenantId: tenant, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	listTotal(t, first, 3)
	if len(first.Items) != 2 || first.NextCursor == "" {
		t.Fatal("missing first page")
	}
	second, err := s.ListVPCs(ctx, &networkv1.ListVPCsRequest{TenantId: tenant, Limit: 2, Cursor: first.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	listTotal(t, second, 3)
	if len(second.Items) != 1 || second.NextCursor != "" {
		t.Fatal("wrong final page")
	}
	for _, tc := range []struct {
		name, tenant string
		state        networkv1.ResourceState
		want         int64
	}{
		{"alpha", tenant, networkv1.ResourceState_RESOURCE_STATE_UNSPECIFIED, 1},
		{"missing", tenant, networkv1.ResourceState_RESOURCE_STATE_UNSPECIFIED, 0},
		{"", other, networkv1.ResourceState_RESOURCE_STATE_UNSPECIFIED, 1},
		{"", uuid.NewString(), networkv1.ResourceState_RESOURCE_STATE_UNSPECIFIED, 0},
		{"", tenant, networkv1.ResourceState_RESOURCE_STATE_DELETED, 1},
	} {
		r, err := s.ListVPCs(ctx, &networkv1.ListVPCsRequest{TenantId: tc.tenant, Name: tc.name, State: tc.state, Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		listTotal(t, r, tc.want)
	}
	// A valid later-page cursor may yield no rows after deletion, while total remains nonzero.
	if _, err := owner.Exec(ctx, `UPDATE network_vpcs SET state='deleted' WHERE tenant_id=$1 AND vpc_id=$2`, tenant, second.Items[0].Id); err != nil {
		t.Fatal(err)
	}
	empty, err := s.ListVPCs(ctx, &networkv1.ListVPCsRequest{TenantId: tenant, Limit: 2, Cursor: first.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	listTotal(t, empty, 2)
	if len(empty.Items) != 0 {
		t.Fatal("expected empty later page")
	}
}

func TestSubnetListTotalAcrossPagesAndParents(t *testing.T) {
	repo, _ := database(t)
	n := newNetwork(t, repo, time.Minute)
	w := subnetWorker(t, repo)
	ctx := context.Background()
	tenant := uuid.NewString()
	parent := availableVPC(t, n, w, tenant, "parent")
	otherParent := availableVPC(t, n, w, tenant, "other-parent")
	for i, cidr := range []string{"10.42.1.0/24", "10.42.2.0/24", "10.42.3.0/24"} {
		if _, err := n.CreateSubnet(ctx, biz.CreateSubnet{TenantID: tenant, VPCID: parent.ID, Name: cidr, CIDR: cidr, IdempotencyKey: string(rune('a' + i))}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := n.CreateSubnet(ctx, biz.CreateSubnet{TenantID: tenant, VPCID: otherParent.ID, Name: "other", CIDR: "10.42.1.0/24", IdempotencyKey: "other"}); err != nil {
		t.Fatal(err)
	}
	s := service.NewNetworkService(n)
	first, err := s.ListSubnets(ctx, &networkv1.ListSubnetsRequest{TenantId: tenant, VpcId: parent.ID, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	listTotal(t, first, 3)
	second, err := s.ListSubnets(ctx, &networkv1.ListSubnetsRequest{TenantId: tenant, VpcId: parent.ID, Limit: 2, Cursor: first.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	listTotal(t, second, 3)
	if len(second.Items) != 1 || second.NextCursor != "" {
		t.Fatal("wrong final subnet page")
	}
	for _, tc := range []struct {
		tenant, parent, name string
		want                 int64
	}{
		{tenant, "", "", 4}, {tenant, parent.ID, "10.42.1.0/24", 1}, {tenant, parent.ID, "absent", 0}, {uuid.NewString(), parent.ID, "", 0},
	} {
		r, err := s.ListSubnets(ctx, &networkv1.ListSubnetsRequest{TenantId: tc.tenant, VpcId: tc.parent, Name: tc.name, Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		listTotal(t, r, tc.want)
	}
}

func TestEgressListTotalsAndPlatformAuthorization(t *testing.T) {
	f := newEgressFixture(t)
	for _, name := range []string{"alpha", "beta", "gamma"} {
		f.eip(t, name)
	}
	tenant := service.NewTenantEgressService(f.e)
	first, err := tenant.ListEIPs(f.ctx, &networkv1.ListEIPsRequest{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	listTotal(t, first, 3)
	second, err := tenant.ListEIPs(f.ctx, &networkv1.ListEIPsRequest{Limit: 2, Cursor: first.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	listTotal(t, second, 3)
	if len(second.Items) != 1 || second.NextCursor != "" {
		t.Fatal("wrong final EIP page")
	}
	filtered, err := tenant.ListEIPs(f.ctx, &networkv1.ListEIPsRequest{Name: "alpha", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	listTotal(t, filtered, 1)
	other := biz.WithEgressCaller(context.Background(), biz.EgressCaller{TenantID: uuid.NewString()})
	empty, err := tenant.ListEIPs(other, &networkv1.ListEIPsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	listTotal(t, empty, 0)
	f.p.UseVirtualSwitchInfrastructure(&switchBoundary{})
	sw, err := f.e.CreateVirtualSwitch(f.ctx, biz.VirtualSwitchIntent{Name: "list-totals", IdempotencyKey: "list-switch"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.e.CreatePlatform(f.ctx, biz.PlatformIntent{Kind: "create_vlan", Name: "vlan", IdempotencyKey: "vlan", Vlan: &biz.VlanConfig{ClusterID: "test-cluster", VirtualSwitchID: sw.ID, DeviceName: "fixture0", VlanID: 0}}); err != nil {
		t.Fatal(err)
	}
	f.platform(t, biz.PlatformIntent{Kind: "create_egress_gateway", Name: "second-gateway", IdempotencyKey: "second-gateway"})
	if _, err := f.e.CreatePlatform(f.ctx, intranetIntent("intranet", "172.20.0.0/24", "172.20.0.1")); err != nil {
		t.Fatal(err)
	}
	platform := service.NewPlatformNetworkService(f.e)
	vlan, err := platform.ListVlanNetworks(f.ctx, &networkv1.ListVlanNetworksRequest{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	listTotal(t, vlan, 1)
	gateway, err := platform.ListEgressGateways(f.ctx, &networkv1.ListEgressGatewaysRequest{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	listTotal(t, gateway, 2)
	nextGateway, err := platform.ListEgressGateways(f.ctx, &networkv1.ListEgressGatewaysRequest{Limit: 1, Cursor: gateway.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	listTotal(t, nextGateway, 2)
	public, err := platform.ListPublicAddressPools(f.ctx, &networkv1.ListPublicAddressPoolsRequest{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	listTotal(t, public, 1)
	intranet, err := platform.ListIntranetAddressPools(f.ctx, &networkv1.ListIntranetAddressPoolsRequest{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	listTotal(t, intranet, 1)
	if _, err = platform.ListPublicAddressPools(other, &networkv1.ListPublicAddressPoolsRequest{}); err == nil {
		t.Fatal("tenant obtained platform count")
	}
}

func TestLoadBalancerListTotalAcrossPagesAndFilters(t *testing.T) {
	f := newLBAdmissionFixture(t)
	for i, ip := range []string{"10.42.1.100", "10.42.1.101", "10.42.1.102"} {
		req := f.request
		req.PrivateIP = ip
		req.IdempotencyKey = fmt.Sprint("total-", i)
		req.Name = fmt.Sprint("lb-", i)
		if _, err := f.lbs.Create(f.f.ctx, req); err != nil {
			t.Fatal(err)
		}
	}
	first, err := f.rpc.ListLoadBalancers(f.f.ctx, &networkv1.ListLoadBalancersRequest{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	listTotal(t, first, 3)
	second, err := f.rpc.ListLoadBalancers(f.f.ctx, &networkv1.ListLoadBalancersRequest{Limit: 2, Cursor: first.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	listTotal(t, second, 3)
	if len(second.Items) != 1 || second.NextCursor != "" {
		t.Fatal("wrong final LB page")
	}
	for _, tc := range []struct {
		name, vpc, subnet string
		exposure          networkv1.LoadBalancerExposure
		want              int64
	}{
		{"lb-1", f.vpc.ID, f.subnet.ID, networkv1.LoadBalancerExposure_LOAD_BALANCER_EXPOSURE_PRIVATE, 1},
		{"absent", "", "", networkv1.LoadBalancerExposure_LOAD_BALANCER_EXPOSURE_UNSPECIFIED, 0},
		{"", f.vpc.ID, f.backendSubnet.ID, networkv1.LoadBalancerExposure_LOAD_BALANCER_EXPOSURE_UNSPECIFIED, 0},
		{"", "", "", networkv1.LoadBalancerExposure_LOAD_BALANCER_EXPOSURE_PUBLIC, 0},
	} {
		r, err := f.rpc.ListLoadBalancers(f.f.ctx, &networkv1.ListLoadBalancersRequest{Name: tc.name, VpcId: tc.vpc, SubnetId: tc.subnet, Exposure: tc.exposure, Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		listTotal(t, r, tc.want)
	}
	other := biz.WithEgressCaller(context.Background(), biz.EgressCaller{TenantID: uuid.NewString()})
	r, err := f.rpc.ListLoadBalancers(other, &networkv1.ListLoadBalancersRequest{})
	if err != nil {
		t.Fatal(err)
	}
	listTotal(t, r, 0)
}
