package vm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

type SnapshotVolume struct {
	Name, Role, DiskID, MediaCopyID string
	PVC, Storage, Class             ObjectRef
	VolumeSnapshot                  ObjectRef
}

type Snapshot struct {
	ID, Name, SourceVMID, SourceVMUID string
	Native, Content                   ObjectRef
	Inventory                         []Disk
	Required                          []SnapshotVolume
	MediaSlots                        []MediaSlot
	MediaCopies                       []MediaCopy
	BootDevice                        string
	Artifacts, Restores               []ObjectRef
	// RestoreOutputsRecorded marks a checkpointed named-output handoff.
	RestoreOutputsRecorded             []ObjectRef
	Ready, Complete, Failed, Tombstone bool
	Included, Excluded, Indications    []string
	Reason                             string
	ObservedAt, CreatedAt              time.Time
}

type RestoreDiskImpact struct{ DiskID, Action, Reason string }
type RestorePlan struct {
	Version                 int64
	SnapshotID, SnapshotUID string
	SourceVMUID, Digest     string
	Impacts                 []RestoreDiskImpact
	Cleanup                 []Disk
	CleanupMedia            []MediaCopy
}
type RestoreObservation struct {
	Native                   ObjectRef
	Complete, Failed, Absent bool
	OutputsKnown             bool
	Disks                    []Disk
	MediaCopies              []MediaCopy
	BackendStates            []BackendState
	Reason                   string
}
type SnapshotProgress struct {
	SnapshotID             string
	Plan                   *RestorePlan
	NativeRestore          ObjectRef
	NativeRestoreCompleted bool
	MappingCommitted       bool
	CleanupCompleted       bool
	RestoredDisks          []Disk
	RestoredMediaCopies    []MediaCopy
}

// Snapshot operations use native VM snapshot/restore resources. This optional
// port leaves existing VM and Instance native consumers unchanged.
type SnapshotNativePort interface {
	PrepareSnapshot(context.Context, VM, Snapshot) (Snapshot, error)
	EnsureSnapshot(context.Context, VM, Snapshot) (Snapshot, error)
	ObserveSnapshot(context.Context, VM, Snapshot) (Snapshot, error)
	DeleteSnapshot(context.Context, VM, Snapshot) (bool, error)
	EnsureRestore(context.Context, VM, Snapshot, ObjectRef) (RestoreObservation, error)
	ApplyRestoredConfiguration(context.Context, VM, Snapshot) (bool, error)
}

// Retired volumes are observed separately from the restored current mapping.
// The adapter preserves exact business DV/PVC identities. PVs are diagnostics.
type RestoreObserver interface {
	ObserveRestore(context.Context, VM, Snapshot, ObjectRef) (RestoreObservation, error)
}

type RetiredVolumeNativePort interface {
	ObserveRetiredVolume(context.Context, VM, Disk) (Disk, error)
}

type snapshotIntent struct {
	Name                    string `json:"name"`
	SnapshotID              string `json:"snapshot_id"`
	PreviewDigest           string `json:"preview_digest"`
	DestructiveConfirmation bool   `json:"destructive_confirmation"`
}

func snapshotCommand(raw json.RawMessage) (snapshotIntent, error) {
	var intent snapshotIntent
	if json.Unmarshal(raw, &intent) != nil {
		return intent, ErrInvalid
	}
	return intent, nil
}

func FindSnapshot(v VM, id string) (int, error) {
	for i := range v.Snapshots {
		if v.Snapshots[i].ID == id && !v.Snapshots[i].Tombstone {
			return i, nil
		}
	}
	return -1, ErrNotFound
}

// ValidateSnapshotCommand is also called with the locked VM before admission.
func ValidateSnapshotCommand(v VM, action string, raw json.RawMessage) error {
	if action != "create-snapshot" && action != "delete-snapshot" && action != "restore-snapshot" {
		return nil
	}
	i, err := snapshotCommand(raw)
	if err != nil {
		return err
	}
	if v.Native.UID == "" {
		return ErrDependency
	}
	if action == "create-snapshot" {
		if strings.TrimSpace(i.Name) == "" || len(i.Name) > 128 {
			return ErrInvalid
		}
		return nil
	}
	if !UUID(i.SnapshotID) {
		return ErrInvalid
	}
	index, err := FindSnapshot(v, i.SnapshotID)
	if err != nil {
		return err
	}
	s := v.Snapshots[index]
	if s.SourceVMID != v.ID || s.SourceVMUID != v.Native.UID {
		return ErrOwnership
	}
	if action == "restore-snapshot" {
		if !i.DestructiveConfirmation || len(i.PreviewDigest) != 64 {
			return ErrInvalid
		}
		plan, err := PreviewSnapshotRestore(v, s)
		if err != nil {
			return err
		}
		if plan.Digest != i.PreviewDigest {
			return ErrChanged
		}
	}
	return nil
}

