package vm

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic/fake"
)

func snapshotObject(api, kind, name, namespace string, fields map[string]any) *unstructured.Unstructured {
	object := &unstructured.Unstructured{Object: map[string]any{"apiVersion": api, "kind": kind, "metadata": map[string]any{"name": name, "uid": uuid.NewString()}}}
	if namespace != "" {
		object.SetNamespace(namespace)
	}
	for key, value := range fields {
		object.Object[key] = value
	}
	return object
}
func TestSnapshotUsesNativeSelectedDeleteClassWithoutChangingSharedPolicy(t *testing.T) {
	sc := snapshotObject("storage.k8s.io/v1", "StorageClass", "block-sc", "", map[string]any{"provisioner": "rbd.csi.example"})
	profile := snapshotObject("cdi.kubevirt.io/v1beta1", "StorageProfile", "block-sc", "", map[string]any{"status": map[string]any{"snapshotClass": "retain-class"}})
	retained := snapshotObject("snapshot.storage.k8s.io/v1", "VolumeSnapshotClass", "retain-class", "", map[string]any{"driver": "rbd.csi.example", "deletionPolicy": "Retain"})
	deletable := snapshotObject("snapshot.storage.k8s.io/v1", "VolumeSnapshotClass", "delete-class", "", map[string]any{"driver": "rbd.csi.example", "deletionPolicy": "Delete"})
	client := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{volumeSnapshotClassResource: "VolumeSnapshotClassList"}, sc, profile, retained, deletable)
	native := NewNativeWithClients(client, nil)
	if _, _, err := native.snapshotClass(t.Context(), "block-sc"); !errors.Is(err, biz.ErrDependency) {
		t.Fatal("Retain profile silently switched to alternate Delete class", err)
	}
	_ = unstructured.SetNestedField(profile.Object, "delete-class", "status", "snapshotClass")
	_, _ = client.Resource(snapshotProfileResource).Update(t.Context(), profile, metav1.UpdateOptions{})
	_, class, err := native.snapshotClass(t.Context(), "block-sc")
	if err != nil || class.Name != "delete-class" || class.UID != string(deletable.GetUID()) {
		t.Fatal(class, err)
	}
	for _, action := range client.Actions() {
		if action.GetVerb() != "get" && action.GetVerb() != "list" && action.GetVerb() != "update" {
			t.Fatal("class selection changed shared infrastructure", action)
		}
	}
}

