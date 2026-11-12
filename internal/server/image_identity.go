package server

import (
	"context"

	imagev1 "github.com/zhangzhe-ctrl/ani-resource-service/api/image/v1"
	imagebiz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/image"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

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
