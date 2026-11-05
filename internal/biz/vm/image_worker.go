package vm

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// ImageRepository and ImageNativePort are already declared in image.go; this
// file owns only the background driver so the command path stays testable.

// ImageWorker advances one image record per claim. It never holds a database
// transaction across a cluster call, and every step is re-entrant.
type ImageWorker struct {
	repo        ImageRepository
	native      ImageNativePort
	environment EnvironmentPort
	identity    string
	cluster     string
	lease       time.Duration
	poll        time.Duration
	logger      *slog.Logger
	// enabled gates only new admissions; existing records always drain.
	enabled func() bool
}

func NewImageWorker(repo ImageRepository, native ImageNativePort, environment EnvironmentPort, identity string, enabled func() bool, logger *slog.Logger) (*ImageWorker, error) {
	if repo == nil || native == nil || environment == nil || identity == "" || !UUID(identity) || enabled == nil || logger == nil {
		return nil, ErrInvalid
	}
	return &ImageWorker{repo: repo, native: native, environment: environment, identity: identity,
		lease: 45 * time.Second, poll: 2 * time.Second, logger: logger, enabled: enabled}, nil
}

// Step implements the shared background Stepper used by every resource lane.
func (w *ImageWorker) Step(ctx context.Context) (bool, error) {
	i, claimed, e := w.repo.ClaimImage(ctx, w.identity, w.lease)
	if e != nil {
		if errors.Is(e, ErrNotFound) {
			return false, nil
		}
		return false, e
	}
	if !claimed {
		return false, nil
	}
	e = w.advance(ctx, i)
	if e == nil || errors.Is(e, ErrImageVersion) || errors.Is(e, ErrLease) {
		return true, nil
	}
	// A dependency failure keeps the lease holder's classification but must not
	// stop the lane; back off this record and report the reason code only.
	return true, w.park(ctx, i, imageFailureReason(e))
}

// save persists a step that made progress and clears the failure streak.
func (w *ImageWorker) save(ctx context.Context, i Image) error {
	i.AttemptCount = 0
	return w.repo.SaveImage(ctx, i)
}

// park persists a step that did not move the record forward: it keeps the state,
// widens the next attempt and leaves the streak visible for the reader. Without
// this the lane re-claims the same broken record every 2s forever.
func (w *ImageWorker) park(ctx context.Context, i Image, reason string) error {
	i.Reason = reason
	i.AttemptCount++
	i.NextAttemptAt = time.Now().Add(imageBackoff(i))
	err := w.repo.SaveImage(ctx, i)
	w.logger.Warn("VM image background step parked", "image_id", i.ID, "state", i.State, "reason", reason,
		"attempt_count", i.AttemptCount, "next_attempt_at", i.NextAttemptAt.UTC().Format(time.RFC3339))
	return err
}

// imageBackoff is 2s while work is in flight and 5s during teardown, and it
// doubles with each consecutive failure of the same record up to 8 minutes.
func imageBackoff(i Image) time.Duration {
	base := 2 * time.Second
	if i.State == StateDeleting || i.CleanupState == CleanupPending ||
		i.CleanupState == CleanupRunning || i.CleanupState == CleanupBlocked {
		base = 5 * time.Second
	}
	shift := i.AttemptCount
	if shift > 6 {
		shift = 6
	}
	wait := base << shift
	if wait > imageBackoffCeiling {
		return imageBackoffCeiling
	}
	return wait
}

var imageBackoffCeiling = 8 * time.Minute

func imageFailureReason(e error) string {
	switch {
	case errors.Is(e, ErrImageDependency), errors.Is(e, ErrDependency):
		return "VM_IMAGE_DEPENDENCY_UNAVAILABLE"
	case errors.Is(e, ErrImageIdentity), errors.Is(e, ErrChanged):
		return "SOURCE_IDENTITY_CHANGED"
	default:
		return "SOURCE_VALIDATION_FAILED"
	}
}