func nativeSnapshotFixture(t *testing.T) (*Native, biz.VM, biz.Snapshot, *fake.FakeDynamicClient) {
	t.Helper()
	native, v, client := mediaNativeFixture(t)
	v.Spec.Media.Kind = "DISK_IMAGE"
	v.Spec.Media.PersistentTPM = false
	v.Spec.Media.PersistentUEFI = false
	v.CurrentBootDevice = "SYSTEM_DISK"
	v.MediaSlots = nil
	v.MediaCopies = nil
	sc := snapshotObject("storage.k8s.io/v1", "StorageClass", v.Spec.StorageClass, "", map[string]any{"provisioner": "rbd.csi.example"})
	v.Spec.StorageClassUID = string(sc.GetUID())
	profile := snapshotObject("cdi.kubevirt.io/v1beta1", "StorageProfile", v.Spec.StorageClass, "", map[string]any{"status": map[string]any{"snapshotClass": "delete-class"}})
	class := snapshotObject("snapshot.storage.k8s.io/v1", "VolumeSnapshotClass", "delete-class", "", map[string]any{"driver": "rbd.csi.example", "deletionPolicy": "Delete"})
	for _, item := range []struct {
		resource schema.GroupVersionResource
		object   *unstructured.Unstructured
	}{{snapshotStorageResource, sc}, {snapshotProfileResource, profile}, {volumeSnapshotClassResource, class}} {
		if _, err := client.Resource(item.resource).Create(t.Context(), item.object, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	// The fixture reactor allocates a UID on create, as the actual API does.
	sc, _ = client.Resource(snapshotStorageResource).Get(t.Context(), sc.GetName(), metav1.GetOptions{})
	v.Spec.StorageClassUID = string(sc.GetUID())
	root, _ := client.Resource(vmResource).Namespace(v.Spec.Namespace).Get(t.Context(), v.Native.Name, metav1.GetOptions{})
	root.SetResourceVersion("17")
	volumes, devices, _ := mediaLists(root, "spec", "template", "spec")
	for _, name := range []string{"data-a", "data-b"} {
		d := biz.NewInitialDisk(v.ID, name, name, "data", 4)
		v.Disks = append(v.Disks, d)
		volumes = append(volumes, map[string]any{"name": d.VolumeName, "dataVolume": map[string]any{"name": d.DV.Name, "hotpluggable": true}})
		devices = append(devices, diskDevice(d))
	}
	_ = unstructured.SetNestedSlice(root.Object, volumes, "spec", "template", "spec", "volumes")
	_ = unstructured.SetNestedSlice(root.Object, devices, "spec", "template", "spec", "domain", "devices", "disks")
	_, _ = client.Resource(vmResource).Namespace(v.Spec.Namespace).Update(t.Context(), root, metav1.UpdateOptions{})
	for i := range v.Disks {
		d := &v.Disks[i]
		d.DV.Namespace = v.Spec.Namespace
		d.DV.UID = uuid.NewString()
		d.PVC = biz.ObjectRef{Kind: "PersistentVolumeClaim", Namespace: v.Spec.Namespace, Name: d.DV.Name}
		pvc := snapshotObject("v1", "PersistentVolumeClaim", d.PVC.Name, v.Spec.Namespace, map[string]any{"spec": map[string]any{"volumeMode": "Block", "storageClassName": v.Spec.StorageClass}, "status": map[string]any{"phase": "Bound"}})
		yes := true
		pvc.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "cdi.kubevirt.io/v1beta1", Kind: "DataVolume", Name: d.DV.Name, UID: types.UID(d.DV.UID), Controller: &yes}})
		created, err := client.Resource(pvcResource).Namespace(v.Spec.Namespace).Create(t.Context(), pvc, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		d.PVC = objectRef(created)
	}
	s, err := biz.NewSnapshot(v, uuid.NewString(), "checkpoint")
	if err != nil {
		t.Fatal(err)
	}
	s, err = native.PrepareSnapshot(t.Context(), v, s)
	if err != nil {
		t.Fatal(err)
	}
	s, err = native.EnsureSnapshot(t.Context(), v, s)
	if err != nil {
		t.Fatal(err)
	}
	object, _ := client.Resource(snapshotResource).Namespace(v.Spec.Namespace).Get(t.Context(), s.Native.Name, metav1.GetOptions{})
	contentName := "vmsnapshot-content-" + s.Native.UID
	var backups []any
	var included []string
	content := snapshotObject("snapshot.kubevirt.io/v1beta1", "VirtualMachineSnapshotContent", contentName, v.Spec.Namespace, map[string]any{"spec": map[string]any{"virtualMachineSnapshotName": s.Native.Name, "source": map[string]any{"virtualMachine": map[string]any{"metadata": map[string]any{"uid": v.Native.UID}}}}, "status": map[string]any{"readyToUse": true}})
	// Native SnapshotContent deliberately has no owner reference to Snapshot.
	content, err = client.Resource(snapshotContentResource).Namespace(v.Spec.Namespace).Create(t.Context(), content, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range s.Required {
		name := "vs-" + required.DiskID
		included = append(included, required.Name)
		vs := snapshotObject("snapshot.storage.k8s.io/v1", "VolumeSnapshot", name, v.Spec.Namespace, map[string]any{"spec": map[string]any{"volumeSnapshotClassName": required.Class.Name, "source": map[string]any{"persistentVolumeClaimName": required.PVC.Name}}, "status": map[string]any{"readyToUse": true, "boundVolumeSnapshotContentName": "content-" + required.DiskID}})
		yes := true
		vs.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "snapshot.kubevirt.io/v1beta1", Kind: "VirtualMachineSnapshotContent", Name: content.GetName(), UID: content.GetUID(), Controller: &yes}})
		vs, err = client.Resource(volumeSnapshotResource).Namespace(v.Spec.Namespace).Create(t.Context(), vs, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		vsc := snapshotObject("snapshot.storage.k8s.io/v1", "VolumeSnapshotContent", "content-"+required.DiskID, "", map[string]any{"spec": map[string]any{"deletionPolicy": "Delete", "volumeSnapshotRef": map[string]any{"uid": string(vs.GetUID()), "namespace": v.Spec.Namespace, "name": name}}})
		if _, err = client.Resource(volumeSnapshotContentResource).Create(t.Context(), vsc, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		backups = append(backups, map[string]any{"volumeName": required.Name, "persistentVolumeClaim": map[string]any{"metadata": map[string]any{"uid": required.PVC.UID}}, "volumeSnapshotName": name})
	}
	_ = unstructured.SetNestedSlice(content.Object, backups, "spec", "volumeBackups")
	_, _ = client.Resource(snapshotContentResource).Namespace(v.Spec.Namespace).Update(t.Context(), content, metav1.UpdateOptions{})
	_ = unstructured.SetNestedMap(object.Object, map[string]any{"sourceUID": v.Native.UID, "phase": "Succeeded", "readyToUse": true, "virtualMachineSnapshotContentName": contentName, "snapshotVolumes": map[string]any{"includedVolumes": stringsToAny(included), "excludedVolumes": []any{}}, "sourceIndications": []any{map[string]any{"indication": "NoGuestAgent"}}}, "status")
	_, _ = client.Resource(snapshotResource).Namespace(v.Spec.Namespace).Update(t.Context(), object, metav1.UpdateOptions{})
	s, err = native.ObserveSnapshot(t.Context(), v, s)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Complete {
		t.Fatal("complete native fixture rejected", s)
	}
	v.Snapshots = []biz.Snapshot{s}
	return native, v, s, client
}
func stringsToAny(values []string) []any {
	result := make([]any, len(values))
	for i, value := range values {
		result[i] = value
	}
	return result
}

