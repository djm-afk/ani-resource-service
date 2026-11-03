package data

import (
	"strings"
	"testing"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/cache"
)

func crFixture(kind, ns, name string) *unstructured.Unstructured {
	o := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "networking.kubercloud.com/v1", "kind": kind, "metadata": map[string]any{"name": name, "namespace": ns, "uid": "uid-" + name, "resourceVersion": "10", "generation": int64(2)}, "spec": map[string]any{}, "status": map[string]any{"conditions": []any{map[string]any{"type": "Valid", "status": "True"}, map[string]any{"type": "Initialized", "status": "True"}, map[string]any{"type": "Ready", "status": "True"}}}}}
	return o
}
func poolFixtures() (egressResolved, *unstructured.Unstructured, *unstructured.Unstructured, *unstructured.Unstructured) {
	r := egressResolved{pool: sqlcgen.NetworkPublicPool{Mode: "overlay", Cidr: "192.0.2.0/24", OvnGatewayIp: "192.0.2.1", ExcludedIps: []string{"192.0.2.1", "192.0.2.8..192.0.2.10"}}}
	pool := crFixture("Subnet", kcSystemNamespace, "pool")
	gw := crFixture("EIPGateway", "", "gateway")
	gw.Object["spec"] = map[string]any{"scope": "Public", "egressType": "Host"}
	_ = unstructured.SetNestedField(gw.Object, "100.64.0.2", "status", "localIP")
	_ = unstructured.SetNestedField(gw.Object, "router", "status", "boundResources", "router")
	pool.Object["spec"] = renderPublicPool(r.pool, "gateway", "")
	eip := crFixture("EIP", "tenant-a", "eip")
	eip.Object["spec"] = map[string]any{"subnet": "kcn-system/pool", "ipVersion": "IPv4", "ipAddress": "192.0.2.20"}
	_ = unstructured.SetNestedField(eip.Object, "gateway", "status", "gateway")
	return r, pool, gw, eip
}
func TestEgressOverlayUnderlayProviderFieldsAndAddressEvidence(t *testing.T) {
	r, pool, gw, eip := poolFixtures()
	if !publicPoolReady(r, pool, gw, nil) {
		t.Fatal("overlay contract not ready")
	}
	if _, ok := pool.Object["spec"].(map[string]any)["underlayConfig"]; ok {
		t.Fatal("overlay emitted underlay")
	}
	for _, ip := range []string{"192.0.2.0", "192.0.2.255", "192.0.2.1", "192.0.2.9", "192.0.3.20", "::1"} {
		copy := eip.DeepCopy()
		_ = unstructured.SetNestedField(copy.Object, ip, "spec", "ipAddress")
		if ok, _ := eipAllocated(r, copy, pool, gw, nil); ok {
			t.Fatal("invalid/reserved allocation accepted", ip)
		}
	}
	if ok, ip := eipAllocated(r, eip, pool, gw, nil); !ok || ip != "192.0.2.20" {
		t.Fatal("provider allocation ignored", ok, ip)
	}
	_ = unstructured.SetNestedField(pool.Object, map[string]any{}, "spec", "underlayConfig")
	if publicPoolReady(r, pool, gw, nil) {
		t.Fatal("empty underlay object accepted on overlay")
	}
	vlanID, upstream := "vlan_"+strings.Repeat("a", 32), "192.0.2.254"
	r.pool.Mode = "underlay"
	r.pool.VlanNetworkID = &vlanID
	r.pool.UpstreamGatewayIp = &upstream
	pool.Object["spec"] = renderPublicPool(r.pool, "gateway", "vlan-zero")
	vlan := crFixture("VlanNetwork", "", "vlan-zero")
	vlan.Object["spec"] = map[string]any{"devName": "fixture0", "vlanID": int64(0)}
	_ = unstructured.SetNestedField(vlan.Object, "kcn-system/pool", "status", "subnet")
	_ = unstructured.SetNestedField(pool.Object, map[string]any{"ready": true, "chassisNode": "node-a", "notReadyNodes": []any{}}, "status", "underlayState")
	if !publicPoolReady(r, pool, gw, vlan) {
		t.Fatal("untagged underlay contract rejected")
	}
	_ = unstructured.SetNestedField(pool.Object, []any{"node-b"}, "status", "underlayState", "notReadyNodes")
	if publicPoolReady(r, pool, gw, vlan) {
		t.Fatal("partially ready underlay accepted")
	}
}
func TestEgressBindingRequiresExactNamespaceGenerationAndAppliedState(t *testing.T) {
	_, _, _, eip := poolFixtures()
	snat := crFixture("Snat", "tenant-a", "binding")
	snat.Object["spec"] = map[string]any{"eip": "eip", "vpc": "vpc", "disable": false}
	_ = unstructured.SetNestedField(snat.Object, "Bound", "status", "phase")
	_ = unstructured.SetNestedField(snat.Object, "192.0.2.20", "status", "eipAddress")
	vpc := crFixture("VPC", "tenant-a", "vpc")
	_ = unstructured.SetNestedField(vpc.Object, int64(2), "status", "observedGeneration")
	_ = unstructured.SetNestedField(eip.Object, "Bound", "status", "phase")
	bound := map[string]any{"resourceType": "Snat", "resource": "binding", "vpc": "tenant-a/vpc", "nodeName": "worker-2", "observedGeneration": int64(2), "disabled": false}
	_ = unstructured.SetNestedMap(eip.Object, bound, "status", "boundResource")
	if !bindingApplied(eip, snat, vpc, true) {
		t.Fatal("valid application evidence rejected")
	}
	for _, tc := range []struct {
		name   string
		mutate func(*unstructured.Unstructured, *unstructured.Unstructured, *unstructured.Unstructured)
	}{
		{"namespace", func(e, s, v *unstructured.Unstructured) { s.SetNamespace("tenant-b") }},
		{"vpc_namespace", func(e, s, v *unstructured.Unstructured) { v.SetNamespace("tenant-b") }},
		{"generation", func(e, s, v *unstructured.Unstructured) { s.SetGeneration(3) }},
		{"bound_name", func(e, s, v *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(e.Object, "other", "status", "boundResource", "resource")
		}},
		{"egress_node", func(e, s, v *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(e.Object, "", "status", "boundResource", "nodeName")
		}},
		{"disable", func(e, s, v *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(s.Object, true, "spec", "disable")
		}},
		{"applied_disable", func(e, s, v *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(e.Object, true, "status", "boundResource", "disabled")
		}},
		{"vpc_generation", func(e, s, v *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(v.Object, int64(1), "status", "observedGeneration")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, s, v := eip.DeepCopy(), snat.DeepCopy(), vpc.DeepCopy()
			tc.mutate(e, s, v)
			if bindingApplied(e, s, v, true) {
				t.Fatal("stale/mismatched binding accepted")
			}
		})
	}
	_ = unstructured.SetNestedField(snat.Object, true, "spec", "disable")
	_ = unstructured.SetNestedField(snat.Object, "Disabled", "status", "phase")
	_ = unstructured.SetNestedField(eip.Object, true, "status", "boundResource", "disabled")
	_ = unstructured.SetNestedField(eip.Object, "", "status", "boundResource", "nodeName")
	if !bindingApplied(eip, snat, vpc, false) {
		t.Fatal("disabled binding incorrectly required active egress node")
	}
}
func TestEgressUIDConflictsAndNatServiceDependencyIndex(t *testing.T) {
	b := sqlcgen.NetworkProviderBinding{TenantID: "tenant-a", ResourceKind: "eip", Namespace: "tenant-a", ProviderName: "eip", ProviderUid: "uid-eip", BindingID: "binding-identity"}
	o := crFixture("EIP", "tenant-a", "eip")
	o.SetLabels(map[string]string{ownerLabel: "ani-network-service", resourceLabel: "resource-id", tenantLabel: "tenant-a", bindingLabel: b.BindingID})
	if err := inspectEgressIdentity(o, b, "resource-id", b.ProviderUid); err != nil {
		t.Fatal(err)
	}
	o.SetUID("replacement")
	if inspectEgressIdentity(o, b, "resource-id", b.ProviderUid) == nil {
		t.Fatal("same name recreated object adopted")
	}
	nat := crFixture("Nat", "tenant-a", "nat")
	nat.Object["spec"] = map[string]any{"eip": "eip"}
	set := egressReadSet{all: map[schema.GroupVersionResource][]unstructured.Unstructured{kcNats: {*nat}}}
	if !eipBindingConflict(set, "tenant-a", "eip", "") {
		t.Fatal("Nat dependency ignored")
	}
	if eipBindingConflict(set, "tenant-b", "eip", "") {
		t.Fatal("cross namespace same name matched")
	}
	svc := crFixture("Service", "tenant-a", "loadbalancer")
	svc.Object["spec"] = map[string]any{"type": "LoadBalancer"}
	svc.SetAnnotations(map[string]string{"networking.kubercloud.com/lb_eips": " eip,second "})
	set.all = map[schema.GroupVersionResource][]unstructured.Unstructured{kcServices: {*svc}}
	if !eipBindingConflict(set, "tenant-a", "eip", "") {
		t.Fatal("Service dependency ignored")
	}
	observer := &KCObservation{options: ObservationOptions{QueueCapacity: 32}, pending: map[string]struct{}{}, changes: map[string]uint64{}}
	next := nat.DeepCopy()
	next.Object["spec"] = map[string]any{"eip": "new-eip"}
	observer.changed(nat, next)
	observer.changed(cache.DeletedFinalStateUnknown{Key: "tenant-a/nat", Obj: next}, nil)
	for _, key := range []string{"eip:tenant-a/eip", "eip:tenant-a/new-eip", "uid:uid-nat"} {
		if _, ok := observer.pending[key]; !ok {
			t.Fatal("lost relation/tombstone", key)
		}
	}
}
func TestEgressProviderImagesUseRunningDigests(t *testing.T) {
	objects := []unstructured.Unstructured{}
	digest := "sha256:" + strings.Repeat("b", 64)
	for _, role := range []string{"controller", "cni-ds", "ovs-ds", "ovn-central"} {
		o := crFixture("Pod", kcSystemNamespace, role)
		o.SetLabels(map[string]string{"networking.kubercloud.com/app": role})
		o.Object["spec"] = map[string]any{"containers": []any{map[string]any{"name": role}}}
		o.Object["status"] = map[string]any{"phase": "Running", "conditions": []any{map[string]any{"type": "Ready", "status": "True"}}, "containerStatuses": []any{map[string]any{"name": role, "ready": true, "imageID": "docker-pullable://provider@" + digest}}}
		objects = append(objects, *o)
	}
	if got := providerImages(objects); len(got) != 1 || got[0] != digest {
		t.Fatal("image digest evidence", got)
	}
	unstructured.RemoveNestedField(objects[0].Object, "status", "containerStatuses")
	if len(providerImages(objects)) != 0 {
		t.Fatal("missing running digest preserved acceptance evidence")
	}
}

