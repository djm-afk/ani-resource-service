package vm

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func snapshotFixture(t *testing.T) (VM, Snapshot) {
	t.Helper()
	v := VM{TenantID: "10000000-0000-4000-8000-000000000001", ID: "20000000-0000-4000-8000-000000000001", Version: 12, Native: ObjectRef{Kind: "VirtualMachine", Namespace: "tenant-a", Name: "vm-a", UID: "root-vm-uid"}, Spec: Spec{Namespace: "tenant-a", NamespaceUID: "tenant-uid", BootDevice: "SYSTEM_DISK", Digest: "initial-digest", Canonical: json.RawMessage(`{"immutable":true}`)}, CurrentBootDevice: "SYSTEM_DISK"}
	for i, name := range []string{"root", "attached", "detached", "deleted"} {
		role := "data"
		if i == 0 {
			role = "system"
		}
		d := NewInitialDisk(v.ID, name, name, role, 10)
		d.DV.Namespace = v.Spec.Namespace
		d.DV.UID = "dv-" + name
		d.PVC = ObjectRef{Kind: "PersistentVolumeClaim", Namespace: v.Spec.Namespace, Name: d.DV.Name, UID: "pvc-" + name}
		d.Cleanup = []ObjectRef{{Kind: "PersistentVolume", Name: "pv-" + name, UID: "pvuid-" + name}}
		v.Disks = append(v.Disks, d)
	}
	s, err := NewSnapshot(v, "30000000-0000-4000-8000-000000000001", "before-change")
	if err != nil {
		t.Fatal(err)
	}
	s.Native.UID = "snapshot-uid"
	s.Content = ObjectRef{Kind: "VirtualMachineSnapshotContent", Namespace: v.Spec.Namespace, Name: "snapshot-content", UID: "content-uid"}
	s.Ready = true
	for _, volume := range s.Required {
		s.Included = append(s.Included, volume.Name)
	}
	s.Complete = SnapshotComplete(s)
	v.Snapshots = []Snapshot{s}
	for i := range v.Disks {
		if v.Disks[i].Name == "detached" {
			v.Disks[i].Attached = false
		}
		if v.Disks[i].Name == "deleted" {
			v.Disks[i].Tombstone = true
			v.Disks[i].Attached = false
		}
	}
	for _, name := range []string{"added-attached", "added-detached"} {
		d := NewInitialDisk(v.ID, name, name, "data", 5)
		d.DV.Namespace = v.Spec.Namespace
		d.DV.UID = "dv-" + name
		d.PVC = ObjectRef{Kind: "PersistentVolumeClaim", Namespace: v.Spec.Namespace, Name: d.DV.Name, UID: "pvc-" + name}
		d.Attached = name == "added-attached"
		v.Disks = append(v.Disks, d)
	}
	return v, s
}

