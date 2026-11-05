package vm

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	pb "github.com/zhangzhe-ctrl/ani-resource-service/api/vm/v1"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
)

type ticketRepo struct {
	biz.ImageRepository
	target    biz.Image
	confirmed int
}

func (r *ticketRepo) RecordSession(_ context.Context, _ biz.ImageCommand, _ string, _ time.Time) (biz.Image, error) {
	return r.target, nil
}
func (r *ticketRepo) ConfirmImageSession(context.Context, biz.Image) error { r.confirmed++; return nil }

type ticketNative struct {
	biz.ImageNativePort
	token string
}

func (n *ticketNative) CreateUploadToken(_ context.Context, i biz.Image, _ string, ttl time.Duration) (biz.Image, string, time.Time, error) {
	return i, n.token, time.Now().UTC().Add(ttl), nil
}

type ticketEnv struct{ biz.EnvironmentPort }

// TestCreateUploadSessionAdvertisesTheVerbatimCDIMethod is the #1 guard from the PR
// review. CDI's async upload handler answers any non-POST with 404, and the backend
// implementation plan requires method="POST"; a ticket that advertises PUT makes every
// faithful client fail on the first byte.
func TestCreateUploadSessionAdvertisesTheVerbatimCDIMethod(t *testing.T) {
	tenant, id := uuid.NewString(), uuid.NewString()
	target := biz.Image{TenantID: tenant, ID: id, MediaKey: biz.ImageMediaKey(id), Kind: "DISK_IMAGE", Format: "qcow2",
		Name: "disk", State: biz.StateImporting, CleanupState: biz.CleanupNone, Version: 2, SourceCapacityGB: 10,
		Native: biz.ImageNative{Namespace: "tenant-" + tenant, DVName: "vmi-src-x", DVUID: uuid.NewString(),
			PVCName: "vmi-src-x", PVCUID: uuid.NewString(), SourceVolumeMode: "Block", SourceContentType: "kubevirt"},
		OS: biz.ImageOS{OSFamily: "linux", OSVersion: "24.04", Architecture: "amd64", Firmware: "bios", RootBus: "virtio", DataBus: "scsi", NICModel: "virtio"}}
	repo := &ticketRepo{target: target}
	native := &ticketNative{token: "eyJhbGciOiJQUzI1NiJ9.eyJzdWIiOiJjZGkifQ.signature"}
	service, e := NewImageService(repo, native, ticketEnv{}, ImageUploadConfig{Enabled: true,
		ExternalUploadOrigin: "https://cdi-uploadproxy:30471",
		ValidatorImage:       "registry/ani-vm-image-verify@sha256:" + strings.Repeat("e", 64),
		UploadDeadline:       "24h", TokenTTL: "5m"}, "cluster-"+uuid.NewString(), slog.New(slog.DiscardHandler))
	if e != nil {
		t.Fatalf("service: %v", e)
	}
	ctx := biz.WithCaller(context.Background(), biz.Caller{TenantID: tenant, Actor: "user:1"})
	session, e := service.CreateUploadSession(ctx, &pb.CreateUploadSessionRequest{TenantId: tenant, ImageId: id, IdempotencyKey: uuid.NewString()})
	if e != nil {
		t.Fatalf("session refused: %v", e)
	}
	if session.Method != "POST" {
		t.Fatalf("ticket advertises %q, but CDI's /v1beta1/upload-async answers anything but POST with 404", session.Method)
	}
	if !strings.HasSuffix(session.UploadUrl, "/v1beta1/upload-async") {
		t.Fatalf("unexpected upload url %q", session.UploadUrl)
	}
	if repo.confirmed != 1 {
		t.Fatalf("session confirmation count = %d", repo.confirmed)
	}
}
