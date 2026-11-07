package vm

import (
	"context"
	"sort"
	"strings"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
)

// KubeVirt v1.9.0 pkg/storage/backend-storage/backend-storage.go creates a
// Filesystem PVC with this label and copies the VMI's VM controller owner.
// Its physical GenerateName is deliberately not used as an ownership proof.
const backendStateLabel = "persistent-state-for"

var backendStorageClasses = schema.GroupVersionResource{Group: "storage.k8s.io", Version: "v1", Resource: "storageclasses"}

func (n *Native) ConfigureBackendDomain(v biz.VM, domain map[string]any) error {
	if !biz.NeedsBackendState(v.Spec) {
		return nil
	}
	if v.Spec.BackendStorageClass == "" || (v.Spec.Media.PersistentUEFI && v.Spec.Media.Firmware != "uefi") {
		return biz.ErrInvalid
	}
	if v.Spec.Media.PersistentTPM {
		if err := unstructured.SetNestedField(domain, true, "devices", "tpm", "enabled"); err != nil {
			return err
		}
		if err := unstructured.SetNestedField(domain, true, "devices", "tpm", "persistent"); err != nil {
			return err
		}
	}
	if v.Spec.Media.PersistentUEFI {
		if _, hasBIOS, _ := unstructured.NestedMap(domain, "firmware", "bootloader", "bios"); hasBIOS {
			return biz.ErrChanged
		}
		if err := unstructured.SetNestedField(domain, true, "firmware", "bootloader", "efi", "persistent"); err != nil {
			return err
		}
	}
	return nil
}

// ValidateBackendConfiguration validates deployment support for first creation.
// Existing VM power requests use their registered native identity.
func (n *Native) ValidateBackendConfiguration(ctx context.Context, s biz.Spec) error {
	if !biz.NeedsBackendState(s) {
		return nil
	}
	if s.BackendStorageClass == "" || (s.Media.PersistentUEFI && s.Media.Firmware != "uefi") {
		return biz.ErrInvalid
	}
	list, err := n.client.Resource(kubevirtResource).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	if len(list.Items) != 1 {
		return biz.ErrDependency
	}
	kv := &list.Items[0]
	version, _, _ := unstructured.NestedString(kv.Object, "status", "observedKubeVirtVersion")
	phase, _, _ := unstructured.NestedString(kv.Object, "status", "phase")
	if kv.GetAPIVersion() != "kubevirt.io/v1" || kv.GetKind() != "KubeVirt" || kv.GetUID() == "" || version != "v1.9.0" || phase != "Deployed" {
		return biz.ErrDependency
	}
	class, _, _ := unstructured.NestedString(kv.Object, "spec", "configuration", "vmStateStorageClass")
	if class == "" {
		return biz.ErrDependency
	}
	if class != s.BackendStorageClass {
		return biz.ErrChanged
	}
	sc, err := n.client.Resource(backendStorageClasses).Get(ctx, class, metav1.GetOptions{})
	if err != nil {
		return err
	}
	policy, _, _ := unstructured.NestedString(sc.Object, "reclaimPolicy")
	if sc.GetUID() == "" || policy != "Delete" {
		return biz.ErrDependency
	}
	return nil
}

func backendVolumeName(vmName string) (string, error) {
	// The product's immutable native name is vm-<UUID>. At this length the
	// v1.9 BackendPVCVolumeName/naming.GetName path does not truncate/hash.
	name := backendStateLabel + "-" + vmName
	if len(name) > 63 || len(validation.IsDNS1035Label(name)) != 0 {
		return "", biz.ErrOwnership
	}
	return name, nil
}

func backendDomainMatches(object *unstructured.Unstructured, v biz.VM, vmi bool) bool {
	path := []string{"spec", "template", "spec", "domain"}
	if vmi {
		path = []string{"spec", "domain"}
	}
	domain, found, err := unstructured.NestedMap(object.Object, path...)
	if err != nil || !found {
		return false
	}
	firmware, _, _ := unstructured.NestedString(domain, "firmware", "uuid")
	if firmware != v.ID {
		return false
	}
	if v.Spec.Media.PersistentTPM {
		persistent, _, _ := unstructured.NestedBool(domain, "devices", "tpm", "persistent")
		enabled, present, _ := unstructured.NestedBool(domain, "devices", "tpm", "enabled")
		if !persistent || (present && !enabled) {
			return false
		}
	}
	if v.Spec.Media.PersistentUEFI {
		persistent, _, _ := unstructured.NestedBool(domain, "firmware", "bootloader", "efi", "persistent")
		_, bios, _ := unstructured.NestedMap(domain, "firmware", "bootloader", "bios")
		if !persistent || bios {
			return false
		}
	}
	return true
}

