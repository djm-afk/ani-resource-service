package vm

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	pb "github.com/zhangzhe-ctrl/ani-resource-service/api/vm/v1"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
)

// TestImageUploadConfigAcceptsDigestPinnedValidator pins the reference shape the
// deployment path depends on. The off-by-one in dataImageNative only surfaced when
// ImageUpload.Enabled was switched to true on the cluster: with the switch off the
// branch never ran, so the service started and every management call still worked.
func TestImageUploadConfigAcceptsDigestPinnedValidator(t *testing.T) {
	good := "10.10.1.66:30003/ani-gpu-platform/ani-vm-image-verify@sha256:" + strings.Repeat("a", 64)
	if !dataImageNative(good) {
		t.Fatalf("digest-pinned validator image rejected: %s", good)
	}
	for _, bad := range []string{"", "alpine:3.22", "latest", good[:len(good)-1], strings.TrimPrefix(good, "10.10.1.66:30003/ani-gpu-platform/ani-vm-image-verify"),
		"repo@sha512:" + strings.Repeat("b", 64), "repo@sha256:" + strings.Repeat("c", 63)} {
		if dataImageNative(bad) {
			t.Errorf("invalid image reference accepted: %q", bad)
		}
	}
	upload := ImageUploadConfig{Enabled: true, ExternalUploadOrigin: "https://cdi-uploadproxy:30471",
		ValidatorImage: good, UploadDeadline: "24h", TokenTTL: "5m"}
	deadline, ttl, e := upload.Durations()
	if e != nil || deadline <= ttl {
		t.Fatalf("durations = %s/%s (%v)", deadline, ttl, e)
	}
	for _, bad := range []ImageUploadConfig{{UploadDeadline: "24h", TokenTTL: "24h"}, {UploadDeadline: "0"}, {TokenTTL: "-1m"},
		{UploadDeadline: "24x", TokenTTL: "5m"}, {UploadDeadline: "", TokenTTL: "bad"}} {
		if _, _, e := bad.Durations(); !errors.Is(e, biz.ErrImageInvalid) {
			t.Errorf("invalid durations accepted: %+v -> %v", bad, e)
		}
	}
	if _, e := NewImageService(nil, nil, nil, upload, "cluster", nil); !errors.Is(e, biz.ErrImageInvalid) {
		t.Errorf("incomplete wiring accepted: %v", e)
	}
	off := upload
	off.Enabled = false
	off.ValidatorImage = ""
	if _, e := NewImageService(nil, nil, nil, off, "cluster", nil); !errors.Is(e, biz.ErrImageInvalid) {
		t.Errorf("nil ports accepted while disabled: %v", e)
	}
}

