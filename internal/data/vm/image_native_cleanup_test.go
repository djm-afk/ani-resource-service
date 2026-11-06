package vm

import (
	"context"
	"testing"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
)

// TestDeleteImageObjectsRemovesTheDataVolumeBeforeItsChildren covers the shape the
// cluster actually had: a live DataVolume whose populator keeps rebuilding child PVCs
// with new UIDs, while the record's chain still holds the UIDs observed earlier. The
// chain must not be read as a foreign occupant, and the DataVolume must go first.
func TestDeleteImageObjectsRemovesTheDataVolumeBeforeItsChildren(t *testing.T) {
	ctx := context.Background()
	i := biz.Image{ID: "2b1f0c2e-6d3a-4f57-9a10-3f7f5c1d8a11", TenantID: "64c5c35a-0000-4000-8000-000000000001",
		State: biz.StateDeleting, CleanupState: biz.CleanupRunning}
	i.Native = biz.ImageNative{Namespace: "tenant-a", DVName: "vmi-src-2b1f0c2e6d3a4f579a103f7f5c1d8a11",
		DVUID: "dv-uid", PVCName: "vmi-src-2b1f0c2e6d3a4f579a103f7f5c1d8a11", PVCUID: "parent-pvc-uid"}
	i.Cleanup = []biz.ObjectRef{
		{Kind: "PersistentVolumeClaim", Namespace: i.Native.Namespace, Name: "prime-parent-pvc-uid", UID: "stale-child-uid"},
		{Kind: "PersistentVolumeClaim", Namespace: i.Native.Namespace, Name: i.Native.PVCName, UID: "parent-pvc-uid"},
	}
	name := i.Native.DVName
	labels := map[string]any{"vm.ani.io/image-id": i.ID, "ani.io/tenant-id": i.TenantID, "vm.ani.io/managed-by": biz.Owner}
	dv := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "cdi.kubevirt.io/v1beta1", "kind": "DataVolume",
		"metadata": map[string]any{"name": name, "namespace": i.Native.Namespace, "uid": "dv-uid", "labels": labels}}}
	parent := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "PersistentVolumeClaim",
		"metadata": map[string]any{"name": name, "namespace": i.Native.Namespace, "uid": "parent-pvc-uid"}}}
	child := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "PersistentVolumeClaim",
		"metadata": map[string]any{"name": "prime-parent-pvc-uid", "namespace": i.Native.Namespace, "uid": "rebuilt-child-uid"}}}
	c := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		dvResource: "DataVolumeList", pvcResource: "PersistentVolumeClaimList", pvResource: "PersistentVolumeList",
		podResource: "PodList"}, dv, parent, child)
	n := &ImageNative{client: c}

	out, done, err := n.DeleteImageObjects(ctx, i)
	if err != nil {
		t.Fatalf("delete chain refused a CDI child rebuilt under the same name: %v", err)
	}
	if done {
		t.Fatal("chain reported completion while objects are still present")
	}
	verbs := map[string]int{}
	for _, action := range c.Actions() {
		verbs[action.GetVerb()+"/"+action.GetResource().Resource]++
	}
	if verbs["delete/datavolumes"] != 1 {
		t.Fatalf("DataVolume was not removed first: %v", verbs)
	}
	if len(out.Cleanup) == 0 || out.Cleanup[0].UID != "rebuilt-child-uid" {
		t.Fatalf("child registration not refreshed to the cluster UID: %+v", out.Cleanup)
	}
}

// TestDeleteImageObjectsStillRefusesAForeignObjectAtTheChainHead keeps the ownership
// fence: only names derived from this record may be adopted after a UID change.
func TestDeleteImageObjectsStillRefusesAForeignObjectAtTheChainHead(t *testing.T) {
	ctx := context.Background()
	i := biz.Image{ID: "2b1f0c2e-6d3a-4f57-9a10-3f7f5c1d8a11", TenantID: "64c5c35a-0000-4000-8000-000000000001"}
	i.Native = biz.ImageNative{Namespace: "tenant-a", PVCName: "vmi-src-x", PVCUID: "parent-pvc-uid"}
	i.Cleanup = []biz.ObjectRef{{Kind: "PersistentVolumeClaim", Namespace: i.Native.Namespace, Name: "someone-elses-pvc", UID: "old-uid"}}
	foreign := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "PersistentVolumeClaim",
		"metadata": map[string]any{"name": "someone-elses-pvc", "namespace": i.Native.Namespace, "uid": "new-uid"}}}
	c := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		pvcResource: "PersistentVolumeClaimList", podResource: "PodList"}, foreign)
	n := &ImageNative{client: c}
	if _, _, err := n.DeleteImageObjects(ctx, i); err != biz.ErrOwnership {
		t.Fatalf("foreign object at the chain head was not refused: %v", err)
	}
}