func TestRestorePreviewFiveSetsAndVersionIdentityDigest(t *testing.T) {
	v, s := snapshotFixture(t)
	plan, err := PreviewSnapshotRestore(v, s)
	if err != nil {
		t.Fatal(err)
	}
	expected := map[string]string{"root": "restore-attached", "attached": "restore-attached", "detached": "restore-detached", "deleted": "restore-deleted", "added-attached": "delete-added-attached", "added-detached": "retain-detached"}
	for _, disk := range v.Disks {
		for _, impact := range plan.Impacts {
			if disk.ID == impact.DiskID {
				if impact.Action != expected[disk.Name] {
					t.Fatalf("disk %s action %s", disk.Name, impact.Action)
				}
				delete(expected, disk.Name)
			}
		}
	}
	if len(expected) != 0 {
		t.Fatalf("missing sets: %v", expected)
	}
	if len(plan.Cleanup) != 4 {
		t.Fatalf("unexpected cleanup: %+v", plan.Cleanup)
	}
	unchanged := v
	unchanged.Disks = append([]Disk(nil), v.Disks...)
	for left, right := 0, len(unchanged.Disks)-1; left < right; left, right = left+1, right-1 {
		unchanged.Disks[left], unchanged.Disks[right] = unchanged.Disks[right], unchanged.Disks[left]
	}
	other, err := PreviewSnapshotRestore(unchanged, s)
	if err != nil || other.Digest != plan.Digest {
		t.Fatal("iteration order changed preview")
	}
	changed := v
	changed.Version++
	other, err = PreviewSnapshotRestore(changed, s)
	if err != nil || other.Digest == plan.Digest {
		t.Fatal("version not bound")
	}
	changed = v
	changed.Disks = append([]Disk(nil), v.Disks...)
	changed.Disks[0].PVC.UID = "replacement"
	other, err = PreviewSnapshotRestore(changed, s)
	if err != nil || other.Digest == plan.Digest {
		t.Fatal("physical identity not bound")
	}
	changed = v
	changed.Native.UID = "new-root"
	if _, err = PreviewSnapshotRestore(changed, s); !errors.Is(err, ErrOwnership) {
		t.Fatal(err)
	}
	s.Excluded = []string{s.Required[0].Name}
	if SnapshotComplete(s) {
		t.Fatal("excluded required disk considered complete")
	}
	if _, err = PreviewSnapshotRestore(v, s); !errors.Is(err, ErrDependency) {
		t.Fatal(err)
	}
}

func restoredDisks(s Snapshot) []Disk {
	var result []Disk
	for _, disk := range s.Inventory {
		disk.DV = ObjectRef{}
		disk.PVC = ObjectRef{Kind: "PersistentVolumeClaim", Namespace: "tenant-a", Name: "restore-" + disk.ID, UID: "restored-" + disk.ID}
		disk.Preparation = "Pending"
		disk.Cleanup = nil
		result = append(result, disk)
	}
	return result
}
func TestRestoreMappingRecoversDeletedInventoryAndKeepsDetachedExtras(t *testing.T) {
	v, s := snapshotFixture(t)
	plan, err := PreviewSnapshotRestore(v, s)
	if err != nil {
		t.Fatal(err)
	}
	after, err := applyRestoreMapping(v, s, SnapshotProgress{Plan: &plan, RestoredDisks: restoredDisks(s)})
	if err != nil {
		t.Fatal(err)
	}
	for _, disk := range after.Disks {
		switch disk.Name {
		case "added-detached":
			if disk.Tombstone || disk.Attached || disk.PVC.UID != "pvc-added-detached" {
				t.Fatal("detached extra changed")
			}
		case "added-attached":
			if !disk.Tombstone || disk.Attached {
				t.Fatal("added attached disk retained")
			}
		default:
			if disk.Tombstone || !disk.Attached || disk.DV.Name != "" || disk.PVC.UID != "restored-"+disk.ID || disk.Preparation != "Pending" {
				t.Fatalf("snapshot disk not restored: %+v", disk)
			}
		}
	}
	if after.Spec.Digest != v.Spec.Digest || string(after.Spec.Canonical) != string(v.Spec.Canonical) {
		t.Fatal("initial audit specification was overwritten")
	}
	if after.Desired != "Stopped" || len(after.Addresses) != 0 {
		t.Fatal("restore inferred a running state or old network address")
	}
}

type snapshotCheckpoint struct {
	Repository
	op        Operation
	vm        VM
	steps     []string
	interrupt string
}

func (r *snapshotCheckpoint) Checkpoint(_ context.Context, o Operation, v VM) error {
	body, _ := json.Marshal(o)
	_ = json.Unmarshal(body, &r.op)
	body, _ = json.Marshal(v)
	_ = json.Unmarshal(body, &r.vm)
	r.steps = append(r.steps, o.Step)
	if r.interrupt != "" && r.interrupt == o.Step {
		r.interrupt = ""
		return ErrLease
	}
	return nil
}

type restoreBoundary struct {
	NativePort
	snapshot                         Snapshot
	ensures, configures, cleanups    int
	configurationReady, cleanupReady bool
	deletedUIDs                      []string
}

