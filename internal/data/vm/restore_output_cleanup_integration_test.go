//go:build instanceintegration

package vm

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	"github.com/zhangzhe-ctrl/ani-resource-service/tests/testenv"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestRestoreMissingRegisteredOutputsDoNotBlockSnapshotDelete(t *testing.T) {
	ctx := t.Context()
	native, v, s, client := nativeSnapshotFixture(t)
	v.Spec.Canonical = []byte("{}")
	v.Spec.BackendStorageClass = "backend-sc"
	media := biz.MediaCopy{ID: uuid.NewString(), Source: biz.Source{VolumeMode: "Filesystem"}}
	backendName, err := backendVolumeName(v.Native.Name)
	if err != nil {
		t.Fatal(err)
	}
	s.Inventory = s.Inventory[:1]
	s.MediaCopies = []biz.MediaCopy{media}
	s.Required = []biz.SnapshotVolume{{Name: s.Inventory[0].VolumeName, Role: "system", DiskID: s.Inventory[0].ID}, {Name: "media-install", Role: "media", MediaCopyID: media.ID}, {Name: backendName, Role: "backend"}}
	var outputs []any
	for _, item := range []struct{ name, volume, mode, class string }{{"old-restored-disk", s.Inventory[0].VolumeName, "Block", "block-sc"}, {"old-restored-media", "media-install", "Filesystem", "iso-sc"}, {"old-restored-backend", backendName, "Filesystem", "backend-sc"}} {
		pvc := snapshotObject("v1", "PersistentVolumeClaim", item.name, v.Spec.Namespace, map[string]any{"spec": map[string]any{"volumeMode": item.mode, "storageClassName": item.class, "resources": map[string]any{"requests": map[string]any{"storage": "16Gi"}}}, "status": map[string]any{"phase": "Bound", "capacity": map[string]any{"storage": "16Gi"}}})
		pvc.SetOwnerReferences(ownReference(v))
		if _, err = client.Resource(pvcResource).Namespace(v.Spec.Namespace).Create(ctx, pvc, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		outputs = append(outputs, map[string]any{"volumeName": item.volume, "persistentVolumeClaim": item.name})
	}
	restore := snapshotObject("snapshot.kubevirt.io/v1beta1", "VirtualMachineRestore", "old-registered-restore", v.Spec.Namespace, map[string]any{"spec": map[string]any{"target": map[string]any{"apiGroup": "kubevirt.io", "kind": "VirtualMachine", "name": v.Native.Name}, "virtualMachineSnapshotName": s.Native.Name, "targetReadinessPolicy": "FailImmediate", "volumeRestorePolicy": "RandomizeNames", "volumeOwnershipPolicy": "Vm"}, "status": map[string]any{"complete": true, "restores": outputs}})
	restore.SetLabels(labelVM(v))
	restore.SetOwnerReferences(ownReference(v))
	restore.SetAnnotations(map[string]string{"vm.ani.io/snapshot-uid": s.Native.UID, "vm.ani.io/source-vm-uid": v.Native.UID})
	restore, err = client.Resource(restoreResource).Namespace(v.Spec.Namespace).Create(ctx, restore, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ref := objectRef(restore)
	first, err := native.ObserveRestore(ctx, v, s, ref)
	if err != nil || !first.OutputsKnown || len(first.BackendStates) != 1 || len(first.MediaCopies) != 1 || len(first.Disks) != 1 {
		t.Fatal(first, err)
	}
	for _, output := range outputs {
		name := output.(map[string]any)["persistentVolumeClaim"].(string)
		if err = client.Resource(pvcResource).Namespace(v.Spec.Namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	absent, err := native.ObserveRestore(ctx, v, s, ref)
	if err != nil || !absent.OutputsKnown || absent.Disks[0].PVC.UID != "" || absent.MediaCopies[0].PVC.UID != "" || absent.BackendStates[0].PVC.UID != "" {
		t.Fatal("public names lost when outputs disappeared", absent, err)
	}
	db := testenv.NewDatabase(t)
	if err = Migrate(ctx, db.Owner.Config().ConnString(), db.RuntimeRole); err != nil {
		t.Fatal(err)
	}
	repo, err := OpenPostgres(ctx, db.RuntimeDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	createID := uuid.NewString()
	if _, err = repo.AcceptCreate(ctx, biz.Acceptance{TenantID: v.TenantID, VMID: v.ID, OperationID: createID, RequestDigest: stringDigest('a'), Spec: v.Spec}); err != nil {
		t.Fatal(err)
	}
	create, working, err := repo.Claim(ctx, uuid.NewString(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	s.Restores = []biz.ObjectRef{ref}
	working.Native = v.Native
	working.Disks = v.Disks
	working.Snapshots = []biz.Snapshot{s}
	working.RetiredDisks = first.Disks
	working.RetiredMediaCopies = first.MediaCopies
	working.BackendStates = first.BackendStates
	working.Observed = "Stopped"
	// Preserve cleanup references and known capacity through the absent reread.
	extra := biz.ObjectRef{Kind: "Secret", Namespace: v.Spec.Namespace, Name: "old-cleanup-secret", UID: "registered-secret"}
	working.RetiredDisks[0].Cleanup = []biz.ObjectRef{extra}
	create.State = "succeeded"
	if err = repo.Save(ctx, create, working); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"snapshot_id": s.ID})
	op, err := repo.AcceptCommand(ctx, biz.Command{TenantID: v.TenantID, VMID: v.ID, Action: "delete-snapshot", Key: "delete-after-old-output-gone", ExpectedVersion: working.Version, RequestDigest: stringDigest('e'), Intent: body})
	if err != nil {
		t.Fatal(err)
	}
	client.ClearActions() // Check only the deletion path, excluding fixture setup.
	worker, err := biz.NewWorker(repo, native, powerVMNetwork{}, discardLoggerVMLane())
	if err != nil {
		t.Fatal(err)
	}
	succeeded := false
	for i := 0; i < 8; i++ {
		if _, err = db.Owner.Exec(ctx, "UPDATE vm.operations SET next_attempt_at=now()-interval '1 second' WHERE tenant_id=$1 AND id=$2", v.TenantID, op.ID); err != nil {
			t.Fatal(err)
		}
		if _, err = worker.Step(ctx); err != nil {
			t.Fatal(err)
		}
		current, e := repo.GetOperation(ctx, v.TenantID, v.ID, op.ID)
		if e != nil {
			t.Fatal(e)
		}
		if current.State == "failed" {
			t.Fatalf("normal disappearance failed snapshot deletion: %+v", current)
		}
		if current.State == "succeeded" {
			succeeded = true
			break
		}
	}
	if !succeeded {
		t.Fatal("snapshot delete did not converge")
	}
	saved, err := repo.Get(ctx, v.TenantID, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !saved.Snapshots[0].Tombstone || len(saved.Snapshots[0].RestoreOutputsRecorded) != 1 {
		t.Fatal("handoff/deletion not persisted", saved.Snapshots)
	}
	if saved.RetiredDisks[0].PVC != first.Disks[0].PVC || len(saved.RetiredDisks[0].Cleanup) != 1 || saved.RetiredDisks[0].ObservedCapacityBytes != first.Disks[0].ObservedCapacityBytes || saved.RetiredMediaCopies[0].PVC != first.MediaCopies[0].PVC || saved.BackendStates[0].PVC != first.BackendStates[0].PVC || saved.BackendStates[0].Present {
		t.Fatal("old output responsibility lost", saved)
	}
	for _, item := range []struct {
		kind string
		name string
	}{{"snapshot", s.Native.Name}, {"restore", ref.Name}} {
		resource := snapshotResource
		if item.kind == "restore" {
			resource = restoreResource
		}
		if _, err = client.Resource(resource).Namespace(v.Spec.Namespace).Get(ctx, item.name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			t.Fatal(item, err)
		}
	}
	for _, action := range client.Actions() {
		if action.GetResource() == pvResource || action.GetResource() == volumeSnapshotResource || (action.GetResource() == pvcResource && action.GetVerb() == "list") {
			t.Fatal("cleanup used lower-level proof or namespace scan", action)
		}
	}
}
