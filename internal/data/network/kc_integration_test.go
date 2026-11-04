package data_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network"
	"k8s.io/client-go/rest"
)

// This HTTP server exercises the real dynamic client and serialized Kubernetes
// requests. It is deliberately not evidence for admission, OVN or finalizers.
type controlledKC struct {
	mu               sync.Mutex
	namespace        map[string]any
	object           map[string]any
	creates, deletes int
	requestInvalid   string
}

func (s *controlledKC) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Query().Get("watch") == "true" {
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"Status","code":403,"reason":"Forbidden","status":"Failure"}`))
		return
	}
	for plural, kind := range map[string]string{"vpcs": "VPC", "subnets": "Subnet", "vnics": "VNic", "vnicips": "VNicIP", "eips": "EIP", "pods": "Pod", "snats": "Snat", "nats": "Nat", "eipgateways": "EIPGateway", "vlannetworks": "VlanNetwork", "nodes": "Node", "configmaps": "ConfigMap", "services": "Service", "servicecidrs": "ServiceCIDR"} {
		base := "/apis/networking.kubercloud.com/v1/"
		version := "networking.kubercloud.com/v1"
		if plural == "pods" || plural == "nodes" || plural == "configmaps" || plural == "services" {
			base = "/api/v1/"
			version = "v1"
		}
		if plural == "servicecidrs" {
			base = "/apis/networking.k8s.io/v1/"
			version = "networking.k8s.io/v1"
		}
		if r.Method == "GET" && r.URL.Path == base+plural {
			items := []any{}
			if plural == "vpcs" && s.object != nil {
				items = append(items, s.object)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": version, "kind": kind + "List", "metadata": map[string]any{"resourceVersion": "1"}, "items": items})
			return
		}
	}
	missing := func() {
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404}`))
	}
	if strings.HasPrefix(r.URL.Path, "/api/v1/namespaces") {
		if r.Method == "POST" {
			if err := json.NewDecoder(r.Body).Decode(&s.namespace); err != nil {
				s.requestInvalid = err.Error()
			}
			s.namespace["kind"] = "Namespace"
			s.namespace["apiVersion"] = "v1"
		}
		if s.namespace == nil {
			missing()
			return
		}
		_ = json.NewEncoder(w).Encode(s.namespace)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/subnets") {
		_, _ = w.Write([]byte(`{"apiVersion":"networking.kubercloud.com/v1","kind":"SubnetList","metadata":{},"items":[]}`))
		return
	}
	switch r.Method {
	case "GET":
		if s.object == nil {
			missing()
			return
		}
		_ = json.NewEncoder(w).Encode(s.object)
	case "POST":
		s.creates++
		if err := json.NewDecoder(r.Body).Decode(&s.object); err != nil {
			s.requestInvalid = err.Error()
		}
		spec, _ := s.object["spec"].(map[string]any)
		ns, _ := spec["allowedNamespaces"].(map[string]any)
		if spec["cidrBlock"] != "10.42.0.0/16" || spec["ipVersion"] != "IPv4" || ns["from"] != "Same" {
			s.requestInvalid = "incorrect VPC spec"
		}
		metadata := s.object["metadata"].(map[string]any)
		metadata["uid"] = "stable-provider-uid"
		metadata["resourceVersion"] = "10"
		metadata["generation"] = float64(1)
		s.object["status"] = map[string]any{"observedGeneration": float64(1), "conditions": []any{
			map[string]any{"type": "Valid", "status": "True"}, map[string]any{"type": "Initialized", "status": "True"}, map[string]any{"type": "Ready", "status": "True"},
		}, "boundResources": map[string]any{"router": "private-router"}}
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(s.object)
	case "DELETE":
		s.deletes++
		var value struct {
			Preconditions     struct{ UID, ResourceVersion string }
			PropagationPolicy string
		}
		if err := json.NewDecoder(r.Body).Decode(&value); err != nil {
			s.requestInvalid = err.Error()
		}
		if value.Preconditions.UID != "stable-provider-uid" || value.Preconditions.ResourceVersion != "10" || value.PropagationPolicy != "" && value.PropagationPolicy != "Background" {
			s.requestInvalid = "delete lacks original identity/version conditions"
		}
		s.object = nil
		_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"Status","status":"Success"}`))
	default:
		s.requestInvalid = "unexpected method " + r.Method
		w.WriteHeader(405)
	}
}

func TestKCAdapterExecutesTheVPCContractThroughHTTP(t *testing.T) {
	repository, _ := database(t)
	network, err := biz.NewNetwork(repository, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	tenant := uuid.NewString()
	api := &controlledKC{namespace: map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "tenant-" + tenant, "uid": uuid.NewString(), "labels": map[string]any{"network.ani.io/managed-by": "ani-network-service", "network.ani.io/tenant-id": tenant}}, "status": map[string]any{"phase": "Active"}}}
	host := httptest.NewServer(api)
	t.Cleanup(host.Close)
	provider, err := data.NewKCProvider(repository, &rest.Config{Host: host.URL})
	if err != nil {
		t.Fatal(err)
	}
	policy := biz.DefaultWorkerPolicy()
	policy.ObserveEvery = time.Millisecond
	policy.RetryMin = time.Millisecond
	worker, err := biz.NewWorker(repository, provider, uuid.NewString(), policy)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	accepted, err := network.CreateVPC(ctx, biz.CreateVPC{TenantID: tenant, Name: "kc", CIDR: "10.42.0.0/16", IdempotencyKey: "kc"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.Step(ctx); err != nil {
		t.Fatal(err)
	}
	available, err := network.GetVPC(ctx, tenant, accepted.ID)
	if err != nil || available.State != biz.Available {
		t.Fatalf("real adapter did not converge: %+v %v", available, err)
	}
	if _, err := network.DeleteVPC(ctx, tenant, accepted.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := worker.Step(ctx); err != nil {
		t.Fatal(err)
	}
	pending, err := network.GetVPC(ctx, tenant, accepted.ID)
	if err != nil || pending.State != biz.Deleting {
		t.Fatalf("delete acceptance mistaken for completion: %+v %v", pending, err)
	}
	time.Sleep(3 * time.Millisecond)
	if _, err := worker.Step(ctx); err != nil {
		t.Fatal(err)
	}
	deleted, err := network.GetVPC(ctx, tenant, accepted.ID)
	if err != nil || deleted.State != biz.Deleted {
		t.Fatalf("deletion not confirmed: %+v %v", deleted, err)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.creates != 1 || api.deletes != 1 || api.requestInvalid != "" {
		t.Fatalf("requests: create=%d delete=%d invalid=%s", api.creates, api.deletes, api.requestInvalid)
	}
}
