package vm

import (
	"context"
	"testing"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

func manualTerminalVMI(v biz.VM, phase string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "kubevirt.io/v1", "kind": "VirtualMachineInstance", "status": map[string]any{"phase": phase}}}
	u.SetName(v.Native.Name)
	u.SetNamespace(v.Spec.Namespace)
	u.SetUID(types.UID("finished-vmi"))
	u.SetLabels(labelVM(v))
	u.SetOwnerReferences(ownReference(v))
	return u
}

func TestNativeManualGuestShutdownNormalizesStopped(t *testing.T) {
	n, v, client, _ := inputStoppedFixture(t)
	v.Disks = nil
	terminal := manualTerminalVMI(v, "Succeeded")
	if _, err := client.Resource(vmiResource).Namespace(v.Spec.Namespace).Create(context.Background(), terminal, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	x, err := n.Observe(context.Background(), v)
	if err != nil || x.State != "Stopped" || !x.Terminal || x.Stopped || x.Running || x.VMI == nil || x.VMI.UID != "finished-vmi" {
		t.Fatalf("guest shutdown must be Stopped while retaining owned Manual terminal VMI: %+v, %v", x, err)
	}
}

func TestNativeExplicitStartUsesKubeVirtForRetainedManualTerminalVMI(t *testing.T) {
	for _, family := range []string{"linux", "windows"} {
		for _, phase := range []string{"Succeeded", "Failed"} {
			t.Run(family+"/"+phase, func(t *testing.T) {
				n, v, client, calls := inputStoppedFixture(t)
				if family == "windows" {
					n, v, client, calls = windowsStoppedFixture(t)
				}
				v.Disks = nil
				terminal := manualTerminalVMI(v, phase)
				if _, err := client.Resource(vmiResource).Namespace(v.Spec.Namespace).Create(context.Background(), terminal, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
				if err := n.Start(context.Background(), v); err != nil || *calls != 1 {
					t.Fatalf("explicit Start must let KubeVirt replace its owned Manual terminal VMI: calls=%d err=%v", *calls, err)
				}
				for _, action := range client.Actions() {
					if action.GetVerb() == "delete" {
						t.Fatal("Resource introduced a separate deletion instead of the native Start transition")
					}
				}
			})
		}
	}
}

func TestStoppedUnknownOrClosingVMDoesNotExposeOldNetworkAddress(t *testing.T) {
	// Any repository access would panic. Inactive states must discard old
	// address eligibility before querying its last published value.
	n := &Network{}
	for _, state := range []string{"Stopped", "Succeeded", "Failed", "Unknown", "Starting"} {
		if addresses, err := n.CurrentAddresses(context.Background(), biz.VM{Observed: state}); err != nil || len(addresses) != 0 {
			t.Fatal("non-running VM exposed an old address", state, addresses, err)
		}
	}
	if addresses, err := n.CurrentAddresses(context.Background(), biz.VM{Observed: "Running", Closing: true}); err != nil || len(addresses) != 0 {
		t.Fatal("closing VM exposed an old address", addresses, err)
	}
}
