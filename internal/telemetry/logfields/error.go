package logfields

import (
	"context"
	kerrors "errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/grpc/status"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

const (
	errorOutputCap   = 32 * 1024
	errorHeadBytes   = 24 * 1024
	errorTailBytes   = 8 * 1024
	errorTruncatedOn = "[truncated]"
)

// ErrorFields extracts actionable fields from the real cause of err. It never
// rewrites the business error: only this log copy is redacted and length-capped.
func ErrorFields(err error) []any {
	if err == nil {
		return nil
	}
	message, truncated, originalBytes := safeErrorMessage(err)
	out := []any{
		"error", message,
		"error_type", errorType(err),
		"error_source", errorSource(err),
	}
	if code := errorCode(err); code != "" {
		out = append(out, "error_code", code)
	}
	if truncated {
		out = append(out, "error_truncated", true, "error_original_bytes", originalBytes)
	}
	return out
}

// ErrorAttrs renders ErrorFields as slog attributes.
func ErrorAttrs(err error) []slog.Attr {
	kv := ErrorFields(err)
	if len(kv) == 0 {
		return nil
	}
	out := make([]slog.Attr, 0, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		key, _ := kv[i].(string)
		switch v := kv[i+1].(type) {
		case string:
			out = append(out, slog.String(key, v))
		case bool:
			out = append(out, slog.Bool(key, v))
		case int:
			out = append(out, slog.Int(key, v))
		}
	}
	return out
}

func errorType(err error) string {
	var (
		k8sErr *apierrors.StatusError
		pgErr  *pgconn.PgError
		netErr net.Error
		urlErr *url.Error
	)
	switch {
	case kerrors.As(err, &k8sErr):
		return "StatusError"
	case kerrors.As(err, &pgErr):
		return "PgError"
	case isGRPCStatus(err):
		return "grpc_status"
	case kerrors.As(err, &urlErr):
		return "url.Error"
	case kerrors.As(err, &netErr):
		return "net.Error"
	}
	return typeName(err)
}

func errorSource(err error) string {
	var (
		k8sErr *apierrors.StatusError
		pgErr  *pgconn.PgError
		urlErr *url.Error
	)
	switch {
	case kerrors.As(err, &k8sErr):
		return "kubernetes"
	case kerrors.As(err, &pgErr):
		return "postgresql"
	case kerrors.As(err, &urlErr):
		return "http"
	}
	if isGRPCStatus(err) {
		return "grpc"
	}
	return "local"
}

func errorCode(err error) string {
	var (
		k8sErr *apierrors.StatusError
		pgErr  *pgconn.PgError
	)
	switch {
	case kerrors.As(err, &k8sErr):
		if reason := string(k8sErr.ErrStatus.Reason); reason != "" {
			return reason
		}
		if k8sErr.ErrStatus.Code != 0 {
			return httpStatusCodeName(int(k8sErr.ErrStatus.Code))
		}
	case kerrors.As(err, &pgErr):
		return pgErr.Code
	}
	if st, ok := status.FromError(err); ok && st.Code().String() != "OK" {
		return st.Code().String()
	}
	return ""
}

func isGRPCStatus(err error) bool {
	st, ok := status.FromError(err)
	return ok && st.Code().String() != "OK"
}

func safeErrorMessage(err error) (string, bool, int) {
	raw := Redact(err.Error())
	size := len(raw)
	if size <= errorOutputCap {
		return raw, false, size
	}
	head := trimToRuneBoundary(raw[:errorHeadBytes], false)
	tail := trimToRuneBoundary(raw[size-errorTailBytes:], true)
	return head + errorTruncatedOn + tail, true, size
}

func trimToRuneBoundary(s string, fromStart bool) string {
	if utf8.ValidString(s) {
		return s
	}
	if fromStart {
		for len(s) > 0 {
			r, size := utf8.DecodeRuneInString(s)
			if r != utf8.RuneError || size > 1 {
				return s
			}
			s = s[size:]
		}
		return s
	}
	for len(s) > 0 {
		r, size := utf8.DecodeLastRuneInString(s)
		if r != utf8.RuneError || size > 1 {
			return s
		}
		s = s[:len(s)-size]
	}
	return s
}

var (
	credentialPair = regexp.MustCompile(`(?i)\b(password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|secret[_-]?key|private[_-]?key|authorization|cookie|credential|client[_-]?secret)\b(\s*[=:]\s*)("[^"]*"|'[^']*'|[^\s,;&}\]"']+)`)
	urlUserinfo    = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^/@\s]+@`)
	bearerToken    = regexp.MustCompile(`(?i)\b(bearer)\s+[A-Za-z0-9._~+/=-]{8,}`)
	pemKey         = regexp.MustCompile(`(?s)-----BEGIN [^-]*PRIVATE KEY-----.*?-----END [^-]*PRIVATE KEY-----`)
	// AWS SigV4 presigned URL parameters.
	sigV4Pair = regexp.MustCompile(`(?i)\b(X-Amz-(Signature|Credential|Security-Token))=[^&\s]+`)
)

// Redact replaces only known credential shapes; call identity, resource
// coordinates, SQLSTATE and provider reason survive.
func Redact(s string) string {
	if s == "" {
		return s
	}
	s = pemKey.ReplaceAllString(s, "[redacted private key]")
	s = urlUserinfo.ReplaceAllString(s, "$1[redacted]@")
	s = bearerToken.ReplaceAllString(s, "$1 [redacted]")
	s = sigV4Pair.ReplaceAllString(s, "$1=[redacted]")
	s = credentialPair.ReplaceAllStringFunc(s, func(match string) string {
		loc := credentialPair.FindStringSubmatchIndex(match)
		if loc == nil {
			return match
		}
		key := match[loc[2]:loc[3]]
		sep := match[loc[4]:loc[5]]
		return key + sep + "[redacted]"
	})
	return s
}

func typeName(err error) string {
	if err == nil {
		return ""
	}
	name := strings.TrimPrefix(fmt.Sprintf("%T", err), "*")
	if idx := strings.LastIndex(name, "."); idx >= 0 {
		name = name[idx+1:]
	}
	if name == "" {
		return "error"
	}
	return name
}

// IsDeadlineOrCanceled reports whether the failure is the caller's own deadline
// or cancellation.
func IsDeadlineOrCanceled(err error) bool {
	return kerrors.Is(err, context.Canceled) || kerrors.Is(err, context.DeadlineExceeded)
}

func httpStatusCodeName(code int) string {
	switch code {
	case 400:
		return "BadRequest"
	case 401:
		return "Unauthorized"
	case 403:
		return "Forbidden"
	case 404:
		return "NotFound"
	case 405:
		return "MethodNotAllowed"
	case 409:
		return "Conflict"
	case 422:
		return "UnprocessableEntity"
	case 429:
		return "TooManyRequests"
	case 500:
		return "InternalError"
	case 503:
		return "ServiceUnavailable"
	case 504:
		return "Timeout"
	default:
		return ""
	}
}
