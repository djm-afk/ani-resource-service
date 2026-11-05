package vm

import (
	"strings"
	"testing"
	"time"
)

func readyImage(t *testing.T) Image {
	t.Helper()
	id := "2b1f0c2e-6d3a-4f57-9a10-3f7f5c1d8a11"
	tenant := "64c5c35a-0000-4000-8000-000000000001"
	i := Image{TenantID: tenant, ID: id, MediaKey: ImageMediaKey(id), Kind: "DISK_IMAGE", Format: "qcow2",
		Name: "tenant disk", State: StatePreparing, CleanupState: CleanupNone, SourceCapacityGB: 10, Version: 1,
		OS: ImageOS{OSFamily: "linux", OSVersion: "24.04", Architecture: "amd64", Firmware: "bios", RootBus: "virtio", DataBus: "scsi", NICModel: "virtio"}}
	i.Native = ImageNative{ClusterID: "f39bf6e2-f3ed-4191-ada3-caeebf86c220", Namespace: "tenant-" + tenant,
		DVName: "vmi-src-" + strings.ReplaceAll(id, "-", ""), DVUID: "dv-uid", PVCName: "vmi-src-" + strings.ReplaceAll(id, "-", ""),
		PVCUID: "pvc-uid", SourceVolumeMode: "Block", SourceContentType: "kubevirt"}
	i.SourceRequestedBytes, i.ObservedSourceCapacityBytes = 1<<30, 1<<30
	if _, e := i.Validated(); e != nil {
		t.Fatalf("fixture rejected: %v", e)
	}
	return i
}

func TestImageValidatedBindsKindToFormat(t *testing.T) {
	i := readyImage(t)
	i.Format = "iso"
	if _, e := i.Validated(); e != ErrImageInvalid {
		t.Fatalf("qcow2-only kind accepted iso format: %v", e)
	}
	i = readyImage(t)
	i.MediaKey = "vmimg:" + strings.ToUpper(i.ID[:8]) + i.ID[8:]
	if _, e := i.Validated(); e != ErrImageInvalid {
		t.Fatalf("media key not derived from the id was accepted")
	}
	i = readyImage(t)
	i.State = StateReady
	i.SourceSHA256 = "not-a-digest"
	if _, e := i.Validated(); e != ErrImageInvalid {
		t.Fatalf("READY without a full digest was accepted: %v", e)
	}
}

func TestImageTransitionTableClosesTerminalStates(t *testing.T) {
	cases := []struct {
		from, to string
		want     bool
	}{
		{StatePreparing, StateImporting, true}, {StateImporting, StateVerifying, true},
		{StateVerifying, StateReady, true}, {StateReady, StateUnavail, true},
		{StateUnavail, StateReady, false}, {StateFailed, StateReady, false},
		{StateDeleted, StatePreparing, false}, {StateDeleting, StateDeleted, true},
		{StateReady, StateImporting, false},
		// Staying put is legal: every worker step is re-entrant.
		{StateVerifying, StateVerifying, true},
	}
	for _, c := range cases {
		if got := ImageTransition(c.from, c.to); got != c.want {
			t.Errorf("%s->%s = %v, want %v", c.from, c.to, got, c.want)
		}
	}
}

// A deadline must never move UNAVAILABLE or READY, which is the exact failure
// mode of a "state != READY" guard.
func TestUploadDeadlineAppliesOnlyToInFlightStates(t *testing.T) {
	now := time.Now().UTC()
	past := now.Add(-time.Hour)
	for _, state := range []string{StatePreparing, StateImporting, StateVerifying} {
		i := readyImage(t)
		i.State, i.UploadDeadline = state, &past
		if !UploadDeadlineElapsed(i, now) {
			t.Errorf("%s expired deadline not reported", state)
		}
	}
	for _, state := range []string{StateReady, StateUnavail, StateFailed, StateDeleting} {
		i := readyImage(t)
		i.State, i.UploadDeadline = state, &past
		if state == StateReady {
			i.SourceSHA256 = strings.Repeat("a", 64)
			if _, e := i.Validated(); e != nil {
				t.Fatalf("ready fixture: %v", e)
			}
		}
		if UploadDeadlineElapsed(i, now) {
			t.Errorf("%s must never be deadline-collected", state)
		}
	}
	i := readyImage(t)
	i.State, i.UploadDeadline = StateImporting, nil
	if UploadDeadlineElapsed(i, now) {
		t.Error("missing deadline reported as elapsed")
	}
}