func (w *ImageWorker) advance(ctx context.Context, i Image) error {
	// Cleanup wins over any upload progress: a delete or deadline that already
	// closed the record must not be reopened by a late success.
	if i.CleanupState == CleanupPending || i.CleanupState == CleanupRunning || i.State == StateDeleting {
		return w.cleanup(ctx, i)
	}
	switch i.State {
	case StatePreparing:
		return w.prepare(ctx, i)
	case StateImporting:
		return w.importing(ctx, i)
	case StateVerifying:
		return w.verifying(ctx, i)
	case StateReady:
		return w.revalidate(ctx, i)
	}
	return nil
}

// prepare binds the tenant namespace and the source DataVolume. The namespace
// identity is frozen here because the media projection never re-resolves it.
func (w *ImageWorker) prepare(ctx context.Context, i Image) error {
	if !w.enabled() {
		// The switch stops new admissions, so a record that has not created its
		// native target yet is failed rather than left to accumulate.
		i.Reason = "VM_IMAGE_UPLOAD_DISABLED"
		i.State = StateFailed
		i.CleanupState = CleanupNone
		i.UploadDeadline = nil
		return w.save(ctx, i)
	}
	if i.Native.Namespace == "" {
		ns, e := w.environment.ResolveNamespace(ctx, i.TenantID, "")
		if e != nil {
			if errors.Is(e, ErrDependency) || errors.Is(e, ErrNotFound) {
				return w.park(ctx, i, "NAMESPACE_INVALID")
			}
			return e
		}
		i.Native.Namespace, i.Native.NamespaceUID = ns.Name, ns.UID
	}
	i.Native.ClusterID = w.clusterID()
	observed, e := w.native.EnsureImageSource(ctx, i)
	if e != nil {
		return e
	}
	if observed.Native.DVUID == "" {
		return ErrImageDependency
	}
	observed.State = StateImporting
	observed.Reason = ""
	return w.save(ctx, observed)
}

func (w *ImageWorker) importing(ctx context.Context, i Image) error {
	observed, e := w.native.ObserveImageSource(ctx, i)
	if errors.Is(e, ErrDependency) {
		observed = i
	} else if e != nil {
		if errors.Is(e, ErrOwnership) || errors.Is(e, ErrImageIdentity) {
			i.Reason = "SOURCE_IDENTITY_CHANGED"
			i.State = StateFailed
			i.CleanupState = CleanupPending
			i.UploadDeadline = nil
			return w.save(ctx, i)
		}
		return e
	}
	if observed.Reason == "UPLOAD_REJECTED" || observed.Reason == "CAPACITY_INSUFFICIENT" {
		observed.State = StateFailed
		observed.CleanupState = CleanupPending
		observed.UploadDeadline = nil
		return w.save(ctx, observed)
	}
	if !observed.ReadyForVerification() {
		// Waiting for the caller's bytes is not a business change, so the row is not
		// rewritten: `version` keeps meaning "something the tenant did", and the claim
		// lease alone paces the next observation instead of a 2s write-per-poll loop.
		return nil
	}
	verified, e := w.native.EnsureImageVerification(ctx, observed)
	if e != nil {
		return e
	}
	verified.State = StateVerifying
	verified.Reason = ""
	return w.save(ctx, verified)
}

// revalidate keeps READY honest. A source whose identity moved can never back a
// new VM, but already admitted consumers keep their own copies untouched.
func (w *ImageWorker) revalidate(ctx context.Context, i Image) error {
	observed, e := w.native.ObserveImageSourceIdentity(ctx, i)
	observed.NextAttemptAt = time.Now().Add(imageRevalidateInterval)
	if e == nil {
		return w.save(ctx, observed)
	}
	if errors.Is(e, ErrDependency) || errors.Is(e, ErrNotFound) ||
		errors.Is(e, ErrOwnership) || errors.Is(e, ErrImageIdentity) || errors.Is(e, ErrChanged) {
		i.State = StateUnavail
		i.Reason = "SOURCE_IDENTITY_CHANGED"
		i.Native.DVPhase = ""
		i.NextAttemptAt = time.Now().Add(imageRevalidateInterval)
		return w.save(ctx, i)
	}
	return e
}

var imageRevalidateInterval = time.Hour

