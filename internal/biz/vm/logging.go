package vm

import (
	"context"
	"time"

	"github.com/zhangzhe-ctrl/ani-resource-service/internal/telemetry"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/telemetry/logfields"
)

// errorAttrs is the single place a background step turns a Go error into the
// registered error fields. The business error itself is never rewritten.
func errorAttrs(err error) []any { return logfields.ErrorFields(err) }

// vmWorkerObject renders the known VM coordinates an operator can query. Only
// values already held by the worker are set; nothing is fetched.
func vmWorkerObject(v VM) telemetry.Target {
	return telemetry.Target{
		ClusterID: v.Spec.ClusterID,
		Kind:      "VirtualMachine",
		Namespace: v.Spec.Namespace,
		Name:      v.Name,
		UID:       v.Native.UID,
	}
}

// operationIdentity attaches the operation/resource identity once, instead of
// repeating the same keys at each call site.
func operationIdentity(o Operation, v VM) []any {
	out := []any{}
	if o.ID != "" {
		out = append(out, "operation_id", o.ID)
	}
	if o.TenantID != "" {
		out = append(out, "tenant_id", o.TenantID)
	}
	if o.Action != "" {
		out = append(out, "action", o.Action)
	}
	if v.ID != "" {
		out = append(out, "resource_type", "vm", "resource_id", v.ID)
	}
	if o.Step != "" {
		out = append(out, "step", o.Step)
	}
	return out
}

// logStepFailed records a real failure at the point it is converted into
// durable state, so a Save that then returns nil cannot hide it.
func (w *Worker) logStepFailed(ctx context.Context, o Operation, v VM, step string, err error) {
	if w.logger == nil {
		return
	}
	attrs := []any{
		"event", "step.failed",
		"step", step,
		"result", "failed",
	}
	attrs = append(attrs, operationIdentity(Operation{ID: o.ID, TenantID: o.TenantID, Action: o.Action}, v)...)
	attrs = append(attrs, errorAttrs(err)...)
	attrs = append(attrs, vmWorkerObject(v).KV()...)
	w.logger.ErrorContext(ctx, "VM step failed", attrs...)
}

// logStepFailedObserved records a failure that exists only as a native status
// reason: there is no Go error to unwrap, so the reason plus step is the
// evidence. The clone lane writes the terminal state on a later tick and
// returns nil, so this is the only place the real failure is visible.
func (w *Worker) logStepFailedObserved(ctx context.Context, o Operation, v VM, step, reason string) {
	if w.logger == nil {
		return
	}
	attrs := []any{
		"event", "step.failed",
		"step", step,
		"result", "failed",
		"reason", reason,
		"error_unavailable", true,
	}
	attrs = append(attrs, operationIdentity(Operation{ID: o.ID, TenantID: o.TenantID, Action: o.Action}, v)...)
	attrs = append(attrs, vmWorkerObject(v).KV()...)
	w.logger.ErrorContext(ctx, "VM step failed with an observed status", attrs...)
}

// logOperationFailed records a durable failed/unknown operation outcome.
func (w *Worker) logOperationFailed(ctx context.Context, o Operation, v VM, reason string, err error) {
	if w.logger == nil {
		return
	}
	attrs := []any{
		"event", "operation.failed",
		"result", "failed",
		"reason", reason,
	}
	attrs = append(attrs, operationIdentity(o, v)...)
	if err != nil {
		attrs = append(attrs, errorAttrs(err)...)
	} else {
		attrs = append(attrs, "error_unavailable", true)
	}
	attrs = append(attrs, vmWorkerObject(v).KV()...)
	w.logger.ErrorContext(ctx, "VM operation failed", attrs...)
}

// logPersistFailed records a failed write of the operation/VM state. It is a
// separate ERROR so the original business failure is never overwritten.
func (w *Worker) logPersistFailed(ctx context.Context, o Operation, v VM, err error) {
	if w.logger == nil || err == nil {
		return
	}
	attrs := []any{
		"event", "state.persist_failed",
		"action", o.Action,
		"step", "persist_operation",
	}
	attrs = append(attrs, operationIdentity(o, v)...)
	attrs = append(attrs, errorAttrs(err)...)
	w.logger.ErrorContext(ctx, "VM operation state save failed", attrs...)
}

// logRetryScheduled records the existing backoff decision. It never adds a
// retry; it only makes the decision already taken visible.
func (w *Worker) logRetryScheduled(ctx context.Context, o Operation, v VM, reason string, next time.Time) {
	if w.logger == nil {
		return
	}
	attrs := []any{
		"event", "operation.retry_scheduled",
		"reason", reason,
	}
	attrs = append(attrs, operationIdentity(o, v)...)
	if !next.IsZero() {
		attrs = append(attrs, "next_retry", next.UTC().Format(time.RFC3339Nano))
	}
	w.logger.WarnContext(ctx, "VM operation retry scheduled", attrs...)
}

// logConflict records a durable version/lease conflict as WARN, never as a
// dependency failure.
func (w *Worker) logConflict(ctx context.Context, o Operation, v VM, reason string) {
	if w.logger == nil {
		return
	}
	attrs := append([]any{"event", "operation.conflict", "reason", reason}, operationIdentity(o, v)...)
	w.logger.WarnContext(ctx, "VM operation conflict", attrs...)
}

// logStepWaiting records an existing normal wait. It is emitted only at a
// business-known state transition, not on every poll.
func (w *Worker) logStepWaiting(ctx context.Context, o Operation, v VM, step, reason string) {
	if w.logger == nil {
		return
	}
	attrs := append([]any{"event", "step.waiting", "step", step, "reason", reason, "result", "waiting"},
		operationIdentity(Operation{ID: o.ID, TenantID: o.TenantID, Action: o.Action}, v)...)
	attrs = append(attrs, vmWorkerObject(v).KV()...)
	w.logger.InfoContext(ctx, "VM step waiting", attrs...)
}
