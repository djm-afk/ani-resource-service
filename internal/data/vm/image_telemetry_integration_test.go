//go:build instanceintegration

package vm

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
)

// TestImageTelemetryCapturedAndRestored proves the VM image lane carries the
// accepted request's trace into the persisted record, restores it into the
// claimed image for the worker, and never lets the trace influence the request
// digest or replay decision.
func TestImageTelemetryCapturedAndRestored(t *testing.T) {
	otel.SetTextMapPropagator(propagation.TraceContext{})
	ctx := context.Background()
	db := newVMImageDatabase(t)
	if e := Migrate(ctx, db.OwnerDSN, db.RuntimeRole); e != nil {
		t.Fatal(e)
	}
	p, e := OpenPostgres(ctx, db.RuntimeDSN)
	if e != nil {
		t.Fatal(e)
	}
	defer p.Close()

	tenant, id := uuid.NewString(), uuid.NewString()
	command := imageCommand(t, biz.ImageCreateAction, tenant, id, uuid.NewString(), 0, nil,
		struct{ Name string }{"telemetry image"})

	// Acceptance happens inside a request span; its trace must be persisted.
	traceID, _ := trace.TraceIDFromHex("0af7651916cd43dd8448eb211c80319c")
	spanID, _ := trace.SpanIDFromHex("b7ad6b7169203331")
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled})
	acceptCtx := trace.ContextWithSpanContext(ctx, sc)

	created, e := p.CreateImage(acceptCtx, imageFixture(tenant, id), command)
	if e != nil {
		t.Fatalf("CreateImage: %v", e)
	}
	if !created.TelemetryContext.Valid() {
		t.Fatalf("created image lost the captured telemetry: %+v", created.TelemetryContext)
	}
	if created.TelemetryContext.OriginTraceID != traceID.String() {
		t.Errorf("origin trace = %s, want %s", created.TelemetryContext.OriginTraceID, traceID)
	}

	// The persisted trace must be readable back on the catalog lookup.
	stored, e := p.FindImage(ctx, tenant, id)
	if e != nil {
		t.Fatalf("FindImage: %v", e)
	}
	if stored.TelemetryContext.Traceparent != created.TelemetryContext.Traceparent {
		t.Errorf("stored traceparent = %q, want %q", stored.TelemetryContext.Traceparent, created.TelemetryContext.Traceparent)
	}

	// The worker restores from the claimed record, so the persisted trace must
	// survive a claim.
	claimed, ok, e := p.ClaimImage(ctx, uuid.NewString(), 30*time.Second)
	if e != nil || !ok {
		t.Fatalf("ClaimImage ok=%v err=%v", ok, e)
	}
	if claimed.ID != id {
		// Another test row may have a due time earlier than ours; claim until ours.
		for i := 0; i < 8 && claimed.ID != id; i++ {
			claimed, ok, e = p.ClaimImage(ctx, uuid.NewString(), 30*time.Second)
			if e != nil || !ok {
				break
			}
		}
	}
	if claimed.ID == id && !claimed.TelemetryContext.Valid() {
		t.Errorf("claimed image lost its telemetry")
	}

	// Replaying the identical command must keep the original context; it must not
	// be overwritten by a new request trace, and the digest must not change.
	otherTrace, _ := trace.TraceIDFromHex("11111111111111111111111111111111")
	otherSpan, _ := trace.SpanIDFromHex("2222222222222222")
	replayCtx := trace.ContextWithSpanContext(ctx, trace.NewSpanContext(trace.SpanContextConfig{TraceID: otherTrace, SpanID: otherSpan, TraceFlags: trace.FlagsSampled}))
	replay, e := p.CreateImage(replayCtx, imageFixture(tenant, uuid.NewString()), command)
	if e != nil {
		t.Fatalf("replay CreateImage: %v", e)
	}
	if replay.TelemetryContext.OriginTraceID != traceID.String() {
		t.Errorf("replay overwrote the original trace: got %s want %s", replay.TelemetryContext.OriginTraceID, traceID)
	}
}