func NewSnapshot(v VM, operationID, name string) (Snapshot, error) {
	if !UUID(operationID) || v.Native.UID == "" || strings.TrimSpace(name) == "" || len(name) > 128 {
		return Snapshot{}, ErrInvalid
	}
	id := uuid.NewSHA1(uuid.NameSpaceOID, []byte("ani-vm-snapshot/"+v.ID+"/"+operationID)).String()
	s := Snapshot{ID: id, Name: name, SourceVMID: v.ID, SourceVMUID: v.Native.UID,
		Native:     ObjectRef{Kind: "VirtualMachineSnapshot", Namespace: v.Spec.Namespace, Name: "vm-snap-" + id},
		MediaSlots: append([]MediaSlot(nil), v.MediaSlots...), BootDevice: v.CurrentBootDevice}
	if s.BootDevice == "" {
		s.BootDevice = v.Spec.BootDevice
	}
	root := false
	for _, d := range v.Disks {
		if d.Tombstone || !d.Attached {
			continue
		}
		if d.Role != "system" && d.Role != "data" {
			return Snapshot{}, ErrChanged
		}
		if d.PVC.UID == "" || d.PVC.Name == "" {
			return Snapshot{}, ErrDependency
		}
		if d.Role == "system" {
			root = true
		}
		d.Cleanup = append([]ObjectRef(nil), d.Cleanup...)
		s.Inventory = append(s.Inventory, d)
		s.Required = append(s.Required, SnapshotVolume{Name: d.VolumeName, Role: d.Role, DiskID: d.ID, PVC: d.PVC})
	}
	if !root {
		return Snapshot{}, ErrDependency
	}
	for _, slot := range s.MediaSlots {
		if slot.DesiredCopyID == "" {
			continue
		}
		copy, ok := FindMediaCopy(v, slot.DesiredCopyID)
		if !ok || copy.PVC.UID == "" {
			return Snapshot{}, ErrDependency
		}
		name := "media-install"
		if slot.Slot == "DRIVER" {
			name = "media-driver"
		} else if slot.Slot != "INSTALL" {
			return Snapshot{}, ErrChanged
		}
		s.MediaCopies = append(s.MediaCopies, copy)
		s.Required = append(s.Required, SnapshotVolume{Name: name, Role: "media", MediaCopyID: copy.ID, PVC: copy.PVC})
	}
	sort.Slice(s.Inventory, func(i, j int) bool { return s.Inventory[i].ID < s.Inventory[j].ID })
	return s, nil
}

func SnapshotComplete(s Snapshot) bool {
	if !s.Ready || s.Failed || s.Tombstone || s.Native.UID == "" || len(s.Required) == 0 {
		return false
	}
	included := map[string]bool{}
	for _, name := range s.Included {
		included[name] = true
	}
	excluded := map[string]bool{}
	for _, name := range s.Excluded {
		excluded[name] = true
	}
	for _, volume := range s.Required {
		if !included[volume.Name] || excluded[volume.Name] {
			return false
		}
	}
	return true
}

