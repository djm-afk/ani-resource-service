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

func vmNativeChainFixture() (biz.AttachmentWork, *unstructured.Unstructured, *unstructured.Unstructured, *unstructured.Unstructured) {
	rootRef := biz.WorkloadReference{APIVersion: "kubevirt.io/v1", Kind: "VirtualMachine", Namespace: "tenant", Name: "vm", UID: "vm-uid"}
	vmiRef := biz.WorkloadReference{APIVersion: "kubevirt.io/v1", Kind: "VirtualMachineInstance", Namespace: "tenant", Name: "vm", UID: "vmi-uid"}
	podRef := biz.WorkloadReference{APIVersion: "v1", Kind: "Pod", Namespace: "tenant", Name: "virt-launcher-vm", UID: "pod-uid"}
	a := biz.Attachment{ID: "attachment", TenantID: "tenant-id", InstanceID: "vm-id", SubmissionID: "submission", Generation: 1, Namespace: "tenant", WorkloadMode: "vm_managed"}
	plan := &biz.VMPrimaryPlan{Namespace: "tenant", NADName: "main", NADUID: "nad-uid", KCNSubnetRef: "tenant/subnet"}
	labels := map[string]any{tenantLabel: a.TenantID, instanceLabel: a.InstanceID, attachmentLabel: a.ID, submissionLabel: a.SubmissionID, generationLabel: "1"}
	spec := map[string]any{"networks": []any{map[string]any{"name": "default", "multus": map[string]any{"default": true, "networkName": "tenant/main"}}}, "domain": map[string]any{"devices": map[string]any{"interfaces": []any{map[string]any{"name": "default", "bridge": map[string]any{}}}}}}
	meta := func(ref biz.WorkloadReference) map[string]any {
		return map[string]any{"name": ref.Name, "namespace": ref.Namespace, "uid": ref.UID, "labels": labels, "annotations": map[string]any{subnetAnnotation: plan.KCNSubnetRef}}
	}
	owner := func(ref biz.WorkloadReference) []any {
		return []any{map[string]any{"apiVersion": ref.APIVersion, "kind": ref.Kind, "name": ref.Name, "uid": ref.UID, "controller": true}}
	}
	root := &unstructured.Unstructured{Object: map[string]any{"apiVersion": rootRef.APIVersion, "kind": rootRef.Kind, "metadata": meta(rootRef), "spec": map[string]any{"template": map[string]any{"metadata": map[string]any{"labels": labels, "annotations": map[string]any{subnetAnnotation: plan.KCNSubnetRef}}, "spec": runtime.DeepCopyJSON(spec)}}}}
	vmi := &unstructured.Unstructured{Object: map[string]any{"apiVersion": vmiRef.APIVersion, "kind": vmiRef.Kind, "metadata": meta(vmiRef), "spec": runtime.DeepCopyJSON(spec), "status": map[string]any{"phase": "Running", "interfaces": []any{map[string]any{"name": "default", "ipAddress": "10.42.1.2"}}}}}
	vmi.Object["metadata"].(map[string]any)["ownerReferences"] = owner(rootRef)
	pod := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": meta(podRef)}}
	pod.SetLabels(map[string]string{tenantLabel: a.TenantID, instanceLabel: a.InstanceID, attachmentLabel: a.ID, submissionLabel: a.SubmissionID, generationLabel: "1", "kubevirt.io": "virt-launcher"})
	pod.SetAnnotations(map[string]string{subnetAnnotation: plan.KCNSubnetRef, "k8s.v1.cni.cncf.io/network-status": `[{"name":"tenant/main","default":true,"interface":"eth0"}]`})
	pod.Object["metadata"].(map[string]any)["ownerReferences"] = owner(vmiRef)
	c := &biz.ConsumerSubmission{RootController: rootRef, State: "open", DesiredRunState: "running", Controllers: []biz.ControllerReference{{Reference: vmiRef, Owner: rootRef}}, CurrentPods: []biz.PodReference{{Reference: podRef, Owner: vmiRef}}}
	return biz.AttachmentWork{Attachment: a, Plan: biz.PodPrimaryPlan{PrimaryNetworkRef: plan.KCNSubnetRef, VMPrimary: plan}, ConsumerSnapshot: c}, root, vmi, pod
}