func backendOwned(object *unstructured.Unstructured, v biz.VM) bool {
	if object.GetAPIVersion() != "v1" || object.GetKind() != "PersistentVolumeClaim" || object.GetNamespace() != v.Spec.Namespace || object.GetUID() == "" {
		return false
	}
	controllers := 0
	for _, owner := range object.GetOwnerReferences() {
		if owner.Controller == nil || !*owner.Controller {
			continue
		}
		controllers++
		if owner.APIVersion != "kubevirt.io/v1" || owner.Kind != "VirtualMachine" || owner.Name != v.Native.Name || string(owner.UID) != v.Native.UID {
			return false
		}
	}
	return controllers == 1 && v.Native.UID != ""
}

func (n *Native) observeBackendPVC(ctx context.Context, v biz.VM, pvc *unstructured.Unstructured, old biz.BackendState) (biz.BackendState, error) {
	if !backendOwned(pvc, v) || (old.PVC.UID != "" && old.PVC != objectRef(pvc)) {
		return old, biz.ErrOwnership
	}
	class, _, _ := unstructured.NestedString(pvc.Object, "spec", "storageClassName")
	mode, _, _ := unstructured.NestedString(pvc.Object, "spec", "volumeMode")
	if mode == "" {
		mode = "Filesystem"
	}
	if class != v.Spec.BackendStorageClass || mode != "Filesystem" {
		return old, biz.ErrChanged
	}
	if old.StorageClass != "" && (class != old.StorageClass || mode != old.VolumeMode) {
		return old, biz.ErrOwnership
	}
	state := old
	state.PVC = objectRef(pvc)
	state.Owner = v.Native
	state.StorageClass = class
	state.VolumeMode = mode
	state.Present = true
	name, err := backendVolumeName(v.Native.Name)
	if err != nil {
		return old, err
	}
	state.VolumeName = name
	state.Phase, _, _ = unstructured.NestedString(pvc.Object, "status", "phase")
	requested, _, _ := unstructured.NestedString(pvc.Object, "spec", "resources", "requests", "storage")
	quantity, err := resource.ParseQuantity(requested)
	if err != nil || quantity.Sign() <= 0 {
		return old, biz.ErrOwnership
	}
	state.RequestedBytes = quantity.Value()
	capacity, found, _ := unstructured.NestedString(pvc.Object, "status", "capacity", "storage")
	if found {
		q, err := resource.ParseQuantity(capacity)
		if err != nil || q.Sign() <= 0 {
			return old, biz.ErrOwnership
		}
		state.CapacityBytes = q.Value()
	}
	return state, nil
}

