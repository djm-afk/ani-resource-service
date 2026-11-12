package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"log/slog"
	"strings"
	"testing"

	"github.com/go-kratos/kratos/v3/transport"
	imagev1 "github.com/zhangzhe-ctrl/ani-resource-service/api/image/v1"
	imagebiz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/image"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

func TestImageGovernanceAllowlistAndScope(t *testing.T) {
	tenant := "11111111-1111-4111-8111-111111111111"
	cert := &x509.Certificate{DNSNames: []string{GovernanceSAN}}
	ctx := peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{Version: tls.VersionTLS13, PeerCertificates: []*x509.Certificate{cert}, VerifiedChains: [][]*x509.Certificate{{cert}}}}})
	ctx = metadata.NewIncomingContext(ctx, metadata.Pairs("x-ani-tenant-id", tenant, "x-ani-actor", "governance:user:7", "x-ani-request-id", "22222222-2222-4222-8222-222222222222"))
	if len(imageGovernanceMethods) != 11 {
		t.Fatal("unexpected Image surface")
	}
	for _, method := range imagev1.TenantImageService_ServiceDesc.Methods {
		name := "/image.v1.TenantImageService/" + method.MethodName
		called := false
		_, err := GovernanceUnary()(ctx, &imagev1.GetImageSpaceRequest{TenantId: tenant}, &grpc.UnaryServerInfo{FullMethod: name}, func(ctx context.Context, _ any) (any, error) {
			called = true
			c, err := imagebiz.RequireTenant(ctx, tenant)
			if err != nil || c.Actor != "governance:user:7" || c.Subject != GovernanceSAN {
				t.Fatal("caller not bound")
			}
			return nil, nil
		})
		if err != nil || !called {
			t.Fatal("tenant method denied", name, err)
		}
	}
	for _, test := range []struct{ method, tenant string }{
		{imagev1.TenantImageService_GetImageSpace_FullMethodName, ""},
		{imagev1.TenantImageService_GetImageSpace_FullMethodName, "33333333-3333-4333-8333-333333333333"},
		{imagev1.ImageRuntimeService_GetTenantPullMaterial_FullMethodName, tenant},
		{imagev1.ImageRuntimeService_ResolveImageForWorkload_FullMethodName, tenant},
		{"/image.v1.TenantImageService/InitializePlatform", tenant},
		{"/image.v1.TenantImageService/GetImageSpaceExtra", tenant},
	} {
		called := false
		_, err := GovernanceUnary()(ctx, &imagev1.GetImageSpaceRequest{TenantId: test.tenant}, &grpc.UnaryServerInfo{FullMethod: test.method}, func(context.Context, any) (any, error) { called = true; return nil, nil })
		if called || status.Code(err) != codes.PermissionDenied {
			t.Fatal("denied Image request reached business handler", test, err)
		}
	}
}

type imageLogTransport struct{ operation string }

func (imageLogTransport) Kind() transport.Kind            { return transport.KindGRPC }
func (imageLogTransport) Endpoint() string                { return "grpc://127.0.0.1" }
func (t imageLogTransport) Operation() string             { return t.operation }
func (imageLogTransport) RequestHeader() transport.Header { return emptyHeader{} }
func (imageLogTransport) ReplyHeader() transport.Header   { return emptyHeader{} }

// emptyHeader satisfies transport.Header; the metadata middleware reads it even
// when no metadata is present.
type emptyHeader struct{}

func (emptyHeader) Get(string) string      { return "" }
func (emptyHeader) Set(string, string)     {}
func (emptyHeader) Add(string, string)     {}
func (emptyHeader) Keys() []string         { return nil }
func (emptyHeader) Values(string) []string { return nil }
func TestImageRequestAndCredentialReplyNotLogged(t *testing.T) {
	// The gRPC listeners no longer install a per-call request logger: request
	// bodies never reach the log. This test pins that property against the
	// surviving middleware chain and proves the handler still sees the real
	// request and reply unchanged.
	var out bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&out, nil))
	sentinel := "image-private-sentinel"
	ctx := transport.NewServerContext(context.Background(), imageLogTransport{imagev1.TenantImageService_IssuePublisherCredential_FullMethodName})
	req := &imagev1.IssuePublisherCredentialRequest{IdempotencyKey: sentinel}
	called := false
	reply := &imagev1.IssuePublisherCredentialResponse{Secret: sentinel}
	_, err := serverMiddlewareChain(logger, func(_ context.Context, v any) (any, error) {
		called = true
		if v != req {
			t.Fatal("request replaced before handler")
		}
		return reply, nil
	})(ctx, req)
	if err != nil || !called {
		t.Fatalf("handler was not invoked normally: err=%v called=%v", err, called)
	}
	if strings.Contains(out.String(), sentinel) {
		t.Fatalf("sensitive request/reply body disclosed in logs: %s", out.String())
	}
}
