package service

import (
	"context"

	imagev1 "github.com/zhangzhe-ctrl/ani-resource-service/api/image/v1"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/image"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// PlatformService is the platform operator surface. Every method requires the
// platform caller identity injected by the governance interceptor; no request
// field can select a tenant.
type PlatformService struct {
	imagev1.UnimplementedPlatformImageServiceServer
	platform *biz.Platform
}

func NewPlatformService(platform *biz.Platform) *PlatformService {
	return &PlatformService{platform: platform}
}
func (s *PlatformService) GetPlatformSpace(ctx context.Context, r *imagev1.GetPlatformSpaceRequest) (*imagev1.GetPlatformSpaceResponse, error) {
	v, err := s.platform.GetPlatformSpace(ctx)
	if err != nil {
		return nil, rpcError(err)
	}
	out := &imagev1.PlatformSpaceView{ConfiguredProject: v.ConfiguredProject, ConfiguredRegistryAuthority: v.ConfiguredRegistryAuthority, BindingStatus: v.BindingStatus}
	if v.Space != nil {
		out.Space = wireSpace(*v.Space)
	}
	return &imagev1.GetPlatformSpaceResponse{View: out}, nil
}
func (s *PlatformService) InitializePlatform(ctx context.Context, r *imagev1.InitializePlatformRequest) (*imagev1.InitializePlatformResponse, error) {
	v, err := s.platform.InitializePlatform(ctx, biz.InitPlatform{IdempotencyKey: r.GetIdempotencyKey()})
	if err != nil {
		return nil, rpcError(err)
	}
	return &imagev1.InitializePlatformResponse{Space: wireSpace(v)}, nil
}
func (s *PlatformService) GetPlatformPublisherCredential(ctx context.Context, r *imagev1.GetPlatformPublisherCredentialRequest) (*imagev1.GetPlatformPublisherCredentialResponse, error) {
	v, err := s.platform.GetPlatformPublisherCredential(ctx)
	if err != nil {
		return nil, rpcError(err)
	}
	return &imagev1.GetPlatformPublisherCredentialResponse{Credential: wireCredential(v)}, nil
}
func (s *PlatformService) IssuePlatformPublisherCredential(ctx context.Context, r *imagev1.IssuePlatformPublisherCredentialRequest) (*imagev1.IssuePlatformPublisherCredentialResponse, error) {
	expected, err := requiredVersion(r.ExpectedVersion)
	if err != nil {
		return nil, rpcError(err)
	}
	v, err := s.platform.IssuePlatformPublisher(ctx, biz.IssuePlatformCredential{IdempotencyKey: r.GetIdempotencyKey(), ExpectedVersion: expected})
	if err != nil {
		return nil, rpcError(err)
	}
	defer clear(v.Secret)
	return &imagev1.IssuePlatformPublisherCredentialResponse{Credential: wireCredential(v.Credential), Secret: string(v.Secret), ReplayUntil: timestamppb.New(v.ReplayUntil)}, nil
}
func (s *PlatformService) ResetPlatformPublisherCredential(ctx context.Context, r *imagev1.ResetPlatformPublisherCredentialRequest) (*imagev1.ResetPlatformPublisherCredentialResponse, error) {
	expected, err := requiredVersion(r.ExpectedVersion)
	if err != nil {
		return nil, rpcError(err)
	}
	v, err := s.platform.IssuePlatformPublisher(ctx, biz.IssuePlatformCredential{IdempotencyKey: r.GetIdempotencyKey(), ExpectedVersion: expected, Rotate: true})
	if err != nil {
		return nil, rpcError(err)
	}
	defer clear(v.Secret)
	return &imagev1.ResetPlatformPublisherCredentialResponse{Credential: wireCredential(v.Credential), Secret: string(v.Secret), ReplayUntil: timestamppb.New(v.ReplayUntil)}, nil
}
func (s *PlatformService) DisablePlatformPublisherCredential(ctx context.Context, r *imagev1.DisablePlatformPublisherCredentialRequest) (*imagev1.DisablePlatformPublisherCredentialResponse, error) {
	expected, err := requiredVersion(r.ExpectedVersion)
	if err != nil {
		return nil, rpcError(err)
	}
	v, err := s.platform.DisablePlatformPublisher(ctx, biz.DisableCredential{IdempotencyKey: r.GetIdempotencyKey(), ExpectedVersion: expected})
	if err != nil {
		return nil, rpcError(err)
	}
	return &imagev1.DisablePlatformPublisherCredentialResponse{Credential: wireCredential(v)}, nil
}
func (s *PlatformService) ListPlatformImages(ctx context.Context, r *imagev1.ListPlatformImagesRequest) (*imagev1.ListPlatformImagesResponse, error) {
	f, p, err := readManagementList(r.GetSearch(), r.GetPurposes(), r.GetAccelerator(), r.Page, r.PageSize)
	if err != nil {
		return nil, rpcError(err)
	}
	v, err := s.platform.ListPlatformImagesPage(ctx, f, p)
	if err != nil {
		return nil, rpcError(err)
	}
	out := &imagev1.ManagementImagePage{Total: v.Total, Items: make([]*imagev1.ImageRegistration, 0, len(v.Items))}
	for _, item := range v.Items {
		out.Items = append(out.Items, wireRegistration(item))
	}
	return &imagev1.ListPlatformImagesResponse{Page: out}, nil
}
func (s *PlatformService) GetPlatformImage(ctx context.Context, r *imagev1.GetPlatformImageRequest) (*imagev1.GetPlatformImageResponse, error) {
	v, err := s.platform.GetPlatformImage(ctx, r.GetImageId())
	if err != nil {
		return nil, rpcError(err)
	}
	return &imagev1.GetPlatformImageResponse{Image: wireRegistration(v)}, nil
}
func (s *PlatformService) RegisterPlatformImage(ctx context.Context, r *imagev1.RegisterPlatformImageRequest) (*imagev1.RegisterPlatformImageResponse, error) {
	m, err := readMetadata(r.GetDisplayName(), r.GetDescription(), r.GetPurposes(), r.GetAccelerator())
	if err != nil {
		return nil, rpcError(err)
	}
	v, err := s.platform.RegisterPlatformImage(ctx, biz.RegisterImage{ImageReference: r.GetImageReference(), IdempotencyKey: r.GetIdempotencyKey(), Metadata: m})
	if err != nil {
		return nil, rpcError(err)
	}
	return &imagev1.RegisterPlatformImageResponse{Image: wireRegistration(v)}, nil
}
func (s *PlatformService) UpdatePlatformImage(ctx context.Context, r *imagev1.UpdatePlatformImageRequest) (*imagev1.UpdatePlatformImageResponse, error) {
	in, err := readPatch(r.GetImageId(), "", r.DisplayName, r.Description, r.GetPurposes(), r.Accelerator, r.ExpectedVersion, r.GetIdempotencyKey(), r.GetUpdateMask())
	if err != nil {
		return nil, rpcError(err)
	}
	v, err := s.platform.PatchPlatformImage(ctx, in)
	if err != nil {
		return nil, rpcError(err)
	}
	return &imagev1.UpdatePlatformImageResponse{Image: wireRegistration(v)}, nil
}
func (s *PlatformService) UnregisterPlatformImage(ctx context.Context, r *imagev1.UnregisterPlatformImageRequest) (*imagev1.UnregisterPlatformImageResponse, error) {
	expected, err := requiredVersion(r.ExpectedVersion)
	if err != nil {
		return nil, rpcError(err)
	}
	v, err := s.platform.UnregisterPlatformImage(ctx, biz.UnregisterImage{ImageID: r.GetImageId(), ExpectedVersion: expected, IdempotencyKey: r.GetIdempotencyKey()})
	if err != nil {
		return nil, rpcError(err)
	}
	return &imagev1.UnregisterPlatformImageResponse{Image: wireRegistration(v)}, nil
}

// requiredVersion enforces an explicitly provided expected version. A missing
// optional field and an explicit zero are both rejected.
func requiredVersion(v *int64) (int64, error) {
	if v == nil || *v < 0 {
		return 0, biz.Fail(biz.InvalidArgument, "explicit expected version required")
	}
	return *v, nil
}

// readManagementList applies the 1/20 defaults and the 100 ceiling; explicit
// out-of-range values are rejected rather than silently repaired.
func readManagementList(search string, purposes []imagev1.ImagePurpose, accelerator imagev1.AcceleratorKind, page, pageSize *uint32) (biz.ManagementFilter, biz.PageRequest, error) {
	values := make([]string, 0, len(purposes))
	for _, v := range purposes {
		name, ok := purposeNames[v]
		if !ok {
			return biz.ManagementFilter{}, biz.PageRequest{}, biz.Fail(biz.InvalidArgument, "invalid purpose")
		}
		values = append(values, name)
	}
	name, ok := acceleratorNames[accelerator]
	if !ok {
		return biz.ManagementFilter{}, biz.PageRequest{}, biz.Fail(biz.InvalidArgument, "invalid accelerator")
	}
	p, size := uint32(1), uint32(20)
	if page != nil {
		p = *page
	}
	if pageSize != nil {
		size = *pageSize
	}
	return biz.ManagementFilter{Search: search, Purposes: values, Accelerator: name}, biz.PageRequest{Page: p, PageSize: size}, nil
}

// readPatch converts the wire patch into the domain patch. Presence is taken
// from the optional fields; the mask is checked in the domain.
func readPatch(imageID, tenantID string, displayName, description *string, purposes []imagev1.ImagePurpose, accelerator *imagev1.AcceleratorKind, expected *int64, key string, mask *fieldmaskpb.FieldMask) (biz.PatchImage, error) {
	version, err := requiredVersion(expected)
	if err != nil {
		return biz.PatchImage{}, err
	}
	if version < 1 {
		return biz.PatchImage{}, biz.Fail(biz.InvalidArgument, "positive expected version required")
	}
	if mask == nil || len(mask.GetPaths()) == 0 {
		return biz.PatchImage{}, biz.Fail(biz.InvalidArgument, "non-empty update mask required")
	}
	in := biz.PatchImage{TenantID: tenantID, ImageID: imageID, ExpectedVersion: version, IdempotencyKey: key, Mask: append([]string(nil), mask.GetPaths()...)}
	in.Patch.DisplayName = displayName
	in.Patch.Description = description
	// A repeated field has no presence bit: only a non-empty list counts as
	// "provided", which keeps "mask without purposes" unambiguous.
	if len(purposes) > 0 {
		values := make([]string, 0, len(purposes))
		for _, v := range purposes {
			name, ok := purposeNames[v]
			if !ok {
				return biz.PatchImage{}, biz.Fail(biz.InvalidArgument, "invalid purpose")
			}
			values = append(values, name)
		}
		in.Patch.Purposes = &values
	}
	if accelerator != nil {
		name, ok := acceleratorNames[*accelerator]
		if !ok {
			return biz.PatchImage{}, biz.Fail(biz.InvalidArgument, "invalid accelerator")
		}
		in.Patch.Accelerator = &name
	}
	return in, nil
}
