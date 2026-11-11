package vm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// Clone APIs are pinned to the served storage version measured on the target
// cluster (v1beta1). A different served version is reported as a dependency.
var cloneResource = schema.GroupVersionResource{Group: "clone.kubevirt.io", Version: "v1beta1", Resource: "virtualmachineclones"}
var storageClassResource = schema.GroupVersionResource{Group: "storage.k8s.io", Version: "v1", Resource: "storageclasses"}

// CloneCreateOperationAnnotation anchors an uncertain create receipt to this
// operation. It is written only on the Clone object.
const CloneCreateOperationAnnotation = "vm.ani.io/clone-create-operation-id"

// cloneSourceAnnotationKeys are source identity markers that must never stay on
// a cloned target volume. Native lifecycle markers (restore.kubevirt.io/*,
// cdi.kubevirt.io/*) are preserved on purpose.
var cloneSourceAnnotationKeys = []string{
	"vm.ani.io/vm-id", "vm.ani.io/create-digest", "vm.ani.io/media-id",
	"vm.ani.io/media-source-digest", "vm.ani.io/initialization-intent",
	"vm.ani.io/network-initialization", "vm.ani.io/snapshot-id",
	"vm.ani.io/snapshot-uid", "vm.ani.io/source-vm-uid", "vm.ani.io/source-sha256",
	"network.ani.io/instance-id", "network.ani.io/attachment-id",
	"network.ani.io/submission-id", "network.ani.io/generation",
	"network.ani.io/interface-revision", "networking.kubercloud.com/vnic",
	"kubectl.kubernetes.io/last-applied-configuration",
}

func cloneUnsupported(volume, reason string) biz.CloneBlocker {
	return biz.CloneBlocker{Kind: "UNSUPPORTED", VolumeName: volume, Reason: reason}
}

func cloneNotReady(volume, reason string) biz.CloneBlocker {
	return biz.CloneBlocker{Kind: "NOT_READY", VolumeName: volume, Reason: reason}
}

// cloneStandardSeedVolume is the fixed template volume of the platform NoCloud
// one-shot seed.
const cloneStandardSeedVolume = "guest-initialization"

