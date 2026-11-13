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
	// The numeric status is a separate contract field: a wrapped Kubernetes
	// StatusError keeps its Reason in error_code and carries the HTTP status here.
	var k8sErr *apierrors.StatusError
	if kerrors.As(err, &k8sErr) && k8sErr.ErrStatus.Code != 0 {
		out = append(out, "http.status_code", int(k8sErr.ErrStatus.Code))
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
	raw := Redact(unwrappedMessage(err))
	size := len(raw)
	if size <= errorOutputCap {
		return raw, false, size
	}
	// The marker counts against the cap: head+marker+tail must stay within
	// errorOutputCap, so the marker's length is taken out of the tail budget
	// before slicing. The tail keeps at least one byte so a marker alone is
	// never produced for a record that still had content to show.
	head := trimToRuneBoundary(raw[:errorHeadBytes], false)
	tailBudget := errorOutputCap - len(head) - len(errorTruncatedOn)
	if tailBudget > errorTailBytes {
		tailBudget = errorTailBytes
	}
	if tailBudget < 1 {
		tailBudget = 1
	}
	tail := trimToRuneBoundary(raw[size-tailBudget:], true)
	return head + errorTruncatedOn + tail, true, size
}

// unwrappedMessage joins the outer action with the innermost real cause.
// Wrapper types such as ProviderError and biz.Error only render a fixed kind or
// reason, so the driver/API message would otherwise be lost.
func unwrappedMessage(err error) string {
	parts := make([]string, 0, 4)
	for level := 0; err != nil && level < 8; level++ {
		text := strings.TrimSpace(err.Error())
		if text != "" && !messageCovered(parts, text) {
			parts = append(parts, text)
		}
		err = kerrors.Unwrap(err)
	}
	return strings.Join(parts, ": ")
}

// messageCovered reports whether text already appears in the joined chain, which
// prevents a wrapper that only repeats its child from duplicating the message.
func messageCovered(parts []string, text string) bool {
	for _, existing := range parts {
		if existing == text || strings.Contains(existing, text) {
			return true
		}
	}
	return false
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
	// A whole Authorization header carries the credential after its scheme
	// token: Basic/Bearer take one opaque value, Digest takes a parameter list.
	// The scheme word is kept so the log stays readable; everything after it is
	// replaced up to the value boundary. This must run before credentialPair,
	// which would otherwise redact only the scheme and leave the credential.
	// Quotes around the whole value are common in structured logs and config
	// dumps, so a quoted blob is matched before the scheme forms. Without that
	// branch the scheme token is followed by a quote, no alternative matches,
	// and the credential survives untouched.
	authHeader     = regexp.MustCompile(`(?i)\b(authorization)(\s*[=:]\s*)("[^"]*"|'[^']*'|(?:basic|bearer|negotiate|ntlm|apikey|token)\s+[^\s,;&}\]"']+|digest\s+[^\r\n]+|[^\s,;&}\]"']+)`)
	credentialPair = regexp.MustCompile(`(?i)\b(password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|secret[_-]?key|private[_-]?key|cookie|credential|client[_-]?secret)\b(\s*[=:]\s*)("[^"]*"|'[^']*'|[^\s,;&}\]"']+)`)
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
	s = authHeader.ReplaceAllString(s, "$1$2[redacted]")
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
