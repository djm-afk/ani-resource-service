package vm

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/zhangzhe-ctrl/ani-resource-service/internal/telemetry"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/telemetry/logfields"
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
	// Restore the accepted request's trace into this run; the image has no
	// operation, so resource_id is the run identity.
	runCtx := w.imageRunContext(ctx, i)
	e = w.advance(runCtx, i)
	if e == nil || errors.Is(e, ErrImageVersion) || errors.Is(e, ErrLease) {
		return true, nil
	}
	// Record the real dependency/validation failure at the point it becomes a
	// parked reason, before park() collapses it into a fixed string.
	w.logImageFailed(runCtx, i, "advance", e)
	// A dependency failure keeps the lease holder's classification but must not
	// stop the lane; back off this record and report the reason code only.
	return true, w.park(runCtx, i, imageFailureReason(e))
}

// imageRunContext restores the image's persisted trace and binds its identity.
func (w *ImageWorker) imageRunContext(ctx context.Context, i Image) context.Context {
	base := telemetry.WithBusiness(ctx, telemetry.BusinessContext{
		ResourceType: "image", ResourceID: i.ID, TenantID: i.TenantID,
	})
	if i.TelemetryContextError != nil {
		w.logImageTelemetry(base, i, "telemetry.context_invalid", i.TelemetryContextError)
		return base
	}
	if !i.TelemetryContext.Valid() {
		w.logImageTelemetry(base, i, "telemetry.context_missing", nil)
		return base
	}
	restored, e := telemetry.Restore(ctx, i.TelemetryContext)
	if e != nil {
		w.logImageTelemetry(base, i, "telemetry.context_invalid", e)
		return base
	}
	return telemetry.WithBusiness(restored, telemetry.BusinessContext{
		ResourceType: "image", ResourceID: i.ID, TenantID: i.TenantID,
	})
}

// logImageFailed records the real error with the known image coordinates.
func (w *ImageWorker) logImageFailed(ctx context.Context, i Image, step string, err error) {
	if w.logger == nil || err == nil {
		return
	}
	attrs := []any{"event", "step.failed", "step", step, "result", "failed",
		"resource_type", "image", "resource_id", i.ID}
	if i.TenantID != "" {
		attrs = append(attrs, "tenant_id", i.TenantID)
	}
	attrs = append(attrs, logfields.ErrorFields(err)...)
	attrs = append(attrs, imageTarget(i).KV()...)
	w.logger.ErrorContext(ctx, "VM image step failed", attrs...)
}

func (w *ImageWorker) logImageTelemetry(ctx context.Context, i Image, event string, err error) {
	if w.logger == nil {
		return
	}
	attrs := []any{"event", event, "resource_type", "image", "resource_id", i.ID}
	if i.TenantID != "" {
		attrs = append(attrs, "tenant_id", i.TenantID)
	}
	if event == "telemetry.context_invalid" {
		attrs = append(attrs, "reason", "TELEMETRY_CONTEXT_INVALID")
		if err != nil {
			attrs = append(attrs, logfields.ErrorFields(err)...)
		}
	} else {
		attrs = append(attrs, "reason", "TELEMETRY_CONTEXT_MISSING")
	}
	w.logger.ErrorContext(ctx, "VM image telemetry context unavailable", attrs...)
}

// imageTarget renders the known DataVolume/PVC/Job coordinates. Unknown fields
// are omitted; no lookup is performed.
func imageTarget(i Image) telemetry.Target {
	t := telemetry.Target{ClusterID: i.Native.ClusterID, Namespace: i.Native.Namespace}
	switch {
	case i.Native.DVName != "":
		t.Kind, t.Name, t.UID = "DataVolume", i.Native.DVName, i.Native.DVUID
	case i.Native.PVCName != "":
		t.Kind, t.Name, t.UID = "PersistentVolumeClaim", i.Native.PVCName, i.Native.PVCUID
	case i.Native.JobName != "":
		t.Kind, t.Name, t.UID = "Job", i.Native.JobName, i.Native.JobUID
	}
	return t
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
	w.logger.WarnContext(ctx, "VM image background step parked", "event", "worker.retry_scheduled",
		"worker", "vm_image", "resource_type", "image", "resource_id", i.ID, "state", i.State,
		"reason", reason, "attempt_count", i.AttemptCount,
		"next_retry", i.NextAttemptAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		attrs := []any{"event", "state.persist_failed", "action", "park", "step", "persist_image",
			"resource_type", "image", "resource_id", i.ID}
		attrs = append(attrs, logfields.ErrorFields(err)...)
		w.logger.ErrorContext(ctx, "VM image park state save failed", attrs...)
	}
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

// cleanup closes only after registered platform business objects exit their
// native lifecycle. CDI internals and PV diagnostics stay outside that set.
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
	// Newly observed refs are platform business objects. Historical CDI/PV
	// refs remain in Cleanup for handoff, outside the regular execution set.
	i.Cleanup = appendImageRefs(i.Cleanup, remaining)
	for _, ref := range remaining {
		if ref.Namespace != i.Native.Namespace {
			return ErrOwnership
		}
		switch ref.Kind {
		case "DataVolume":
			if i.Native.DVUID == "" {
				i.Native.DVName, i.Native.DVUID = ref.Name, ref.UID
			}
		case "PersistentVolumeClaim":
			if i.Native.PVCUID == "" {
				i.Native.PVCName, i.Native.PVCUID = ref.Name, ref.UID
			}
		case "Job":
			if i.Native.JobUID == "" {
				i.Native.JobName, i.Native.JobUID = ref.Name, ref.UID
			}
		case "UploadTokenRequest":
			if i.Native.TokenUID == "" {
				i.Native.TokenName, i.Native.TokenUID = ref.Name, ref.UID
			}
		}
	}

	if len(remaining) == 0 && len(ImageCleanupObjects(i)) == 0 && i.Native.DVName == "" && i.Native.PVCName == "" &&
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
		observed.Cleanup = appendImageRefs(observed.Cleanup, recheck)
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

// ImageCleanupObjects is the pending business set from legacy Cleanup.
// Only the platform's own source PVC belongs here; other historical PVCs/Pods
// are CDI internals handed to component operations. They remain diagnostic.
func ImageCleanupObjects(i Image) []ObjectRef {
	refs := []ObjectRef{}
	source := "vmi-src-" + strings.ReplaceAll(i.ID, "-", "")
	for _, ref := range i.Cleanup {
		if ref.Kind == "PersistentVolume" || ref.Kind == "Pod" {
			continue
		}
		// Classify only existing legacy registrations, never enumerate these
		// names or use them to adopt a different UID. Unknown business refs stay.
		if ref.Kind == "PersistentVolumeClaim" && ((i.Native.PVCUID != "" && (ref.Name == "prime-"+i.Native.PVCUID || ref.Name == "prime-"+i.Native.PVCUID+"-scratch")) || ref.Name == source+"-scratch" || (i.Native.PVCName != "" && ref.Name == i.Native.PVCName+"-scratch")) {
			continue
		}
		refs = append(refs, ref)
	}
	return refs
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