func TestPublishReadyRefusesLateAndIncompleteEvidence(t *testing.T) {
	i := readyImage(t)
	i.State = StateVerifying
	now := time.Now().UTC()
	if e := i.PublishReady("pvc-uid", 1<<30, 1<<30, 1<<30, strings.Repeat("b", 64), now); e != nil {
		t.Fatalf("valid evidence rejected: %v", e)
	}
	late := readyImage(t)
	late.State = StateVerifying
	late.SourceSHA256 = strings.Repeat("b", 64)
	if e := late.PublishReady("other-pvc-uid", 1<<30, 1<<30, 1<<30, strings.Repeat("c", 64), now); e != ErrImageIdentity {
		t.Errorf("PVC identity swap accepted: %v", e)
	}
	short := readyImage(t)
	short.State = StateVerifying
	if e := short.PublishReady("pvc-uid", 1<<30, 1<<29, 1<<30, strings.Repeat("b", 64), now); e != ErrImageIdentity {
		t.Errorf("observed below request accepted: %v", e)
	}
	deleted := readyImage(t)
	deleted.State, deleted.CleanupState = StateDeleting, CleanupPending
	if e := deleted.PublishReady("pvc-uid", 1<<30, 1<<30, 1<<30, strings.Repeat("b", 64), now); e != ErrImageNotReady {
		t.Errorf("delete winner reopened: %v", e)
	}
}

func TestImageRequestDigestBindingRules(t *testing.T) {
	fields := struct {
		Name, Description string
	}{Name: "a", Description: "b"}
	base, e := ImageRequestDigest(ImageUpdateAction, "64c5c35a-0000-4000-8000-000000000001", "2b1f0c2e-6d3a-4f57-9a10-3f7f5c1d8a11", fields, 3, []string{"description", "name"})
	if e != nil {
		t.Fatalf("digest: %v", e)
	}
	reordered, e := ImageRequestDigest(ImageUpdateAction, "64c5c35a-0000-4000-8000-000000000001", "2b1f0c2e-6d3a-4f57-9a10-3f7f5c1d8a11", fields, 3, []string{"name", "description"})
	if e != nil || reordered != base {
		t.Fatalf("mask order changed the digest: %v %s/%s", e, base, reordered)
	}
	for name, args := range map[string][]any{
		"tenant":  {ImageUpdateAction, "64c5c35a-0000-4000-8000-000000000002", "2b1f0c2e-6d3a-4f57-9a10-3f7f5c1d8a11", fields, int64(3), []string{"name", "description"}},
		"target":  {ImageUpdateAction, "64c5c35a-0000-4000-8000-000000000001", "2b1f0c2e-6d3a-4f57-9a10-3f7f5c1d8a12", fields, int64(3), []string{"name", "description"}},
		"version": {ImageUpdateAction, "64c5c35a-0000-4000-8000-000000000001", "2b1f0c2e-6d3a-4f57-9a10-3f7f5c1d8a11", fields, int64(4), []string{"name", "description"}},
		"action":  {ImageDeleteAction, "64c5c35a-0000-4000-8000-000000000001", "2b1f0c2e-6d3a-4f57-9a10-3f7f5c1d8a11", fields, int64(3), []string{"name", "description"}},
		"fields": {ImageUpdateAction, "64c5c35a-0000-4000-8000-000000000001", "2b1f0c2e-6d3a-4f57-9a10-3f7f5c1d8a11", struct {
			Name, Description string
		}{"a", "c"}, int64(3), []string{"name", "description"}},
	} {
		got, e := ImageRequestDigest(args[0].(string), args[1].(string), args[2].(string), args[3], args[4].(int64), args[5].([]string))
		if e != nil {
			t.Errorf("%s: %v", name, e)
			continue
		}
		if got == base {
			t.Errorf("%s change did not alter the digest", name)
		}
	}
	if _, e := ImageRequestDigest(ImageUpdateAction, "64c5c35a-0000-4000-8000-000000000001", "2b1f0c2e-6d3a-4f57-9a10-3f7f5c1d8a11", fields, 3, []string{"name", "name"}); e != ErrImageInvalid {
		t.Errorf("repeated mask item accepted: %v", e)
	}
	if _, e := ImageRequestDigest("/vm.v1.VMService/Start", "64c5c35a-0000-4000-8000-000000000001", "2b1f0c2e-6d3a-4f57-9a10-3f7f5c1d8a11", fields, 3, nil); e != ErrImageInvalid {
		t.Errorf("non-image action accepted: %v", e)
	}
}

