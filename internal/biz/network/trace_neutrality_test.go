package biz

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/zhangzhe-ctrl/ani-resource-service/internal/telemetry"
)

func initTraceForTest(t *testing.T) {
	t.Helper()
	otel.SetTextMapPropagator(propagation.TraceContext{})
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider())
	t.Cleanup(func() { otel.SetTracerProvider(previous) })
}

// changeTrace injects a different active trace and returns a context carrying it.
func changeTrace(t *testing.T, traceHex, spanHex string) context.Context {
	t.Helper()
	traceID, err := trace.TraceIDFromHex(traceHex)
	if err != nil {
		t.Fatal(err)
	}
	spanID, err := trace.SpanIDFromHex(spanHex)
	if err != nil {
		t.Fatal(err)
	}
	saved := telemetry.TelemetryContext{Traceparent: "00-" + traceID.String() + "-" + spanID.String() + "-01"}
	ctx, err := telemetry.Restore(context.Background(), saved)
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

// TestTraceDoesNotAffectBusinessIdentity is NG29: changing the trace context must
// leave every idempotency fingerprint and resource version untouched.
func TestTraceDoesNotAffectBusinessIdentity(t *testing.T) {
	initTraceForTest(t)

	vpc := VPCIntent{TenantID: "tenant-1", Name: "vpc-a", CIDR: "10.0.0.0/16", IdempotencyKey: "key-1"}
	subnet := SubnetIntent{VPCIntent: VPCIntent{TenantID: "tenant-1", Name: "subnet-a", CIDR: "10.0.0.0/24", IdempotencyKey: "key-2"}, VPCID: "vpc_1"}
	platform := PlatformIntent{Kind: "create_public_pool", Name: "pool-a", IdempotencyKey: "key-3"}

	base := []string{vpc.Fingerprint(), subnet.Fingerprint(), platform.VersionedFingerprint()}

	ctx := changeTrace(t, "0af7651916cd43dd8448eb211c80319c", "b7ad6b7169203331")
	// Bind the business identity as the admission path does, then confirm the
	// persisted snapshot never carries telemetry.
	bound := telemetry.WithBusiness(ctx, telemetry.BusinessContext{OperationID: "op-1", ResourceID: "vpc_1"})
	other := changeTrace(t, "1bf8651916cd43dd8448eb211c80319d", "c7ad6b7169203332")
	_ = telemetry.WithBusiness(other, telemetry.BusinessContext{OperationID: "op-2", ResourceID: "vpc_2"})

	after := []string{vpc.Fingerprint(), subnet.Fingerprint(), platform.VersionedFingerprint()}
	for i := range base {
		if base[i] != after[i] {
			t.Errorf("fingerprint %d changed with trace: %q -> %q", i, base[i], after[i])
		}
	}

	// Each trace snapshot is captured, but the business JSON must not include it.
	if telemetry.Capture(bound).OriginTraceID == "" {
		t.Error("capture lost the origin trace")
	}
	value := VPC{ID: "vpc_1", TenantID: "tenant-1", Version: 7, State: Available}
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	for _, leaked := range []string{"traceparent", "Traceparent", "origin_trace_id", "OriginTraceID"} {
		if containsFold(string(body), leaked) {
			t.Errorf("resource snapshot leaked telemetry field %q: %s", leaked, body)
		}
	}
	if value.Version != 7 {
		t.Errorf("version changed: %d", value.Version)
	}
}

func containsFold(haystack, needle string) bool {
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}