// classifyCloneSource is the structural source decision. It reads no other
// object: the business record proves seed ownership and boot intent, while the
// native template proves volume, device and network structure.
func classifyCloneSource(source *biz.VM, target biz.VM, native *unstructured.Unstructured) (biz.CloneEligibility, map[string]string) {
	var result biz.CloneEligibility
	claims := map[string]string{}
	if target.Spec.CloneSource == nil {
		result.Unsupported = append(result.Unsupported, cloneUnsupported("", "clone source is not frozen"))
		return result, claims
	}
	sourceID := target.Spec.CloneSource.SourceVMID
	seedName := "vm-init-" + sourceID
	if dvt, found, _ := unstructured.NestedSlice(native.Object, "spec", "dataVolumeTemplates"); found && len(dvt) > 0 {
		result.Unsupported = append(result.Unsupported, cloneUnsupported("", "source uses dataVolumeTemplates"))
	}
	volumes, _, _ := unstructured.NestedSlice(native.Object, "spec", "template", "spec", "volumes")
	plan := map[string]bool{}
	for _, name := range target.FrozenVolumeNames() {
		plan[name] = true
	}
	for _, raw := range volumes {
		vol, ok := raw.(map[string]any)
		if !ok {
			result.Unsupported = append(result.Unsupported, cloneUnsupported("", "source volume is not an object"))
			continue
		}
		name, _ := vol["name"].(string)
		if name == "" {
			result.Unsupported = append(result.Unsupported, cloneUnsupported("", "source volume has no name"))
			continue
		}
		if pvc, ok := vol["persistentVolumeClaim"].(map[string]any); ok {
			claim, _ := pvc["claimName"].(string)
			if claim == "" {
				result.Unsupported = append(result.Unsupported, cloneUnsupported(name, "volume has no persistentVolumeClaim.claimName"))
				continue
			}
			claims[name] = claim
			continue
		}
		if dv, ok := vol["dataVolume"].(map[string]any); ok {
			dvName, _ := dv["name"].(string)
			if dvName == "" {
				result.Unsupported = append(result.Unsupported, cloneUnsupported(name, "volume has no dataVolume.name"))
				continue
			}
			claims[name] = dvName
			continue
		}
		if name == cloneStandardSeedVolume {
			ref, _, _ := unstructured.NestedString(vol, "cloudInitNoCloud", "secretRef", "name")
			netRef, _, _ := unstructured.NestedString(vol, "cloudInitNoCloud", "networkDataSecretRef", "name")
			if ref != seedName || netRef != seedName {
				result.Unsupported = append(result.Unsupported, cloneUnsupported(name, "source uses a non-standard initialization secret"))
				continue
			}
			// The business record proves the seed is the platform one; the native
			// reference alone cannot tell a managed seed from a caller's Secret.
			if source != nil {
				if source.Spec.GuestInitialization == nil {
					result.Unsupported = append(result.Unsupported, cloneUnsupported(name, "source seed has no business guest initialization"))
				} else if source.InitializationSecret.Name != "" && source.InitializationSecret.Name != seedName {
					result.Unsupported = append(result.Unsupported, cloneUnsupported(name, "source initialization secret is not the platform one"))
				}
			}
			continue
		}
		if strings.HasPrefix(name, "media-install") || strings.HasPrefix(name, "media-driver") {
			result.Unsupported = append(result.Unsupported, cloneUnsupported(name, "source still mounts installation media"))
			continue
		}
		result.Unsupported = append(result.Unsupported, cloneUnsupported(name, "unsupported source volume type"))
	}
	// A retired media copy is normal history: Eject only unbinds the current
	// mount and the copy stays until the VM is deleted, so its presence must not
	// gate cloning. Only media that is still mounted is rejected, by the volume
	// name and the cdrom device checks below.
	for name := range claims {
		if !plan[name] {
			result.Unsupported = append(result.Unsupported, cloneUnsupported(name, "source volume is not part of the frozen disk plan"))
		}
	}
	for name := range plan {
		if _, ok := claims[name]; !ok {
			result.Unsupported = append(result.Unsupported, cloneUnsupported(name, "frozen disk volume is absent from the source template"))
		}
	}
	if source != nil {
		boot := source.CurrentBootDevice
		if boot == "" {
			boot = source.Spec.BootDevice
		}
		if boot == "" {
			boot = "SYSTEM_DISK"
		}
		if boot != "SYSTEM_DISK" {
			result.Unsupported = append(result.Unsupported, cloneUnsupported("", "source boot device is not SYSTEM_DISK"))
		}
		// Phase 1 admits only an automatic primary interface and a VM without
		// native backend state. Both are business facts, not native hints: a
		// manual VNIC lives in NetworkInterfaces and persistent firmware state is
		// frozen in Media. A clone can carry neither the source vnic binding nor
		// its persistent-state volume, so accepting such a source would silently
		// move it to another network or lose firmware state.
		//
		// Only the applied interface counts. A desired-manual interface that never
		// took effect (the switch requires a stopped VM) leaves the source on the
		// automatic profile the clone actually copies, so it is not a manual
		// source yet.
		if biz.PrimaryNetworkInterface(*source).Applied.Mode == "manual" {
			result.Unsupported = append(result.Unsupported, cloneUnsupported("", "source uses a manual primary network interface"))
		}
		if biz.NeedsBackendState(source.Spec) {
			result.Unsupported = append(result.Unsupported, cloneUnsupported("", "source uses persistent TPM/UEFI backend state"))
		}
	}
	// Media devices and multi-NIC/manual VNIC sources stay out of phase 1.
	disks, _, _ := unstructured.NestedSlice(native.Object, "spec", "template", "spec", "domain", "devices", "disks")
	for _, raw := range disks {
		dev, ok := raw.(map[string]any)
		if !ok {
			result.Unsupported = append(result.Unsupported, cloneUnsupported("", "source disk device is not an object"))
			continue
		}
		if _, ok := dev["cdrom"]; ok {
			result.Unsupported = append(result.Unsupported, cloneUnsupported("", "source still boots from media"))
			break
		}
	}
	interfaces, _, _ := unstructured.NestedSlice(native.Object, "spec", "template", "spec", "domain", "devices", "interfaces")
	networks, _, _ := unstructured.NestedSlice(native.Object, "spec", "template", "spec", "networks")
	if len(interfaces) > 1 || len(networks) > 1 {
		result.Unsupported = append(result.Unsupported, cloneUnsupported("", "source uses multiple network interfaces"))
	}
	annotations := native.GetAnnotations()
	if annotations["networking.kubercloud.com/vnic"] != "" || annotations["kubevirt.io/persistent-state-for"] != "" {
		result.Unsupported = append(result.Unsupported, cloneUnsupported("", "source uses a manual VNIC or backend storage"))
	}
	if pstate := native.GetLabels()["persistent-state-for"]; pstate != "" {
		result.Unsupported = append(result.Unsupported, cloneUnsupported("", "source uses backend storage"))
	}
	return result, claims
}

// ObserveSourceCloneEligibility reads the accepted source native VM and checks
// the first-phase source scope plus the PVC/DV snapshot capability. It is the
// full admission used by Resolve: it also verifies the platform seed Secret.
func (n *Native) ObserveSourceCloneEligibility(ctx context.Context, target, source biz.VM) (biz.CloneEligibility, error) {
	return n.sourceEligibility(ctx, target, &source, true)
}

// RecheckCloneSource is the pre-submit recheck. It re-reads the same named
// source and repeats only the native structure and snapshot-capability checks;
// admission already froze PVC, StorageClass and Secret.
func (n *Native) RecheckCloneSource(ctx context.Context, target biz.VM) (biz.CloneEligibility, error) {
	return n.sourceEligibility(ctx, target, nil, false)
}