// TestImageCreateDigestIgnoresTheServerAllocatedID is the A08 guard: a create names no
// target in the request, so its digest must not depend on the id the server happens to
// allocate for that attempt. Binding it made every identical replay read as a different
// request and the retry was refused with 409 instead of returning the first resource.
func TestImageCreateDigestIgnoresTheServerAllocatedID(t *testing.T) {
	fields := struct {
		Name, Description string
	}{Name: "a", Description: "b"}
	tenant := "64c5c35a-0000-4000-8000-000000000001"
	first, e := ImageRequestDigest(ImageCreateAction, tenant, "2b1f0c2e-6d3a-4f57-9a10-3f7f5c1d8a11", fields, 0, nil)
	if e != nil {
		t.Fatalf("digest: %v", e)
	}
	second, e := ImageRequestDigest(ImageCreateAction, tenant, "9f8e7d6c-5b4a-4392-8170-6f5e4d3c2b1a", fields, 0, nil)
	if e != nil {
		t.Fatalf("digest: %v", e)
	}
	if first != second {
		t.Fatalf("create digest depends on the server-allocated id: %s != %s", first, second)
	}
	// Updates and deletes still bind the path target: there the id is client input.
	update, e := ImageRequestDigest(ImageUpdateAction, tenant, "2b1f0c2e-6d3a-4f57-9a10-3f7f5c1d8a11", fields, 0, nil)
	if e != nil {
		t.Fatalf("update digest: %v", e)
	}
	otherUpdate, e := ImageRequestDigest(ImageUpdateAction, tenant, "9f8e7d6c-5b4a-4392-8170-6f5e4d3c2b1a", fields, 0, nil)
	if e != nil {
		t.Fatalf("update digest: %v", e)
	}
	if update == otherUpdate {
		t.Fatal("update digest stopped binding its path target")
	}
}

func TestImageMediaKeyRoundTrip(t *testing.T) {
	id := "2b1f0c2e-6d3a-4f57-9a10-3f7f5c1d8a11"
	key := ImageMediaKey(id)
	if !strings.HasPrefix(key, ImageMediaKeyPrefix) {
		t.Fatalf("key %q lost the reserved prefix", key)
	}
	back, e := ImageIDFromMediaKey(key)
	if e != nil || back != id {
		t.Fatalf("round trip failed: %v %s", e, back)
	}
	for _, bad := range []string{"virtio-win-0.1.271", ImageMediaKeyPrefix + "not-a-uuid", ImageMediaKeyPrefix + strings.ToUpper(id)} {
		if _, e := ImageIDFromMediaKey(bad); e != ErrImageInvalid {
			t.Errorf("%q accepted as an image key: %v", bad, e)
		}
	}
}

// The fingerprint must be identical whether it is derived from the READY record
// or from the source already frozen inside a VM spec, or pins would self-reject.
func TestImageSourceFingerprintIsPathIndependent(t *testing.T) {
	i := readyImage(t)
	i.SourceSHA256 = strings.Repeat("d", 64)
	fromImage := ImageSourceFingerprint(i)
	ref, e := NewImageReference(VM{TenantID: i.TenantID, ID: "64c5c35a-0000-4000-8000-000000000009"}, "2b1f0c2e-6d3a-4f57-9a10-3f7f5c1d8a12", "SYSTEM_DISK", i.Source())
	if e != nil {
		t.Fatalf("reference: %v", e)
	}
	if ref.SourceFingerprint != fromImage {
		t.Fatalf("fingerprint diverged: %s vs %s", ref.SourceFingerprint, fromImage)
	}
	if ref.ImageID != i.ID || ref.State != "HELD" {
		t.Fatalf("reference identity: %+v", ref)
	}
	static := i.Source()
	static.Key = "ubuntu-24.04-cloud"
	if _, e = NewImageReference(VM{TenantID: i.TenantID}, "2b1f0c2e-6d3a-4f57-9a10-3f7f5c1d8a12", "SYSTEM_DISK", static); e != ErrImageInvalid {
		t.Errorf("static key pinned as an image: %v", e)
	}
}

