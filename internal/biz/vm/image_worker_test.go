package vm

import (
	"context"
	"log/slog"
	"testing"
	"time"
)

type workerRepo struct {
	queue []Image
	saved []Image
}

func (r *workerRepo) ClaimImage(context.Context, string, time.Duration) (Image, bool, error) {
	if len(r.queue) == 0 {
		return Image{}, false, nil
	}
	i := r.queue[0]
	r.queue = r.queue[1:]
	return i, true, nil
}

func (r *workerRepo) SaveImage(_ context.Context, i Image) error {
	r.saved = append(r.saved, i)
	return nil
}

func (r *workerRepo) requeue() {
	if len(r.saved) > 0 {
		r.queue = []Image{r.saved[len(r.saved)-1]}
	}
}

func (r *workerRepo) last() Image { return r.saved[len(r.saved)-1] }

func (r *workerRepo) ResolveImageMedia(context.Context, string, string) (Image, error) {
	return Image{}, ErrNotFound
}
func (r *workerRepo) ListReadyImages(context.Context, string) ([]Image, error) { return nil, nil }
func (r *workerRepo) ListImages(context.Context, string, ImageListFilter) ([]Image, int64, error) {
	return nil, 0, nil
}
func (r *workerRepo) FindImage(context.Context, string, string) (Image, error) {
	return Image{}, ErrNotFound
}
func (r *workerRepo) CreateImage(context.Context, Image, ImageCommand) (Image, error) {
	return Image{}, ErrInvalid
}
func (r *workerRepo) RecordSession(context.Context, ImageCommand, string, time.Time) (Image, error) {
	return Image{}, ErrInvalid
}
func (r *workerRepo) ConfirmImageSession(context.Context, Image) error { return ErrInvalid }
func (r *workerRepo) UpdateImage(context.Context, ImageCommand, string, string, []string) (Image, error) {
	return Image{}, ErrInvalid
}
func (r *workerRepo) DeleteImage(context.Context, ImageCommand) (Image, error) {
	return Image{}, ErrInvalid
}
func (r *workerRepo) CountHeldReferences(context.Context, string, string) (int, error) {
	return 0, nil
}
func (r *workerRepo) ListImageReferences(context.Context, string, string) ([]ImageReference, error) {
	return nil, nil
}
func (r *workerRepo) ReleaseImageReference(context.Context, ImageReference, string) error { return nil }

type workerNative struct {
	ensureErr    error
	ensureOK     bool
	observeImage Image
	observeErr   error
	probeRefs    []ObjectRef
	probeErr     error
	verifyImage  Image
	verifyResult VerificationResult
	verifyErr    error
	annotated    *Image
	deleteDone   bool
	deleteErr    error
	deleted      int
}

func (n *workerNative) EnsureImageSource(_ context.Context, i Image) (Image, error) {
	if n.ensureErr != nil {
		return i, n.ensureErr
	}
	if !n.ensureOK {
		return i, ErrImageDependency
	}
	i.Native.DVUID = "dv-uid"
	i.Native.DVName = "vmi-src-"
	return i, nil
}

func (n *workerNative) CreateUploadToken(context.Context, Image, string, time.Duration) (Image, string, time.Time, error) {
	return Image{}, "", time.Time{}, ErrInvalid
}
func (n *workerNative) ObserveImageSource(_ context.Context, i Image) (Image, error) {
	if n.observeErr != nil {
		return i, n.observeErr
	}
	return n.observeImage, nil
}
func (n *workerNative) EnsureImageVerification(context.Context, Image) (Image, error) {
	return Image{}, ErrInvalid
}
func (n *workerNative) ObserveImageVerification(_ context.Context, i Image) (Image, VerificationResult, error) {
	if n.verifyErr != nil {
		return i, VerificationResult{}, n.verifyErr
	}
	if n.verifyResult.Checksum == "" {
		return i, VerificationResult{}, ErrInvalid
	}
	return n.verifyImage, n.verifyResult, nil
}