// TestNewUploadedImageRecordShapesTheRowTheDomainExpects is the regression for the create
// path that returned VM_IMAGE_INVALID_ARGUMENT for every tenant on the cluster.
func TestNewUploadedImageRecordShapesTheRowTheDomainExpects(t *testing.T) {
	tenant, id := uuid.NewString(), uuid.NewString()
	profile, e := biz.ImageOSProfile("linux", "24.04", "amd64")
	if e != nil {
		t.Fatal(e)
	}
	request := &pb.CreateImageRequest{TenantId: tenant, Kind: "DISK_IMAGE", Format: "qcow2", Name: " e2e qcow2 ",
		Description: "kept", SourceCapacityGb: 8}
	deadline := time.Now().UTC().Add(3 * time.Hour)
	record, e := newUploadedImageRecord(request, id, profile, "admin", deadline)
	if e != nil {
		t.Fatalf("valid create rejected: %v", e)
	}
	if record.MediaKey != biz.ImageMediaKey(id) || record.State != biz.StatePreparing ||
		record.CleanupState != biz.CleanupNone || record.Version != 1 {
		t.Errorf("record identity/state not stamped: %+v", record)
	}
	if record.Name != "e2e qcow2" {
		t.Errorf("name not trimmed: %q", record.Name)
	}
	if record.CreatedBy != "admin" {
		t.Errorf("actor not recorded as creator: %q", record.CreatedBy)
	}
	// The configured window must travel with the record: the data layer only falls back
	// to its own default when the service left it unset.
	if record.UploadDeadline == nil || !record.UploadDeadline.Equal(deadline) {
		t.Errorf("upload deadline not taken from configuration: %v", record.UploadDeadline)
	}
	for _, bad := range []*pb.CreateImageRequest{{TenantId: tenant, Kind: "DISK_IMAGE", Format: "qcow2", SourceCapacityGb: 8},
		{TenantId: tenant, Kind: "DISK_IMAGE", Format: "qcow2", Name: strings.Repeat("n", 81), SourceCapacityGb: 8},
		{TenantId: tenant, Kind: "DISK_IMAGE", Format: "qcow2", Name: "x", SourceCapacityGb: 0},
		{TenantId: tenant, Kind: "INSTALL_ISO", Format: "qcow2", Name: "x", SourceCapacityGb: 8},
		{TenantId: "not-a-uuid", Kind: "DISK_IMAGE", Format: "qcow2", Name: "x", SourceCapacityGb: 8},
		{TenantId: tenant, Kind: "DISK_IMAGE", Format: "qcow2", Name: "x", SourceCapacityGb: 8, InitializationCapability: "SOMETHING"},
		// NoCloud on an ISO is refused by Validated(); a NoCloud claim on a Windows
		// profile is refused by the CreateImage handler itself, which owns the profile.
		{TenantId: tenant, Kind: "INSTALL_ISO", Format: "iso", Name: "x", SourceCapacityGb: 8, InitializationCapability: "CLOUD_INIT_NOCLOUD", Os: &pb.ImageOS{OsFamily: "linux", OsVersion: "24.04", Firmware: "bios", RootBus: "virtio", DataBus: "scsi", NicModel: "virtio"}}} {
		if _, e := newUploadedImageRecord(bad, id, profile, "admin", deadline); !errors.Is(e, biz.ErrImageInvalid) {
			t.Errorf("invalid record accepted: %+v -> %v", bad, e)
		}
	}
	declared := &pb.CreateImageRequest{TenantId: tenant, Kind: "DISK_IMAGE", Format: "qcow2", Name: "cloud-ready",
		SourceCapacityGb: 8, InitializationCapability: " CLOUD_INIT_NOCLOUD ", Os: &pb.ImageOS{OsFamily: "linux", OsVersion: "24.04", Firmware: "bios", RootBus: "virtio", DataBus: "scsi", NicModel: "virtio"}}
	withCap, e := newUploadedImageRecord(declared, id, profile, "admin", deadline)
	if e != nil {
		t.Fatalf("declared capability rejected: %v", e)
	}
	if withCap.InitializationCapability != biz.ImageInitNoCloud {
		t.Errorf("declared capability not frozen: %q", withCap.InitializationCapability)
	}
	if defaultCap, e := newUploadedImageRecord(request, id, profile, "admin", deadline); e != nil || defaultCap.InitializationCapability != biz.ImageInitNone {
		t.Errorf("absent capability not normalized to NONE: %v %q", e, defaultCap.InitializationCapability)
	}
}

// TestImageCommandOfStoresTheDigestedPayload is the second half of the create failure:
// the data layer rejects a command without a valid intent, so every write that only set
// RequestDigest returned VM_IMAGE_INVALID_ARGUMENT from the repository.
func TestImageCommandOfStoresTheDigestedPayload(t *testing.T) {
	tenant, id, key := uuid.NewString(), uuid.NewString(), uuid.NewString()
	fields := struct{ Name string }{"first"}
	command, e := imageCommandOf(biz.ImageCreateAction, tenant, id, key, fields, 0, nil)
	if e != nil {
		t.Fatal(e)
	}
	if len(command.RequestDigest) != 64 || !json.Valid(command.Intent) {
		t.Fatalf("digest/intent not stamped: %+v", command)
	}
	if string(command.Intent) != `{"Name":"first"}` {
		t.Fatalf("intent is not the digested value: %s", command.Intent)
	}
	other, e := imageCommandOf(biz.ImageCreateAction, tenant, id, key, struct{ Name string }{"second"}, 0, nil)
	if e != nil || other.RequestDigest == command.RequestDigest {
		t.Errorf("different payload reused the digest: %v %v", other.RequestDigest, e)
	}
	// An action the digest helper rejects must not produce a half-built command.
	if _, e = imageCommandOf(biz.ImageUpdateAction, tenant, id, key, fields, 1, []string{"", "name"}); e == nil {
		t.Error("empty mask path accepted")
	}
}

// TestValidateStaticMediaKeysKeepsReservedPrefix is the startup guard: a static media
// key that already uses the image prefix would let a catalog entry shadow a tenant image.
func TestValidateStaticMediaKeysKeepsReservedPrefix(t *testing.T) {
	if e := ValidateStaticMediaKeys(nil); e != nil {
		t.Errorf("empty catalog rejected: %v", e)
	}
	if e := ValidateStaticMediaKeys([]*pb.MediaReference{{MediaKey: "ubuntu-24.04-cloud"}, {MediaKey: "virtio-win-0.1.271"}}); e != nil {
		t.Errorf("clean catalog rejected: %v", e)
	}
	conflict := []*pb.MediaReference{{MediaKey: biz.ImageMediaKey(strings.Repeat("d", 32))}}
	if e := ValidateStaticMediaKeys(conflict); !errors.Is(e, biz.ErrImageInvalid) && e == nil {
		t.Errorf("reserved prefix accepted: %v", e)
	} else if e != nil && !strings.Contains(e.Error(), biz.ImageMediaKeyPrefix) {
		t.Errorf("conflict must name the offending key, got %v", e)
	}
}
