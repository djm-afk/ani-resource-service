package vm

import (
	"context"
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
)

type backendFixture struct {
	v                     biz.VM
	n                     *Native
	c                     *fake.FakeDynamicClient
	pvc, pv, root, kv, sc *unstructured.Unstructured
}

func newBackendFixture(t *testing.T) backendFixture {
	t.Helper()
	v := biz.VM{TenantID: uuid.NewString(), ID: uuid.NewString(), Spec: biz.Spec{Namespace: "backend-test", NamespaceUID: uuid.NewString(), BackendStorageClass: "backend-fs", Digest: strings.Repeat("a", 64), Media: biz.Source{Firmware: "uefi", PersistentTPM: true, PersistentUEFI: true, Namespace: "missing-admin-source", PVCName: "source-no-longer-exists"}}}
	v.Native = biz.ObjectRef{Kind: "VirtualMachine", Namespace: v.Spec.Namespace, Name: "vm-" + v.ID, UID: uuid.NewString()}
	ns := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": v.Spec.Namespace, "uid": v.Spec.NamespaceUID, "labels": map[string]any{"network.ani.io/tenant-id": v.TenantID, "network.ani.io/managed-by": "ani-network-service"}}}}
	domain := map[string]any{"firmware": map[string]any{"uuid": v.ID, "bootloader": map[string]any{"efi": map[string]any{"secureBoot": false}}}, "devices": map[string]any{"disks": []any{map[string]any{"name": "root", "disk": map[string]any{"bus": "virtio"}}}}}
	if err := (&Native{}).ConfigureBackendDomain(v, domain); err != nil {
		t.Fatal(err)
	}
	root := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "kubevirt.io/v1", "kind": "VirtualMachine", "metadata": map[string]any{"name": v.Native.Name, "namespace": v.Spec.Namespace, "uid": v.Native.UID}, "spec": map[string]any{"template": map[string]any{"spec": map[string]any{"domain": domain}}}}}
	root.SetLabels(labelVM(v))
	pvc := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": map[string]any{"name": "native-generated-random-suffix", "namespace": v.Spec.Namespace, "uid": uuid.NewString(), "labels": map[string]any{backendStateLabel: v.Native.Name}}, "spec": map[string]any{"storageClassName": "backend-fs", "volumeMode": "Filesystem", "volumeName": "native-generated-pv", "resources": map[string]any{"requests": map[string]any{"storage": "10Mi"}}}, "status": map[string]any{"phase": "Bound", "capacity": map[string]any{"storage": "16Mi"}}}}
	pvc.SetOwnerReferences(ownReference(v))
	pv := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "PersistentVolume", "metadata": map[string]any{"name": "native-generated-pv", "uid": uuid.NewString()}, "spec": map[string]any{"storageClassName": "backend-fs", "volumeMode": "Filesystem", "persistentVolumeReclaimPolicy": "Delete", "claimRef": map[string]any{"name": pvc.GetName(), "namespace": v.Spec.Namespace, "uid": string(pvc.GetUID())}, "csi": map[string]any{"driver": "rook-ceph.cephfs.csi.ceph.com", "volumeHandle": "native-backend-handle"}}}}
	kv := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "kubevirt.io/v1", "kind": "KubeVirt", "metadata": map[string]any{"name": "kubevirt", "namespace": "kubevirt", "uid": uuid.NewString()}, "spec": map[string]any{"configuration": map[string]any{"vmStateStorageClass": "backend-fs", "developerConfiguration": map[string]any{}}}, "status": map[string]any{"observedKubeVirtVersion": "v1.9.0", "phase": "Deployed"}}}
	sc := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "storage.k8s.io/v1", "kind": "StorageClass", "metadata": map[string]any{"name": "backend-fs", "uid": uuid.NewString()}, "reclaimPolicy": "Delete"}}
	c := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{pvcResource: "PersistentVolumeClaimList", pvResource: "PersistentVolumeList", kubevirtResource: "KubeVirtList"}, ns, root, pvc, pv, kv, sc)
	return backendFixture{v: v, n: NewNativeWithClients(c, nil), c: c, pvc: pvc, pv: pv, root: root, kv: kv, sc: sc}
}

func backendReadsOnly(t *testing.T, f backendFixture, from int) {
	t.Helper()
	for _, action := range f.c.Actions()[from:] {
		if action.GetVerb() != "get" && action.GetVerb() != "list" {
			t.Fatal("backend helper mutated native resources", action.GetVerb(), action.GetResource())
		}
	}
}

