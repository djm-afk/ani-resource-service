package service

import (
	"context"
	"errors"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/image"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func rpcError(err error) error {
	if err == nil {
		return nil
	}
	reason := biz.ReasonOf(err)
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		reason = biz.DeadlineExceeded
	}
	code := codes.Internal
	switch reason {
	case biz.InvalidArgument, biz.InvalidReference, biz.InvalidCursor:
		code = codes.InvalidArgument
	case biz.TrustedCallerRequired:
		code = codes.Unauthenticated
	case biz.PermissionDenied, biz.ImageProjectDenied:
		code = codes.PermissionDenied
	case biz.ImageNotFound, biz.SpaceNotFound:
		code = codes.NotFound
	case biz.ImageAlreadyRegistered, biz.SpaceNameConflict, biz.IdempotencyConflict:
		code = codes.AlreadyExists
	case biz.VersionConflict:
		code = codes.Aborted
	case biz.SpaceNameImmutable, biz.SpaceNotReady, biz.CredentialAlreadyActive, biz.CredentialNotIssued, biz.CredentialDeliveryExpired, biz.UnsupportedArtifact, biz.PlatformMismatch, biz.SpaceOwnershipUnconfirmed:
		code = codes.FailedPrecondition
	case biz.ImageSpaceBindingMismatch, biz.CandidateRecoveryCompleted:
		// 两者都是“状态不允许本次写入”，调用方需要 409 而不是 500 或 503。
		code = codes.FailedPrecondition
	case biz.DependencyUnavailable, biz.RequestInProgress:
		code = codes.Unavailable
	case biz.DeadlineExceeded:
		code = codes.DeadlineExceeded
	}
	// Only the bounded reason crosses the transport. An unexpected adapter error
	// can contain a DSN, URL or response body and must never become a status message.
	s := status.New(code, string(reason))
	detailed, e := s.WithDetails(&errdetails.ErrorInfo{Domain: "image.ani.io", Reason: string(reason)})
	if e != nil {
		return s.Err()
	}
	return detailed.Err()
}
