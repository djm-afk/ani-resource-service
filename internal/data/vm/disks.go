package vm

import (
	"context"
	"encoding/json"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

var kubevirtResource = schema.GroupVersionResource{Group: "kubevirt.io", Version: "v1", Resource: "kubevirts"}

func (n *Native) ValidateDiskAttachment(ctx context.Context, v biz.VM, d biz.Disk, attached bool) (biz.Observation, error) {
	if d.Role != "data" || d.Tombstone {
		return biz.Observation{}, biz.ErrInvalid
	}
	x, e := n.Observe(ctx, v)
	if e != nil {
		return x, e
	}
	if x.VM.UID != v.Native.UID || v.Native.UID == "" {
		return x, biz.ErrOwnership
	}
	if x.VMI == nil && x.Stopped {
		return x, nil
	}
	if !x.Running {
		return x, biz.ErrDependency
	}
	// This service only implements the native declarative path. It must never
	// silently submit a template-only change as a successful live disk change.
	enabled, e := n.declarativeHotplugEnabled(ctx)
	if e != nil {
		return x, e
	}
	if !enabled {
		return x, biz.ErrDependency
	}
	return x, nil
}

func (n *Native) declarativeHotplugEnabled(ctx context.Context) (bool, error) {
	list, e := n.client.Resource(kubevirtResource).List(ctx, metav1.ListOptions{})
	if e != nil {
		return false, e
	}
	if len(list.Items) != 1 {
		return false, biz.ErrDependency
	}
	gates, _, _ := unstructured.NestedStringSlice(list.Items[0].Object, "spec", "configuration", "developerConfiguration", "featureGates")
	disabled, _, _ := unstructured.NestedStringSlice(list.Items[0].Object, "spec", "configuration", "developerConfiguration", "disabledFeatureGates")
	explicit := false
	for _, gate := range gates {
		if gate == "HotplugVolumes" {
			return false, nil
		}
		if gate == "DeclarativeHotplugVolumes" {
			explicit = true
		}
	}
	if explicit {
		return true, nil
	}
	for _, gate := range disabled {
		if gate == "DeclarativeHotplugVolumes" {
			return false, nil
		}
	}
	version, _, _ := unstructured.NestedString(list.Items[0].Object, "status", "observedKubeVirtVersion")
	// KubeVirt v1.9.0 registers DeclarativeHotplugVolumes as Beta, enabled
	// by default. An empty featureGates list must not disable native support.
	// Unknown versions require an explicit gate rather than guessed defaults.
	return version == "v1.9.0", nil
}

func diskVolume(d biz.Disk) map[string]any {
	if d.DV.Name != "" {
		return map[string]any{"name": d.VolumeName, "dataVolume": map[string]any{"name": d.DV.Name, "hotpluggable": true}}
	}
	return map[string]any{"name": d.VolumeName, "persistentVolumeClaim": map[string]any{"claimName": d.PVC.Name, "hotpluggable": true}}
}
func diskDevice(d biz.Disk) map[string]any {
	return map[string]any{"name": d.VolumeName, "disk": map[string]any{"bus": "scsi"}, "serial": d.Serial}
}
func validDiskDevice(device map[string]any, d biz.Disk) bool {
	if device == nil || device["name"] != d.VolumeName || device["serial"] != d.Serial {
		return false
	}
	bus, _, _ := unstructured.NestedString(device, "disk", "bus")
	return bus == "scsi" && device["cdrom"] == nil && device["lun"] == nil
}
func validDiskVolume(volume map[string]any, d biz.Disk) bool {
	if volume == nil || volume["name"] != d.VolumeName {
		return false
	}
	dv, _, _ := unstructured.NestedString(volume, "dataVolume", "name")
	pvc, _, _ := unstructured.NestedString(volume, "persistentVolumeClaim", "claimName")
	if dv != "" {
		hot, _, _ := unstructured.NestedBool(volume, "dataVolume", "hotpluggable")
		return d.DV.Name != "" && dv == d.DV.Name && pvc == "" && hot
	}
	hot, _, _ := unstructured.NestedBool(volume, "persistentVolumeClaim", "hotpluggable")
	readOnly, _, _ := unstructured.NestedBool(volume, "persistentVolumeClaim", "readOnly")
	return d.PVC.Name != "" && pvc == d.PVC.Name && hot && !readOnly
}
func namedItems(items []any, name string) ([]any, map[string]any, error) {
	keep := []any{}
	var found map[string]any
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, nil, biz.ErrChanged
		}
		if m["name"] == name {
			if found != nil {
				return nil, nil, biz.ErrChanged
			}
			found = m
		} else {
			keep = append(keep, item)
		}
	}
	return keep, found, nil
}
func (n *Native) EnsureDiskAttachment(ctx context.Context, v biz.VM, d biz.Disk, attached bool) (bool, error) {
	if _, e := n.ValidateDiskAttachment(ctx, v, d, attached); e != nil {
		return false, e
	}
	observed, e := n.observeDisk(ctx, v, d)
	if e != nil {
		return false, e
	}
	if observed.DV.UID == "" && observed.PVC.UID == "" {
		return false, biz.ErrDependency
	}
	o, e := n.client.Resource(vmResource).Namespace(v.Spec.Namespace).Get(ctx, v.Native.Name, metav1.GetOptions{})
	if e != nil {
		return false, e
	}
	if !matches(o, v) || string(o.GetUID()) != v.Native.UID {
		return false, biz.ErrOwnership
	}
	volumes, _, _ := unstructured.NestedSlice(o.Object, "spec", "template", "spec", "volumes")
	disks, _, _ := unstructured.NestedSlice(o.Object, "spec", "template", "spec", "domain", "devices", "disks")
	keepVolumes, oldVolume, e := namedItems(volumes, d.VolumeName)
	if e != nil {
		return false, e
	}
	keepDisks, oldDisk, e := namedItems(disks, d.VolumeName)
	if e != nil {
		return false, e
	}
	if oldVolume != nil && !validDiskVolume(oldVolume, d) {
		return false, biz.ErrOwnership
	}
	if oldDisk != nil && !validDiskDevice(oldDisk, d) {
		return false, biz.ErrOwnership
	}
	changed := (attached && (oldVolume == nil || oldDisk == nil)) || (!attached && (oldVolume != nil || oldDisk != nil))
	if changed {
		if attached {
			keepVolumes = append(keepVolumes, diskVolume(d))
			keepDisks = append(keepDisks, diskDevice(d))
		}
		patch := []map[string]any{{"op": "test", "path": "/metadata/uid", "value": v.Native.UID}}
		if o.GetResourceVersion() != "" {
			patch = append(patch, map[string]any{"op": "test", "path": "/metadata/resourceVersion", "value": o.GetResourceVersion()})
		}
		patch = append(patch, map[string]any{"op": "add", "path": "/spec/template/spec/volumes", "value": keepVolumes}, map[string]any{"op": "add", "path": "/spec/template/spec/domain/devices/disks", "value": keepDisks})
		body, _ := json.Marshal(patch)
		_, e = n.client.Resource(vmResource).Namespace(v.Spec.Namespace).Patch(ctx, v.Native.Name, types.JSONPatchType, body, metav1.PatchOptions{})
		return false, e
	}
	x, e := n.Observe(ctx, v)
	if e != nil {
		return false, e
	}
	if x.Stopped && x.VMI == nil {
		return true, nil
	}
	if !x.Running || x.VMI == nil {
		return false, nil
	}
	vmi, e := n.client.Resource(vmiResource).Namespace(v.Spec.Namespace).Get(ctx, x.VMI.Name, metav1.GetOptions{})
	if e != nil {
		return false, e
	}
	if string(vmi.GetUID()) != x.VMI.UID || !owned(vmi, "VirtualMachine", v.Native.UID) {
		return false, biz.ErrOwnership
	}
	runningVolumes, _, _ := unstructured.NestedSlice(vmi.Object, "spec", "volumes")
	runningDisks, _, _ := unstructured.NestedSlice(vmi.Object, "spec", "domain", "devices", "disks")
	_, rv, e := namedItems(runningVolumes, d.VolumeName)
	if e != nil {
		return false, e
	}
	_, rd, e := namedItems(runningDisks, d.VolumeName)
	if e != nil {
		return false, e
	}
	statuses, _, _ := unstructured.NestedSlice(vmi.Object, "status", "volumeStatus")
	_, volumeStatus, e := namedItems(statuses, d.VolumeName)
	if e != nil {
		return false, e
	}
	if !attached {
		return rv == nil && rd == nil && volumeStatus == nil, nil
	}
	return validDiskVolume(rv, observed) && validDiskDevice(rd, observed) && volumeStatus != nil && volumeStatus["phase"] == "Ready", nil
}