func TestBackendFrozenDevicesAndConfiguration(t *testing.T) {
	ctx := context.Background()
	f := newBackendFixture(t)
	domain, _, _ := unstructured.NestedMap(f.root.Object, "spec", "template", "spec", "domain")
	if !backendDomainMatches(f.root, f.v, false) {
		t.Fatal("persistent TPM/EFI missing")
	}
	secure, _, _ := unstructured.NestedBool(domain, "firmware", "bootloader", "efi", "secureBoot")
	firmware, _, _ := unstructured.NestedString(domain, "firmware", "uuid")
	disks, _, _ := unstructured.NestedSlice(domain, "devices", "disks")
	if secure || firmware != f.v.ID || len(disks) != 1 {
		t.Fatal("backend projection overwrote other frozen devices")
	}
	if err := f.n.ValidateBackendConfiguration(ctx, f.v.Spec); err != nil {
		t.Fatal("v1.9 GA persistent state with exact configured class rejected", err)
	}
	for _, tc := range []struct {
		name, class, policy string
		want                error
	}{{"default-is-not-frozen", "", "Delete", biz.ErrDependency}, {"class-drift", "another", "Delete", biz.ErrChanged}, {"retain", "backend-fs", "Retain", biz.ErrDependency}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newBackendFixture(t)
			_ = unstructured.SetNestedField(f.kv.Object, tc.class, "spec", "configuration", "vmStateStorageClass")
			_, _ = f.c.Resource(kubevirtResource).Namespace("kubevirt").Update(ctx, f.kv, metav1.UpdateOptions{})
			f.sc.Object["reclaimPolicy"] = tc.policy
			_, _ = f.c.Resource(backendStorageClasses).Update(ctx, f.sc, metav1.UpdateOptions{})
			from := len(f.c.Actions())
			if err := f.n.ValidateBackendConfiguration(ctx, f.v.Spec); err != tc.want {
				t.Fatalf("got %v want %v", err, tc.want)
			}
			backendReadsOnly(t, f, from)
		})
	}
	f.v.Spec.Media.Firmware = "bios"
	if err := f.n.ConfigureBackendDomain(f.v, map[string]any{}); err != biz.ErrInvalid {
		t.Fatal("persistent EFI silently downgraded", err)
	}
}

func TestBackendNativeGeneratedIdentityAndStoppedRetention(t *testing.T) {
	ctx := context.Background()
	f := newBackendFixture(t)
	from := len(f.c.Actions())
	states, err := f.n.ObserveBackendState(ctx, f.v, nil)
	if err != nil || len(states) != 1 {
		t.Fatal(states, err)
	}
	state := states[0]
	if state.PVC.UID != string(f.pvc.GetUID()) || state.Owner != f.v.Native || !state.Current || !state.Present || state.RequestedBytes != 10<<20 || state.CapacityBytes != 16<<20 || state.VolumeName != backendStateLabel+"-"+f.v.Native.Name {
		t.Fatal("actual generated identity/capacity was not frozen", state)
	}
	states, err = f.n.ObserveBackendState(ctx, f.v, states)
	if err != nil || len(states) != 1 || states[0] != state {
		t.Fatal("stopped VM lost its backend state", states, err)
	}
	backendReadsOnly(t, f, from)
}

func TestBackendRejectsForeignPVCAndMode(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(backendFixture)
	}{
		{"owner-uid", func(f backendFixture) {
			owners := f.pvc.GetOwnerReferences()
			owners[0].UID = types.UID(uuid.NewString())
			f.pvc.SetOwnerReferences(owners)
		}},
		{"vmi-owner-is-not-persistent-vm-owner", func(f backendFixture) {
			owners := f.pvc.GetOwnerReferences()
			owners[0].Kind = "VirtualMachineInstance"
			f.pvc.SetOwnerReferences(owners)
		}},
		{"block-backend", func(f backendFixture) { _ = unstructured.SetNestedField(f.pvc.Object, "Block", "spec", "volumeMode") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newBackendFixture(t)
			tc.mutate(f)
			_, _ = f.c.Resource(pvcResource).Namespace(f.v.Spec.Namespace).Update(ctx, f.pvc, metav1.UpdateOptions{})
			_, _ = f.c.Resource(pvResource).Update(ctx, f.pv, metav1.UpdateOptions{})
			from := len(f.c.Actions())
			if _, err := f.n.ObserveBackendState(ctx, f.v, nil); err == nil {
				t.Fatal("foreign/drifted backend was adopted")
			}
			f.v.Closing = true
			if _, err := f.n.ObserveBackendState(ctx, f.v, nil); err == nil {
				t.Fatal("closing bypassed backend owner/mode identity fencing")
			}
			backendReadsOnly(t, f, from)
		})
	}
}