func (n *restoreBoundary) Observe(_ context.Context, v VM) (Observation, error) {
	return Observation{VM: v.Native, Stopped: true, State: "Stopped"}, nil
}
func (n *restoreBoundary) PrepareSnapshot(context.Context, VM, Snapshot) (Snapshot, error) {
	panic("unexpected prepare")
}
func (n *restoreBoundary) EnsureSnapshot(context.Context, VM, Snapshot) (Snapshot, error) {
	panic("unexpected snapshot create")
}
func (n *restoreBoundary) ObserveSnapshot(_ context.Context, _ VM, s Snapshot) (Snapshot, error) {
	return s, nil
}
func (n *restoreBoundary) DeleteSnapshot(context.Context, VM, Snapshot) (bool, error) {
	panic("unexpected snapshot delete")
}
func (n *restoreBoundary) EnsureRestore(_ context.Context, _ VM, s Snapshot, ref ObjectRef) (RestoreObservation, error) {
	n.ensures++
	ref.UID = "restore-uid"
	return RestoreObservation{Native: ref, Complete: true, OutputsKnown: true, Disks: restoredDisks(s)}, nil
}
func (n *restoreBoundary) ApplyRestoredConfiguration(context.Context, VM, Snapshot) (bool, error) {
	n.configures++
	return n.configurationReady, nil
}
func (n *restoreBoundary) ObserveRetiredVolume(_ context.Context, _ VM, d Disk) (Disk, error) {
	return d, nil
}
func (n *restoreBoundary) DeleteVolume(_ context.Context, _ VM, d Disk) (bool, error) {
	n.cleanups++
	n.deletedUIDs = append(n.deletedUIDs, d.PVC.UID)
	return n.cleanupReady, nil
}

func TestRestoreDurableCheckpointsResumeWithoutRepeatingNativeOrRollingBackMapping(t *testing.T) {
	v, s := snapshotFixture(t)
	plan, err := PreviewSnapshotRestore(v, s)
	if err != nil {
		t.Fatal(err)
	}
	intent, _ := json.Marshal(map[string]any{"snapshot_id": s.ID, "preview_digest": plan.Digest, "destructive_confirmation": true})
	v.Version++
	op := Operation{ID: "40000000-0000-4000-8000-000000000001", VMID: v.ID, TenantID: v.TenantID, Action: "restore-snapshot", Intent: intent, IntentVersion: v.Version, State: "running"}
	repo := &snapshotCheckpoint{}
	native := &restoreBoundary{snapshot: s}
	worker := &Worker{repo: repo, native: native}
	if err = worker.snapshot(t.Context(), &op, &v); err != nil {
		t.Fatal(err)
	}
	repo.interrupt = "native_restore_completed"
	if err = worker.snapshot(t.Context(), &op, &v); !errors.Is(err, ErrLease) {
		t.Fatal("expected checkpoint process boundary", err)
	}
	op, v = repo.op, repo.vm
	if !op.Snapshot.NativeRestoreCompleted || op.Snapshot.MappingCommitted || native.ensures != 1 || len(v.RetiredDisks) != len(s.Inventory) {
		t.Fatal("native progress not durable")
	}
	oldRoot := v.Disks[0].PVC.UID
	if err = worker.snapshot(t.Context(), &op, &v); err != nil {
		t.Fatal(err)
	}
	if v.Disks[0].PVC.UID != oldRoot || op.Snapshot.MappingCommitted {
		t.Fatal("unconfirmed device patch committed mapping")
	}
	native.configurationReady = true
	if err = worker.snapshot(t.Context(), &op, &v); err != nil {
		t.Fatal(err)
	}
	if !op.Snapshot.MappingCommitted || v.Disks[0].PVC.UID == oldRoot || len(v.RetiredDisks) != len(plan.Cleanup) {
		t.Fatal("mapping and retired refs checkpoint absent")
	}
	for _, old := range v.RetiredDisks {
		for _, current := range op.Snapshot.RestoredDisks {
			if old.PVC.UID == current.PVC.UID {
				t.Fatal("current restored PVC remained retired")
			}
		}
	}
	newMapping := append([]Disk(nil), v.Disks...)
	if err = worker.snapshot(t.Context(), &op, &v); !errors.Is(err, ErrDependency) {
		t.Fatal("pending cleanup was not retained", err)
	}
	if !reflect.DeepEqual(v.Disks, newMapping) || native.ensures != 1 {
		t.Fatal("cleanup failure repeated restore or rewrote old mapping")
	}
	op, v = repo.op, repo.vm
	native.cleanupReady = true
	repo.interrupt = "cleanup_completed"
	for attempts := 0; attempts < 20; attempts++ {
		err = worker.snapshot(t.Context(), &op, &v)
		if errors.Is(err, ErrLease) {
			break
		}
		if err != nil && !errors.Is(err, ErrDependency) {
			t.Fatal(err)
		}
	}
	if !errors.Is(err, ErrLease) {
		t.Fatal("cleanup checkpoint not reached", err)
	}
	op, v = repo.op, repo.vm
	if err = worker.snapshot(t.Context(), &op, &v); err != nil {
		t.Fatal(err)
	}
	if op.State != "succeeded" || !op.Snapshot.CleanupCompleted || native.ensures != 1 {
		t.Fatal("resume repeated native restore")
	}
	for _, uid := range native.deletedUIDs {
		for _, disk := range op.Snapshot.RestoredDisks {
			if uid == disk.PVC.UID {
				t.Fatal("new PVC entered old cleanup")
			}
		}
	}
}

