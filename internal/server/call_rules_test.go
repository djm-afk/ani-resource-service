package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	imagev1 "github.com/zhangzhe-ctrl/ani-resource-service/api/image/v1"
	networkv1 "github.com/zhangzhe-ctrl/ani-resource-service/api/network/v1"
	conf "github.com/zhangzhe-ctrl/ani-resource-service/internal/conf/v1"
	"github.com/zhangzhe-ctrl/ani-resource-service/tests/testenv"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
)

const callRulesTenant = "11111111-1111-4111-8111-111111111111"
const callRulesRequest = "22222222-2222-4222-8222-222222222222"

// These probes test the actual listener's authorization before dispatch, not
// persistence or provider behavior. Cross-repository acceptance tests use the
// actual business services and repositories separately.
type callRulesProbe struct {
	networkv1.UnimplementedNetworkServiceServer
	calls atomic.Int32
}

func (p *callRulesProbe) trusted(ctx context.Context) error {
	principal, ok := ctx.Value(governancePrincipalKey{}).(governancePrincipal)
	if !ok || principal.Workload != GovernanceSAN || principal.TenantID != callRulesTenant || principal.RequestID != callRulesRequest {
		return status.Error(codes.Internal, "trusted service context missing")
	}
	p.calls.Add(1)
	return nil
}
func (p *callRulesProbe) GetVPC(ctx context.Context, r *networkv1.GetVPCRequest) (*networkv1.GetVPCResponse, error) {
	if err := p.trusted(ctx); err != nil {
		return nil, err
	}
	return &networkv1.GetVPCResponse{Vpc: &networkv1.VPC{TenantId: r.TenantId}}, nil
}
func (p *callRulesProbe) ListVPCs(ctx context.Context, r *networkv1.ListVPCsRequest) (*networkv1.ListVPCsResponse, error) {
	if err := p.trusted(ctx); err != nil {
		return nil, err
	}
	return &networkv1.ListVPCsResponse{}, nil
}
func (p *callRulesProbe) CreateVPC(ctx context.Context, r *networkv1.CreateVPCRequest) (*networkv1.CreateVPCResponse, error) {
	if err := p.trusted(ctx); err != nil {
		return nil, err
	}
	return &networkv1.CreateVPCResponse{Vpc: &networkv1.VPC{TenantId: r.TenantId}}, nil
}
func (p *callRulesProbe) CreateSubnet(ctx context.Context, r *networkv1.CreateSubnetRequest) (*networkv1.CreateSubnetResponse, error) {
	if err := p.trusted(ctx); err != nil {
		return nil, err
	}
	return &networkv1.CreateSubnetResponse{Subnet: &networkv1.Subnet{TenantId: r.TenantId}}, nil
}

