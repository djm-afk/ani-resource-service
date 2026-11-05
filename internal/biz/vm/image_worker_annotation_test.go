package vm

import (
	"context"
	"strings"
	"testing"
)

// TestImageWorkerPublishesTheVerifiedChecksumAnnotation is the #3 guard: the annotation
// has to carry the checksum the verifier actually reported. Publishing it before the
// record has one wrote an empty annotation, and the read-back check passed vacuously
// because both sides were empty, leaving a READY record the consumer path rejects.
func TestImageWorkerPublishesTheVerifiedChecksumAnnotation(t *testing.T) {
	i := readyImage(t)
	i.State = StateVerifying
	i.CleanupState = CleanupNone
	checksum := strings.Repeat("a", 64)
	result := VerificationResult{Pass: true, Checksum: checksum, Bytes: 1 << 30,
		VolumeMode: "Block", PVCUID: i.Native.PVCUID, RequestedBytes: i.SourceRequestedBytes,
		ObservedBytes: i.ObservedSourceCapacityBytes, JobUID: i.Native.JobUID, PodUID: "pod-uid",
		VerifierImage: i.Native.VerifierImage, ImageID: i.ID}
	repo := &workerRepo{queue: []Image{i}}
	native := &workerNative{verifyImage: i, verifyResult: result}
	w := newWorker(repo, native, workerEnv{})

	if _, e := w.Step(context.Background()); e != nil {
		t.Fatalf("verifying step: %v", e)
	}
	if native.annotated == nil {
		t.Fatal("annotation was never published")
	}
	if native.annotated.SourceSHA256 != checksum {
		t.Fatalf("annotation published with %q, want the verified %q", native.annotated.SourceSHA256, checksum)
	}
	if saved := repo.last(); saved.State != StateReady || saved.SourceSHA256 != checksum {
		t.Fatalf("record did not reach READY with the verified digest: state=%s sha=%q", saved.State, saved.SourceSHA256)
	}
}
