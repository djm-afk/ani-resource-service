package vm

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/google/uuid"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
)

// Literal requirements from KubeVirt v1.9.0 examples/vmi-windows.yaml.
func windowsDomainWant() map[string]any {
	return map[string]any{
		"features": map[string]any{
			"acpi": map[string]any{"enabled": true},
			"apic": map[string]any{"enabled": true},
			"hyperv": map[string]any{
				"relaxed":   map[string]any{"enabled": true},
				"vapic":     map[string]any{"enabled": true},
				"spinlocks": map[string]any{"enabled": true, "spinlocks": int64(8191)},
			},
		},
		"clock": map[string]any{
			"utc": map[string]any{},
			"timer": map[string]any{
				"hpet":   map[string]any{"present": false},
				"pit":    map[string]any{"present": true, "tickPolicy": "delay"},
				"rtc":    map[string]any{"present": true, "tickPolicy": "catchup"},
				"hyperv": map[string]any{"present": true},
			},
		},
	}
}

func windowsStoppedFixture(t *testing.T) (*Native, biz.VM, *fake.FakeDynamicClient, *int) {
	t.Helper()
	n, v, client, calls := inputStoppedFixture(t)
	v.Spec.Media.OSFamily = "windows"
	root, err := client.Resource(vmResource).Namespace(v.Spec.Namespace).Get(context.Background(), v.Native.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_ = unstructured.SetNestedSlice(root.Object, []any{consoleTablet()}, "spec", "template", "spec", "domain", "devices", "inputs")
	_ = unstructured.SetNestedMap(root.Object, map[string]any{"smm": map[string]any{"enabled": true}, "hyperv": map[string]any{"relaxed": map[string]any{"enabled": true}}}, "spec", "template", "spec", "domain", "features")
	_ = unstructured.SetNestedMap(root.Object, map[string]any{"utc": map[string]any{"offsetSeconds": int64(0)}, "timer": map[string]any{"kvm": map[string]any{"present": true}}}, "spec", "template", "spec", "domain", "clock")
	if _, err = client.Resource(vmResource).Namespace(v.Spec.Namespace).Update(context.Background(), root, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	client.ClearActions()
	return n, v, client, calls
}

func TestNativeWindowsDomainStartFillsMissingOnlyAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	n, v, client, calls := windowsStoppedFixture(t)
	before, _ := client.Resource(vmResource).Namespace(v.Spec.Namespace).Get(ctx, v.Native.Name, metav1.GetOptions{})
	patches := 0
	client.PrependReactor("patch", "virtualmachines", func(action ktesting.Action) (bool, runtime.Object, error) {
		patches++
		request := action.(ktesting.PatchAction)
		var patch []map[string]any
		if request.GetPatchType() != types.JSONPatchType || json.Unmarshal(request.GetPatch(), &patch) != nil || len(patch) != 4 {
			t.Fatal("Windows profile must use one UID/RV guarded features/clock patch", string(request.GetPatch()))
		}
		if patch[0]["op"] != "test" || patch[0]["path"] != "/metadata/uid" || patch[0]["value"] != v.Native.UID || patch[1]["op"] != "test" || patch[1]["path"] != "/metadata/resourceVersion" || patch[1]["value"] != "20" {
			t.Fatal("Windows profile lost UID/RV fence", patch)
		}
		for _, op := range patch[2:] {
			if op["op"] != "add" || op["path"] != "/spec/template/spec/domain/features" && op["path"] != "/spec/template/spec/domain/clock" {
				t.Fatal("Windows profile patched unrelated native field", op)
			}
		}
		return false, nil, nil
	})
	if err := n.Start(ctx, v); err != nil || *calls != 1 {
		t.Fatal("normal owned Stopped Windows Start failed", err, *calls)
	}
	after, _ := client.Resource(vmResource).Namespace(v.Spec.Namespace).Get(ctx, v.Native.Name, metav1.GetOptions{})
	want := windowsDomainWant()
	want["features"].(map[string]any)["smm"] = map[string]any{"enabled": true}
	want["clock"].(map[string]any)["utc"] = map[string]any{"offsetSeconds": int64(0)}
	want["clock"].(map[string]any)["timer"].(map[string]any)["kvm"] = map[string]any{"present": true}
	for key, value := range want {
		got, _, _ := unstructured.NestedMap(after.Object, "spec", "template", "spec", "domain", key)
		if !reflect.DeepEqual(got, value) {
			t.Fatalf("legacy Windows profile not completed without overriding other fields: %s got=%v want=%v", key, got, value)
		}
		unstructured.RemoveNestedField(before.Object, "spec", "template", "spec", "domain", key)
		unstructured.RemoveNestedField(after.Object, "spec", "template", "spec", "domain", key)
	}
	if patches != 1 || !reflect.DeepEqual(before.Object, after.Object) {
		t.Fatal("Windows profile changed disks/boot/firmware/compute/network or lacked a single patch", patches)
	}
	if err := n.Start(ctx, v); err != nil || patches != 1 || *calls != 2 {
		t.Fatal("profile was not idempotent on a subsequent normal Stopped Start", err, patches, *calls)
	}
}

func TestNativeWindowsDomainExistingEnsureIsReadOnly(t *testing.T) {
	n, v, client, calls := windowsStoppedFixture(t)
	before, _ := client.Resource(vmResource).Namespace(v.Spec.Namespace).Get(context.Background(), v.Native.Name, metav1.GetOptions{})
	if ref, err := n.EnsureVM(context.Background(), v); err != nil || ref.UID != v.Native.UID {
		t.Fatal(ref, err)
	}
	after, _ := client.Resource(vmResource).Namespace(v.Spec.Namespace).Get(context.Background(), v.Native.Name, metav1.GetOptions{})
	if !reflect.DeepEqual(before.Object, after.Object) || *calls != 0 {
		t.Fatal("EnsureVM observation migrated an existing Windows VM")
	}
	for _, a := range client.Actions() {
		if a.GetVerb() == "patch" || a.GetVerb() == "update" {
			t.Fatal("EnsureVM wrote an existing template", a)
		}
	}
}

func TestNativeWindowsDomainLinuxStartPreservesCompleteTemplate(t *testing.T) {
	n, v, client, calls := windowsStoppedFixture(t)
	v.Spec.Media.OSFamily = "linux"
	// A misleading name must never select Windows defaults.
	v.Spec.Media.Key = "windows-looking-media"
	before, _ := client.Resource(vmResource).Namespace(v.Spec.Namespace).Get(context.Background(), v.Native.Name, metav1.GetOptions{})
	if err := n.Start(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	after, _ := client.Resource(vmResource).Namespace(v.Spec.Namespace).Get(context.Background(), v.Native.Name, metav1.GetOptions{})
	if *calls != 1 || !reflect.DeepEqual(before.Object, after.Object) {
		t.Fatal("Linux Start changed features/clock or unrelated native state")
	}
}

func TestNativeWindowsDomainStartRejectsExplicitConflicts(t *testing.T) {
	cases := []struct {
		name  string
		path  []string
		value any
	}{
		{"acpi-disabled", []string{"features", "acpi", "enabled"}, false},
		{"apic-disabled", []string{"features", "apic", "enabled"}, false},
		{"relaxed-disabled", []string{"features", "hyperv", "relaxed", "enabled"}, false},
		{"vapic-disabled", []string{"features", "hyperv", "vapic", "enabled"}, false},
		{"spinlocks-disabled", []string{"features", "hyperv", "spinlocks", "enabled"}, false},
		{"spinlocks-other-retries", []string{"features", "hyperv", "spinlocks", "spinlocks"}, int64(4096)},
		{"hyperv-passthrough", []string{"features", "hypervPassthrough"}, map[string]any{}},
		{"timezone", []string{"clock", "timezone"}, "America/New_York"},
		{"utc-offset", []string{"clock", "utc", "offsetSeconds"}, int64(60)},
		{"hpet-enabled", []string{"clock", "timer", "hpet", "present"}, true},
		{"pit-disabled", []string{"clock", "timer", "pit", "present"}, false},
		{"pit-other-policy", []string{"clock", "timer", "pit", "tickPolicy"}, "discard"},
		{"rtc-disabled", []string{"clock", "timer", "rtc", "present"}, false},
		{"rtc-other-policy", []string{"clock", "timer", "rtc", "tickPolicy"}, "merge"},
		{"hypervclock-disabled", []string{"clock", "timer", "hyperv", "present"}, false},
		{"malformed-clock", []string{"clock"}, "wrong-type"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n, v, client, calls := windowsStoppedFixture(t)
			root, _ := client.Resource(vmResource).Namespace(v.Spec.Namespace).Get(context.Background(), v.Native.Name, metav1.GetOptions{})
			if err := unstructured.SetNestedField(root.Object, tc.value, append([]string{"spec", "template", "spec", "domain"}, tc.path...)...); err != nil {
				t.Fatal(err)
			}
			_, _ = client.Resource(vmResource).Namespace(v.Spec.Namespace).Update(context.Background(), root, metav1.UpdateOptions{})
			client.ClearActions()
			if err := n.Start(context.Background(), v); !errors.Is(err, biz.ErrChanged) || *calls != 0 {
				t.Fatal("explicit Windows profile conflict was not rejected before native power", err, *calls)
			}
			after, _ := client.Resource(vmResource).Namespace(v.Spec.Namespace).Get(context.Background(), v.Native.Name, metav1.GetOptions{})
			if !reflect.DeepEqual(root.Object, after.Object) {
				t.Fatal("conflicting Windows profile was overwritten")
			}
		})
	}
}

func TestNativeWindowsDomainStartRequiresOwnedStoppedIdentity(t *testing.T) {
	for _, condition := range []string{"running-vmi", "old-launcher", "pending", "missing-rv", "namespace-uid", "vm-uid", "tenant-label", "closing"} {
		t.Run(condition, func(t *testing.T) {
			n, v, client, calls := windowsStoppedFixture(t)
			root, _ := client.Resource(vmResource).Namespace(v.Spec.Namespace).Get(context.Background(), v.Native.Name, metav1.GetOptions{})
			switch condition {
			case "running-vmi", "failed-vmi":
				phase := "Running"
				if condition == "failed-vmi" {
					phase = "Failed"
				}
				vmi := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "kubevirt.io/v1", "kind": "VirtualMachineInstance", "metadata": map[string]any{"name": v.Native.Name, "namespace": v.Spec.Namespace, "uid": uuid.NewString()}, "status": map[string]any{"phase": phase}}}
				vmi.SetOwnerReferences(ownReference(v))
				_, _ = client.Resource(vmiResource).Namespace(v.Spec.Namespace).Create(context.Background(), vmi, metav1.CreateOptions{})
			case "old-launcher":
				pod := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{"name": "old-launcher", "namespace": v.Spec.Namespace, "uid": uuid.NewString()}, "status": map[string]any{"phase": "Running"}}}
				pod.SetLabels(labelVM(v))
				_, _ = client.Resource(podResource).Namespace(v.Spec.Namespace).Create(context.Background(), pod, metav1.CreateOptions{})
			case "pending":
				_ = unstructured.SetNestedSlice(root.Object, []any{map[string]any{"action": "Start"}}, "status", "stateChangeRequests")
			case "missing-rv":
				root.SetResourceVersion("")
			case "namespace-uid":
				ns, _ := client.Resource(nsResource).Get(context.Background(), v.Spec.Namespace, metav1.GetOptions{})
				ns.SetUID(types.UID(uuid.NewString()))
				_, _ = client.Resource(nsResource).Update(context.Background(), ns, metav1.UpdateOptions{})
			case "vm-uid":
				root.SetUID(types.UID(uuid.NewString()))
			case "tenant-label":
				labels := root.GetLabels()
				labels["ani.io/tenant-id"] = uuid.NewString()
				root.SetLabels(labels)
			case "closing":
				v.Closing = true
			}
			_, _ = client.Resource(vmResource).Namespace(v.Spec.Namespace).Update(context.Background(), root, metav1.UpdateOptions{})
			client.ClearActions()
			if err := n.Start(context.Background(), v); err == nil || *calls != 0 {
				t.Fatal("unsafe Windows profile migration or native Start accepted", condition, err, *calls)
			}
			for _, action := range client.Actions() {
				if action.GetVerb() == "patch" || action.GetVerb() == "update" {
					t.Fatal("unsafe identity/state changed the native domain", action)
				}
			}
		})
	}
}