// PreviewSnapshotRestore freezes all five logical disk sets, including removed
// disks retained only in the snapshot inventory. Observation time is excluded
// from the digest. Only business identities, intent and actual affected DV/PVCs
// participate; native storage diagnostics never change the confirmation.
func PreviewSnapshotRestore(v VM, s Snapshot) (RestorePlan, error) {
	plan := RestorePlan{Version: v.Version, SnapshotID: s.ID, SnapshotUID: s.Native.UID, SourceVMUID: v.Native.UID}
	if v.Closing {
		return plan, ErrBusy
	}
	if s.SourceVMID != v.ID || s.SourceVMUID != v.Native.UID || v.Native.UID == "" {
		return plan, ErrOwnership
	}
	if !s.Complete || !SnapshotComplete(s) {
		return plan, ErrDependency
	}
	inSnapshot := map[string]Disk{}
	for _, disk := range s.Inventory {
		if disk.ID == "" || disk.VolumeName == "" || disk.Serial == "" || disk.Tombstone || !disk.Attached {
			return plan, ErrChanged
		}
		if _, duplicate := inSnapshot[disk.ID]; duplicate {
			return plan, ErrChanged
		}
		inSnapshot[disk.ID] = disk
	}
	current := map[string]Disk{}
	for _, disk := range v.Disks {
		if _, duplicate := current[disk.ID]; duplicate {
			return plan, ErrChanged
		}
		current[disk.ID] = disk
	}
	for _, frozen := range s.Inventory {
		action, reason := "restore-deleted", "snapshot disk was deleted; restore its original logical identity"
		if disk, exists := current[frozen.ID]; exists && !disk.Tombstone {
			if disk.VolumeName != frozen.VolumeName || disk.Serial != frozen.Serial || disk.Role != frozen.Role {
				return plan, ErrOwnership
			}
			action, reason = "restore-detached", "restore and attach the disk recorded by the snapshot"
			if disk.Attached {
				action, reason = "restore-attached", "replace current contents and physical volume"
			}
			plan.Cleanup = append(plan.Cleanup, disk)
		}
		plan.Impacts = append(plan.Impacts, RestoreDiskImpact{frozen.ID, action, reason})
	}
	for _, disk := range v.Disks {
		if disk.Tombstone {
			continue
		}
		if _, exists := inSnapshot[disk.ID]; exists {
			continue
		}
		if disk.Role != "data" {
			return plan, ErrChanged
		}
		action, reason := "retain-detached", "disk outside snapshot remains owned and detached"
		if disk.Attached {
			action, reason = "delete-added-attached", "disk added after snapshot is removed after successful restore"
			plan.Cleanup = append(plan.Cleanup, disk)
		}
		plan.Impacts = append(plan.Impacts, RestoreDiskImpact{disk.ID, action, reason})
	}
	for _, copy := range s.MediaCopies {
		current, found := FindMediaCopy(v, copy.ID)
		if found {
			if current.SourceDigest != copy.SourceDigest {
				return plan, ErrOwnership
			}
			plan.CleanupMedia = append(plan.CleanupMedia, current)
		}
	}
	sort.Slice(plan.Impacts, func(i, j int) bool { return plan.Impacts[i].DiskID < plan.Impacts[j].DiskID })
	sort.Slice(plan.Cleanup, func(i, j int) bool { return plan.Cleanup[i].ID < plan.Cleanup[j].ID })
	sort.Slice(plan.CleanupMedia, func(i, j int) bool { return plan.CleanupMedia[i].ID < plan.CleanupMedia[j].ID })
	// Use an explicit projection; nested inventory/cleanup structures cannot
	// accidentally reintroduce Content/Class/PV or observation timestamps.
	businessRefs := func(items []ObjectRef) []ObjectRef {
		refs := []ObjectRef{}
		for _, ref := range items {
			switch ref.Kind {
			case "DataVolume", "PersistentVolumeClaim", "Job", "Secret":
				refs = append(refs, ref)
			}
		}
		sort.Slice(refs, func(i, j int) bool {
			a, b := refs[i], refs[j]
			return strings.Join([]string{a.Kind, a.Namespace, a.Name, a.UID}, "/") < strings.Join([]string{b.Kind, b.Namespace, b.Name, b.UID}, "/")
		})
		return refs
	}
	disks := func(items []Disk) []any {
		items = append([]Disk(nil), items...)
		sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
		result := []any{}
		for _, d := range items {
			result = append(result, []any{d.ID, d.Role, d.Name, d.SizeGB, d.RequestedBytes, d.VolumeName, d.Serial, d.Attached, d.Tombstone, d.DV, d.PVC, businessRefs(d.Cleanup)})
		}
		return result
	}
	media := func(items []MediaCopy) []any {
		items = append([]MediaCopy(nil), items...)
		sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
		result := []any{}
		for _, c := range items {
			result = append(result, []any{c.ID, c.SourceDigest, c.DV, c.PVC, c.Tombstone, businessRefs(c.Cleanup)})
		}
		return result
	}
	slots := func(items []MediaSlot) []any {
		items = append([]MediaSlot(nil), items...)
		sort.Slice(items, func(i, j int) bool { return items[i].Slot < items[j].Slot })
		result := []any{}
		for _, slot := range items {
			result = append(result, []string{slot.Slot, slot.DesiredCopyID})
		}
		return result
	}
	identity := []any{v.Version, v.ID, v.Native.UID, s.ID, s.Native.UID,
		disks(s.Inventory), disks(v.Disks), disks(plan.Cleanup),
		media(s.MediaCopies), media(plan.CleanupMedia), slots(s.MediaSlots),
		slots(v.MediaSlots), s.BootDevice, v.CurrentBootDevice, plan.Impacts}
	body, err := json.Marshal(identity)
	if err != nil {
		return plan, err
	}
	digest := sha256.Sum256(body)
	plan.Digest = hex.EncodeToString(digest[:])
	return plan, nil
}

