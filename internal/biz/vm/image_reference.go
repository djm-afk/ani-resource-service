package vm

import (
	"context"
	"errors"
	"log/slog"
)

const (
	// ReferenceReleasedClone proves the consumer owns its own completed copy.
	ReferenceReleasedClone = "CLONE_COMPLETED"
	// ReferenceReleasedRetired proves the consumer object is gone for good.
	ReferenceReleasedRetired = "CONSUMER_RETIRED"
)

// ImageReferencePort is the release half of the pin contract.
type ImageReferencePort interface {
	ListHeldReferences(context.Context, int) ([]ImageReference, error)
}

// ReferenceWorker keeps image pins honest independently of the VM operation
// lifecycle: an operation that "succeeded" is not evidence that a clone
// finished, and only real completion or proven retirement releases a pin.
type ReferenceWorker struct {
	repo      ImageRepository
	vm        Repository
	reference ImageReferencePort
	native    ImageNativePort
	logger    *slog.Logger
}

func NewReferenceWorker(repo ImageRepository, vm Repository, native ImageNativePort, logger *slog.Logger) (*ReferenceWorker, error) {
	reference, ok := repo.(ImageReferencePort)
	if !ok || vm == nil || native == nil || logger == nil {
		return nil, ErrInvalid
	}
	return &ReferenceWorker{repo: repo, vm: vm, reference: reference, native: native, logger: logger}, nil
}

func (w *ReferenceWorker) Step(ctx context.Context) (bool, error) {
	refs, e := w.reference.ListHeldReferences(ctx, 50)
	if e != nil {
		return false, e
	}
	work := false
	for _, ref := range refs {
		changed, e := w.settle(ctx, ref)
		if e != nil {
			// Identity conflicts stay HELD: an unproven release would let a
			// source be deleted out from under a live consumer.
			if errors.Is(e, ErrNotFound) || errors.Is(e, ErrDependency) || errors.Is(e, ErrLease) || errors.Is(e, ErrImageVersion) {
				continue
			}
			w.logger.Warn("VM image reference check failed", "image_id", ref.ImageID, "vm_id", ref.VMID, "reason", imageFailureReason(e))
			continue
		}
		if changed {
			work = true
		}
	}
	return work, nil
}

func (w *ReferenceWorker) settle(ctx context.Context, ref ImageReference) (bool, error) {
	i, e := w.repo.FindImage(ctx, ref.TenantID, ref.ImageID)
	if errors.Is(e, ErrNotFound) {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	v, e := w.vm.Get(ctx, ref.TenantID, ref.VMID)
	if errors.Is(e, ErrNotFound) {
		// The VM row is already gone, so its copies cannot still reference it.
		return true, w.repo.ReleaseImageReference(ctx, ref, ReferenceReleasedRetired)
	}
	if e != nil {
		return false, e
	}
	if ImageSourceFingerprint(i) != ref.SourceFingerprint {
		return false, ErrImageIdentity
	}
	return w.settleClone(ctx, i, v, ref)
}

func (w *ReferenceWorker) settleClone(ctx context.Context, i Image, v VM, ref ImageReference) (bool, error) {
	state, e := w.native.ObserveImageClone(ctx, v, i, ref)
	if e != nil {
		if errors.Is(e, ErrOwnership) || errors.Is(e, ErrImageIdentity) {
			return false, ErrImageIdentity
		}
		return false, e
	}
	if state == CloneCompleted {
		return true, w.repo.ReleaseImageReference(ctx, ref, ReferenceReleasedClone)
	}
	// A retiring consumer releases on proven absence. The delete path only sets
	// Tombstone and keeps the historical DV/PVC UID, so demanding empty UIDs made
	// this condition unreachable and the source stayed pinned forever.
	if v.Closing && (v.Observed == "Deleted" || v.Desired == "Deleted") &&
		(state == CloneAbsent || w.consumerAbsent(v, ref)) {
		return true, w.repo.ReleaseImageReference(ctx, ref, ReferenceReleasedRetired)
	}
	return false, nil
}

// consumerAbsent is the fallback for consumers with no cluster identity left to
// probe (an unregistered copy). It is no longer the primary proof: the clone probe
// answers absence directly.
func (w *ReferenceWorker) consumerAbsent(v VM, ref ImageReference) bool {
	if ref.ConsumerKind == "SYSTEM_DISK" {
		for _, d := range v.Disks {
			if d.ID == ref.ConsumerID {
				return d.Tombstone && d.DV.UID == "" && d.PVC.UID == ""
			}
		}
		return true
	}
	for _, copy := range v.MediaCopies {
		if copy.ID == ref.ConsumerID {
			return copy.Tombstone && copy.DV.UID == "" && copy.PVC.UID == ""
		}
	}
	return true
}

var _ = slog.Default
