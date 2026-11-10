package telemetry

import (
	"context"
	"encoding/json"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

func init() { otel.SetTextMapPropagator(propagation.TraceContext{}) }
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