func applyRestoreMapping(v VM, s Snapshot, p SnapshotProgress) (VM, error) {
	if p.Plan == nil || len(p.RestoredDisks) != len(s.Inventory) {
		return v, ErrChanged
	}
	result := v
	result.Disks = append([]Disk(nil), v.Disks...)
	result.MediaCopies = append([]MediaCopy(nil), v.MediaCopies...)
	result.RetiredDisks = nil
	for _, old := range v.RetiredDisks {
		isCurrent := false
		for _, disk := range p.RestoredDisks {
			if old.PVC == disk.PVC {
				isCurrent = true
				break
			}
		}
		if !isCurrent {
			result.RetiredDisks = append(result.RetiredDisks, old)
		}
	}
	result.RetiredMediaCopies = nil
	for _, old := range v.RetiredMediaCopies {
		isCurrent := false
		for _, copy := range p.RestoredMediaCopies {
			if old.PVC == copy.PVC {
				isCurrent = true
				break
			}
		}
		if !isCurrent {
			result.RetiredMediaCopies = append(result.RetiredMediaCopies, old)
		}
	}
	frozen := map[string]Disk{}
	for _, d := range s.Inventory {
		frozen[d.ID] = d
	}
	seen := map[string]bool{}
	for _, disk := range p.RestoredDisks {
		original, ok := frozen[disk.ID]
		if !ok || seen[disk.ID] || disk.VolumeName != original.VolumeName || disk.Serial != original.Serial || disk.Role != original.Role || disk.PVC.UID == "" || disk.PVC.Namespace != v.Spec.Namespace || disk.DV.Name != "" || disk.DV.UID != "" {
			return v, ErrOwnership
		}
		seen[disk.ID] = true
		disk.Attached = true
		disk.Tombstone = false
		index := -1
		for i := range result.Disks {
			if result.Disks[i].ID == disk.ID {
				index = i
				break
			}
		}
		if index < 0 {
			result.Disks = append(result.Disks, disk)
		} else {
			result.Disks[index] = disk
		}
	}
	for i := range result.Disks {
		if _, ok := frozen[result.Disks[i].ID]; !ok && result.Disks[i].Attached && !result.Disks[i].Tombstone {
			result.Disks[i].Tombstone = true
			result.Disks[i].Attached = false
		}
	}
	if len(p.RestoredMediaCopies) != len(s.MediaCopies) {
		return v, ErrChanged
	}
	for _, copy := range p.RestoredMediaCopies {
		valid := false
		for _, original := range s.MediaCopies {
			if original.ID == copy.ID && original.SourceDigest == copy.SourceDigest && reflect.DeepEqual(original.Source, copy.Source) {
				valid = true
				break
			}
		}
		if !valid || copy.PVC.UID == "" || copy.PVC.Namespace != v.Spec.Namespace || copy.DV.Name != "" || copy.DV.UID != "" {
			return v, ErrOwnership
		}
		index := -1
		for i := range result.MediaCopies {
			if result.MediaCopies[i].ID == copy.ID {
				if result.MediaCopies[i].SourceDigest != copy.SourceDigest {
					return v, ErrOwnership
				}
				index = i
				break
			}
		}
		if index < 0 {
			result.MediaCopies = append(result.MediaCopies, copy)
		} else {
			result.MediaCopies[index] = copy
		}
	}
	// Publish old physical refs with the new mapping in the same checkpoint.
	// A later DELETE superseding this operation must retain its cleanup work.
	for _, old := range p.Plan.Cleanup {
		if old.PVC.UID == "" {
			return v, ErrOwnership
		}
		for _, current := range p.RestoredDisks {
			if old.PVC.UID == current.PVC.UID {
				return v, ErrOwnership
			}
		}
		found := false
		for _, retired := range result.RetiredDisks {
			if retired.PVC.UID == old.PVC.UID {
				if retired.PVC != old.PVC || retired.DV != old.DV {
					return v, ErrOwnership
				}
				found = true
				break
			}
		}
		if !found {
			old.Cleanup = append([]ObjectRef(nil), old.Cleanup...)
			result.RetiredDisks = append(result.RetiredDisks, old)
		}
	}
	for _, old := range p.Plan.CleanupMedia {
		if old.PVC.UID == "" {
			return v, ErrOwnership
		}
		for _, current := range p.RestoredMediaCopies {
			if old.PVC.UID == current.PVC.UID {
				return v, ErrOwnership
			}
		}
		found := false
		for _, retired := range result.RetiredMediaCopies {
			if retired.PVC.UID == old.PVC.UID {
				if retired.PVC != old.PVC || retired.DV != old.DV {
					return v, ErrOwnership
				}
				found = true
				break
			}
		}
		if !found {
			old.Cleanup = append([]ObjectRef(nil), old.Cleanup...)
			result.RetiredMediaCopies = append(result.RetiredMediaCopies, old)
		}
	}
	result.MediaSlots = append([]MediaSlot(nil), s.MediaSlots...)
	result.CurrentBootDevice = s.BootDevice
	for i := range result.MediaSlots {
		result.MediaSlots[i].ObservedCopyID = ""
		result.MediaSlots[i].EffectiveNow = false
		result.MediaSlots[i].PendingRestart = false
	}
	result.Desired = "Stopped"
	result.Observed = "Stopped"
	result.Addresses = nil
	return result, nil
}

