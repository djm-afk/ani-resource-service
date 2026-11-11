package vm

import (
	"context"
	"testing"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
)

const classSourceID = "21cf875f-781a-41db-81cf-7063f4f4cfc4"

// classNativeSource is a standard single-NIC Linux source with one system disk
// and the platform one-shot seed.
func classNativeSource(sourceID string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kubevirt.io/v1",
		"kind":       "VirtualMachine",
		"metadata": map[string]any{
			"name": "vm-source", "namespace": "tenant-a", "uid": "source-uid",
			"labels": map[string]any{"vm.ani.io/vm-id": sourceID},
		},
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
			"volumes": []any{
				map[string]any{"name": "disk-sys", "persistentVolumeClaim": map[string]any{"claimName": "pvc-sys"}},
				map[string]any{"name": "guest-initialization", "cloudInitNoCloud": map[string]any{
					"secretRef":            map[string]any{"name": "vm-init-" + sourceID},
					"networkDataSecretRef": map[string]any{"name": "vm-init-" + sourceID},
				}},
			},
			"domain": map[string]any{"devices": map[string]any{
				"disks":      []any{map[string]any{"name": "disk-sys", "disk": map[string]any{"bus": "virtio"}}},
				"interfaces": []any{map[string]any{"name": "default", "masquerade": map[string]any{}}},
			}},
			"networks": []any{map[string]any{"name": "default", "pod": map[string]any{}}},
		}}},
	}}
}

func classTarget(sourceID string) biz.VM {
	return biz.VM{ID: "target-vm", Spec: biz.Spec{
		Namespace: "tenant-a",
		CloneSource: &biz.CloneSource{
			SourceVMID: sourceID,
			Native:     biz.ObjectRef{Kind: "VirtualMachine", Namespace: "tenant-a", Name: "vm-source", UID: "source-uid"},
			Disks:      []biz.CloneDisk{{SourceDiskID: "disk-1", VolumeName: "disk-sys", Role: "system", Name: "system", SizeGB: 8}},
		},
	}}
}

func classBusinessSource(sourceID string) biz.VM {
	return biz.VM{
		ID:       sourceID,
		TenantID: "64c5c35a-f554-432c-b4bc-7b4f36f08420",
		Native:   biz.ObjectRef{Kind: "VirtualMachine", Namespace: "tenant-a", Name: "vm-source", UID: "source-uid"},
		Spec: biz.Spec{
			Namespace:           "tenant-a",
			Digest:              "digest",
			BootDevice:          "SYSTEM_DISK",
			GuestInitialization: &biz.GuestInitialization{Method: "CLOUD_INIT_NOCLOUD", Username: "ani"},
		},
		InitializationSecret: biz.ObjectRef{Kind: "Secret", Namespace: "tenant-a", Name: "vm-init-" + sourceID, UID: "secret-uid"},
		CurrentBootDevice:    "SYSTEM_DISK",
	}
}

func TestClassifyCloneSource(t *testing.T) {
	target := classTarget(classSourceID)
	source := classBusinessSource(classSourceID)
	if result, claims := classifyCloneSource(&source, target, classNativeSource(classSourceID)); !result.Ready() || claims["disk-sys"] != "pvc-sys" {
		t.Fatalf("standard source rejected: %v %v", result, claims)
	}

	nonStandard := classNativeSource(classSourceID)
	volumes, _, _ := unstructured.NestedSlice(nonStandard.Object, "spec", "template", "spec", "volumes")
	volumes[1].(map[string]any)["cloudInitNoCloud"].(map[string]any)["networkDataSecretRef"] = map[string]any{"name": "caller-secret"}
	if err := unstructured.SetNestedSlice(nonStandard.Object, volumes, "spec", "template", "spec", "volumes"); err != nil {
		t.Fatal(err)
	}
	if result, _ := classifyCloneSource(&source, target, nonStandard); result.Ready() {
		t.Fatal("non-standard initialization secret accepted")
	}

	mediaBoot := source
	mediaBoot.CurrentBootDevice = "INSTALL_MEDIA"
	if result, _ := classifyCloneSource(&mediaBoot, target, classNativeSource(classSourceID)); result.Ready() {
		t.Fatal("source that boots from media accepted")
	}

	// A retired or still-recorded historical media copy is normal history: Eject
	// only unbinds the mount, so neither may gate a source whose template carries
	// no media volume. Only the template and the boot device decide.
	historical := source
	historical.MediaCopies = []biz.MediaCopy{{ID: "install", Tombstone: true}, {ID: "driver", Tombstone: true}}
	if result, _ := classifyCloneSource(&historical, target, classNativeSource(classSourceID)); !result.Ready() {
		t.Fatalf("retired historical media rejected: %v", result)
	}
	ejectedMedia := source
	ejectedMedia.MediaCopies = []biz.MediaCopy{{ID: "install"}}
	if result, _ := classifyCloneSource(&ejectedMedia, target, classNativeSource(classSourceID)); !result.Ready() {
		t.Fatalf("an ejected historical copy blocked the clone: %v", result)
	}
	mountedMedia := classNativeSource(classSourceID)
	volumes, _, _ = unstructured.NestedSlice(mountedMedia.Object, "spec", "template", "spec", "volumes")
	volumes = append(volumes, map[string]any{"name": "media-install", "persistentVolumeClaim": map[string]any{"claimName": "pvc-media"}})
	if err := unstructured.SetNestedSlice(mountedMedia.Object, volumes, "spec", "template", "spec", "volumes"); err != nil {
		t.Fatal(err)
	}
	if result, _ := classifyCloneSource(&source, target, mountedMedia); result.Ready() {
		t.Fatal("source with a mounted media volume accepted")
	}

	noGuest := source
	noGuest.Spec.GuestInitialization = nil
	if result, _ := classifyCloneSource(&noGuest, target, classNativeSource(classSourceID)); result.Ready() {
		t.Fatal("seed without business guest initialization accepted")
	}
}