func (n *Native) DiskUnreferenced(ctx context.Context, v biz.VM, d biz.Disk) (bool, error) {
	x, e := n.Observe(ctx, v)
	if e != nil {
		return false, e
	}
	if x.VM.UID != v.Native.UID {
		return false, biz.ErrOwnership
	}
	for _, item := range []struct {
		resource schema.GroupVersionResource
		ref      *biz.ObjectRef
	}{{vmResource, &v.Native}, {vmiResource, x.VMI}} {
		if item.ref == nil {
			continue
		}
		o, e := n.client.Resource(item.resource).Namespace(v.Spec.Namespace).Get(ctx, item.ref.Name, metav1.GetOptions{})
		if e != nil {
			return false, e
		}
		if string(o.GetUID()) != item.ref.UID {
			return false, biz.ErrOwnership
		}
		path := []string{"spec"}
		if item.resource == vmResource {
			path = append(path, "template", "spec")
		}
		volumes, _, _ := unstructured.NestedSlice(o.Object, append(path, "volumes")...)
		for _, entry := range volumes {
			m, ok := entry.(map[string]any)
			if !ok {
				return false, biz.ErrChanged
			}
			dv, _, _ := unstructured.NestedString(m, "dataVolume", "name")
			pvc, _, _ := unstructured.NestedString(m, "persistentVolumeClaim", "claimName")
			if m["name"] == d.VolumeName || (d.DV.Name != "" && dv == d.DV.Name) || (d.PVC.Name != "" && pvc == d.PVC.Name) {
				return false, nil
			}
		}
		devices, _, _ := unstructured.NestedSlice(o.Object, append(path, "domain", "devices", "disks")...)
		_, device, e := namedItems(devices, d.VolumeName)
		if e != nil {
			return false, e
		}
		if device != nil {
			return false, nil
		}
		if item.resource == vmiResource {
			statuses, _, _ := unstructured.NestedSlice(o.Object, "status", "volumeStatus")
			_, status, e := namedItems(statuses, d.VolumeName)
			if e != nil {
				return false, e
			}
			if status != nil {
				return false, nil
			}
		}
	}
	return true, nil
}

var _ biz.DiskNativePort = (*Native)(nil)
