package vm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
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
	"k8s.io/client-go/rest"
	ktesting "k8s.io/client-go/testing"
)

func inputNativeFixture(t *testing.T, family string) (*Native, biz.VM, *fake.FakeDynamicClient) {
	t.Helper()
	v := biz.VM{ID: uuid.NewString(), TenantID: uuid.NewString()}
	v.Native = biz.ObjectRef{Kind: "VirtualMachine", Namespace: "tenant-input", Name: "vm-" + v.ID}
	media := biz.Source{Key: family + "-install", Kind: "INSTALL_ISO", ClusterID: uuid.NewString(), Namespace: "media", PVCName: family + "-iso", PVCUID: uuid.NewString(), Checksum: strings.Repeat("a", 64), RequestedBytes: 2 << 30, VirtualBytes: 1 << 30, VolumeMode: "Filesystem", ContentType: "kubevirt", OSFamily: family, OSVersion: "test", Architecture: "amd64", Firmware: "bios", RootBus: "scsi", DataBus: "scsi", NICModel: "virtio"}
	v.Spec = biz.Spec{Digest: strings.Repeat("b", 64), Namespace: v.Native.Namespace, NamespaceUID: uuid.NewString(), ClusterID: media.ClusterID, CPUMillis: 2000, MemoryMiB: 4096, Cores: 2, ServiceAccount: "default", StorageClass: "ani-block", MediaStorageClass: "ani-block", Media: media, BootDevice: "INSTALL_MEDIA"}
	v.Disks = []biz.Disk{biz.NewInitialDisk(v.ID, "system", "system", "system", 80)}
	v.Network = biz.NetworkPlan{NADName: "main", KCNSubnetRef: "tenant-input/subnet"}
	if family == "windows" {
		driver := media
		driver.Kind, driver.Key, driver.PVCName, driver.PVCUID = "DRIVER_ISO", "virtio", "virtio-iso", uuid.NewString()
		v.Spec.Driver = &driver
	}
	namespace := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": v.Spec.Namespace, "uid": v.Spec.NamespaceUID, "labels": map[string]any{"network.ani.io/tenant-id": v.TenantID, "network.ani.io/managed-by": "ani-network-service"}}}}
	client := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{podResource: "PodList"}, namespace)
	client.PrependReactor("create", "virtualmachines", func(action ktesting.Action) (bool, runtime.Object, error) {
		object := action.(ktesting.CreateAction).GetObject().(*unstructured.Unstructured)
		object.SetUID(types.UID(uuid.NewString()))
		object.SetResourceVersion("10")
		return false, nil, nil
	})
	return NewNativeWithClients(client, nil), v, client
}

