package data_test

import (
	"context"
	"github.com/google/uuid"
	networkv1 "github.com/zhangzhe-ctrl/ani-resource-service/api/network/v1"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/service/network"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"net"
	"testing"
)

func TestEgressGRPCContractAuthorizationAndNullableAppliedState(t *testing.T) {
	f := newEgressFixture(t)
	connect := func(caller *biz.EgressCaller) (networkv1.TenantEgressServiceClient, networkv1.PlatformNetworkServiceClient, networkv1.NetworkServiceClient) {
		listener := bufconn.Listen(1 << 20)
		options := []grpc.ServerOption{}
		if caller != nil {
			options = append(options, grpc.UnaryInterceptor(func(ctx context.Context, r any, _ *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
				return next(biz.WithEgressCaller(ctx, *caller), r)
			}))
		}
		server := grpc.NewServer(options...)
		networkv1.RegisterTenantEgressServiceServer(server, service.NewTenantEgressService(f.e))
		networkv1.RegisterPlatformNetworkServiceServer(server, service.NewPlatformNetworkService(f.e))
		networkv1.RegisterNetworkServiceServer(server, service.NewNetworkService(f.n))
		go func() { _ = server.Serve(listener) }()
		t.Cleanup(server.Stop)
		c, err := grpc.NewClient("passthrough:///egress-test", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return networkv1.NewTenantEgressServiceClient(c), networkv1.NewPlatformNetworkServiceClient(c), networkv1.NewNetworkServiceClient(c)
	}
	ctx := context.Background()
	deny, platformDeny, _ := connect(nil)
	if _, err := deny.CreateEIP(ctx, &networkv1.CreateEIPRequest{TargetTenantId: f.tenant, Name: "spoof", IdempotencyKey: "spoof"}); status.Code(err) != codes.PermissionDenied {
		t.Fatal("untrusted body became context", err)
	}
	if _, err := platformDeny.ListPublicAddressPools(ctx, &networkv1.ListPublicAddressPoolsRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Fatal("platform list missing authorization", err)
	}
	client, platform, n := connect(&biz.EgressCaller{TenantID: f.tenant, PlatformAdministrator: true, Attribution: biz.Attribution{Actor: "controlled-grpc", DirectCaller: "fixture"}})
	p, err := platform.GetPublicAddressPool(ctx, &networkv1.GetPublicAddressPoolRequest{PoolId: f.pool.ID})
	if err != nil || p.Resource.GetPool().TopologyFingerprint != "" || len(p.Resource.GetPool().ObservedProviderImages) != 0 {
		t.Fatal("admin verification identity missing from wire", p, err)
	}
	created, err := client.CreateEIP(ctx, &networkv1.CreateEIPRequest{Name: "rpc-address", IdempotencyKey: "rpc-address"})
	if err != nil || created.Eip.TenantId != f.tenant || created.Eip.LastOperationId == "" {
		t.Fatal("trusted context not used", created, err)
	}
	replay, err := client.CreateEIP(ctx, &networkv1.CreateEIPRequest{Name: "rpc-address", IdempotencyKey: "rpc-address"})
	if err != nil || replay.Eip.Id != created.Eip.Id {
		t.Fatal("RPC replay changed receipt", err)
	}
	operation, err := n.GetOperation(ctx, &networkv1.GetOperationRequest{TenantId: f.tenant, OperationId: created.Eip.LastOperationId})
	if err != nil || operation.Operation.ResourceType != networkv1.ResourceType_RESOURCE_TYPE_EIP || operation.Operation.Kind != networkv1.OperationKind_OPERATION_KIND_CREATE_EIP {
		t.Fatal("egress operation wire type missing", operation, err)
	}
	if _, err = client.GetEIP(ctx, &networkv1.GetEIPRequest{TargetTenantId: uuid.NewString(), EipId: created.Eip.Id}); status.Code(err) != codes.PermissionDenied {
		t.Fatal("admin implicitly delegated", err)
	}
	f.drive(t, func() bool {
		v, err := f.e.GetEIP(f.ctx, "", created.Eip.Id)
		return err == nil && v.State == biz.Available
	})
	vpc := availableEgressVPC(t, f, "rpc-vpc")
	bound, err := client.BindVPCSnat(ctx, &networkv1.BindVPCSnatRequest{VpcId: vpc.ID, EipId: created.Eip.Id, IdempotencyKey: "rpc-bind"})
	if err != nil || !bound.Binding.DesiredEnabled || bound.Binding.AppliedEnabled != nil {
		t.Fatal("acceptance fabricated applied state", bound, err)
	}
	enabled := true
	current := f.binding(t, bound.Binding.Id, biz.Available, &enabled)
	changed, err := client.SetVPCSnatEnabled(ctx, &networkv1.SetVPCSnatEnabledRequest{BindingId: current.ID, Enabled: false, ExpectedVersion: current.Version, IdempotencyKey: "rpc-disable"})
	if err != nil || changed.Binding.AppliedEnabled != nil || changed.Binding.DesiredEnabled {
		t.Fatal("wire disable acceptance", changed, err)
	}
	enabled = false
	f.binding(t, current.ID, biz.Available, &enabled)
	disabled, err := client.GetVPCSnatBinding(ctx, &networkv1.GetVPCSnatBindingRequest{BindingId: current.ID})
	if err != nil || disabled.Binding.AppliedEnabled == nil || *disabled.Binding.AppliedEnabled {
		t.Fatal("false vs absent wire evidence", disabled, err)
	}
	tenantOnly, tenantPlatform, _ := connect(&biz.EgressCaller{TenantID: f.tenant})
	if _, err = tenantPlatform.GetPublicAddressPool(ctx, &networkv1.GetPublicAddressPoolRequest{PoolId: f.pool.ID}); status.Code(err) != codes.PermissionDenied {
		t.Fatal("tenant got infrastructure details", err)
	}
	if _, err = tenantOnly.DeleteEIP(ctx, &networkv1.DeleteEIPRequest{EipId: created.Eip.Id}); status.Code(err) != codes.FailedPrecondition {
		t.Fatal("wire release occupancy lost", err)
	}
}
