package testenv

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"strconv"
)

// Controlled contract only: this is never a provider implementation or live
// networking evidence. Tests can change any status independently of its spec.
func fixtureConditions() []any {
	return []any{map[string]any{"type": "Valid", "status": "True"}, map[string]any{"type": "Initialized", "status": "True"}, map[string]any{"type": "Ready", "status": "True"}}
}
func (s *KC) egressCreated(kind, ns string, o map[string]any) bool {
	spec, _ := o["spec"].(map[string]any)
	m := o["metadata"].(map[string]any)
	switch kind {
	case "eipgateways":
		if ns != "" || spec["scope"] != "Public" || spec["egressType"] != "Host" || spec["type"] != nil {
			return false
		}
		o["status"] = map[string]any{"conditions": fixtureConditions(), "localIP": "100.64.0.2", "boundResources": map[string]any{"router": "fixture-egress-router"}}
	case "vlannetworks":
		if ns != "" || spec["devName"] == nil || spec["vlanID"] == nil {
			return false
		}
		o["status"] = map[string]any{"conditions": fixtureConditions(), "notReadyNodes": []any{}}
	case "subnets":
		if spec["type"] != "Public" {
			return true
		}
		allowed, _ := spec["allowedNamespaces"].(map[string]any)
		gw, _ := spec["gateway"].(string)
		if ns != "kcn-system" || allowed["from"] != "All" || s.Objects["eipgateways//"+gw] == nil {
			return false
		}
		if underlay, ok := spec["underlayConfig"].(map[string]any); ok {
			vlan := s.Objects["vlannetworks//"+fmt.Sprint(underlay["vlanNetwork"])]
			if vlan == nil || underlay["gatewayIP"] == nil {
				return false
			}
			vlan["status"].(map[string]any)["subnet"] = ns + "/" + fmt.Sprint(m["name"])
			o["status"].(map[string]any)["underlayState"] = map[string]any{"ready": true, "chassisNode": "fixture-node", "notReadyNodes": []any{}}
		}
	case "eips":
		poolRef, _ := spec["subnet"].(string)
		pool := s.Objects["subnets/"+poolRef]
		if pool == nil || spec["ipVersion"] != "IPv4" || spec["ipAddress"] != nil {
			return false
		}
		ps := pool["spec"].(map[string]any)
		cidr, err := netip.ParsePrefix(fmt.Sprint(ps["cidrBlock"]))
		if err != nil {
			return false
		}
		ip := cidr.Addr()
		for i := 0; i < 10+s.Creates[kind]; i++ {
			ip = ip.Next()
		}
		spec["ipAddress"] = ip.String()
		m["generation"] = 2
		o["status"] = map[string]any{"conditions": fixtureConditions(), "phase": "Available", "gateway": ps["gateway"], "boundResource": nil}
	case "snats":
		if spec["cidrs"] != nil {
			return false
		}
		return s.applyFixtureSnat(ns, o)
	}
	return true
}
func (s *KC) applyFixtureSnat(ns string, o map[string]any) bool {
	spec := o["spec"].(map[string]any)
	eip := s.Objects["eips/"+ns+"/"+fmt.Sprint(spec["eip"])]
	vpc := s.Objects["vpcs/"+ns+"/"+fmt.Sprint(spec["vpc"])]
	if eip == nil || vpc == nil {
		return false
	}
	es := eip["status"].(map[string]any)
	m := o["metadata"].(map[string]any)
	disabled, _ := spec["disable"].(bool)
	phase, node := "Bound", "fixture-worker"
	if disabled {
		phase, node = "Disabled", ""
	}
	es["phase"] = "Bound"
	es["boundResource"] = map[string]any{"resourceType": "Snat", "resource": m["name"], "vpc": ns + "/" + fmt.Sprint(spec["vpc"]), "nodeName": node, "disabled": disabled, "observedGeneration": m["generation"]}
	o["status"] = map[string]any{"conditions": fixtureConditions(), "phase": phase, "eipAddress": eip["spec"].(map[string]any)["ipAddress"]}
	return true
}
func (s *KC) egressPatch(w http.ResponseWriter, r *http.Request, key, kind string, fail func(int, string)) {
	if kind == "configmaps" {
		s.fixtureConfigPatch(w, r, key, fail)
		return
	}
	o := s.Objects[key]
	if o == nil {
		fail(404, "NotFound")
		return
	}
	if kind == "vnics" {
		var patch []struct {
			Op, Path string
			Value    any
		}
		if json.NewDecoder(r.Body).Decode(&patch) != nil || len(patch) != 4 || patch[0].Op != "test" || patch[0].Path != "/metadata/uid" || patch[1].Op != "test" || patch[1].Path != "/metadata/resourceVersion" || patch[2].Op != "add" || patch[2].Path != "/metadata/annotations" || patch[3].Op != "add" || patch[3].Path != "/spec/securityGroups" {
			fail(422, "Invalid")
			return
		}
		m := o["metadata"].(map[string]any)
		if patch[0].Value != m["uid"] || patch[1].Value != m["resourceVersion"] {
			fail(409, "Conflict")
			return
		}
		annotations, ok := patch[2].Value.(map[string]any)
		if !ok {
			fail(422, "Invalid")
			return
		}
		groups, ok := patch[3].Value.([]any)
		if !ok {
			fail(422, "Invalid")
			return
		}
		m["annotations"] = annotations
		o["spec"].(map[string]any)["securityGroups"] = groups
		rv, _ := strconv.Atoi(fmt.Sprint(m["resourceVersion"]))
		m["resourceVersion"] = strconv.Itoa(rv + 1)
		_ = json.NewEncoder(w).Encode(o)
		return
	}
	if kind != "snats" {
		fail(405, "MethodNotAllowed")
		return
	}
	var patch []struct {
		Op, Path string
		Value    any
	}
	if json.NewDecoder(r.Body).Decode(&patch) != nil || len(patch) != 3 || patch[0].Op != "test" || patch[0].Path != "/metadata/uid" || patch[1].Op != "test" || patch[1].Path != "/metadata/resourceVersion" || patch[2].Op != "add" || patch[2].Path != "/spec/disable" {
		s.Invalid = "patch missing identity preconditions"
		fail(422, "Invalid")
		return
	}
	m := o["metadata"].(map[string]any)
	if patch[0].Value != m["uid"] || patch[1].Value != m["resourceVersion"] {
		fail(409, "Conflict")
		return
	}
	disabled, ok := patch[2].Value.(bool)
	if !ok {
		fail(422, "Invalid")
		return
	}
	o["spec"].(map[string]any)["disable"] = disabled
	generation, _ := strconv.Atoi(fmt.Sprint(m["generation"]))
	rv, _ := strconv.Atoi(fmt.Sprint(m["resourceVersion"]))
	m["generation"] = generation + 1
	m["resourceVersion"] = strconv.Itoa(rv + 1)
	if !s.applyFixtureSnat(fmt.Sprint(m["namespace"]), o) {
		fail(422, "Invalid")
		return
	}
	_ = json.NewEncoder(w).Encode(o)
}