func startCallRulesServer(t *testing.T, readOnly bool) (*grpc.ClientConn, *callRulesProbe, string) {
	t.Helper()
	fixture := testenv.NewServiceTLS(t)
	serverTLS, err := GovernanceTLS(fixture.CAFile, fixture.CertFile, fixture.KeyFile)
	if err != nil {
		t.Fatal(err)
	}
	gs, err := NewGRPCServer(&conf.Server_GRPC{Network: "tcp", Addr: "127.0.0.1:0", Timeout: durationpb.New(time.Second)}, NewReadiness(), serverTLS, readOnly, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	probe := &callRulesProbe{}
	networkv1.RegisterNetworkServiceServer(gs, probe)
	networkv1.RegisterPlatformNetworkServiceServer(gs, &networkv1.UnimplementedPlatformNetworkServiceServer{})
	imagev1.RegisterTenantImageServiceServer(gs, imageIdentityProbe{})
	endpoint, err := gs.Endpoint()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- gs.Start(context.Background()) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := gs.Stop(ctx); err != nil {
			t.Error(err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(time.Second):
			t.Error("business listener did not stop")
		}
	})
	conn, err := grpc.NewClient(endpoint.Host, grpc.WithTransportCredentials(credentials.NewTLS(fixture.Client)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn, probe, endpoint.Host
}

func callRulesContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(metadata.NewOutgoingContext(context.Background(), metadata.Pairs("x-ani-tenant-id", callRulesTenant, "x-ani-actor", "governance:user:7", "x-ani-request-id", callRulesRequest)), time.Second)
}

func trustedAttribution() *networkv1.Attribution {
	return &networkv1.Attribution{Actor: "governance:user:7", DirectCaller: GovernanceSAN, CorrelationId: callRulesRequest}
}

func TestInternalBusinessListenerPolicy(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		name := "full"
		if readOnly {
			name = "vpc-read"
		}
		t.Run(name, func(t *testing.T) {
			conn, probe, addr := startCallRulesServer(t, readOnly)
			for _, tc := range []struct {
				name, method      string
				request, response proto.Message
				want              codes.Code
			}{
				{"get", GetVPCMethod, &networkv1.GetVPCRequest{TenantId: callRulesTenant}, &networkv1.GetVPCResponse{}, codes.OK},
				{"list", "/network.v1.NetworkService/ListVPCs", &networkv1.ListVPCsRequest{TenantId: callRulesTenant}, &networkv1.ListVPCsResponse{}, codes.OK},
				{"create", "/network.v1.NetworkService/CreateVPC", &networkv1.CreateVPCRequest{TenantId: callRulesTenant, Attribution: trustedAttribution()}, &networkv1.CreateVPCResponse{}, codes.OK},
				{"image", imagev1.TenantImageService_GetImageSpace_FullMethodName, &imagev1.GetImageSpaceRequest{TenantId: callRulesTenant}, &imagev1.GetImageSpaceResponse{}, codes.OK},
				{"attachment", "/network.v1.NetworkService/PrepareAttachment", &networkv1.PrepareAttachmentRequest{}, &networkv1.PrepareAttachmentResponse{}, codes.PermissionDenied},
				{"platform", networkv1.PlatformNetworkService_CreateVirtualSwitch_FullMethodName, &networkv1.CreateVirtualSwitchRequest{}, &networkv1.CreateVirtualSwitchResponse{}, codes.PermissionDenied},
				{"unknown", "/network.v1.UnknownService/Anything", &networkv1.GetVPCRequest{}, &networkv1.GetVPCResponse{}, codes.PermissionDenied},
			} {
				t.Run(tc.name, func(t *testing.T) {
					want := tc.want
					if readOnly && tc.method != GetVPCMethod {
						want = codes.PermissionDenied
					}
					before := probe.calls.Load()
					ctx, cancel := callRulesContext()
					defer cancel()
					err := conn.Invoke(ctx, tc.method, tc.request, tc.response)
					if status.Code(err) != want {
						t.Fatalf("status=%s want=%s: %v", status.Code(err), want, err)
					}
					if want != codes.OK && probe.calls.Load() != before {
						t.Fatal("rejected request reached business probe")
					}
				})
			}
			plain, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				t.Fatal(err)
			}
			defer plain.Close()
			ctx, cancel := callRulesContext()
			defer cancel()
			if err := plain.Invoke(ctx, GetVPCMethod, &networkv1.GetVPCRequest{TenantId: callRulesTenant}, &networkv1.GetVPCResponse{}); status.Code(err) != codes.Unavailable {
				t.Fatalf("plaintext accepted: %v", err)
			}
		})
	}
}

