package vm

import (
	"context"
	"encoding/json"
	"reflect"
	"strconv"

	"github.com/google/uuid"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func (p *Postgres) AcceptMedia(ctx context.Context, c biz.Command, mutation biz.MediaMutation) (biz.Operation, error) {
	if c.Action != "set-media" && c.Action != "eject-media" && c.Action != "set-boot" {
		return biz.Operation{}, biz.ErrInvalid
	}
	if !biz.UUID(c.TenantID) || !biz.UUID(c.VMID) || c.Key == "" || len(c.Key) > 256 || c.ExpectedVersion < 1 || len(c.RequestDigest) != 64 || !json.Valid(c.Intent) {
		return biz.Operation{}, biz.ErrInvalid
	}
	// Keep the public intent digest unchanged: read-only replay has no need to
	// consult a newer catalog or repeat source validation.
	mutation.UserIntent = append(json.RawMessage(nil), c.Intent...)
	raw, e := json.Marshal(mutation)
	if e != nil {
		return biz.Operation{}, e
	}
	c.Intent = raw
	return p.accept(ctx, c, uuid.NewString(), "")
}

func mediaVolumeName(slot string) string {
	if slot == "INSTALL" {
		return "media-install"
	}
	if slot == "DRIVER" {
		return "media-driver"
	}
	return ""
}
func managedMediaName(name string) bool { return name == "media-install" || name == "media-driver" }
func plainSlice(value []any) []any {
	result := make([]any, 0, len(value))
	for _, item := range value {
		result = append(result, deepCopyMediaValue(item))
	}
	return result
}
func deepCopyMediaValue(value any) any {
	switch item := value.(type) {
	case map[string]any:
		copy := map[string]any{}
		for key, value := range item {
			copy[key] = deepCopyMediaValue(value)
		}
		return copy
	case []any:
		copy := make([]any, len(item))
		for i, value := range item {
			copy[i] = deepCopyMediaValue(value)
		}
		return copy
	default:
		return value
	}
}

// MediaTemplate owns only the two bounded optical names and root boot order.
// Existing data disks and cloud-init devices are passed through unchanged.
func (n *Native) MediaTemplate(v biz.VM, domain map[string]any, volumes, disks []any) ([]any, []any, error) {
	if e := biz.InitializeMedia(&v); e != nil {
		return nil, nil, e
	}
	newVolumes := []any{}
	newDisks := []any{}
	for _, value := range plainSlice(volumes) {
		item, ok := value.(map[string]any)
		if !ok {
			return nil, nil, biz.ErrChanged
		}
		name, _ := item["name"].(string)
		if !managedMediaName(name) {
			newVolumes = append(newVolumes, item)
		}
	}
	rootName := ""
	for _, disk := range v.Disks {
		if disk.Role == "system" && !disk.Tombstone {
			rootName = disk.VolumeName
		}
	}
	for _, value := range plainSlice(disks) {
		item, ok := value.(map[string]any)
		if !ok {
			return nil, nil, biz.ErrChanged
		}
		name, _ := item["name"].(string)
		if managedMediaName(name) {
			continue
		}
		if name == rootName {
			if v.CurrentBootDevice == "SYSTEM_DISK" {
				item["bootOrder"] = int64(1)
			} else {
				item["bootOrder"] = int64(2)
			}
		}
		newDisks = append(newDisks, item)
	}
	for _, slot := range v.MediaSlots {
		if slot.DesiredCopyID == "" {
			continue
		}
		copy, ok := biz.FindMediaCopy(v, slot.DesiredCopyID)
		if !ok {
			return nil, nil, biz.ErrChanged
		}
		name := mediaVolumeName(slot.Slot)
		if name == "" || copy.PVC.Namespace != v.Spec.Namespace || copy.PVC.Name == "" {
			return nil, nil, biz.ErrOwnership
		}
		// KubeVirt initializes ownership inside its private Filesystem copy.
		// A read-only host mount fails that initialization before the domain
		// starts; the separate CD-ROM readonly flag protects the guest view.
		claim := map[string]any{"claimName": copy.PVC.Name}
		if copy.Source.VolumeMode != "Filesystem" {
			claim["readOnly"] = true
		}
		newVolumes = append(newVolumes, map[string]any{"name": name, "persistentVolumeClaim": claim})
		device := map[string]any{"name": name, "cdrom": map[string]any{"bus": "sata", "readonly": true, "tray": "closed"}}
		if slot.Slot == "INSTALL" {
			if v.CurrentBootDevice == "INSTALL_MEDIA" {
				device["bootOrder"] = int64(1)
			} else {
				device["bootOrder"] = int64(2)
			}
		}
		newDisks = append(newDisks, device)
	}
	_ = domain
	return newVolumes, newDisks, nil
}

func (n *Native) mediaVM(ctx context.Context, v biz.VM) (*unstructured.Unstructured, error) {
	if e := n.namespace(ctx, v); e != nil {
		return nil, e
	}
	object, e := n.client.Resource(vmResource).Namespace(v.Spec.Namespace).Get(ctx, v.Native.Name, metav1.GetOptions{})
	if e != nil {
		return nil, e
	}
	if v.Native.UID == "" || string(object.GetUID()) != v.Native.UID || !matches(object, v) {
		return nil, biz.ErrOwnership
	}
	return object, nil
}
func validMediaDV(object *unstructured.Unstructured, v biz.VM, copy biz.MediaCopy) bool {
	if !matches(object, v) || !owned(object, "VirtualMachine", v.Native.UID) || object.GetLabels()["vm.ani.io/media-id"] != copy.ID || object.GetAnnotations()["vm.ani.io/media-source-digest"] != copy.SourceDigest || (copy.DV.UID != "" && copy.DV.UID != string(object.GetUID())) {
		return false
	}
	namespace, _, _ := unstructured.NestedString(object.Object, "spec", "source", "pvc", "namespace")
	name, _, _ := unstructured.NestedString(object.Object, "spec", "source", "pvc", "name")
	mode, _, _ := unstructured.NestedString(object.Object, "spec", "storage", "volumeMode")
	return namespace == copy.Source.Namespace && name == copy.Source.PVCName && mode == copy.Source.VolumeMode
}
func (n *Native) validateFrozenMediaSource(ctx context.Context, source biz.Source) error {
	object, e := n.client.Resource(pvcResource).Namespace(source.Namespace).Get(ctx, source.PVCName, metav1.GetOptions{})
	if apierrors.IsNotFound(e) {
		return biz.ErrChanged
	}
	if e != nil {
		return e
	}
	mode, _, _ := unstructured.NestedString(object.Object, "spec", "volumeMode")
	if mode == "" {
		mode = "Filesystem"
	}
	contentType := object.GetAnnotations()["cdi.kubevirt.io/storage.contentType"]
	if contentType == "" {
		contentType = "kubevirt"
	}
	if string(object.GetUID()) != source.PVCUID || mode != source.VolumeMode || object.GetAnnotations()["vm.ani.io/source-sha256"] != source.Checksum || contentType != source.ContentType {
		return biz.ErrChanged
	}
	phase, _, _ := unstructured.NestedString(object.Object, "status", "phase")
	if phase != "Bound" {
		return biz.ErrDependency
	}
	return nil
}
func (n *Native) observeMediaCopy(ctx context.Context, v biz.VM, copy biz.MediaCopy) (biz.MediaCopy, error) {
	restored := copy.DV.Name == "" && copy.DV.UID == "" && copy.PVC.UID != ""
	if (!restored && copy.DV.Namespace != v.Spec.Namespace) || copy.PVC.Namespace != v.Spec.Namespace || copy.SourceDigest != biz.MediaSourceDigest(copy.Source) {
		return copy, biz.ErrOwnership
	}
	var e error
	if !restored {
		dv, readErr := n.client.Resource(dvResource).Namespace(v.Spec.Namespace).Get(ctx, copy.DV.Name, metav1.GetOptions{})
		e = readErr
		if e != nil && !apierrors.IsNotFound(e) {
			return copy, e
		}
		if e == nil {
			if !validMediaDV(dv, v, copy) {
				return copy, biz.ErrOwnership
			}
			copy.DV = objectRef(dv)
			phase, _, _ := unstructured.NestedString(dv.Object, "status", "phase")
			copy.Ready = phase == "Succeeded"
		}
		referenced := false
		for _, slot := range v.MediaSlots {
			if slot.DesiredCopyID == copy.ID {
				referenced = true
			}
		}
		if referenced && !copy.Ready && !v.Closing {
			if e = n.validateFrozenMediaSource(ctx, copy.Source); e != nil {
				return copy, e
			}
		}
	}
	pvc, e := n.client.Resource(pvcResource).Namespace(v.Spec.Namespace).Get(ctx, copy.PVC.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(e) {
		if restored && !v.Closing {
			return copy, biz.ErrOwnership
		}
		return copy, nil
	}
	if e != nil {
		return copy, e
	}
	ownerMatches := owned(pvc, "DataVolume", copy.DV.UID)
	if restored {
		ownerMatches = owned(pvc, "VirtualMachine", v.Native.UID)
	}
	if !ownerMatches || (copy.PVC.UID != "" && copy.PVC.UID != string(pvc.GetUID())) {
		return copy, biz.ErrOwnership
	}
	mode, _, _ := unstructured.NestedString(pvc.Object, "spec", "volumeMode")
	if mode == "" {
		mode = "Filesystem"
	}
	if mode != copy.Source.VolumeMode {
		return copy, biz.ErrOwnership
	}
	copy.PVC = objectRef(pvc)
	if restored {
		phase, _, _ := unstructured.NestedString(pvc.Object, "status", "phase")
		copy.Ready = phase == "Bound"
	}
	return copy, nil
}
func (n *Native) EnsureMediaCopy(ctx context.Context, v biz.VM, copy biz.MediaCopy) (biz.MediaCopy, error) {
	if _, e := n.mediaVM(ctx, v); e != nil {
		return copy, e
	}
	if copy.SourceDigest != biz.MediaSourceDigest(copy.Source) {
		return copy, biz.ErrInvalid
	}
	// A trusted native restore registers a new PVC UID and removes its old DV
	// reference. Observe that owned identity; never recreate the original clone.
	if copy.DV.Name == "" && copy.DV.UID == "" && copy.PVC.UID != "" {
		return n.observeMediaCopy(ctx, v, copy)
	}
	if v.Spec.MediaStorageClass == "" {
		return copy, biz.ErrInvalid
	}
	old, e := n.client.Resource(dvResource).Namespace(v.Spec.Namespace).Get(ctx, copy.DV.Name, metav1.GetOptions{})
	if e == nil {
		if !validMediaDV(old, v, copy) {
			return copy, biz.ErrOwnership
		}
		return n.observeMediaCopy(ctx, v, copy)
	}
	if !apierrors.IsNotFound(e) {
		return copy, e
	}
	if copy.DV.UID != "" {
		return copy, biz.ErrOwnership
	}
	if e = n.validateFrozenMediaSource(ctx, copy.Source); e != nil {
		return copy, e
	}
	bytes := copy.Source.RequestedBytes
	if copy.Source.VirtualBytes > bytes {
		bytes = copy.Source.VirtualBytes
	}
	if bytes < 1 {
		return copy, biz.ErrInvalid
	}
	object := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "cdi.kubevirt.io/v1beta1", "kind": "DataVolume", "metadata": map[string]any{"name": copy.DV.Name, "namespace": v.Spec.Namespace}, "spec": map[string]any{"source": map[string]any{"pvc": map[string]any{"namespace": copy.Source.Namespace, "name": copy.Source.PVCName}}, "contentType": copy.Source.ContentType, "storage": map[string]any{"storageClassName": v.Spec.MediaStorageClass, "volumeMode": copy.Source.VolumeMode, "accessModes": []any{"ReadWriteOnce"}, "resources": map[string]any{"requests": map[string]any{"storage": strconv.FormatInt(bytes, 10)}}}}}}
	labels := labelVM(v)
	labels["vm.ani.io/media-id"] = copy.ID
	object.SetLabels(labels)
	object.SetAnnotations(map[string]string{"vm.ani.io/media-source-digest": copy.SourceDigest})
	object.SetOwnerReferences(ownReference(v))
	created, e := n.client.Resource(dvResource).Namespace(v.Spec.Namespace).Create(ctx, object, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(e) {
		return n.EnsureMediaCopy(ctx, v, copy)
	}
	if e != nil {
		return copy, e
	}
	copy.DV = objectRef(created)
	return n.observeMediaCopy(ctx, v, copy)
}

func mediaLists(object *unstructured.Unstructured, prefix ...string) ([]any, []any, error) {
	volumePath := append(append([]string{}, prefix...), "volumes")
	diskPath := append(append([]string{}, prefix...), "domain", "devices", "disks")
	volumes, _, e := unstructured.NestedSlice(object.Object, volumePath...)
	if e != nil {
		return nil, nil, e
	}
	disks, _, e := unstructured.NestedSlice(object.Object, diskPath...)
	return volumes, disks, e
}

// Kubernetes' PVC source omits readOnly=false during typed serialization. Treat
// that default as equivalent only for our bounded optical volume names.
func mediaVolumesEqual(actual, desired []any) bool {
	canonical := func(volumes []any) []any {
		result := plainSlice(volumes)
		for _, value := range result {
			item, ok := value.(map[string]any)
			if !ok {
				continue
			}
			name, _ := item["name"].(string)
			if !managedMediaName(name) {
				continue
			}
			if pvc, ok := item["persistentVolumeClaim"].(map[string]any); ok && pvc["readOnly"] == false {
				delete(pvc, "readOnly")
			}
		}
		return result
	}
	return reflect.DeepEqual(canonical(actual), canonical(desired))
}

func runtimeMediaCopy(v biz.VM, slot string, volumes, disks []any) (string, error) {
	name := mediaVolumeName(slot)
	claim := ""
	hostReadOnly := false
	volumeFound := false
	diskFound := false
	for _, value := range volumes {
		item, ok := value.(map[string]any)
		if !ok {
			return "", biz.ErrChanged
		}
		if item["name"] != name {
			continue
		}
		volumeFound = true
		if pvc, ok := item["persistentVolumeClaim"].(map[string]any); ok {
			claim, _ = pvc["claimName"].(string)
			if value, present := pvc["readOnly"]; present {
				var valid bool
				hostReadOnly, valid = value.(bool)
				if !valid {
					return "", biz.ErrOwnership
				}
			}
		} else {
			return "", biz.ErrOwnership
		}
	}
	for _, value := range disks {
		item, ok := value.(map[string]any)
		if !ok {
			return "", biz.ErrChanged
		}
		if item["name"] != name {
			continue
		}
		diskFound = true
		cdrom, ok := item["cdrom"].(map[string]any)
		if !ok || cdrom["bus"] != "sata" || cdrom["readonly"] != true {
			return "", biz.ErrOwnership
		}
	}
	if !volumeFound && !diskFound {
		return "", nil
	}
	if !volumeFound || !diskFound {
		return "", biz.ErrOwnership
	}
	for _, copy := range v.MediaCopies {
		if !copy.Tombstone && copy.PVC.Name == claim {
			// Existing read-only Filesystem templates remain observable for
			// normal cleanup. Block media retains its read-only host contract.
			if copy.Source.VolumeMode != "Filesystem" && !hostReadOnly {
				return "", biz.ErrOwnership
			}
			return copy.ID, nil
		}
	}
	return "", biz.ErrOwnership
}
func nativeBootMatches(v biz.VM, actual, desired []any) bool {
	orders := func(disks []any) map[string]any {
		result := map[string]any{}
		for _, value := range disks {
			item, ok := value.(map[string]any)
			if !ok {
				continue
			}
			name, _ := item["name"].(string)
			if name == "media-install" {
				result[name] = item["bootOrder"]
			}
			for _, disk := range v.Disks {
				if disk.Role == "system" && disk.VolumeName == name {
					result[name] = item["bootOrder"]
				}
			}
		}
		return result
	}
	return reflect.DeepEqual(orders(actual), orders(desired))
}
func (n *Native) ObserveMedia(ctx context.Context, v biz.VM) (biz.MediaObservation, error) {
	result := biz.MediaObservation{}
	if e := biz.InitializeMedia(&v); e != nil {
		return result, e
	}
	object, e := n.mediaVM(ctx, v)
	if e != nil && !(v.Closing && apierrors.IsNotFound(e)) {
		return result, e
	}
	for i, copy := range v.MediaCopies {
		if copy.Tombstone {
			continue
		}
		observed, e := n.observeMediaCopy(ctx, v, copy)
		if e != nil {
			return result, e
		}
		v.MediaCopies[i] = observed
	}
	vmi, e := n.client.Resource(vmiResource).Namespace(v.Spec.Namespace).Get(ctx, v.Native.Name, metav1.GetOptions{})
	if e != nil && !apierrors.IsNotFound(e) {
		return result, e
	}
	return n.observeMediaRuntime(v, object, vmi)
}

// observeMediaRuntime projects only the VM/VMI already read for lifecycle.
// It never resolves a source or reads DV/PVC/storage inventory.
func (n *Native) observeMediaRuntime(v biz.VM, object, vmi *unstructured.Unstructured) (biz.MediaObservation, error) {
	result := biz.MediaObservation{}
	if err := biz.InitializeMedia(&v); err != nil {
		return result, err
	}
	result.Copies = v.MediaCopies
	var desiredDisks []any
	if object != nil {
		volumes, disks, e := mediaLists(object, "spec", "template", "spec")
		if e != nil {
			return result, e
		}
		desiredVolumes, wantedDisks, e := n.MediaTemplate(v, nil, volumes, disks)
		if e != nil {
			return result, e
		}
		desiredDisks = wantedDisks
		result.TemplateMatches = mediaVolumesEqual(volumes, desiredVolumes) && reflect.DeepEqual(disks, wantedDisks)
	} else {
		result.TemplateMatches = v.Closing
	}
	var runtimeVolumes, runtimeDisks []any
	if vmi != nil {
		if !owned(vmi, "VirtualMachine", v.Native.UID) {
			return result, biz.ErrOwnership
		}
		result.HasVMI = true
		var err error
		runtimeVolumes, runtimeDisks, err = mediaLists(vmi, "spec")
		if err != nil {
			return result, err
		}
	}
	bootMatches := !result.HasVMI || nativeBootMatches(v, runtimeDisks, desiredDisks)
	for _, slot := range v.MediaSlots {
		observed, e := runtimeMediaCopy(v, slot.Slot, runtimeVolumes, runtimeDisks)
		if e != nil {
			return result, e
		}
		slot.ObservedCopyID = observed
		slot.EffectiveNow = slot.DesiredCopyID == observed && bootMatches
		slot.PendingRestart = result.HasVMI && !slot.EffectiveNow
		result.Slots = append(result.Slots, slot)
	}
	if bootMatches && result.HasVMI {
		result.BootDevice = v.CurrentBootDevice
	}
	return result, nil
}
func (n *Native) ApplyMedia(ctx context.Context, v biz.VM) (biz.MediaObservation, error) {
	object, e := n.mediaVM(ctx, v)
	if e != nil {
		return biz.MediaObservation{}, e
	}
	volumes, disks, e := mediaLists(object, "spec", "template", "spec")
	if e != nil {
		return biz.MediaObservation{}, e
	}
	newVolumes, newDisks, e := n.MediaTemplate(v, nil, volumes, disks)
	if e != nil {
		return biz.MediaObservation{}, e
	}
	if !mediaVolumesEqual(volumes, newVolumes) || !reflect.DeepEqual(disks, newDisks) {
		// Update is UID/resourceVersion fenced and keeps every other native
		// field. No VMI write, power request, source write, or automatic restart.
		if e = unstructured.SetNestedSlice(object.Object, newVolumes, "spec", "template", "spec", "volumes"); e != nil {
			return biz.MediaObservation{}, e
		}
		if e = unstructured.SetNestedSlice(object.Object, newDisks, "spec", "template", "spec", "domain", "devices", "disks"); e != nil {
			return biz.MediaObservation{}, e
		}
		if _, e = n.client.Resource(vmResource).Namespace(v.Spec.Namespace).Update(ctx, object, metav1.UpdateOptions{}); e != nil {
			return biz.MediaObservation{}, e
		}
	}
	return n.ObserveMedia(ctx, v)
}
func (n *Native) DeleteMediaCopy(ctx context.Context, v biz.VM, copy biz.MediaCopy) (biz.MediaCopy, bool, error) {
	if !v.Closing {
		return copy, false, biz.ErrBusy
	}
	if e := n.namespace(ctx, v); e != nil {
		return copy, false, e
	}
	observed, e := n.observeMediaCopy(ctx, v, copy)
	if e != nil {
		return copy, false, e
	}
	done, e := n.DeleteVolume(ctx, v, biz.Disk{DV: observed.DV, PVC: observed.PVC, Cleanup: observed.Cleanup})
	return observed, done, e
}

var _ biz.MediaRepository = (*Postgres)(nil)
var _ biz.MediaNativePort = (*Native)(nil)
