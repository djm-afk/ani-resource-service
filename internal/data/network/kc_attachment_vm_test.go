package data

import (
	"context"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/tenantnamespace"
	"testing"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/fake"
)

func TestVMManagedNADRequiresTrustedIdentityAndCanonicalConfig(t *testing.T) {
	const raw = `{"cniVersion":"0.3.1","name":"vm-main","type":"kc-networking","server_socket":"/run/kcn.sock","mtu":1500}`
	digest, err := VMNADConfigDigest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if equivalent, err := VMNADConfigDigest(`{ "mtu":1500,"type":"kc-networking","server_socket":"/run/kcn.sock","name":"vm-main","cniVersion":"0.3.1" }`); err != nil || equivalent != digest {
		t.Fatal("NAD canonical digest unstable", err)
	}
	ns := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "tenant-11111111-1111-4111-8111-111111111111", "uid": "namespace-uid", "labels": map[string]any{tenantLabel: "11111111-1111-4111-8111-111111111111", ownerLabel: "ani-network-service"}}}}
	nad := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "k8s.cni.cncf.io/v1", "kind": "NetworkAttachmentDefinition", "metadata": map[string]any{"name": "main", "namespace": "tenant-11111111-1111-4111-8111-111111111111", "uid": "nad-uid"}, "spec": map[string]any{"config": raw}}}
	client := fake.NewSimpleDynamicClient(runtime.NewScheme(), ns)
	// The CRD plural contains hyphens; the generic fake's kind-based guess does not.
	if err = client.Tracker().Create(vmNADs, nad, "tenant-11111111-1111-4111-8111-111111111111"); err != nil {
		t.Fatal(err)
	}
	p := &KCProvider{repository: &Postgres{placement: Placement{ClusterID: "cluster"}}, client: client, namespaceIdentityReader: func(context.Context, string) (tenantnamespace.Identity, error) {
		return tenantnamespace.Identity{TenantID: "11111111-1111-4111-8111-111111111111", ClusterID: "cluster", Name: "tenant-11111111-1111-4111-8111-111111111111", UID: string(ns.GetUID())}, nil
	}}
	policy := ManagedVMNAD{TenantID: "11111111-1111-4111-8111-111111111111", VPCID: "vpc", SubnetID: "subnet", ClusterID: "cluster", Namespace: "tenant-11111111-1111-4111-8111-111111111111", NamespaceUID: "namespace-uid", NADName: "main", NADUID: "nad-uid", NADConfigDigest: digest, KCNSubnetRef: "tenant-11111111-1111-4111-8111-111111111111/kcn-subnet"}
	s := biz.VMNetworkSelection{TenantID: policy.TenantID, VPCID: policy.VPCID, SubnetID: policy.SubnetID, ClusterID: policy.ClusterID, Namespace: policy.Namespace, KCNSubnetRef: policy.KCNSubnetRef}
	e := biz.VMNetworkExpectation{NADName: policy.NADName, NADUID: policy.NADUID, NADConfigDigest: digest}
	if _, err = p.resolveVMNetwork(context.Background(), s, e, true); err == nil {
		t.Fatal("arbitrary observed NAD promoted to managed without policy")
	}
	if err = p.ConfigureVMNetworks([]ManagedVMNAD{policy}); err != nil {
		t.Fatal(err)
	}
	if plan, err := p.resolveVMNetwork(context.Background(), s, e, true); err != nil || plan.KCNSubnetRef == plan.NADName || plan.NADUID != e.NADUID {
		t.Fatal("trusted NAD plan", plan, err)
	}
	nad.SetUID("replacement")
	if _, err = p.client.Resource(vmNADs).Namespace("tenant-11111111-1111-4111-8111-111111111111").Update(context.Background(), nad, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err = p.resolveVMNetwork(context.Background(), s, e, true); err == nil {
		t.Fatal("same-name NAD replacement accepted")
	}
}

