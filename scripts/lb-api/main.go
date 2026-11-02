// lb-api is an explicit, local-only acceptance fixture. It supplies a fixed
// caller context, then invokes the actual Network transport and use cases.
// It never runs a worker, mutates product CRs, or supplies readiness facts.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/go-kratos/kratos/v3/config"
	"github.com/go-kratos/kratos/v3/config/env"
	"github.com/go-kratos/kratos/v3/config/file"
	"github.com/jackc/pgx/v5"
	networkv1 "github.com/zhangzhe-ctrl/ani-resource-service/api/network/v1"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	conf "github.com/zhangzhe-ctrl/ani-resource-service/internal/conf/v1"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/service/network"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: lb-api serve|call|owner|installation [flags]")
	}
	switch args[0] {
	case "installation":
		return installation(args[1:])
	case "serve":
		return serve(args[1:])
	case "call":
		return call(args[1:])
	case "owner":
		return serveOwner(args[1:])
	default:
		return errors.New("unknown mode")
	}
}
func localSocket(path string) (net.Listener, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("socket must be an absolute path")
	}
	parent := filepath.Dir(path)
	resolved, err := filepath.EvalSymlinks(parent)
	if err != nil || resolved != parent {
		return nil, errors.New("socket directory must exist without symlinks")
	}
	info, err := os.Stat(parent)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("socket directory must be private (0700)")
	}
	if _, err = os.Lstat(path); !os.IsNotExist(err) {
		return nil, errors.New("socket path already exists or cannot be inspected; do not overwrite it")
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, errors.New("cannot create isolated socket")
	}
	if err = os.Chmod(path, 0600); err != nil {
		listener.Close()
		return nil, err
	}
	return listener, nil
}
func fixedCaller(tenant string, platform bool) grpc.UnaryServerInterceptor {
	caller := biz.EgressCaller{TenantID: tenant, PlatformAdministrator: platform, Attribution: biz.Attribution{Actor: "net-vpc-lb-02-fixture", DirectCaller: "private-unix-socket"}}
	return func(ctx context.Context, request any, _ *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
		// The legacy internal NetworkService accepts an explicit tenant field.
		// Bind that field to this socket's identity too, before the real service.
		if message, ok := request.(proto.Message); ok && !platform {
			value := message.ProtoReflect()
			if field := value.Descriptor().Fields().ByName("tenant_id"); field != nil {
				target := value.Get(field).String()
				if target != "" && target != tenant {
					return nil, status.Error(codes.PermissionDenied, "fixture socket tenant mismatch")
				}
				value.Set(field, protoreflect.ValueOfString(tenant))
			}
		}
		return next(biz.WithEgressCaller(ctx, caller), request)
	}
}
func serve(args []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	path := flags.String("socket", "", "absolute socket path in a private directory")
	cfg := flags.String("conf", "", "isolated instance configuration")
	tenant := flags.String("tenant", "", "one fixed tenant UUID")
	platform := flags.Bool("platform", false, "register only the platform service with an explicit fixed administrator context")
	dbName := flags.String("database", "", "exact isolated database name, starting with net_vpc_lb_02_")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *cfg == "" || !strings.HasPrefix(*dbName, "net_vpc_lb_02_") || len(*dbName) <= len("net_vpc_lb_02_") {
		return errors.New("isolated configuration and exact net_vpc_lb_02_ database are required")
	}
	if *platform {
		if *tenant != "" {
			return errors.New("platform and tenant sockets are separate")
		}
	} else if parsed, err := biz.ParseTenant(*tenant); err != nil || parsed != *tenant {
		return errors.New("canonical fixed tenant UUID required")
	}
	c := config.New(config.WithSource(file.NewSource(*cfg), env.NewSource("ANI")))
	defer c.Close()
	if err := c.Load(); err != nil {
		return errors.New("cannot load isolated configuration")
	}
	var bc conf.Bootstrap
	if err := c.Scan(&bc); err != nil {
		return errors.New("cannot parse isolated configuration")
	}
	if err := bc.Validate(); err != nil {
		return err
	}
	if bc.Network.LoadBalancer == nil || !bc.Network.LoadBalancer.EnableIsolatedApi {
		return errors.New("enable_isolated_api must be explicitly true in this instance")
	}
	parsed, err := pgx.ParseConfig(bc.Network.DatabaseDsn)
	if err != nil || parsed.Database != *dbName {
		return errors.New("database does not match the explicit isolated target")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	repository, err := data.OpenPostgres(ctx, bc.Network.DatabaseDsn, data.Placement{ClusterID: bc.Network.ClusterId, NamespacePrefix: "tenant-"})
	if err != nil {
		return err
	}
	defer repository.Close()
	stale := bc.Network.Worker.StaleAfter.AsDuration()
	repository.UseBaseConnectivityFreshness(stale)
	provider, err := data.OpenKCProvider(repository, bc.Network.Kubeconfig, data.KCClientPolicy{QPS: 5, Burst: 10})
	if err != nil {
		return err
	}
	repository.UseEgressInfrastructure(provider)
	key, err := base64.StdEncoding.DecodeString(bc.Network.CursorSigningKey)
	if err != nil {
		return errors.New("invalid cursor key")
	}
	egress, err := biz.NewEgress(repository, provider, biz.ContextEgressAuthorization{}, key, stale, time.Now)
	if err != nil {
		return err
	}
	server := grpc.NewServer(grpc.UnaryInterceptor(fixedCaller(*tenant, *platform)))
	if *platform {
		networkv1.RegisterPlatformNetworkServiceServer(server, service.NewPlatformNetworkService(egress))
	} else {
		network, err := biz.NewNetwork(repository, key, stale, time.Now)
		if err != nil {
			return err
		}
		lbs, err := biz.NewLoadBalancers(repository, biz.ContextEgressAuthorization{}, key, stale, time.Now)
		if err != nil {
			return err
		}
		networkv1.RegisterNetworkServiceServer(server, service.NewNetworkService(network, biz.NewAttachments(repository, stale)))
		networkv1.RegisterTenantEgressServiceServer(server, service.NewTenantEgressService(egress))
		networkv1.RegisterTenantLoadBalancerServiceServer(server, service.NewTenantLoadBalancerService(lbs))
	}
	listener, err := localSocket(*path)
	if err != nil {
		return err
	}
	defer listener.Close()
	return runServer(server, listener)
}
func runServer(server *grpc.Server, listener net.Listener) error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		server.Stop()
		return nil
	}
}
func call(args []string) error {
	flags := flag.NewFlagSet("call", flag.ContinueOnError)
	target := flags.String("target", "", "unix:///absolute/socket or explicit loopback host:port")
	name := flags.String("service", "TenantLoadBalancerService", "fixed Network service name")
	method := flags.String("method", "", "RPC method")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if !strings.HasPrefix(*target, "unix:///") {
		host, _, err := net.SplitHostPort(*target)
		ip := net.ParseIP(host)
		if err != nil || ip == nil || !ip.IsLoopback() {
			return errors.New("call target must be a private unix socket or literal loopback address")
		}
	}
	switch *name {
	case "NetworkService", "TenantLoadBalancerService", "TenantEgressService", "PlatformNetworkService", "InstanceNetworkConsumerService":
	default:
		return errors.New("unknown Network service")
	}
	descriptor, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName("network.v1." + *name))
	if err != nil {
		return errors.New("service descriptor unavailable")
	}
	rpc := descriptor.(protoreflect.ServiceDescriptor).Methods().ByName(protoreflect.Name(*method))
	if rpc == nil || rpc.IsStreamingClient() || rpc.IsStreamingServer() {
		return errors.New("unknown unary method")
	}
	body, err := io.ReadAll(io.LimitReader(os.Stdin, (1<<20)+1))
	if err != nil || len(body) > 1<<20 {
		return errors.New("request exceeds 1 MiB or cannot be read")
	}
	request, response := dynamicpb.NewMessage(rpc.Input()), dynamicpb.NewMessage(rpc.Output())
	if err = protojson.Unmarshal(body, request); err != nil {
		return err
	}
	conn, err := grpc.NewClient(*target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err = conn.Invoke(ctx, "/"+string(descriptor.FullName())+"/"+*method, request, response)
	result := map[string]any{"code": status.Code(err).String()}
	marshal := protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: true}
	if err != nil {
		body, _ = marshal.Marshal(status.Convert(err).Proto())
		result["status"] = json.RawMessage(body)
	} else {
		body, err = marshal.Marshal(response)
		if err != nil {
			return err
		}
		result["response"] = json.RawMessage(body)
	}
	if e := json.NewEncoder(os.Stdout).Encode(result); e != nil {
		return e
	}
	if err != nil {
		return errors.New("RPC failed; structured status is in stdout")
	}
	return nil
}

