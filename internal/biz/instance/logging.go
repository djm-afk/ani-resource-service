package instance

import (
	"context"

	"github.com/zhangzhe-ctrl/ani-resource-service/internal/telemetry"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/telemetry/logfields"
)

// instanceIdentity is the fixed operation/resource identity every background
// failure record carries. Empty fields are omitted; no id is fabricated.
func instanceIdentity(w *Work) []any {
	out := []any{"resource_type", "instance"}
	if w.Instance.ID != "" {
		out = append(out, "resource_id", w.Instance.ID)
	}
	if w.Instance.TenantID != "" {
		out = append(out, "tenant_id", w.Instance.TenantID)
	}
	if w.Operation != nil {
		if w.Operation.ID != "" {
			out = append(out, "operation_id", w.Operation.ID)
		}
		if w.Operation.Action != "" {
			out = append(out, "action", w.Operation.Action)
		}
		if w.Operation.Execution != nil && w.Operation.Execution.Phase != "" {
			out = append(out, "step", w.Operation.Execution.Phase)
		}
		if w.Operation.TelemetryContext.OriginTraceID != "" {
			out = append(out, "origin_trace_id", w.Operation.TelemetryContext.OriginTraceID)
		}
	}
	return out
}

// instanceTarget renders the known workload coordinates from fields already
// held by the instance; it never queries the cluster.
func instanceTarget(i Instance) telemetry.Target {
	t := telemetry.Target{ClusterID: i.Spec.ClusterID, Namespace: i.Spec.Namespace}
	switch {
	case i.NotebookUID != "":
		t.Kind, t.Name, t.UID = "Notebook", i.Spec.NotebookName, i.NotebookUID
	case i.Spec.NotebookName != "":
		t.Kind, t.Name = "Notebook", i.Spec.NotebookName
	}
	return t
}

// logInstanceStepFailed records a real failure at the point it is turned into a
// durable Reason/Progress, so a succeeding Finish cannot hide it.
func (w *Worker) logInstanceStepFailed(ctx context.Context, work *Work, step string, err error) {
	if w.logger == nil || err == nil {
		return
	}
	attrs := []any{"event", "step.failed", "step", step, "result", "failed"}
	attrs = append(attrs, instanceIdentity(work)...)
	attrs = append(attrs, logfields.ErrorFields(err)...)
	attrs = append(attrs, instanceTarget(work.Instance).KV()...)
	w.logger.ErrorContext(ctx, "instance step failed", attrs...)
}

// logInstancePersistFailed records a failed Finish/state write, distinct from the
// business error that preceded it.
func (w *Worker) logInstancePersistFailed(ctx context.Context, work *Work, step string, err error) {
	if w.logger == nil || err == nil {
		return
	}
	attrs := []any{"event", "state.persist_failed", "action", operationAction(work), "step", step}
	attrs = append(attrs, instanceIdentity(work)...)
	attrs = append(attrs, logfields.ErrorFields(err)...)
	w.logger.ErrorContext(ctx, "instance state save failed", attrs...)
}

// logInstanceWaiting records an existing normal wait at a business-known state.
func (w *Worker) logInstanceWaiting(ctx context.Context, work *Work, reason string) {
	if w.logger == nil {
		return
	}
	step := "observe"
	if work.Operation != nil && work.Operation.Execution != nil && work.Operation.Execution.Phase != "" {
		step = work.Operation.Execution.Phase
	}
	attrs := []any{"event", "step.waiting", "step", step, "reason", reason, "result", "waiting"}
	attrs = append(attrs, instanceIdentity(work)...)
	attrs = append(attrs, instanceTarget(work.Instance).KV()...)
	w.logger.InfoContext(ctx, "instance step waiting", attrs...)
}

// logInstanceTelemetry records a missing/malformed persisted trace. The business
// work continues; the run keeps its own valid trace.
func (w *Worker) logInstanceTelemetry(ctx context.Context, work *Work, event string, err error) {
	if w.logger == nil {
		return
	}
	attrs := []any{"event", event}
	attrs = append(attrs, instanceIdentity(work)...)
	if event == "telemetry.context_invalid" {
		attrs = append(attrs, "reason", "TELEMETRY_CONTEXT_INVALID")
		if err != nil {
			attrs = append(attrs, logfields.ErrorFields(err)...)
		}
	} else {
		attrs = append(attrs, "reason", "TELEMETRY_CONTEXT_MISSING")
	}
	w.logger.ErrorContext(ctx, "instance telemetry context unavailable", attrs...)
}

func operationAction(w *Work) string {
	if w.Operation == nil {
		return ""
	}
	return w.Operation.Action
}

// restoreInstanceContext restores the operation's persisted trace and binds the
// instance identity. A missing or malformed context is recorded once and the
// caller keeps running on a fresh trace.
func (w *Worker) restoreInstanceContext(ctx context.Context, work *Work) context.Context {
	business := telemetry.BusinessContext{ResourceType: "instance", ResourceID: work.Instance.ID, TenantID: work.Instance.TenantID}
	if work.Operation != nil {
		business.OperationID = work.Operation.ID
		business.Action = work.Operation.Action
	}
	if work.Operation == nil {
		return telemetry.WithBusiness(ctx, business)
	}
	if work.Operation.TelemetryContextError != nil {
		w.logInstanceTelemetry(ctx, work, "telemetry.context_invalid", work.Operation.TelemetryContextError)
		return telemetry.WithBusiness(ctx, business)
	}
	if !work.Operation.TelemetryContext.Valid() {
		w.logInstanceTelemetry(ctx, work, "telemetry.context_missing", nil)
		return telemetry.WithBusiness(ctx, business)
	}
	if restored, e := telemetry.Restore(ctx, work.Operation.TelemetryContext); e == nil {
		return telemetry.WithBusiness(restored, business)
	} else {
		w.logInstanceTelemetry(ctx, work, "telemetry.context_invalid", e)
	}
	return telemetry.WithBusiness(ctx, business)
}