func TestRestoreMappingRetiresMediaWithoutDiscardingNewerLogicalCopies(t *testing.T) {
	v, s := snapshotFixture(t)
	copy := MediaCopy{ID: "install-copy", SourceDigest: "frozen-media", Source: Source{Key: "iso"}, DV: ObjectRef{Kind: "DataVolume", Namespace: v.Spec.Namespace, Name: "install-old", UID: "dv-old"}, PVC: ObjectRef{Kind: "PersistentVolumeClaim", Namespace: v.Spec.Namespace, Name: "install-old", UID: "pvc-old"}, Cleanup: []ObjectRef{{Kind: "PersistentVolume", Name: "pv-old", UID: "pvuid-old"}}}
	s.MediaCopies = []MediaCopy{copy}
	s.MediaSlots = []MediaSlot{{Slot: "INSTALL", DesiredCopyID: copy.ID}}
	newer := copy
	newer.ID = "newer-copy"
	newer.PVC.UID = "pvc-newer"
	v.MediaCopies = []MediaCopy{copy, newer}
	plan, err := PreviewSnapshotRestore(v, s)
	if err != nil {
		t.Fatal(err)
	}
	restored := copy
	restored.DV = ObjectRef{}
	restored.PVC.Name = "restored-iso"
	restored.PVC.UID = "restored-iso"
	restored.Cleanup = nil
	after, err := applyRestoreMapping(v, s, SnapshotProgress{Plan: &plan, RestoredDisks: restoredDisks(s), RestoredMediaCopies: []MediaCopy{restored}})
	if err != nil {
		t.Fatal(err)
	}
	if len(after.MediaCopies) != 2 || len(after.RetiredMediaCopies) != 1 || after.RetiredMediaCopies[0].PVC.UID != copy.PVC.UID {
		t.Fatal("media cleanup or newer inventory lost", after)
	}
	after, err = applyRestoreMapping(after, s, SnapshotProgress{Plan: &plan, RestoredDisks: restoredDisks(s), RestoredMediaCopies: []MediaCopy{restored}})
	if err != nil || len(after.RetiredMediaCopies) != 1 || len(after.RetiredDisks) != len(plan.Cleanup) {
		t.Fatal("mapping replay duplicated retired refs", err)
	}
}

