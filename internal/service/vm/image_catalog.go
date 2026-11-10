package vm

import (
	"context"

	pb "github.com/zhangzhe-ctrl/ani-resource-service/api/vm/v1"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	"google.golang.org/protobuf/proto"
)

// SetImageCatalog attaches the tenant image lane to the existing VM service.
// Without it the catalog is exactly the static configuration it was before this
// feature existed, so an unconfigured deployment cannot reach image code at all.
func (s *Service) SetImageCatalog(c biz.ImageCatalogPort) { s.images = c }

// resolveMedia is the only media-key lookup used by every admission path. A
// reserved image key never falls back to the static table.
func (s *Service) resolveMedia(ctx context.Context, tenant, key string) (*pb.MediaReference, error) {
	if !hasImagePrefix(key) {
		configured := s.media[key]
		if configured == nil {
			return nil, biz.ErrDependency
		}
		return proto.Clone(configured).(*pb.MediaReference), nil
	}
	if s.images == nil {
		return nil, biz.ErrDependency
	}
	i, e := s.images.ResolveImageMedia(ctx, tenant, key)
	if e != nil {
		if e == biz.ErrImageNotReady {
			return nil, biz.ErrImageNotReady
		}
		return nil, biz.ErrDependency
	}
	return imageMediaReference(i), nil
}

func hasImagePrefix(key string) bool {
	return len(key) >= len(biz.ImageMediaKeyPrefix) && key[:len(biz.ImageMediaKeyPrefix)] == biz.ImageMediaKeyPrefix
}

// imageMediaReference projects a READY image into the frozen media contract. The
// capability is the tenant's own create-time declaration (empty legacy rows read
// as NONE): the platform attests nothing about a tenant image, so what a tenant
// declared is exactly what admission sees.
func imageMediaReference(i biz.Image) *pb.MediaReference {
	return &pb.MediaReference{MediaKey: i.MediaKey, Name: i.Name, Kind: i.Kind,
		ClusterId: i.Native.ClusterID, Namespace: i.Native.Namespace, PvcName: i.Native.PVCName, PvcUid: i.Native.PVCUID,
		Checksum: i.SourceSHA256, VolumeMode: i.Native.SourceVolumeMode, ContentType: "kubevirt",
		VirtualCapacityBytes: i.ObservedSourceCapacityBytes, RequestedBytes: i.SourceRequestedBytes,
		OsFamily: i.OS.OSFamily, OsVersion: i.OS.OSVersion, Architecture: i.OS.Architecture,
		Firmware: i.OS.Firmware, RootBus: i.OS.RootBus, DataBus: i.OS.DataBus, NicModel: i.OS.NICModel,
		InitializationCapability: biz.ImageInitializationCapabilityFor(i.InitializationCapability)}
}

// dynamicMedia lists the tenant's READY images for the media picker. The static
// entries keep their exact order and content, so an existing page is unchanged.
func (s *Service) dynamicMedia(ctx context.Context, tenant string) ([]*pb.MediaReference, error) {
	if s.images == nil {
		return nil, nil
	}
	items, e := s.images.ListReadyImages(ctx, tenant)
	if e != nil {
		return nil, e
	}
	out := make([]*pb.MediaReference, 0, len(items))
	for _, item := range items {
		out = append(out, imageMediaReference(item))
	}
	return out, nil
}