// TestClassifyCloneSourceBusinessScope covers the two source facts that live
// only in the business record. The native template cannot reveal either one: an
// automatic profile and a manual VNIC render the same interface, and backend
// state is a separate PVC that the clone does not reproduce.
func TestClassifyCloneSourceBusinessScope(t *testing.T) {
	target := classTarget(classSourceID)
	for _, test := range []struct {
		name   string
		mutate func(*biz.VM)
		reason string
	}{
		{
			name: "manual applied interface",
			mutate: func(v *biz.VM) {
				v.NetworkInterfaces = []biz.NetworkInterface{{Slot: "primary",
					Desired: biz.NetworkInterfaceChoice{Mode: "manual", VNicID: "vnic_11111111111111111111111111111111"},
					Applied: biz.NetworkInterfaceChoice{Mode: "manual", VNicID: "vnic_11111111111111111111111111111111"}}}
			},
			reason: "source uses a manual primary network interface",
		},
		{
			name:   "persistent TPM backend state",
			mutate: func(v *biz.VM) { v.Spec.Media.PersistentTPM = true },
			reason: "source uses persistent TPM/UEFI backend state",
		},
		{
			name:   "persistent UEFI backend state",
			mutate: func(v *biz.VM) { v.Spec.Media.PersistentUEFI = true },
			reason: "source uses persistent TPM/UEFI backend state",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := classBusinessSource(classSourceID)
			test.mutate(&source)
			result, _ := classifyCloneSource(&source, target, classNativeSource(classSourceID))
			if result.Ready() || len(result.Unsupported) != 1 || result.Unsupported[0].Reason != test.reason {
				t.Fatalf("business scope not rejected precisely: %v", result)
			}
		})
	}
	// The same business record with an applied automatic interface and no backend
	// state stays admitted, so the new rule does not narrow the ordinary source.
	// A pending (desired-manual, applied-automatic) switch is the state the live
	// source `test` is in: the native template still carries the automatic
	// profile, so it stays admissible until the switch takes effect.
	source := classBusinessSource(classSourceID)
	source.NetworkInterfaces = []biz.NetworkInterface{{Slot: "primary",
		Desired: biz.NetworkInterfaceChoice{Mode: "manual", VNicID: "vnic_11111111111111111111111111111111", Name: "manual"},
		Applied: biz.NetworkInterfaceChoice{Mode: "automatic", VNicID: "vnic_11111111111111111111111111111111"}}}
	if result, _ := classifyCloneSource(&source, target, classNativeSource(classSourceID)); !result.Ready() {
		t.Fatalf("pending manual switch rejected as already applied: %v", result)
	}
}

// TestCloneSourceEligibilitySeedAndRecheck covers the full admission and the
// frozen-dependency-free recheck. The source's own one-shot initialization Secret
// is not a clone input, so its absence must not gate the disk copy.
func TestCloneSourceEligibilitySeedAndRecheck(t *testing.T) {
	ctx := context.Background()
	target := classTarget(classSourceID)
	source := classBusinessSource(classSourceID)
	// Baseline native source with snapshot capability reported.
	status := classNativeSource(classSourceID)
	if err := unstructured.SetNestedSlice(status.Object, []any{map[string]any{"name": "disk-sys", "enabled": true}}, "status", "volumeSnapshotStatuses"); err != nil {
		t.Fatal(err)
	}
	pvc := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "PersistentVolumeClaim",
		"metadata": map[string]any{"name": "pvc-sys", "namespace": "tenant-a", "uid": "pvc-uid"},
		"spec":     map[string]any{"volumeMode": "Block", "accessModes": []any{"ReadWriteOnce"}, "storageClassName": "ani-block"},
		"status":   map[string]any{"phase": "Bound"},
	}}
	storageClass := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "storage.k8s.io/v1", "kind": "StorageClass",
		"metadata":          map[string]any{"name": "ani-block"},
		"volumeBindingMode": "Immediate",
	}}
	kinds := map[schema.GroupVersionResource]string{
		vmResource:           "VirtualMachineList",
		secretResource:       "SecretList",
		pvcResource:          "PersistentVolumeClaimList",
		storageClassResource: "StorageClassList",
	}
	client := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), kinds, copyObject(status), copyObject(pvc), copyObject(storageClass))
	native := NewNativeWithClients(client, nil)

	result, err := native.ObserveSourceCloneEligibility(ctx, target, source)
	if err != nil || !result.Ready() {
		t.Fatalf("full admission failed: %v %v", result, err)
	}

	// The cheap recheck never re-reads PVC, StorageClass or the source Secret.
	reads := map[string]int{}
	client.PrependReactor("get", "*", func(action ktesting.Action) (bool, runtime.Object, error) {
		reads[action.GetResource().Resource]++
		return false, nil, nil
	})
	result, err = native.RecheckCloneSource(ctx, target)
	if err != nil || !result.Ready() {
		t.Fatalf("recheck failed: %v %v", result, err)
	}
	if reads["persistentvolumeclaims"] != 0 || reads["storageclasses"] != 0 || reads["secrets"] != 0 {
		t.Fatalf("recheck read a frozen dependency: %v", reads)
	}
	if reads["virtualmachines"] == 0 {
		t.Fatal("recheck did not read the same named source")
	}
}

// toAnyMap converts typed labels into the map[string]any an unstructured object
// needs.
func toAnyMap(values map[string]string) map[string]any {
	result := make(map[string]any, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func copyObject(object *unstructured.Unstructured) *unstructured.Unstructured {
	return object.DeepCopy()
}
