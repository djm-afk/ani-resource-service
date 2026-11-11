// Package telemetry carries the phase-one logging context for the resource
// service: the persisted W3C trace snapshot, the small business attribute set
// bound once per operation, and the error/target field helpers.
//
// It contains no storage, no Kubernetes access and no logger registry.
package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

const (
	HeaderTraceparent = "traceparent"
	HeaderTracestate  = "tracestate"
)

// TelemetryContext is the persisted trace snapshot. It is an independent column
// or JSON attribute and never participates in business hashes, digests or
// idempotency keys.
type TelemetryContext struct {
	Traceparent   string `json:"traceparent,omitempty"`
	Tracestate    string `json:"tracestate,omitempty"`
	OriginTraceID string `json:"origin_trace_id,omitempty"`
}

// Valid reports whether a traceparent was captured.
func (t TelemetryContext) Valid() bool { return strings.TrimSpace(t.Traceparent) != "" }

// Capture snapshots the current W3C trace context using the standard propagator.
func Capture(ctx context.Context) TelemetryContext {
	if ctx == nil {
		return TelemetryContext{}
	}
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	out := TelemetryContext{
		Traceparent: carrier.Get(HeaderTraceparent),
		Tracestate:  carrier.Get(HeaderTracestate),
	}
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() && sc.TraceID().IsValid() {
		out.OriginTraceID = sc.TraceID().String()
	}
	return out
}

// Decode parses only the independent logging metadata. Empty yields the zero
// value; a malformed value returns an error the worker records and then ignores.
func Decode(raw json.RawMessage) (TelemetryContext, error) {
	var out TelemetryContext
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" || trimmed == "{}" {
		return out, nil
	}
	if err := json.Unmarshal([]byte(trimmed), &out); err != nil {
		return TelemetryContext{}, fmt.Errorf("decode telemetry context: %w", err)
	}
	return out, nil
}

// Restore extracts the saved W3C trace into base. It restores span metadata
// only; deadlines, cancellation, tenant, authorization and credentials are not
// carried over.
func Restore(base context.Context, saved TelemetryContext) (context.Context, error) {
	if base == nil {
		base = context.Background()
	}
	carrier := propagation.MapCarrier{}
	if saved.Traceparent != "" {
		carrier.Set(HeaderTraceparent, saved.Traceparent)
	}
	if saved.Tracestate != "" {
		carrier.Set(HeaderTracestate, saved.Tracestate)
	}
	ctx := otel.GetTextMapPropagator().Extract(base, carrier)
	if sc := trace.SpanContextFromContext(ctx); !sc.IsValid() {
		return base, errors.New("telemetry context does not contain a valid traceparent")
	}
	return ctx, nil
}

// StartRun opens the span for one worker pass on top of base. The trace from
// Restore is a finished request span, so reusing it would attribute every later
// pass to the accepting HTTP span; a child span keeps the same trace_id and
// gives this pass its own span_id. Without a parent the provider assigns a new
// trace, which keeps trace_id/span_id present for the required worker events.
func StartRun(base context.Context, name string) (context.Context, trace.Span) {
	return otel.Tracer("ani-resource-service/worker").Start(base, name)
}

// Resume is the single restore step for a claimed background record: it
// restores the persisted trace when it is usable, always opens this pass's run
// span, and reports the one telemetry.context_* event the caller must record
// once that span exists ("" when the metadata was usable). The returned error is
// the real decode/validate cause for the invalid case. The caller continues the
// original business work in every case; nothing here changes business state.
func Resume(base context.Context, saved TelemetryContext, savedErr error, name string) (context.Context, trace.Span, string, error) {
	event, cause := "", error(nil)
	restored := base
	switch {
	case savedErr != nil:
		event, cause = "telemetry.context_invalid", savedErr
	case !saved.Valid():
		event = "telemetry.context_missing"
	default:
		next, e := Restore(base, saved)
		if e != nil {
			event, cause = "telemetry.context_invalid", e
		} else {
			restored = next
		}
	}
	ctx, span := StartRun(restored, name)
	return ctx, span, event, cause
}

// BusinessContext is the fixed business attribute set bound once per operation.
type BusinessContext struct {
	OperationID   string
	ResourceType  string
	ResourceID    string
	TenantID      string
	Action        string
	OriginTraceID string
}

type businessKey struct{}

// WithBusiness replaces the bound business attributes.
func WithBusiness(ctx context.Context, value BusinessContext) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, businessKey{}, value)
}

// BusinessAttrs renders the bound attributes for a log record.
func BusinessAttrs(ctx context.Context) []slog.Attr {
	if ctx == nil {
		return nil
	}
	v, ok := ctx.Value(businessKey{}).(BusinessContext)
	if !ok {
		return nil
	}
	out := make([]slog.Attr, 0, 6)
	for _, f := range []struct{ key, value string }{
		{"operation_id", v.OperationID},
		{"resource_type", v.ResourceType},
		{"resource_id", v.ResourceID},
		{"tenant_id", v.TenantID},
		{"action", v.Action},
		{"origin_trace_id", v.OriginTraceID},
	} {
		if f.value != "" {
			out = append(out, slog.String(f.key, f.value))
		}
	}
	return out
}
