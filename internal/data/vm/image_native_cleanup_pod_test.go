package vm

import (
	"context"
	"testing"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
)

// The upload server is a CDI-owned Pod. When an upload fails it exits Failed and is
// left behind; the DataVolume then sits in foregroundDeletion waiting for a dependent
// that never goes away, and pvc-protection keeps the prime PVC Terminating forever.
// This is component fault maintenance; regular Resource workers retain the
// native incomplete result and leave the limited handoff to operations.

const (
	uploadPodLabel = "cdi-upload-server"
	cleanupTenant  = "64c5c35a-f554-432c-b4bc-7b4f36f08420"
	cleanupImageID = "c1d0f0a1-1111-4222-8333-444455556666"
	cleanupPVCUID  = "pvc-uid-cleanup"
)

func cleanupRecord() biz.Image {
	i := biz.Image{TenantID: cleanupTenant, ID: cleanupImageID, Kind: "DISK_IMAGE", State: biz.StateDeleting,
		CleanupState: biz.CleanupRunning}
	i.Native = biz.ImageNative{Namespace: "tenant-" + cleanupTenant, ClusterID: "cluster-a",
		DVName: imageName(i), DVUID: "dv-uid-cleanup",
		PVCName: imageName(i), PVCUID: cleanupPVCUID, SourceVolumeMode: "Block"}
	// The probe found the populator PVCs; they are what cleanup is draining now.
	i.Cleanup = []biz.ObjectRef{
		{Kind: "PersistentVolumeClaim", Namespace: i.Native.Namespace, Name: "prime-" + cleanupPVCUID, UID: "prime-uid"},
		{Kind: "PersistentVolumeClaim", Namespace: i.Native.Namespace, Name: "prime-" + cleanupPVCUID + "-scratch", UID: "scratch-uid"},
	}
	return i
}

func uploadPod(namespace, primePVC string, phase string, uid string) *unstructured.Unstructured {
	pod := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{
			"name": "cdi-upload-" + primePVC, "namespace": namespace, "uid": uid,
			"labels": map[string]any{
				"cdi.kubevirt.io":              uploadPodLabel,
				"service":                      "cdi-upload-" + primePVC,
				"app.kubernetes.io/managed-by": "cdi-controller",
			},
		},
		"status": map[string]any{"phase": phase},
	}}
	pod.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: primePVC, UID: "prime-uid", Controller: boolPtr(true),
	}})
	return pod
}

func cleanupCluster(t *testing.T, i biz.Image, extra ...runtime.Object) *fake.FakeDynamicClient {
	t.Helper()
	dv := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cdi.kubevirt.io/v1beta1", "kind": "DataVolume",
		"metadata": map[string]any{"name": i.Native.DVName, "namespace": i.Native.Namespace, "uid": i.Native.DVUID,
			"deletionTimestamp": "2026-11-05T06:21:25Z", "finalizers": []any{"foregroundDeletion"},
			"labels": map[string]any{"vm.ani.io/image-id": i.ID, "ani.io/tenant-id": i.TenantID, "vm.ani.io/managed-by": biz.Owner}},
	}}
	prime := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "PersistentVolumeClaim",
		"metadata": map[string]any{"name": "prime-" + cleanupPVCUID, "namespace": i.Native.Namespace, "uid": "prime-uid",
			"deletionTimestamp": "2026-11-05T06:21:25Z", "finalizers": []any{"kubernetes.io/pvc-protection", "foregroundDeletion"}},
	}}
	objects := append([]runtime.Object{dv, prime}, extra...)
	return fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		dvResource: "DataVolumeList", pvcResource: "PersistentVolumeClaimList",
		pvResource: "PersistentVolumeList", podResource: "PodList"}, objects...)
}

func TestDeleteImageObjectsLeavesAllCDIUploadersToOperations(t *testing.T) {
	for _, phase := range []string{"Failed", "Running", "Succeeded"} {
		t.Run(phase, func(t *testing.T) {
			i := cleanupRecord()
			pod := uploadPod(i.Native.Namespace, "prime-"+cleanupPVCUID, phase, "pod-uid")
			pod.SetFinalizers([]string{"foregroundDeletion", "example.com/cleanup"})
			cluster := cleanupCluster(t, i, pod)
			n := &ImageNative{client: cluster}
			out, done, err := n.DeleteImageObjects(context.Background(), i)
			if err != nil || done {
				t.Fatal(out, done, err)
			}
			for _, a := range cluster.Actions() {
				if a.GetResource() == podResource || (a.GetResource() == pvcResource && a.(interface{ GetName() string }).GetName() != i.Native.PVCName) {
					t.Fatal("regular worker touched CDI internal objects", a)
				}
			}
			stored, err := cluster.Resource(podResource).Namespace(i.Native.Namespace).Get(context.Background(), pod.GetName(), metav1.GetOptions{})
			if err != nil || !sameStrings(stored.GetFinalizers(), pod.GetFinalizers()) {
				t.Fatal("uploader finalizers changed", err)
			}
			if len(out.Cleanup) != len(i.Cleanup) {
				t.Fatal("legacy handoff references discarded")
			}
		})
	}
}

func boolPtr(b bool) *bool { return &b }

func sameStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}