// PublishSourceAnnotation records the image it was handed so a test can assert what the
// annotation would carry, which is the only durable evidence the checksum was known first.
func (n *workerNative) PublishSourceAnnotation(_ context.Context, i Image) error {
	copy := i
	n.annotated = &copy
	return nil
}
func (n *workerNative) ObserveImageSourceIdentity(context.Context, Image) (Image, error) {
	return Image{}, ErrInvalid
}
func (n *workerNative) ObserveManagedSources(context.Context, Image) ([]ObjectRef, error) {
	return n.probeRefs, n.probeErr
}
func (n *workerNative) DeleteImageObjects(_ context.Context, i Image) (Image, bool, error) {
	if n.deleteErr != nil {
		return i, false, n.deleteErr
	}
	n.deleted++
	// A delete request keeps the registration until the probe reports it gone,
	// exactly like the real adapter.
	return i, n.deleteDone, nil
}
func (n *workerNative) ObserveImageClone(context.Context, VM, Image, ImageReference) (CloneState, error) {
	return ClonePending, ErrInvalid
}

type workerEnv struct{ err error }

func (e workerEnv) ResolveNamespace(context.Context, string, string) (ObjectRef, error) {
	if e.err != nil {
		return ObjectRef{}, e.err
	}
	return ObjectRef{Name: "tenant-a", UID: "ns-uid"}, nil
}
func (e workerEnv) ValidateStorage(context.Context, string, string) error { return nil }
func (e workerEnv) ValidateNetwork(context.Context, string, string, string, NetworkSelection) (NetworkSelection, error) {
	return NetworkSelection{}, nil
}
func (e workerEnv) ValidateExistingNetwork(context.Context, VM) error { return nil }

func newWorker(repo ImageRepository, native ImageNativePort, environment EnvironmentPort) *ImageWorker {
	w, e := NewImageWorker(repo, native, environment, "0d1e2f3a-4b5c-4d6e-8f70-112233445566",
		func() bool { return true }, slog.New(slog.DiscardHandler))
	if e != nil {
		panic(e)
	}
	return w
}

// TestImageWorkerWidensBackoffWhileARecordCannotProgress is the R2 guard: a
// dependency that never answers must not be re-claimed on a flat 2s cadence.
func TestImageWorkerWidensBackoffWhileARecordCannotProgress(t *testing.T) {
	i := readyImage(t)
	i.State = StatePreparing
	i.CleanupState = CleanupNone
	repo := &workerRepo{queue: []Image{i}}
	native := &workerNative{ensureErr: ErrImageDependency}
	w := newWorker(repo, native, workerEnv{})

	ctx := context.Background()
	waits := make([]time.Duration, 0, 5)
	for step := 1; step <= 5; step++ {
		if _, e := w.Step(ctx); e != nil {
			t.Fatalf("step %d: %v", step, e)
		}
		saved := repo.last()
		if saved.AttemptCount != step {
			t.Fatalf("step %d persisted attempt_count=%d", step, saved.AttemptCount)
		}
		if saved.Reason != "VM_IMAGE_DEPENDENCY_UNAVAILABLE" {
			t.Fatalf("step %d reason=%q", step, saved.Reason)
		}
		if saved.State != StatePreparing {
			t.Fatalf("step %d moved state to %q without evidence", step, saved.State)
		}
		waits = append(waits, time.Until(saved.NextAttemptAt))
		repo.requeue()
	}
	for k := 1; k < len(waits); k++ {
		if waits[k] <= waits[k-1] {
			t.Fatalf("backoff stopped widening at attempt %d: %v", k+1, waits)
		}
	}
	if waits[len(waits)-1] < 30*time.Second {
		t.Fatalf("five consecutive failures still retry in %v", waits[len(waits)-1])
	}

	// Recovery clears the streak, so a record that starts progressing is not
	// punished for the earlier outage.
	native.ensureErr, native.ensureOK = nil, true
	if _, e := w.Step(ctx); e != nil {
		t.Fatalf("recovery step: %v", e)
	}
	if saved := repo.last(); saved.AttemptCount != 0 || saved.State != StateImporting {
		t.Fatalf("recovery kept streak: attempt=%d state=%s", saved.AttemptCount, saved.State)
	}
}