func TestNativeSnapshotInventoryCompletenessUIDFencesAndControllerCleanup(t *testing.T) {
	native, v, s, client := nativeSnapshotFixture(t)
	if len(s.Inventory) != 3 || len(s.Indications) != 1 || s.Indications[0] != "NoGuestAgent" {
		t.Fatal(s)
	}
	// Lower layers may be unavailable or carry historic artifacts. Readiness
	// and deletion still depend only on the registered native Snapshot.
	s.Artifacts = []biz.ObjectRef{{Kind: "VolumeSnapshotContent", Name: "historic", UID: "historic-uid"}}
	before := len(client.Actions())
	ready, err := native.ObserveSnapshot(t.Context(), v, s)
	if err != nil || !ready.Complete {
		t.Fatal(ready, err)
	}
	if done, err := native.DeleteSnapshot(t.Context(), v, s); err != nil || done {
		t.Fatal(done, err)
	}
	if done, err := native.DeleteSnapshot(t.Context(), v, s); err != nil || !done {
		t.Fatal(done, err)
	}
	for _, action := range client.Actions()[before:] {
		if action.GetResource() != nsResource && action.GetResource() != snapshotResource {
			t.Fatal("snapshot read internal proof layer", action)
		}
	}
}

func TestNativeRestoreUsesSameVMPoliciesAndAcceptsPendingPVCWithoutPowerOrInitialization(t *testing.T) {
	native, v, s, client := nativeSnapshotFixture(t)
	ref := biz.ObjectRef{Kind: "VirtualMachineRestore", Namespace: v.Spec.Namespace, Name: "restore-" + uuid.NewString()}
	first, err := native.EnsureRestore(t.Context(), v, s, ref)
	if err != nil || first.Native.UID == "" || first.Complete {
		t.Fatal(first, err)
	}
	object, _ := client.Resource(restoreResource).Namespace(v.Spec.Namespace).Get(t.Context(), ref.Name, metav1.GetOptions{})
	for field, expected := range map[string]string{"targetReadinessPolicy": "FailImmediate", "volumeRestorePolicy": "RandomizeNames", "volumeOwnershipPolicy": "Vm"} {
		if snapshotString(object, "spec", field) != expected {
			t.Fatal(field)
		}
	}
	root, _ := client.Resource(vmResource).Namespace(v.Spec.Namespace).Get(t.Context(), v.Native.Name, metav1.GetOptions{})
	annotations := root.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations["restore.kubevirt.io/lastRestoreUID"] = object.GetName() + "-" + string(object.GetUID())
	root.SetAnnotations(annotations)
	volumes, devices, _ := mediaLists(root, "spec", "template", "spec")
	var restores []any
	for _, disk := range s.Inventory {
		name := "restored-" + disk.ID
		pvc := snapshotObject("v1", "PersistentVolumeClaim", name, v.Spec.Namespace, map[string]any{"spec": map[string]any{"volumeMode": "Block", "resources": map[string]any{"requests": map[string]any{"storage": fmt.Sprint(disk.RequestedBytes)}}, "storageClassName": v.Spec.StorageClass, "dataSource": map[string]any{"apiGroup": "snapshot.storage.k8s.io", "kind": "VolumeSnapshot", "name": "vs-" + disk.ID}}, "status": map[string]any{"phase": "Pending"}})
		pvc.SetOwnerReferences(ownReference(v))
		pvc.SetAnnotations(map[string]string{"restore.kubevirt.io/lastRestoreUID": object.GetName() + "-" + string(object.GetUID())})
		if _, err = client.Resource(pvcResource).Namespace(v.Spec.Namespace).Create(t.Context(), pvc, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		for i, value := range volumes {
			m := value.(map[string]any)
			if m["name"] == disk.VolumeName {
				volumes[i] = map[string]any{"name": disk.VolumeName, "persistentVolumeClaim": map[string]any{"claimName": name}}
			}
		}
		restores = append(restores, map[string]any{"volumeName": disk.VolumeName, "persistentVolumeClaim": name, "volumeSnapshotName": "vs-" + disk.ID})
	}
	_ = unstructured.SetNestedSlice(root.Object, volumes, "spec", "template", "spec", "volumes")
	_ = unstructured.SetNestedSlice(root.Object, devices, "spec", "template", "spec", "domain", "devices", "disks")
	_, _ = client.Resource(vmResource).Namespace(v.Spec.Namespace).Update(t.Context(), root, metav1.UpdateOptions{})
	_ = unstructured.SetNestedMap(object.Object, map[string]any{"complete": true, "restores": restores}, "status")
	_, _ = client.Resource(restoreResource).Namespace(v.Spec.Namespace).Update(t.Context(), object, metav1.UpdateOptions{})
	before := len(client.Actions())
	completed, err := native.EnsureRestore(t.Context(), v, s, first.Native)
	if err != nil || !completed.Complete || len(completed.Disks) != 3 {
		t.Fatal(completed, err)
	}
	v.Disks = completed.Disks
	done, err := native.ApplyRestoredConfiguration(t.Context(), v, s)
	if err != nil || done {
		t.Fatal("missing native hotplug contract did not produce a patch", done, err)
	}
	if done, err = native.ApplyRestoredConfiguration(t.Context(), v, s); err != nil || !done {
		t.Fatal(done, err)
	}
	for _, action := range client.Actions()[before:] {
		if action.GetVerb() == "create" || action.GetResource().Resource == "secrets" || action.GetSubresource() == "start" || action.GetSubresource() == "stop" {
			t.Fatal("restore rewrote initialization, recreated volumes or powered VM", action)
		}
	}
	root, _ = client.Resource(vmResource).Namespace(v.Spec.Namespace).Get(context.Background(), v.Native.Name, metav1.GetOptions{})
	volumes, devices, _ = mediaLists(root, "spec", "template", "spec")
	for _, disk := range completed.Disks {
		if disk.Preparation != "Pending" || disk.DV.Name != "" {
			t.Fatal("WFFC PVC not retained", disk)
		}
		if disk.Role == "data" {
			_, volume, _ := namedItems(volumes, disk.VolumeName)
			_, device, _ := namedItems(devices, disk.VolumeName)
			if !validDiskVolume(volume, disk) || !validDiskDevice(device, disk) {
				t.Fatal("restored data hotplug/serial contract missing")
			}
		}
	}
	_, cloud, _ := namedItems(volumes, "guest-initialization")
	secret, _, _ := unstructured.NestedString(cloud, "cloudInitNoCloud", "secretRef", "name")
	if secret != "private-init" {
		t.Fatal("original NoCloud reference changed")
	}

	// Native included/excluded is the business coverage boundary.
	snapshot, _ := client.Resource(snapshotResource).Namespace(v.Spec.Namespace).Get(t.Context(), s.Native.Name, metav1.GetOptions{})
	_ = unstructured.SetNestedStringSlice(snapshot.Object, []string{s.Required[0].Name}, "status", "snapshotVolumes", "excludedVolumes")
	_, _ = client.Resource(snapshotResource).Namespace(v.Spec.Namespace).Update(t.Context(), snapshot, metav1.UpdateOptions{})
	incomplete, err := native.ObserveSnapshot(t.Context(), v, s)
	if err != nil || incomplete.Complete {
		t.Fatal("excluded required volume accepted", incomplete, err)
	}
	// A closing worker only observes the existing Restore, including partial
	// status and a lost completed checkpoint/response. It never recreates it.
	v.Closing = true
	recovered, err := native.ObserveRestore(t.Context(), v, s, ref)
	if err != nil || recovered.Native.UID != first.Native.UID || len(recovered.Disks) != len(s.Inventory) {
		t.Fatal("lost restore response not recovered", recovered, err)
	}
	_ = unstructured.SetNestedMap(object.Object, map[string]any{"complete": false, "restores": restores}, "status")
	_, _ = client.Resource(restoreResource).Namespace(v.Spec.Namespace).Update(t.Context(), object, metav1.UpdateOptions{})
	from := len(client.Actions())
	partial, err := native.ObserveRestore(t.Context(), v, s, first.Native)
	if err != nil || partial.Complete || len(partial.Disks) != len(s.Inventory) {
		t.Fatal("partial status lost already-created PVCs", partial, err)
	}
	for _, action := range client.Actions()[from:] {
		if action.GetVerb() != "get" && action.GetVerb() != "list" {
			t.Fatal("closing observation mutated native resources", action)
		}
	}
	if err = client.Resource(restoreResource).Namespace(v.Spec.Namespace).Delete(t.Context(), first.Native.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := native.ObserveRestore(t.Context(), v, s, ref); !errors.Is(err, biz.ErrUnknown) {
		t.Fatal("lost result carrier inferred empty output", err)
	}
	s.RestoreOutputsRecorded = []biz.ObjectRef{first.Native}
	absent, err := native.ObserveRestore(t.Context(), v, s, first.Native)
	if err != nil || !absent.Absent || !absent.OutputsKnown || len(absent.Disks) != 0 {
		t.Fatal(absent, err)
	}

}

func TestRetiredVolumeIgnoresPVAndRejectsBusinessPVCReplacement(t *testing.T) {
	native, v, s, client := nativeSnapshotFixture(t)
	d := s.Inventory[0]
	d.Cleanup = []biz.ObjectRef{{Kind: "PersistentVolume", Name: "historic", UID: "pv-old"}}
	old, _ := client.Resource(pvcResource).Namespace(v.Spec.Namespace).Get(t.Context(), d.PVC.Name, metav1.GetOptions{})
	replacement := old.DeepCopy()
	replacement.SetUID(types.UID(uuid.NewString()))
	_, _ = client.Resource(pvcResource).Namespace(v.Spec.Namespace).Update(t.Context(), replacement, metav1.UpdateOptions{})
	if _, err := native.ObserveRetiredVolume(t.Context(), v, d); !errors.Is(err, biz.ErrOwnership) {
		t.Fatal("replacement PVC adopted", err)
	}
	_ = client.Resource(pvcResource).Namespace(v.Spec.Namespace).Delete(t.Context(), d.PVC.Name, metav1.DeleteOptions{})
	_ = client.Resource(dvResource).Namespace(v.Spec.Namespace).Delete(t.Context(), d.DV.Name, metav1.DeleteOptions{})
	before := len(client.Actions())
	observed, err := native.ObserveRetiredVolume(t.Context(), v, d)
	if err != nil || len(observed.Cleanup) != 1 {
		t.Fatal(observed, err)
	}
	if done, err := native.DeleteVolume(t.Context(), v, observed); err != nil || !done {
		t.Fatal(done, err)
	}
	for _, action := range client.Actions()[before:] {
		if action.GetResource() == pvResource {
			t.Fatal("retired cleanup read PV", action)
		}
	}
}
