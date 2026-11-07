package vm

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

var snapshotResource = schema.GroupVersionResource{Group: "snapshot.kubevirt.io", Version: "v1beta1", Resource: "virtualmachinesnapshots"}
var snapshotContentResource = schema.GroupVersionResource{Group: "snapshot.kubevirt.io", Version: "v1beta1", Resource: "virtualmachinesnapshotcontents"}
var restoreResource = schema.GroupVersionResource{Group: "snapshot.kubevirt.io", Version: "v1beta1", Resource: "virtualmachinerestores"}
var volumeSnapshotResource = schema.GroupVersionResource{Group: "snapshot.storage.k8s.io", Version: "v1", Resource: "volumesnapshots"}
var volumeSnapshotContentResource = schema.GroupVersionResource{Group: "snapshot.storage.k8s.io", Version: "v1", Resource: "volumesnapshotcontents"}
var volumeSnapshotClassResource = schema.GroupVersionResource{Group: "snapshot.storage.k8s.io", Version: "v1", Resource: "volumesnapshotclasses"}
var snapshotStorageResource = schema.GroupVersionResource{Group: "storage.k8s.io", Version: "v1", Resource: "storageclasses"}
var snapshotProfileResource = schema.GroupVersionResource{Group: "cdi.kubevirt.io", Version: "v1beta1", Resource: "storageprofiles"}

// Called from the existing lease/version-fenced transaction, alongside disks.
func saveSnapshots(ctx context.Context, tx pgx.Tx, v biz.VM) error {
	for _, s := range v.Snapshots {
		_, err := tx.Exec(ctx, `INSERT INTO vm.snapshots(tenant_id,vm_id,snapshot_id,record) VALUES($1,$2,$3,$4) ON CONFLICT(tenant_id,vm_id,snapshot_id) DO UPDATE SET record=excluded.record`, v.TenantID, v.ID, s.ID, encode(s))
		if err != nil {
			return err
		}
	}
	return nil
}

func snapshotString(o *unstructured.Unstructured, path ...string) string {
	s, _, _ := unstructured.NestedString(o.Object, path...)
	return s
}
func snapshotBool(o *unstructured.Unstructured, path ...string) bool {
	b, _, _ := unstructured.NestedBool(o.Object, path...)
	return b
}

// Follow KubeVirt's native class selection. No class is injected into a VM
// Snapshot request, and a Retain/ambiguous selection is never silently changed.
func (n *Native) snapshotClass(ctx context.Context, storageName string) (biz.ObjectRef, biz.ObjectRef, error) {
	sc, err := n.client.Resource(snapshotStorageResource).Get(ctx, storageName, metav1.GetOptions{})
	if err != nil {
		return biz.ObjectRef{}, biz.ObjectRef{}, err
	}
	driver := snapshotString(sc, "provisioner")
	if driver == "" {
		return biz.ObjectRef{}, biz.ObjectRef{}, biz.ErrDependency
	}
	name := ""
	profile, err := n.client.Resource(snapshotProfileResource).Get(ctx, storageName, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return biz.ObjectRef{}, biz.ObjectRef{}, err
	}
	if err == nil {
		name = snapshotString(profile, "status", "snapshotClass")
	}
	if name == "" {
		list, err := n.client.Resource(volumeSnapshotClassResource).List(ctx, metav1.ListOptions{})
		if err != nil {
			return biz.ObjectRef{}, biz.ObjectRef{}, err
		}
		var candidates, defaults []string
		for i := range list.Items {
			item := &list.Items[i]
			if snapshotString(item, "driver") != driver {
				continue
			}
			candidates = append(candidates, item.GetName())
			if _, present := item.GetAnnotations()["snapshot.storage.kubernetes.io/is-default-class"]; present {
				defaults = append(defaults, item.GetName())
			}
		}
		if len(candidates) == 1 {
			name = candidates[0]
		} else if len(defaults) == 1 {
			name = defaults[0]
		} else {
			return biz.ObjectRef{}, biz.ObjectRef{}, biz.ErrDependency
		}
	}
	class, err := n.client.Resource(volumeSnapshotClassResource).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return biz.ObjectRef{}, biz.ObjectRef{}, err
	}
	if snapshotString(class, "driver") != driver || snapshotString(class, "deletionPolicy") != "Delete" || class.GetUID() == "" {
		return biz.ObjectRef{}, biz.ObjectRef{}, biz.ErrDependency
	}
	return objectRef(sc), objectRef(class), nil
}