func TestVerificationResultValidatedFor(t *testing.T) {
	i := readyImage(t)
	i.State = StateVerifying
	i.Native.JobUID, i.Native.VerifierImage = "job-uid", "registry/ani-vm-image-verify@sha256:"+strings.Repeat("e", 64)
	good := VerificationResult{Pass: true, ImageID: i.ID, Checksum: strings.Repeat("a", 64), Bytes: 1 << 30,
		VolumeMode: "Block", PVCUID: i.Native.PVCUID, RequestedBytes: 1 << 30, ObservedBytes: 1 << 30,
		ReadOnlyProof: "O_WRONLY=permission denied,O_RDWR=permission denied", JobUID: "job-uid", PodUID: "pod-uid",
		VerifierImage: i.Native.VerifierImage}
	if e := good.ValidatedFor(i); e != nil {
		t.Fatalf("valid result rejected: %v", e)
	}
	moved := good
	moved.JobUID = "another-job"
	if e := moved.ValidatedFor(i); e != ErrImageIdentity {
		t.Errorf("result from another Job adopted: %v", e)
	}
	unproven := good
	unproven.ReadOnlyProof = ""
	if e := unproven.ValidatedFor(i); e != ErrImageIdentity {
		t.Errorf("write-open probe skipped: %v", e)
	}
	truncated := good
	truncated.Bytes = 1024
	truncated.VolumeMode = "Block"
	if e := truncated.ValidatedFor(i); e != nil {
		t.Errorf("short hash length is a cluster-side comparison, not a schema reject: %v", e)
	}
	iso := readyImage(t)
	iso.Kind, iso.Format, iso.Native.SourceVolumeMode = "INSTALL_ISO", "iso", "Filesystem"
	iso.Native.JobUID, iso.Native.VerifierImage = "job-uid", i.Native.VerifierImage
	isoResult := good
	isoResult.VolumeMode, isoResult.FSType = "Filesystem", "ext4"
	if e := isoResult.ValidatedFor(iso); e != ErrImageIdentity {
		t.Errorf("non-ISO filesystem signature accepted: %v", e)
	}
	isoResult.FSType = "iso9660"
	if e := isoResult.ValidatedFor(iso); e != nil {
		t.Errorf("iso9660 signature rejected: %v", e)
	}
	failed := VerificationResult{Pass: false, ImageID: i.ID, FailureReason: "SOURCE_VALIDATION_FAILED"}
	if e := failed.ValidatedFor(i); e != nil {
		t.Errorf("structured failure rejected: %v", e)
	}
	failed.FailureReason = "something from kubectl"
	if e := failed.ValidatedFor(i); e != ErrImageIdentity {
		t.Errorf("unregistered failure reason adopted: %v", e)
	}
}

func TestImageListFilterValidated(t *testing.T) {
	f, e := ImageListFilter{}.Validated()
	if e != nil || f.Page != 1 || f.PageSize != 20 {
		t.Fatalf("defaults: %v %+v", e, f)
	}
	for _, bad := range []ImageListFilter{{PageSize: 101}, {State: "RUNNING"}, {Kind: "AMI"}, {Keyword: strings.Repeat("x", 81)}} {
		if _, e := bad.Validated(); e != ErrImageInvalid {
			t.Errorf("%+v accepted: %v", bad, e)
		}
	}
	f, e = ImageListFilter{State: "all", Kind: "INSTALL_ISO"}.Validated()
	if e != nil || f.State != "" {
		t.Fatalf("all-state filter: %v %+v", e, f)
	}
}

func TestStaticCatalogRejectsReservedPrefix(t *testing.T) {
	if e := ImageCatalogConflict(ImageMediaKeyPrefix + "x"); e == nil || !strings.Contains(e.Error(), ImageMediaKeyPrefix) {
		t.Fatalf("collision error must name the prefix: %v", e)
	}
	if _, e := ImageOSProfile("linux", "24.04", ""); e != nil {
		t.Fatalf("profile: %v", e)
	}
	if _, e := ImageOSProfile("BSD", "1", ""); e != ErrImageInvalid {
		t.Errorf("unknown family accepted: %v", e)
	}
}