func (s *KC) fixtureConfigPatch(w http.ResponseWriter, r *http.Request, key string, fail func(int, string)) {
	o := s.Objects[key]
	if o == nil {
		fail(404, "NotFound")
		return
	}
	m := o["metadata"].(map[string]any)
	if m["name"] != "kcn-config" {
		fail(405, "MethodNotAllowed")
		return
	}
	var patch []struct {
		Op, Path string
		Value    any
	}
	if json.NewDecoder(r.Body).Decode(&patch) != nil || (len(patch) != 3 && len(patch) != 4) || patch[0].Op != "test" || patch[0].Path != "/metadata/uid" || patch[0].Value != m["uid"] || patch[1].Op != "test" || patch[1].Path != "/metadata/resourceVersion" || patch[1].Value != m["resourceVersion"] {
		fail(409, "Conflict")
		return
	}
	last := len(patch) - 1
	if patch[last].Op != "add" || patch[last].Path != "/metadata/annotations" || (len(patch) == 4 && (patch[2].Op != "add" || patch[2].Path != "/data/managedDevices")) {
		s.Invalid = "unexpected ConfigMap mutation"
		fail(422, "Invalid")
		return
	}
	if _, ok := patch[last].Value.(map[string]any); !ok {
		fail(422, "Invalid")
		return
	}
	if len(patch) == 4 {
		if _, ok := patch[2].Value.(string); !ok {
			fail(422, "Invalid")
			return
		}
		o["data"].(map[string]any)["managedDevices"] = patch[2].Value
	}
	m["annotations"] = patch[last].Value
	rv, _ := strconv.Atoi(fmt.Sprint(m["resourceVersion"]))
	m["resourceVersion"] = strconv.Itoa(rv + 1)
	_ = json.NewEncoder(w).Encode(o)
}