func TestGovernanceAttributionCannotOverrideTrustedScope(t *testing.T) {
	conn, probe, _ := startCallRulesServer(t, false)
	for _, method := range []string{"CreateVPC", "CreateSubnet"} {
		for _, tc := range []struct {
			name   string
			change func(*networkv1.Attribution)
			want   codes.Code
		}{
			{"matching", func(*networkv1.Attribution) {}, codes.OK},
			{"actor", func(a *networkv1.Attribution) { a.Actor = "governance:user:8" }, codes.PermissionDenied},
			{"caller", func(a *networkv1.Attribution) { a.DirectCaller = "other-service" }, codes.PermissionDenied},
			{"trace", func(a *networkv1.Attribution) { a.CorrelationId = "33333333-3333-4333-8333-333333333333" }, codes.PermissionDenied},
			{"missing-actor", func(a *networkv1.Attribution) { a.Actor = "" }, codes.PermissionDenied},
		} {
			t.Run(method+"/"+tc.name, func(t *testing.T) {
				a := trustedAttribution()
				tc.change(a)
				var request, response proto.Message
				if method == "CreateVPC" {
					request, response = &networkv1.CreateVPCRequest{TenantId: callRulesTenant, Attribution: a}, &networkv1.CreateVPCResponse{}
				} else {
					request, response = &networkv1.CreateSubnetRequest{TenantId: callRulesTenant, Attribution: a}, &networkv1.CreateSubnetResponse{}
				}
				before := probe.calls.Load()
				ctx, cancel := callRulesContext()
				defer cancel()
				err := conn.Invoke(ctx, "/network.v1.NetworkService/"+method, request, response)
				if status.Code(err) != tc.want {
					t.Fatalf("status=%s want=%s: %v", status.Code(err), tc.want, err)
				}
				if tc.want != codes.OK && probe.calls.Load() != before {
					t.Fatal("attribution conflict reached handler")
				}
			})
		}
	}
	ctx, cancel := callRulesContext()
	defer cancel()
	if err := conn.Invoke(ctx, "/network.v1.NetworkService/CreateVPC", &networkv1.CreateVPCRequest{TenantId: callRulesTenant}, &networkv1.CreateVPCResponse{}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("missing attribution accepted: %v", err)
	}
}

func TestClientIdentityRequiresOnlyOneExactDNS(t *testing.T) {
	for _, cert := range []*x509.Certificate{
		nil, {}, {DNSNames: []string{GovernanceSAN, GovernanceSAN}},
		{DNSNames: []string{GovernanceSAN}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}},
		{DNSNames: []string{GovernanceSAN}, URIs: []*url.URL{{Scheme: "spiffe", Host: "ani"}}},
		{DNSNames: []string{GovernanceSAN}, EmailAddresses: []string{"gov@example.test"}},
		{DNSNames: []string{"*.ani-governance"}},
	} {
		if exactClientDNS(cert, GovernanceSAN) {
			t.Fatalf("ambiguous client identity accepted: %v", cert)
		}
	}
	if !exactClientDNS(&x509.Certificate{DNSNames: []string{GovernanceSAN}}, GovernanceSAN) {
		t.Fatal("registered identity rejected")
	}
	if exactDNS(&x509.Certificate{DNSNames: []string{NetworkSAN, "*.internal.test"}}, NetworkSAN) {
		t.Fatal("wildcard server identity accepted")
	}
}

func TestInternalBusinessListenerRejectsMissingTLS(t *testing.T) {
	config := &conf.Server_GRPC{Network: "tcp", Addr: "127.0.0.1:0", Timeout: durationpb.New(time.Second)}
	for _, tlsConfig := range []*tls.Config{nil, {MinVersion: tls.VersionTLS12}, {MinVersion: tls.VersionTLS13}} {
		if _, err := NewGRPCServer(config, NewReadiness(), tlsConfig, false, discardLogger()); err == nil {
			t.Fatal("incomplete TLS silently enabled business listener")
		}
	}
}

// An optional legacy operator tag cannot claim a different delegated actor.
func TestGovernanceOperatorCannotReplaceTrace(t *testing.T) {
	conn, _, _ := startCallRulesServer(t, false)
	ctx, cancel := callRulesContext()
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, OperatorHeader, "gov-user-8")
	if err := conn.Invoke(ctx, GetVPCMethod, &networkv1.GetVPCRequest{TenantId: callRulesTenant}, &networkv1.GetVPCResponse{}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("conflicting operator accepted: %v", err)
	}
}
