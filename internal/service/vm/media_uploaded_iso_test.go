package vm

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	pb "github.com/zhangzhe-ctrl/ani-resource-service/api/vm/v1"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
)

type uploadedISOCatalog struct {
	biz.ImageCatalogPort
	image biz.Image
}

func (c *uploadedISOCatalog) ResolveImageMedia(context.Context, string, string) (biz.Image, error) {
	return c.image, nil
}

func uploadedISOImage(tenant string) biz.Image {
	id := uuid.NewString()
	return biz.Image{TenantID: tenant, ID: id, MediaKey: biz.ImageMediaKey(id), Name: "installer",
		Kind: "INSTALL_ISO", Format: "iso", State: biz.StateReady, CleanupState: biz.CleanupNone, Version: 6,
		SourceCapacityGB: 2, SourceSHA256: strings.Repeat("b", 64), SourceChecksumBytes: 1 << 30,
		Native: biz.ImageNative{Namespace: "tenant-" + tenant, PVCName: "vmi-src-iso", PVCUID: uuid.NewString(),
			DVUID: uuid.NewString(), SourceVolumeMode: "Filesystem", SourceContentType: "kubevirt"},
		OS: biz.ImageOS{OSFamily: "linux", OSVersion: "24.04", Architecture: "amd64", Firmware: "uefi", RootBus: "virtio", DataBus: "scsi", NICModel: "virtio"}}
}

// TestSetMediaAcceptsTenantUploadedISOForFirstMount is the #8 guard: ListMedia and the
// create path already resolve tenant-uploaded media through resolveMedia, so a first
// attach must not fall back to the administrator's static table, which never contains
// the uploaded key.
func TestSetMediaAcceptsTenantUploadedISOForFirstMount(t *testing.T) {
	service, repo, native, ctx, request := serviceMediaFixture()
	tenant := repo.current.TenantID
	uploaded := uploadedISOImage(tenant)
	// A READY uploaded image always carries the cluster it was verified in and the
	// capacity its source proved, exactly as the media contract requires.
	uploaded.Native.ClusterID = repo.current.Spec.ClusterID
	uploaded.SourceRequestedBytes = 2 << 30
	uploaded.ObservedSourceCapacityBytes = 2 << 30
	service.media = map[string]*pb.MediaReference{}
	service.SetImageCatalog(&uploadedISOCatalog{image: uploaded})
	request.MediaKey = uploaded.MediaKey
	request.ExpectedVersion = repo.current.Version

	operation, e := service.SetMedia(ctx, request)
	if e != nil {
		t.Fatalf("first attach of an uploaded ISO refused: %v", e)
	}
	if operation.Id == "" || repo.frozen.Copy == nil {
		t.Fatalf("no media copy frozen: %+v", repo.frozen)
	}
	if repo.frozen.Copy.Source.PVCUID != uploaded.Native.PVCUID || native.validated == 0 {
		t.Fatalf("uploaded source not frozen or native source never validated: %+v", repo.frozen.Copy.Source)
	}
}
