package data

import (
	"context"
	"strings"

	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func (p *KCProvider) lbGenerated(ctx context.Context, r lbResolved, o *biz.LoadBalancerObservation) error {
	l := r.work.Resource.LoadBalancer.LoadBalancer
	flavor, ok := biz.FindLoadBalancerFlavor(l.Flavor)
	if !ok {
		return &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	namespace := r.binding.Namespace
	gateway := lbFind(r.objects[lbGateways], namespace, r.binding.ProviderName)
	// Existing six-class installations need an explicit migration. Never patch
	// their Gateway class or create a new proxy behind the old live data plane.
	if gateway != nil && crString(gateway, "spec", "gatewayClassName") != biz.LoadBalancerGatewayClass && r.work.Resource.State != biz.Deleting && r.work.Resource.State != biz.Deleted && len(biz.ProjectLoadBalancerListeners(biz.EffectiveLoadBalancerListeners(l))) > 0 {
		return &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	uid := r.binding.ProviderUid
	if gateway != nil {
		uid = string(gateway.GetUID())
	}
	prior, err := p.repository.queries.ListLBGenerated(ctx, sqlcgen.ListLBGeneratedParams{TenantID: l.TenantID, LbID: l.ID})
	if err != nil {
		return readFailure(err)
	}
	// Released descendants belong to a retired Gateway incarnation. Keep their
	// durable tombstones, but only the current Gateway can prove readiness or
	// release for the current binding.
	current := prior[:0]
	for _, g := range prior {
		if g.GatewayUid != uid && g.ReleasedAt != nil {
			continue
		}
		current = append(current, g)
	}
	prior = current
	// All owner edges are scoped to this namespace and typed kind/name/UID.
	// Previously observed intermediate UIDs keep orphaned descendants visible.
	type identity struct{ kind, name, uid string }
	owners := map[string]identity{}
	if uid != "" {
		owners[uid] = identity{"Gateway", r.binding.ProviderName, uid}
	}
	for _, g := range prior {
		if g.GatewayUid != uid || g.Namespace != namespace {
			return &biz.ProviderError{Kind: biz.ProviderConflict}
		}
		owners[g.ProviderUid] = identity{g.Kind, g.ProviderName, g.ProviderUid}
	}
	seen := map[string]bool{}
	allGone := true
	var service, deployment *unstructured.Unstructured
	var proxyPods []*unstructured.Unstructured
	gvrs := []schema.GroupVersionResource{kcServices, lbDeployments, lbReplicaSets, pods, lbEndpointSlices, observationGVRs[3], observationGVRs[4]}
	for _, gvr := range gvrs {
		for i := range r.objects[gvr] {
			obj := &r.objects[gvr][i]
			if obj.GetNamespace() != namespace {
				continue
			}
			owned := false
			for _, ref := range obj.GetOwnerReferences() {
				parent, known := owners[string(ref.UID)]
				if known && parent.kind == ref.Kind && parent.name == ref.Name && ref.APIVersion == lbOwnerAPIVersion(parent.kind) {
					allowed := (obj.GetKind() == "Service" || obj.GetKind() == "Deployment") && parent.kind == "Gateway" || obj.GetKind() == "ReplicaSet" && parent.kind == "Deployment" || obj.GetKind() == "Pod" && parent.kind == "ReplicaSet" || obj.GetKind() == "EndpointSlice" && parent.kind == "Service" || obj.GetKind() == "VNic" && parent.kind == "Pod" || obj.GetKind() == "VNicIP" && parent.kind == "VNic"
					owned = owned || allowed
				}
			}
			if known, ok := owners[string(obj.GetUID())]; ok && known.kind == obj.GetKind() && known.name == obj.GetName() {
				if !owned && len(obj.GetOwnerReferences()) > 0 {
					return &biz.ProviderError{Kind: biz.ProviderConflict}
				}
				owned = true
			}
			for _, old := range prior {
				if (gvr == kcServices || gvr == lbDeployments) && old.Kind == obj.GetKind() && old.ProviderName == obj.GetName() && old.ProviderUid != string(obj.GetUID()) {
					return &biz.ProviderError{Kind: biz.ProviderConflict}
				}
			}
			// These are fixed GatewayNamespace names; an unrelated same-name
			// Service/Deployment must block, never be adopted or deleted.
			if (gvr == kcServices || gvr == lbDeployments) && obj.GetName() == r.binding.ProviderName && !owned {
				return &biz.ProviderError{Kind: biz.ProviderConflict}
			}
			if !owned {
				continue
			}
			if obj.GetUID() == "" || uid == "" {
				return &biz.ProviderError{Kind: biz.ProviderConflict}
			}
			allGone = false
			owners[string(obj.GetUID())] = identity{obj.GetKind(), obj.GetName(), string(obj.GetUID())}
			seen[obj.GetKind()+"/"+string(obj.GetUID())] = true
			o.Generated = append(o.Generated, biz.LoadBalancerGeneratedResource{Kind: obj.GetKind(), Namespace: namespace, Name: obj.GetName(), Identity: string(obj.GetUID()), GatewayIdentity: uid})
			if gvr == kcServices {
				if service != nil {
					return &biz.ProviderError{Kind: biz.ProviderConflict}
				}
				service = obj
			}
			if gvr == lbDeployments {
				if deployment != nil {
					return &biz.ProviderError{Kind: biz.ProviderConflict}
				}
				deployment = obj
			}
			if gvr == pods {
				proxyPods = append(proxyPods, obj)
			}
		}
	}
	for _, g := range prior {
		if !seen[g.Kind+"/"+g.ProviderUid] {
			o.Generated = append(o.Generated, biz.LoadBalancerGeneratedResource{Kind: g.Kind, Namespace: g.Namespace, Name: g.ProviderName, Identity: g.ProviderUid, GatewayIdentity: g.GatewayUid, Released: true})
		}
	}
	o.GeneratedReleased = allGone && gateway == nil
	serviceReady := false
	if service != nil {
		if !lbServiceMatches(service, r) {
			return &biz.ProviderError{Kind: biz.ProviderConflict}
		}
		serviceReady = service.GetDeletionTimestamp() == nil && lbServiceConditions(service, "KcnValid", "KcnReady")
	}
	ipReleased, ipAllocated := true, true
	if l.PublicEIPID != "" {
		eip := lbFind(r.objects[kcEIPs], namespace, r.eip.ProviderName)
		if eip == nil || string(eip.GetUID()) != r.eip.ProviderUid || crString(eip, "spec", "ipAddress") != l.PublicAddress {
			return &biz.ProviderError{Kind: biz.ProviderConflict}
		}
		conflict := lbAddressConflict(r, service) || lbEIPBindingConflict(eip, r)
		if conflict {
			return &biz.ProviderError{Kind: biz.ProviderConflict}
		}
		ipReleased = goodConditions(eip, "Valid", "Initialized") && emptyBoundResource(eip) && crString(eip, "status", "phase") == "Available"
		ipAllocated = lbEIPBound(eip, service, r)
		if !ipReleased && !ipAllocated {
			o.DependenciesReady = false
		}
	}
	if l.PrivateIP != "" {
		subnet := lbFind(r.objects[kcSubnets], namespace, r.subnet.ProviderName)
		used, usedOK := lbIPInRange(l.PrivateIP, crString(subnet, "status", "v4usingIPrange"))
		free, freeOK := lbIPInRange(l.PrivateIP, crString(subnet, "status", "v4availableIPrange"))
		valid := subnet != nil && string(subnet.GetUID()) == r.subnet.ProviderUid && usedOK && freeOK && goodConditions(subnet, "Valid", "Initialized", "Ready") && crInt(subnet, "status", "observedGeneration") == subnet.GetGeneration()
		vipAllocated := serviceReady && valid && used && !free
		if vipAllocated {
			o.VIPOccupiedRevision = subnet.GetResourceVersion()
		}
		ipAllocated = ipAllocated && vipAllocated
		vipReleased := false
		neverSent := false
		for _, c := range r.work.Resource.LoadBalancer.Components {
			if c.Kind == "gateway" {
				neverSent = !c.CreateDispatched && c.PendingAction == "" && c.Identity == ""
			}
		}
		if neverSent && o.GeneratedReleased {
			vipReleased = true
		} else if o.GeneratedReleased && valid {
			o.VIPAbsenceRevision = subnet.GetResourceVersion()
			checkpoint := r.work.Resource.LoadBalancer.VIPOccupiedRevision
			// A Gateway may be deleted before the controller creates a Service.
			// With no observed allocation, complete generated-object absence and
			// a fresh, valid free range suffice; an unrelated Subnet change is
			// neither required nor evidence of release. If allocation was seen,
			// the free fact must supersede that occupied revision. Unknown POSTs
			// remain fenced by the component's persistent pending mutation.
			vipReleased = (checkpoint == "" || subnet.GetResourceVersion() != checkpoint) && free && !used
		}
		ipReleased = ipReleased && vipReleased
	}
	o.AddressReleased = o.GeneratedReleased && ipReleased
	deploymentReady := deployment != nil && deployment.GetDeletionTimestamp() == nil && crInt(deployment, "spec", "replicas") == flavor.Replicas && crInt(deployment, "status", "observedGeneration") == deployment.GetGeneration() && crInt(deployment, "status", "updatedReplicas") == flavor.Replicas && crInt(deployment, "status", "availableReplicas") == flavor.Replicas
	readyPods := 0
	proxyByUID := map[string]*unstructured.Unstructured{}
	for _, pod := range proxyPods {
		if pod.GetDeletionTimestamp() != nil || crString(pod, "status", "phase") != "Running" {
			continue
		}
		if expected := p.loadBalancerInstallation; expected != nil {
			valid := map[string]bool{}
			statuses, _, _ := unstructured.NestedSlice(pod.Object, "status", "containerStatuses")
			for _, raw := range statuses {
				c, _ := raw.(map[string]any)
				if c["name"] == "envoy" {
					valid["envoy"] = c["imageID"] == expected.EnvoyImageID
				}
				if c["name"] == "shutdown-manager" {
					valid["shutdown"] = c["imageID"] == expected.ShutdownImageID
				}
			}
			if !valid["envoy"] || !valid["shutdown"] {
				continue
			}
		}
		conditions, _, _ := unstructured.NestedSlice(pod.Object, "status", "conditions")
		for _, raw := range conditions {
			c, _ := raw.(map[string]any)
			if c["type"] == "Ready" && c["status"] == "True" {
				readyPods++
				proxyByUID[string(pod.GetUID())] = pod
			}
		}
	}
	resourcesMatch := false
	if deployment != nil {
		containers, _, _ := unstructured.NestedSlice(deployment.Object, "spec", "template", "spec", "containers")
		for _, raw := range containers {
			c, _ := raw.(map[string]any)
			if c["name"] == "envoy" {
				obj := &unstructured.Unstructured{Object: c}
				resourcesMatch = lbQuantityEquals(obj, flavor.RequestCPU, "resources", "requests", "cpu") && lbQuantityEquals(obj, flavor.RequestMemory, "resources", "requests", "memory") && lbQuantityEquals(obj, flavor.LimitCPU, "resources", "limits", "cpu") && lbQuantityEquals(obj, flavor.LimitMemory, "resources", "limits", "memory")
			}
		}
	}
	slicePods := map[uint32]map[string]bool{}
	targetPorts, portsReady := lbServiceTargets(service, l, proxyByUID)
	for target := range targetPorts {
		slicePods[target] = map[string]bool{}
	}
	if service != nil {
		for i := range r.objects[lbEndpointSlices] {
			slice := &r.objects[lbEndpointSlices][i]
			if slice.GetNamespace() != namespace || !lbOwnedBy(slice, "Service", service.GetName(), string(service.GetUID())) {
				continue
			}
			if slice.GetLabels()["kubernetes.io/service-name"] != service.GetName() || crString(slice, "addressType") != "IPv4" || slice.GetDeletionTimestamp() != nil {
				continue
			}
			ports, _, _ := unstructured.NestedSlice(slice.Object, "ports")
			slicePorts := []uint32{}
			for _, raw := range ports {
				port, _ := raw.(map[string]any)
				value, _, _ := unstructured.NestedInt64(port, "port")
				name, _ := port["name"].(string)
				if value > 0 && value <= 65535 && slicePods[uint32(value)] != nil && port["protocol"] == lbServiceProtocol(service, name) && name == targetPorts[uint32(value)] {
					slicePorts = append(slicePorts, uint32(value))
				} else {
					portsReady = false
				}
			}
			if len(slicePorts) == 0 {
				continue
			}
			endpoints, _, _ := unstructured.NestedSlice(slice.Object, "endpoints")
			for _, raw := range endpoints {
				e, _ := raw.(map[string]any)
				target, _, _ := unstructured.NestedMap(e, "targetRef")
				id, _ := target["uid"].(string)
				pod := proxyByUID[id]
				ready, _, _ := unstructured.NestedBool(e, "conditions", "ready")
				if pod == nil || !ready || target["kind"] != "Pod" || target["namespace"] != namespace || target["name"] != pod.GetName() {
					continue
				}
				addresses, _, _ := unstructured.NestedStringSlice(e, "addresses")
				if len(addresses) == 1 && addresses[0] == crString(pod, "status", "podIP") && addresses[0] != "" {
					for _, port := range slicePorts {
						slicePods[port][id] = true
					}
				}
			}
		}
	}
	for _, pods := range slicePods {
		portsReady = portsReady && int64(len(pods)) == flavor.Replicas
	}
	o.GeneratedReady = serviceReady && deploymentReady && int64(readyPods) == flavor.Replicas && ipAllocated && resourcesMatch && portsReady
	return nil
}
func lbServiceConditions(obj *unstructured.Unstructured, kinds ...string) bool {
	// Core Service has no metadata generation. kc publishes its specific
	// conditions without observedGeneration; the fresh read carries the UID.
	conditions, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	good := map[string]bool{}
	for _, raw := range conditions {
		c, _ := raw.(map[string]any)
		kind, _ := c["type"].(string)
		good[kind] = c["status"] == "True"
	}
	for _, kind := range kinds {
		if !good[kind] {
			return false
		}
	}
	return true
}
func lbServiceMatches(s *unstructured.Unstructured, r lbResolved) bool {
	l := r.work.Resource.LoadBalancer.LoadBalancer
	if s.GetName() != r.binding.ProviderName || s.GetNamespace() != r.binding.Namespace || s.GetUID() == "" {
		return false
	}
	expected := "LoadBalancer"
	if l.Exposure == "private" {
		expected = "ClusterIP"
	}
	if crString(s, "spec", "type") != expected {
		return false
	}
	annotations := s.GetAnnotations()
	vip := l.PrivateIP
	if vip == "" {
		vip = "disable"
	}
	if annotations["networking.kubercloud.com/lb_vpc"] != r.vpc.Namespace+"/"+r.vpc.ProviderName || annotations["networking.kubercloud.com/subnet"] != r.subnet.Namespace+"/"+r.subnet.ProviderName || annotations["networking.kubercloud.com/lb_vip_address"] != vip || annotations["networking.kubercloud.com/lb_eips"] != r.eip.ProviderName {
		return false
	}
	return true
}

// Service exposes listener ports, while EndpointSlices contain resolved Pod
// target ports. Numeric and named targetPort mappings are both verified.
func lbServiceTargets(s *unstructured.Unstructured, l biz.LoadBalancer, pods map[string]*unstructured.Unstructured) (map[uint32]string, bool) {
	result := map[uint32]string{}
	if s == nil {
		return result, false
	}
	ports, _, _ := unstructured.NestedSlice(s.Object, "spec", "ports")
	wanted := map[int64]string{}
	for _, listener := range biz.ProjectLoadBalancerListeners(biz.EffectiveLoadBalancerListeners(l)) {
		protocol := "TCP"
		if listener.Protocol == "UDP" {
			protocol = "UDP"
		}
		wanted[int64(listener.Port)] = protocol
	}
	if len(ports) != len(wanted) {
		return result, false
	}
	for _, raw := range ports {
		port, ok := raw.(map[string]any)
		if !ok {
			return result, false
		}
		exposed, _, _ := unstructured.NestedInt64(port, "port")
		protocol := wanted[exposed]
		if protocol == "" || port["protocol"] != protocol {
			return result, false
		}
		delete(wanted, exposed)
		target := exposed
		if value, exists := port["targetPort"]; exists {
			switch value := value.(type) {
			case int64:
				target = value
			case string:
				if len(pods) == 0 || value == "" {
					return result, false
				}
				target = 0
				for _, pod := range pods {
					containers, _, _ := unstructured.NestedSlice(pod.Object, "spec", "containers")
					resolved := int64(0)
					for _, raw := range containers {
						container, ok := raw.(map[string]any)
						if !ok {
							return result, false
						}
						if container["name"] != "envoy" {
							continue
						}
						containerPorts, _, _ := unstructured.NestedSlice(container, "ports")
						for _, raw := range containerPorts {
							entry, ok := raw.(map[string]any)
							if !ok {
								return result, false
							}
							if entry["name"] == value {
								number, _, _ := unstructured.NestedInt64(entry, "containerPort")
								if resolved != 0 || (entry["protocol"] != nil && entry["protocol"] != protocol) {
									return result, false
								}
								resolved = number
							}
						}
					}
					if resolved == 0 || (target != 0 && target != resolved) {
						return result, false
					}
					target = resolved
				}
			default:
				return result, false
			}
		}
		if target < 1 || target > 65535 {
			return result, false
		}
		if _, exists := result[uint32(target)]; exists {
			return result, false
		}
		name, _ := port["name"].(string)
		result[uint32(target)] = name
	}
	return result, len(wanted) == 0
}

func lbEIPBound(eip, svc *unstructured.Unstructured, r lbResolved) bool {
	if svc == nil || eip == nil {
		return false
	}
	return goodConditions(eip, "Valid", "Initialized") && crString(eip, "status", "phase") == "Bound" && crString(eip, "status", "boundResource", "resourceType") == "Service" && crString(eip, "status", "boundResource", "resource") == svc.GetName() && crString(eip, "status", "boundResource", "vpc") == r.vpc.Namespace+"/"+r.vpc.ProviderName && crString(eip, "status", "boundResource", "nodeName") != "" && crInt(eip, "status", "boundResource", "observedGeneration") == svc.GetGeneration()
}
func lbEIPBindingConflict(eip *unstructured.Unstructured, r lbResolved) bool {
	if emptyBoundResource(eip) {
		return false
	}
	return crString(eip, "status", "boundResource", "resourceType") != "Service" ||
		crString(eip, "status", "boundResource", "resource") != r.binding.ProviderName ||
		crString(eip, "status", "boundResource", "vpc") != r.vpc.Namespace+"/"+r.vpc.ProviderName
}
func lbAddressConflict(r lbResolved, expected *unstructured.Unstructured) bool {
	name, namespace := r.eip.ProviderName, r.binding.Namespace
	for _, gvr := range []schema.GroupVersionResource{kcSnats, kcNats} {
		for _, obj := range r.objects[gvr] {
			if kcRef(crString(&obj, "spec", "eip"), obj.GetNamespace()) == namespace+"/"+name {
				return true
			}
		}
	}
	for _, svc := range r.objects[kcServices] {
		for _, value := range strings.Split(svc.GetAnnotations()["networking.kubercloud.com/lb_eips"], ",") {
			if strings.TrimSpace(value) == "" {
				continue
			}
			if kcRef(strings.TrimSpace(value), svc.GetNamespace()) != namespace+"/"+name {
				continue
			}
			if expected == nil || svc.GetNamespace() != namespace || svc.GetName() != expected.GetName() || svc.GetUID() != expected.GetUID() {
				return true
			}
		}
	}
	return false
}

func lbServiceProtocol(service *unstructured.Unstructured, name string) string {
	if service == nil {
		return ""
	}
	ports, _, _ := unstructured.NestedSlice(service.Object, "spec", "ports")
	for _, raw := range ports {
		port, ok := raw.(map[string]any)
		if ok && port["name"] == name {
			protocol, _ := port["protocol"].(string)
			return protocol
		}
	}
	return ""
}
