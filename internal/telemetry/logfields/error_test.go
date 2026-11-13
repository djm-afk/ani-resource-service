package logfields

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func fields(err error) map[string]any {
	out := make(map[string]any)
	kv := ErrorFields(err)
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i].(string)] = kv[i+1]
	}
	return out
}

func init() { otel.SetTextMapPropagator(propagation.TraceContext{}) }

func TestErrorFieldsKubernetesForbidden(t *testing.T) {
	err := apierrors.NewForbidden(schema.GroupResource{Resource: "persistentvolumeclaims"}, "src-pvc", errors.New(`User "system:serviceaccount:ani:res" cannot get resource "persistentvolumeclaims"`))
	f := fields(err)
	if f["error_source"] != "kubernetes" || f["error_code"] != "Forbidden" {
		t.Errorf("classification = %v/%v", f["error_source"], f["error_code"])
	}
	// The numeric HTTP status is a separate contract field and must be present.
	if f["http.status_code"] != 403 {
		t.Errorf("missing numeric http.status_code: %v", f["http.status_code"])
	}
	if !strings.Contains(f["error"].(string), "cannot get resource") {
		t.Errorf("actionable message lost: %v", f["error"])
	}
}

// TestErrorFieldsKeepWrappedCause is NG17: a ProviderError wrapper renders only a
// fixed kind, so the driver/API message must come from the unwrapped cause while
// error_code/error_type still describe the typed status.
func TestErrorFieldsKeepWrappedCause(t *testing.T) {
	inner := apierrors.NewForbidden(schema.GroupResource{Resource: "vpcs"}, "vpc-a", errors.New(`User "system:serviceaccount:ani:res" cannot create resource "vpcs" in namespace "tenant-1"`))
	wrapped := fmt.Errorf("network provider: %s: %w", "temporary", inner)
	f := fields(wrapped)
	message, _ := f["error"].(string)
	if strings.Contains(message, "network provider") == false {
		t.Errorf("outer action should stay readable: %v", message)
	}
	if !strings.Contains(message, "cannot create resource") {
		t.Errorf("unwrapped cause lost: %v", message)
	}
	if f["error_source"] != "kubernetes" || f["error_code"] != "Forbidden" || f["http.status_code"] != 403 {
		t.Errorf("typed classification lost after unwrap: %v", f)
	}
}

func TestErrorFieldsPostgresAndGRPC(t *testing.T) {
	pg := fields(fmt.Errorf("save: %w", &pgconn.PgError{Code: "42501", Message: "permission denied for table vm_operations"}))
	if pg["error_source"] != "postgresql" || pg["error_code"] != "42501" {
		t.Errorf("pg classification = %v", pg)
	}
	gr := fields(status.Error(codes.Unavailable, "connection refused"))
	if gr["error_source"] != "grpc" || gr["error_code"] != "Unavailable" {
		t.Errorf("grpc classification = %v", gr)
	}
}

func TestRedactKeepsSigV4OutAndReasonIn(t *testing.T) {
	raw := "GET https://s3/obj?X-Amz-Signature=deadbeef&X-Amz-Credential=AKIA/2026 failed state=S3_ACCESS_DENIED password=hunter2"
	got := Redact(raw)
	for _, secret := range []string{"deadbeef", "hunter2", "AKIA/2026"} {
		if strings.Contains(got, secret) {
			t.Errorf("secret %q leaked: %s", secret, got)
		}
	}
	for _, keep := range []string{"S3_ACCESS_DENIED", "state="} {
		if !strings.Contains(got, keep) {
			t.Errorf("lost actionable %q: %s", keep, got)
		}
	}
}

func TestTruncation(t *testing.T) {
	f := fields(errors.New(strings.Repeat("y", 40*1024)))
	if f["error_truncated"] != true || f["error_original_bytes"].(int) != 40*1024 {
		t.Errorf("truncation metadata wrong: %v", f)
	}
}

// TestRedactAuthorizationHeader covers review finding P1: the whole header value
// is a credential, not just its scheme token. Only the scheme word must survive
// so the log stays readable as "an Authorization header was present".
func TestRedactAuthorizationHeader(t *testing.T) {
	cases := []struct{ in, secret string }{
		{`Authorization: Basic dXNlcjpwYXNzd29yZA==`, `dXNlcjpwYXNzd29yZA==`},
		{`authorization=Basic YWJjOmRlZg==`, `YWJjOmRlZg==`},
		{`Authorization: Bearer abcdefghijklmnop`, `abcdefghijklmnop`},
		{`Authorization: Digest username="u", response="deadbeef"`, `deadbeef`},
		{`Authorization: dXNlcjpwYXNz`, `dXNlcjpwYXNz`},
	}
	for _, tc := range cases {
		out := Redact(tc.in)
		if strings.Contains(out, tc.secret) {
			t.Errorf("credential leaked from %q: %s", tc.in, out)
		}
		if !strings.Contains(strings.ToLower(out), "authorization") {
			t.Errorf("header name should stay readable: %s", out)
		}
	}
}

// TestWrappedErrorRedactsAuthorization proves the shared extractor scrubs the
// marker even when the credential sits inside a wrapped cause, which is the
// path review finding P1 described.
func TestWrappedErrorRedactsAuthorization(t *testing.T) {
	const marker = "c3VwZXItc2VjcmV0LW1hcmtlcg=="
	inner := fmt.Errorf(`api call failed: Authorization: Basic %s`, marker)
	wrapped := fmt.Errorf("network provider: temporary: %w", inner)
	f := fields(wrapped)
	msg, _ := f["error"].(string)
	if strings.Contains(msg, marker) {
		t.Fatalf("wrapped credential leaked: %s", msg)
	}
	if !strings.Contains(msg, "api call failed") {
		t.Errorf("outer action lost while redacting: %s", msg)
	}
}

// TestTruncatedErrorStaysWithinCap is NG28: the marker counts against the
// 32 KiB cap, so the emitted string must not exceed it.
func TestTruncatedErrorStaysWithinCap(t *testing.T) {
	for _, n := range []int{33 * 1024, 40 * 1024, 100 * 1024} {
		f := fields(errors.New(strings.Repeat("a", n)))
		msg := f["error"].(string)
		if len(msg) > 32*1024 {
			t.Errorf("n=%d: emitted %d bytes, exceeds 32768", n, len(msg))
		}
		if f["error_truncated"] != true || f["error_original_bytes"].(int) != n {
			t.Errorf("n=%d: truncation metadata wrong", n)
		}
		if !utf8.ValidString(msg) {
			t.Errorf("n=%d: truncated output is not valid UTF-8", n)
		}
	}
}

// TestTruncationKeepsUTF8Boundary checks the head/tail cut never splits a rune
// once the marker takes part of the budget.
func TestTruncationKeepsUTF8Boundary(t *testing.T) {
	f := fields(errors.New(strings.Repeat("错", 20000)))
	msg := f["error"].(string)
	if !utf8.ValidString(msg) {
		t.Fatal("truncated multi-byte output is not valid UTF-8")
	}
	if len(msg) > 32*1024 {
		t.Fatalf("emitted %d bytes, exceeds 32768", len(msg))
	}
}
