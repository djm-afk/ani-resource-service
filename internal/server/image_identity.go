package server

import (
	"context"

	imagev1 "github.com/zhangzhe-ctrl/ani-resource-service/api/image/v1"
	imagebiz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/image"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// imageGovernanceMethods is the tenant allowlist; it is unchanged by the
// management surfaces and still requires the governance tenant identity.
var imageGovernanceMethods = map[string]struct{}{
	imagev1.TenantImageService_EnsureImageSpace_FullMethodName:           {},
	imagev1.TenantImageService_GetImageSpace_FullMethodName:              {},
	imagev1.TenantImageService_GetPublisherCredential_FullMethodName:     {},
	imagev1.TenantImageService_IssuePublisherCredential_FullMethodName:   {},
	imagev1.TenantImageService_ResetPublisherCredential_FullMethodName:   {},
	imagev1.TenantImageService_DisablePublisherCredential_FullMethodName: {},
	imagev1.TenantImageService_RegisterImage_FullMethodName:              {},
	imagev1.TenantImageService_GetImage_FullMethodName:                   {},
	imagev1.TenantImageService_ListImages_FullMethodName:                 {},
	imagev1.TenantImageService_UpdateImage_FullMethodName:                {},
	imagev1.TenantImageService_UnregisterImage_FullMethodName:            {},
}

// imagePlatformGovernanceMethods is the platform allowlist. These methods accept
// only the platform scope of the governance listener and are never reachable
// with a tenant identity.
var imagePlatformGovernanceMethods = map[string]struct{}{
	imagev1.PlatformImageService_GetPlatformSpace_FullMethodName:                    {},
	imagev1.PlatformImageService_InitializePlatform_FullMethodName:                  {},
	imagev1.PlatformImageService_GetPlatformPublisherCredential_FullMethodName:      {},
	imagev1.PlatformImageService_IssuePlatformPublisherCredential_FullMethodName:    {},
	imagev1.PlatformImageService_ResetPlatformPublisherCredential_FullMethodName:    {},
	imagev1.PlatformImageService_DisablePlatformPublisherCredential_FullMethodName:  {},
	imagev1.PlatformImageService_ListPlatformImages_FullMethodName:                  {},
	imagev1.PlatformImageService_GetPlatformImage_FullMethodName:                    {},
	imagev1.PlatformImageService_RegisterPlatformImage_FullMethodName:               {},
	imagev1.PlatformImageService_UpdatePlatformImage_FullMethodName:                 {},
	imagev1.PlatformImageService_UnregisterPlatformImage_FullMethodName:             {},
	imagev1.AdminTenantImageService_GetTenantImageSpace_FullMethodName:              {},
	imagev1.AdminTenantImageService_EnsureTenantImageSpace_FullMethodName:           {},
	imagev1.AdminTenantImageService_GetTenantPublisherCredential_FullMethodName:     {},
	imagev1.AdminTenantImageService_IssueTenantPublisherCredential_FullMethodName:   {},
	imagev1.AdminTenantImageService_ResetTenantPublisherCredential_FullMethodName:   {},
	imagev1.AdminTenantImageService_DisableTenantPublisherCredential_FullMethodName: {},
	imagev1.AdminTenantImageService_ListTenantImages_FullMethodName:                 {},
	imagev1.AdminTenantImageService_GetTenantImage_FullMethodName:                   {},
	imagev1.AdminTenantImageService_RegisterTenantImage_FullMethodName:              {},
	imagev1.AdminTenantImageService_UpdateTenantImage_FullMethodName:                {},
	imagev1.AdminTenantImageService_UnregisterTenantImage_FullMethodName:            {},
}

func withImageGovernanceCaller(ctx context.Context, p governancePrincipal, request proto.Message) (context.Context, error) {
	value := request.ProtoReflect()
	if !value.IsValid() {
		return nil, status.Error(codes.PermissionDenied, "image request required")
	}
	fd := value.Descriptor().Fields().ByName("tenant_id")
	if fd == nil || value.Get(fd).String() != p.TenantID {
		return nil, status.Error(codes.PermissionDenied, "image request tenant differs from authenticated scope")
	}
	return imagebiz.WithCaller(ctx, imagebiz.Caller{Kind: imagebiz.GovernanceCaller, Subject: p.Workload, Actor: p.Actor, TenantID: p.TenantID}), nil
}
