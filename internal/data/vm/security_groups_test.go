package vm

import (
	"context"
	"errors"
	"testing"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

func TestSecurityGroupsVMTemplateVMILauncherAndEmptyDetach(t *testing.T) {
	n, v, client, _ := inputStoppedFixture(t)
	ctx := context.Background()
	yes := true
	vmi := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "kubevirt.io/v1", "kind": "VirtualMachineInstance", "metadata": map[string]any{"name": v.Native.Name, "namespace": v.Spec.Namespace, "uid": "vmi-current", "resourceVersion": "30"}, "status": map[string]any{"phase": "Running"}}}
	vmi.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "kubevirt.io/v1", Kind: "VirtualMachine", Name: v.Native.Name, UID: types.UID(v.Native.UID), Controller: &yes}})
	pod := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{"name": "virt-launcher-current", "namespace": v.Spec.Namespace, "uid": "launcher-current", "resourceVersion": "40", "labels": map[string]any{"kubevirt.io/created-by": "vmi-current"}}, "status": map[string]any{"phase": "Running"}}}
	pod.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "kubevirt.io/v1", Kind: "VirtualMachineInstance", Name: vmi.GetName(), UID: vmi.GetUID(), Controller: &yes}})
	if _, err := client.Resource(vmiResource).Namespace(v.Spec.Namespace).Create(ctx, vmi, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Resource(podResource).Namespace(v.Spec.Namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	v.Network.Consumer = &biz.NetworkConsumerState{State: "open",
		CurrentVMI:      &biz.ObjectRef{Namespace: v.Spec.Namespace, Name: vmi.GetName(), UID: string(vmi.GetUID())},
		CurrentLauncher: &biz.ObjectRef{Namespace: v.Spec.Namespace, Name: pod.GetName(), UID: string(pod.GetUID())}}
	client.ClearActions()
	for _, names := range [][]string{{"sg-first", "sg-second"}, nil} {
		if err := n.SetSecurityGroups(ctx, v, names); err != nil {
			t.Fatal(err)
		}
		expected := ""
		if len(names) > 0 {
			expected = "sg-first,sg-second"
		}
		root, _ := client.Resource(vmResource).Namespace(v.Spec.Namespace).Get(ctx, v.Native.Name, metav1.GetOptions{})
		ann, _, _ := unstructured.NestedStringMap(root.Object, "spec", "template", "metadata", "annotations")
		if value, present := ann[biz.SecurityGroupsAnnotation]; !present || value != expected {
			t.Fatal("template lost selection", ann)
		}
		for _, item := range []struct {
			kind string
			name string
		}{{"vmi", vmi.GetName()}, {"pod", pod.GetName()}} {
			r := vmiResource
			if item.kind == "pod" {
				r = podResource
			}
			obj, _ := client.Resource(r).Namespace(v.Spec.Namespace).Get(ctx, item.name, metav1.GetOptions{})
			if value, present := obj.GetAnnotations()[biz.SecurityGroupsAnnotation]; !present || value != expected {
				t.Fatal("runtime lost selection", obj.GetAnnotations())
			}
		}
	}
	for _, action := range client.Actions() {
		if action.GetVerb() == "list" {
			t.Fatal("security group submission scanned workload descendants", action)
		}
	}
	// A replaced root never authorizes updating the old VMI or launcher.
	root, _ := client.Resource(vmResource).Namespace(v.Spec.Namespace).Get(ctx, v.Native.Name, metav1.GetOptions{})
	root.SetUID("foreign")
	_, _ = client.Resource(vmResource).Namespace(v.Spec.Namespace).Update(ctx, root, metav1.UpdateOptions{})
	client.ClearActions()
	if err := n.SetSecurityGroups(ctx, v, []string{"sg-next"}); !errors.Is(err, biz.ErrOwnership) {
		t.Fatal(err)
	}
	for _, a := range client.Actions() {
		if a.GetVerb() == "patch" {
			t.Fatal("replaced root was mutated")
		}
	}
}
