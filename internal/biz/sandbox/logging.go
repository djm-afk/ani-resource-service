package sandbox

import (
	"context"

	"github.com/zhangzhe-ctrl/ani-resource-service/internal/telemetry"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/telemetry/logfields"
)

// sandboxIdentity is the fixed operation/resource identity. The sandbox worker
// carries LastOperationID, so operation_id is real (not fabricated); it is
// omitted only when the record has none.
func sandboxIdentity(work *Work) []any {
	out := []any{"resource_type", "sandbox"}
	if work.Sandbox.ID != "" {
		out = append(out, "resource_id", work.Sandbox.ID)
	}
	if work.Sandbox.TenantID != "" {
		out = append(out, "tenant_id", work.Sandbox.TenantID)
	}
	if work.Sandbox.LastOperationID != "" {
		out = append(out, "operation_id", work.Sandbox.LastOperationID)
	}
	return out
}

// sandboxTarget renders the known Pod/workspace coordinates from ProviderRefs.
func sandboxTarget(s Sandbox) telemetry.Target {
	t := telemetry.Target{ClusterID: s.Snapshot.ClusterID, Namespace: s.Snapshot.Namespace}
	if s.ProviderRefs.PodName != "" {
		t.Kind, t.Name, t.UID = "Pod", s.ProviderRefs.PodName, s.ProviderRefs.PodUID
	}
	return t
}

// logSandboxStepFailed records the real provider error before it becomes a
// durable Observed/Reason and Finish returns nil.
func (w *Worker) logSandboxStepFailed(ctx context.Context, work *Work, step string, err error) {
	if w.logger == nil || err == nil {
		return
	}
	attrs := []any{"event", "step.failed", "dependency", "kubernetes", "step", step, "result", "failed"}
	attrs = append(attrs, sandboxIdentity(work)...)
	attrs = append(attrs, logfields.ErrorFields(err)...)
	attrs = append(attrs, sandboxTarget(work.Sandbox).KV()...)
	w.logger.ErrorContext(ctx, "sandbox step failed", attrs...)
}

// logSandboxPersistFailed records a failed Finish, separate from the provider
// error that preceded it.
func (w *Worker) logSandboxPersistFailed(ctx context.Context, work *Work, step string, err error) {
	if w.logger == nil || err == nil {
		return
	}
	attrs := []any{"event", "state.persist_failed", "action", "sandbox." + work.Sandbox.Desired, "step", step}
	attrs = append(attrs, sandboxIdentity(work)...)
	attrs = append(attrs, logfields.ErrorFields(err)...)
	w.logger.ErrorContext(ctx, "sandbox state save failed", attrs...)
}

// logSandboxTelemetry records a missing/malformed persisted trace; the business
// work continues.
func (w *Worker) logSandboxTelemetry(ctx context.Context, work *Work, event string, err error) {
	if w.logger == nil || event == "" {
		return
	}
	attrs := []any{"event", event}
	attrs = append(attrs, sandboxIdentity(work)...)
	if event == "telemetry.context_invalid" {
		attrs = append(attrs, "reason", "TELEMETRY_CONTEXT_INVALID")
		if err != nil {
			attrs = append(attrs, logfields.ErrorFields(err)...)
		}
	} else {
		attrs = append(attrs, "reason", "TELEMETRY_CONTEXT_MISSING")
	}
	w.logger.ErrorContext(ctx, "sandbox telemetry context unavailable", attrs...)
}

// restoreSandboxContext restores the claimed operation's trace and binds identity.
// It returns the telemetry.context_* event to report once the run span exists
// ("" when the metadata is usable) and the real decode/validate error.
func (w *Worker) restoreSandboxContext(ctx context.Context, work *Work) (context.Context, string, error) {
	business := telemetry.BusinessContext{
		ResourceType: "sandbox", ResourceID: work.Sandbox.ID, TenantID: work.Sandbox.TenantID, OperationID: work.Sandbox.LastOperationID,
	}
	if work.TelemetryContextError != nil {
		return telemetry.WithBusiness(ctx, business), "telemetry.context_invalid", work.TelemetryContextError
	}
	if !work.TelemetryContext.Valid() {
		return telemetry.WithBusiness(ctx, business), "telemetry.context_missing", nil
	}
	restored, e := telemetry.Restore(ctx, work.TelemetryContext)
	if e != nil {
		return telemetry.WithBusiness(ctx, business), "telemetry.context_invalid", e
	}
	return telemetry.WithBusiness(restored, business), "", nil
}