func TestNativeWindowsDomainStartRejectsConcurrentUIDOrRVChange(t *testing.T) {
	for _, field := range []string{"uid", "resourceVersion"} {
		t.Run(field, func(t *testing.T) {
			n, v, client, calls := windowsStoppedFixture(t)
			client.PrependReactor("patch", "virtualmachines", func(action ktesting.Action) (bool, runtime.Object, error) {
				obj, err := client.Tracker().Get(vmResource, v.Spec.Namespace, v.Native.Name)
				if err != nil {
					t.Fatal(err)
				}
				root := obj.(*unstructured.Unstructured)
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
			if err := n.Start(context.Background(), v); err == nil || *calls != 0 {
				t.Fatal("concurrent Windows UID/RV replacement bypassed CAS", err, *calls)
			}
			after, _ := client.Resource(vmResource).Namespace(v.Spec.Namespace).Get(context.Background(), v.Native.Name, metav1.GetOptions{})
			if _, found, _ := unstructured.NestedMap(after.Object, "spec", "template", "spec", "domain", "clock", "timer", "hpet"); found {
				t.Fatal("failed CAS still added Windows domain defaults")
			}
		})
	}
}

func TestNativeWindowsDomainCreateProjection(t *testing.T) {
	for _, family := range []string{"windows", "linux"} {
		t.Run(family, func(t *testing.T) {
			n, v, client := inputNativeFixture(t, family)
			ref, err := n.EnsureVM(context.Background(), v)
			if err != nil {
				t.Fatal(err)
			}
			root, err := client.Resource(vmResource).Namespace(v.Spec.Namespace).Get(context.Background(), ref.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			for key, want := range windowsDomainWant() {
				got, found, err := unstructured.NestedMap(root.Object, "spec", "template", "spec", "domain", key)
				if family == "linux" {
					if err != nil || found {
						t.Fatalf("Linux acquired a Windows %s: %v %v", key, got, err)
					}
				} else if err != nil || !found || !reflect.DeepEqual(got, want) {
					t.Fatalf("Windows %s missing the v1.9 minimum profile: got=%v want=%v err=%v", key, got, want, err)
				}
			}
		})
	}
}
