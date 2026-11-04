package data_test

import (
	"bytes"
	"encoding/json"
	"github.com/google/uuid"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	controlled "github.com/zhangzhe-ctrl/ani-resource-service/tests/net05a/provider"
	"io"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// This adds only the new Intranet contract to the existing controlled server.
// It does not implement kc routing and provides no data-plane proof.
var intranetFixtureMutation sync.Mutex

func intranetProviderInterceptor(w http.ResponseWriter, r *http.Request, api *controlled.Server) bool {
	if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/subnets") {
		return false
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return false
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	var object map[string]any
	if json.Unmarshal(body, &object) != nil {
		return false
	}
	spec, _ := object["spec"].(map[string]any)
	if spec["type"] != "Intranet" {
		return false
	}
	intranetFixtureMutation.Lock()
	defer intranetFixtureMutation.Unlock()
	fail := func(code int, reason string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": "v1", "kind": "Status", "status": "Failure", "reason": reason, "code": code})
	}
	meta, _ := object["metadata"].(map[string]any)
	allowed, _ := spec["allowedNamespaces"].(map[string]any)
	name, _ := meta["name"].(string)
	gateway := api.Object("vpcs", "kcn-system", "kcn-cluster")
	gatewayMeta, _ := gateway["metadata"].(map[string]any)
	cidr, parseErr := netip.ParsePrefix(fmtString(spec["cidrBlock"]))
	if r.URL.Path != "/apis/networking.kubercloud.com/v1/namespaces/kcn-system/subnets" || object["apiVersion"] != "networking.kubercloud.com/v1" || object["kind"] != "Subnet" || name == "" || meta["namespace"] != "kcn-system" || spec["gateway"] != "kcn-system/kcn-cluster" || gatewayMeta["uid"] == nil || allowed["from"] != "All" || spec["underlayConfig"] != nil || spec["ipVersion"] != "IPv4" || parseErr != nil || !cidr.Addr().Is4() {
		fail(http.StatusUnprocessableEntity, "Invalid")
		return true
	}
	if api.Object("subnets", "kcn-system", name) != nil {
		fail(http.StatusConflict, "AlreadyExists")
		return true
	}
	api.Backend.Mu.Lock()
	api.Backend.Creates["subnets"]++
	count := api.Backend.Creates["subnets"]
	api.Backend.Mu.Unlock()
	meta["uid"] = uuid.NewString()
	meta["generation"] = 1
	meta["resourceVersion"] = strconv.Itoa(count)
	object["status"] = map[string]any{"observedGeneration": 1, "boundResources": map[string]any{"switch": "controlled-intranet-switch", "gatewayPort": "controlled-intranet-port"}, "conditions": []any{map[string]any{"type": "Valid", "status": "True", "observedGeneration": 1}, map[string]any{"type": "Initialized", "status": "True", "observedGeneration": 1}, map[string]any{"type": "Ready", "status": "True", "observedGeneration": 1}}}
	api.Change("subnets", object, false)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(api.Object("subnets", "kcn-system", name))
	return true
}
func fmtString(v any) string { s, _ := v.(string); return s }
func seedIntranetInfrastructure(api *controlled.Server) string {
	uid := uuid.NewString()
	api.Change("vpcs", map[string]any{"apiVersion": "networking.kubercloud.com/v1", "kind": "VPC", "metadata": map[string]any{"namespace": "kcn-system", "name": "kcn-cluster", "uid": uid}, "status": map[string]any{"default": true}}, false)
	return uid
}
func TestIntranetKCPlatformUsesItsOwnNativeStatus(t *testing.T) {
	f, api, _, _ := newEgressKCFixture(t, intranetProviderInterceptor)
	uid := seedIntranetInfrastructure(api)
	intent := intranetIntent("controlled-intranet", "10.232.254.0/24", "10.232.254.1")
	intent.Pool.DefaultVPCUID = uid
	pool := readyIntranetPool(t, f, f.platform(t, intent))
	setIntranetPool(t, f, biz.PlatformIntent{Kind: "set_default_intranet_pool", ID: pool.ID, IdempotencyKey: "default-controlled"})
	name := strings.Replace(pool.ID, "_", "-", 1)
	object := api.Object("subnets", "kcn-system", name)
	spec := object["spec"].(map[string]any)
	if spec["type"] != "Intranet" || spec["gateway"] != "kcn-system/kcn-cluster" || spec["underlayConfig"] != nil {
		t.Fatal(spec)
	}
	// The parent can report different runtime facts without replacing this
	// Subnet controller's own Ready status.
	gateway := api.Object("vpcs", "kcn-system", "kcn-cluster")
	gateway["metadata"].(map[string]any)["uid"] = uuid.NewString()
	gateway["status"].(map[string]any)["conditions"] = []any{map[string]any{"type": "Ready", "status": "False"}}
	api.Change("vpcs", gateway, false)
	current, err := f.e.GetPlatform(f.ctx, "intranet_pool", pool.ID)
	if err != nil || current.State != biz.Available {
		t.Fatal(current, err)
	}
	object["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "False", "reason": "ControllerPending"}}}
	api.Change("subnets", object, false)
	f.drive(t, func() bool {
		v, err := f.e.GetPlatform(f.ctx, "intranet_pool", pool.ID)
		return err == nil && v.State == biz.Degraded
	})
	caps, err := f.e.GetPlatformCapabilities(f.ctx)
	if err != nil || caps.BaseConnectivity.Ready || !caps.PublicAddress.Ready {
		t.Fatal(caps, err)
	}
	object["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}
	api.Change("subnets", object, false)
	f.drive(t, func() bool {
		v, err := f.e.GetPlatform(f.ctx, "intranet_pool", pool.ID)
		return err == nil && v.State == biz.Available
	})
	if after := api.Object("subnets", "kcn-system", name); after["metadata"].(map[string]any)["uid"] != object["metadata"].(map[string]any)["uid"] {
		t.Fatal("status recovery recreated pool")
	}
	if api.Backend.Invalid != "" {
		t.Fatal(api.Backend.Invalid)
	}
}

func TestIntranetPoolDefaultsUsesNativeDefaultFlag(t *testing.T) {
	f, api, _, _ := newEgressKCFixture(t, intranetProviderInterceptor)
	uid := seedIntranetInfrastructure(api)
	got, err := f.e.GetIntranetPoolDefaults(f.ctx)
	if err != nil || got.DefaultVPCName != "kcn-cluster" || got.DefaultVPCUID != uid || len(got.IntranetNetworks) != 0 {
		t.Fatal(got, err)
	}
	object := api.Object("vpcs", "kcn-system", "kcn-cluster")
	object["status"].(map[string]any)["default"] = false
	api.Change("vpcs", object, false)
	if _, err = f.e.GetIntranetPoolDefaults(f.ctx); err == nil {
		t.Fatal("missing native default returned a reference")
	}
}