func TestNativeConsoleTabletRenderForBothOS(t *testing.T) {
	for _, family := range []string{"linux", "windows"} {
		t.Run(family, func(t *testing.T) {
			native, v, client := inputNativeFixture(t, family)
			ref, err := native.EnsureVM(context.Background(), v)
			if err != nil {
				t.Fatal(err)
			}
			root, err := client.Resource(vmResource).Namespace(v.Spec.Namespace).Get(context.Background(), ref.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			inputs, _, err := unstructured.NestedSlice(root.Object, "spec", "template", "spec", "domain", "devices", "inputs")
			wanted := []any{map[string]any{"name": "console-tablet", "type": "tablet", "bus": "usb"}}
			if err != nil || !reflect.DeepEqual(inputs, wanted) {
				t.Fatal("native VNC has no driver-independent absolute USB tablet", inputs, err)
			}
		})
	}
}

func inputStoppedFixture(t *testing.T) (*Native, biz.VM, *fake.FakeDynamicClient, *int) {
	t.Helper()
	native, v, client := inputNativeFixture(t, "linux")
	ref, err := native.EnsureVM(context.Background(), v)
	if err != nil {
		t.Fatal(err)
	}
	v.Native = ref
	root, err := client.Resource(vmResource).Namespace(v.Spec.Namespace).Get(context.Background(), ref.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	unstructured.RemoveNestedField(root.Object, "spec", "template", "spec", "domain", "devices", "inputs")
	root.SetResourceVersion("20")
	if _, err = client.Resource(vmResource).Namespace(v.Spec.Namespace).Update(context.Background(), root, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	calls := new(int)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls++
		if r.Method != http.MethodPut || !strings.HasSuffix(r.URL.Path, "/start") {
			t.Errorf("unexpected native request: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)
	native, err = NewNative(client, &rest.Config{Host: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	client.ClearActions()
	return native, v, client, calls
}

func TestNativeStartAddsTabletWithUIDRVFenceAndPreservesTemplate(t *testing.T) {
	ctx := context.Background()
	native, v, client, calls := inputStoppedFixture(t)
	root, _ := client.Resource(vmResource).Namespace(v.Spec.Namespace).Get(ctx, v.Native.Name, metav1.GetOptions{})
	keyboard := map[string]any{"name": "operator-keyboard", "type": "keyboard", "bus": "usb"}
	_ = unstructured.SetNestedSlice(root.Object, []any{keyboard}, "spec", "template", "spec", "domain", "devices", "inputs")
	_, _ = client.Resource(vmResource).Namespace(v.Spec.Namespace).Update(ctx, root, metav1.UpdateOptions{})
	before := root.DeepCopy()
	patches := 0
	client.PrependReactor("patch", "virtualmachines", func(action ktesting.Action) (bool, runtime.Object, error) {
		patches++
		request := action.(ktesting.PatchAction)
		var patch []map[string]any
		if request.GetPatchType() != types.JSONPatchType || json.Unmarshal(request.GetPatch(), &patch) != nil || len(patch) != 3 {
			t.Fatal("input migration is not a narrow JSON patch", string(request.GetPatch()))
		}
		if patch[0]["op"] != "test" || patch[0]["path"] != "/metadata/uid" || patch[0]["value"] != v.Native.UID || patch[1]["op"] != "test" || patch[1]["path"] != "/metadata/resourceVersion" || patch[1]["value"] != "20" || patch[2]["op"] != "add" || patch[2]["path"] != "/spec/template/spec/domain/devices/inputs" {
			t.Fatal("input migration lost its UID/RV fence", patch)
		}
		return false, nil, nil
	})
	if err := native.Start(ctx, v); err != nil || *calls != 1 || patches != 1 {
		t.Fatal("normal Start did not migrate inputs then issue one power request", err, *calls, patches)
	}
	after, _ := client.Resource(vmResource).Namespace(v.Spec.Namespace).Get(ctx, v.Native.Name, metav1.GetOptions{})
	inputs, _, _ := unstructured.NestedSlice(after.Object, "spec", "template", "spec", "domain", "devices", "inputs")
	if !reflect.DeepEqual(inputs, []any{keyboard, consoleTablet()}) {
		t.Fatal("existing input changed or USB tablet missing", inputs)
	}
	unstructured.RemoveNestedField(before.Object, "spec", "template", "spec", "domain", "devices", "inputs")
	unstructured.RemoveNestedField(after.Object, "spec", "template", "spec", "domain", "devices", "inputs")
	if !reflect.DeepEqual(before.Object, after.Object) {
		t.Fatal("input migration changed disks, firmware, network, or other native fields")
	}
	if err := native.ensureConsoleTablet(ctx, v); err != nil || patches != 1 || *calls != 1 {
		t.Fatal("existing managed tablet was duplicated or sent power", err, patches, *calls)
	}
}

func TestNativeStartTabletRequiresOwnedStoppedVM(t *testing.T) {
	for _, condition := range []string{"running-vmi", "old-launcher", "native-pending", "namespace-replaced", "tenant-label", "vm-replaced", "managed-name-conflict", "missing-rv", "closing"} {
		t.Run(condition, func(t *testing.T) {
			ctx := context.Background()
			native, v, client, calls := inputStoppedFixture(t)
			root, _ := client.Resource(vmResource).Namespace(v.Spec.Namespace).Get(ctx, v.Native.Name, metav1.GetOptions{})
			switch condition {
			case "running-vmi":
				vmi := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "kubevirt.io/v1", "kind": "VirtualMachineInstance", "metadata": map[string]any{"name": v.Native.Name, "namespace": v.Spec.Namespace, "uid": uuid.NewString()}, "status": map[string]any{"phase": "Running"}}}
				vmi.SetOwnerReferences(ownReference(v))
				_, _ = client.Resource(vmiResource).Namespace(v.Spec.Namespace).Create(ctx, vmi, metav1.CreateOptions{})
			case "old-launcher":
				pod := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{"name": "old-launcher", "namespace": v.Spec.Namespace, "uid": uuid.NewString()}, "status": map[string]any{"phase": "Running"}}}
				pod.SetLabels(labelVM(v))
				_, _ = client.Resource(podResource).Namespace(v.Spec.Namespace).Create(ctx, pod, metav1.CreateOptions{})
			case "native-pending":
				_ = unstructured.SetNestedSlice(root.Object, []any{map[string]any{"action": "Start"}}, "status", "stateChangeRequests")
			case "namespace-replaced":
				namespace, _ := client.Resource(nsResource).Get(ctx, v.Spec.Namespace, metav1.GetOptions{})
				namespace.SetUID(types.UID(uuid.NewString()))
				_, _ = client.Resource(nsResource).Update(ctx, namespace, metav1.UpdateOptions{})
			case "tenant-label":
				labels := root.GetLabels()
				labels["ani.io/tenant-id"] = uuid.NewString()
				root.SetLabels(labels)
			case "vm-replaced":
				root.SetUID(types.UID(uuid.NewString()))
			case "managed-name-conflict":
				_ = unstructured.SetNestedSlice(root.Object, []any{map[string]any{"name": "console-tablet", "type": "tablet", "bus": "virtio"}}, "spec", "template", "spec", "domain", "devices", "inputs")
			case "missing-rv":
				root.SetResourceVersion("")
			case "closing":
				v.Closing = true
			}
			_, _ = client.Resource(vmResource).Namespace(v.Spec.Namespace).Update(ctx, root, metav1.UpdateOptions{})
			client.ClearActions()
			err := native.Start(ctx, v)
			var notSubmitted *biz.NativeNotSubmittedError
			if !errors.As(err, &notSubmitted) || *calls != 0 {
				t.Fatal("unsafe Start changed inputs or submitted native power", err, *calls)
			}
			for _, action := range client.Actions() {
				if action.GetVerb() == "patch" || action.GetVerb() == "update" {
					t.Fatal("unsafe Start mutated native template", action)
				}
			}
		})
	}
}

func TestNativeStartTabletPatchRejectsConcurrentUIDOrRVChange(t *testing.T) {
	for _, field := range []string{"uid", "resourceVersion"} {
		t.Run(field, func(t *testing.T) {
			native, v, client, calls := inputStoppedFixture(t)
			client.PrependReactor("patch", "virtualmachines", func(action ktesting.Action) (bool, runtime.Object, error) {
				object, err := client.Tracker().Get(vmResource, v.Spec.Namespace, v.Native.Name)
				if err != nil {
					t.Fatal(err)
				}
				root := object.(*unstructured.Unstructured)
				if field == "uid" {
					root.SetUID(types.UID(uuid.NewString()))
				} else {
					root.SetResourceVersion("21")
				}
				if err = client.Tracker().Update(vmResource, root, v.Spec.Namespace); err != nil {
					t.Fatal(err)
				}
				return false, nil, nil
			})
			if err := native.Start(context.Background(), v); err == nil || *calls != 0 {
				t.Fatal("concurrent native identity/version change bypassed patch fence", err, *calls)
			}
			root, _ := client.Resource(vmResource).Namespace(v.Spec.Namespace).Get(context.Background(), v.Native.Name, metav1.GetOptions{})
			if _, found, _ := unstructured.NestedSlice(root.Object, "spec", "template", "spec", "domain", "devices", "inputs"); found {
				t.Fatal("failed test operation still changed inputs")
			}
		})
	}
}

func TestNativeExistingUSBTabletDoesNotDuplicate(t *testing.T) {
	for _, name := range []string{consoleTabletName, "existing-absolute-pointer"} {
		inputs := []any{map[string]any{"name": name, "type": "tablet", "bus": "usb"}}
		actual, changed, err := withConsoleTablet(inputs)
		if err != nil || changed || !reflect.DeepEqual(inputs, actual) {
			t.Fatal("compatible USB tablet duplicated", actual, changed, err)
		}
	}
}