func TestKCNStringNotReadyNodesBlocksPrematureVLANAvailability(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
		ready bool
	}{
		{"all_nodes_acknowledged", "", true},
		{"one_node_missing", "ani-02", false},
		{"truncated_node_summary", "ani-01,ani-02...", false},
		{"malformed_status", map[string]any{"unexpected": true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vlan := crFixture("VlanNetwork", "", "prepared-network")
			vlan.SetLabels(map[string]string{ownerLabel: "ani-network-service", resourceLabel: "vlan_" + strings.Repeat("a", 32), bindingLabel: "binding"})
			vlan.Object["spec"] = map[string]any{"devName": "bond0", "vlanID": int64(3), "vClusterName": "prepared"}
			_ = unstructured.SetNestedField(vlan.Object, tc.value, "status", "notReadyNodes")
			r := egressResolved{binding: sqlcgen.NetworkProviderBinding{ResourceKind: "vlan", ProviderName: vlan.GetName(), ProviderUid: string(vlan.GetUID()), BindingID: "binding"}, target: biz.ProviderTarget{Kind: "vlan", ResourceID: "vlan_" + strings.Repeat("a", 32), KnownIdentity: string(vlan.GetUID())}, spec: vlan.Object["spec"].(map[string]any)}
			observed, err := inspectEgressSet(r, egressReadSet{objects: map[string]*unstructured.Unstructured{"self": vlan}})
			if err != nil {
				t.Fatal(err)
			}
			if observed.Ready != tc.ready {
				t.Fatalf("ready=%t want=%t for KCN status %#v", observed.Ready, tc.ready, tc.value)
			}
		})
	}
}

func TestEIPSpecLeavesManagedSecurityGroupsToBindingWorker(t *testing.T) {
	o := crFixture("EIP", "tenant-a", "eip")
	o.Object["spec"] = map[string]any{"subnet": "kcn-system/pool", "ipVersion": "IPv4", "securityGroups": []any{"old"}}
	core := map[string]any{"subnet": "kcn-system/pool", "ipVersion": "IPv4"}
	if matchesEgressSpec(o, core, "eip") {
		t.Fatal("unmanaged SG drift accepted")
	}
	core["securityGroups"] = []any{"next"}
	if !matchesEgressSpec(o, core, "eip") {
		t.Fatal("address worker rejected an in-flight SG transition")
	}
	o.Object["spec"].(map[string]any)["subnet"] = "kcn-system/foreign"
	if matchesEgressSpec(o, core, "eip") {
		t.Fatal("SG field ownership hid core address drift")
	}
}
