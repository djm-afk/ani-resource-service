package data_test

import (
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	controlled "github.com/zhangzhe-ctrl/ani-resource-service/tests/net05a/provider"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// This HTTP fixture implements the Kubernetes/controller side only. Every LB
// product CR originates in the actual Network adapter, after PG admission.
// Generated objects/status/IPAM are controlled facts, never live acceptance.
type lbControllerFixture struct {
	mu                         sync.Mutex
	requests                   []string
	holdGenerated, holdCleanup bool
	rejectPolicy               bool
}

func lbTestConditions(generation any, kinds ...string) []any {
	out := []any{}
	for _, kind := range kinds {
		out = append(out, map[string]any{"type": kind, "status": "True", "observedGeneration": generation})
	}
	return out
}
func (c *lbControllerFixture) http(w http.ResponseWriter, r *http.Request, api *controlled.Server) bool {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 4 {
		return false
	}
	offset := 3
	if parts[0] == "api" {
		offset = 2
	}
	if len(parts) <= offset {
		return false
	}
	kind := parts[offset]
	ns, name := "", ""
	if kind == "namespaces" && len(parts) > offset+2 {
		ns, kind = parts[offset+1], parts[offset+2]
		if len(parts) > offset+3 {
			name = parts[offset+3]
		}
	} else if len(parts) > offset+1 {
		name = parts[offset+1]
	}
	kinds := map[string]string{"envoyproxies": "EnvoyProxy", "gateways": "Gateway", "backends": "Backend", "httproutes": "HTTPRoute", "backendtrafficpolicies": "BackendTrafficPolicy", "tcproutes": "TCPRoute", "udproutes": "UDPRoute", "clienttrafficpolicies": "ClientTrafficPolicy", "envoypatchpolicies": "EnvoyPatchPolicy", "secrets": "Secret"}
	if _, ok := kinds[kind]; !ok {
		return false
	}
	if r.Method == "GET" && name == "" {
		return false
	} // Existing complete List/Watch transport.
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests = append(c.requests, r.Method+"/"+kind+"/"+name)
	write := func(code int, obj any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(obj)
	}
	fail := func(code int, reason string) {
		write(code, map[string]any{"apiVersion": "v1", "kind": "Status", "status": "Failure", "reason": reason, "code": code})
	}
	obj := api.Object(kind, ns, name)
	if r.Method == "GET" {
		if obj == nil {
			fail(404, "NotFound")
		} else {
			write(200, obj)
		}
		return true
	}
	if c.rejectPolicy && kind == "backendtrafficpolicies" && (r.Method == "POST" || r.Method == "PATCH") {
		fail(403, "Forbidden")
		return true
	}
	switch r.Method {
	case "POST":
		if json.NewDecoder(r.Body).Decode(&obj) != nil {
			fail(400, "BadRequest")
			return true
		}
		m, _ := obj["metadata"].(map[string]any)
		if m == nil || m["namespace"] != ns || obj["kind"] != kinds[kind] {
			fail(422, "Invalid")
			return true
		}
		name, _ = m["name"].(string)
		if api.Object(kind, ns, name) != nil {
			fail(409, "AlreadyExists")
			return true
		}
		m["uid"], m["generation"] = uuid.NewString(), float64(1)
	case "PATCH":
		if obj == nil {
			fail(404, "NotFound")
			return true
		}
		var patch []map[string]any
		if json.NewDecoder(r.Body).Decode(&patch) != nil || len(patch) != 4 {
			fail(400, "BadRequest")
			return true
		}
		m := obj["metadata"].(map[string]any)
		if patch[0]["op"] != "test" || patch[0]["path"] != "/metadata/uid" || patch[0]["value"] != m["uid"] || patch[1]["path"] != "/metadata/resourceVersion" || patch[1]["value"] != m["resourceVersion"] || patch[2]["path"] != "/spec" || patch[3]["path"] != "/metadata/annotations" {
			fail(409, "Conflict")
			return true
		}
		obj["spec"], m["annotations"] = patch[2]["value"], patch[3]["value"]
		m["generation"] = m["generation"].(float64) + 1
	case "DELETE":
		if obj == nil {
			fail(404, "NotFound")
			return true
		}
		var options struct {
			Preconditions     struct{ UID, ResourceVersion string }
			PropagationPolicy string
		}
		if json.NewDecoder(r.Body).Decode(&options) != nil {
			fail(400, "BadRequest")
			return true
		}
		m := obj["metadata"].(map[string]any)
		if options.Preconditions.UID != m["uid"] || options.Preconditions.ResourceVersion != m["resourceVersion"] || options.PropagationPolicy != "" {
			fail(409, "Conflict")
			return true
		}
		if c.holdCleanup {
			m["deletionTimestamp"] = time.Now().UTC().Format(time.RFC3339)
			m["finalizers"] = []any{"controlled-provider/finalize"}
			api.Change(kind, obj, false)
		} else {
			api.Change(kind, obj, true)
			if kind == "gateways" {
				c.generated(api, obj, true)
			}
		}
		write(200, map[string]any{"apiVersion": "v1", "kind": "Status", "status": "Success"})
		return true
	default:
		fail(405, "MethodNotAllowed")
		return true
	}
	m := obj["metadata"].(map[string]any)
	g := m["generation"]
	spec := obj["spec"].(map[string]any)
	switch kind {
	case "envoyproxies":
		// Model only actual CRD defaults, not EnvoyProxy business status.
		spec["logging"] = map[string]any{"level": map[string]any{"default": "warn"}}
		spec["provider"].(map[string]any)["kubernetes"].(map[string]any)["envoyService"].(map[string]any)["externalTrafficPolicy"] = "Local"
	case "backends":
		obj["status"] = map[string]any{"conditions": lbTestConditions(g, "Accepted")}
	case "gateways":
		statusListeners := []any{}
		for _, raw := range spec["listeners"].([]any) {
			listener := raw.(map[string]any)
			// Gateway API defaults an omitted RouteGroup to its own group.
			for _, rawKind := range listener["allowedRoutes"].(map[string]any)["kinds"].([]any) {
				kind := rawKind.(map[string]any)
				if _, explicit := kind["group"]; !explicit {
					kind["group"] = "gateway.networking.k8s.io"
				}
			}
			statusListeners = append(statusListeners, map[string]any{"name": listener["name"], "conditions": lbTestConditions(g, "Accepted", "Programmed", "ResolvedRefs")})
		}
		obj["status"] = map[string]any{"conditions": lbTestConditions(g, "Accepted", "Programmed"), "listeners": statusListeners}
	case "httproutes", "tcproutes", "udproutes":
		obj["status"] = map[string]any{"parents": []any{map[string]any{"parentRef": spec["parentRefs"].([]any)[0], "controllerName": "gateway.envoyproxy.io/gatewayclass-controller", "conditions": lbTestConditions(g, "Accepted", "ResolvedRefs")}}}
	case "backendtrafficpolicies":
		routeName := spec["targetRefs"].([]any)[0].(map[string]any)["name"].(string)
		routeKind := map[string]string{"HTTPRoute": "httproutes", "TCPRoute": "tcproutes", "UDPRoute": "udproutes"}[spec["targetRefs"].([]any)[0].(map[string]any)["kind"].(string)]
		route := api.Object(routeKind, ns, routeName)
		gateway := ""
		if route != nil {
			gateway = route["spec"].(map[string]any)["parentRefs"].([]any)[0].(map[string]any)["name"].(string)
		} else {
			// A policy can precede its route; controller status will reconcile on
			// the next read through refreshPolicies below.
			gateway = "pending-route"
		}
		obj["status"] = map[string]any{"ancestors": []any{map[string]any{"ancestorRef": map[string]any{"group": "gateway.networking.k8s.io", "kind": "Gateway", "name": gateway, "namespace": ns}, "controllerName": "gateway.envoyproxy.io/gatewayclass-controller", "conditions": lbTestConditions(g, "Accepted")}}}
	case "clienttrafficpolicies":
		ref := spec["targetRefs"].([]any)[0].(map[string]any)
		obj["status"] = map[string]any{"ancestors": []any{map[string]any{"ancestorRef": ref, "controllerName": "gateway.envoyproxy.io/gatewayclass-controller", "conditions": lbTestConditions(g, "Accepted")}}}
	case "envoypatchpolicies":
		obj["status"] = map[string]any{"conditions": lbTestConditions(g, "Accepted", "Programmed")}
	}
	api.Change(kind, obj, false)
	if kind == "gateways" && !c.holdGenerated {
		c.generated(api, obj, false)
	}
	if kind == "httproutes" || kind == "tcproutes" || kind == "udproutes" {
		c.refreshPolicies(api, ns, name, spec["parentRefs"].([]any)[0].(map[string]any)["name"].(string))
	}
	code := 200
	if r.Method == "POST" {
		code = 201
	}
	write(code, api.Object(kind, ns, name))
	return true
}
func (c *lbControllerFixture) refreshPolicies(api *controlled.Server, ns, route, gateway string) {
	api.Backend.Mu.Lock()
	items := []map[string]any{}
	for key, obj := range api.Backend.Objects {
		if strings.HasPrefix(key, "backendtrafficpolicies/"+ns+"/") {
			body, _ := json.Marshal(obj)
			var copy map[string]any
			_ = json.Unmarshal(body, &copy)
			items = append(items, copy)
		}
	}
	api.Backend.Mu.Unlock()
	for _, obj := range items {
		spec := obj["spec"].(map[string]any)
		if spec["targetRefs"].([]any)[0].(map[string]any)["name"] != route {
			continue
		}
		obj["status"].(map[string]any)["ancestors"].([]any)[0].(map[string]any)["ancestorRef"].(map[string]any)["name"] = gateway
		api.Change("backendtrafficpolicies", obj, false)
	}
}
func (c *lbControllerFixture) generated(api *controlled.Server, gateway map[string]any, deleted bool) {
	m := gateway["metadata"].(map[string]any)
	ns, name, uid := m["namespace"].(string), m["name"].(string), m["uid"].(string)
	// A Gateway removed before its Service exists has no allocation to release.
	// In particular its deletion does not manufacture a new Subnet revision.
	spec := gateway["spec"].(map[string]any)
	proxyName := spec["infrastructure"].(map[string]any)["parametersRef"].(map[string]any)["name"].(string)
	proxy := api.Object("envoyproxies", ns, proxyName)
	if proxy == nil || spec["gatewayClassName"] != biz.LoadBalancerGatewayClass {
		panic("fixture gateway does not reference a dedicated EnvoyProxy")
	}
	proxyKubernetes := proxy["spec"].(map[string]any)["provider"].(map[string]any)["kubernetes"].(map[string]any)
	proxyDeployment := proxyKubernetes["envoyDeployment"].(map[string]any)
	var flavor biz.LoadBalancerFlavor
	for _, candidate := range biz.LoadBalancerFlavors() {
		if proxyDeployment["replicas"] == float64(candidate.Replicas) {
			flavor = candidate
		}
	}
	if flavor.Name == "" {
		panic("unsupported fixture capacity")
	}
	a := spec["infrastructure"].(map[string]any)["annotations"].(map[string]any)
	servicePorts, slicePorts := []any{}, []any{}
	for _, raw := range spec["listeners"].([]any) {
		listener := raw.(map[string]any)
		port := listener["port"]
		target := port.(float64)
		if target < 1024 {
			target += 10000
		}
		protocol := "TCP"
		if listener["protocol"] == "UDP" {
			protocol = "UDP"
		}
		servicePorts = append(servicePorts, map[string]any{"name": listener["name"], "port": port, "targetPort": target, "protocol": protocol})
		slicePorts = append(slicePorts, map[string]any{"name": listener["name"], "port": target, "protocol": protocol})
	}
	owner := func(kind, name, id string) []any {
		version := "apps/v1"
		if kind == "Gateway" {
			version = "gateway.networking.k8s.io/v1"
		}
		return []any{map[string]any{"kind": kind, "apiVersion": version, "name": name, "uid": id}}
	}
	makeObject := func(gvr, kind, name string, ownerRefs []any, spec, status map[string]any) map[string]any {
		old := api.Object(gvr, ns, name)
		if deleted {
			if old != nil {
				api.Change(gvr, old, true)
			}
			return old
		}
		version := "apps/v1"
		if kind == "EndpointSlice" {
			version = "discovery.k8s.io/v1"
		}
		if kind == "Service" || kind == "Pod" {
			version = "v1"
		}
		if kind == "Pod" {
			status["containerStatuses"] = []any{map[string]any{"name": "envoy", "ready": true, "imageID": "fixture@sha256:" + strings.Repeat("c", 64)}, map[string]any{"name": "shutdown-manager", "ready": true, "imageID": "fixture@sha256:" + strings.Repeat("d", 64)}}
		}
		id := uuid.NewString()
		if old != nil {
			id = old["metadata"].(map[string]any)["uid"].(string)
		}
		obj := map[string]any{"apiVersion": version, "kind": kind, "metadata": map[string]any{"name": name, "namespace": ns, "uid": id, "generation": float64(1), "ownerReferences": ownerRefs}, "spec": spec, "status": status}
		api.Change(gvr, obj, false)
		return obj
	}
	typ := proxyKubernetes["envoyService"].(map[string]any)["type"].(string)
	svc := makeObject("services", "Service", name, owner("Gateway", name, uid), map[string]any{"type": typ, "ports": servicePorts}, map[string]any{"conditions": lbTestConditions(float64(0), "KcnValid", "KcnReady")})
	if !deleted {
		svc["metadata"].(map[string]any)["annotations"] = a
		delete(svc["metadata"].(map[string]any), "generation")
		api.Change("services", svc, false)
	}
	dep := makeObject("deployments", "Deployment", name, owner("Gateway", name, uid), map[string]any{"replicas": float64(flavor.Replicas), "template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"name": "envoy", "resources": map[string]any{"requests": map[string]any{"cpu": flavor.RequestCPU, "memory": flavor.RequestMemory}, "limits": map[string]any{"cpu": flavor.LimitCPU, "memory": flavor.LimitMemory}}}}}}}, map[string]any{"observedGeneration": float64(1), "updatedReplicas": float64(flavor.Replicas), "availableReplicas": float64(flavor.Replicas)})
	depUID := ""
	if dep != nil {
		depUID = dep["metadata"].(map[string]any)["uid"].(string)
	}
	rs := makeObject("replicasets", "ReplicaSet", name+"-rs", owner("Deployment", name, depUID), map[string]any{}, map[string]any{})
	rsUID := ""
	if rs != nil {
		rsUID = rs["metadata"].(map[string]any)["uid"].(string)
	}
	endpoints := []any{}
	for i := 0; i < int(flavor.Replicas); i++ {
		pod := makeObject("pods", "Pod", fmt.Sprintf("%s-proxy-%d", name, i), owner("ReplicaSet", name+"-rs", rsUID), map[string]any{}, map[string]any{"phase": "Running", "podIP": fmt.Sprintf("10.42.1.%d", i+2), "conditions": []any{map[string]any{"type": "Ready", "status": "True"}}})
		if pod != nil {
			pm := pod["metadata"].(map[string]any)
			endpoints = append(endpoints, map[string]any{"addresses": []any{fmt.Sprintf("10.42.1.%d", i+2)}, "conditions": map[string]any{"ready": true}, "targetRef": map[string]any{"kind": "Pod", "name": pm["name"], "namespace": ns, "uid": pm["uid"]}})
		}
	}
	serviceUID := ""
	if svc != nil {
		serviceUID = svc["metadata"].(map[string]any)["uid"].(string)
	}
	slice := makeObject("endpointslices", "EndpointSlice", name+"-slice", []any{map[string]any{"kind": "Service", "name": name, "uid": serviceUID, "apiVersion": "v1"}}, map[string]any{}, map[string]any{})
	if !deleted {
		delete(slice, "spec")
		delete(slice, "status")
		slice["addressType"] = "IPv4"
		slice["ports"] = slicePorts
		slice["endpoints"] = endpoints
		slice["metadata"].(map[string]any)["labels"] = map[string]any{"kubernetes.io/service-name": name}
		api.Change("endpointslices", slice, false)
	}
	if vip, _ := a["networking.kubercloud.com/lb_vip_address"].(string); vip != "disable" && vip != "" {
		ref := strings.Split(a["networking.kubercloud.com/subnet"].(string), "/")
		subnet := api.Object("subnets", ref[0], ref[1])
		status := subnet["status"].(map[string]any)
		wasAllocated := status["v4usingIPrange"] == vip
		if deleted {
			status["v4usingIPrange"] = ""
			status["v4availableIPrange"] = "10.42.1.2-10.42.1.254"
		} else {
			status["v4usingIPrange"] = vip
			status["v4availableIPrange"] = "10.42.1.2-10.42.1.99,10.42.1.101-10.42.1.254"
		}
		if !deleted || wasAllocated {
			api.Change("subnets", subnet, false)
		}
	}
	if eipName, _ := a["networking.kubercloud.com/lb_eips"].(string); eipName != "" {
		eip := api.Object("eips", ns, eipName)
		status := eip["status"].(map[string]any)
		if deleted {
			status["phase"], status["boundResource"] = "Available", nil
		} else {
			status["phase"] = "Bound"
			status["boundResource"] = map[string]any{"resourceType": "Service", "resource": name, "vpc": a["networking.kubercloud.com/lb_vpc"], "observedGeneration": float64(0), "nodeName": "node-1"}
		}
		api.Change("eips", eip, false)
	}
}

func TestLBActualAdapterDeletePartialCreationWithoutGeneratedService(t *testing.T) {
	for _, phase := range []string{"backend", "envoy_proxy", "gateway"} {
		t.Run(phase, func(t *testing.T) {
			c := &lbControllerFixture{holdGenerated: true}
			f := newLBAdmissionFixture(t, c.http)
			var ns, subnet string
			if err := f.f.owner.QueryRow(f.f.ctx, `SELECT namespace,provider_name FROM network_provider_bindings WHERE tenant_id=$1 AND subnet_id=$2`, f.f.tenant, f.subnet.ID).Scan(&ns, &subnet); err != nil {
				t.Fatal(err)
			}
			obj := f.api.Object("subnets", ns, subnet)
			obj["status"].(map[string]any)["v4usingIPrange"] = ""
			obj["status"].(map[string]any)["v4availableIPrange"] = "10.42.1.2-10.42.1.254"
			f.api.Change("subnets", obj, false)
			before := f.api.Object("subnets", ns, subnet)["metadata"].(map[string]any)["resourceVersion"]
			r, err := f.lbs.Create(f.f.ctx, f.request)
			if err != nil {
				t.Fatal(err)
			}
			f.f.drive(t, func() bool {
				var uid string
				query := `SELECT provider_uid FROM network_lb_components WHERE tenant_id=$1 AND lb_id=$2 AND kind=$3`
				if phase == "gateway" {
					query = `SELECT provider_uid FROM network_provider_bindings WHERE tenant_id=$1 AND lb_id=$2 AND resource_kind=$3`
					return f.f.owner.QueryRow(f.f.ctx, query, f.f.tenant, r.LoadBalancer.ID, "load_balancer").Scan(&uid) == nil && uid != ""
				}
				return f.f.owner.QueryRow(f.f.ctx, query, f.f.tenant, r.LoadBalancer.ID, phase).Scan(&uid) == nil && uid != ""
			})
			if _, err = f.lbs.Delete(f.f.ctx, "", r.LoadBalancer.ID); err != nil {
				t.Fatal(err)
			}
			lbState(t, f, r.LoadBalancer.ID, biz.Deleted)
			if after := f.api.Object("subnets", ns, subnet)["metadata"].(map[string]any)["resourceVersion"]; before != after {
				t.Fatal("partial cleanup required a synthetic IPAM change", before, after)
			}
			var occupied int
			if err = f.f.owner.QueryRow(f.f.ctx, `SELECT count(*) FROM network_lb_vip_intents WHERE tenant_id=$1 AND lb_id=$2 AND released_at IS NULL`, f.f.tenant, r.LoadBalancer.ID).Scan(&occupied); err != nil || occupied != 0 {
				t.Fatal("partial cleanup leaked VIP", occupied, err)
			}
		})
	}
}
func lbState(t *testing.T, f *lbAdmissionFixture, id string, state biz.ResourceState) biz.LoadBalancer {
	t.Helper()
	var lb biz.LoadBalancer
	f.f.drive(t, func() bool {
		var err error
		lb, err = f.lbs.Get(f.f.ctx, "", id)
		return err == nil && lb.State == state
	})
	return lb
}
func TestLBActualAdapterThreeExposuresUpdateAndDelete(t *testing.T) {
	for _, scenario := range []struct{ exposure, flavor string }{{"private", "small"}, {"private", "medium"}, {"private", "large"}, {"public", "small"}, {"public_private", "small"}} {
		t.Run(scenario.exposure+"/"+scenario.flavor, func(t *testing.T) {
			exposure := scenario.exposure
			controller := &lbControllerFixture{}
			f := newLBAdmissionFixture(t, controller.http)
			request := f.request
			request.Exposure = exposure
			request.Flavor = scenario.flavor
			var eip biz.EIP
			if exposure != "private" {
				eip = f.f.eip(t, "lb-entry-eip")
				request.PublicEIPID = eip.ID
			}
			if exposure == "public" {
				request.PrivateIP = ""
			}
			r, err := f.lbs.Create(f.f.ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			lb := lbState(t, f, r.LoadBalancer.ID, biz.Available)
			var persistedFlavor string
			if err = f.f.owner.QueryRow(f.f.ctx, `SELECT flavor FROM network_load_balancers WHERE tenant_id=$1 AND lb_id=$2`, f.f.tenant, lb.ID).Scan(&persistedFlavor); err != nil || persistedFlavor != scenario.flavor || lb.Flavor != scenario.flavor {
				t.Fatal("selected flavor was not persisted", persistedFlavor, lb.Flavor, err)
			}
			if lb.ConfigurationState != "configured" || lb.AppliedVersion != 1 {
				t.Fatal("configuration was confused with health", lb)
			}
			op, err := f.lbs.GetOperation(f.f.ctx, "", r.Operation.ID)
			if err != nil || op.State != biz.Succeeded {
				t.Fatal(op, err)
			}
			if eip.ID != "" {
				f.f.drive(t, func() bool {
					v, err := f.f.e.GetEIP(f.f.ctx, "", eip.ID)
					return err == nil && v.State == biz.Available && v.BindingState == "bound" && v.BindingID == "" && v.BindingTarget != nil && v.BindingTarget.ID == lb.ID
				})
			}
			// Creation consumes the declared objects' controller status for
			// each flavor. Weight updates and
			// receipt replay share the same path across exposures; exercise them
			// once with both public and private entries present.
			if exposure == "public_private" && scenario.flavor == "small" {
				zero := uint32(0)
				update := biz.UpdateLoadBalancer{ID: lb.ID, ExpectedVersion: lb.Version, IdempotencyKey: "weight-zero", LoadBalancerMutableInput: biz.LoadBalancerMutableInput{Health: f.request.Health, Name: "updated", Backends: []biz.LoadBalancerBackendInput{{ID: lb.Backends[0].ID, SubnetID: lb.Backends[0].SubnetID, Address: lb.Backends[0].Address, Port: lb.Backends[0].Port, Weight: &zero}}}}
				updated, err := f.lbs.Update(f.f.ctx, update)
				if err != nil {
					t.Fatal(err)
				}
				f.f.drive(t, func() bool {
					lb, err = f.lbs.Get(f.f.ctx, "", lb.ID)
					return err == nil && lb.AppliedVersion == 2 && lb.ConfigurationState == "configured"
				})
				if lb.Backends[0].Weight != 0 {
					t.Fatal("weight zero lost")
				}
				replay, err := f.lbs.Update(f.f.ctx, update)
				if err != nil {
					t.Fatal(err)
				}
				// Compare the complete persisted receipt representation. A PG
				// timestamp in time.Local and its JSON replay in time.UTC can
				// represent the same instant while reflect.DeepEqual is false.
				acceptedJSON, err := json.Marshal(updated)
				if err != nil {
					t.Fatal(err)
				}
				replayJSON, err := json.Marshal(replay)
				if err != nil {
					t.Fatal(err)
				}
				if string(acceptedJSON) != string(replayJSON) {
					t.Fatalf("update receipt was not immutable: accepted=%s replay=%s", acceptedJSON, replayJSON)
				}
			}
			del, err := f.lbs.Delete(f.f.ctx, "", lb.ID)
			if err != nil {
				t.Fatal(err)
			}
			lbState(t, f, lb.ID, biz.Deleted)
			op, err = f.lbs.GetOperation(f.f.ctx, "", del.Operation.ID)
			if err != nil || op.State != biz.Succeeded {
				t.Fatal("delete operation incomplete", op, err)
			}
			var occupied int
			if err = f.f.owner.QueryRow(f.f.ctx, `SELECT (SELECT count(*) FROM network_lb_vip_intents WHERE tenant_id=$1 AND lb_id=$2 AND released_at IS NULL)+(SELECT count(*) FROM network_eip_claims WHERE tenant_id=$1 AND lb_id=$2 AND released_at IS NULL)+(SELECT count(*) FROM network_lb_subnet_refs WHERE tenant_id=$1 AND lb_id=$2 AND released_at IS NULL)`, f.f.tenant, lb.ID).Scan(&occupied); err != nil || occupied != 0 {
				t.Fatal("occupancy leaked", occupied, err)
			}
			if apiPod := f.api.Object("pods", "tenant-"+f.f.tenant, "lb-backend"); apiPod == nil {
				t.Fatal("business owner Pod deleted")
			}
			baseState(t, f.f, f.vpc.ID, biz.Available)
			if eip.ID != "" {
				f.f.drive(t, func() bool {
					v, err := f.f.e.GetEIP(f.f.ctx, "", eip.ID)
					return err == nil && v.State == biz.Available && v.BindingState == "unbound"
				})
			}
			controller.mu.Lock()
			deletes := []string{}
			for _, request := range controller.requests {
				if strings.HasPrefix(request, "DELETE/") {
					deletes = append(deletes, strings.Split(request, "/")[1])
				}
			}
			controller.mu.Unlock()
			if !reflect.DeepEqual(deletes, []string{"httproutes", "backendtrafficpolicies", "gateways", "backends", "envoyproxies"}) {
				t.Fatal("wrong deletion order", deletes)
			}
		})
	}
}

func TestLBActualAdapterIdentityLossRemovesRouteAndRetainsAppliedVersion(t *testing.T) {
	c := &lbControllerFixture{}
	f := newLBAdmissionFixture(t, c.http)
	r, err := f.lbs.Create(f.f.ctx, f.request)
	if err != nil {
		t.Fatal(err)
	}
	lb := lbState(t, f, r.LoadBalancer.ID, biz.Available)
	var ns, route string
	if err = f.f.owner.QueryRow(f.f.ctx, `SELECT namespace,provider_name FROM network_lb_components WHERE tenant_id=$1 AND lb_id=$2 AND kind='route'`, f.f.tenant, lb.ID).Scan(&ns, &route); err != nil {
		t.Fatal(err)
	}
	setLBCurrentBackendAddresses(t, f, lb.ID, lb.Backends[0].AttachmentID, []string{})
	f.f.drive(t, func() bool {
		lb, err = f.lbs.Get(f.f.ctx, "", lb.ID)
		obj := f.api.Object("httproutes", ns, route)
		if err != nil || obj == nil {
			return false
		}
		rule := obj["spec"].(map[string]any)["rules"].([]any)[0].(map[string]any)
		return lb.State == biz.Degraded && rule["backendRefs"] == nil
	})
	if lb.AppliedVersion != 1 || lb.Reason != biz.BackendIdentityMismatch || lb.Backends[0].State != "unavailable" {
		t.Fatal("replacement address became a legitimate target", lb)
	}
	if _, err = f.lbs.Delete(f.f.ctx, "", lb.ID); err != nil {
		t.Fatal(err)
	}
	lbState(t, f, lb.ID, biz.Deleted)
}

func TestLBActualAdapterFinalizerAndNeverSentDelete(t *testing.T) {
	for _, neverSent := range []bool{true, false} {
		t.Run(strconv.FormatBool(neverSent), func(t *testing.T) {
			c := &lbControllerFixture{}
			f := newLBAdmissionFixture(t, c.http)
			r, err := f.lbs.Create(f.f.ctx, f.request)
			if err != nil {
				t.Fatal(err)
			}
			if !neverSent {
				lbState(t, f, r.LoadBalancer.ID, biz.Available)
				c.mu.Lock()
				c.holdCleanup = true
				c.mu.Unlock()
			}
			if _, err = f.lbs.Delete(f.f.ctx, "", r.LoadBalancer.ID); err != nil {
				t.Fatal(err)
			}
			if neverSent {
				lbState(t, f, r.LoadBalancer.ID, biz.Deleted)
				c.mu.Lock()
				defer c.mu.Unlock()
				for _, request := range c.requests {
					if strings.HasPrefix(request, "POST/") {
						t.Fatal("never-sent delete created CR", request)
					}
				}
				return
			}
			f.f.drive(t, func() bool {
				v, err := f.lbs.Get(f.f.ctx, "", r.LoadBalancer.ID)
				return err == nil && v.Reason == biz.CleanupPending
			})
			var refs int
			if err = f.f.owner.QueryRow(f.f.ctx, `SELECT count(*) FROM network_lb_vip_intents WHERE tenant_id=$1 AND lb_id=$2 AND released_at IS NULL`, f.f.tenant, r.LoadBalancer.ID).Scan(&refs); err != nil || refs != 1 {
				t.Fatal("finalizer released occupancy", refs, err)
			}
			// The controller fixture resumes its own finalizer. Network never clears it.
			var ns, name string
			if err = f.f.owner.QueryRow(f.f.ctx, `SELECT namespace,provider_name FROM network_lb_components WHERE tenant_id=$1 AND lb_id=$2 AND kind='route'`, f.f.tenant, r.LoadBalancer.ID).Scan(&ns, &name); err != nil {
				t.Fatal(err)
			}
			obj := f.api.Object("httproutes", ns, name)
			f.api.Change("httproutes", obj, true)
			c.mu.Lock()
			c.holdCleanup = false
			c.mu.Unlock()
			lbState(t, f, r.LoadBalancer.ID, biz.Deleted)
		})
	}
}
