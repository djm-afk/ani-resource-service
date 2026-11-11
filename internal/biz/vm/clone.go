package vm

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// CloneDisk is the frozen source disk plan. The source identity is only used to
// build the target disk identity; it is never registered as the target volume.
type CloneDisk struct {
	SourceDiskID string
	VolumeName   string
	Role         string
	Name         string
	SizeGB       int64
}

// CloneSource is the trusted business snapshot of one managed source VM.
type CloneSource struct {
	SourceVMID    string
	SourceName    string
	SourceVersion int64
	Native        ObjectRef
	Disks         []CloneDisk
}

// VolumeRef is a named clone output before it is registered as a business disk.
type VolumeRef struct {
	VolumeName, PVCName, PVCUID, DVName, DVUID string
}

// CloneProgress is the durable step state of one clone target. It is stored on
// the VM because it outlives a single operation.
type CloneProgress struct {
	Name                  string // fixed VirtualMachineClone name
	UID                   string // adopted clone UID; empty until confirmed
	Phase                 string // last observed native phase or condition reason
	RegistrationCompleted bool   // target adaptation and named disk registration committed
	CloneDeleteRequested  bool   // delete intent checkpointed
	// CancelHandled records that the source reference was already released by a
	// cancelled delete, so later delete ticks do not re-run the cancel hand-off.
	CancelHandled    bool
	PendingTargetRef *ObjectRef
	PendingVolumes   []VolumeRef
}

// CloneBlocker is one precise reason why a managed source cannot be cloned.
type CloneBlocker struct {
	// Kind is "UNSUPPORTED" for out-of-scope structure and "NOT_READY" for a
	// dependency that may recover.
	Kind, VolumeName, Reason string
}

// CloneEligibility is the source admission result. Unsupported sources never
// create an operation; not-ready sources keep the same recoverable error.
type CloneEligibility struct {
	Unsupported []CloneBlocker
	NotReady    []CloneBlocker
}

func (e CloneEligibility) Ready() bool {
	return len(e.Unsupported) == 0 && len(e.NotReady) == 0
}

// Error reports the first blocker with its volume name so the caller keeps the
// native reason instead of a generic dependency failure.
func (e CloneEligibility) Error() error {
	if len(e.Unsupported) == 0 && len(e.NotReady) == 0 {
		return nil
	}
	blockers := append(append([]CloneBlocker(nil), e.Unsupported...), e.NotReady...)
	return fmt.Errorf("%w: %s", ErrCloneSourceUnavailable, blockerText(blockers))
}

// UnsupportedError is the stable "cannot clone" answer. It is a failed
// precondition: retrying the same source never helps until it is changed.
func (e CloneEligibility) UnsupportedError() error {
	if len(e.Unsupported) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrCloneSourceUnavailable, blockerText(e.Unsupported))
}

// NotReadyError is the recoverable "not observed yet" answer. It must map to an
// unavailable dependency so the caller may retry the same accepted source.
func (e CloneEligibility) NotReadyError() error {
	if len(e.NotReady) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrCloneSourceNotReady, blockerText(e.NotReady))
}

// blockerText renders each blocker with its volume name, never a generic code.
func blockerText(blockers []CloneBlocker) string {
	parts := make([]string, 0, len(blockers))
	for _, b := range blockers {
		if b.VolumeName != "" {
			parts = append(parts, fmt.Sprintf("volume %s: %s", b.VolumeName, b.Reason))
			continue
		}
		parts = append(parts, b.Reason)
	}
	return strings.Join(parts, "; ")
}

// CloneRequest is the fixed native submission contract of one target.
type CloneRequest struct {
	Name         string
	OperationID  string
	TargetName   string
	SMBiosSerial string
	Source       ObjectRef
}

// CloneObservation is the read-only projection of one VirtualMachineClone.
type CloneObservation struct {
	Exists          bool
	UID             string
	ResourceVersion string
	Phase           string
	Pending         bool
	Succeeded       bool
	Failed          bool
	SnapshotName    string
	RestoreName     string
	Reason          string
	Target          ObjectRef
	Outputs         []VolumeRef
}

// CloneDeleteResult is one preconditioned clone delete attempt. A conflict
// carries the freshly re-read object so the caller re-evaluates its conditions
// instead of reusing the stale one.
type CloneDeleteResult struct {
	Gone     bool
	Conflict bool
	Current  CloneObservation
}

