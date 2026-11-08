package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	pb "github.com/zhangzhe-ctrl/ani-resource-service/api/clusterops/v1"
	networkpb "github.com/zhangzhe-ctrl/ani-resource-service/api/network/v1"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/clusterops"
	conf "github.com/zhangzhe-ctrl/ani-resource-service/internal/conf/v1"
	service "github.com/zhangzhe-ctrl/ani-resource-service/internal/service/clusterops"
	"github.com/zhangzhe-ctrl/ani-resource-service/tests/testenv"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/emptypb"
	"io"
	"log/slog"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func clusterOpsMD() metadata.MD {
	return metadata.Pairs("x-ani-scope", "platform", "x-ani-actor", "governance:user:7", "x-ani-request-id", callRulesRequest)
}
func clusterOpsContext() context.Context {
	return metadata.NewOutgoingContext(context.Background(), clusterOpsMD())
}
func TestClusterOpsExactDelegation(t *testing.T) {
	cert := &x509.Certificate{DNSNames: []string{GovernanceSAN}}
	base := peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{Version: tls.VersionTLS13, PeerCertificates: []*x509.Certificate{cert}, VerifiedChains: [][]*x509.Certificate{{cert}}}}})
	if len(clusterOpsPlatformMethods) != 10 {
		t.Fatal("unary allowlist differs from contract")
	}
	for method := range clusterOpsPlatformMethods {
		called := false
		_, err := governanceUnary(false)(metadata.NewIncomingContext(base, clusterOpsMD()), &emptypb.Empty{}, &grpc.UnaryServerInfo{FullMethod: method}, func(ctx context.Context, _ any) (any, error) { called = true; return nil, biz.RequirePlatform(ctx) })
		if err != nil || !called {
			t.Fatal(method, err)
		}
	}
	for _, tc := range []struct {
		md       metadata.MD
		method   string
		readOnly bool
	}{
		{metadata.Pairs("x-ani-tenant-id", callRulesTenant, "x-ani-actor", "governance:user:7", "x-ani-request-id", callRulesRequest), pb.PlatformClusterOpsService_ListClusters_FullMethodName, false},
		{metadata.Join(clusterOpsMD(), metadata.Pairs("x-ani-tenant-id", "")), pb.PlatformClusterOpsService_ListClusters_FullMethodName, false},
		{metadata.Pairs("x-ani-scope", "platform", "x-ani-actor", "governance:access-key:7", "x-ani-request-id", callRulesRequest), pb.PlatformClusterOpsService_ListClusters_FullMethodName, false},
		{metadata.Join(clusterOpsMD(), metadata.Pairs("x-ani-scope", "platform")), pb.PlatformClusterOpsService_ListClusters_FullMethodName, false},
		{clusterOpsMD(), "/clusterops.v1.PlatformClusterOpsService/Unknown", false},
		{clusterOpsMD(), pb.PlatformClusterOpsService_ListClusters_FullMethodName, true},
	} {
		called := false
		_, err := governanceUnary(tc.readOnly)(metadata.NewIncomingContext(base, tc.md), &emptypb.Empty{}, &grpc.UnaryServerInfo{FullMethod: tc.method}, func(context.Context, any) (any, error) { called = true; return nil, nil })
		if err == nil || called {
			t.Fatal("invalid delegation reached clusterops", tc, err)
		}
	}
}

type slowClusterOpsProbe struct {
	pb.UnimplementedPlatformClusterOpsServiceServer
}

func (slowClusterOpsProbe) ListClusters(ctx context.Context, _ *emptypb.Empty) (*pb.ListClustersResponse, error) {
	if err := biz.RequirePlatform(ctx); err != nil {
		return nil, status.Error(codes.PermissionDenied, "caller missing")
	}
	select {
	case <-ctx.Done():
		return nil, status.FromContextError(ctx.Err()).Err()
	case <-time.After(1150 * time.Millisecond):
		return &pb.ListClustersResponse{}, nil
	}
}

type oldBudgetProbe struct {
	networkpb.UnimplementedNetworkServiceServer
}

