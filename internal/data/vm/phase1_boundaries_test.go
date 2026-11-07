package vm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

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

func TestPhase1PowerSubmissionsIgnoreStorageAndPreserveUID(t *testing.T) {
	_, v, _, original := nativeSnapshotFixture(t)
	ns, _ := original.Resource(nsResource).Get(t.Context(), v.Spec.Namespace, metav1.GetOptions{})
	root, _ := original.Resource(vmResource).Namespace(v.Spec.Namespace).Get(t.Context(), v.Native.Name, metav1.GetOptions{})
	client := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{podResource: "PodList"}, ns, root)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PUT" {
			t.Errorf("unexpected method %s", r.Method)
		}
		requests++
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	native, err := NewNative(client, &rest.Config{Host: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	client.PrependReactor("*", "*", func(a ktesting.Action) (bool, runtime.Object, error) {
		switch a.GetResource().Resource {
		case "datavolumes", "persistentvolumeclaims", "persistentvolumes", "kubevirts", "storageclasses":
			return true, nil, errors.New("unrelated storage unavailable")
		}
		return false, nil, nil
	})
	// Existing backend identity cannot be invalidated by a new deployment default.
	v.Spec.Media.PersistentTPM = true
	v.Spec.BackendStorageClass = "old-frozen-class"
	for _, action := range []func() error{
		func() error { return native.Stop(t.Context(), v, false) },
		func() error { return native.Stop(t.Context(), v, true) },
		func() error { return native.Restart(t.Context(), v) },
		func() error { return native.Start(t.Context(), v) },
	} {
		if err := action(); err != nil {
			t.Fatal(err)
		}
	}
	if requests != 4 {
		t.Fatal("power request not submitted", requests)
	}
	if x, err := native.ObserveLifecycle(t.Context(), v); err != nil || x.Deleted {
		t.Fatal(x, err)
	}
	wrong := v
	wrong.Native.UID = "replaced"
	if err := native.Stop(t.Context(), wrong, false); !errors.Is(err, biz.ErrOwnership) {
		t.Fatal("wrong VM UID accepted", err)
	}
	if requests != 4 {
		t.Fatal("wrong UID submitted")
	}
	if err := native.Delete(t.Context(), v); err != nil {
		t.Fatal("root delete depended on storage", err)
	}
}

func TestPhase1SnapshotReplayAndReadyUseOnlyNativeObject(t *testing.T) {
	native, v, s, client := nativeSnapshotFixture(t)
	client.PrependReactor("*", "*", func(a ktesting.Action) (bool, runtime.Object, error) {
		switch a.GetResource().Resource {
		case "virtualmachinesnapshotcontents", "volumesnapshots", "volumesnapshotcontents", "storageprofiles", "volumesnapshotclasses", "storageclasses":
			return true, nil, errors.New("lower storage unavailable")
		}
		return false, nil, nil
	})
	s.Content = biz.ObjectRef{}
	again, err := native.EnsureSnapshot(t.Context(), v, s)
	if err != nil || !again.Complete || again.Native != s.Native {
		t.Fatal(again, err)
	}
	changed := s
	changed.Native.UID = "replacement"
	if _, err := native.EnsureSnapshot(t.Context(), v, changed); !errors.Is(err, biz.ErrOwnership) {
		t.Fatal("snapshot replacement accepted", err)
	}
}

func TestPhase1CDICompletionDoesNotReadTargetPVC(t *testing.T) {
	for _, phase := range []string{"Succeeded", "Pending"} {
		t.Run(phase, func(t *testing.T) {
			_, v, _, _ := nativeSnapshotFixture(t)
			d := v.Disks[0]
			dv := snapshotObject("cdi.kubevirt.io/v1beta1", "DataVolume", d.DV.Name, v.Spec.Namespace,
				map[string]any{"spec": map[string]any{"source": map[string]any{"pvc": map[string]any{"namespace": "source-ns", "name": "source-pvc"}}}, "status": map[string]any{"phase": phase}})
			dv.SetUID(types.UID(d.DV.UID))
			dv.SetLabels(labelVM(v))
			dv.SetOwnerReferences(ownReference(v))
			client := fake.NewSimpleDynamicClient(runtime.NewScheme(), dv)
			client.PrependReactor("*", "persistentvolumeclaims", func(ktesting.Action) (bool, runtime.Object, error) {
				t.Fatal("source pin settlement read target PVC")
				return true, nil, biz.ErrDependency
			})
			native := &ImageNative{client: client}
			image := biz.Image{Native: biz.ImageNative{Namespace: "source-ns", PVCName: "source-pvc"}}
			ref := biz.ImageReference{ConsumerKind: "SYSTEM_DISK", ConsumerID: d.ID}
			got, err := native.ObserveImageClone(t.Context(), v, image, ref)
			want := biz.ClonePending
			if phase == "Succeeded" {
				want = biz.CloneCompleted
			}
			if err != nil || got != want {
				t.Fatal(got, err)
			}
			v.Disks[0].DV.UID = "foreign-uid"
			if _, err := native.ObserveImageClone(t.Context(), v, image, ref); !errors.Is(err, biz.ErrOwnership) {
				t.Fatal("foreign DV released source", err)
			}
		})
	}
}

func TestPhase1ExistingImageSurvivesNewDefaultStorageClass(t *testing.T) {
	image := verifierRecord()
	image.Kind = "DISK_IMAGE"
	image.SourceRequestedBytes = 1 << 30
	pvc := snapshotObject("v1", "PersistentVolumeClaim", image.Native.PVCName, image.Native.Namespace,
		map[string]any{"spec": map[string]any{"volumeMode": "Block", "storageClassName": "old-class"}, "status": map[string]any{"phase": "Bound", "capacity": map[string]any{"storage": "1Gi"}}})
	pvc.SetUID(types.UID(image.Native.PVCUID))
	pvc.SetLabels(imageLabels(image))
	client := fake.NewSimpleDynamicClient(runtime.NewScheme(), pvc)
	native := &ImageNative{client: client, config: ImageUploadConfig{StorageClass: "new-upload-class"}}
	if _, err := native.ObserveImageSourceIdentity(t.Context(), image); err != nil {
		t.Fatal(err)
	}
	pvc.SetUID("replacement")
	_, _ = client.Resource(pvcResource).Namespace(pvc.GetNamespace()).Update(t.Context(), pvc, metav1.UpdateOptions{})
	if _, err := native.ObserveImageSourceIdentity(t.Context(), image); !errors.Is(err, biz.ErrOwnership) {
		t.Fatal("UID replacement accepted", err)
	}
}

func TestPhase1RestoreKeepsNamedPendingOutputsWithoutScanning(t *testing.T) {
	native, v, s, client := nativeSnapshotFixture(t)
	ref := biz.ObjectRef{Kind: "VirtualMachineRestore", Namespace: v.Spec.Namespace, Name: "restore-named"}
	first, err := native.EnsureRestore(t.Context(), v, s, ref)
	if err != nil {
		t.Fatal(err)
	}
	object, _ := client.Resource(restoreResource).Namespace(v.Spec.Namespace).Get(t.Context(), ref.Name, metav1.GetOptions{})
	outputs := []any{}
	for _, d := range s.Inventory {
		outputs = append(outputs, map[string]any{"volumeName": d.VolumeName, "persistentVolumeClaim": "not-created-" + d.ID})
	}
	_ = unstructured.SetNestedSlice(object.Object, outputs, "status", "restores")
	_, _ = client.Resource(restoreResource).Namespace(v.Spec.Namespace).Update(t.Context(), object, metav1.UpdateOptions{})
	client.PrependReactor("*", "*", func(a ktesting.Action) (bool, runtime.Object, error) {
		if a.GetResource() == pvResource || a.GetResource() == volumeSnapshotResource || (a.GetResource() == pvcResource && a.GetVerb() == "list") {
			t.Fatal("restore scanned storage internals", a)
		}
		return false, nil, nil
	})
	observed, err := native.ObserveRestore(t.Context(), v, s, first.Native)
	if err != nil || !observed.OutputsKnown || len(observed.Disks) != len(s.Inventory) {
		t.Fatal(observed, err)
	}
	for _, d := range observed.Disks {
		if d.PVC.UID != "" || d.PVC.Name == "" {
			t.Fatal("uncreated output name lost", d)
		}
	}
	// Losing the CR before checkpoint cannot be interpreted as empty output.
	_ = client.Resource(restoreResource).Namespace(ref.Namespace).Delete(t.Context(), ref.Name, metav1.DeleteOptions{})
	if _, err := native.ObserveRestore(t.Context(), v, s, first.Native); !errors.Is(err, biz.ErrUnknown) {
		t.Fatal("missing output carrier inferred success", err)
	}
}

func TestPhase1MixedVolumeCleanupRetainsBusinessResponsibilities(t *testing.T) {
	native, v, s, client := nativeSnapshotFixture(t)
	d := s.Inventory[0]
	_ = client.Resource(dvResource).Namespace(v.Spec.Namespace).Delete(t.Context(), d.DV.Name, metav1.DeleteOptions{})
	_ = client.Resource(pvcResource).Namespace(v.Spec.Namespace).Delete(t.Context(), d.PVC.Name, metav1.DeleteOptions{})
	job := snapshotObject("batch/v1", "Job", "owned-cleanup-job", v.Spec.Namespace, nil)
	_, _ = client.Resource(jobResource).Namespace(v.Spec.Namespace).Create(t.Context(), job, metav1.CreateOptions{})
	// The fake reactor assigns actual UIDs on create.
	job, _ = client.Resource(jobResource).Namespace(v.Spec.Namespace).Get(t.Context(), job.GetName(), metav1.GetOptions{})
	d.Cleanup = []biz.ObjectRef{{Kind: "PersistentVolume", Name: "old-pv", UID: "pv-uid"}, objectRef(job)}
	if done, err := native.DeleteVolume(context.Background(), v, d); err != nil || done {
		t.Fatal("live business cleanup ignored", done, err)
	}
	if done, err := native.DeleteVolume(context.Background(), v, d); err != nil || !done {
		t.Fatal("PV diagnostic blocked completed cleanup", done, err)
	}

}

func TestPhase1LifecycleRefreshesRuntimeMediaWithoutStorageReads(t *testing.T) {
	_, v, original := mediaNativeFixture(t)
	ns, _ := original.Resource(nsResource).Get(t.Context(), v.Spec.Namespace, metav1.GetOptions{})
	originalRoot, _ := original.Resource(vmResource).Namespace(v.Spec.Namespace).Get(t.Context(), v.Native.Name, metav1.GetOptions{})
	client := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{podResource: "PodList"}, ns, originalRoot)
	native := NewNativeWithClients(client, nil)
	root, err := client.Resource(vmResource).Namespace(v.Spec.Namespace).Get(t.Context(), v.Native.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	volumes, disks, err := mediaLists(root, "spec", "template", "spec")
	if err != nil {
		t.Fatal(err)
	}
	volumes, disks, err = native.MediaTemplate(v, nil, volumes, disks)
	if err != nil {
		t.Fatal(err)
	}
	if err = unstructured.SetNestedSlice(root.Object, volumes, "spec", "template", "spec", "volumes"); err != nil {
		t.Fatal(err)
	}
	if err = unstructured.SetNestedSlice(root.Object, disks, "spec", "template", "spec", "domain", "devices", "disks"); err != nil {
		t.Fatal(err)
	}
	if _, err = client.Resource(vmResource).Namespace(v.Spec.Namespace).Update(t.Context(), root, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	spec, _, _ := unstructured.NestedMap(root.Object, "spec", "template", "spec")
	instance := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "kubevirt.io/v1", "kind": "VirtualMachineInstance", "metadata": map[string]any{"name": v.Native.Name, "namespace": v.Spec.Namespace, "uid": "current-vmi"}, "spec": spec, "status": map[string]any{"phase": "Running"}}}
	controller := true
	instance.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "kubevirt.io/v1", Kind: "VirtualMachine", Name: v.Native.Name, UID: types.UID(v.Native.UID), Controller: &controller}})
	if _, err = client.Resource(vmiResource).Namespace(v.Spec.Namespace).Create(t.Context(), instance, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	client.PrependReactor("list", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, &unstructured.UnstructuredList{}, nil
	})
	storageReads := 0
	client.PrependReactor("*", "*", func(a ktesting.Action) (bool, runtime.Object, error) {
		switch a.GetResource().Resource {
		case "datavolumes", "persistentvolumeclaims", "persistentvolumes", "storageclasses":
			storageReads++
			return true, nil, errors.New("storage must not be read")
		}
		return false, nil, nil
	})
	observed, err := native.ObserveLifecycle(t.Context(), v)
	if err != nil || !observed.Running || len(observed.MediaSlots) != 2 || !observed.MediaSlots[0].EffectiveNow || observed.MediaSlots[0].PendingRestart || observed.MediaSlots[0].ObservedCopyID != v.MediaSlots[0].DesiredCopyID {
		t.Fatal("runtime media did not refresh independently of storage", observed, err)
	}
	if storageReads != 0 {
		t.Fatal("lifecycle read storage", storageReads)
	}
	// An invalid, unrelated media projection still cannot block lifecycle.
	v.MediaSlots[0].DesiredCopyID = "unknown-copy"
	if observed, err = native.ObserveLifecycle(t.Context(), v); err != nil || !observed.Running {
		t.Fatal(observed, err)
	}
}
