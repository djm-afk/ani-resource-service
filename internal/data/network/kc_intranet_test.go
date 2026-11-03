package data

import (
	"strings"
	"testing"

	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func intranetFixtures() (egressResolved, egressReadSet, *unstructured.Unstructured) {
	c := sqlcgen.NetworkPublicPool{Scope: "intranet", Mode: "overlay", Cidr: "10.232.254.0/24", OvnGatewayIp: "10.232.254.1", ExcludedIps: []string{"10.232.254.1"}, DefaultVpcName: "kcn-cluster", DefaultVpcUid: "uid-kcn-cluster", IntranetNetworks: []string{"10.96.0.0/12"}}
	gateway := crFixture("VPC", kcSystemNamespace, "kcn-cluster")
	_ = unstructured.SetNestedField(gateway.Object, int64(2), "status", "observedGeneration")
	pool := crFixture("Subnet", kcSystemNamespace, "pool")
	pool.Object["spec"] = renderIntranetPool(c)
	_ = unstructured.SetNestedField(pool.Object, int64(2), "status", "observedGeneration")
	config := crFixture("ConfigMap", kcSystemNamespace, "kcn-config")
	config.SetAPIVersion("v1")
	config.Object["data"] = map[string]any{"intranetNetworks": "- 10.0.0.0/8\n"}
	service := crFixture("ServiceCIDR", "", "kubernetes")
	service.SetAPIVersion("networking.k8s.io/v1")
	service.Object["spec"] = map[string]any{"cidrs": []any{"10.96.0.0/12"}}
	providerPods := []unstructured.Unstructured{}
	for _, role := range []string{"controller", "cni-ds", "ovs-ds", "ovn-central"} {
		pod := crFixture("Pod", kcSystemNamespace, role)
		pod.SetAPIVersion("v1")
		pod.SetLabels(map[string]string{"networking.kubercloud.com/app": role})
		pod.Object["spec"] = map[string]any{"containers": []any{map[string]any{"name": role, "command": []any{"/kc-networking/start-" + role + ".sh"}}}}
		pod.Object["status"] = map[string]any{"phase": "Running", "conditions": []any{map[string]any{"type": "Ready", "status": "True"}}, "containerStatuses": []any{map[string]any{"name": role, "ready": true, "imageID": "fixture@sha256:" + strings.Repeat("a", 64)}}}
		providerPods = append(providerPods, *pod)
	}
	eip := crFixture("EIP", "tenant-a", "eip")
	eip.Object["spec"] = map[string]any{"subnet": "kcn-system/pool", "ipVersion": "IPv4", "ipAddress": "10.232.254.2"}
	_ = unstructured.SetNestedField(eip.Object, "kcn-system/kcn-cluster", "status", "gateway")
	r := egressResolved{pool: c}
	set := egressReadSet{objects: map[string]*unstructured.Unstructured{"pool": pool, "default_vpc": gateway, "intranet_config": config}, all: map[schema.GroupVersionResource][]unstructured.Unstructured{kcServiceCIDRs: {*service}, pods: providerPods}}
	return r, set, eip
}
func TestIntranetAdapterUsesDefaultVPCAndQualifiedGateway(t *testing.T) {
	r, set, eip := intranetFixtures()
	if !addressPoolReady(r, set.objects["pool"], set) {
		t.Fatal("intranet fixture not ready")
	}
	if ready, ip := allocatedEIP(r, eip, set); !ready || ip != "10.232.254.2" {
		t.Fatal("intranet EIP proof rejected", ready, ip)
	}
	if crString(set.objects["pool"], "spec", "type") != "Intranet" || crString(set.objects["pool"], "spec", "gateway") != "kcn-system/kcn-cluster" || hasField(set.objects["pool"], "spec", "underlayConfig") {
		t.Fatal("intranet uses wrong gateway contract")
	}
	_ = unstructured.SetNestedField(eip.Object, "kcn-cluster", "status", "gateway")
	if ready, _ := allocatedEIP(r, eip, set); ready {
		t.Fatal("unqualified status gateway accepted")
	}
}
func TestIntranetAdapterRejectsStaleIdentityRoutingAndProviderConfiguration(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*egressResolved, *egressReadSet)
	}{
		{"gateway uid", func(r *egressResolved, s *egressReadSet) { s.objects["default_vpc"].SetUID("replacement") }},
		{"gateway namespace", func(r *egressResolved, s *egressReadSet) { s.objects["default_vpc"].SetNamespace("tenant-a") }},
		{"pool generation", func(r *egressResolved, s *egressReadSet) {
			_ = unstructured.SetNestedField(s.objects["pool"].Object, int64(1), "status", "observedGeneration")
		}},
		{"gateway generation", func(r *egressResolved, s *egressReadSet) {
			_ = unstructured.SetNestedField(s.objects["default_vpc"].Object, int64(1), "status", "observedGeneration")
		}},
		{"public gateway", func(r *egressResolved, s *egressReadSet) { s.objects["default_vpc"].SetKind("EIPGateway") }},
		{"route removal", func(r *egressResolved, s *egressReadSet) {
			s.objects["intranet_config"].Object["data"] = map[string]any{"intranetNetworks": "[]"}
		}},
		{"default route", func(r *egressResolved, s *egressReadSet) {
			s.objects["intranet_config"].Object["data"] = map[string]any{"intranetNetworks": "- 0.0.0.0/0"}
		}},
		{"service coverage", func(r *egressResolved, s *egressReadSet) { r.pool.IntranetNetworks = []string{"192.168.0.0/16"} }},
		{"controller router", func(r *egressResolved, s *egressReadSet) {
			s.all[pods][0].Object["spec"].(map[string]any)["containers"] = []any{map[string]any{"command": []any{"/kc-networking/start-controller.sh"}, "args": []any{"--cluster-router=other"}}}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, s, _ := intranetFixtures()
			tc.mutate(&r, &s)
			if addressPoolReady(r, s.objects["pool"], s) {
				t.Fatal("unsafe intranet capability accepted")
			}
		})
	}
}
