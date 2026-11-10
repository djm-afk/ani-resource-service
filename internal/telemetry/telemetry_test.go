package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func init() { otel.SetTextMapPropagator(propagation.TraceContext{}) }

func fields(err error) map[string]any {
	out := make(map[string]any)
	kv := ErrorFields(err)
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i].(string)] = kv[i+1]
	}
	return out
}

func TestErrorFieldsKubernetesForbidden(t *testing.T) {
	err := apierrors.NewForbidden(schema.GroupResource{Resource: "persistentvolumeclaims"}, "src-pvc", errors.New(`User "system:serviceaccount:ani:res" cannot get resource "persistentvolumeclaims"`))
	f := fields(err)
	if f["error_source"] != "kubernetes" || f["error_code"] != "Forbidden" {
		t.Errorf("classification = %v/%v", f["error_source"], f["error_code"])
	}
	if !strings.Contains(f["error"].(string), "cannot get resource") {
		t.Errorf("actionable message lost: %v", f["error"])
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

func TestCaptureRestoreRoundTrip(t *testing.T) {
	traceID, _ := trace.TraceIDFromHex("0af7651916cd43dd8448eb211c80319c")
	spanID, _ := trace.SpanIDFromHex("b7ad6b7169203331")
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)

	saved := Capture(ctx)
	if !saved.Valid() || saved.OriginTraceID != traceID.String() {
		t.Fatalf("capture = %+v", saved)
	}
	restored, err := Restore(context.Background(), saved)
	if err != nil {
		t.Fatal(err)
	}
	if got := trace.SpanContextFromContext(restored).TraceID().String(); got != traceID.String() {
		t.Errorf("restored trace = %s", got)
	}
}

func TestDecodeBadMetadataIsolated(t *testing.T) {
	if _, err := Decode(json.RawMessage(`{"traceparent": 123}`)); err == nil {
		t.Fatal("expected error for wrongly typed traceparent")
	}
	for _, raw := range []string{"", "null", "{}"} {
		got, err := Decode(json.RawMessage(raw))
		if err != nil || got.Valid() {
			t.Errorf("Decode(%q) = %v, %v", raw, got, err)
		}
	}
}

func TestTargetOmitUnknown(t *testing.T) {
	if got := (Target{}).KV(); got != nil {
		t.Errorf("empty target should render nothing: %v", got)
	}
	attrs := (Target{Kind: "Pod", Namespace: "tenant-a", Name: "p1"}).Attr()
	if len(attrs) != 1 || attrs[0].Key != "target" {
		t.Fatalf("target attr = %v", attrs)
	}
}