func (n *Native) sourceEligibility(ctx context.Context, target biz.VM, source *biz.VM, deep bool) (biz.CloneEligibility, error) {
	var result biz.CloneEligibility
	c := target.Spec.CloneSource
	if c == nil || c.Native.Name == "" || c.Native.UID == "" {
		return result, biz.ErrInvalid
	}
	src, err := n.client.Resource(vmResource).Namespace(target.Spec.Namespace).Get(ctx, c.Native.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		result.Unsupported = append(result.Unsupported, cloneUnsupported("", "source VM is gone"))
		return result, nil
	}
	if err != nil {
		return result, err
	}
	if string(src.GetUID()) != c.Native.UID || src.GetLabels()["vm.ani.io/vm-id"] != c.SourceVMID || src.GetNamespace() != target.Spec.Namespace {
		return result, biz.ErrOwnership
	}
	if source != nil && (source.Native.UID == "" || source.Native.UID != c.Native.UID) {
		return result, biz.ErrOwnership
	}
	structure, claims := classifyCloneSource(source, target, src)
	result = structure
	if len(result.Unsupported) > 0 {
		return result, nil
	}
	// The source's one-shot initialization Secret is not a clone input: a standard
	// NoCloud target removes the seed and never replays source initialization, so
	// that Secret's presence or readability must not gate the disk copy. The
	// standard structure itself is still required and checked above.
	// Field-level capability. KubeVirt reports one entry per volume it can
	// snapshot; a missing entry is "not reported yet", not "supported".
	statuses, _, _ := unstructured.NestedSlice(src.Object, "status", "volumeSnapshotStatuses")
	byVolume := map[string]map[string]any{}
	for _, raw := range statuses {
		entry, ok := raw.(map[string]any)
		if !ok {
			return result, biz.ErrChanged
		}
		if name, _ := entry["name"].(string); name != "" {
			byVolume[name] = entry
		}
	}
	scCache := map[string]string{}
	for volume, claim := range claims {
		entry, ok := byVolume[volume]
		if !ok {
			result.NotReady = append(result.NotReady, cloneNotReady(volume, "snapshot status not reported yet"))
			continue
		}
		if enabled, _, _ := unstructured.NestedBool(entry, "enabled"); !enabled {
			reason, _ := entry["reason"].(string)
			if reason == "" {
				reason = "native volume snapshot is disabled"
			}
			result.NotReady = append(result.NotReady, cloneNotReady(volume, reason))
		}
		if !deep {
			// The cheap recheck never re-reads the frozen dependency objects.
			continue
		}
		pvc, err := n.client.Resource(pvcResource).Namespace(target.Spec.Namespace).Get(ctx, claim, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			result.NotReady = append(result.NotReady, cloneNotReady(volume, "source PVC is absent"))
			continue
		}
		if err != nil {
			return result, err
		}
		mode, _, _ := unstructured.NestedString(pvc.Object, "spec", "volumeMode")
		if mode != "Block" {
			result.Unsupported = append(result.Unsupported, cloneUnsupported(volume, "source PVC volumeMode is not Block"))
		}
		modes, _, _ := unstructured.NestedStringSlice(pvc.Object, "spec", "accessModes")
		if len(modes) != 1 || modes[0] != "ReadWriteOnce" {
			result.Unsupported = append(result.Unsupported, cloneUnsupported(volume, "source PVC accessModes is not ReadWriteOnce"))
		}
		if phase, _, _ := unstructured.NestedString(pvc.Object, "status", "phase"); phase != "Bound" {
			result.NotReady = append(result.NotReady, cloneNotReady(volume, "source PVC is not Bound"))
		}
		class, _, _ := unstructured.NestedString(pvc.Object, "spec", "storageClassName")
		if class == "" {
			result.Unsupported = append(result.Unsupported, cloneUnsupported(volume, "source PVC has no StorageClass"))
			continue
		}
		binding, ok := scCache[class]
		if !ok {
			sc, err := n.client.Resource(storageClassResource).Get(ctx, class, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				result.NotReady = append(result.NotReady, cloneNotReady(volume, "source StorageClass is absent"))
				continue
			}
			if err != nil {
				return result, err
			}
			binding, _, _ = unstructured.NestedString(sc.Object, "volumeBindingMode")
			if binding == "" {
				binding = "Immediate"
			}
			scCache[class] = binding
		}
		if binding != "Immediate" {
			result.Unsupported = append(result.Unsupported, cloneUnsupported(volume, "source StorageClass is not Immediate"))
		}
	}
	return result, nil
}

// newCloneObject builds the fixed clone submission. The four metadata filter
// groups, both explicit typed references and the absence of spec.patches are part
// of the contract and are asserted by tests.
func newCloneObject(v biz.VM, r biz.CloneRequest) *unstructured.Unstructured {
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "clone.kubevirt.io/v1beta1",
		"kind":       "VirtualMachineClone",
		"metadata":   map[string]any{"name": r.Name, "namespace": v.Spec.Namespace},
		"spec": map[string]any{
			"source":            map[string]any{"apiGroup": "kubevirt.io", "kind": "VirtualMachine", "name": r.Source.Name},
			"target":            map[string]any{"apiGroup": "kubevirt.io", "kind": "VirtualMachine", "name": r.TargetName},
			"volumeNamePolicy":  "RandomizeNames",
			"newSMBiosSerial":   r.SMBiosSerial,
			"labelFilters":      cloneFilters,
			"annotationFilters": cloneFilters,
			"template": map[string]any{
				"labelFilters":      cloneFilters,
				"annotationFilters": cloneFilters,
			},
		},
	}}
	object.SetLabels(labelVM(v))
	object.SetAnnotations(map[string]string{CloneCreateOperationAnnotation: r.OperationID})
	return object
}