func (w *ImageWorker) verifying(ctx context.Context, i Image) error {
	observed, result, e := w.native.ObserveImageVerification(ctx, i)
	if errors.Is(e, ErrDependency) {
		observed.NextAttemptAt = time.Now().Add(w.poll)
		return w.save(ctx, observed)
	}
	if e != nil {
		if errors.Is(e, ErrImageIdentity) || errors.Is(e, ErrOwnership) {
			observed.Reason = "SOURCE_IDENTITY_CHANGED"
			observed.State = StateFailed
			observed.CleanupState = CleanupPending
			observed.UploadDeadline = nil
			return w.save(ctx, observed)
		}
		return e
	}
	if !result.Pass {
		observed.Reason = result.FailureReason
		if observed.Reason == "" {
			observed.Reason = "SOURCE_VALIDATION_FAILED"
		}
		observed.State = StateFailed
		observed.CleanupState = CleanupPending
		observed.UploadDeadline = nil
		return w.save(ctx, observed)
	}
	// The annotation has to carry the digest the verifier reported. Publishing it from a
	// record that does not hold one yet wrote an empty annotation, and the read-back
	// check passed because both sides were empty, leaving a READY image the consumer path
	// rejects for a missing source identity.
	observed.SourceSHA256 = result.Checksum
	if err := w.native.PublishSourceAnnotation(ctx, observed); err != nil {
		if errors.Is(err, ErrImageVersion) {
			// Someone else changed the annotation; re-observe before deciding.
			observed.NextAttemptAt = time.Now().Add(w.poll)
			return w.save(ctx, observed)
		}
		return err
	}
	// The annotation is durable before READY, so ValidateSource can never see a
	// READY record whose source identity is unwritten.
	if err := observed.PublishReady(result.PVCUID, result.RequestedBytes, result.ObservedBytes, result.Bytes, result.Checksum, time.Now().UTC()); err != nil {
		observed.Reason = "SOURCE_VALIDATION_FAILED"
		observed.State = StateFailed
		observed.CleanupState = CleanupPending
		observed.UploadDeadline = nil
		return w.save(ctx, observed)
	}
	observed.NextAttemptAt = time.Now().Add(imageRevalidateInterval)
	return w.save(ctx, observed)
}

// cleanup removes exactly the registered chain, then closes the tombstone. The
// cluster probe is the proof: a worker that died between creating an object and
// registering it must still show the namespace empty before DELETED is written,
// otherwise the record vanishes while its volume keeps consuming capacity.
func (w *ImageWorker) cleanup(ctx context.Context, i Image) error {
	if i.CleanupState == CleanupComplete {
		return w.closeDeleted(ctx, i)
	}
	if i.Native.Namespace == "" {
		if i.Native.DVUID != "" || i.Native.PVCUID != "" || i.Native.JobUID != "" || i.Native.TokenUID != "" || len(i.Cleanup) > 0 {
			// A registered identity with no namespace cannot be probed at all, so
			// the record stays open instead of closing on an assumption.
			return w.park(ctx, i, "NAMESPACE_INVALID")
		}
		// Nothing was ever bound: without a namespace no object of this image can exist.
		i.CleanupState = CleanupComplete
		i.NextAttemptAt = time.Now().Add(w.poll)
		return w.save(ctx, i)
	}
	remaining, e := w.probe(ctx, i)
	if errors.Is(e, errImageDeferred) {
		return nil
	}
	if e != nil {
		return e
	}
	// The drain chain only owns PersistentVolumeClaim entries; a DataVolume or Job found
	// by the probe is removed by its own name+UID step, so registering it as a chain
	// entry would be read as a foreign object and block the tombstone forever. Rows
	// written before that rule carry such entries already, so they are dropped here:
	// the probe that just ran covers every one of those kinds, and it is the probe,
	// not the chain, that decides whether the namespace is empty.
	i.Cleanup = appendImageRefs(drainableImageRefs(i.Cleanup), registrableImageRefs(remaining))
	if len(remaining) == 0 && len(i.Cleanup) == 0 && i.Native.DVName == "" && i.Native.PVCName == "" &&
		i.Native.JobName == "" && i.Native.TokenName == "" {
		i.CleanupState = CleanupComplete
		i.NextAttemptAt = time.Now().Add(w.poll)
		return w.save(ctx, i)
	}
	i.CleanupState = CleanupRunning
	observed, done, e := w.native.DeleteImageObjects(ctx, i)
	if e != nil {
		if errors.Is(e, ErrImageCleanup) || errors.Is(e, ErrOwnership) {
			observed.CleanupState = CleanupBlocked
			if save := w.park(ctx, observed, "NATIVE_CLEANUP_BLOCKED"); save != nil && !errors.Is(save, ErrImageVersion) {
				return save
			}
			return nil
		}
		return e
	}
	if !done {
		observed.NextAttemptAt = time.Now().Add(imageBackoff(observed))
		return w.save(ctx, observed)
	}
	// Deletion reported gone; only a second empty probe may close the tombstone.
	recheck, e := w.probe(ctx, observed)
	if errors.Is(e, errImageDeferred) {
		return nil
	}
	if e != nil {
		return e
	}
	if len(recheck) > 0 {
		observed.Cleanup = appendImageRefs(observed.Cleanup, registrableImageRefs(recheck))
		observed.NextAttemptAt = time.Now().Add(w.poll)
		return w.save(ctx, observed)
	}
	observed.CleanupState = CleanupComplete
	return w.closeDeleted(ctx, observed)
}