func TestVMNativeOwnerAndPrimaryBindingAreIndependentProof(t *testing.T) {
	for _, name := range []string{"valid", "root-replaced", "vmi-replaced", "wrong-vmi-owner", "wrong-bridge", "wrong-NAD", "wrong-launcher-NAD", "helper"} {
		t.Run(name, func(t *testing.T) {
			w, root, vmi, pod := vmNativeChainFixture()
			switch name {
			case "root-replaced":
				root.SetUID("different-vm")
			case "vmi-replaced":
				vmi.SetUID("different-vmi")
			case "wrong-vmi-owner":
				refs := vmi.GetOwnerReferences()
				refs[0].Kind = "Notebook"
				vmi.SetOwnerReferences(refs)
			case "wrong-bridge":
				_ = unstructured.SetNestedSlice(vmi.Object, []any{map[string]any{"name": "default", "masquerade": map[string]any{}}}, "spec", "domain", "devices", "interfaces")
			case "wrong-NAD":
				_ = unstructured.SetNestedSlice(vmi.Object, []any{map[string]any{"name": "default", "multus": map[string]any{"default": true, "networkName": "tenant/other"}}}, "spec", "networks")
			case "wrong-launcher-NAD":
				pod.SetAnnotations(map[string]string{"k8s.v1.cni.cncf.io/network-status": `[{"name":"tenant/other","default":true,"interface":"eth0"}]`})
			case "helper":
				labels := pod.GetLabels()
				labels["kubevirt.io"] = "hotplug-disk"
				pod.SetLabels(labels)
			}
			p := &KCProvider{client: fake.NewSimpleDynamicClient(runtime.NewScheme(), root, vmi)}
			verified, pending, err := p.verifyVMPod(context.Background(), w, pod, func(attachmentRelation) {})
			if name == "valid" {
				if err != nil || !verified || pending {
					t.Fatal("valid native VM chain rejected", verified, pending, err)
				}
			} else if err == nil || verified {
				t.Fatal("untrusted native chain accepted", verified, pending, err)
			}
		})
	}
}

func TestVMPendingLauncherWaitsForCNIWithoutProtocolConflict(t *testing.T) {
	w, root, vmi, pod := vmNativeChainFixture()
	pod.SetAnnotations(map[string]string{subnetAnnotation: w.Plan.PrimaryNetworkRef, "v1.multus-cni.io/default-network": "tenant/main"})
	_ = unstructured.SetNestedField(pod.Object, "Pending", "status", "phase")
	_ = unstructured.SetNestedField(vmi.Object, "Scheduling", "status", "phase")
	p := &KCProvider{client: fake.NewSimpleDynamicClient(runtime.NewScheme(), root, vmi)}
	verified, pending, err := p.verifyVMPod(context.Background(), w, pod, func(attachmentRelation) {})
	if err != nil || verified || !pending {
		t.Fatal("legitimate launcher before CNI feedback became a protocol conflict", verified, pending, err)
	}
	annotations := pod.GetAnnotations()
	annotations["v1.multus-cni.io/default-network"] = "tenant/foreign"
	pod.SetAnnotations(annotations)
	if _, _, err := p.verifyVMPod(context.Background(), w, pod, func(attachmentRelation) {}); err == nil {
		t.Fatal("pending launcher on another primary NAD was accepted")
	}
}

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
	if _, err = p.ResolveVMNetwork(context.Background(), s, e); err == nil {
		t.Fatal("arbitrary observed NAD promoted to managed without policy")
	}
	if err = p.ConfigureVMNetworks([]ManagedVMNAD{policy}); err != nil {
		t.Fatal(err)
	}
	if plan, err := p.ResolveVMNetwork(context.Background(), s, e); err != nil || plan.KCNSubnetRef == plan.NADName || plan.NADUID != e.NADUID {
		t.Fatal("trusted NAD plan", plan, err)
	}
	nad.SetUID("replacement")
	if _, err = p.client.Resource(vmNADs).Namespace("tenant-11111111-1111-4111-8111-111111111111").Update(context.Background(), nad, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err = p.ResolveVMNetwork(context.Background(), s, e); err == nil {
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
			_, err = provider.ResolveVMNetwork(context.Background(), selection, expectation)
			if (err == nil) != test.valid {
				t.Fatalf("valid=%v, resolve error=%v", test.valid, err)
			}
		})
	}
}