func (n *Native) snapshotRoot(ctx context.Context, v biz.VM) (*unstructured.Unstructured, error) {
	if err := n.namespace(ctx, v); err != nil {
		return nil, err
	}
	root, err := n.client.Resource(vmResource).Namespace(v.Spec.Namespace).Get(ctx, v.Native.Name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if root.GetAPIVersion() != "kubevirt.io/v1" || root.GetKind() != "VirtualMachine" || v.Native.UID == "" || string(root.GetUID()) != v.Native.UID || !matches(root, v) || root.GetDeletionTimestamp() != nil {
		return nil, biz.ErrOwnership
	}
	return root, nil
}
func (n *Native) PrepareSnapshot(ctx context.Context, v biz.VM, s biz.Snapshot) (biz.Snapshot, error) {
	root, err := n.snapshotRoot(ctx, v)
	if err != nil {
		return s, err
	}
	volumes, _, _ := unstructured.NestedSlice(root.Object, "spec", "template", "spec", "volumes")
	if v.Spec.Media.PersistentTPM || v.Spec.Media.PersistentUEFI {
		list, err := n.client.Resource(pvcResource).Namespace(v.Spec.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "persistent-state-for=" + v.Native.Name})
		if err != nil {
			return s, err
		}
		var backend *unstructured.Unstructured
		for i := range list.Items {
			candidate := &list.Items[i]
			if candidate.GetDeletionTimestamp() != nil {
				continue
			}
			if backend != nil {
				return s, biz.ErrDependency
			}
			backend = candidate
		}
		if backend == nil || !owned(backend, "VirtualMachine", v.Native.UID) {
			return s, biz.ErrDependency
		}
		name := "persistent-state-for-" + v.Native.Name
		if len(name) > 63 {
			return s, biz.ErrDependency
		}
		s.Required = append(s.Required, biz.SnapshotVolume{Name: name, Role: "backend", PVC: objectRef(backend)})
	}
	for i, volume := range s.Required {
		pvc, err := n.client.Resource(pvcResource).Namespace(v.Spec.Namespace).Get(ctx, volume.PVC.Name, metav1.GetOptions{})
		if err != nil {
			return s, err
		}
		if volume.PVC.UID == "" || volume.PVC.Namespace != v.Spec.Namespace || string(pvc.GetUID()) != volume.PVC.UID {
			return s, biz.ErrOwnership
		}
		ownerValid := owned(pvc, "VirtualMachine", v.Native.UID)
		for _, disk := range s.Inventory {
			if disk.ID == volume.DiskID && owned(pvc, "DataVolume", disk.DV.UID) {
				ownerValid = true
			}
		}
		for _, copy := range s.MediaCopies {
			if copy.ID == volume.MediaCopyID && owned(pvc, "DataVolume", copy.DV.UID) {
				ownerValid = true
			}
		}
		if !ownerValid {
			return s, biz.ErrOwnership
		}
		if volume.Role != "backend" {
			_, native, err := namedItems(volumes, volume.Name)
			if err != nil {
				return s, err
			}
			if native == nil {
				return s, biz.ErrChanged
			}
			claim, _, _ := unstructured.NestedString(native, "persistentVolumeClaim", "claimName")
			dv, _, _ := unstructured.NestedString(native, "dataVolume", "name")
			if claim != volume.PVC.Name && dv != volume.PVC.Name {
				return s, biz.ErrChanged
			}
		}
		mode := snapshotString(pvc, "spec", "volumeMode")
		if mode == "" {
			mode = "Filesystem"
		}
		if (volume.Role == "system" || volume.Role == "data") && mode != "Block" {
			return s, biz.ErrOwnership
		}
		if volume.Role == "backend" && mode != "Filesystem" {
			return s, biz.ErrOwnership
		}
		storageName := snapshotString(pvc, "spec", "storageClassName")
		if storageName == "" {
			return s, biz.ErrDependency
		}
		storage, class, err := n.snapshotClass(ctx, storageName)
		if err != nil {
			return s, err
		}
		if storageName == v.Spec.StorageClass && storage.UID != v.Spec.StorageClassUID {
			return s, biz.ErrChanged
		}
		volume.Storage = storage
		volume.Class = class
		s.Required[i] = volume
	}
	return s, nil
}
func validSnapshotObject(o *unstructured.Unstructured, v biz.VM, s biz.Snapshot) bool {
	return o.GetAPIVersion() == "snapshot.kubevirt.io/v1beta1" && o.GetKind() == "VirtualMachineSnapshot" && matches(o, v) && owned(o, "VirtualMachine", v.Native.UID) && o.GetLabels()["vm.ani.io/snapshot-id"] == s.ID && (s.Native.UID == "" || string(o.GetUID()) == s.Native.UID) && snapshotString(o, "spec", "source", "kind") == "VirtualMachine" && snapshotString(o, "spec", "source", "apiGroup") == "kubevirt.io" && snapshotString(o, "spec", "source", "name") == v.Native.Name && snapshotString(o, "spec", "deletionPolicy") == "Delete"
}
func (n *Native) EnsureSnapshot(ctx context.Context, v biz.VM, s biz.Snapshot) (biz.Snapshot, error) {
	old, err := n.client.Resource(snapshotResource).Namespace(v.Spec.Namespace).Get(ctx, s.Native.Name, metav1.GetOptions{})
	if err == nil {
		if !validSnapshotObject(old, v, s) {
			return s, biz.ErrOwnership
		}
		s.Native = objectRef(old)
		return n.ObserveSnapshot(ctx, v, s)
	}
	if !apierrors.IsNotFound(err) {
		return s, err
	}
	if s.Native.UID != "" {
		return s, biz.ErrOwnership
	}
	if _, err := n.snapshotRoot(ctx, v); err != nil {
		return s, err
	}
	labels := labelVM(v)
	labels["vm.ani.io/snapshot-id"] = s.ID
	object := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "snapshot.kubevirt.io/v1beta1", "kind": "VirtualMachineSnapshot", "metadata": map[string]any{"name": s.Native.Name, "namespace": v.Spec.Namespace}, "spec": map[string]any{"source": map[string]any{"apiGroup": "kubevirt.io", "kind": "VirtualMachine", "name": v.Native.Name}, "deletionPolicy": "Delete"}}}
	object.SetLabels(labels)
	object.SetOwnerReferences(ownReference(v))
	created, err := n.client.Resource(snapshotResource).Namespace(v.Spec.Namespace).Create(ctx, object, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return n.EnsureSnapshot(ctx, v, s)
	}
	if err != nil {
		return s, err
	}
	s.Native = objectRef(created)
	return n.ObserveSnapshot(ctx, v, s)
}
func (n *Native) ObserveSnapshot(ctx context.Context, v biz.VM, s biz.Snapshot) (biz.Snapshot, error) {
	if err := n.namespace(ctx, v); err != nil {
		return s, err
	}
	object, err := n.client.Resource(snapshotResource).Namespace(v.Spec.Namespace).Get(ctx, s.Native.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		s.Ready = false
		s.Complete = false
		s.Reason = "VM_SNAPSHOT_NATIVE_ABSENT"
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if !validSnapshotObject(object, v, s) {
		return s, biz.ErrOwnership
	}
	s.Native = objectRef(object)
	s.CreatedAt = object.GetCreationTimestamp().Time
	s.ObservedAt = time.Now().UTC()
	if uid := snapshotString(object, "status", "sourceUID"); uid != "" && uid != s.SourceVMUID {
		return s, biz.ErrOwnership
	}
	s.Ready = snapshotBool(object, "status", "readyToUse") && object.GetDeletionTimestamp() == nil
	s.Failed = snapshotString(object, "status", "phase") == "Failed" || snapshotString(object, "status", "error", "message") != ""
	s.Included, _, _ = unstructured.NestedStringSlice(object.Object, "status", "snapshotVolumes", "includedVolumes")
	s.Excluded, _, _ = unstructured.NestedStringSlice(object.Object, "status", "snapshotVolumes", "excludedVolumes")
	s.Indications, _, _ = unstructured.NestedStringSlice(object.Object, "status", "indications")
	indications, _, _ := unstructured.NestedSlice(object.Object, "status", "sourceIndications")
	for _, value := range indications {
		if m, ok := value.(map[string]any); ok {
			if code, ok := m["indication"].(string); ok {
				found := false
				for _, old := range s.Indications {
					if old == code {
						found = true
					}
				}
				if !found {
					s.Indications = append(s.Indications, code)
				}
			}
		}
	}
	sort.Strings(s.Included)
	sort.Strings(s.Excluded)
	sort.Strings(s.Indications)
	s.Complete = biz.SnapshotComplete(s)
	s.Reason = "VM_SNAPSHOT_PREPARING"
	if s.Failed {
		s.Reason = "VM_SNAPSHOT_NATIVE_FAILED"
		if message := snapshotString(object, "status", "error", "message"); message != "" {
			s.Reason += ": " + message
		}
	} else if s.Ready {
		if s.Complete {
			s.Reason = ""
		} else {
			s.Reason = "VM_SNAPSHOT_INCOMPLETE"
		}
	}
	return s, nil
}
func (n *Native) DeleteSnapshot(ctx context.Context, v biz.VM, s biz.Snapshot) (bool, error) {
	if err := n.namespace(ctx, v); err != nil {
		return false, err
	}
	for _, ref := range s.Restores {
		handedOff := false
		for _, saved := range s.RestoreOutputsRecorded {
			if saved == ref && ref.UID != "" {
				handedOff = true
			}
		}
		if !handedOff {
			return false, biz.ErrUnknown
		}
		object, err := n.client.Resource(restoreResource).Namespace(v.Spec.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return false, err
		}
		if !validRestoreObject(object, v, s, ref) {
			return false, biz.ErrOwnership
		}
		uid := object.GetUID()
		err = n.client.Resource(restoreResource).Namespace(v.Spec.Namespace).Delete(ctx, ref.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
		return false, err
	}
	object, err := n.client.Resource(snapshotResource).Namespace(v.Spec.Namespace).Get(ctx, s.Native.Name, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return false, err
	}
	if err == nil {
		if !validSnapshotObject(object, v, s) {
			return false, biz.ErrOwnership
		}
		uid := object.GetUID()
		policy := metav1.DeletePropagationForeground
		err = n.client.Resource(snapshotResource).Namespace(v.Spec.Namespace).Delete(ctx, s.Native.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}, PropagationPolicy: &policy})
		return false, err
	}
	return true, nil
}