// errImageDeferred marks a step that was parked and reported; the lane treats it
// as handled without letting the caller keep going on an unproven listing.
var errImageDeferred = errors.New("VM_IMAGE_STEP_DEFERRED")

// probe turns a native listing failure into a parked record. A cluster that
// cannot answer is never read as "nothing left to clean".
func (w *ImageWorker) probe(ctx context.Context, i Image) ([]ObjectRef, error) {
	remaining, e := w.native.ObserveManagedSources(ctx, i)
	if e == nil {
		return remaining, nil
	}
	if errors.Is(e, ErrOwnership) || errors.Is(e, ErrImageIdentity) {
		if save := w.park(ctx, i, "SOURCE_IDENTITY_CHANGED"); save != nil {
			return nil, save
		}
		return nil, errImageDeferred
	}
	if errors.Is(e, ErrDependency) || errors.Is(e, ErrNotFound) {
		if save := w.park(ctx, i, "VM_IMAGE_DEPENDENCY_UNAVAILABLE"); save != nil {
			return nil, save
		}
		return nil, errImageDeferred
	}
	return nil, e
}

func registrableImageRefs(found []ObjectRef) []ObjectRef {
	out := make([]ObjectRef, 0, len(found))
	for _, ref := range found {
		if ref.Kind == "PersistentVolumeClaim" {
			out = append(out, ref)
		}
	}
	return out
}

// drainableImageRefs keeps the types the registered chain can act on. A record
// written by an older worker may hold probe kinds there; those objects are still
// covered by the current probe, and leaving them in would block the chain on a
// kind it can never remove.
func drainableImageRefs(existing []ObjectRef) []ObjectRef {
	out := make([]ObjectRef, 0, len(existing))
	for _, ref := range existing {
		if ref.Kind == "PersistentVolumeClaim" || ref.Kind == "PersistentVolume" {
			out = append(out, ref)
		}
	}
	return out
}

func appendImageRefs(existing []ObjectRef, found []ObjectRef) []ObjectRef {
	for _, ref := range found {
		if ref.Name == "" {
			continue
		}
		duplicate := false
		for _, item := range existing {
			if item.Kind == ref.Kind && item.Namespace == ref.Namespace && item.Name == ref.Name {
				duplicate = true
				break
			}
		}
		if !duplicate {
			existing = append(existing, ref)
		}
	}
	return existing
}

func (w *ImageWorker) closeDeleted(ctx context.Context, i Image) error {
	if i.State != StateDeleting {
		i.CleanupState = CleanupComplete
		return w.save(ctx, i)
	}
	i.State = StateDeleted
	i.Reason = ""
	i.CleanupState = CleanupComplete
	i.Cleanup = nil
	return w.save(ctx, i)
}

// SetImageClusterID lets the composition root hand the worker the same cluster
// identity the VM lane already freezes, without a second configuration source.
func (w *ImageWorker) SetImageClusterID(cluster string) {
	if cluster != "" {
		w.cluster = cluster
	}
}

func (w *ImageWorker) clusterID() string { return w.cluster }

var _ = slog.Default