// EnsureClone creates the fixed-name clone or adopts the same object by its
// recorded identity. A same-name object with another UID is never adopted.
func (n *Native) EnsureClone(ctx context.Context, v biz.VM, r biz.CloneRequest) (biz.CloneObservation, error) {
	existing, err := n.client.Resource(cloneResource).Namespace(v.Spec.Namespace).Get(ctx, r.Name, metav1.GetOptions{})
	if err == nil {
		return n.projectClone(ctx, v, r, existing)
	}
	if !apierrors.IsNotFound(err) {
		return biz.CloneObservation{}, err
	}
	object := newCloneObject(v, r)
	created, err := n.client.Resource(cloneResource).Namespace(v.Spec.Namespace).Create(ctx, object, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return n.EnsureClone(ctx, v, r)
	}
	if apierrors.IsInvalid(err) || apierrors.IsBadRequest(err) || apierrors.IsForbidden(err) {
		// A definite admission rejection cannot have created the object, so the
		// worker may close this create without waiting for an unknown receipt.
		return biz.CloneObservation{}, fmt.Errorf("%w: %v", biz.ErrInvalid, err)
	}
	if err != nil {
		return biz.CloneObservation{}, err
	}
	return n.projectClone(ctx, v, r, created)
}

// cloneFilters keeps every unlisted metadata key and drops exactly the known
// source identity keys. The same array is used for all four metadata groups.
var cloneFilters = []any{
	"*",
	"!^vm[.]ani[.]io/vm-id$",
	"!^vm[.]ani[.]io/create-digest$",
	"!^vm[.]ani[.]io/clone-create-operation-id$",
	"!^network[.]ani[.]io/instance-id$",
	"!^network[.]ani[.]io/attachment-id$",
	"!^network[.]ani[.]io/submission-id$",
	"!^network[.]ani[.]io/generation$",
	"!^network[.]ani[.]io/interface-revision$",
	"!^networking[.]kubercloud[.]com/vnic$",
	"!^kubectl[.]kubernetes[.]io/last-applied-configuration$",
	"!^vm[.]ani[.]io/media-id$",
	"!^vm[.]ani[.]io/media-source-digest$",
	"!^vm[.]ani[.]io/initialization-intent$",
	"!^vm[.]ani[.]io/network-initialization$",
	"!^vm[.]ani[.]io/snapshot-id$",
	"!^vm[.]ani[.]io/snapshot-uid$",
	"!^vm[.]ani[.]io/source-vm-uid$",
	"!^vm[.]ani[.]io/source-sha256$",
	"!^verification[.]ani[.]io/run-id$",
	"!^verification[.]ani[.]io/preparation-method$",
	"!^verification[.]ani[.]io/origin-vm-uid$",
}

// ObserveCloneTarget reads the frozen target VM by its fixed name. A clone delete
// does not delete the target, so a target created between the last clone
// observation and the delete completing is only visible this way.
func (n *Native) ObserveCloneTarget(ctx context.Context, v biz.VM, r biz.CloneRequest) (biz.ObjectRef, error) {
	if r.TargetName == "" {
		return biz.ObjectRef{}, nil
	}
	target, err := n.client.Resource(vmResource).Namespace(v.Spec.Namespace).Get(ctx, r.TargetName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return biz.ObjectRef{}, nil
	}
	if err != nil {
		return biz.ObjectRef{}, err
	}
	return objectRef(target), nil
}

func (n *Native) ObserveClone(ctx context.Context, v biz.VM, r biz.CloneRequest) (biz.CloneObservation, error) {
	object, err := n.client.Resource(cloneResource).Namespace(v.Spec.Namespace).Get(ctx, r.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return biz.CloneObservation{Exists: false}, nil
	}
	if err != nil {
		return biz.CloneObservation{}, err
	}
	return n.projectClone(ctx, v, r, object)
}

// projectClone verifies ordinary ownership before exposing any state.
func (n *Native) projectClone(ctx context.Context, v biz.VM, r biz.CloneRequest, object *unstructured.Unstructured) (biz.CloneObservation, error) {
	state := biz.CloneObservation{Exists: true, UID: string(object.GetUID()), ResourceVersion: object.GetResourceVersion()}
	if object.GetLabels()["vm.ani.io/vm-id"] != v.ID || object.GetAnnotations()[CloneCreateOperationAnnotation] != r.OperationID {
		return state, biz.ErrOwnership
	}
	sourceName, _, _ := unstructured.NestedString(object.Object, "spec", "source", "name")
	targetName, _, _ := unstructured.NestedString(object.Object, "spec", "target", "name")
	if sourceName != r.Source.Name || targetName != r.TargetName {
		return state, biz.ErrOwnership
	}
	state.Phase, _, _ = unstructured.NestedString(object.Object, "status", "phase")
	state.SnapshotName, _, _ = unstructured.NestedString(object.Object, "status", "snapshotName")
	state.RestoreName, _, _ = unstructured.NestedString(object.Object, "status", "restoreName")
	conditions, _, _ := unstructured.NestedSlice(object.Object, "status", "conditions")
	for _, raw := range conditions {
		condition, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		kind, _ := condition["type"].(string)
		status, _ := condition["status"].(string)
		reason, _ := condition["reason"].(string)
		message, _ := condition["message"].(string)
		if kind == "Progressing" && status == "False" && reason == "Pending" {
			state.Pending = true
			if message != "" {
				state.Reason = message
			} else {
				state.Reason = reason
			}
		}
		if kind == "Ready" && status != "True" && message != "" && state.Reason == "" {
			state.Reason = message
		}
	}
	switch {
	case state.Phase == "Succeeded":
		state.Succeeded = true
	case state.Phase == "Failed":
		state.Failed = true
		if state.Reason == "" {
			state.Reason = "clone failed"
		}
	}
	state.Target = biz.ObjectRef{Kind: "VirtualMachine", Namespace: v.Spec.Namespace, Name: targetName}
	if target, err := n.client.Resource(vmResource).Namespace(v.Spec.Namespace).Get(ctx, targetName, metav1.GetOptions{}); err == nil {
		state.Target = objectRef(target)
		// The target's own volume references are the primary cancel hand-off; the
		// restore list is only a fallback while the target cannot be read.
		outputs, err := n.cloneTargetOutputs(ctx, v, target)
		if err != nil {
			return state, err
		}
		state.Outputs = mergeCloneOutputs(outputs, state.Outputs)
	} else if !apierrors.IsNotFound(err) {
		return state, err
	}
	if state.RestoreName != "" {
		outputs, err := n.cloneRestoreOutputs(ctx, v, state.RestoreName)
		if err != nil {
			return state, err
		}
		state.Outputs = mergeCloneOutputs(state.Outputs, outputs)
	}
	return state, nil
}

