package biz

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/zhangzhe-ctrl/ani-resource-service/internal/telemetry"
)

func TestStartNetworkRunResumesOnlyActiveOperation(t *testing.T) {
	otel.SetTextMapPropagator(propagation.TraceContext{})
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider())
	t.Cleanup(func() { otel.SetTracerProvider(previous) })

	traceID, _ := trace.TraceIDFromHex("0af7651916cd43dd8448eb211c80319c")
	spanID, _ := trace.SpanIDFromHex("b7ad6b7169203331")
	saved := telemetry.TelemetryContext{
		Traceparent:   "00-" + traceID.String() + "-" + spanID.String() + "-01",
		OriginTraceID: traceID.String(),
	}

	active := Work{ActiveOperation: true, Operation: Operation{TelemetryContext: saved}}
	ctx, span, event, err := startNetworkRun(context.Background(), active, "network.vpc")
	if err != nil || event != "" {
		t.Fatalf("active resume = event %q err %v", event, err)
	}
	sc := trace.SpanContextFromContext(ctx)
	if sc.TraceID() != traceID {
		t.Errorf("active run trace = %s, want %s", sc.TraceID(), traceID)
	}
	if sc.SpanID() == spanID {
		t.Error("active run reused the accepting span instead of opening its own")
	}
	span.End()

	terminal := Work{ActiveOperation: false, Operation: Operation{TelemetryContext: saved}}
	ctx, span, event, err = startNetworkRun(context.Background(), terminal, "network.vpc")
	if err != nil || event != "" {
		t.Fatalf("terminal start = event %q err %v", event, err)
	}
	sc = trace.SpanContextFromContext(ctx)
	if !sc.IsValid() || sc.TraceID() == traceID {
		t.Errorf("terminal observation inherited the finished operation trace: %s", sc.TraceID())
	}
	span.End()
}
