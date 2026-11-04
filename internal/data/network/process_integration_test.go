package data_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	networkv1 "github.com/zhangzhe-ctrl/ani-resource-service/api/network/v1"
	controlled "github.com/zhangzhe-ctrl/ani-resource-service/tests/net05a/provider"
	"github.com/zhangzhe-ctrl/ani-resource-service/tests/testenv"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

func TestServiceProcessesRecoverT1AndProviderSuccessWithoutCaller(t *testing.T) {
	if os.Getenv("NETWORK_TEST_ADMIN_DSN") == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "network")
	build := exec.Command("go", "build", "-race", "-trimpath", "-o", binary, "./cmd/ani-resource-service")
	build.Dir = root
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build recovery process: %v\n%s", err, output)
	}
	for _, fault := range []string{"after-T1", "after-provider-success"} {
		t.Run(fault, func(t *testing.T) {
			fixture := testenv.NewDatabase(t)
			signingKey := testenv.SigningKey()
			// Both creation crash points must recover independently. Once available,
			// they share the same deletion path, so exercise its crashes and CLI
			// migration replay once rather than repeating them for each create fault.
			exerciseDeleteRecovery := fault == "after-provider-success"
			if exerciseDeleteRecovery {
				migration := exec.Command(binary, "-migrate")
				migration.Env = append(os.Environ(), "ANI_NETWORK_MIGRATION_DSN="+fixture.Owner.Config().ConnString(), "ANI_NETWORK_RUNTIME_ROLE="+fixture.RuntimeRole)
				if output, err := migration.CombinedOutput(); err != nil {
					t.Fatalf("explicit migration replay failed: %v %s", err, output)
				}
			}
			api := &controlledKC{}
			platform := controlled.New()
			renewFacts := seedProcessVPCPlatformFacts(t, platform)
			config := processVPCPresetConfig(t, root, "10.42.0.0/16")
			reached := make(chan struct{})
			release := make(chan struct{})
			var captured atomic.Bool
			deleteReached := make(chan struct{})
			deleteRelease := make(chan struct{})
			var capturedDelete atomic.Bool
			host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				renewFacts(r)
				if r.Method == "GET" {
					switch r.URL.Path {
					case "/api/v1/nodes", "/api/v1/configmaps", "/api/v1/pods", "/api/v1/namespaces/kcn-system/configmaps", "/api/v1/namespaces/kcn-system/pods", "/apis/networking.k8s.io/v1/servicecidrs", "/apis/networking.kubercloud.com/v1/subnets":
						platform.ServeHTTP(w, r)
						return
					}
				}
				isObject := strings.Contains(r.URL.Path, "/vpcs/")
				captureCreate := (fault == "after-T1" && r.Method == "GET" && isObject) || (fault == "after-provider-success" && r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/vpcs"))
				if captureCreate && captured.CompareAndSwap(false, true) {
					recorder := httptest.NewRecorder()
					api.ServeHTTP(recorder, r)
					close(reached)
					<-release
					for key, values := range recorder.Header() {
						w.Header()[key] = values
					}
					w.WriteHeader(recorder.Code)
					_, _ = w.Write(recorder.Body.Bytes())
					return
				}
				if exerciseDeleteRecovery && r.Method == "DELETE" && isObject && capturedDelete.CompareAndSwap(false, true) {
					recorder := httptest.NewRecorder()
					api.ServeHTTP(recorder, r)
					close(deleteReached)
					<-deleteRelease
					for key, values := range recorder.Header() {
						w.Header()[key] = values
					}
					w.WriteHeader(recorder.Code)
					_, _ = w.Write(recorder.Body.Bytes())
					return
				}
				api.ServeHTTP(w, r)
			}))
			// Processes register cleanup below; stop them before closing their
			// long-lived watch server, including when an assertion fails early.
			t.Cleanup(host.Close)
			defer close(release)
			defer close(deleteRelease)
			kubeconfig := testenv.Kubeconfig(t, host.URL)
			start := func() *networkProcess {
				return startNetworkProcess(t, root, binary, fixture.RuntimeDSN, kubeconfig, signingKey, "config="+config)
			}
			first := start()
			tenant := uuid.NewString()
			client, closeClient := first.client(t, tenant)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			accepted, err := client.CreateVPC(ctx, &networkv1.CreateVPCRequest{TenantId: tenant, Name: fault, Cidr: "10.42.0.0/16", IdempotencyKey: "restart"})
			cancel()
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-reached:
			case <-time.After(3 * time.Second):
				t.Fatal("process did not reach selected crash point")
			}
			first.kill(t)
			closeClient() // No Gateway/Core or original requester remains.
			release <- struct{}{}
			api.mu.Lock()
			createCountBefore := api.creates
			api.mu.Unlock()
			second := start()
			contender := start() // A distinct service process shares the same PG lease authority.
			resumed, closeResumed := second.client(t, tenant)
			defer closeResumed()
			current := waitRPCState(t, resumed, tenant, accepted.Vpc.Id, networkv1.ResourceState_RESOURCE_STATE_AVAILABLE)
			if current.LastOperationId != accepted.Vpc.LastOperationId {
				t.Fatal("restart changed operation identity")
			}
			api.mu.Lock()
			creates := api.creates
			api.mu.Unlock()
			if creates != 1 {
				t.Fatalf("provider success replay created %d objects", creates)
			}
			contender.stop(t)
			ctx, cancel = context.WithTimeout(context.Background(), 3*time.Second)
			deleting, err := resumed.DeleteVPC(ctx, &networkv1.DeleteVPCRequest{TenantId: tenant, VpcId: current.Id})
			cancel()
			if err != nil {
				t.Fatal(err)
			}
			cleanup := resumed
			deletionProcess := second
			if exerciseDeleteRecovery {
				select {
				case <-deleteReached:
				case <-time.After(3 * time.Second):
					output, _ := os.ReadFile(second.logPath)
					t.Fatalf("process never issued DELETE; process trace: %s", output)
				}
				second.kill(t)
				deleteRelease <- struct{}{}
				deletionProcess = start()
				var closeCleanup func()
				cleanup, closeCleanup = deletionProcess.client(t, tenant)
				defer closeCleanup()
			}
			deleted := waitRPCState(t, cleanup, tenant, current.Id, networkv1.ResourceState_RESOURCE_STATE_DELETED)
			ctx, cancel = context.WithTimeout(context.Background(), 3*time.Second)
			completed, err := cleanup.GetOperation(ctx, &networkv1.GetOperationRequest{TenantId: tenant, OperationId: deleting.Vpc.LastOperationId})
			cancel()
			if err != nil || completed.Operation.State != networkv1.OperationState_OPERATION_STATE_SUCCEEDED {
				t.Fatalf("delete did not complete: %v %v", completed, err)
			}
			if deleted.LastOperationId != deleting.Vpc.LastOperationId {
				t.Fatal("delete recovery changed operation identity")
			}
			var parked bool
			if err = fixture.Owner.QueryRow(context.Background(), `SELECT retired AND next_run_at='infinity'::timestamptz FROM network_reconciliations WHERE tenant_id=$1 AND vpc_id=$2`, tenant, current.Id).Scan(&parked); err != nil || !parked {
				t.Fatal("completed deletion did not retire its durable task", parked, err)
			}
			deletionProcess.stop(t)
			t.Logf("%s: VPC=%s create_op=%s create_count_before_crash=%d final_creates=%d delete_op=%s; independent service processes recovered creation and deletion using the same durable identities", fault, current.Id, accepted.Vpc.LastOperationId, createCountBefore, creates, deleting.Vpc.LastOperationId)
		})
	}
}