func (n *Native) ObserveBackendState(ctx context.Context, v biz.VM, registered []biz.BackendState) ([]biz.BackendState, error) {
	states := append([]biz.BackendState(nil), registered...)
	for i := range states {
		states[i].Current = false
	}
	if !biz.NeedsBackendState(v.Spec) && len(states) == 0 {
		return states, nil
	}
	if v.Native.Kind != "VirtualMachine" || v.Native.UID == "" || v.Native.Namespace != v.Spec.Namespace {
		return states, biz.ErrOwnership
	}
	if err := n.namespace(ctx, v); err != nil {
		return states, err
	}
	root, err := n.client.Resource(vmResource).Namespace(v.Spec.Namespace).Get(ctx, v.Native.Name, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return states, err
	}
	if err == nil && (!matches(root, v) || string(root.GetUID()) != v.Native.UID || (!v.Closing && !backendDomainMatches(root, v, false))) {
		return states, biz.ErrOwnership
	}
	if apierrors.IsNotFound(err) && !v.Closing {
		return states, biz.ErrOwnership
	}
	currentName := ""
	vmi, err := n.client.Resource(vmiResource).Namespace(v.Spec.Namespace).Get(ctx, v.Native.Name, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return states, err
	}
	running := false
	if err == nil {
		if !owned(vmi, "VirtualMachine", v.Native.UID) || (!v.Closing && !backendDomainMatches(vmi, v, true)) {
			return states, biz.ErrOwnership
		}
		phase, _, _ := unstructured.NestedString(vmi.Object, "status", "phase")
		running = phase == "Running"
		if !v.Closing {
			volumes, _, _ := unstructured.NestedSlice(vmi.Object, "status", "volumeStatus")
			for _, raw := range volumes {
				volume, ok := raw.(map[string]any)
				if !ok {
					return states, biz.ErrOwnership
				}
				name, _ := volume["name"].(string)
				if !strings.HasPrefix(name, backendStateLabel) {
					continue
				}
				claim, _, _ := unstructured.NestedString(volume, "persistentVolumeClaimInfo", "claimName")
				if claim == "" || (currentName != "" && currentName != claim) {
					return states, biz.ErrOwnership
				}
				currentName = claim
			}
		}
	}
	if v.Closing {
		currentName = ""
	}
	list, err := n.client.Resource(pvcResource).Namespace(v.Spec.Namespace).List(ctx, metav1.ListOptions{LabelSelector: backendStateLabel + "=" + v.Native.Name})
	if err != nil {
		return states, err
	}
	candidates := map[string]*unstructured.Unstructured{}
	for i := range list.Items {
		pvc := &list.Items[i]
		if pvc.GetLabels()[backendStateLabel] != v.Native.Name || !backendOwned(pvc, v) {
			return states, biz.ErrOwnership
		}
		if pvc.GetDeletionTimestamp() == nil || v.Closing {
			candidates[pvc.GetName()] = pvc
		}
	}
	if !v.Closing && running && biz.NeedsBackendState(v.Spec) && currentName == "" {
		return states, biz.ErrDependency
	}
	if !v.Closing && currentName == "" && len(candidates) == 1 {
		for name := range candidates {
			currentName = name
		}
	}
	if !v.Closing && len(candidates) > 1 && currentName == "" {
		return states, biz.ErrDependency
	}
	if !v.Closing && currentName != "" && candidates[currentName] == nil {
		return states, biz.ErrDependency
	}
	if len(registered) > 0 && len(candidates) == 0 && !v.Closing {
		return states, biz.ErrDependency
	}
	known := map[string]bool{}
	for i := range states {
		state := states[i]
		known[state.PVC.UID] = true
		if state.Owner != v.Native || state.PVC.Kind != "PersistentVolumeClaim" || state.PVC.Namespace != v.Spec.Namespace || state.PVC.UID == "" {
			return states, biz.ErrOwnership
		}
		pvc, err := n.client.Resource(pvcResource).Namespace(v.Spec.Namespace).Get(ctx, state.PVC.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			state.Current = false
			state.Present = false
			states[i] = state
			continue
		}
		if err != nil {
			return states, err
		}
		observed, err := n.observeBackendPVC(ctx, v, pvc, state)
		if err != nil {
			return states, err
		}
		observed.Current = !v.Closing && pvc.GetName() == currentName && pvc.GetDeletionTimestamp() == nil
		states[i] = observed
	}
	names := make([]string, 0, len(candidates))
	for name := range candidates {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		pvc := candidates[name]
		if known[string(pvc.GetUID())] {
			continue
		}
		observed, err := n.observeBackendPVC(ctx, v, pvc, biz.BackendState{})
		if err != nil {
			return states, err
		}
		observed.Current = !v.Closing && pvc.GetName() == currentName
		states = append(states, observed)
	}
	if !v.Closing && running && biz.NeedsBackendState(v.Spec) {
		ready := false
		for _, state := range states {
			if state.Current && state.Present && state.Phase == "Bound" {
				ready = true
			}
		}
		if !ready {
			return states, biz.ErrDependency
		}
	}
	return states, nil
}

// This only confirms native GC/restore cleanup. It never deletes a PVC/PV or
// weakens its finalizers, and does not claim CSI backend reclamation from HTTP.
func (n *Native) BackendStateRemoved(ctx context.Context, v biz.VM, state biz.BackendState) (bool, error) {
	if state.Owner != v.Native || state.PVC.Kind != "PersistentVolumeClaim" || state.PVC.Name == "" || state.PVC.Namespace != v.Spec.Namespace || v.Native.UID == "" {
		return false, biz.ErrOwnership
	}
	pvc, err := n.client.Resource(pvcResource).Namespace(state.PVC.Namespace).Get(ctx, state.PVC.Name, metav1.GetOptions{})
	if err == nil {
		if (state.PVC.UID != "" && objectRef(pvc) != state.PVC) || !backendOwned(pvc, v) {
			return false, biz.ErrOwnership
		}
		return false, nil
	}
	if !apierrors.IsNotFound(err) {
		return false, err
	}
	return true, nil
}
