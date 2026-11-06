package vm

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// These cases cover the release half of the pin contract, which had no test at all:
// an interrupted clone that is then deleted must still stop holding its source.

type retireImageRepo struct {
	ImageRepository
	image    Image
	released []ImageReference
	reasons  []string
}

func (r *retireImageRepo) FindImage(context.Context, string, string) (Image, error) {
	return r.image, nil
}
func (r *retireImageRepo) ReleaseImageReference(_ context.Context, ref ImageReference, reason string) error {
	r.released = append(r.released, ref)
	r.reasons = append(r.reasons, reason)
	return nil
}

type retireVMRepo struct {
	Repository
	vm  VM
	err error
}

func (v *retireVMRepo) Get(context.Context, string, string) (VM, error) { return v.vm, v.err }

// retireNative answers the clone probe with whatever the case needs. The initial red
// run used the old boolean port; the cases themselves are about release behaviour and
// were written to compile against both shapes.
type retireNative struct {
	ImageNativePort
	clone CloneState
	err   error
}

func (n *retireNative) ObserveImageClone(context.Context, VM, Image, ImageReference) (CloneState, error) {
	return n.clone, n.err
}

func retireFixture(t *testing.T, closing bool, observed string, uid string) (
	*retireImageRepo, *retireVMRepo, *retireNative, ImageReference) {
	t.Helper()
	tenant, imageID, vmID, diskID := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	i := Image{TenantID: tenant, ID: imageID, MediaKey: ImageMediaKey(imageID), Kind: "DISK_IMAGE", Format: "qcow2",
		Name: "retire source", State: StateReady, CleanupState: CleanupNone, Version: 4, SourceCapacityGB: 10,
		SourceSHA256: strings.Repeat("a", 64), SourceRequestedBytes: 10 << 30, ObservedSourceCapacityBytes: 10 << 30,
		SourceChecksumBytes: 10 << 30,
		Native: ImageNative{Namespace: "tenant-" + tenant, PVCName: "vmi-src-x", PVCUID: uuid.NewString(),
			DVName: "vmi-src-x", DVUID: uuid.NewString(), SourceVolumeMode: "Block"},
		OS: ImageOS{OSFamily: "linux", OSVersion: "24.04", Architecture: "amd64", Firmware: "bios",
			RootBus: "virtio", DataBus: "scsi", NICModel: "virtio"}}
	v := VM{TenantID: tenant, ID: vmID, Version: 7, Closing: closing, Observed: observed, Desired: "Deleted",
		Spec: Spec{Media: i.Source()},
		Disks: []Disk{{ID: diskID, Role: "system", SizeGB: 10, Tombstone: true,
			DV:  ObjectRef{Kind: "DataVolume", Namespace: "tenant-" + tenant, Name: "vd-" + diskID, UID: uid},
			PVC: ObjectRef{Kind: "PersistentVolumeClaim", Namespace: "tenant-" + tenant, Name: "vd-" + diskID, UID: uid}}}}
	ref, e := NewImageReference(v, diskID, "SYSTEM_DISK", i.Source())
	if e != nil {
		t.Fatal(e)
	}
	repo := &retireImageRepo{image: i}
	vms := &retireVMRepo{vm: v}
	return repo, vms, &retireNative{}, ref
}

func newRetireWorker(repo *retireImageRepo, vms *retireVMRepo, native *retireNative) *ReferenceWorker {
	return &ReferenceWorker{repo: repo, vm: vms, reference: nil, native: native, logger: slog.New(slog.DiscardHandler)}
}

// TestImageReferenceDeletedVMReleasesWhenTheCloneIsProvenGone is the P2 guard: the VM was
// deleted while its clone was still in flight, so the clone never reached Succeeded. The
// target objects are now proven absent, and that absence is the evidence to release.
// The retired disk keeps its historical DV/PVC UID because the delete path only sets
// Tombstone, so a judgement that demands an empty UID can never be satisfied.
func TestImageReferenceDeletedVMReleasesWhenTheCloneIsProvenGone(t *testing.T) {
	repo, vms, native, ref := retireFixture(t, true, "Deleted", uuid.NewString())
	native.clone = CloneAbsent
	w := newRetireWorker(repo, vms, native)

	if _, e := w.settle(context.Background(), ref); e != nil {
		t.Fatalf("settle: %v", e)
	}
	if len(repo.reasons) != 1 || repo.reasons[0] != ReferenceReleasedRetired {
		t.Fatalf("a retiring VM whose clone target is gone kept the pin held: released=%v", repo.reasons)
	}
	if repo.released[0].ConsumerID != ref.ConsumerID {
		t.Fatalf("released the wrong consumer: %+v", repo.released[0])
	}
}

// TestImageReferenceLiveVMNeverReleases keeps the other boundary honest: while the VM is
// not retiring, an unfinished clone must hold the source no matter what.
func TestImageReferenceLiveVMNeverReleases(t *testing.T) {
	repo, vms, native, ref := retireFixture(t, false, "Running", uuid.NewString())
	native.clone = CloneAbsent
	w := newRetireWorker(repo, vms, native)

	if _, e := w.settle(context.Background(), ref); e != nil {
		t.Fatalf("settle: %v", e)
	}
	if len(repo.reasons) != 0 {
		t.Fatalf("a live VM released its source pin without a completed clone: %v", repo.reasons)
	}
}

// TestImageReferenceRetiringVMWithStillPresentCloneKeepsThePin proves the release needs
// real absence, not just a deleted-looking record: the target copy is still there, so a
// pending probe must hold the pin even though the disk is already tombstoned.
func TestImageReferenceRetiringVMWithStillPresentCloneKeepsThePin(t *testing.T) {
	repo, vms, native, ref := retireFixture(t, true, "Deleted", uuid.NewString())
	native.clone = ClonePending
	w := newRetireWorker(repo, vms, native)

	if _, e := w.settle(context.Background(), ref); e != nil {
		t.Fatalf("settle: %v", e)
	}
	if len(repo.reasons) != 0 {
		t.Fatalf("a copy that is not proven absent released the pin: %v", repo.reasons)
	}
}

// TestImageReferenceOwnershipDoubtKeepsThePin: an identity conflict must never turn into
// a release, and must not be reported as success.
func TestImageReferenceOwnershipDoubtKeepsThePin(t *testing.T) {
	repo, vms, native, ref := retireFixture(t, true, "Deleted", uuid.NewString())
	native.err = ErrOwnership
	w := newRetireWorker(repo, vms, native)

	if _, e := w.settle(context.Background(), ref); !errors.Is(e, ErrImageIdentity) {
		t.Fatalf("ownership doubt returned %v, want the identity error", e)
	}
	if len(repo.reasons) != 0 {
		t.Fatalf("an unproven consumer retired the pin: %v", repo.reasons)
	}
}

// TestImageReferenceMissingVMReleases stays as the existing path, so the new evidence
// rule does not quietly replace it.
func TestImageReferenceMissingVMReleases(t *testing.T) {
	repo, vms, native, ref := retireFixture(t, true, "Deleted", uuid.NewString())
	vms.err = ErrNotFound
	w := newRetireWorker(repo, vms, native)

	if _, e := w.settle(context.Background(), ref); e != nil {
		t.Fatalf("settle: %v", e)
	}
	if len(repo.reasons) != 1 || repo.reasons[0] != ReferenceReleasedRetired {
		t.Fatalf("a VM row that is gone did not retire its pin: %v", repo.reasons)
	}
}