// Match the real admission seam: a configured UID/digest alone does not make
// a Multus-executable conflist readable by the KCN PodReconciler.
func TestVMManagedNADRequiresKCNControllerCompatibleSinglePlugin(t *testing.T) {
	for _, test := range []struct {
		name  string
		raw   string
		valid bool
	}{
		{"single-plugin", `{"cniVersion":"0.3.1","name":"kc-networking","type":"kc-networking","server_socket":"/run/openvswitch/kc-networking-daemon.sock"}`, true},
		{"deployment-specific-socket", `{"cniVersion":"0.4.0","name":"vm-main","type":"kc-networking","server_socket":"/run/other-kcn.sock"}`, true},
		{"live-bad-conflist", `{"cniVersion":"0.3.1","name":"kc-networking","plugins":[{"server_socket":"/run/openvswitch/kc-networking-daemon.sock","type":"kc-networking"},{"capabilities":{"portMappings":true},"type":"portmap"}]}`, false},
		{"plugins-with-top-level-fields", `{"type":"kc-networking","server_socket":"/run/kcn.sock","plugins":[{"type":"kc-networking","server_socket":"/run/kcn.sock"}]}`, false},
		{"legacy-fake-type", `{"type":"kcn","server_socket":"/run/kcn.sock"}`, false},
		{"missing-socket", `{"type":"kc-networking"}`, false},
		{"empty-socket", `{"type":"kc-networking","server_socket":"   "}`, false},
		{"socket-wrong-type", `{"type":"kc-networking","server_socket":123}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			digest, err := VMNADConfigDigest(test.raw)
			if err != nil {
				t.Fatal(err)
			}
			ns := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "tenant-11111111-1111-4111-8111-111111111111", "uid": "ns-uid", "labels": map[string]any{tenantLabel: "11111111-1111-4111-8111-111111111111", ownerLabel: "ani-network-service"}}}}
			nad := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "k8s.cni.cncf.io/v1", "kind": "NetworkAttachmentDefinition", "metadata": map[string]any{"name": "main", "namespace": "tenant-11111111-1111-4111-8111-111111111111", "uid": "nad-uid"}, "spec": map[string]any{"config": test.raw}}}
			client := fake.NewSimpleDynamicClient(runtime.NewScheme(), ns)
			if err := client.Tracker().Create(vmNADs, nad, "tenant-11111111-1111-4111-8111-111111111111"); err != nil {
				t.Fatal(err)
			}
			provider := &KCProvider{repository: &Postgres{placement: Placement{ClusterID: "cluster"}}, client: client, namespaceIdentityReader: func(context.Context, string) (tenantnamespace.Identity, error) {
				return tenantnamespace.Identity{TenantID: "11111111-1111-4111-8111-111111111111", ClusterID: "cluster", Name: "tenant-11111111-1111-4111-8111-111111111111", UID: string(ns.GetUID())}, nil
			}}
			policy := ManagedVMNAD{TenantID: "11111111-1111-4111-8111-111111111111", VPCID: "vpc", SubnetID: "subnet", ClusterID: "cluster", Namespace: "tenant-11111111-1111-4111-8111-111111111111", NamespaceUID: "ns-uid", NADName: "main", NADUID: "nad-uid", NADConfigDigest: digest, KCNSubnetRef: "tenant-11111111-1111-4111-8111-111111111111/subnet"}
			if err := provider.ConfigureVMNetworks([]ManagedVMNAD{policy}); err != nil {
				t.Fatal(err)
			}
			selection := biz.VMNetworkSelection{TenantID: policy.TenantID, VPCID: policy.VPCID, SubnetID: policy.SubnetID, ClusterID: policy.ClusterID, Namespace: policy.Namespace, KCNSubnetRef: policy.KCNSubnetRef}
			expectation := biz.VMNetworkExpectation{NADName: policy.NADName, NADUID: policy.NADUID, NADConfigDigest: digest}
			_, err = provider.resolveVMNetwork(context.Background(), selection, expectation, true)
			if (err == nil) != test.valid {
				t.Fatalf("valid=%v, resolve error=%v", test.valid, err)
			}
		})
	}
}
