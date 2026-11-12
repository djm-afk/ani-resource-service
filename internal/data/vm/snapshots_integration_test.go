//go:build instanceintegration

package vm

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	"github.com/zhangzhe-ctrl/ani-resource-service/tests/testenv"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic/fake"
)

func TestSnapshotPostgresInventoryVersionFencesAndRestoreCheckpoints(t *testing.T) {
	for _, takeover := range []bool{false, true} {
		name := "cleanup-checkpoint"
		if takeover {
			name = "delete-takeover"
		}
		t.Run(name, func(t *testing.T) { snapshotPostgresRestore(t, takeover) })
	}
}
func snapshotPostgresRestore(t *testing.T, takeover bool) {
	ctx := context.Background()
	db := testenv.NewDatabase(t)
	if err := Migrate(ctx, db.Owner.Config().ConnString(), db.RuntimeRole); err != nil {
		t.Fatal(err)
	}
	repo, err := OpenPostgres(ctx, db.RuntimeDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	tenant, id, createID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	disk := biz.NewInitialDisk(id, "system", "system", "system", 8)
	a := biz.Acceptance{TenantID: tenant, VMID: id, OperationID: createID, RequestDigest: stringDigest('a'), Spec: biz.Spec{Canonical: json.RawMessage(`{"snapshot_fixture":true}`), Digest: stringDigest('b'), Name: "snapshot-fixture", Namespace: "tenant-test", NamespaceUID: uuid.NewString(), BootDevice: "SYSTEM_DISK", InitialDisks: []biz.Disk{disk}}}
	if _, err = repo.AcceptCreate(ctx, a); err != nil {
		t.Fatal(err)
	}
	create, v, err := repo.Claim(ctx, uuid.NewString(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	v.Native = biz.ObjectRef{Kind: "VirtualMachine", Namespace: v.Spec.Namespace, Name: "vm-" + id, UID: uuid.NewString()}
	v.Disks[0].DV.Namespace = v.Spec.Namespace
	v.Disks[0].DV.UID = uuid.NewString()
	v.Disks[0].PVC = biz.ObjectRef{Kind: "PersistentVolumeClaim", Namespace: v.Spec.Namespace, Name: v.Disks[0].DV.Name, UID: uuid.NewString()}
	snapshot, err := biz.NewSnapshot(v, uuid.NewString(), "snapshot")
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Native.UID = uuid.NewString()
	snapshot.Content = biz.ObjectRef{Kind: "VirtualMachineSnapshotContent", Namespace: v.Spec.Namespace, Name: "content", UID: uuid.NewString()}
	snapshot.Ready = true
	snapshot.Included = []string{v.Disks[0].VolumeName}
	snapshot.Complete = biz.SnapshotComplete(snapshot)
	v.Snapshots = []biz.Snapshot{snapshot}
	create.State = "succeeded"
	if err = repo.Save(ctx, create, v); err != nil {
		t.Fatal(err)
	}
	var body []byte
	if err = repo.pool.QueryRow(ctx, "SELECT record FROM vm.snapshots WHERE tenant_id=$1 AND vm_id=$2 AND snapshot_id=$3", tenant, id, snapshot.ID).Scan(&body); err != nil {
		t.Fatal("snapshot inventory was not saved inside the fenced transaction", err)
	}
	var frozen biz.Snapshot
	if err = json.Unmarshal(body, &frozen); err != nil || frozen.Inventory[0].PVC.UID != v.Disks[0].PVC.UID {
		t.Fatal("frozen physical inventory missing", err)
	}
	if _, err = db.Owner.Exec(ctx, "INSERT INTO vm.snapshots(tenant_id,vm_id,snapshot_id,record) VALUES($1,$2,$3,'{}')", uuid.NewString(), id, uuid.NewString()); err == nil {
		t.Fatal("cross-tenant snapshot FK accepted")
	}
	plan, err := biz.PreviewSnapshotRestore(v, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	intent, _ := json.Marshal(map[string]any{"snapshot_id": snapshot.ID, "preview_digest": plan.Digest, "destructive_confirmation": true, "expected_version": v.Version})
	command := biz.Command{TenantID: tenant, VMID: id, Action: "restore-snapshot", Key: "restore-once", ExpectedVersion: v.Version, RequestDigest: stringDigest('c'), Intent: intent}
	accepted, err := repo.AcceptCommand(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := repo.AcceptCommand(ctx, command)
	if err != nil || replayed.ID != accepted.ID {
		t.Fatal("original-version replay lost", err)
	}
	conflict := command
	conflict.Key = "changed-preview"
	conflict.ExpectedVersion = 2
	if _, err = repo.AcceptCommand(ctx, conflict); !errors.Is(err, biz.ErrBusy) {
		t.Fatal("concurrent restore admitted", err)
	}
	op, working, err := repo.Claim(ctx, uuid.NewString(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	newDisk := working.Disks[0]
	oldUID := newDisk.PVC.UID
	newDisk.DV = biz.ObjectRef{}
	newDisk.PVC = biz.ObjectRef{Kind: "PersistentVolumeClaim", Namespace: working.Spec.Namespace, Name: "restored-system", UID: uuid.NewString()}
	newDisk.Preparation = "Pending"
	op.Snapshot = &biz.SnapshotProgress{SnapshotID: snapshot.ID, Plan: &plan, NativeRestore: biz.ObjectRef{Kind: "VirtualMachineRestore", Namespace: working.Spec.Namespace, Name: "restore-" + op.ID, UID: uuid.NewString()}, NativeRestoreCompleted: true, RestoredDisks: []biz.Disk{newDisk}}
	op.Step = "native_restore_completed"
	if err = repo.Checkpoint(ctx, op, working); err != nil {
		t.Fatal(err)
	}
	other, err := OpenPostgres(ctx, db.RuntimeDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	durable, err := other.GetOperation(ctx, tenant, id, op.ID)
	if err != nil || durable.Snapshot == nil || !durable.Snapshot.NativeRestoreCompleted || durable.Snapshot.MappingCommitted {
		t.Fatal("native checkpoint lost across process boundary", err)
	}
	before, err := other.Get(ctx, tenant, id)
	if err != nil || before.Disks[0].PVC.UID != oldUID {
		t.Fatal("uncommitted mapping replaced current configuration", err)
	}
	if _, err = db.Owner.Exec(ctx, "UPDATE vm.operations SET lease_until=now()-interval '1 second',next_attempt_at=now()-interval '1 second' WHERE tenant_id=$1 AND id=$2", tenant, op.ID); err != nil {
		t.Fatal(err)
	}
	resumed, after, err := other.Claim(ctx, uuid.NewString(), time.Minute)
	if err != nil || resumed.ID != op.ID || !resumed.Snapshot.NativeRestoreCompleted {
		t.Fatal("new worker lost restore progress", err)
	}
	if err = repo.Checkpoint(ctx, op, working); !errors.Is(err, biz.ErrLease) {
		t.Fatal("stale worker lease overwrote progress", err)
	}
	retired := after.Disks[0]
	retired.Cleanup = []biz.ObjectRef{{Kind: "PersistentVolume", Name: "old-pv", UID: uuid.NewString()}}
	after.RetiredDisks = []biz.Disk{retired}
	after.Disks[0] = newDisk
	resumed.Snapshot.MappingCommitted = true
	resumed.Step = "mapping_committed"
	if err = other.Checkpoint(ctx, resumed, after); err != nil {
		t.Fatal(err)
	}
	current, err := repo.Get(ctx, tenant, id)
	if err != nil || current.Disks[0].PVC.UID != newDisk.PVC.UID || current.Disks[0].Preparation != "Pending" {
		t.Fatal("new mapping not durable", err)
	}
	if err = repo.pool.QueryRow(ctx, "SELECT record FROM vm.snapshots WHERE tenant_id=$1 AND vm_id=$2 AND snapshot_id=$3", tenant, id, snapshot.ID).Scan(&body); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(body, &frozen); err != nil || frozen.Inventory[0].PVC.UID != oldUID {
		t.Fatal("restore rewrote immutable snapshot inventory", err)
	}
	if !takeover {
		after.RetiredDisks = nil
		resumed.Snapshot.CleanupCompleted = true
		resumed.Step = "cleanup_completed"
		if err = other.Checkpoint(ctx, resumed, after); err != nil {
			t.Fatal(err)
		}
		final, err := repo.GetOperation(ctx, tenant, id, op.ID)
		if err != nil || !final.Snapshot.NativeRestoreCompleted || !final.Snapshot.MappingCommitted || !final.Snapshot.CleanupCompleted || final.State == "succeeded" {
			t.Fatal("three checkpoints were not independently durable", err)
		}
		return
	}
	// A cleanup failure is durable while the new mapping remains authoritative.
	resumed.Reason = "VM_RESTORE_OLD_VOLUME_CLEANUP"
	resumed.State = "running"
	if err = other.Save(ctx, resumed, after); err != nil {
		t.Fatal(err)
	}
	deletion, err := repo.AcceptDelete(ctx, biz.Acceptance{TenantID: tenant, VMID: id, OperationID: uuid.NewString(), CreateOperationID: createID, ExpectedVersion: after.Version, RequestDigest: stringDigest('d')})
	if err != nil {
		t.Fatal(err)
	}
	current, err = other.Get(ctx, tenant, id)
	if err != nil || !current.Closing || current.Disks[0].PVC.UID != newDisk.PVC.UID || len(current.RetiredDisks) != 1 || current.RetiredDisks[0].Cleanup[0] != retired.Cleanup[0] {
		t.Fatal("DELETE admission lost committed mapping or retired physical refs", err)
	}
	oldOperation, err := other.GetOperation(ctx, tenant, id, op.ID)
	if err != nil || !oldOperation.Snapshot.MappingCommitted || oldOperation.Snapshot.CleanupCompleted {
		t.Fatal("superseded restore progress changed", err)
	}
	ns := snapshotObject("v1", "Namespace", after.Spec.Namespace, "", nil)
	ns.SetUID(types.UID(after.Spec.NamespaceUID))
	ns.SetLabels(map[string]string{"network.ani.io/tenant-id": tenant, "network.ani.io/managed-by": "ani-network-service"})
	pv := snapshotObject("v1", "PersistentVolume", "old-pv", "", map[string]any{"spec": map[string]any{"persistentVolumeReclaimPolicy": "Delete", "claimRef": map[string]any{"name": retired.PVC.Name, "namespace": retired.PVC.Namespace, "uid": retired.PVC.UID}}})
	pv.SetUID(types.UID(retired.Cleanup[0].UID))
	client := fake.NewSimpleDynamicClient(runtime.NewScheme(), ns, pv)
	boundary := &snapshotDeleteBoundary{physical: NewNativeWithClients(client, nil)}
	worker, err := biz.NewWorker(repo, boundary, snapshotDeleteNetwork{}, discardLoggerVMLane())
	if err != nil {
		t.Fatal(err)
	}
	run := func() {
		t.Helper()
		if _, err = db.Owner.Exec(ctx, "UPDATE vm.operations SET next_attempt_at=now()-interval '1 second' WHERE tenant_id=$1 AND id=$2", tenant, deletion.ID); err != nil {
			t.Fatal(err)
		}
		if _, err = worker.Step(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for attempts := 0; attempts < 8; attempts++ {
		run()
	}
	pending, err := repo.GetOperation(ctx, tenant, id, deletion.ID)
	if err != nil || pending.State != "succeeded" || boundary.oldPVChecks == 0 {
		t.Fatal("DELETE remained blocked on historical PV after business PVC absence", pending, err)
	}
	current, err = other.Get(ctx, tenant, id)
	if err != nil || len(current.RetiredDisks) != 0 {
		t.Fatal("completed business retirement remained pending on PV", err)
	}
	current, err = other.Get(ctx, tenant, id)
	if err != nil || pending.State != "succeeded" || len(current.RetiredDisks) != 0 || current.Disks[0].PVC.UID != newDisk.PVC.UID {
		t.Fatal("DELETE did not finish at the business object boundary", pending, err)
	}
}

// Native systems alone are replaced; DELETE admission, supersession, worker
// checkpoints, leases and aggregate inventory use actual PostgreSQL.
type snapshotDeleteBoundary struct {
	biz.NativePort
	physical    *Native
	oldPVChecks int
	restored    []biz.Disk
	restoreUID  string
}

func (n *snapshotDeleteBoundary) Observe(_ context.Context, v biz.VM) (biz.Observation, error) {
	return biz.Observation{VM: v.Native, Deleted: true}, nil
}
func (n *snapshotDeleteBoundary) ObserveRetiredVolume(ctx context.Context, v biz.VM, d biz.Disk) (biz.Disk, error) {
	return n.physical.ObserveRetiredVolume(ctx, v, d)
}
func (n *snapshotDeleteBoundary) DeleteVolume(ctx context.Context, v biz.VM, d biz.Disk) (bool, error) {
	if len(d.Cleanup) > 0 {
		n.oldPVChecks++
	}
	return n.physical.DeleteVolume(ctx, v, d)
}
func (n *snapshotDeleteBoundary) PrepareSnapshot(context.Context, biz.VM, biz.Snapshot) (biz.Snapshot, error) {
	panic("unexpected prepare")
}
func (n *snapshotDeleteBoundary) EnsureSnapshot(context.Context, biz.VM, biz.Snapshot) (biz.Snapshot, error) {
	panic("unexpected create")
}
func (n *snapshotDeleteBoundary) ObserveSnapshot(_ context.Context, _ biz.VM, s biz.Snapshot) (biz.Snapshot, error) {
	return s, nil
}
func (n *snapshotDeleteBoundary) DeleteSnapshot(context.Context, biz.VM, biz.Snapshot) (bool, error) {
	return true, nil
}
func (n *snapshotDeleteBoundary) EnsureRestore(context.Context, biz.VM, biz.Snapshot, biz.ObjectRef) (biz.RestoreObservation, error) {
	panic("unexpected restore")
}
func (n *snapshotDeleteBoundary) ApplyRestoredConfiguration(context.Context, biz.VM, biz.Snapshot) (bool, error) {
	panic("unexpected configure")
}

type snapshotDeleteNetwork struct{ biz.NetworkPort }

func (snapshotDeleteNetwork) Release(context.Context, biz.VM) (bool, error) { return true, nil }

func (n *snapshotDeleteBoundary) ObserveRestore(_ context.Context, _ biz.VM, _ biz.Snapshot, ref biz.ObjectRef) (biz.RestoreObservation, error) {
	if n.restoreUID != "" {
		ref.UID = n.restoreUID
	}
	return biz.RestoreObservation{Native: ref, Complete: true, OutputsKnown: true, Disks: n.restored}, nil
}

func TestSnapshotPostgresDeleteBeforeMappingAndLostCheckpoint(t *testing.T) {
	for _, completedCheckpoint := range []bool{true, false} {
		name := "native-completed"
		if !completedCheckpoint {
			name = "lost-checkpoint"
		}
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			db := testenv.NewDatabase(t)
			if err := Migrate(ctx, db.Owner.Config().ConnString(), db.RuntimeRole); err != nil {
				t.Fatal(err)
			}
			repo, err := OpenPostgres(ctx, db.RuntimeDSN)
			if err != nil {
				t.Fatal(err)
			}
			defer repo.Close()
			tenant, id, createID := uuid.NewString(), uuid.NewString(), uuid.NewString()
			disk := biz.NewInitialDisk(id, "system", "system", "system", 8)
			_, err = repo.AcceptCreate(ctx, biz.Acceptance{TenantID: tenant, VMID: id, OperationID: createID, RequestDigest: stringDigest('a'), Spec: biz.Spec{Canonical: json.RawMessage(`{"restore-delete":true}`), Digest: stringDigest('b'), Namespace: "tenant-restore-delete", NamespaceUID: uuid.NewString(), InitialDisks: []biz.Disk{disk}}})
			if err != nil {
				t.Fatal(err)
			}
			create, v, err := repo.Claim(ctx, uuid.NewString(), time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			v.Native = biz.ObjectRef{Kind: "VirtualMachine", Namespace: v.Spec.Namespace, Name: "vm-" + id, UID: uuid.NewString()}
			v.Disks[0].DV = biz.ObjectRef{}
			v.Disks[0].PVC = biz.ObjectRef{Kind: "PersistentVolumeClaim", Namespace: v.Spec.Namespace, Name: "old-root", UID: uuid.NewString()}
			snapshot, err := biz.NewSnapshot(v, uuid.NewString(), "frozen")
			if err != nil {
				t.Fatal(err)
			}
			snapshot.Native.UID = uuid.NewString()
			snapshot.Content = biz.ObjectRef{Kind: "VirtualMachineSnapshotContent", Namespace: v.Spec.Namespace, Name: "content", UID: uuid.NewString()}
			snapshot.Ready = true
			snapshot.Included = []string{v.Disks[0].VolumeName}
			snapshot.Complete = biz.SnapshotComplete(snapshot)
			v.Snapshots = []biz.Snapshot{snapshot}
			create.State = "succeeded"
			if err = repo.Save(ctx, create, v); err != nil {
				t.Fatal(err)
			}
			plan, err := biz.PreviewSnapshotRestore(v, snapshot)
			if err != nil {
				t.Fatal(err)
			}
			intent, _ := json.Marshal(map[string]any{"snapshot_id": snapshot.ID, "preview_digest": plan.Digest, "destructive_confirmation": true})
			accepted, err := repo.AcceptCommand(ctx, biz.Command{TenantID: tenant, VMID: id, Action: "restore-snapshot", Key: "restore", ExpectedVersion: v.Version, RequestDigest: stringDigest('c'), Intent: intent})
			if err != nil {
				t.Fatal(err)
			}
			op, v, err := repo.Claim(ctx, uuid.NewString(), time.Minute)
			if err != nil || op.ID != accepted.ID {
				t.Fatal(err)
			}
			ref := biz.ObjectRef{Kind: "VirtualMachineRestore", Namespace: v.Spec.Namespace, Name: "vm-restore-" + op.ID}
			v.Snapshots[0].Restores = []biz.ObjectRef{ref}
			op.Snapshot = &biz.SnapshotProgress{SnapshotID: snapshot.ID, Plan: &plan, NativeRestore: ref}
			op.Step = "restore-snapshot"
			if err = repo.Checkpoint(ctx, op, v); err != nil {
				t.Fatal(err)
			}
			restoreUID := uuid.NewString()
			newDisk := v.Disks[0]
			newDisk.PVC = biz.ObjectRef{Kind: "PersistentVolumeClaim", Namespace: v.Spec.Namespace, Name: "restored-root", UID: uuid.NewString()}
			newDisk.Cleanup = []biz.ObjectRef{{Kind: "PersistentVolume", Name: "restored-pv", UID: uuid.NewString()}}
			if completedCheckpoint {
				v.RetiredDisks = []biz.Disk{newDisk}
				op.Snapshot.NativeRestore.UID = restoreUID
				op.Snapshot.NativeRestoreCompleted = true
				op.Snapshot.RestoredDisks = []biz.Disk{newDisk}
				op.Step = "native_restore_completed"
				if err = repo.Checkpoint(ctx, op, v); err != nil {
					t.Fatal(err)
				}
			}
			deletion, err := repo.AcceptDelete(ctx, biz.Acceptance{TenantID: tenant, VMID: id, OperationID: uuid.NewString(), CreateOperationID: createID, ExpectedVersion: v.Version, RequestDigest: stringDigest('d')})
			if err != nil {
				t.Fatal(err)
			}
			if err = repo.Checkpoint(ctx, op, v); !errors.Is(err, biz.ErrVersion) && !errors.Is(err, biz.ErrLease) {
				t.Fatal("superseded restore worker overwrote DELETE fence", err)
			}
			if _, _, err = repo.Claim(ctx, uuid.NewString(), time.Minute); !errors.Is(err, biz.ErrNotFound) {
				t.Fatal("DELETE bypassed the original worker lease", err)
			}
			if _, err = db.Owner.Exec(ctx, "UPDATE vm.operations SET lease_until=now()-interval '1 second' WHERE tenant_id=$1 AND id=$2", tenant, op.ID); err != nil {
				t.Fatal(err)
			}

			ns := snapshotObject("v1", "Namespace", v.Spec.Namespace, "", nil)
			ns.SetUID(types.UID(v.Spec.NamespaceUID))
			ns.SetLabels(map[string]string{"network.ani.io/tenant-id": tenant, "network.ani.io/managed-by": "ani-network-service"})
			pv := snapshotObject("v1", "PersistentVolume", newDisk.Cleanup[0].Name, "", map[string]any{"spec": map[string]any{"persistentVolumeReclaimPolicy": "Delete", "claimRef": map[string]any{"name": newDisk.PVC.Name, "namespace": newDisk.PVC.Namespace, "uid": newDisk.PVC.UID}}})
			pv.SetUID(types.UID(newDisk.Cleanup[0].UID))
			client := fake.NewSimpleDynamicClient(runtime.NewScheme(), ns, pv)
			boundary := &snapshotDeleteBoundary{physical: NewNativeWithClients(client, nil), restored: []biz.Disk{newDisk}, restoreUID: restoreUID}
			worker, err := biz.NewWorker(repo, boundary, snapshotDeleteNetwork{}, discardLoggerVMLane())
			if err != nil {
				t.Fatal(err)
			}
			run := func() {
				t.Helper()
				if _, err = db.Owner.Exec(ctx, "UPDATE vm.operations SET next_attempt_at=now()-interval '1 second' WHERE tenant_id=$1 AND id=$2", tenant, deletion.ID); err != nil {
					t.Fatal(err)
				}
				if _, err = worker.Step(ctx); err != nil {
					t.Fatal(err)
				}
			}
			run() // Output and cancel responsibility are checkpointed before producer deletion.
			current, err := repo.Get(ctx, tenant, id)
			if err != nil || len(current.RetiredDisks) != 1 || current.RetiredDisks[0].PVC != newDisk.PVC ||
				len(current.Snapshots[0].RestoreOutputsRecorded) != 1 {
				t.Fatal("named outputs were not durable before cleanup", current, err)
			}
			var pending biz.Operation
			for i := 0; i < 8; i++ {
				run()
				pending, err = repo.GetOperation(ctx, tenant, id, deletion.ID)
				if err != nil {
					t.Fatal(err)
				}
				if pending.State == "succeeded" {
					break
				}
			}
			if pending.State != "succeeded" || boundary.oldPVChecks == 0 {
				t.Fatal("business cleanup stayed blocked on PV", pending)
			}
			// The historical PV still exists; platform completion does not assert
			// physical reclamation or mutate the storage controller's object.
			if _, err = client.Resource(pvResource).Get(ctx, pv.GetName(), metav1.GetOptions{}); err != nil {
				t.Fatal(err)
			}

		})
	}
}