// TestImageWorkerClosesTombstoneOnlyAfterAnEmptyProbe is the R3 guard: unprobed
// leftovers keep the record open and become part of the registered chain.
func TestImageWorkerClosesTombstoneOnlyAfterAnEmptyProbe(t *testing.T) {
	i := readyImage(t)
	i.State = StateDeleting
	i.CleanupState = CleanupPending
	i.Native.DVUID = ""
	i.Native.PVCUID = ""
	leftover := ObjectRef{Kind: "PersistentVolumeClaim", Namespace: i.Native.Namespace, Name: "prime-" + i.ID, UID: "prime-uid"}
	source := ObjectRef{Kind: "DataVolume", Namespace: i.Native.Namespace, Name: "vmi-src-" + i.ID, UID: "dv-uid"}
	repo := &workerRepo{queue: []Image{i}}
	native := &workerNative{probeRefs: []ObjectRef{leftover, source}}
	w := newWorker(repo, native, workerEnv{})

	ctx := context.Background()
	if _, e := w.Step(ctx); e != nil {
		t.Fatalf("first cleanup pass: %v", e)
	}
	saved := repo.last()
	if saved.State == StateDeleted || saved.CleanupState == CleanupComplete {
		t.Fatalf("tombstone closed with a live leftover: state=%s cleanup=%s", saved.State, saved.CleanupState)
	}
	if saved.CleanupState == CleanupBlocked {
		t.Fatalf("probe result blocked the drain chain: reason=%q cleanup=%+v", saved.Reason, saved.Cleanup)
	}
	if len(saved.Cleanup) != 1 || saved.Cleanup[0].Name != leftover.Name {
		t.Fatalf("observed leftover not registered on its own: %+v", saved.Cleanup)
	}

	// A cluster that cannot answer is not read as "nothing left".
	repo.requeue()
	native.probeRefs, native.probeErr = nil, ErrImageDependency
	if _, e := w.Step(ctx); e != nil {
		t.Fatalf("probe outage: %v", e)
	}
	saved = repo.last()
	if saved.State == StateDeleted {
		t.Fatalf("tombstone closed while the cluster was unreachable")
	}

	native.probeErr = nil
	native.deleteDone = true
	repo.requeue()
	if _, e := w.Step(ctx); e != nil {
		t.Fatalf("final cleanup pass: %v", e)
	}
	saved = repo.last()
	if saved.State != StateDeleted || saved.CleanupState != CleanupComplete {
		t.Fatalf("clean chain did not close: state=%s cleanup=%s", saved.State, saved.CleanupState)
	}

	// A record that never bound a namespace can close: nothing of its own can exist.
	fresh := readyImage(t)
	fresh.State = StateDeleting
	fresh.CleanupState = CleanupPending
	fresh.Native = ImageNative{ClusterID: fresh.Native.ClusterID}
	repo.saved = append(repo.saved, fresh)
	repo.requeue()
	for pass := 1; pass <= 2; pass++ {
		if _, e := w.Step(ctx); e != nil {
			t.Fatalf("unbound record pass %d: %v", pass, e)
		}
		repo.requeue()
	}
	if saved := repo.last(); saved.CleanupState != CleanupComplete || saved.State != StateDeleted {
		t.Fatalf("unbound record did not close: state=%s cleanup=%s", saved.State, saved.CleanupState)
	}
}