// cloneTargetOutputs reads the named PVC volumes the clone target actually
// references. Only volumes of the frozen plan are returned.
func (n *Native) cloneTargetOutputs(ctx context.Context, v biz.VM, target *unstructured.Unstructured) ([]biz.VolumeRef, error) {
	if v.Spec.CloneSource == nil {
		return nil, nil
	}
	frozen := map[string]bool{}
	for _, name := range v.FrozenVolumeNames() {
		frozen[name] = true
	}
	volumes, _, _ := unstructured.NestedSlice(target.Object, "spec", "template", "spec", "volumes")
	var outputs []biz.VolumeRef
	for _, raw := range volumes {
		volume, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name, _ := volume["name"].(string)
		if !frozen[name] {
			continue
		}
		claim, _, _ := unstructured.NestedString(volume, "persistentVolumeClaim", "claimName")
		if claim == "" {
			continue
		}
		ref := biz.VolumeRef{VolumeName: name, PVCName: claim}
		if pvc, err := n.client.Resource(pvcResource).Namespace(v.Spec.Namespace).Get(ctx, claim, metav1.GetOptions{}); err == nil {
			ref.PVCUID = string(pvc.GetUID())
		} else if !apierrors.IsNotFound(err) {
			return nil, err
		}
		outputs = append(outputs, ref)
	}
	return outputs, nil
}

// mergeCloneOutputs keeps the primary list and appends only fallback volumes it
// does not cover.
func mergeCloneOutputs(primary, fallback []biz.VolumeRef) []biz.VolumeRef {
	if len(primary) == 0 {
		return fallback
	}
	seen := map[string]bool{}
	merged := append([]biz.VolumeRef(nil), primary...)
	for _, ref := range primary {
		seen[ref.VolumeName] = true
	}
	for _, ref := range fallback {
		if !seen[ref.VolumeName] {
			merged = append(merged, ref)
		}
	}
	return merged
}

