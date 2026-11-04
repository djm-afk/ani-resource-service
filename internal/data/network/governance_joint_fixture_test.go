//go:build networkintegration

package data_test

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	networkv1 "github.com/zhangzhe-ctrl/ani-resource-service/api/network/v1"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	data "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network"
	runtime "github.com/zhangzhe-ctrl/ani-resource-service/internal/server"
	service "github.com/zhangzhe-ctrl/ani-resource-service/internal/service/network"
	controlled "github.com/zhangzhe-ctrl/ani-resource-service/tests/net05a/provider"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"k8s.io/client-go/tools/clientcmd"
)

// This subprocess is a test-only composition root. All admission, PostgreSQL,
// observations, lifecycle execution and mTLS caller checks are production code.
// Only kc/Envoy and the instance owner's submission boundary are controlled.
func TestNetworkGovernanceFixture(t *testing.T) {
	dir := os.Getenv("NETWORK_JOINT_FIXTURE_DIR")
	if dir == "" || os.Getenv("NETWORK_TEST_ADMIN_DSN") == "" {
		t.Fatal("task-private fixture directory and isolated PostgreSQL required")
	}
	controller := &lbControllerFixture{}
	switchBoundary := &networkJointSwitchBoundary{}
	securityGroupBoundary := &networkJointSecurityGroupBoundary{}
	f := newLBAdmissionFixture(t, func(w http.ResponseWriter, r *http.Request, api *controlled.Server) bool {
		if securityGroupBoundary.http(w, r, api) {
			return true
		}
		if switchBoundary.http(w, r, api) {
			return true
		}
		if r.Method == http.MethodGet && r.URL.Path == "/network-joint/lb-requests" {
			// Read-only observation of the external protocol boundary. Admission,
			// persistence and lifecycle execution still use production code.
			controller.mu.Lock()
			requests := append([]string{}, controller.requests...)
			controller.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(requests)
			return true
		}
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/subjectaccessreviews") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"apiVersion":"authorization.k8s.io/v1","kind":"SubjectAccessReview","status":{"allowed":true}}`))
			return true
		}
		if networkJointSubnetStatus(w, r, api) {
			return true
		}
		if networkJointSnatMutation(w, r, api) {
			return true
		}
		return controller.http(w, r, api)
	})
	addLBBackend(t, f, f.backendSubnet, "lb-second", "10.42.2.3")
	networkJointPlatformFacts(t, f.api)
	var boundNamespace string
	if err := f.f.owner.QueryRow(f.f.ctx, `SELECT namespace FROM network_provider_bindings WHERE tenant_id=$1 AND subnet_id=$2 AND resource_kind='subnet'`, f.f.tenant, f.subnet.ID).Scan(&boundNamespace); err != nil {
		t.Fatal(err)
	}
	f.api.Change("secrets", map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": "tenant-tls", "namespace": boundNamespace, "uid": uuid.NewString(), "labels": map[string]any{"network.ani.io/tenant-id": f.f.tenant}}, "type": "kubernetes.io/tls", "data": map[string]any{"tls.crt": "external-cert", "tls.key": "external-key"}}, false)
	// Replace the admission helper's U05 capability record with actual observation
	// of the external fixture before any Governance request can reach Resource.
	if _, err := f.f.owner.Exec(f.f.ctx, "DELETE FROM network_lb_capabilities WHERE cluster_id='test-cluster'"); err != nil {
		t.Fatal(err)
	}
	seedLBInstallation(f.api)
	cfg, err := clientcmd.BuildConfigFromFlags("", f.kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	cfg.QPS, cfg.Burst = 1000, 1000
	provider, err := data.NewKCProvider(f.f.p, cfg)
	if err != nil {
		t.Fatal(err)
	}
	f.f.p.UseLoadBalancerCapabilityReader(provider)
	observer, err := provider.EnableObservation(5 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	observed := make(chan error, 1)
	go func() { observed <- observer.Start(ctx) }()
	defer func() {
		cancel()
		if err := <-observed; err != nil {
			t.Error(err)
		}
	}()
	capabilities, err := f.f.p.GetPlatformCapabilities(ctx)
	if err != nil || !capabilities.LoadBalancer.Ready {
		t.Fatalf("GatewayClass diagnostic: %+v %v", capabilities.LoadBalancer, err)
	}
	policy := biz.DefaultWorkerPolicy()
	policy.ObserveEvery = 100 * time.Millisecond
	policy.RetryMin = 5 * time.Millisecond
	policy.RetryMax = 20 * time.Millisecond
	worker, err := biz.NewWorker(f.f.p, provider, uuid.NewString(), policy)
	if err != nil {
		t.Fatal(err)
	}
	f.f.w = worker
	// A dedicated HTTP scenario can run new VPCs without the optional base
	// network. Use the production rollout command; the already prepared LB
	// fixture and its Intranet association retain their normal lifecycle.
	withoutNewVPCBase := os.Getenv("NETWORK_JOINT_WITHOUT_NEW_VPC_BASE") == "1"
	if withoutNewVPCBase {
		rollout, err := f.f.p.SetNewVPCBaseConnectivity(f.f.ctx, false)
		if err != nil || rollout.NewVPCsEnabled {
			t.Fatalf("disable optional new-VPC base connectivity: %v", err)
		}
	}
	securityGroupWorker := biz.NewSecurityGroupWorker(f.f.p, provider, uuid.NewString(), policy)
	attachmentWorker := networkJointBackendObserver(t, f, provider, policy)
	aggregates := wireNetworkJointAggregates(t, f, provider, policy)
	ca, cert, key, clientCert, clientKey := networkJointCertificates(t, dir)
	tlsConfig, err := runtime.GovernanceTLS(ca, cert, key)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsConfig)), grpc.UnaryInterceptor(runtime.GovernanceUnary()), grpc.StreamInterceptor(runtime.DenyGovernanceStreams))
	networkv1.RegisterNetworkServiceServer(server, service.NewNetworkService(f.f.n))
	networkv1.RegisterNetworkSecurityGroupServiceServer(server, service.NewNetworkSecurityGroupService(biz.NewSecurityGroups(f.f.p)))
	networkv1.RegisterTenantEgressServiceServer(server, service.NewTenantEgressService(f.f.e))
	networkv1.RegisterPlatformNetworkServiceServer(server, service.NewPlatformNetworkService(f.f.e))
	networkv1.RegisterPlatformDefaultEgressServiceServer(server, aggregates.defaultService)
	networkv1.RegisterTenantLoadBalancerServiceServer(server, service.NewTenantLoadBalancerService(f.lbs))
	serving := make(chan error, 1)
	go func() { serving <- server.Serve(listener) }()
	defer func() {
		server.Stop()
		if err := <-serving; err != nil {
			t.Error(err)
		}
	}()
	stepping := make(chan error, 1)
	workerCtx, stopWorker := context.WithCancel(ctx)
	defer stopWorker()
	go func() {
		ticker := time.NewTicker(3 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-workerCtx.Done():
				stepping <- nil
				return
			case <-ticker.C:
				switchBoundary.refresh(f.api)
				for _, stepper := range append([]runtime.Stepper{worker, attachmentWorker, securityGroupWorker}, aggregates.steppers...) {
					if _, err := stepper.Step(workerCtx); err != nil && workerCtx.Err() == nil {
						stepping <- err
						return
					}
				}
			}
		}
	}()
	info := map[string]string{"ProviderURL": cfg.Host, "CertificateName": "tenant-tls", "Namespace": boundNamespace, "Address": listener.Addr().String(), "CAFile": ca, "CertFile": clientCert, "KeyFile": clientKey, "RuntimeDSN": f.runtimeDSN, "TenantID": f.f.tenant, "ParentVPC": f.vpc.ID, "EntrySubnet": f.subnet.ID, "BackendSubnet": f.backendSubnet.ID, "BackendAddress": "10.42.2.2", "SecondBackendAddress": "10.42.2.3"}
	info["ClusterID"], info["ClusterUID"] = aggregates.identity.ClusterID, aggregates.identity.ClusterUID
	info["DefaultVPCNamespace"], info["DefaultVPCName"], info["DefaultVPCUID"] = aggregates.identity.Namespace, aggregates.identity.Name, aggregates.identity.UID
	info["QualifiedPublicPoolID"], info["GatewayID"] = f.f.pool.ID, f.f.pool.Pool.GatewayID
	if withoutNewVPCBase {
		info["NewVPCBaseDisabled"] = "true"
	}
	raw, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "fixture.json.tmp"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(filepath.Join(dir, "fixture.json.tmp"), filepath.Join(dir, "fixture.json")); err != nil {
		t.Fatal(err)
	}
	t.Log("real Resource mTLS fixture ready; provider boundary is controlled")
	scan := bufio.NewScanner(os.Stdin)
	if !scan.Scan() || scan.Text() != "stop" {
		t.Error("fixture stopped without explicit product cleanup completion")
	}
	// Serving observes the live backend owners continuously. Cleanup switches to
	// the finalized owner protocol and drives the same production workers serially.
	stopWorker()
	if err := <-stepping; err != nil {
		t.Fatal(err)
	}
	// The HTTP suite deletes its tenant resources first. Internal instance-owner
	// fixture resources follow the production Attachment release protocol.
	attachments := biz.NewAttachments(f.f.p)
	closed := lbFinalizedOwners{}
	ids := []string{}
	for _, name := range []string{"lb-backend", "lb-second"} {
		var attachmentID string
		if err = f.f.owner.QueryRow(f.f.ctx, "SELECT attachment_id FROM network_attachments WHERE tenant_id=$1 AND instance_id=$2", f.f.tenant, name).Scan(&attachmentID); err != nil {
			t.Fatal(err)
		}
		a, err := attachments.Get(f.f.ctx, f.f.tenant, attachmentID)
		if err != nil {
			t.Fatal(err)
		}
		finalization := uuid.NewString()
		if _, err = attachments.Release(f.f.ctx, biz.ReleaseAttachment{TenantID: f.f.tenant, AttachmentID: a.ID, ExpectedVersion: a.Version, FinalizationID: finalization}); err != nil {
			t.Fatal(err)
		}
		for _, k := range []struct{ kind, name string }{{"pods", name}, {"vnics", name + "-nic"}, {"vnicips", name + "-ip"}} {
			if obj := f.api.Object(k.kind, a.Namespace, k.name); obj != nil {
				f.api.Change(k.kind, obj, true)
			}
		}
		value := consumerFor(a, "closed", finalization)
		closedAt := time.Now()
		value.ClosedAt = &closedAt
		value.PodUIDs = []string{a.PodUID}
		closed[a.ID] = value
		ids = append(ids, a.ID)
	}
	aw, err := biz.NewAttachmentWorker(f.f.p, closed, provider, uuid.NewString(), policy)
	if err != nil {
		t.Fatal(err)
	}
	awaitNET05A(t, 5*time.Second, func() bool {
		if _, err := aw.Step(f.f.ctx); err != nil {
			t.Fatal(err)
		}
		for _, id := range ids {
			v, err := attachments.Get(f.f.ctx, f.f.tenant, id)
			if err != nil || v.State != biz.Released {
				return false
			}
		}
		return true
	})
	for _, s := range []biz.Subnet{f.subnet, f.backendSubnet} {
		if _, err = f.f.n.DeleteSubnet(f.f.ctx, f.f.tenant, s.ID); err != nil {
			t.Fatal(err)
		}
		driveSubnet(t, f.f.n, worker, s, biz.Deleted)
	}
	if _, err = f.f.n.DeleteVPC(f.f.ctx, f.f.tenant, f.vpc.ID); err != nil {
		t.Fatal(err)
	}
	baseState(t, f.f, f.vpc.ID, biz.Deleted)
	// Continue the same production worker serially for platform retirement, so
	// continuous observations cannot invalidate the version between GET and
	// SetPool. Shared provider observation remains running through deletion.
	for _, kind := range []string{"public_pool", "intranet_pool", "egress_gateway"} {
		resources, next, _, err := f.f.e.ListPlatform(f.f.ctx, kind, biz.ListVPCs{Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		if next != "" {
			t.Fatal("unexpected platform fixture pagination")
		}
		for _, resource := range resources {
			// Admission, resource state and operation completion are separate.
			f.f.drive(t, func() bool {
				var err error
				resource, err = f.f.e.GetPlatform(f.f.ctx, kind, resource.ID)
				if err != nil {
					t.Fatal(err)
				}
				op, err := f.f.e.GetPlatformOperation(f.f.ctx, resource.LastOperationID)
				return err == nil && op.State == biz.Succeeded
			})
			if resource.AllocationEnabled {
				action := "set_pool_allocation"
				if kind == "intranet_pool" {
					action = "set_intranet_pool_allocation"
				}
				if _, err = f.f.e.SetPool(f.f.ctx, biz.PlatformIntent{Kind: action, ID: resource.ID, ExpectedVersion: resource.Version, IdempotencyKey: "joint-cleanup-close-" + resource.ID, Enabled: false}); err != nil {
					t.Fatal(err)
				}
			}
			deleted, err := f.f.e.DeletePlatform(f.f.ctx, kind, resource.ID)
			if err != nil {
				t.Fatal(err)
			}
			f.f.drive(t, func() bool {
				current, err := f.f.e.GetPlatform(f.f.ctx, kind, resource.ID)
				if err != nil || current.State != biz.Deleted {
					return false
				}
				op, err := f.f.e.GetPlatformOperation(f.f.ctx, deleted.LastOperationID)
				return err == nil && op.State == biz.Succeeded
			})
			t.Logf("Resource platform product cleanup pass: %s %s deleted", kind, resource.ID)
		}
	}
	// There is no clear-default command. Product retirement preserves default
	// references to deleted pool history; it closes allocation and retires the
	// pool slot. Do not erase those records outside the production contract.
	var active int
	if err = f.f.owner.QueryRow(f.f.ctx, `SELECT (SELECT count(*) FROM network_vpcs WHERE state<>'deleted')+(SELECT count(*) FROM network_subnets WHERE state<>'deleted')+(SELECT count(*) FROM network_eips WHERE state<>'deleted')+(SELECT count(*) FROM network_snat_bindings WHERE state<>'deleted')+(SELECT count(*) FROM network_load_balancers WHERE state<>'deleted')+(SELECT count(*) FROM network_attachments WHERE state<>'released')+(SELECT count(*) FROM network_platform_resources WHERE kind<>'device' AND state<>'deleted')+(SELECT count(*) FROM network_virtual_switches WHERE state<>'deleted')+(SELECT count(*) FROM network_default_vpc_egresses WHERE state<>'deleted')+(SELECT count(*) FROM network_security_groups WHERE state<>'deleted')+(SELECT count(*) FROM network_security_group_references WHERE NOT released)`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 0 {
		t.Fatalf("product cleanup left %d active tenant/platform resources", active)
	}
	var pendingDevice int
	if err = f.f.owner.QueryRow(f.f.ctx, `SELECT count(*) FROM network_platform_resources r JOIN network_platform_operations o ON o.operation_id=r.last_operation_id JOIN network_device_adoptions d ON d.resource_id=r.resource_id WHERE r.kind='device' AND NOT d.retired AND (r.state<>'available' OR o.state<>'succeeded')`).Scan(&pendingDevice); err != nil || pendingDevice != 0 {
		t.Fatalf("internal switch device must be fully registered before isolated fixture teardown: %d %v", pendingDevice, err)
	}
	t.Log("Resource product cleanup pass: no active runtime resources or aggregates; device registration history retained")
	cancel()
}

// The controlled kc backend updates the associated EIP in-place when a SNAT
// changes. Publish that external controller update with a new resourceVersion,
// just as a real status write does; otherwise the real observer can retain the
// pre-toggle EIP binding facts while the SNAT operation has already finished.
func networkJointSnatMutation(w http.ResponseWriter, r *http.Request, api *controlled.Server) bool {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 6 || parts[0] != "apis" || parts[1] != "networking.kubercloud.com" || parts[2] != "v1" || parts[3] != "namespaces" || parts[5] != "snats" || (r.Method != http.MethodPost && r.Method != http.MethodPatch && r.Method != http.MethodDelete) {
		return false
	}
	namespace, eipName := parts[4], ""
	if len(parts) > 6 {
		if existing := api.Object("snats", namespace, parts[6]); existing != nil {
			if spec, ok := existing["spec"].(map[string]any); ok {
				eipName, _ = spec["eip"].(string)
			}
		}
	}
	recorder := httptest.NewRecorder()
	api.ServeHTTP(recorder, r)
	if recorder.Code >= 200 && recorder.Code < 300 {
		if eipName == "" {
			var snat map[string]any
			if json.Unmarshal(recorder.Body.Bytes(), &snat) == nil {
				if spec, ok := snat["spec"].(map[string]any); ok {
					eipName, _ = spec["eip"].(string)
				}
			}
		}
		if eipName != "" {
			if eip := api.Object("eips", namespace, eipName); eip != nil {
				api.Change("eips", eip, false)
			}
		}
	}
	for key, values := range recorder.Header() {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(recorder.Code)
	_, _ = w.Write(recorder.Body.Bytes())
	return true
}

func networkJointCertificates(t *testing.T, dir string) (string, string, string, string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Network joint private CA"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	write := func(name, typ string, bytes []byte) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: bytes}), 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	caPath := write("ca.pem", "CERTIFICATE", der)
	issue := func(name string, usage x509.ExtKeyUsage, serial int64) (string, string) {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		c := &x509.Certificate{SerialNumber: big.NewInt(serial), DNSNames: []string{name}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage}}
		d, err := x509.CreateCertificate(rand.Reader, c, ca, &k.PublicKey, key)
		if err != nil {
			t.Fatal(err)
		}
		pk, err := x509.MarshalECPrivateKey(k)
		if err != nil {
			t.Fatal(err)
		}
		return write(fmt.Sprintf("%s.pem", name), "CERTIFICATE", d), write(fmt.Sprintf("%s.key", name), "EC PRIVATE KEY", pk)
	}
	cp, kp := issue("ani-network-service", x509.ExtKeyUsageServerAuth, 2)
	clientCP, clientKP := issue("ani-governance", x509.ExtKeyUsageClientAuth, 3)
	return caPath, cp, kp, clientCP, clientKP
}

// Standard Kubernetes status subresource, only in this external fixture. The
// production observer/worker still validates and persists every fact.
func networkJointSubnetStatus(w http.ResponseWriter, r *http.Request, api *controlled.Server) bool {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if r.Method != http.MethodPut || len(parts) != 8 || parts[0] != "apis" || parts[1] != "networking.kubercloud.com" || parts[3] != "namespaces" || parts[5] != "subnets" || parts[7] != "status" {
		return false
	}
	obj := api.Object("subnets", parts[4], parts[6])
	if obj == nil {
		http.Error(w, "not found", 404)
		return true
	}
	var input map[string]any
	if json.NewDecoder(r.Body).Decode(&input) != nil {
		http.Error(w, "invalid body", 400)
		return true
	}
	old := obj["metadata"].(map[string]any)
	m, _ := input["metadata"].(map[string]any)
	if m == nil || m["uid"] != old["uid"] || m["resourceVersion"] != old["resourceVersion"] {
		http.Error(w, "conflict", 409)
		return true
	}
	obj["status"] = input["status"]
	api.Change("subnets", obj, false)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(api.Object("subnets", parts[4], parts[6]))
	return true
}

// Only external Node/CNI observations are controlled here. Switch admission,
// authorization, PostgreSQL, fencing and worker behavior are production code.
func networkJointPlatformFacts(t *testing.T, api *controlled.Server) {
	t.Helper()
	config := api.Object("configmaps", "kcn-system", "kcn-config")
	if config == nil {
		t.Fatal("existing CNI configuration required")
	}
	node := map[string]any{
		"apiVersion": "v1", "kind": "Node",
		"metadata": map[string]any{"name": "node-1", "uid": uuid.NewString(),
			"annotations": map[string]any{"networking.kubercloud.com/managed_netdevs": `{"phys0":true}`, "networking.kubercloud.com/chassis": "joint-chassis-1"},
			"labels":      map[string]any{"networking.kubercloud.com/allocated": "true"}},
		"status": map[string]any{"addresses": []any{map[string]any{"type": "InternalIP", "address": "172.16.101.10"}},
			"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}},
	}
	config["data"].(map[string]any)["managedDevices"] = "phys0"
	api.Change("nodes", node, false)
	api.Change("configmaps", config, false)
	daemon := api.Object("pods", "kcn-system", "cni-ds")
	if daemon == nil {
		t.Fatal("existing CNI daemon boundary required")
	}
	daemon["spec"].(map[string]any)["nodeName"] = "node-1"
	daemon["status"].(map[string]any)["conditions"] = []any{map[string]any{"type": "Ready", "status": "True"}}
	api.Change("pods", daemon, false)
}