// TestImageWorkerDropsProbeKindsLeftInARegisteredChain covers the records that were
// already BLOCKED in the cluster: an older worker had stored probe kinds in the chain,
// and the drain switch only knows PVC/PV, so every pass ended on ErrOwnership and the
// tombstone could never close even though the probe found nothing.
func TestImageWorkerDropsProbeKindsLeftInARegisteredChain(t *testing.T) {
	i := readyImage(t)
	i.State = StateDeleting
	i.CleanupState = CleanupBlocked
	i.Reason = "NATIVE_CLEANUP_BLOCKED"
	i.Native.DVUID = ""
	i.Native.PVCUID = ""
	i.Cleanup = []ObjectRef{
		{Kind: "DataVolume", Namespace: i.Native.Namespace, Name: "vmi-src-" + i.ID, UID: "dv-uid"},
		{Kind: "PersistentVolumeClaim", Namespace: i.Native.Namespace, Name: "prime-" + i.ID, UID: "prime-uid"},
	}
	repo := &workerRepo{queue: []Image{i}}
	native := &workerNative{}
	w := newWorker(repo, native, workerEnv{})

	ctx := context.Background()
	if _, e := w.Step(ctx); e != nil {
		t.Fatalf("cleanup pass: %v", e)
	}
	saved := repo.last()
	if saved.CleanupState == CleanupBlocked {
		t.Fatalf("legacy chain blocked the drain again: %+v", saved.Cleanup)
	}
	if len(saved.Cleanup) != 1 || saved.Cleanup[0].Kind != "PersistentVolumeClaim" {
		t.Fatalf("legacy probe kind survived in the chain: %+v", saved.Cleanup)
	}
	if native.deleted == 0 {
		t.Fatal("drain step never reached the native adapter")
	}
}

// TestImageWorkerNamespaceFailureParksWithoutLosingTheRecord covers the prepare
// branch that used to write the reason and retry on the flat cadence.
func TestImageWorkerNamespaceFailureParksWithoutLosingTheRecord(t *testing.T) {
	i := readyImage(t)
	i.State = StatePreparing
	i.Native.Namespace = ""
	repo := &workerRepo{queue: []Image{i}}
	w := newWorker(repo, &workerNative{}, workerEnv{err: ErrDependency})

	if _, e := w.Step(context.Background()); e != nil {
		t.Fatalf("namespace step: %v", e)
	}
	saved := repo.last()
	if saved.Reason != "NAMESPACE_INVALID" || saved.AttemptCount != 1 {
		t.Fatalf("namespace failure not parked: reason=%q attempt=%d", saved.Reason, saved.AttemptCount)
	}
	if saved.State != StatePreparing {
		t.Fatalf("namespace failure moved state to %q", saved.State)
	}
}

// TestImageWorkerLeavesARecordWaitingForBytesUnwritten keeps `version` meaning a
// business change. Round 1 inflated an idle IMPORTING record past version 30 because
// every 2s poll rewrote the row, which also made replay/version assertions meaningless.
func TestImageWorkerLeavesARecordWaitingForBytesUnwritten(t *testing.T) {
	i := readyImage(t)
	i.State = StateImporting
	i.CleanupState = CleanupNone
	waiting := i
	waiting.Native.DVPhase = "UploadReady"
	repo := &workerRepo{queue: []Image{i}}
	native := &workerNative{observeImage: waiting}
	w := newWorker(repo, native, workerEnv{})

	if _, e := w.Step(context.Background()); e != nil {
		t.Fatalf("waiting step: %v", e)
	}
	if len(repo.saved) != 0 {
		t.Fatalf("a poll with nothing new persisted %d rows (version would move without a business change)", len(repo.saved))
	}

	// Half-evidence is still not evidence: the DV succeeded but the source volume was
	// never observed, so the row must stay untouched rather than enter VERIFYING.
	half := waiting
	half.Native.DVPhase = "Succeeded"
	half.Native.PVCUID = ""
	repo.queue = []Image{i}
	if _, e := w.Step(context.Background()); e != nil {
		t.Fatalf("half-evidence step: %v", e)
	}
	if len(repo.saved) != 0 {
		t.Fatalf("incomplete clone evidence persisted %d rows: %+v", len(repo.saved), repo.last())
	}
}
