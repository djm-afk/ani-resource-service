//go:build networkintegration

package data_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	data "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network"
	runtime "github.com/zhangzhe-ctrl/ani-resource-service/internal/server"
	service "github.com/zhangzhe-ctrl/ani-resource-service/internal/service/network"
	controlled "github.com/zhangzhe-ctrl/ani-resource-service/tests/net05a/provider"
	jsonpatch "gopkg.in/evanphx/json-patch.v4"
	"k8s.io/client-go/tools/clientcmd"
)

const jointClusterUID = "b490eae7-5317-411b-8cc1-b8bf79ec1234"
const jointGroupPrefix = "vcluster.networking.kubercloud.com/"

type networkJointAggregates struct {
	steppers       []runtime.Stepper
	defaultService *service.PlatformDefaultEgressService
	identity       biz.DefaultVPCIdentity
}

func wireNetworkJointAggregates(t *testing.T, f *lbAdmissionFixture, provider *data.KCProvider, policy biz.WorkerPolicy) *networkJointAggregates {
	t.Helper()
	f.f.p.UseVirtualSwitchInfrastructure(provider)
	e, err := biz.NewEgress(f.f.p, provider, biz.ContextEgressAuthorization{}, []byte("0123456789abcdef0123456789abcdef"), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	f.f.e = e
	switchWorker, err := biz.NewVirtualSwitchWorker(f.f.p, provider, uuid.NewString(), policy)
	if err != nil {
		t.Fatal(err)
	}
	vpc := f.api.Object("vpcs", "kcn-system", "kcn-cluster")
	if vpc == nil {
		t.Fatal("fixed existing default VPC required")
	}
	identity := biz.DefaultVPCIdentity{ClusterID: "test-cluster", ClusterUID: jointClusterUID, Namespace: "kcn-system", Name: "kcn-cluster", UID: fmt.Sprint(vpc["metadata"].(map[string]any)["uid"])}
	f.api.Change("namespaces", map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "kube-system", "uid": jointClusterUID}}, false)
	adapter, err := data.NewDefaultEgressKCProvider(provider, identity)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := data.NewDefaultVPCEgressRepository(f.f.p, identity)
	if err != nil {
		t.Fatal(err)
	}
	usecase, err := biz.NewDefaultVPCEgress(repo, biz.ContextEgressAuthorization{})
	if err != nil {
		t.Fatal(err)
	}
	defaultWorker, err := biz.NewDefaultVPCEgressWorker(repo, adapter, uuid.NewString(), policy)
	if err != nil {
		t.Fatal(err)
	}
	return &networkJointAggregates{steppers: []runtime.Stepper{switchWorker, defaultWorker}, defaultService: service.NewPlatformDefaultEgressService(usecase), identity: identity}
}

// This is exclusively the external Kubernetes/CNI side. It neither reads nor
// writes Resource SQL and never substitutes for the product worker or owner CAS.
type networkJointSwitchBoundary struct {
	mu          sync.Mutex
	ready       bool
	lastRefresh time.Time
}

