package data

import (
	"context"
	"log/slog"

	"github.com/zhangzhe-ctrl/ani-resource-service/internal/telemetry"
)

// acceptedEvent records the post-commit outcome of an admission. It is the only
// place a replay can be distinguished from a new acceptance, because both the
// service and business layers see the same returned value.
//
// operation.rejected is not recorded here: a business rejection happens before
// the admission transaction commits, so it is recorded once from the inbound
// RPC completion instead.
type acceptedEvent struct {
	action       string
	resourceType string
	resourceID   string
	tenantID     string
	operationID  string
	replayed     bool
}

// logAcceptance writes operation.accepted or operation.replayed at INFO. A nil
// logger (or a struct with no logger bound) records nothing and never blocks the
// business path.
func (p *Postgres) logAcceptance(ctx context.Context, e acceptedEvent) {
	logger := p.logger
	// operation.accepted/operation.replayed both require an operation_id; an
	// admission without one belongs to the attachment mutation vocabulary.
	if logger == nil || e.operationID == "" {
		return
	}
	event, message := "operation.accepted", "network operation accepted"
	if e.replayed {
		event, message = "operation.replayed", "network operation replayed"
	}
	attrs := []any{"event", event, "action", e.action,
		"resource_type", e.resourceType, "resource_id", e.resourceID,
		"tenant_id", e.tenantID, "operation_id", e.operationID}
	if e.replayed {
		attrs = append(attrs, "result", "replayed")
		if origin := telemetry.Capture(ctx).OriginTraceID; origin != "" {
			attrs = append(attrs, "origin_trace_id", origin)
		}
	}
	logger.InfoContext(ctx, message, attrs...)
}

// logResourceMutation records a synchronous attachment mutation that has no
// business operation, so no operation_id is fabricated.
func (p *Postgres) logResourceMutation(ctx context.Context, replayed bool, action, resourceID, tenantID string) {
	logger := p.logger
	if logger == nil {
		return
	}
	event, result, message := "resource.mutated", "succeeded", "network attachment mutated"
	if replayed {
		event, result, message = "resource.replayed", "replayed", "network attachment replayed"
	}
	logger.InfoContext(ctx, message, "event", event, "action", action,
		"resource_type", "attachment", "resource_id", resourceID,
		"attachment_id", resourceID, "tenant_id", tenantID, "result", result)
}

// SetLogger binds the logger used for acceptance events. It is optional: the
// lane runs identically without telemetry.
func (p *Postgres) SetLogger(logger *slog.Logger) { p.logger = logger }