func (w *Worker) snapshot(ctx context.Context, o *Operation, v *VM) error {
	native, ok := w.native.(SnapshotNativePort)
	if !ok {
		return ErrDependency
	}
	intent, err := snapshotCommand(o.Intent)
	if err != nil {
		return err
	}
	if o.Action == "create-snapshot" {
		if o.Snapshot == nil {
			x, err := w.native.Observe(ctx, *v)
			if err != nil {
				return err
			}
			if x.VM.UID != v.Native.UID {
				return ErrOwnership
			}
			v.BackendStates = x.BackendStates
			for i := range v.Disks {
				for _, disk := range x.Volumes {
					if v.Disks[i].ID == disk.ID {
						v.Disks[i] = disk
					}
				}
			}
			s, err := NewSnapshot(*v, o.ID, intent.Name)
			if err != nil {
				return err
			}
			s, err = native.PrepareSnapshot(ctx, *v, s)
			if err != nil {
				return err
			}
			v.Snapshots = append(v.Snapshots, s)
			o.Snapshot = &SnapshotProgress{SnapshotID: s.ID}
			return w.checkpoint(ctx, o, *v, "create-snapshot", s.Native)
		}
		index, err := FindSnapshot(*v, o.Snapshot.SnapshotID)
		if err != nil {
			return err
		}
		s, err := native.EnsureSnapshot(ctx, *v, v.Snapshots[index])
		if err != nil {
			return err
		}
		s.Complete = SnapshotComplete(s)
		v.Snapshots[index] = s
		o.Pending = nil
		if s.Failed {
			return fmt.Errorf("%w: %s", ErrChanged, s.Reason)
		}
		if s.Ready {
			o.State = "succeeded"
		}
		return nil
	}
	index, err := FindSnapshot(*v, intent.SnapshotID)
	if err != nil {
		return err
	}
	s := v.Snapshots[index]
	if o.Action == "delete-snapshot" {
		if err := w.observeRestoreCleanup(ctx, v, &s); err != nil {
			return err
		}
		observed, err := native.ObserveSnapshot(ctx, *v, s)
		if err != nil {
			return err
		}
		v.Snapshots[index] = observed
		if err = w.checkpoint(ctx, o, *v, "delete-snapshot", observed.Native); err != nil {
			return err
		}
		done, err := native.DeleteSnapshot(ctx, *v, observed)
		if err != nil {
			return err
		}
		if done {
			v.Snapshots[index].Tombstone = true
			o.Pending = nil
			o.State = "succeeded"
		}
		return nil
	}
	if o.Action != "restore-snapshot" {
		return ErrInvalid
	}
	if !intent.DestructiveConfirmation {
		return ErrInvalid
	}
	x, err := w.observeLifecycle(ctx, *v)
	if err != nil {
		return err
	}
	if x.VM.UID != v.Native.UID {
		return ErrOwnership
	}
	if x.VMI != nil || x.Launcher != nil || x.RelatedPodsPresent || !x.Stopped {
		return ErrBusy
	}
	if o.Snapshot == nil {
		fresh, err := native.ObserveSnapshot(ctx, *v, s)
		if err != nil {
			return err
		}
		fresh.Complete = SnapshotComplete(fresh)
		previewVM := *v
		previewVM.Version = o.IntentVersion - 1
		plan, err := PreviewSnapshotRestore(previewVM, fresh)
		if err != nil {
			return err
		}
		if plan.Digest != intent.PreviewDigest {
			return ErrChanged
		}
		ref := ObjectRef{Kind: "VirtualMachineRestore", Namespace: v.Spec.Namespace, Name: "vm-restore-" + o.ID}
		o.Snapshot = &SnapshotProgress{SnapshotID: s.ID, Plan: &plan, NativeRestore: ref}
		v.Snapshots[index].Restores = append(v.Snapshots[index].Restores, ref)
		return w.checkpoint(ctx, o, *v, "restore-snapshot", ref)
	}
	p := o.Snapshot
	if p.Plan == nil || p.Plan.SourceVMUID != v.Native.UID || p.Plan.SnapshotUID != s.Native.UID {
		return ErrOwnership
	}
	if !p.NativeRestoreCompleted {
		observed, err := native.EnsureRestore(ctx, *v, s, p.NativeRestore)
		if err != nil {
			return err
		}
		p.NativeRestore = observed.Native
		for i := range v.Snapshots[index].Restores {
			if v.Snapshots[index].Restores[i].Name == observed.Native.Name {
				v.Snapshots[index].Restores[i] = observed.Native
			}
		}
		if observed.Failed {
			return fmt.Errorf("%w: %s", ErrChanged, observed.Reason)
		}
		if err = recordRestoreVolumes(v, observed); err != nil {
			return err
		}
		if !observed.Complete {
			return nil
		}
		p.RestoredDisks = observed.Disks
		p.RestoredMediaCopies = observed.MediaCopies
		p.NativeRestoreCompleted = true
		o.Step = "native_restore_completed"
		o.Pending = nil
		return w.repo.Checkpoint(ctx, *o, *v)
	}
	if !p.MappingCommitted {
		candidate, err := applyRestoreMapping(*v, s, *p)
		if err != nil {
			return err
		}
		done, err := native.ApplyRestoredConfiguration(ctx, candidate, s)
		if err != nil {
			return err
		}
		if !done {
			return nil
		}
		*v = candidate
		p.MappingCommitted = true
		o.Step = "mapping_committed"
		return w.repo.Checkpoint(ctx, *o, *v)
	}
	if !p.CleanupCompleted {
		done, err := w.removeRetiredVolumes(ctx, o, v)
		if err != nil {
			return err
		}
		if !done {
			return fmt.Errorf("%w: VM_RESTORE_OLD_VOLUME_CLEANUP", ErrDependency)
		}
		p.CleanupCompleted = true
		o.Step = "cleanup_completed"
		return w.repo.Checkpoint(ctx, *o, *v)
	}
	o.State = "succeeded"
	o.Pending = nil
	return nil
}