func validRestoreObject(o *unstructured.Unstructured, v biz.VM, s biz.Snapshot, ref biz.ObjectRef) bool {
	return o.GetKind() == "VirtualMachineRestore" && o.GetAPIVersion() == "snapshot.kubevirt.io/v1beta1" && matches(o, v) && owned(o, "VirtualMachine", v.Native.UID) && (ref.UID == "" || string(o.GetUID()) == ref.UID) && o.GetAnnotations()["vm.ani.io/snapshot-uid"] == s.Native.UID && o.GetAnnotations()["vm.ani.io/source-vm-uid"] == v.Native.UID && snapshotString(o, "spec", "target", "kind") == "VirtualMachine" && snapshotString(o, "spec", "target", "apiGroup") == "kubevirt.io" && snapshotString(o, "spec", "target", "name") == v.Native.Name && snapshotString(o, "spec", "virtualMachineSnapshotName") == s.Native.Name && snapshotString(o, "spec", "targetReadinessPolicy") == "FailImmediate" && snapshotString(o, "spec", "volumeRestorePolicy") == "RandomizeNames" && snapshotString(o, "spec", "volumeOwnershipPolicy") == "Vm"
}
func (n *Native) EnsureRestore(ctx context.Context, v biz.VM, s biz.Snapshot, ref biz.ObjectRef) (biz.RestoreObservation, error) {
	result := biz.RestoreObservation{Native: ref}
	if err := n.namespace(ctx, v); err != nil {
		return result, err
	}
	old, err := n.client.Resource(restoreResource).Namespace(v.Spec.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if err == nil {
		return n.observeRestoreObject(ctx, v, s, ref, old, true)
	}
	if !apierrors.IsNotFound(err) {
		return result, err
	}
	if ref.UID != "" {
		return result, biz.ErrUnknown
	}
	root, err := n.snapshotRoot(ctx, v)
	if err != nil {
		return result, err
	}
	vmi, err := n.client.Resource(vmiResource).Namespace(v.Spec.Namespace).Get(ctx, v.Native.Name, metav1.GetOptions{})
	if err == nil && vmi != nil {
		return result, biz.ErrBusy
	}
	if !apierrors.IsNotFound(err) {
		return result, err
	}
	strategy := snapshotString(root, "spec", "runStrategy")
	if snapshotBool(root, "spec", "running") || (strategy != "" && strategy != "Halted" && strategy != "Manual") {
		return result, biz.ErrBusy
	}
	fresh, err := n.ObserveSnapshot(ctx, v, s)
	if err != nil {
		return result, err
	}
	if !biz.SnapshotComplete(fresh) || fresh.Native.UID != s.Native.UID {
		return result, biz.ErrChanged
	}
	labels := labelVM(v)
	labels["vm.ani.io/snapshot-id"] = s.ID
	object := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "snapshot.kubevirt.io/v1beta1", "kind": "VirtualMachineRestore", "metadata": map[string]any{"name": ref.Name, "namespace": v.Spec.Namespace}, "spec": map[string]any{"target": map[string]any{"apiGroup": "kubevirt.io", "kind": "VirtualMachine", "name": v.Native.Name}, "virtualMachineSnapshotName": s.Native.Name, "targetReadinessPolicy": "FailImmediate", "volumeRestorePolicy": "RandomizeNames", "volumeOwnershipPolicy": "Vm"}}}
	object.SetLabels(labels)
	object.SetOwnerReferences(ownReference(v))
	object.SetAnnotations(map[string]string{"vm.ani.io/snapshot-uid": s.Native.UID, "vm.ani.io/source-vm-uid": v.Native.UID})
	created, err := n.client.Resource(restoreResource).Namespace(v.Spec.Namespace).Create(ctx, object, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return n.EnsureRestore(ctx, v, s, ref)
	}
	if err != nil {
		return result, err
	}
	result.Native = objectRef(created)
	return result, nil
}

