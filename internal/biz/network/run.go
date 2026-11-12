package biz

import (
	"context"

	"go.opentelemetry.io/otel/trace"

	"github.com/zhangzhe-ctrl/ani-resource-service/internal/telemetry"
)

// activeRun starts this pass's span. Each lane keeps a durable "unfinished
// operation" signal from its own Claim; only that case may resume the accepted
// trace. A terminal or observation-only pass opens a new trace so a finished
// operation is never inherited, and a terminal pass with absent metadata does
// not report context_missing.
func activeRun(base context.Context, active bool, saved telemetry.TelemetryContext, savedErr error, name string) (context.Context, trace.Span, string, error) {
	if !active {
		ctx, span := telemetry.StartRun(base, name)
		return ctx, span, "", nil
	}
	return telemetry.Resume(base, saved, savedErr, name)
}