// CloneNativePort is the native adapter for the clone lifecycle. It is optional
// so existing native consumers are unaffected.
type CloneNativePort interface {
	EnsureClone(context.Context, VM, CloneRequest) (CloneObservation, error)
	ObserveClone(context.Context, VM, CloneRequest) (CloneObservation, error)
	// DeleteClone deletes by UID and, when given, resourceVersion. A conflict
	// returns the current observation; it is never reported as gone.
	DeleteClone(context.Context, VM, CloneRequest, string, string) (CloneDeleteResult, error)
	// ObserveSourceCloneEligibility is the full admission. The source business
	// record carries the seed and boot facts that the native object cannot prove.
	ObserveSourceCloneEligibility(context.Context, VM, VM) (CloneEligibility, error)
	// RecheckCloneSource is the cheap pre-submit recheck: same named source plus
	// native structure and snapshot capability only. It never re-reads PVC,
	// StorageClass or Secret because admission already froze those.
	RecheckCloneSource(context.Context, VM) (CloneEligibility, error)
	ApplyCloneTargetConfiguration(context.Context, VM, CloneRequest) ([]Disk, ObjectRef, error)
	// DeleteCloneTarget deletes the failed clone's own target VM and reports
	// whether it is gone. The target may not carry business labels, so this
	// check is by name and expected UID, not by the ordinary label matcher.
	DeleteCloneTarget(context.Context, VM, ObjectRef) (bool, error)
	// ObserveCloneTarget reads the frozen target name of this clone. It exists so a
	// cancel that deleted the clone can still take over a target the native restore
	// created after the last clone observation: deleting the clone never deletes
	// the target VM. NotFound returns an empty ObjectRef.
	ObserveCloneTarget(context.Context, VM, CloneRequest) (ObjectRef, error)
}

// CloneCleanupPort removes the residual named outputs of a failed clone. It is
// optional so native ports without clone support stay unaffected.
type CloneCleanupPort interface {
	// DeleteNamedVolumes returns the refs whose identity it adopted by the
	// registered name (previous UID empty), whether every owned volume is gone,
	// and any error. An output that exists but whose identity hand-off is still
	// incomplete is never reported as cleaned.
	DeleteNamedVolumes(context.Context, VM, []VolumeRef) ([]VolumeRef, bool, error)
}

// CloneSourceRelease carries the audit columns required by the HELD table.
type CloneSourceRelease struct {
	TenantID, TargetVMID, Kind, By, Reason, Evidence string
}

// CloneSourceReference is the audit projection of one HELD/RELEASED row.
type CloneSourceReference struct {
	TenantID, SourceVMID, TargetVMID, CreateOperationID, SourceNativeUID string
	State, ReleaseKind, ReleaseReason, ReleasedBy, ReleaseEvidence       string
	SourceVersion                                                        int64
	ReleasedAt                                                           *time.Time
}

// CloneSourcePort owns the narrow HELD reference table. The reference table is
// the only authority for HELD/RELEASED state.
type CloneSourcePort interface {
	ReleaseCloneSourceHELD(context.Context, CloneSourceRelease) error
}

// CloneSourceHeldPort is the read-only query resolve needs before it hands a
// clone request to the acceptance path. It is separate from CloneSourcePort so
// the worker keeps depending only on the release it performs.
type CloneSourceHeldPort interface {
	// SourceHeld reports whether another clone still holds this VM as its
	// source. Resolve reads it so a second clone of a busy source is refused
	// above the owner instead of being accepted and then dropped during
	// dispatch. It never takes the reference itself.
	SourceHeld(context.Context, string, string) (bool, error)
}

// CloneName is the fixed native name of one target's clone.
func CloneName(targetID string) string { return "vm-clone-" + targetID }

// IsClone reports whether this create came from a managed VM source.
func (s Spec) IsClone() bool { return s.CloneSource != nil }

// NewCloneTargetDisk derives a fresh logical target disk from the frozen source
// plan. The volume name stays the source mapping key; only the local identity is
// new and is fixed by the first write.
func NewCloneTargetDisk(targetID string, d CloneDisk) Disk {
	disk := NewInitialDisk(targetID, "clone/"+d.SourceDiskID, d.Name, d.Role, d.SizeGB)
	disk.VolumeName = d.VolumeName
	disk.DV = ObjectRef{}
	disk.Preparation = "PendingPopulation"
	return disk
}

// FrozenVolumeNames is the completeness boundary of the cancel hand-off: the
// system and data volume names frozen at acceptance. A standard NoCloud seed is
// not a PVC and is not part of this set.
func (v VM) FrozenVolumeNames() []string {
	if v.Spec.CloneSource == nil {
		return nil
	}
	names := make([]string, 0, len(v.Spec.CloneSource.Disks))
	for _, d := range v.Spec.CloneSource.Disks {
		names = append(names, d.VolumeName)
	}
	return names
}

// CloneVolumesCovered reports whether the saved named outputs cover every frozen
// system/data volume. An empty list never proves zero volumes.
func CloneVolumesCovered(names []string, outputs []VolumeRef) bool {
	if len(names) == 0 {
		return false
	}
	seen := map[string]bool{}
	for _, o := range outputs {
		if o.VolumeName != "" {
			seen[o.VolumeName] = true
		}
	}
	for _, n := range names {
		if !seen[n] {
			return false
		}
	}
	return true
}