func (n *Native) ApplyRestoredConfiguration(ctx context.Context, v biz.VM, s biz.Snapshot) (bool, error) {
	root, err := n.snapshotRoot(ctx, v)
	if err != nil {
		return false, err
	}
	_, err = n.client.Resource(vmiResource).Namespace(v.Spec.Namespace).Get(ctx, v.Native.Name, metav1.GetOptions{})
	if err == nil {
		return false, biz.ErrBusy
	}
	if !apierrors.IsNotFound(err) {
		return false, err
	}
	volumes, _, _ := unstructured.NestedSlice(root.Object, "spec", "template", "spec", "volumes")
	devices, _, _ := unstructured.NestedSlice(root.Object, "spec", "template", "spec", "domain", "devices", "disks")
	newVolumes := plainSlice(volumes)
	newDevices := plainSlice(devices)
	for _, disk := range v.Disks {
		if disk.Tombstone || !disk.Attached {
			continue
		}
		pvc, err := n.client.Resource(pvcResource).Namespace(v.Spec.Namespace).Get(ctx, disk.PVC.Name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if disk.PVC.UID == "" || string(pvc.GetUID()) != disk.PVC.UID || !owned(pvc, "VirtualMachine", v.Native.UID) || snapshotString(pvc, "spec", "volumeMode") != "Block" {
			return false, biz.ErrOwnership
		}
		_, volume, err := namedItems(newVolumes, disk.VolumeName)
		if err != nil {
			return false, err
		}
		if volume == nil {
			return false, biz.ErrChanged
		}
		claim, _, _ := unstructured.NestedString(volume, "persistentVolumeClaim", "claimName")
		if claim != disk.PVC.Name || disk.DV.Name != "" {
			return false, biz.ErrOwnership
		}
		if disk.Role == "data" {
			if err = unstructured.SetNestedField(volume, true, "persistentVolumeClaim", "hotpluggable"); err != nil {
				return false, err
			}
			_, device, err := namedItems(newDevices, disk.VolumeName)
			if err != nil {
				return false, err
			}
			if device == nil {
				return false, biz.ErrChanged
			}
			device["disk"] = map[string]any{"bus": "scsi"}
			device["serial"] = disk.Serial
			delete(device, "lun")
			delete(device, "cdrom")
		}
	}
	for _, copy := range s.MediaCopies {
		current, ok := biz.FindMediaCopy(v, copy.ID)
		if !ok {
			return false, biz.ErrOwnership
		}
		pvc, err := n.client.Resource(pvcResource).Namespace(v.Spec.Namespace).Get(ctx, current.PVC.Name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if current.PVC.UID == "" || string(pvc.GetUID()) != current.PVC.UID || !owned(pvc, "VirtualMachine", v.Native.UID) {
			return false, biz.ErrOwnership
		}
	}
	domain, _, _ := unstructured.NestedMap(root.Object, "spec", "template", "spec", "domain")
	newVolumes, newDevices, err = n.MediaTemplate(v, domain, newVolumes, newDevices)
	if err != nil {
		return false, err
	}
	beforeVolumes, _ := json.Marshal(volumes)
	afterVolumes, _ := json.Marshal(newVolumes)
	beforeDevices, _ := json.Marshal(devices)
	afterDevices, _ := json.Marshal(newDevices)
	if string(beforeVolumes) == string(afterVolumes) && string(beforeDevices) == string(afterDevices) {
		return true, nil
	}
	patch := []map[string]any{{"op": "test", "path": "/metadata/uid", "value": v.Native.UID}, {"op": "test", "path": "/metadata/resourceVersion", "value": root.GetResourceVersion()}, {"op": "add", "path": "/spec/template/spec/volumes", "value": newVolumes}, {"op": "add", "path": "/spec/template/spec/domain/devices/disks", "value": newDevices}}
	body, _ := json.Marshal(patch)
	_, err = n.client.Resource(vmResource).Namespace(v.Spec.Namespace).Patch(ctx, v.Native.Name, types.JSONPatchType, body, metav1.PatchOptions{})
	return false, err
}

var _ biz.SnapshotNativePort = (*Native)(nil)

// ObserveRetiredVolume does not resolve a catalog or require a current root VM.
// Exact business PVC/DV lifecycle is the boundary; legacy PV refs are diagnostic.
func (n *Native) ObserveRetiredVolume(ctx context.Context, v biz.VM, d biz.Disk) (biz.Disk, error) {
	if err := n.namespace(ctx, v); err != nil {
		return d, err
	}
	if d.PVC.Kind != "PersistentVolumeClaim" || d.PVC.Namespace != v.Spec.Namespace || d.PVC.Name == "" {
		return d, biz.ErrOwnership
	}
	pvc, err := n.client.Resource(pvcResource).Namespace(d.PVC.Namespace).Get(ctx, d.PVC.Name, metav1.GetOptions{})
	if err == nil {
		if (d.PVC.UID != "" && objectRef(pvc) != d.PVC) || (!owned(pvc, "VirtualMachine", v.Native.UID) && !owned(pvc, "DataVolume", d.DV.UID)) {
			return d, biz.ErrOwnership
		}
		d.PVC = objectRef(pvc)
	} else if !apierrors.IsNotFound(err) {
		return d, err
	}
	return d, nil
}

var _ biz.RetiredVolumeNativePort = (*Native)(nil)

// Closing only reads this Restore's public named outputs. Losing the result
// carrier before durable handoff is unknown, never proof of zero output.
func (n *Native) ObserveRestore(ctx context.Context, v biz.VM, s biz.Snapshot, ref biz.ObjectRef) (biz.RestoreObservation, error) {
	result := biz.RestoreObservation{Native: ref}
	if err := n.namespace(ctx, v); err != nil {
		return result, err
	}
	if ref.Kind != "VirtualMachineRestore" || ref.Namespace != v.Spec.Namespace || ref.Name == "" || s.SourceVMID != v.ID || s.SourceVMUID != v.Native.UID {
		return result, biz.ErrOwnership
	}
	object, err := n.client.Resource(restoreResource).Namespace(v.Spec.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		result.Absent = true
		for _, saved := range s.RestoreOutputsRecorded {
			if saved == ref && ref.UID != "" {
				result.OutputsKnown = true
				return result, nil
			}
		}
		return result, biz.ErrUnknown
	}
	if err != nil {
		return result, err
	}
	return n.observeRestoreObject(ctx, v, s, ref, object, false)
}

func (n *Native) observeRestoreObject(ctx context.Context, v biz.VM, s biz.Snapshot, ref biz.ObjectRef, object *unstructured.Unstructured, requireCurrent bool) (biz.RestoreObservation, error) {
	result := biz.RestoreObservation{Native: ref}
	if object == nil {
		return result, biz.ErrUnknown
	}
	if !validRestoreObject(object, v, s, ref) {
		return result, biz.ErrOwnership
	}
	result.Native = objectRef(object)
	result.Complete = snapshotBool(object, "status", "complete")
	conditions, _, _ := unstructured.NestedSlice(object.Object, "status", "conditions")
	for _, value := range conditions {
		if condition, ok := value.(map[string]any); ok && condition["type"] == "Failure" && condition["status"] == "True" {
			result.Failed = true
			result.Reason = "VM_RESTORE_NATIVE_FAILED"
			if message, _ := condition["message"].(string); message != "" {
				result.Reason += ": " + message
			}
		}
	}
	var volumes []any
	if requireCurrent && result.Complete {
		root, err := n.snapshotRoot(ctx, v)
		if err != nil {
			return result, err
		}
		volumes, _, _ = unstructured.NestedSlice(root.Object, "spec", "template", "spec", "volumes")
	}
	requiredByName := map[string]biz.SnapshotVolume{}
	for _, required := range s.Required {
		requiredByName[required.Name] = required
	}
	restores, _, _ := unstructured.NestedSlice(object.Object, "status", "restores")
	seen, claims := map[string]bool{}, map[string]bool{}
	for _, value := range restores {
		entry, ok := value.(map[string]any)
		if !ok {
			return result, biz.ErrChanged
		}
		volume, _ := entry["volumeName"].(string)
		name, _ := entry["persistentVolumeClaim"].(string)
		required, ok := requiredByName[volume]
		if !ok || name == "" {
			return result, biz.ErrOwnership
		}
		if seen[volume] || claims[name] {
			return result, biz.ErrChanged
		}
		seen[volume], claims[name] = true, true
		pvcRef := biz.ObjectRef{Kind: "PersistentVolumeClaim", Namespace: v.Spec.Namespace, Name: name}
		pvc, err := n.client.Resource(pvcResource).Namespace(v.Spec.Namespace).Get(ctx, name, metav1.GetOptions{})
		present := err == nil
		if err != nil && !apierrors.IsNotFound(err) {
			return result, err
		}
		mode, phase := "", ""
		var requested, capacity int64
		if present {
			if pvc.GetUID() == "" || !owned(pvc, "VirtualMachine", v.Native.UID) || pvc.GetDeletionTimestamp() != nil {
				return result, biz.ErrOwnership
			}
			pvcRef = objectRef(pvc)
			mode = snapshotString(pvc, "spec", "volumeMode")
			if mode == "" {
				mode = "Filesystem"
			}
			phase = snapshotString(pvc, "status", "phase")
			q, err := resource.ParseQuantity(snapshotString(pvc, "spec", "resources", "requests", "storage"))
			if err != nil || q.Sign() <= 0 {
				return result, biz.ErrChanged
			}
			requested = q.Value()
			if size := snapshotString(pvc, "status", "capacity", "storage"); size != "" {
				q, err = resource.ParseQuantity(size)
				if err != nil || q.Sign() <= 0 {
					return result, biz.ErrChanged
				}
				capacity = q.Value()
			}
		}
		if requireCurrent && result.Complete {
			if !present {
				return result, biz.ErrDependency
			}
			if required.Role != "backend" {
				_, actual, err := namedItems(volumes, required.Name)
				if err != nil {
					return result, err
				}
				claim, _, _ := unstructured.NestedString(actual, "persistentVolumeClaim", "claimName")
				if claim != name {
					return result, biz.ErrOwnership
				}
			}
		}
		if required.Role == "backend" {
			state := biz.BackendState{PVC: pvcRef, Owner: v.Native, VolumeName: required.Name}
			if present {
				state, err = n.observeBackendPVC(ctx, v, pvc, state)
				if err != nil {
					return result, err
				}
				state.Current = requireCurrent && result.Complete
			}
			result.BackendStates = append(result.BackendStates, state)
			continue
		}
		if required.Role == "media" {
			found := false
			for _, copy := range s.MediaCopies {
				if copy.ID != required.MediaCopyID {
					continue
				}
				if present && copy.Source.VolumeMode != mode {
					return result, biz.ErrOwnership
				}
				copy.DV, copy.PVC, copy.Cleanup = biz.ObjectRef{}, pvcRef, nil
				copy.Ready, copy.Tombstone = phase == "Bound", false
				result.MediaCopies = append(result.MediaCopies, copy)
				found = true
			}
			if !found {
				return result, biz.ErrOwnership
			}
		} else {
			if present && mode != "Block" {
				return result, biz.ErrOwnership
			}
			found := false
			for _, disk := range s.Inventory {
				if disk.ID != required.DiskID {
					continue
				}
				if present && (requested < disk.RequestedBytes || (capacity > 0 && capacity < requested)) {
					return result, biz.ErrChanged
				}
				disk.DV, disk.PVC, disk.Cleanup = biz.ObjectRef{}, pvcRef, nil
				disk.Preparation, disk.Reason, disk.Attached, disk.Tombstone = phase, "", true, false
				if present {
					disk.RequestedBytes, disk.ObservedCapacityBytes = requested, capacity
				}
				result.Disks = append(result.Disks, disk)
				found = true
			}
			if !found {
				return result, biz.ErrOwnership
			}
		}
	}
	result.OutputsKnown = len(seen) == len(requiredByName) && len(requiredByName) > 0
	if requireCurrent && result.Complete && !result.OutputsKnown {
		return result, biz.ErrDependency
	}
	return result, nil
}

var _ biz.RestoreObserver = (*Native)(nil)