// cloneRestoreOutputs reads the named outputs of the clone's own Restore. The
// names are a cancellation hand-off, not a copy-success proof.
func (n *Native) cloneRestoreOutputs(ctx context.Context, v biz.VM, name string) ([]biz.VolumeRef, error) {
	restore, err := n.client.Resource(restoreResource).Namespace(v.Spec.Namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	entries, _, _ := unstructured.NestedSlice(restore.Object, "status", "restores")
	var outputs []biz.VolumeRef
	for _, raw := range entries {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		volume, _ := entry["volumeName"].(string)
		claim, _ := entry["persistentVolumeClaim"].(string)
		if volume == "" {
			continue
		}
		ref := biz.VolumeRef{VolumeName: volume, PVCName: claim}
		if claim != "" {
			if pvc, err := n.client.Resource(pvcResource).Namespace(v.Spec.Namespace).Get(ctx, claim, metav1.GetOptions{}); err == nil {
				ref.PVCUID = string(pvc.GetUID())
			} else if !apierrors.IsNotFound(err) {
				return nil, err
			}
		}
		outputs = append(outputs, ref)
	}
	return outputs, nil
}

// DeleteClone deletes the adopted clone with UID and, when known,
// resourceVersion preconditions. A conflict returns the freshly re-read object
// so the caller re-evaluates instead of reusing stale facts.
func (n *Native) DeleteClone(ctx context.Context, v biz.VM, r biz.CloneRequest, uid, rv string) (biz.CloneDeleteResult, error) {
	object, err := n.client.Resource(cloneResource).Namespace(v.Spec.Namespace).Get(ctx, r.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return biz.CloneDeleteResult{Gone: true}, nil
	}
	if err != nil {
		return biz.CloneDeleteResult{}, err
	}
	if uid != "" && string(object.GetUID()) != uid {
		return biz.CloneDeleteResult{}, biz.ErrOwnership
	}
	currentUID := object.GetUID()
	preconditions := &metav1.Preconditions{UID: &currentUID}
	if rv != "" {
		preconditions.ResourceVersion = &rv
	}
	policy := metav1.DeletePropagationForeground
	err = n.client.Resource(cloneResource).Namespace(v.Spec.Namespace).Delete(ctx, r.Name, metav1.DeleteOptions{Preconditions: preconditions, PropagationPolicy: &policy})
	if apierrors.IsNotFound(err) {
		return biz.CloneDeleteResult{Gone: true}, nil
	}
	if apierrors.IsConflict(err) {
		fresh, e := n.client.Resource(cloneResource).Namespace(v.Spec.Namespace).Get(ctx, r.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(e) {
			return biz.CloneDeleteResult{Gone: true}, nil
		}
		if e != nil {
			return biz.CloneDeleteResult{}, e
		}
		observation, e := n.projectClone(ctx, v, r, fresh)
		if e != nil {
			return biz.CloneDeleteResult{}, e
		}
		return biz.CloneDeleteResult{Conflict: true, Current: observation}, nil
	}
	if err != nil {
		return biz.CloneDeleteResult{}, err
	}
	object, err = n.client.Resource(cloneResource).Namespace(v.Spec.Namespace).Get(ctx, r.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return biz.CloneDeleteResult{Gone: true}, nil
	}
	if err != nil {
		return biz.CloneDeleteResult{}, err
	}
	return biz.CloneDeleteResult{}, nil
}

// DeleteCloneTarget deletes the failed clone's own target VM. The target may not
// carry business labels, so identity is proven by name and expected UID. An
// empty UID is never accepted: a same-name object could be an unrelated VM.
func (n *Native) DeleteCloneTarget(ctx context.Context, v biz.VM, target biz.ObjectRef) (bool, error) {
	if target.Name == "" {
		return true, nil
	}
	if target.UID == "" {
		return false, biz.ErrOwnership
	}
	object, err := n.client.Resource(vmResource).Namespace(v.Spec.Namespace).Get(ctx, target.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if string(object.GetUID()) != target.UID {
		return false, biz.ErrOwnership
	}
	uid := object.GetUID()
	policy := metav1.DeletePropagationForeground
	err = n.client.Resource(vmResource).Namespace(v.Spec.Namespace).Delete(ctx, target.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}, PropagationPolicy: &policy})
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil && !apierrors.IsConflict(err) {
		return false, err
	}
	object, err = n.client.Resource(vmResource).Namespace(v.Spec.Namespace).Get(ctx, target.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return false, nil
}

// DeleteNamedVolumes removes the residual PVCs of a failed clone. A PVC is only
// deleted when it is provably ours: the recorded UID matches or the target VM is
// an owner. A registered output whose recorded UID is still empty is adopted by
// its registered name first and reported back to the caller so it can be
// persisted; that output is not reported as cleaned until its identity hand-off
// is complete. A recorded UID that no longer matches means our object is already
// gone, so it is skipped instead of touching a foreign same-name PVC. One delete
// is issued per call; the caller re-observes next tick.
func (n *Native) DeleteNamedVolumes(ctx context.Context, v biz.VM, refs []biz.VolumeRef) ([]biz.VolumeRef, bool, error) {
	done := true
	adopted := make([]biz.VolumeRef, 0, len(refs))
	for _, ref := range refs {
		if ref.PVCName == "" {
			continue
		}
		object, err := n.client.Resource(pvcResource).Namespace(v.Spec.Namespace).Get(ctx, ref.PVCName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return adopted, false, err
		}
		if ref.PVCUID == "" {
			// The name came from the native output list, so this object is the one our
			// clone produced. Persist its real identity first: an output whose
			// identity hand-off is still incomplete is never deleted or reported as
			// cleaned in the same tick.
			ref.PVCUID = string(object.GetUID())
			adopted = append(adopted, ref)
			done = false
			continue
		}
		if !cloneOutputOwned(v, ref, object) {
			continue
		}
		uid := object.GetUID()
		policy := metav1.DeletePropagationForeground
		if err = n.client.Resource(pvcResource).Namespace(v.Spec.Namespace).Delete(ctx, ref.PVCName, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}, PropagationPolicy: &policy}); err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
			return adopted, false, err
		}
		if _, err = n.client.Resource(pvcResource).Namespace(v.Spec.Namespace).Get(ctx, ref.PVCName, metav1.GetOptions{}); apierrors.IsNotFound(err) {
			// Foreground deletion may complete before the next tick.
			continue
		} else if err != nil {
			return adopted, false, err
		}
		done = false
	}
	return adopted, done, nil
}

// cloneOutputOwned accepts a PVC by recorded UID or by any owner reference to
// the target VM (controller or not).
func cloneOutputOwned(v biz.VM, ref biz.VolumeRef, object *unstructured.Unstructured) bool {
	if ref.PVCUID != "" && string(object.GetUID()) == ref.PVCUID {
		return true
	}
	if v.Native.UID == "" {
		return false
	}
	for _, owner := range object.GetOwnerReferences() {
		if owner.Kind == "VirtualMachine" && string(owner.UID) == v.Native.UID {
			return true
		}
	}
	return false
}