func (oldBudgetProbe) GetVPC(ctx context.Context, _ *networkpb.GetVPCRequest) (*networkpb.GetVPCResponse, error) {
	select {
	case <-ctx.Done():
		return nil, status.FromContextError(ctx.Err()).Err()
	case <-time.After(1150 * time.Millisecond):
		return &networkpb.GetVPCResponse{}, nil
	}
}
func clusterOpsListener(t *testing.T, readOnly bool, handler pb.PlatformClusterOpsServiceServer, logger *slog.Logger) *grpc.ClientConn {
	t.Helper()
	fixture := testenv.NewServiceTLS(t)
	tlsConfig, err := GovernanceTLS(fixture.CAFile, fixture.CertFile, fixture.KeyFile)
	if err != nil {
		t.Fatal(err)
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	gs, err := NewGRPCServer(&conf.Server_GRPC{Network: "tcp", Addr: "127.0.0.1:0", Timeout: durationpb.New(time.Second)}, NewReadiness(), tlsConfig, readOnly, requestLogging(logger))
	if err != nil {
		t.Fatal(err)
	}
	pb.RegisterPlatformClusterOpsServiceServer(gs, handler)
	networkpb.RegisterNetworkServiceServer(gs, oldBudgetProbe{})
	endpoint, err := gs.Endpoint()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- gs.Start(context.Background()) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = gs.Stop(ctx)
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("listener did not stop")
		}
	})
	conn, err := grpc.NewClient(endpoint.Host, grpc.WithTransportCredentials(credentials.NewTLS(fixture.Client)), grpc.WithInitialWindowSize(65535), grpc.WithInitialConnWindowSize(65535))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}
func TestClusterOpsRealGRPCMethodBudgets(t *testing.T) {
	conn := clusterOpsListener(t, false, slowClusterOpsProbe{}, nil)
	start := time.Now()
	if _, err := pb.NewPlatformClusterOpsServiceClient(conn).ListClusters(clusterOpsContext(), &emptypb.Empty{}); err != nil || time.Since(start) < time.Second {
		t.Fatal("clusterops inherited old 1s timeout", err, time.Since(start))
	}
	tenant := metadata.NewOutgoingContext(context.Background(), metadata.Pairs("x-ani-tenant-id", callRulesTenant, "x-ani-actor", "governance:user:7", "x-ani-request-id", callRulesRequest))
	if _, err := networkpb.NewNetworkServiceClient(conn).GetVPC(tenant, &networkpb.GetVPCRequest{TenantId: callRulesTenant}); status.Code(err) != codes.DeadlineExceeded {
		t.Fatal("old method lost configured budget", err)
	}
	short, cancel := context.WithTimeout(clusterOpsContext(), 25*time.Millisecond)
	defer cancel()
	if _, err := pb.NewPlatformClusterOpsServiceClient(conn).ListClusters(short, &emptypb.Empty{}); status.Code(err) != codes.DeadlineExceeded {
		t.Fatal(err)
	}
	readonly := clusterOpsListener(t, true, slowClusterOpsProbe{}, nil)
	if _, err := networkpb.NewNetworkServiceClient(readonly).GetVPC(tenant, &networkpb.GetVPCRequest{TenantId: callRulesTenant}); status.Code(err) != codes.DeadlineExceeded {
		t.Fatal("read-only mode lost old budget", err)
	}
	if _, err := pb.NewPlatformClusterOpsServiceClient(readonly).ListClusters(clusterOpsContext(), &emptypb.Empty{}); status.Code(err) != codes.PermissionDenied {
		t.Fatal("read-only mode expanded", err)
	}
}

type streamKube struct {
	biz.KubernetesPort
	reader *streamReader
}

func (k streamKube) OpenPodLogs(context.Context, biz.PodLogQuery) (biz.OpenedPodLog, error) {
	return biz.OpenedPodLog{Reader: k.reader, PodUID: "pod-uid", Container: "main"}, nil
}

type streamReader struct {
	remaining int
	closed    chan struct{}
	once      sync.Once
}

func (r *streamReader) Read(b []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, io.EOF
	}
	n := len(b)
	if n > r.remaining {
		n = r.remaining
	}
	for i := range b[:n] {
		b[i] = 'x'
	}
	r.remaining -= n
	return n, nil
}
func (r *streamReader) Close() error { r.once.Do(func() { close(r.closed) }); return nil }

type streamProbe struct {
	pb.UnimplementedPlatformClusterOpsServiceServer
	actual                      *service.Service
	handlerDone, senderReturned chan struct{}
	once                        sync.Once
	active, maxActive           atomic.Int32
	result                      chan error
}