func (w *Worker) removeSnapshots(ctx context.Context, o *Operation, v *VM) (bool, error) {
	for i, s := range v.Snapshots {
		if s.Tombstone {
			continue
		}
		native, ok := w.native.(SnapshotNativePort)
		if !ok {
			return false, ErrDependency
		}
		if err := w.observeRestoreCleanup(ctx, v, &s); err != nil {
			return false, err
		}
		observed, err := native.ObserveSnapshot(ctx, *v, s)
		if err != nil {
			return false, err
		}
		v.Snapshots[i] = observed
		if err = w.checkpoint(ctx, o, *v, "delete-snapshot", observed.Native); err != nil {
			return false, err
		}
		done, err := native.DeleteSnapshot(ctx, *v, observed)
		if err != nil {
			return false, err
		}
		if done {
			v.Snapshots[i].Tombstone = true
			o.Pending = nil
		}
		return false, nil
	}
	return true, nil
}

// Both restore and superseding VM deletion drain this aggregate inventory. A
// false native result retains the ref until its business DV/PVC lifecycle exits.
func (w *Worker) removeRetiredVolumes(ctx context.Context, o *Operation, v *VM) (bool, error) {
	if len(v.RetiredDisks) == 0 && len(v.RetiredMediaCopies) == 0 {
		return true, nil
	}
	native, ok := w.native.(RetiredVolumeNativePort)
	if !ok {
		return false, ErrDependency
	}
	disk := Disk{}
	media := len(v.RetiredDisks) == 0
	if media {
		copy := v.RetiredMediaCopies[0]
		disk = Disk{ID: copy.ID, Role: "media", DV: copy.DV, PVC: copy.PVC, Cleanup: copy.Cleanup}
	} else {
		disk = v.RetiredDisks[0]
	}
	if disk.PVC.Name == "" || disk.PVC.Namespace != v.Spec.Namespace {
		return false, ErrOwnership
	}
	for _, current := range v.Disks {
		if !current.Tombstone && current.PVC.UID == disk.PVC.UID {
			return false, ErrOwnership
		}
	}
	for _, current := range v.MediaCopies {
		if !current.Tombstone && current.PVC.UID == disk.PVC.UID {
			return false, ErrOwnership
		}
	}
	observed, err := native.ObserveRetiredVolume(ctx, *v, disk)
	if err != nil {
		return false, err
	}
	if observed.PVC.Kind != disk.PVC.Kind || observed.PVC.Namespace != disk.PVC.Namespace || observed.PVC.Name != disk.PVC.Name || (disk.PVC.UID != "" && observed.PVC.UID != disk.PVC.UID) || observed.DV != disk.DV {
		return false, ErrOwnership
	}
	if media {
		copy := &v.RetiredMediaCopies[0]
		copy.Cleanup = observed.Cleanup
		copy.PVC = observed.PVC
	} else {
		v.RetiredDisks[0] = observed
	}
	if err = w.checkpoint(ctx, o, *v, "delete-retired-volume", observed.PVC); err != nil {
		return false, err
	}
	done, err := w.native.DeleteVolume(ctx, *v, observed)
	if err != nil {
		return false, err
	}
	if !done {
		return false, nil
	}
	if media {
		v.RetiredMediaCopies = v.RetiredMediaCopies[1:]
	} else {
		v.RetiredDisks = v.RetiredDisks[1:]
	}
	o.Pending = nil
	if err = w.repo.Checkpoint(ctx, *o, *v); err != nil {
		return false, err
	}
	return false, nil
}