// ApplyCloneTargetConfiguration adapts the stopped clone target and registers
// the named volumes it actually references. It is idempotent: a replayed call
// with the same registered refs converges to the same patch.
func (n *Native) ApplyCloneTargetConfiguration(ctx context.Context, v biz.VM, r biz.CloneRequest) ([]biz.Disk, biz.ObjectRef, error) {
	target, err := n.client.Resource(vmResource).Namespace(v.Spec.Namespace).Get(ctx, r.TargetName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, biz.ObjectRef{}, biz.ErrDependency
	}
	if err != nil {
		return nil, biz.ObjectRef{}, err
	}
	ref := objectRef(target)
	if v.Native.UID != "" && v.Native.UID != ref.UID {
		return nil, ref, biz.ErrOwnership
	}
	if _, err := n.client.Resource(vmiResource).Namespace(v.Spec.Namespace).Get(ctx, r.TargetName, metav1.GetOptions{}); err == nil {
		return nil, ref, biz.ErrBusy
	} else if !apierrors.IsNotFound(err) {
		return nil, ref, err
	}
	volumes, _, _ := unstructured.NestedSlice(target.Object, "spec", "template", "spec", "volumes")
	devices, _, _ := unstructured.NestedSlice(target.Object, "spec", "template", "spec", "domain", "devices", "disks")
	newVolumes := plainSlice(volumes)
	newDevices := plainSlice(devices)
	// Remove the standard one-shot seed by its actual template name, never by index.
	var keptVolumes []any
	for _, raw := range newVolumes {
		volume, ok := raw.(map[string]any)
		if !ok {
			return nil, ref, biz.ErrChanged
		}
		if name, _ := volume["name"].(string); name == "guest-initialization" {
			continue
		}
		keptVolumes = append(keptVolumes, volume)
	}
	newVolumes = keptVolumes
	var keptDevices []any
	for _, raw := range newDevices {
		device, ok := raw.(map[string]any)
		if !ok {
			return nil, ref, biz.ErrChanged
		}
		if name, _ := device["name"].(string); name == "guest-initialization" {
			continue
		}
		keptDevices = append(keptDevices, device)
	}
	newDevices = keptDevices
	// Unknown extra PVC volumes are never ignored: the target must reference
	// exactly the frozen plan after the one-shot seed is removed.
	planned := map[string]bool{}
	for _, disk := range v.Disks {
		if disk.Tombstone || !disk.Attached {
			continue
		}
		planned[disk.VolumeName] = true
	}
	for _, raw := range newVolumes {
		volume, ok := raw.(map[string]any)
		if !ok {
			return nil, ref, biz.ErrChanged
		}
		name, _ := volume["name"].(string)
		if _, isPVC := volume["persistentVolumeClaim"]; !isPVC || planned[name] {
			continue
		}
		return nil, ref, fmt.Errorf("%w: unexpected target volume %s", biz.ErrChanged, name)
	}
	registered := make([]biz.Disk, 0, len(v.Disks))
	for _, disk := range v.Disks {
		if disk.Tombstone || !disk.Attached {
			continue
		}
		_, volume, err := namedItems(newVolumes, disk.VolumeName)
		if err != nil {
			return nil, ref, err
		}
		if volume == nil {
			return nil, ref, biz.ErrChanged
		}
		claim, _, _ := unstructured.NestedString(volume, "persistentVolumeClaim", "claimName")
		if claim == "" {
			return nil, ref, biz.ErrChanged
		}
		if disk.Role == "data" {
			if err = unstructured.SetNestedField(volume, true, "persistentVolumeClaim", "hotpluggable"); err != nil {
				return nil, ref, err
			}
		}
		_, device, err := namedItems(newDevices, disk.VolumeName)
		if err != nil {
			return nil, ref, err
		}
		if device == nil {
			return nil, ref, biz.ErrChanged
		}
		if disk.Role == "data" {
			device["disk"] = map[string]any{"bus": "scsi"}
			delete(device, "lun")
			delete(device, "cdrom")
		}
		device["serial"] = disk.Serial
		pvc, err := n.client.Resource(pvcResource).Namespace(v.Spec.Namespace).Get(ctx, claim, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil, ref, biz.ErrDependency
		}
		if err != nil {
			return nil, ref, err
		}
		if mode, _, _ := unstructured.NestedString(pvc.Object, "spec", "volumeMode"); mode != "Block" {
			return nil, ref, biz.ErrOwnership
		}
		if phase, _, _ := unstructured.NestedString(pvc.Object, "status", "phase"); phase != "Bound" {
			return nil, ref, biz.ErrDependency
		}
		if err = n.adoptTargetVolume(ctx, v, ref, pvc); err != nil {
			return nil, ref, err
		}
		capacity := int64(0)
		if size, _, _ := unstructured.NestedString(pvc.Object, "status", "capacity", "storage"); size != "" {
			quantity, err := resource.ParseQuantity(size)
			if err != nil {
				return nil, ref, biz.ErrChanged
			}
			capacity = quantity.Value()
		}
		registeredDisk := disk
		registeredDisk.PVC = biz.ObjectRef{Kind: "PersistentVolumeClaim", Namespace: v.Spec.Namespace, Name: claim, UID: string(pvc.GetUID())}
		registeredDisk.DV = biz.ObjectRef{}
		registeredDisk.ObservedCapacityBytes = capacity
		registeredDisk.Preparation = "Bound"
		registered = append(registered, registeredDisk)
	}
	// Merge target identity into the metadata the filters preserved instead of
	// replacing whole maps, so native restore/PCI/network markers stay intact.
	rawLabels := map[string]any{}
	for k, x := range target.GetLabels() {
		rawLabels[k] = x
	}
	for k, x := range labelVM(v) {
		rawLabels[k] = x
	}
	for k, x := range v.Network.Labels {
		rawLabels[k] = x
	}
	templateAnnotations := map[string]any{}
	if template, ok, _ := unstructured.NestedMap(target.Object, "spec", "template", "metadata", "annotations"); ok {
		for k, x := range template {
			templateAnnotations[k] = x
		}
	}
	if v.Network.KCNSubnetRef != "" {
		templateAnnotations["networking.kubercloud.com/subnet"] = v.Network.KCNSubnetRef
	}
	templateAnnotations[biz.SecurityGroupsAnnotation] = strings.Join(v.SecurityGroupNames, ",")
	if v.Network.BasicIsolationName != "" {
		templateAnnotations["networking.kubercloud.com/basic-network-isolation"] = v.Network.BasicIsolationName
	}
	patch := []map[string]any{
		{"op": "test", "path": "/metadata/uid", "value": ref.UID},
		{"op": "test", "path": "/metadata/resourceVersion", "value": target.GetResourceVersion()},
		{"op": "add", "path": "/metadata/labels", "value": rawLabels},
		{"op": "add", "path": "/spec/template/metadata/labels", "value": rawLabels},
		{"op": "add", "path": "/spec/template/metadata/annotations", "value": templateAnnotations},
		{"op": "add", "path": "/spec/template/spec/volumes", "value": newVolumes},
		{"op": "add", "path": "/spec/template/spec/domain/devices/disks", "value": newDevices},
		{"op": "add", "path": "/spec/template/spec/domain/firmware/uuid", "value": v.ID},
		{"op": "add", "path": "/spec/runStrategy", "value": "Manual"},
	}
	body, err := json.Marshal(patch)
	if err != nil {
		return nil, ref, err
	}
	if _, err = n.client.Resource(vmResource).Namespace(v.Spec.Namespace).Patch(ctx, r.TargetName, types.JSONPatchType, body, metav1.PatchOptions{}); err != nil {
		return nil, ref, err
	}
	return registered, ref, nil
}

