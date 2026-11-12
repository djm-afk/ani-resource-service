package service

import (
	"context"

	imagev1 "github.com/zhangzhe-ctrl/ani-resource-service/api/image/v1"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/image"
)

// AdminTenantService is the platform-side managed-tenant surface. The named
// tenant is only ever taken from the path/UUID supplied by Governance; the
// operator identity stays in the caller context and is never replaced.
type AdminTenantService struct {
	imagev1.UnimplementedAdminTenantImageServiceServer
	tenant  *TenantService
	catalog *biz.Catalog
}

func NewAdminTenantService(tenant *TenantService, catalog *biz.Catalog) *AdminTenantService {
	return &AdminTenantService{tenant: tenant, catalog: catalog}
}

// withManagedTenant proves the platform caller and binds the target tenant as
// the domain caller. Only this file uses it.
func (s *AdminTenantService) withManagedTenant(ctx context.Context, target string) (context.Context, error) {
	p, err := biz.RequirePlatform(ctx)
	if err != nil {
		return nil, err
	}
	if _, err = biz.ParseTenant(target); err != nil {
		return nil, err
	}
	return biz.WithCaller(ctx, biz.Caller{Kind: biz.GovernanceCaller, Subject: p.Subject, Actor: p.Actor, TenantID: target}), nil
}
func (s *AdminTenantService) GetTenantImageSpace(ctx context.Context, r *imagev1.GetTenantImageSpaceRequest) (*imagev1.GetTenantImageSpaceResponse, error) {
	ctx, err := s.withManagedTenant(ctx, r.GetTargetTenantId())
	if err != nil {
		return nil, rpcError(err)
	}
	v, err := s.tenant.GetImageSpace(ctx, &imagev1.GetImageSpaceRequest{TenantId: r.GetTargetTenantId()})
	if err != nil {
		return nil, err
	}
	return &imagev1.GetTenantImageSpaceResponse{Space: v.GetSpace()}, nil
}
func (s *AdminTenantService) EnsureTenantImageSpace(ctx context.Context, r *imagev1.EnsureTenantImageSpaceRequest) (*imagev1.EnsureTenantImageSpaceResponse, error) {
	ctx, err := s.withManagedTenant(ctx, r.GetTargetTenantId())
	if err != nil {
		return nil, rpcError(err)
	}
	v, err := s.tenant.EnsureImageSpace(ctx, &imagev1.EnsureImageSpaceRequest{TenantId: r.GetTargetTenantId(), Slug: r.GetSlug(), IdempotencyKey: r.GetIdempotencyKey()})
	if err != nil {
		return nil, err
	}
	return &imagev1.EnsureTenantImageSpaceResponse{Space: v.GetSpace()}, nil
}
func (s *AdminTenantService) GetTenantPublisherCredential(ctx context.Context, r *imagev1.GetTenantPublisherCredentialRequest) (*imagev1.GetTenantPublisherCredentialResponse, error) {
	ctx, err := s.withManagedTenant(ctx, r.GetTargetTenantId())
	if err != nil {
		return nil, rpcError(err)
	}
	v, err := s.tenant.GetPublisherCredential(ctx, &imagev1.GetPublisherCredentialRequest{TenantId: r.GetTargetTenantId()})
	if err != nil {
		return nil, err
	}
	return &imagev1.GetTenantPublisherCredentialResponse{Credential: v.GetCredential()}, nil
}
func (s *AdminTenantService) IssueTenantPublisherCredential(ctx context.Context, r *imagev1.IssueTenantPublisherCredentialRequest) (*imagev1.IssueTenantPublisherCredentialResponse, error) {
	expected, err := requiredVersion(r.ExpectedVersion)
	if err != nil {
		return nil, rpcError(err)
	}
	ctx, err = s.withManagedTenant(ctx, r.GetTargetTenantId())
	if err != nil {
		return nil, rpcError(err)
	}
	v, err := s.tenant.IssuePublisherCredential(ctx, &imagev1.IssuePublisherCredentialRequest{TenantId: r.GetTargetTenantId(), IdempotencyKey: r.GetIdempotencyKey(), ExpectedVersion: expected})
	if err != nil {
		return nil, err
	}
	return &imagev1.IssueTenantPublisherCredentialResponse{Credential: v.GetCredential(), Secret: v.GetSecret(), ReplayUntil: v.GetReplayUntil()}, nil
}
func (s *AdminTenantService) ResetTenantPublisherCredential(ctx context.Context, r *imagev1.ResetTenantPublisherCredentialRequest) (*imagev1.ResetTenantPublisherCredentialResponse, error) {
	expected, err := requiredVersion(r.ExpectedVersion)
	if err != nil {
		return nil, rpcError(err)
	}
	ctx, err = s.withManagedTenant(ctx, r.GetTargetTenantId())
	if err != nil {
		return nil, rpcError(err)
	}
	v, err := s.tenant.ResetPublisherCredential(ctx, &imagev1.ResetPublisherCredentialRequest{TenantId: r.GetTargetTenantId(), IdempotencyKey: r.GetIdempotencyKey(), ExpectedVersion: expected})
	if err != nil {
		return nil, err
	}
	return &imagev1.ResetTenantPublisherCredentialResponse{Credential: v.GetCredential(), Secret: v.GetSecret(), ReplayUntil: v.GetReplayUntil()}, nil
}
func (s *AdminTenantService) DisableTenantPublisherCredential(ctx context.Context, r *imagev1.DisableTenantPublisherCredentialRequest) (*imagev1.DisableTenantPublisherCredentialResponse, error) {
	expected, err := requiredVersion(r.ExpectedVersion)
	if err != nil {
		return nil, rpcError(err)
	}
	ctx, err = s.withManagedTenant(ctx, r.GetTargetTenantId())
	if err != nil {
		return nil, rpcError(err)
	}
	v, err := s.tenant.DisablePublisherCredential(ctx, &imagev1.DisablePublisherCredentialRequest{TenantId: r.GetTargetTenantId(), IdempotencyKey: r.GetIdempotencyKey(), ExpectedVersion: expected})
	if err != nil {
		return nil, err
	}
	return &imagev1.DisableTenantPublisherCredentialResponse{Credential: v.GetCredential()}, nil
}
func (s *AdminTenantService) ListTenantImages(ctx context.Context, r *imagev1.ListTenantImagesRequest) (*imagev1.ListTenantImagesResponse, error) {
	ctx, err := s.withManagedTenant(ctx, r.GetTargetTenantId())
	if err != nil {
		return nil, rpcError(err)
	}
	f, p, err := readManagementList(r.GetSearch(), r.GetPurposes(), r.GetAccelerator(), r.Page, r.PageSize)
	if err != nil {
		return nil, rpcError(err)
	}
	v, err := s.catalog.ListTenantImagesPage(ctx, r.GetTargetTenantId(), f, p)
	if err != nil {
		return nil, rpcError(err)
	}
	out := &imagev1.ManagementImagePage{Total: v.Total, Items: make([]*imagev1.ImageRegistration, 0, len(v.Items))}
	for _, item := range v.Items {
		out.Items = append(out.Items, wireRegistration(item))
	}
	return &imagev1.ListTenantImagesResponse{Page: out}, nil
}
func (s *AdminTenantService) GetTenantImage(ctx context.Context, r *imagev1.GetTenantImageRequest) (*imagev1.GetTenantImageResponse, error) {
	ctx, err := s.withManagedTenant(ctx, r.GetTargetTenantId())
	if err != nil {
		return nil, rpcError(err)
	}
	v, err := s.tenant.GetImage(ctx, &imagev1.GetImageRequest{TenantId: r.GetTargetTenantId(), ImageId: r.GetImageId(), Scope: imagev1.ImageScope_IMAGE_SCOPE_TENANT})
	if err != nil {
		return nil, err
	}
	return &imagev1.GetTenantImageResponse{Image: v.GetImage()}, nil
}
func (s *AdminTenantService) RegisterTenantImage(ctx context.Context, r *imagev1.RegisterTenantImageRequest) (*imagev1.RegisterTenantImageResponse, error) {
	ctx, err := s.withManagedTenant(ctx, r.GetTargetTenantId())
	if err != nil {
		return nil, rpcError(err)
	}
	v, err := s.tenant.RegisterImage(ctx, &imagev1.RegisterImageRequest{TenantId: r.GetTargetTenantId(), ImageReference: r.GetImageReference(), DisplayName: r.GetDisplayName(), Description: r.GetDescription(), Purposes: r.GetPurposes(), Accelerator: r.GetAccelerator(), IdempotencyKey: r.GetIdempotencyKey()})
	if err != nil {
		return nil, err
	}
	return &imagev1.RegisterTenantImageResponse{Image: v.GetImage()}, nil
}
func (s *AdminTenantService) UpdateTenantImage(ctx context.Context, r *imagev1.UpdateTenantImageRequest) (*imagev1.UpdateTenantImageResponse, error) {
	in, err := readPatch(r.GetImageId(), r.GetTargetTenantId(), r.DisplayName, r.Description, r.GetPurposes(), r.Accelerator, r.ExpectedVersion, r.GetIdempotencyKey(), r.GetUpdateMask())
	if err != nil {
		return nil, rpcError(err)
	}
	ctx, err = s.withManagedTenant(ctx, r.GetTargetTenantId())
	if err != nil {
		return nil, rpcError(err)
	}
	v, err := s.catalog.PatchTenantImage(ctx, in)
	if err != nil {
		return nil, rpcError(err)
	}
	return &imagev1.UpdateTenantImageResponse{Image: wireRegistration(v)}, nil
}
func (s *AdminTenantService) UnregisterTenantImage(ctx context.Context, r *imagev1.UnregisterTenantImageRequest) (*imagev1.UnregisterTenantImageResponse, error) {
	expected, err := requiredVersion(r.ExpectedVersion)
	if err != nil {
		return nil, rpcError(err)
	}
	ctx, err = s.withManagedTenant(ctx, r.GetTargetTenantId())
	if err != nil {
		return nil, rpcError(err)
	}
	v, err := s.tenant.UnregisterImage(ctx, &imagev1.UnregisterImageRequest{TenantId: r.GetTargetTenantId(), ImageId: r.GetImageId(), ExpectedVersion: expected, IdempotencyKey: r.GetIdempotencyKey()})
	if err != nil {
		return nil, err
	}
	return &imagev1.UnregisterTenantImageResponse{Image: v.GetImage()}, nil
}