// mergeRegisteredRef preserves a registered identity when an observation has
// no UID. A different observed UID is still a replacement, never an adoption.
func mergeRegisteredRef(old, observed ObjectRef) (ObjectRef, error) {
	if old.Kind != observed.Kind || old.Namespace != observed.Namespace || old.Name != observed.Name ||
		(old.UID != "" && observed.UID != "" && old.UID != observed.UID) {
		return ObjectRef{}, ErrOwnership
	}
	if observed.UID == "" {
		return old, nil
	}
	return observed, nil
}

func restoreCleanupRefs(old, observed []ObjectRef) []ObjectRef {
	result := append([]ObjectRef(nil), old...)
	for _, ref := range observed {
		found := false
		for _, saved := range result {
			if saved == ref {
				found = true
				break
			}
		}
		if !found {
			result = append(result, ref)
		}
	}
	return result
}

// Native completion may precede the mapping checkpoint (or its ACK). Keep all
// newly observed physical refs in the aggregate before either operation wins.
func recordRestoreVolumes(v *VM, x RestoreObservation) error {
	for _, disk := range x.Disks {
		if disk.PVC.Kind != "PersistentVolumeClaim" || disk.PVC.Name == "" || disk.PVC.Namespace != v.Spec.Namespace || disk.DV.Name != "" || disk.DV.UID != "" {
			return ErrOwnership
		}
		current := false
		for _, d := range v.Disks {
			if d.PVC.Namespace == disk.PVC.Namespace && d.PVC.Name == disk.PVC.Name {
				if _, err := mergeRegisteredRef(d.PVC, disk.PVC); err != nil {
					return err
				}
				current = true
				break
			}
		}
		if current {
			continue
		}
		found := false
		for i, old := range v.RetiredDisks {
			if old.PVC.Namespace == disk.PVC.Namespace && old.PVC.Name == disk.PVC.Name {
				ref, err := mergeRegisteredRef(old.PVC, disk.PVC)
				if err != nil {
					return err
				}
				if disk.PVC.UID != "" {
					disk.PVC = ref
					disk.Cleanup = restoreCleanupRefs(old.Cleanup, disk.Cleanup)
					v.RetiredDisks[i] = disk
				}
				found = true
				break
			}
		}
		if !found {
			v.RetiredDisks = append(v.RetiredDisks, disk)
		}
	}
	for _, copy := range x.MediaCopies {
		if copy.PVC.Kind != "PersistentVolumeClaim" || copy.PVC.Name == "" || copy.PVC.Namespace != v.Spec.Namespace || copy.DV.Name != "" || copy.DV.UID != "" {
			return ErrOwnership
		}
		current := false
		for _, c := range v.MediaCopies {
			if c.PVC.Namespace == copy.PVC.Namespace && c.PVC.Name == copy.PVC.Name {
				if _, err := mergeRegisteredRef(c.PVC, copy.PVC); err != nil {
					return err
				}
				current = true
				break
			}
		}
		if current {
			continue
		}
		found := false
		for i, old := range v.RetiredMediaCopies {
			if old.PVC.Namespace == copy.PVC.Namespace && old.PVC.Name == copy.PVC.Name {
				ref, err := mergeRegisteredRef(old.PVC, copy.PVC)
				if err != nil {
					return err
				}
				if copy.PVC.UID != "" {
					copy.PVC = ref
					copy.Cleanup = restoreCleanupRefs(old.Cleanup, copy.Cleanup)
					v.RetiredMediaCopies[i] = copy
				}
				found = true
				break
			}
		}
		if !found {
			v.RetiredMediaCopies = append(v.RetiredMediaCopies, copy)
		}
	}
	for _, state := range x.BackendStates {
		if state.PVC.Kind != "PersistentVolumeClaim" || state.PVC.Name == "" || state.Owner != v.Native || state.PVC.Namespace != v.Spec.Namespace {
			return ErrOwnership
		}
		found := false
		for i, old := range v.BackendStates {
			if old.PVC.Namespace == state.PVC.Namespace && old.PVC.Name == state.PVC.Name {
				if old.Owner != v.Native {
					return ErrOwnership
				}
				ref, err := mergeRegisteredRef(old.PVC, state.PVC)
				if err != nil {
					return err
				}
				if state.PVC.UID == "" {
					state = old
					state.Current, state.Present = false, false
				} else {
					state.PVC = ref
					// Legacy lower-layer fields remain diagnostics for the same identity.
					state.PV, state.ReclaimPolicy, state.CSIDriver, state.VolumeHandle = old.PV, old.ReclaimPolicy, old.CSIDriver, old.VolumeHandle
				}
				v.BackendStates[i] = state
				found = true
				break
			}
		}
		if !found {
			v.BackendStates = append(v.BackendStates, state)
		}
	}
	return nil
}
func (w *Worker) observeRestoreCleanup(ctx context.Context, v *VM, s *Snapshot) error {
	if len(s.Restores) == 0 {
		return nil
	}
	observer, ok := w.native.(RestoreObserver)
	if !ok {
		return ErrDependency
	}
	for i, ref := range s.Restores {
		observed, err := observer.ObserveRestore(ctx, *v, *s, ref)
		if errors.Is(err, ErrUnknown) {
			// Compatibility for a registered old Restore whose complete outputs
			// were already checkpointed before its CR disappeared. The original
			// operation is durable output, not a native marker or namespace scan.
			id := strings.TrimPrefix(ref.Name, "vm-restore-")
			if id != ref.Name && UUID(id) {
				old, readErr := w.repo.GetOperation(ctx, v.TenantID, v.ID, id)
				if readErr != nil {
					return readErr
				}
				p := old.Snapshot
				if p != nil && p.NativeRestore == ref && ref.UID != "" && p.NativeRestoreCompleted && p.Plan != nil &&
					p.Plan.SourceVMUID == v.Native.UID && p.Plan.SnapshotUID == s.Native.UID &&
					len(p.RestoredDisks) == len(s.Inventory) && len(p.RestoredMediaCopies) == len(s.MediaCopies) {
					observed = RestoreObservation{Native: ref, Complete: true, Absent: true, OutputsKnown: true,
						Disks: p.RestoredDisks, MediaCopies: p.RestoredMediaCopies}
					err = nil
				}
			}
		}
		if err != nil {
			return err
		}
		if observed.Native.Name != ref.Name || observed.Native.Namespace != ref.Namespace || (ref.UID != "" && ref.UID != observed.Native.UID) {
			return ErrOwnership
		}
		s.Restores[i] = observed.Native
		if err = recordRestoreVolumes(v, observed); err != nil {
			return err
		}
		if !observed.OutputsKnown {
			return ErrUnknown
		}
		known := false
		for _, saved := range s.RestoreOutputsRecorded {
			if saved == observed.Native {
				known = true
			}
		}
		if !known {
			s.RestoreOutputsRecorded = append(s.RestoreOutputsRecorded, observed.Native)
		}
	}
	return nil
}
