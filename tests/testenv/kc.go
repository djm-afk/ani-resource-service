package testenv

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/google/uuid"
)

// KC is a controlled Kubernetes HTTP server for adapter/process tests. It
// supplies the pinned CR contract, never evidence of controller or OVN behavior.
// Objects and counters are retained while client processes are restarted.
type KC struct {
	Mu               sync.Mutex
	Objects          map[string]map[string]any
	Creates, Deletes map[string]int
	Invalid          string
}

func (s *KC) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if s.Objects == nil {
		s.Objects = map[string]map[string]any{}
		s.Creates = map[string]int{}
		s.Deletes = map[string]int{}
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	offset := 3
	if len(parts) > 0 && parts[0] == "api" {
		offset = 2
	}
	if len(parts) <= offset {
		w.WriteHeader(404)
		return
	}
	kind, ns, name := parts[offset], "", ""
	if kind == "namespaces" && len(parts) > offset+2 {
		ns = parts[offset+1]
		kind = parts[offset+2]
		if len(parts) > offset+3 {
			name = parts[offset+3]
		}
	} else if len(parts) > offset+1 {
		name = parts[offset+1]
	}
	kinds := map[string]string{"namespaces": "Namespace", "vpcs": "VPC", "subnets": "Subnet", "vnics": "VNic", "vnicips": "VNicIP", "eips": "EIP", "pods": "Pod", "notebooks": "Notebook", "statefulsets": "StatefulSet", "nics": "Nic", "snats": "Snat", "nats": "Nat", "eipgateways": "EIPGateway", "vlannetworks": "VlanNetwork", "nodes": "Node", "configmaps": "ConfigMap", "services": "Service", "servicecidrs": "ServiceCIDR"}
	objectKind, ok := kinds[kind]
	if !ok {
		s.Invalid = "unexpected resource " + kind
		w.WriteHeader(404)
		return
	}
	apiVersion := "networking.kubercloud.com/v1"
	if kind == "namespaces" || kind == "pods" || kind == "nodes" || kind == "configmaps" || kind == "services" {
		apiVersion = "v1"
	}
	if kind == "notebooks" {
		apiVersion = "kubeflow.org/v1"
	}
	if kind == "statefulsets" {
		apiVersion = "apps/v1"
	}
	key := kind + "/" + ns + "/" + name
	fail := func(code int, reason string) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": "v1", "kind": "Status", "status": "Failure", "code": code, "reason": reason})
	}
	if r.Method == "GET" && name == "" {
		items := []any{}
		for k, o := range s.Objects {
			if strings.HasPrefix(k, kind+"/") && (ns == "" || strings.HasPrefix(k, kind+"/"+ns+"/")) {
				items = append(items, o)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": apiVersion, "kind": objectKind + "List", "metadata": map[string]any{}, "items": items})
		return
	}
	switch r.Method {
	case "GET":
		o := s.Objects[key]
		if o == nil {
			fail(404, "NotFound")
			return
		}
		_ = json.NewEncoder(w).Encode(o)
	case "POST":
		var o map[string]any
		if err := json.NewDecoder(r.Body).Decode(&o); err != nil {
			fail(400, "BadRequest")
			return
		}
		meta, _ := o["metadata"].(map[string]any)
		objectName, _ := meta["name"].(string)
		if meta == nil || objectName == "" || o["kind"] != objectKind || (ns != "" && meta["namespace"] != ns) {
			s.Invalid = "object identity mismatch"
			fail(422, "Invalid")
			return
		}
		key = kind + "/" + ns + "/" + objectName
		if s.Objects[key] != nil {
			fail(409, "AlreadyExists")
			return
		}
		if kind == "subnets" && o["spec"].(map[string]any)["type"] != "Public" {
			spec, _ := o["spec"].(map[string]any)
			allowed, _ := spec["allowedNamespaces"].(map[string]any)
			parent, _ := spec["gateway"].(string)
			if spec["type"] != "VPC" || spec["ipVersion"] != "IPv4" || allowed["from"] != "Same" || spec["gatewayIP"] == "" || s.Objects["vpcs/"+parent] == nil {
				s.Invalid = "subnet contract mismatch"
				fail(422, "Invalid")
				return
			}
		}
		s.Creates[kind]++
		meta["uid"] = uuid.NewString()
		meta["generation"] = 1
		meta["resourceVersion"] = strconv.Itoa(s.Creates[kind])
		if kind == "vpcs" || kind == "subnets" {
			bound := map[string]any{"router": "test-router"}
			if kind == "subnets" {
				bound = map[string]any{"switch": "test-switch", "gatewayPort": "test-port"}
			}
			o["status"] = map[string]any{"observedGeneration": 1, "boundResources": bound, "conditions": []any{map[string]any{"type": "Valid", "status": "True"}, map[string]any{"type": "Initialized", "status": "True"}, map[string]any{"type": "Ready", "status": "True"}}}
		}
		if !s.egressCreated(kind, ns, o) {
			s.Invalid = "egress contract mismatch"
			fail(422, "Invalid")
			return
		}
		s.Objects[key] = o
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(o)
	case "PATCH":
		s.egressPatch(w, r, key, kind, fail)
	case "DELETE":
		o := s.Objects[key]
		if o == nil {
			fail(404, "NotFound")
			return
		}
		var options struct {
			Preconditions     struct{ UID, ResourceVersion string }
			PropagationPolicy string
		}
		if err := json.NewDecoder(r.Body).Decode(&options); err != nil {
			fail(400, "BadRequest")
			return
		}
		meta := o["metadata"].(map[string]any)
		// Native garbage collection owns derived objects. Res still supplies
		// the original UID and resourceVersion for its declaration deletion.
		validPropagation := options.PropagationPolicy == "" || options.PropagationPolicy == "Background" || options.PropagationPolicy == "Orphan"
		if options.Preconditions.UID != meta["uid"] || options.Preconditions.ResourceVersion != meta["resourceVersion"] || !validPropagation {
			s.Invalid = fmt.Sprintf("unconditional deletion of %s", kind)
			fail(409, "Conflict")
			return
		}
		if kind == "snats" {
			spec := o["spec"].(map[string]any)
			if eip := s.Objects["eips/"+ns+"/"+fmt.Sprint(spec["eip"])]; eip != nil {
				status := eip["status"].(map[string]any)
				status["boundResource"] = nil
				status["phase"] = "Available"
			}
		}
		s.Deletes[kind]++
		delete(s.Objects, key)
		_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": "v1", "kind": "Status", "status": "Success"})
	default:
		s.Invalid = "unexpected mutation " + r.Method
		fail(405, "MethodNotAllowed")
	}
}