// These positive process tests declare their own administrator-selected
// presets. Other callers' configuration and the formal example stay unchanged.
func processVPCPresetConfig(t *testing.T, root string, presets ...string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(root, "configs/config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	const empty = "vpc_cidr_presets: []"
	if len(presets) == 0 || strings.Count(string(body), empty) != 1 {
		t.Fatal("positive process fixture requires explicit presets and the unchanged example config")
	}
	values, err := json.Marshal(presets)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(strings.Replace(string(body), empty, "vpc_cidr_presets: "+string(values), 1)), 0600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// Only the existing external Kubernetes boundary is controlled. The actual
// adapter, preset admission, conflict checks and SQL remain production code.
func seedProcessVPCPlatformFacts(t *testing.T, api *controlled.Server) func(*http.Request) {
	t.Helper()
	const nodeName = "process-platform-node"
	nodeUID := uuid.NewString()
	api.Change("nodes", map[string]any{"apiVersion": "v1", "kind": "Node", "metadata": map[string]any{"name": nodeName, "uid": nodeUID}, "status": map[string]any{"addresses": []any{map[string]any{"type": "InternalIP", "address": "172.16.101.10"}}}}, false)
	api.Change("servicecidrs", map[string]any{"apiVersion": "networking.k8s.io/v1", "kind": "ServiceCIDR", "metadata": map[string]any{"name": "kubernetes", "uid": uuid.NewString()}, "spec": map[string]any{"cidrs": []any{"10.96.0.0/12"}}}, false)
	api.Change("pods", map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{"name": "process-platform-controller", "namespace": "kcn-system", "uid": uuid.NewString(), "labels": map[string]any{"networking.kubercloud.com/app": "controller"}}, "spec": map[string]any{"containers": []any{map[string]any{"name": "controller", "command": []any{"/kcn/controller"}, "args": []any{"--default-subnet=kcn-default"}}}}}, false)
	api.Change("subnets", map[string]any{"apiVersion": "networking.kubercloud.com/v1", "kind": "Subnet", "metadata": map[string]any{"namespace": "kcn-system", "name": "kcn-default", "uid": uuid.NewString()}, "spec": map[string]any{"type": "VPC", "cidrBlock": "10.16.0.0/16", "gateway": "kcn-cluster"}}, false)
	return func(r *http.Request) {
		if strings.Contains(r.URL.Query().Get("labelSelector"), "ani-network-node-facts") {
			t.Error("process attempted to read removed node facts")
		}
	}
}

type networkProcess struct {
	command                   *exec.Cmd
	done                      chan error
	exited                    bool
	grpcAddress, adminAddress string
	logPath                   string
	serviceTLS                *testenv.ServiceTLS
	legacyPlaintext           bool
}

func freeAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	return address
}
func startNetworkProcess(t *testing.T, root, binary, dsn, kubeconfig, signingKey string, resourceKinds ...string) *networkProcess {
	t.Helper()
	grpcAddress := freeAddress(t)
	adminAddress := freeAddress(t)
	legacyPlaintext := false
	dependentResources := false
	for adminAddress == grpcAddress {
		adminAddress = freeAddress(t)
	}
	for _, option := range resourceKinds {
		if option == "subnet" || option == "egress" || option == "base" || option == "load_balancer" {
			dependentResources = true
		}
		if option == "legacy-plaintext" {
			// Only the fixed schema-compatible pre-contract executable in the
			// rollback fixture keeps its original transport. Candidates use mTLS.
			if filepath.Base(binary) != "baseline" {
				t.Fatal("legacy plaintext is reserved for the fixed baseline binary")
			}
			legacyPlaintext = true
		}
		if strings.HasPrefix(option, "grpc-address=") {
			grpcAddress = strings.TrimPrefix(option, "grpc-address=")
		}
		if strings.HasPrefix(option, "admin-address=") {
			adminAddress = strings.TrimPrefix(option, "admin-address=")
		}
	}
	logPath := filepath.Join(t.TempDir(), "process.log")
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	command := exec.Command(binary, "-conf", filepath.Join(root, "configs"))
	for _, option := range resourceKinds {
		if strings.HasPrefix(option, "config=") {
			command.Args[2] = strings.TrimPrefix(option, "config=")
		}
	}
	command.Dir = root
	command.Stdout = log
	command.Stderr = log
	// No ambient ANI setting may silently alter this test's process contract.
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "ANI_") && !strings.HasPrefix(entry, "NETWORK_TEST_ADMIN_DSN=") {
			command.Env = append(command.Env, entry)
		}
	}
	command.Env = append(command.Env, "ANI_NETWORK_DATABASE_DSN="+dsn, "ANI_NETWORK_KUBECONFIG="+kubeconfig, "ANI_NETWORK_CLUSTER_ID=test-cluster",
		"ANI_NETWORK_CURSOR_SIGNING_KEY="+signingKey, "ANI_SERVER_GRPC_ADDR="+grpcAddress, "ANI_SERVER_ADMIN_ADDR="+adminAddress,
		"ANI_WORKER_LEASE=3s", "ANI_WORKER_REQUEST_TIMEOUT=1s", "ANI_WORKER_OBSERVE_EVERY=0.1s", "ANI_WORKER_STALE_AFTER=2s",
		"ANI_OBSERVATION_AUDIT_INTERVAL=0.2s", "ANI_OBSERVATION_AUDIT_JITTER=0.1s", "ANI_OBSERVATION_AUDIT_TIMEOUT=1s", "ANI_OBSERVATION_FLUSH_INTERVAL=0.02s", "ANI_OBSERVATION_REQUEST_QPS=40", "ANI_OBSERVATION_REQUEST_BURST=80",
		"ANI_WORKER_RETRY_MIN=0.02s", "ANI_WORKER_RETRY_MAX=0.05s", "ANI_WORKER_POLL_INTERVAL=0.01s", "ANI_SERVER_SHUTDOWN_TIMEOUT=3s")
	var serviceTLS *testenv.ServiceTLS
	if !legacyPlaintext {
		fixture := testenv.NewServiceTLS(t)
		serviceTLS = &fixture
		command.Env = append(command.Env,
			"ANI_NETWORK_CLIENT_CA="+fixture.CAFile,
			"ANI_NETWORK_TLS_CERT="+fixture.CertFile,
			"ANI_NETWORK_TLS_KEY="+fixture.KeyFile)
	}
	if dependentResources {
		// Multi-resource fixtures allow their own declaration requests a longer
		// budget while preserving the production lease/request timing contract.
		for i, entry := range command.Env {
			if strings.HasPrefix(entry, "ANI_WORKER_LEASE=") {
				command.Env[i] = "ANI_WORKER_LEASE=6s"
			}
			if strings.HasPrefix(entry, "ANI_WORKER_REQUEST_TIMEOUT=") {
				command.Env[i] = "ANI_WORKER_REQUEST_TIMEOUT=2s"
			}
			if strings.HasPrefix(entry, "ANI_WORKER_OBSERVE_EVERY=") {
				command.Env[i] = "ANI_WORKER_OBSERVE_EVERY=0.5s"
			}
			if strings.HasPrefix(entry, "ANI_WORKER_STALE_AFTER=") {
				command.Env[i] = "ANI_WORKER_STALE_AFTER=10s"
			}
		}
	}
	for _, option := range resourceKinds {
		if option == "load_balancer" {
			for i, entry := range command.Env {
				for key, value := range map[string]string{"ANI_WORKER_LEASE=": "12s", "ANI_WORKER_REQUEST_TIMEOUT=": "4s", "ANI_OBSERVATION_AUDIT_TIMEOUT=": "3s", "ANI_OBSERVATION_REQUEST_QPS=": "100", "ANI_OBSERVATION_REQUEST_BURST=": "200"} {
					if strings.HasPrefix(entry, key) {
						command.Env[i] = key + value
					}
				}
			}
		}
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	process := &networkProcess{command: command, done: make(chan error, 1), grpcAddress: grpcAddress, adminAddress: adminAddress, logPath: logPath, serviceTLS: serviceTLS, legacyPlaintext: legacyPlaintext}
	go func() { process.done <- command.Wait() }()
	t.Cleanup(func() {
		if !process.exited {
			process.kill(t)
		}
	})
	deadline := time.Now().Add(8 * time.Second)
	httpClient := &http.Client{Timeout: 100 * time.Millisecond}
	for time.Now().Before(deadline) {
		select {
		case err := <-process.done:
			process.exited = true
			content, _ := os.ReadFile(logPath)
			t.Fatalf("service exited before ready: %v\n%s", err, content)
		default:
		}
		response, err := httpClient.Get("http://" + adminAddress + "/readyz")
		if err == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			if response.StatusCode == 200 {
				return process
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	process.kill(t)
	content, _ := os.ReadFile(logPath)
	t.Fatalf("service never ready: %s", content)
	return nil
}
func (p *networkProcess) kill(t *testing.T) {
	t.Helper()
	if p.exited {
		return
	}
	_ = p.command.Process.Kill()
	<-p.done
	p.exited = true
}
func (p *networkProcess) stop(t *testing.T) {
	t.Helper()
	if p.exited {
		return
	}
	_ = p.command.Process.Signal(os.Interrupt)
	select {
	case err := <-p.done:
		p.exited = true
		if err != nil {
			content, _ := os.ReadFile(p.logPath)
			t.Fatalf("process stop failed: %v\n%s", err, content)
		}
	case <-time.After(5 * time.Second):
		p.kill(t)
		t.Fatal("process did not stop")
	}
}
func (p *networkProcess) client(t *testing.T, tenant string) (networkv1.NetworkServiceClient, func()) {
	t.Helper()
	parsed, err := uuid.Parse(tenant)
	if err != nil || parsed == uuid.Nil || parsed.String() != tenant {
		t.Fatal("process fixture client requires an explicit canonical trusted tenant")
	}
	options := []grpc.DialOption{grpc.WithDisableRetry(), grpc.WithDisableServiceConfig()}
	if p.legacyPlaintext {
		options = append(options, grpc.WithTransportCredentials(insecure.NewCredentials()))
	} else {
		if p.serviceTLS == nil {
			t.Fatal("candidate process fixture is missing mTLS credentials")
		}
		options = append(options, grpc.WithTransportCredentials(credentials.NewTLS(p.serviceTLS.Client.Clone())), grpc.WithUnaryInterceptor(func(ctx context.Context, method string, request, response any, conn *grpc.ClientConn, invoke grpc.UnaryInvoker, callOptions ...grpc.CallOption) error {
			requestID := uuid.NewString()
			const actor = "governance:user:7"
			attribution := &networkv1.Attribution{Actor: actor, DirectCaller: "ani-governance", CorrelationId: requestID}
			// These are test-owned requests delegated by the explicit fixture
			// principal. Scope is never inferred from incoming request fields.
			switch original := request.(type) {
			case *networkv1.CreateVPCRequest:
				if original != nil && original.Attribution == nil {
					delegated := proto.Clone(original).(*networkv1.CreateVPCRequest)
					delegated.Attribution = attribution
					request = delegated
				}
			case *networkv1.CreateSubnetRequest:
				if original != nil && original.Attribution == nil {
					delegated := proto.Clone(original).(*networkv1.CreateSubnetRequest)
					delegated.Attribution = attribution
					request = delegated
				}
			}
			ctx = metadata.NewOutgoingContext(ctx, metadata.Pairs("x-ani-tenant-id", tenant, "x-ani-actor", actor, "x-ani-request-id", requestID))
			if _, ok := ctx.Deadline(); !ok {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 3*time.Second)
				defer cancel()
			}
			return invoke(ctx, method, request, response, conn, callOptions...)
		}))
	}
	conn, err := grpc.NewClient(p.grpcAddress, options...)
	if err != nil {
		t.Fatal(err)
	}
	return networkv1.NewNetworkServiceClient(conn), func() { _ = conn.Close() }
}
func waitRPCState(t *testing.T, client networkv1.NetworkServiceClient, tenant, id string, state networkv1.ResourceState) *networkv1.VPC {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	last := ""
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		v, err := client.GetVPC(ctx, &networkv1.GetVPCRequest{TenantId: tenant, VpcId: id})
		cancel()
		if err == nil {
			if v.Vpc.State == state {
				return v.Vpc
			}
			last = fmt.Sprintf("state=%s reason=%s", v.Vpc.State, v.Vpc.Reason)
		} else {
			last = err.Error()
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("RPC never reached %s: %s", state, last)
	return nil
}