func TestRestorePreviewIgnoresNestedStorageDiagnostics(t *testing.T) {
	v, s := snapshotFixture(t)
	original, err := PreviewSnapshotRestore(v, s)
	if err != nil {
		t.Fatal(err)
	}
	s.Content = ObjectRef{Kind: "VirtualMachineSnapshotContent", Name: "changed", UID: "changed"}
	s.Artifacts = []ObjectRef{{Kind: "VolumeSnapshotContent", Name: "new-diagnostic"}}
	for i := range s.Required {
		s.Required[i].Class = ObjectRef{Name: "new-class"}
		s.Required[i].VolumeSnapshot = ObjectRef{Name: "new-vs"}
	}
	for i := range s.Inventory {
		s.Inventory[i].Cleanup = []ObjectRef{{Kind: "PersistentVolume", Name: "new-pv"}}
		s.Inventory[i].Reason = "new diagnostic"
	}
	for i := range v.Disks {
		v.Disks[i].Cleanup = []ObjectRef{{Kind: "PersistentVolume", Name: "other-pv"}}
		v.Disks[i].Preparation = "diagnostic"
	}
	same, err := PreviewSnapshotRestore(v, s)
	if err != nil || same.Digest != original.Digest {
		t.Fatal("diagnostics changed confirmation", same, err)
	}
	v.Disks[0].Cleanup = append(v.Disks[0].Cleanup, ObjectRef{Kind: "PersistentVolumeClaim", Namespace: v.Spec.Namespace, Name: "actual-extra-cleanup", UID: "extra-pvc"})
	extra, err := PreviewSnapshotRestore(v, s)
	if err != nil || extra.Digest == original.Digest {
		t.Fatal("business cleanup omitted from confirmation", extra, err)
	}
	v.Disks[0].PVC.UID = "real-new-volume"
	changed, err := PreviewSnapshotRestore(v, s)
	if err != nil || changed.Digest == original.Digest {
		t.Fatal("actual cleanup identity omitted", changed, err)
	}
}

type savedRestoreRepository struct {
	Repository
	saved Operation
}

func (r *savedRestoreRepository) GetOperation(context.Context, string, string, string) (Operation, error) {
	return r.saved, nil
}

type lostRestoreBoundary struct{ restoreBoundary }

func (n *lostRestoreBoundary) ObserveRestore(context.Context, VM, Snapshot, ObjectRef) (RestoreObservation, error) {
	return RestoreObservation{}, ErrUnknown
}

func TestRestoreCleanupReusesDurableLegacyOutputsWithoutRewritingPlan(t *testing.T) {
	v, s := snapshotFixture(t)
	plan, err := PreviewSnapshotRestore(v, s)
	if err != nil {
		t.Fatal(err)
	}
	// An accepted legacy digest is intentionally different from the new preview.
	plan.Digest = "accepted-legacy-digest"
	id := "40000000-0000-4000-8000-000000000001"
	ref := ObjectRef{Kind: "VirtualMachineRestore", Namespace: v.Spec.Namespace, Name: "vm-restore-" + id, UID: "registered-restore"}
	p := SnapshotProgress{Plan: &plan, NativeRestore: ref, NativeRestoreCompleted: true, RestoredDisks: restoredDisks(s)}
	repo := &savedRestoreRepository{saved: Operation{ID: id, Snapshot: &p}}
	native := &lostRestoreBoundary{}
	worker := &Worker{repo: repo, native: native}
	s.Restores = []ObjectRef{ref}
	if err := worker.observeRestoreCleanup(context.Background(), &v, &s); err != nil {
		t.Fatal(err)
	}
	if len(s.RestoreOutputsRecorded) != 1 || len(v.RetiredDisks) != len(s.Inventory) || p.Plan.Digest != "accepted-legacy-digest" {
		t.Fatal("legacy output/intent lost")
	}
	repo.saved.Snapshot.NativeRestoreCompleted = false
	s.RestoreOutputsRecorded = nil
	if err := worker.observeRestoreCleanup(context.Background(), &v, &s); !errors.Is(err, ErrUnknown) {
		t.Fatal("unknown legacy output inferred success", err)
	}
}
