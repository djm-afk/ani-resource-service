//go:build sandboxintegration

package sandbox_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/google/uuid"
	pb "github.com/zhangzhe-ctrl/ani-resource-service/api/sandbox/v1"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/sandbox"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/server"
	service "github.com/zhangzhe-ctrl/ani-resource-service/internal/service/sandbox"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

func TestTenantSpaceMTLSAndPostgres(t *testing.T) {
	f := sandboxDatabase(t)
	handler, err := service.NewService(f.repo, unusedSpaceCompute{}, service.Config{ClusterID: uuid.NewString(), NamespacePrefix: "tenant-", NamespaceResolver: unusedSpaceNamespace{}, RuntimeClass: "kata", WorkspaceSize: "5Gi"})
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err = x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	issue := func(name string, usage x509.ExtKeyUsage, serial int64) tls.Certificate {
		t.Helper()
		leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		leaf := &x509.Certificate{SerialNumber: big.NewInt(serial), DNSNames: []string{name}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{usage}, KeyUsage: x509.KeyUsageDigitalSignature}
		der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &leafKey.PublicKey, key)
		if err != nil {
			t.Fatal(err)
		}
		return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: leafKey}
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{issue(server.NetworkSAN, x509.ExtKeyUsageServerAuth, 2)}, ClientCAs: roots, ClientAuth: tls.RequireAndVerifyClientCert})), grpc.UnaryInterceptor(server.GovernanceUnary()))
	pb.RegisterSandboxServiceServer(grpcServer, handler)
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(func() { grpcServer.Stop(); _ = listener.Close() })
	conn, err := grpc.NewClient("passthrough:///tenant-space-test", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }), grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: server.NetworkSAN, Certificates: []tls.Certificate{issue(server.GovernanceSAN, x509.ExtKeyUsageClientAuth, 3)}})))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	tenant, op := uuid.NewString(), uuid.NewString()
	callContext := func(actor string) (context.Context, context.CancelFunc) {
		ctx := metadata.NewOutgoingContext(context.Background(), metadata.Pairs("x-ani-tenant-id", tenant, "x-ani-actor", actor, "x-ani-request-id", uuid.NewString()))
		return context.WithTimeout(ctx, 5*time.Second)
	}
	client := pb.NewSandboxServiceClient(conn)
	ctx, cancel := callContext("governance:user:42")
	_, err = client.OpenTenantSpace(ctx, &pb.OpenTenantSpaceRequest{TenantId: tenant, OperationId: op})
	cancel()
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("ordinary user accepted: %v", err)
	}
	ctx, cancel = callContext(biz.TenantSpaceActor)
	opened, err := client.OpenTenantSpace(ctx, &pb.OpenTenantSpaceRequest{TenantId: tenant, OperationId: op})
	cancel()
	if err != nil || opened.GetSpace().GetPhase() != "open" {
		t.Fatalf("open: %v %v", opened, err)
	}
	ctx, cancel = callContext(biz.TenantSpaceActor)
	_, err = client.ListSandboxes(ctx, &pb.ListSandboxesRequest{TenantId: tenant})
	cancel()
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("controller accessed sandbox listing: %v", err)
	}
	ctx, cancel = callContext(biz.TenantSpaceActor)
	closed, err := client.CloseTenantSpace(ctx, &pb.CloseTenantSpaceRequest{TenantId: tenant, OperationId: op})
	cancel()
	if err != nil || closed.GetSpace().GetPhase() != "closed" {
		t.Fatalf("close: %v %v", closed, err)
	}
}
