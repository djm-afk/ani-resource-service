package vm

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestConsoleIdentityObservationDoesNotDependOnStorage(t *testing.T) {
	for _, mode := range []string{"running", "wrong-vm", "wrong-vmi-owner", "two-launchers", "not-running"} {
		t.Run(mode, func(t *testing.T) {
			v := biz.VM{TenantID: uuid.NewString(), ID: uuid.NewString(), Spec: biz.Spec{Namespace: "tenant-console", Digest: strings.Repeat("a", 64)}}
			v.Native = biz.ObjectRef{Kind: "VirtualMachine", Namespace: v.Spec.Namespace, Name: "vm-" + v.ID, UID: uuid.NewString()}
			v.Disks = []biz.Disk{biz.NewInitialDisk(v.ID, "system", "system", "system", 8), biz.NewInitialDisk(v.ID, "data", "data", "data", 3)}
			root := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "kubevirt.io/v1", "kind": "VirtualMachine", "metadata": map[string]any{"name": v.Native.Name, "namespace": v.Spec.Namespace, "uid": v.Native.UID}}}
			root.SetLabels(labelVM(v))
			if mode == "wrong-vm" {
				root.SetUID(types.UID(uuid.NewString()))
			}
			vmi := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "kubevirt.io/v1", "kind": "VirtualMachineInstance", "metadata": map[string]any{"name": v.Native.Name, "namespace": v.Spec.Namespace, "uid": uuid.NewString()}, "status": map[string]any{"phase": "Running"}}}
			vmi.SetOwnerReferences(ownReference(v))
			if mode == "wrong-vmi-owner" {
				refs := vmi.GetOwnerReferences()
				refs[0].UID = types.UID(uuid.NewString())
				vmi.SetOwnerReferences(refs)
			}
			if mode == "not-running" {
				_ = unstructured.SetNestedField(vmi.Object, "Pending", "status", "phase")
			}
			pod := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{"name": "launcher", "namespace": v.Spec.Namespace, "uid": uuid.NewString(), "labels": map[string]any{"kubevirt.io/created-by": string(vmi.GetUID())}}, "status": map[string]any{"phase": "Running"}}}
			controller := true
			pod.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "kubevirt.io/v1", Kind: "VirtualMachineInstance", Name: v.Native.Name, UID: vmi.GetUID(), Controller: &controller}})
			objects := []runtime.Object{root, vmi, pod}
			if mode == "two-launchers" {
				second := pod.DeepCopy()
				second.SetName("other-launcher")
				second.SetUID(types.UID(uuid.NewString()))
				objects = append(objects, second)
			}
			client := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{podResource: "PodList"}, objects...)
			storageReads := 0
			client.PrependReactor("*", "*", func(a ktesting.Action) (bool, runtime.Object, error) {
				switch a.GetResource().Resource {
				case "datavolumes", "persistentvolumeclaims", "persistentvolumes", "kubevirts", "storageclasses":
					storageReads++
					return true, nil, biz.ErrDependency
				}
				return false, nil, nil
			})
			n := NewNativeWithClients(client, nil)
			x, err := n.ObserveConsole(context.Background(), v)
			switch mode {
			case "wrong-vm", "wrong-vmi-owner":
				if !errors.Is(err, biz.ErrOwnership) {
					t.Fatalf("identity mismatch accepted: %v", err)
				}
			case "two-launchers":
				if err != nil || !x.Running || x.Launcher != nil || !x.RelatedPodsPresent {
					t.Fatalf("ambiguous launcher changed VMI power fact: %+v %v", x, err)
				}
			default:
				if err != nil || x.VM.UID != v.Native.UID || x.VMI == nil || x.VMI.UID != string(vmi.GetUID()) || x.Launcher == nil || x.Launcher.UID != string(pod.GetUID()) || x.Running != (mode == "running") || len(x.Volumes) != 0 || len(x.BackendStates) != 0 {
					t.Fatalf("console depended on unavailable storage: %+v %v", x, err)
				}
			}
			if storageReads != 0 {
				t.Fatalf("console read unrelated storage %d times", storageReads)
			}
			if mode == "running" {
				if _, err = n.Observe(context.Background(), v); !errors.Is(err, biz.ErrDependency) || storageReads == 0 {
					t.Fatalf("ordinary lifecycle observation lost storage checks: %v", err)
				}
			}
		})
	}
}