func (b *networkJointSwitchBoundary) refresh(api *controlled.Server) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if time.Since(b.lastRefresh) < 500*time.Millisecond {
		return
	}
	b.publishLocked(api)
}
func jointObjects(api *controlled.Server, resource string) []map[string]any {
	api.Backend.Mu.Lock()
	defer api.Backend.Mu.Unlock()
	out := []map[string]any{}
	for key, obj := range api.Backend.Objects {
		if strings.HasPrefix(key, resource+"/") {
			body, _ := json.Marshal(obj)
			var copied map[string]any
			_ = json.Unmarshal(body, &copied)
			out = append(out, copied)
		}
	}
	return out
}
func (b *networkJointSwitchBoundary) publishLocked(api *controlled.Server) {
	node := api.Object("nodes", "", "node-1")
	if node == nil {
		return
	}
	meta := node["metadata"].(map[string]any)
	labels, _ := meta["labels"].(map[string]any)
	annotations, _ := meta["annotations"].(map[string]any)
	if annotations == nil {
		annotations = map[string]any{}
	}
	before, _ := json.Marshal(annotations)
	groups := map[string]bool{}
	for key, value := range annotations {
		if strings.HasPrefix(key, jointGroupPrefix) && fmt.Sprint(value) == "phys1" {
			groups[strings.TrimPrefix(key, jointGroupPrefix)] = false
		}
	}
	vlans := jointObjects(api, "vlannetworks")
	for _, vlan := range vlans {
		spec, _ := vlan["spec"].(map[string]any)
		if spec["devName"] != "phys1" {
			continue
		}
		group, _ := spec["vClusterName"].(string)
		wired := b.ready && group != "" && labels[jointGroupPrefix+group] == "100"
		groups[group] = groups[group] || wired
		status, _ := vlan["status"].(map[string]any)
		if status == nil {
			status = map[string]any{}
		}
		beforeStatus, _ := json.Marshal(status)
		status["conditions"] = []any{map[string]any{"type": "Valid", "status": "True"}, map[string]any{"type": "Initialized", "status": "True"}}
		status["notReadyNodes"] = []any{}
		if !wired {
			status["notReadyNodes"] = []any{"node-1"}
		}
		afterStatus, _ := json.Marshal(status)
		if string(beforeStatus) != string(afterStatus) {
			vlan["status"] = status
			api.Change("vlannetworks", vlan, false)
		}
	}
	wiredDevice := false
	for group, wired := range groups {
		if wired {
			annotations[jointGroupPrefix+group] = "phys1"
			wiredDevice = true
		} else {
			delete(annotations, jointGroupPrefix+group)
		}
	}
	managed := map[string]bool{}
	if raw, ok := annotations["networking.kubercloud.com/managed_netdevs"].(string); ok {
		_ = json.Unmarshal([]byte(raw), &managed)
	}
	delete(managed, "phys1")
	if wiredDevice {
		managed["phys1"] = true
	}
	rawManaged, _ := json.Marshal(managed)
	annotations["networking.kubercloud.com/managed_netdevs"] = string(rawManaged)
	meta["annotations"] = annotations
	after, _ := json.Marshal(annotations)
	if string(before) != string(after) {
		api.Change("nodes", node, false)
	}
	b.lastRefresh = time.Now()
}
func jointHTTPResponse(w http.ResponseWriter, recorder *httptest.ResponseRecorder) {
	for key, values := range recorder.Header() {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(recorder.Code)
	_, _ = w.Write(recorder.Body.Bytes())
}
func (b *networkJointSwitchBoundary) http(w http.ResponseWriter, r *http.Request, api *controlled.Server) bool {
	if r.URL.Path == "/network-joint/switch-wiring" && r.Method == http.MethodPost {
		var input struct {
			Ready bool `json:"ready"`
		}
		if json.NewDecoder(r.Body).Decode(&input) != nil {
			http.Error(w, "invalid body", 400)
			return true
		}
		b.mu.Lock()
		b.ready = input.Ready
		b.publishLocked(api)
		b.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]bool{"ready": input.Ready})
		return true
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	nodePatch := r.Method == http.MethodPatch && len(parts) == 4 && parts[0] == "api" && parts[2] == "nodes"
	if nodePatch {
		b.mu.Lock()
		defer b.mu.Unlock()
		resource, namespace, name := "nodes", "", parts[len(parts)-1]
		object := api.Object(resource, namespace, name)
		if object == nil {
			http.Error(w, "not found", 404)
			return true
		}
		raw, err := io.ReadAll(r.Body)
		var operations []struct {
			Op, Path string
			Value    any
		}
		if err != nil || json.Unmarshal(raw, &operations) != nil || len(operations) < 2 || operations[0].Op != "test" || operations[0].Path != "/metadata/uid" || operations[1].Op != "test" || operations[1].Path != "/metadata/resourceVersion" {
			http.Error(w, "identity CAS required", 422)
			return true
		}
		patch, err := jsonpatch.DecodePatch(raw)
		if err != nil {
			http.Error(w, "invalid patch", 422)
			return true
		}
		body, _ := json.Marshal(object)
		updated, err := patch.Apply(body)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(409)
			_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"Status","status":"Failure","reason":"Conflict","code":409}`))
			return true
		}
		_ = json.Unmarshal(updated, &object)
		api.Change(resource, object, false)
		b.publishLocked(api)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.Object(resource, namespace, name))
		return true
	}
	vlanMutation := len(parts) >= 4 && parts[0] == "apis" && parts[3] == "vlannetworks" && (r.Method == http.MethodPost || r.Method == http.MethodDelete)
	subnetMutation := len(parts) >= 6 && parts[0] == "apis" && parts[3] == "namespaces" && parts[5] == "subnets" && (r.Method == http.MethodPost || r.Method == http.MethodDelete)
	if !vlanMutation && !subnetMutation {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	var before map[string]any
	if subnetMutation && r.Method == http.MethodDelete && len(parts) == 7 {
		before = api.Object("subnets", parts[4], parts[6])
	}
	recorder := httptest.NewRecorder()
	// Preserve the existing Intranet external-controller contract before the
	// generic controlled API. The switch layer also sees its Subnet requests.
	if !subnetMutation || !intranetProviderInterceptor(recorder, r, api) {
		api.ServeHTTP(recorder, r)
	}
	if recorder.Code >= 200 && recorder.Code < 300 {
		if before != nil {
			spec, _ := before["spec"].(map[string]any)
			underlay, _ := spec["underlayConfig"].(map[string]any)
			if underlay != nil {
				vlan := api.Object("vlannetworks", "", fmt.Sprint(underlay["vlanNetwork"]))
				if vlan != nil {
					status, _ := vlan["status"].(map[string]any)
					delete(status, "subnet")
					vlan["status"] = status
					api.Change("vlannetworks", vlan, false)
				}
			}
		}
		b.publishLocked(api)
		if r.Method == http.MethodPost {
			var created map[string]any
			if json.Unmarshal(recorder.Body.Bytes(), &created) == nil {
				meta, _ := created["metadata"].(map[string]any)
				resource, namespace := "vlannetworks", ""
				if subnetMutation {
					resource, namespace = "subnets", parts[4]
				}
				if updated := api.Object(resource, namespace, fmt.Sprint(meta["name"])); updated != nil {
					if subnetMutation {
						spec, _ := updated["spec"].(map[string]any)
						underlay, _ := spec["underlayConfig"].(map[string]any)
						vlanName, _ := underlay["vlanNetwork"].(string)
						status, _ := updated["status"].(map[string]any)
						observed, _ := status["underlayState"].(map[string]any)
						if vlanName != "" && observed != nil && api.Object("vlannetworks", "", vlanName) != nil {
							observed["vlanNetwork"] = vlanName
							api.Change(resource, updated, false)
							updated = api.Object(resource, namespace, fmt.Sprint(meta["name"]))
						}
					}
					recorder.Body.Reset()
					_ = json.NewEncoder(recorder).Encode(updated)
				}
			}
		}
	}
	jointHTTPResponse(w, recorder)
	return true
}

func TestNetworkJointBoundaryPreservesIntranetSubnetCreate(t *testing.T) {
	api := controlled.New()
	seedEgressInfrastructure(api)
	seedIntranetInfrastructure(api)
	body := []byte(`{"apiVersion":"networking.kubercloud.com/v1","kind":"Subnet","metadata":{"name":"joint-intranet","namespace":"kcn-system"},"spec":{"type":"Intranet","gateway":"kcn-system/kcn-cluster","allowedNamespaces":{"from":"All"},"ipVersion":"IPv4","cidrBlock":"10.232.254.0/24"}}`)
	r := httptest.NewRequest(http.MethodPost, "/apis/networking.kubercloud.com/v1/namespaces/kcn-system/subnets", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	boundary := &networkJointSwitchBoundary{}
	if !boundary.http(response, r, api) || response.Code != http.StatusCreated {
		t.Fatalf("switch boundary rejected existing Intranet create contract: status=%d body=%s", response.Code, response.Body.String())
	}
	object := api.Object("subnets", "kcn-system", "joint-intranet")
	if object == nil || object["spec"].(map[string]any)["type"] != "Intranet" {
		t.Fatal("Intranet provider object was not preserved", object)
	}
}

func TestNetworkJointBoundaryPublishesV09UnderlayReference(t *testing.T) {
	api := controlled.New()
	api.Change("eipgateways", map[string]any{"apiVersion": "networking.kubercloud.com/v1", "kind": "EIPGateway", "metadata": map[string]any{"name": "joint-gateway", "uid": uuid.NewString()}, "spec": map[string]any{"scope": "Public", "egressType": "Host"}}, false)
	api.Change("vlannetworks", map[string]any{"apiVersion": "networking.kubercloud.com/v1", "kind": "VlanNetwork", "metadata": map[string]any{"name": "joint-vlan", "uid": uuid.NewString()}, "spec": map[string]any{"devName": "phys1", "vClusterName": "gov-kcn-switch", "vlanID": 0}, "status": map[string]any{"conditions": []any{map[string]any{"type": "Valid", "status": "True"}, map[string]any{"type": "Initialized", "status": "True"}}}}, false)
	body := []byte(`{"apiVersion":"networking.kubercloud.com/v1","kind":"Subnet","metadata":{"name":"joint-underlay","namespace":"kcn-system"},"spec":{"type":"Public","allowedNamespaces":{"from":"All"},"cidrBlock":"198.51.101.0/24","gatewayIP":"198.51.101.254","gateway":"joint-gateway","underlayConfig":{"gatewayIP":"198.51.101.1","vlanNetwork":"joint-vlan"}}}`)
	r := httptest.NewRequest(http.MethodPost, "/apis/networking.kubercloud.com/v1/namespaces/kcn-system/subnets", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	boundary := &networkJointSwitchBoundary{}
	if !boundary.http(response, r, api) || response.Code != http.StatusCreated {
		t.Fatalf("underlay controller create failed: status=%d body=%s", response.Code, response.Body.String())
	}
	object := api.Object("subnets", "kcn-system", "joint-underlay")
	status := object["status"].(map[string]any)
	underlay := status["underlayState"].(map[string]any)
	if underlay["vlanNetwork"] != "joint-vlan" {
		t.Fatal("v0.9 readiness must identify the actual native VLAN", underlay)
	}
	vlan := api.Object("vlannetworks", "", "joint-vlan")
	if vlan["status"].(map[string]any)["subnet"] != "kcn-system/joint-underlay" {
		t.Fatal("VLAN and Subnet observations must refer to each other", vlan)
	}
}

func networkJointBackendObserver(t *testing.T, f *lbAdmissionFixture, provider *data.KCProvider, policy biz.WorkerPolicy) *biz.AttachmentWorker {
	t.Helper()
	attachments := biz.NewAttachments(f.f.p)
	owners := lbKnownOwners{}
	for _, name := range []string{"lb-backend", "lb-second"} {
		var id string
		if err := f.f.owner.QueryRow(f.f.ctx, "SELECT attachment_id FROM network_attachments WHERE tenant_id=$1 AND instance_id=$2", f.f.tenant, name).Scan(&id); err != nil {
			t.Fatal(err)
		}
		attachment, err := attachments.Get(f.f.ctx, f.f.tenant, id)
		if err != nil {
			t.Fatal(err)
		}
		owners[id] = attachment
	}
	worker, err := biz.NewAttachmentWorker(f.f.p, owners, provider, uuid.NewString(), policy)
	if err != nil {
		t.Fatal(err)
	}
	return worker
}

func TestNetworkJointBackendWorkerRefreshesAttachmentObservation(t *testing.T) {
	f := newLBAdmissionFixture(t)
	addLBBackend(t, f, f.backendSubnet, "lb-second", "10.42.2.3")
	cfg, err := clientcmd.BuildConfigFromFlags("", f.kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := data.NewKCProvider(f.f.p, cfg)
	if err != nil {
		t.Fatal(err)
	}
	policy := biz.DefaultWorkerPolicy()
	policy.ObserveEvery = 10 * time.Millisecond
	worker := networkJointBackendObserver(t, f, provider, policy)
	var before time.Time
	var podUID string
	if err = f.f.owner.QueryRow(f.f.ctx, "SELECT observed_at,pod_uid FROM network_attachments WHERE tenant_id=$1 AND instance_id='lb-backend'", f.f.tenant).Scan(&before, &podUID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.f.owner.Exec(f.f.ctx, "UPDATE network_attachments SET next_check_at=clock_timestamp() WHERE tenant_id=$1 AND instance_id='lb-backend'", f.f.tenant); err != nil {
		t.Fatal(err)
	}
	awaitNET05A(t, 3*time.Second, func() bool {
		if _, err := worker.Step(f.f.ctx); err != nil {
			t.Fatal(err)
		}
		var observed time.Time
		var samePod bool
		return f.f.owner.QueryRow(f.f.ctx, "SELECT observed_at,(pod_uid=$2 AND state='attached') FROM network_attachments WHERE tenant_id=$1 AND instance_id='lb-backend'", f.f.tenant, podUID).Scan(&observed, &samePod) == nil && observed.After(before) && samePod
	})
}
