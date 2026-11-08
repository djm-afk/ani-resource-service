package clusterops

import (
	"context"
	"errors"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/clusterops"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"regexp"
	"strconv"
	"strings"
)

var safeField = regexp.MustCompile("^[a-zA-Z0-9_.\\[\\]-]+$")

func kubeError(err error, deleting bool) error {
	if err == nil {
		return nil
	}
	reason := "KUBERNETES_UNAVAILABLE"
	switch {
	case apierrors.IsNotFound(err):
		reason = "RESOURCE_NOT_FOUND"
	case apierrors.IsAlreadyExists(err):
		reason = "RESOURCE_ALREADY_EXISTS"
	case apierrors.IsConflict(err):
		if deleting {
			reason = "RESOURCE_UID_CHANGED"
		} else {
			reason = "RESOURCE_VERSION_CONFLICT"
		}
	case apierrors.IsResourceExpired(err) || apierrors.IsGone(err):
		reason = "LIST_EXPIRED"
	case apierrors.IsForbidden(err):
		reason = "KUBERNETES_FORBIDDEN"
	case apierrors.IsUnauthorized(err):
		reason = "KUBERNETES_UNAUTHORIZED"
	case apierrors.IsInvalid(err) || apierrors.IsBadRequest(err) || apierrors.IsMethodNotSupported(err):
		reason = "KUBERNETES_INVALID"
	case apierrors.IsTooManyRequests(err):
		reason = "RESOURCE_EXHAUSTED"
	}
	e := biz.NewError(reason)
	var s apierrors.APIStatus
	if errors.As(err, &s) {
		st := s.Status()
		e.Metadata = map[string]string{"kubernetes_http_status": strconv.Itoa(int(st.Code))}
		// Reasons are protocol identifiers; Status.Message and Cause.Message may
		// contain submitted values and must never cross this boundary.
		switch string(st.Reason) {
		case "NotFound", "AlreadyExists", "Conflict", "Expired", "Gone", "Forbidden", "Unauthorized", "Invalid", "BadRequest", "MethodNotAllowed", "TooManyRequests", "Timeout", "ServerTimeout", "InternalError", "ServiceUnavailable":
			e.Metadata["kubernetes_reason"] = string(st.Reason)
		}
		if st.Details != nil {
			fields := []string{}
			for _, c := range st.Details.Causes {
				if safeField.MatchString(c.Field) && len(c.Field) <= 200 {
					fields = append(fields, c.Field)
				}
			}
			if len(fields) > 0 {
				e.Metadata["field_paths"] = strings.Join(fields, ",")
			}
		}
	}
	return e
}
func apiCall[T any](ctx context.Context, fn func(context.Context) (T, error)) (T, error) {
	var zero T
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	sub, cancel := context.WithTimeout(ctx, biz.KubernetesTimeout)
	defer cancel()
	out, err := fn(sub)
	if ctx.Err() != nil {
		return zero, ctx.Err()
	}
	if err != nil {
		return zero, kubeError(err, false)
	}
	return out, nil
}