func TestBackendClosingRegistersTerminatingVolumeWithoutReadiness(t *testing.T) {
	ctx := context.Background()
	f := newBackendFixture(t)
	spec, _, _ := unstructured.NestedMap(f.root.Object, "spec", "template", "spec")
	vmi := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "kubevirt.io/v1", "kind": "VirtualMachineInstance", "metadata": map[string]any{"name": f.v.Native.Name, "namespace": f.v.Spec.Namespace, "uid": uuid.NewString()}, "spec": spec, "status": map[string]any{"phase": "Running", "volumeStatus": []any{map[string]any{"name": "persistent-state-for-this-vm"}}}}}
	vmi.SetOwnerReferences(ownReference(f.v))
	_, _ = f.c.Resource(vmiResource).Namespace(f.v.Spec.Namespace).Create(ctx, vmi, metav1.CreateOptions{})
	stamp := metav1.Now()
	f.pvc.SetDeletionTimestamp(&stamp)
	_, _ = f.c.Resource(pvcResource).Namespace(f.v.Spec.Namespace).Update(ctx, f.pvc, metav1.UpdateOptions{})
	f.v.Closing = true
	from := len(f.c.Actions())
	states, err := f.n.ObserveBackendState(ctx, f.v, nil)
	if err != nil || len(states) != 1 || states[0].Current || !states[0].Present || states[0].PVC.UID != string(f.pvc.GetUID()) {
		t.Fatal("backend readiness blocked root deletion or lost cleanup identity", states, err)
	}
	if gone, err := f.n.BackendStateRemoved(ctx, f.v, states[0]); err != nil || gone {
		t.Fatal("terminating backend was considered reclaimed", gone, err)
	}
	backendReadsOnly(t, f, from)
}

func TestBackendRunningRequiresNativeVolumeStatus(t *testing.T) {
	ctx := context.Background()
	f := newBackendFixture(t)
	spec, _, _ := unstructured.NestedMap(f.root.Object, "spec", "template", "spec")
	vmi := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "kubevirt.io/v1", "kind": "VirtualMachineInstance", "metadata": map[string]any{"name": f.v.Native.Name, "namespace": f.v.Spec.Namespace, "uid": uuid.NewString()}, "spec": spec, "status": map[string]any{"phase": "Running"}}}
	vmi.SetOwnerReferences(ownReference(f.v))
	_, _ = f.c.Resource(vmiResource).Namespace(f.v.Spec.Namespace).Create(ctx, vmi, metav1.CreateOptions{})
	if _, err := f.n.ObserveBackendState(ctx, f.v, nil); err != biz.ErrDependency {
		t.Fatal("Running without backend volumeStatus passed", err)
	}
	_ = unstructured.SetNestedSlice(vmi.Object, []any{map[string]any{"name": "persistent-state-for-this-vm", "persistentVolumeClaimInfo": map[string]any{"claimName": f.pvc.GetName()}}}, "status", "volumeStatus")
	_, _ = f.c.Resource(vmiResource).Namespace(f.v.Spec.Namespace).Update(ctx, vmi, metav1.UpdateOptions{})
	if states, err := f.n.ObserveBackendState(ctx, f.v, nil); err != nil || len(states) != 1 || !states[0].Current {
		t.Fatal("native current backend identity rejected", states, err)
	}
	_ = unstructured.SetNestedSlice(vmi.Object, []any{map[string]any{"name": "persistent-state-for-this-vm", "persistentVolumeClaimInfo": map[string]any{"claimName": "another-vms-backend"}}}, "status", "volumeStatus")
	_, _ = f.c.Resource(vmiResource).Namespace(f.v.Spec.Namespace).Update(ctx, vmi, metav1.UpdateOptions{})
	if _, err := f.n.ObserveBackendState(ctx, f.v, nil); err != biz.ErrDependency {
		t.Fatal("VMI status selected an unproven backend", err)
	}
}

