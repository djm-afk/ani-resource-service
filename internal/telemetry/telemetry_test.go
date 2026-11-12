package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdkTrace "go.opentelemetry.io/otel/sdk/trace"
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

// A worker pass must keep the accepted request's trace_id but report its own
// span_id: the persisted traceparent belongs to a request span that already
// ended. This is the LG13/LG14 requirement, asserted at Resume.
func TestResumeKeepsTraceAndOpensRunSpan(t *testing.T) {
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(sdkTrace.NewTracerProvider())
	defer otel.SetTracerProvider(previous)

	traceID, _ := trace.TraceIDFromHex("0af7651916cd43dd8448eb211c80319c")
	requestSpanID, _ := trace.SpanIDFromHex("b7ad6b7169203331")
	request := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: traceID, SpanID: requestSpanID, TraceFlags: trace.FlagsSampled,
	})
	saved := Capture(trace.ContextWithSpanContext(context.Background(), request))

	runCtx, span, event, err := Resume(context.Background(), saved, nil, "vm.operation.create")
	defer span.End()
	if event != "" || err != nil {
		t.Fatalf("usable metadata must not report a context event, got %q %v", event, err)
	}
	got := trace.SpanContextFromContext(runCtx)
	if got.TraceID() != traceID {
		t.Errorf("worker trace_id = %s, want the accepted trace %s", got.TraceID(), traceID)
	}
	if got.SpanID() == requestSpanID || !got.SpanID().IsValid() {
		t.Errorf("worker span_id = %s, want a new valid span distinct from the request span", got.SpanID())
	}
}

// Missing or malformed metadata is reported, but the run still gets a valid
// span so the required ERROR carries trace_id/span_id.
func TestResumeWithoutUsableContextStillHasRunSpan(t *testing.T) {
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(sdkTrace.NewTracerProvider())
	defer otel.SetTracerProvider(previous)

	runCtx, span, event, cause := Resume(context.Background(), TelemetryContext{}, nil, "vm.operation.create")
	span.End()
	if event != "telemetry.context_missing" || cause != nil {
		t.Fatalf("empty metadata = (%q, %v), want context_missing without a cause", event, cause)
	}
	if sc := trace.SpanContextFromContext(runCtx); !sc.IsValid() {
		t.Fatal("run span must be valid even when the persisted trace is unusable")
	}

	bad := errors.New("decode telemetry context: bad json")
	runCtx2, span2, event2, cause2 := Resume(context.Background(), TelemetryContext{}, bad, "vm.operation.create")
	span2.End()
	if event2 != "telemetry.context_invalid" || !errors.Is(cause2, bad) {
		t.Fatalf("malformed metadata = (%q, %v), want context_invalid carrying the real cause", event2, cause2)
	}
	if sc := trace.SpanContextFromContext(runCtx2); !sc.IsValid() {
		t.Fatal("run span must be valid when metadata is malformed")
	}
}
