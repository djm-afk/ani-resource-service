package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"testing"

	pb "github.com/zhangzhe-ctrl/ani-resource-service/api/sandbox/v1"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/sandbox"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func TestTenantSpaceActorIsLimitedToExactMethodsAndTenant(t *testing.T) {
	cert := &x509.Certificate{DNSNames: []string{GovernanceSAN}}
	base := peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{Version: tls.VersionTLS13, PeerCertificates: []*x509.Certificate{cert}, VerifiedChains: [][]*x509.Certificate{{cert}}}}})
	const tenant = "11111111-1111-4111-8111-111111111111"
	other := "33333333-3333-4333-8333-333333333333"
	for _, tc := range []struct {
		name, actor, method string
		request             proto.Message
		allow               bool
	}{
		{"open", biz.TenantSpaceActor, pb.SandboxService_OpenTenantSpace_FullMethodName, &pb.OpenTenantSpaceRequest{TenantId: tenant}, true},
		{"close", biz.TenantSpaceActor, pb.SandboxService_CloseTenantSpace_FullMethodName, &pb.CloseTenantSpaceRequest{TenantId: tenant}, true},
		{"get", biz.TenantSpaceActor, pb.SandboxService_GetTenantSpace_FullMethodName, &pb.GetTenantSpaceRequest{TenantId: tenant}, true},
		{"foreign tenant", biz.TenantSpaceActor, pb.SandboxService_OpenTenantSpace_FullMethodName, &pb.OpenTenantSpaceRequest{TenantId: other}, false},
		{"ordinary user cannot open", "governance:user:42", pb.SandboxService_OpenTenantSpace_FullMethodName, &pb.OpenTenantSpaceRequest{TenantId: tenant}, false},
		{"token cannot close", "governance:sandbox-token:33333333-3333-4333-8333-333333333333", pb.SandboxService_CloseTenantSpace_FullMethodName, &pb.CloseTenantSpaceRequest{TenantId: tenant}, false},
		{"controller cannot create sandbox", biz.TenantSpaceActor, pb.SandboxService_ResolveCreateSpec_FullMethodName, &pb.ResolveCreateSpecRequest{TenantId: tenant}, false},
		{"controller cannot call network", biz.TenantSpaceActor, "/network.v1.NetworkService/GetVPC", &pb.GetTenantSpaceRequest{TenantId: tenant}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := metadata.NewIncomingContext(base, metadata.Pairs("x-ani-tenant-id", tenant, "x-ani-actor", tc.actor, "x-ani-request-id", "22222222-2222-4222-8222-222222222222"))
			called := false
			_, err := GovernanceUnary()(ctx, tc.request, &grpc.UnaryServerInfo{FullMethod: tc.method}, func(context.Context, any) (any, error) { called = true; return nil, nil })
			if tc.allow {
				if err != nil || !called {
					t.Fatalf("allowed call rejected: %v", err)
				}
			} else if called || status.Code(err) != codes.PermissionDenied {
				t.Fatalf("unexpected access: called=%v err=%v", called, err)
			}
		})
	}
}