func TestBackendRestoreKeepsOldUIDsUntilNativeReclamation(t *testing.T) {
	ctx := context.Background()
	f := newBackendFixture(t)
	states, err := f.n.ObserveBackendState(ctx, f.v, nil)
	if err != nil {
		t.Fatal(err)
	}
	old := states[0]
	// Native Restore removes the old backend label and creates an owned,
	// randomized PVC. The Resource observer must not orchestrate either step.
	f.pvc.SetLabels(map[string]string{"restore.kubevirt.io/cleanup-backend-pvc": old.VolumeName})
	_, _ = f.c.Resource(pvcResource).Namespace(f.v.Spec.Namespace).Update(ctx, f.pvc, metav1.UpdateOptions{})
	newPVC := f.pvc.DeepCopy()
	newPVC.SetName("restore-random-backend")
	newPVC.SetUID(types.UID(uuid.NewString()))
	newPVC.SetLabels(map[string]string{backendStateLabel: f.v.Native.Name})
	_ = unstructured.SetNestedField(newPVC.Object, "restore-random-pv", "spec", "volumeName")
	newPV := f.pv.DeepCopy()
	newPV.SetName("restore-random-pv")
	newPV.SetUID(types.UID(uuid.NewString()))
	_ = unstructured.SetNestedField(newPV.Object, newPVC.GetName(), "spec", "claimRef", "name")
	_ = unstructured.SetNestedField(newPV.Object, string(newPVC.GetUID()), "spec", "claimRef", "uid")
	_ = unstructured.SetNestedField(newPV.Object, "restore-backend-handle", "spec", "csi", "volumeHandle")
	_, _ = f.c.Resource(pvcResource).Namespace(f.v.Spec.Namespace).Create(ctx, newPVC, metav1.CreateOptions{})
	_, _ = f.c.Resource(pvResource).Create(ctx, newPV, metav1.CreateOptions{})
	from := len(f.c.Actions())
	states, err = f.n.ObserveBackendState(ctx, f.v, states)
	if err != nil || len(states) != 2 || states[0].Current || !states[0].Present || !states[1].Current || states[1].PVC.UID != string(newPVC.GetUID()) || states[0].PV != old.PV {
		t.Fatal("restore replaced the old cleanup identities", states, err)
	}
	backendReadsOnly(t, f, from)
	_ = f.c.Resource(pvcResource).Namespace(f.v.Spec.Namespace).Delete(ctx, old.PVC.Name, metav1.DeleteOptions{})
	from = len(f.c.Actions())
	if gone, err := f.n.BackendStateRemoved(ctx, f.v, old); err != nil || !gone {
		t.Fatal("legacy PV blocked business PVC completion", gone, err)
	}
	backendReadsOnly(t, f, from)
	_ = f.c.Resource(pvResource).Delete(ctx, f.pv.GetName(), metav1.DeleteOptions{})
	from = len(f.c.Actions())
	states, err = f.n.ObserveBackendState(ctx, f.v, states)
	if err != nil || len(states) != 2 || states[0].Current || states[0].Present || states[0].PVC != old.PVC || states[0].PV != old.PV {
		t.Fatal("retired backend history disappeared", states, err)
	}
	if gone, err := f.n.BackendStateRemoved(ctx, f.v, old); err != nil || !gone {
		t.Fatal("native reclamation not confirmed", gone, err)
	}
	backendReadsOnly(t, f, from)
}

func TestBackendCleanupIgnoresLatePVAndCurrentDefaults(t *testing.T) {
	ctx := context.Background()
	f := newBackendFixture(t)
	states, err := f.n.ObserveBackendState(ctx, f.v, nil)
	if err != nil {
		t.Fatal(err)
	}
	state := states[0]
	_ = f.c.Resource(pvcResource).Namespace(f.v.Spec.Namespace).Delete(ctx, state.PVC.Name, metav1.DeleteOptions{})
	_ = f.c.Resource(backendStorageClasses).Delete(ctx, "backend-fs", metav1.DeleteOptions{})
	state.PV = biz.ObjectRef{}
	from := len(f.c.Actions())
	if gone, err := f.n.BackendStateRemoved(ctx, f.v, state); err != nil || !gone {
		t.Fatal("late PV became a business cleanup gate", gone, err)
	}
	backendReadsOnly(t, f, from)
	f.v.Closing = true
	from = len(f.c.Actions())
	late, err := f.n.ObserveBackendState(ctx, f.v, []biz.BackendState{state})
	if err != nil || len(late) != 1 || late[0].PV.UID != "" || late[0].Current || late[0].Present {
		t.Fatal("unexpected late PV discovery during business cleanup", late, err)
	}
	backendReadsOnly(t, f, from)
	_ = f.c.Resource(pvResource).Delete(ctx, f.pv.GetName(), metav1.DeleteOptions{})
	from = len(f.c.Actions())
	if gone, err := f.n.BackendStateRemoved(ctx, f.v, state); err != nil || !gone {
		t.Fatal("cleanup incorrectly needed a current class/source", gone, err)
	}
	backendReadsOnly(t, f, from)
}