// adoptTargetVolume clears source identity from the clone's own PVC and makes the
// target VM its controller owner. Native restore/CDI markers are preserved.
func (n *Native) adoptTargetVolume(ctx context.Context, v biz.VM, target biz.ObjectRef, pvc *unstructured.Unstructured) error {
	owners := pvc.GetOwnerReferences()
	hasTarget := false
	hasOtherController := false
	for _, owner := range owners {
		if owner.Kind == "VirtualMachine" && string(owner.UID) == target.UID {
			hasTarget = true
			continue
		}
		if owner.Controller != nil && *owner.Controller {
			hasOtherController = true
		}
	}
	if !hasTarget && hasOtherController {
		return biz.ErrOwnership
	}
	labels := map[string]any{}
	annotations := map[string]any{}
	for _, key := range cloneSourceAnnotationKeys {
		labels[key] = nil
		annotations[key] = nil
	}
	targetLabels := labelVM(v)
	for key, value := range targetLabels {
		labels[key] = value
	}
	patch := map[string]any{"metadata": map[string]any{"labels": labels, "annotations": annotations}}
	if !hasTarget {
		yes := true
		patch["metadata"].(map[string]any)["ownerReferences"] = []metav1.OwnerReference{{APIVersion: "kubevirt.io/v1", Kind: "VirtualMachine", Name: target.Name, UID: types.UID(target.UID), Controller: &yes, BlockOwnerDeletion: &yes}}
	}
	body, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	if _, err = n.client.Resource(pvcResource).Namespace(v.Spec.Namespace).Patch(ctx, pvc.GetName(), types.MergePatchType, body, metav1.PatchOptions{}); err != nil {
		if apierrors.IsConflict(err) {
			return biz.ErrChanged
		}
		return err
	}
	return nil
}

// ClonePresence is the read-only residual check used by the controlled release
// command. It never writes and never enumerates child objects.
type ClonePresence struct {
	Exists          bool
	UID             string
	ResourceVersion string
	Deleting        bool
	Finalizers      []string
}

// ClonePresence reads one clone object by its fixed name.
func (n *Native) ClonePresence(ctx context.Context, namespace, name string) (ClonePresence, error) {
	object, err := n.client.Resource(cloneResource).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return ClonePresence{}, nil
	}
	if err != nil {
		return ClonePresence{}, err
	}
	return ClonePresence{Exists: true, UID: string(object.GetUID()), ResourceVersion: object.GetResourceVersion(), Deleting: object.GetDeletionTimestamp() != nil, Finalizers: object.GetFinalizers()}, nil
}

var _ biz.CloneNativePort = (*Native)(nil)

// cloneSummary keeps a stable diagnostic string for the worker reason field.
func cloneSummary(state biz.CloneObservation) string {
	if state.Phase != "" {
		return fmt.Sprintf("clone %s", state.Phase)
	}
	if state.Reason != "" {
		return state.Reason
	}
	return "clone pending"
}