func (p *streamProbe) StreamPodLogs(q *pb.StreamPodLogsRequest, s pb.PlatformClusterOpsService_StreamPodLogsServer) error {
	defer close(p.handlerDone)
	err := p.actual.StreamPodLogs(q, &observedLogStream{PlatformClusterOpsService_StreamPodLogsServer: s, probe: p})
	p.result <- err
	return err
}

type observedLogStream struct {
	pb.PlatformClusterOpsService_StreamPodLogsServer
	probe *streamProbe
}

func (s *observedLogStream) Send(f *pb.PodLogFrame) error {
	active := s.probe.active.Add(1)
	defer s.probe.active.Add(-1)
	if active > s.probe.maxActive.Load() {
		s.probe.maxActive.Store(active)
	}
	err := s.PlatformClusterOpsService_StreamPodLogsServer.Send(f)
	if err != nil {
		s.probe.once.Do(func() { close(s.probe.senderReturned) })
	}
	return err
}
func TestClusterOpsRealGRPCSlowClientReleasesHandlerReaderSlotAndSender(t *testing.T) {
	reader := &streamReader{remaining: 8 * 1024 * 1024, closed: make(chan struct{})}
	uc, err := biz.NewUseCase("cluster", streamKube{reader: reader}, biz.NewNamespacePolicy(biz.UnavailableNamespaceSource{}, biz.UnavailableNamespaceSource{}))
	if err != nil {
		t.Fatal(err)
	}
	probe := &streamProbe{actual: service.NewService(uc), handlerDone: make(chan struct{}), senderReturned: make(chan struct{}), result: make(chan error, 1)}
	conn := clusterOpsListener(t, false, probe, nil)
	ctx, cancel := context.WithTimeout(clusterOpsContext(), 23*time.Second)
	defer cancel()
	stream, err := pb.NewPlatformClusterOpsServiceClient(conn).StreamPodLogs(ctx, &pb.StreamPodLogsRequest{ClusterId: "cluster", Namespace: "tenant", PodName: "pod", Follow: true})
	if err != nil {
		t.Fatal(err)
	}
	first, err := stream.Recv()
	if err != nil || first.Type != pb.PodLogFrameType_POD_LOG_FRAME_TYPE_READY {
		t.Fatal(first, err)
	}
	// Stop Recv without canceling the RPC or closing the connection. The finite
	// 8MiB source exceeds the fixed HTTP/2 windows and blocks the unique Send.
	select {
	case <-probe.handlerDone:
	case <-time.After(19 * time.Second):
		t.Fatal("slow client held handler")
	}
	if ctx.Err() != nil {
		t.Fatal("test client ended the RPC before server cleanup")
	}
	err = <-probe.result
	if status.Code(err) != codes.DeadlineExceeded || !strings.Contains(err.Error(), "LOG_SEND_TIMEOUT") {
		t.Fatal(err)
	}
	select {
	case <-reader.closed:
	default:
		t.Fatal("Kubernetes reader remained open")
	}
	// Inspect the internal semaphore in this transport test without adding a
	// production diagnostic method or a configurable log lifetime.
	if n := reflect.ValueOf(uc).Elem().FieldByName("logSlots").Len(); n != 0 {
		t.Fatal("log slot retained", n)
	}
	select {
	case <-probe.senderReturned:
	case <-time.After(time.Second):
		t.Fatal("Send did not exit when handler closed underlying stream")
	}
	deadline := time.Now().Add(time.Second)
	for {
		stack := make([]byte, 1024*1024)
		n := runtime.Stack(stack, true)
		if !bytes.Contains(stack[:n], []byte("clusterops.(*UseCase).StreamPodLogs.func")) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("clusterops log goroutine survived stream closure")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if probe.maxActive.Load() != 1 {
		t.Fatal("concurrent Send", probe.maxActive.Load())
	}
}
func TestClusterOpsRequestLoggingRedactsBody(t *testing.T) {
	var logs bytes.Buffer
	conn := clusterOpsListener(t, false, slowClusterOpsProbe{}, slog.New(slog.NewJSONHandler(&logs, nil)))
	err := conn.Invoke(clusterOpsContext(), pb.PlatformClusterOpsService_CreateResource_FullMethodName, &pb.CreateResourceRequest{ManifestYaml: "private-manifest-sentinel"}, &pb.MutationResponse{})
	if status.Code(err) != codes.Unimplemented {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "private-manifest-sentinel") {
		t.Fatal("manifest entered ordinary logs")
	}
}