// Owner mode is a separate, controlled instance-owner protocol endpoint. Its
// registry contains owner facts only; Network still verifies Kubernetes UIDs.
type registryOwner struct {
	networkv1.UnimplementedInstanceNetworkConsumerServiceServer
	path string
}

func (o *registryOwner) GetSubmission(_ context.Context, r *networkv1.GetSubmissionRequest) (*networkv1.GetSubmissionResponse, error) {
	body, err := os.ReadFile(o.path)
	if err != nil {
		return nil, status.Error(codes.Unavailable, "owner registry unavailable")
	}
	var entries []json.RawMessage
	if len(body) > 1<<20 || json.Unmarshal(body, &entries) != nil {
		return nil, status.Error(codes.Unavailable, "owner registry invalid")
	}
	var result *networkv1.GetSubmissionResponse
	for _, entry := range entries {
		item := &networkv1.GetSubmissionResponse{}
		if protojson.Unmarshal(entry, item) != nil {
			return nil, status.Error(codes.Unavailable, "owner registry invalid")
		}
		if r.ProtocolVersion == 1 && item.ProtocolVersion == 1 && item.TenantId == r.TenantId && item.InstanceId == r.InstanceId && item.AttachmentId == r.AttachmentId && item.SubmissionId == r.SubmissionId && item.Generation == r.Generation {
			if result != nil {
				return nil, status.Error(codes.Unavailable, "ambiguous owner registry")
			}
			if r.ExpectedFinalizationId != "" && item.FinalizationId != r.ExpectedFinalizationId {
				return nil, status.Error(codes.FailedPrecondition, "owner finalization differs")
			}
			result = item
		}
	}
	if result == nil {
		return nil, status.Error(codes.NotFound, "owner submission not found")
	}
	return result, nil
}
func serveOwner(args []string) error {
	flags := flag.NewFlagSet("owner", flag.ContinueOnError)
	path := flags.String("socket", "", "private absolute owner socket")
	registry := flags.String("registry", "", "owner-maintained JSON array of GetSubmissionResponse facts")
	if err := flags.Parse(args); err != nil {
		return err
	}
	info, err := os.Lstat(*registry)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return errors.New("owner registry must be a private regular file")
	}
	listener, err := localSocket(*path)
	if err != nil {
		return err
	}
	defer listener.Close()
	server := grpc.NewServer()
	networkv1.RegisterInstanceNetworkConsumerServiceServer(server, &registryOwner{path: *registry})
	return runServer(server, listener)
}
