package clusterops

import (
	"context"
	"errors"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/clusterops"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var errorCodes = map[string]codes.Code{
	"INVALID_ARGUMENT": codes.InvalidArgument, "SYSTEM_FIELD_READ_ONLY": codes.InvalidArgument,
	"PLATFORM_SCOPE_REQUIRED": codes.PermissionDenied, "CLUSTER_NOT_CONFIGURED": codes.NotFound,
	"RESOURCE_TYPE_UNAVAILABLE": codes.FailedPrecondition, "RESOURCE_READ_ONLY": codes.PermissionDenied,
	"TENANT_NAMESPACE_READ_ONLY": codes.PermissionDenied, "NAMESPACE_POLICY_UNAVAILABLE": codes.Unavailable,
	"RESOURCE_NOT_FOUND": codes.NotFound, "RESOURCE_ALREADY_EXISTS": codes.AlreadyExists,
	"RESOURCE_UID_CHANGED": codes.Aborted, "RESOURCE_VERSION_CONFLICT": codes.Aborted,
	"ACTION_NOT_SUPPORTED": codes.FailedPrecondition, "RESTART_STRATEGY_UNSUPPORTED": codes.FailedPrecondition,
	"CRD_HAS_PROTECTED_NAMESPACE_INSTANCES": codes.FailedPrecondition, "CRD_INSPECTION_FAILED": codes.FailedPrecondition,
	"LIST_EXPIRED": codes.Aborted, "KUBERNETES_FORBIDDEN": codes.PermissionDenied, "KUBERNETES_UNAUTHORIZED": codes.Unauthenticated,
	"KUBERNETES_INVALID": codes.InvalidArgument, "KUBERNETES_UNAVAILABLE": codes.Unavailable, "RESOURCE_EXHAUSTED": codes.ResourceExhausted,
	"POD_CHANGED": codes.Aborted, "POD_CONTAINER_REQUIRED": codes.InvalidArgument, "LOG_READ_FAILED": codes.Unavailable,
	"LOG_SEND_TIMEOUT": codes.DeadlineExceeded, "INTERNAL_ERROR": codes.Internal,
}

func serviceError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return status.Error(codes.Canceled, "request canceled")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return status.Error(codes.DeadlineExceeded, "request deadline exceeded")
	}
	var e *biz.Error
	if !errors.As(err, &e) {
		if _, ok := status.FromError(err); ok {
			return err
		}
		e = biz.NewError("INTERNAL_ERROR")
	}
	code, ok := errorCodes[e.Reason]
	if !ok {
		e = biz.NewError("INTERNAL_ERROR")
		code = codes.Internal
	}
	s := status.New(code, e.Reason)
	detailed, detailErr := s.WithDetails(&errdetails.ErrorInfo{Reason: e.Reason, Domain: "clusterops.ani.io", Metadata: e.Metadata})
	if detailErr != nil {
		return s.Err()
	}
	return detailed.Err()
}
