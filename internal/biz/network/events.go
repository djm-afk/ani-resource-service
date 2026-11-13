package biz

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/zhangzhe-ctrl/ani-resource-service/internal/telemetry"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/telemetry/logfields"
)

// Fixed worker names from the Network execution plan. Every lane declares its
// own so run-level records never guess another lane's identity.
const (
	WorkerMain          = "network-worker"
	WorkerAttachment    = "network-attachment-worker"
	WorkerVirtualSwitch = "network-virtual-switch-worker"
	WorkerSecurityGroup = "network-security-group-worker"
	WorkerDefaultEgress = "network-default-egress-worker"
	WorkerObservation   = "network-observation"
)

// Fixed step names from the Network execution plan.
const (
	StepObserveProvider   = "observe_provider"
	StepEnsureVPC         = "ensure_vpc"
	StepEnsureSubnet      = "ensure_subnet"
	StepEnsureEgress      = "ensure_egress"
	StepUpdateEgress      = "update_egress"
	StepDeleteProvider    = "delete_provider"
	StepPersistProgress   = "persist_progress"
	StepPrepareVMNetwork  = "prepare_vm_network"
	StepPlatformNetwork   = "platform_network"
	StepLoadBalancer      = "load_balancer"
	StepGetSubmission     = "get_submission"
	StepObserveAttachment = "observe_attachment"
	StepPersistAttachment = "persist_attachment"
	StepObserveSwitch     = "observe_switch"
	StepApplySwitch       = "apply_switch"
	StepPersistSwitch     = "persist_switch"
	StepApplySecurityGrp  = "apply_security_group"
	StepPersistSecGrp     = "persist_security_group"
	StepObserveDefaultEG  = "observe_default_egress"
	StepApplyDefaultEG    = "apply_default_egress"
	StepPersistDefaultEG  = "persist_default_egress"
	StepWatchSource       = "watch_source"
	StepNotifyObserved    = "notify_observed_object"
)

// Contract event names used by the Network lanes.
const (
	EventRunStarted        = "worker.run_started"
	EventRunCompleted      = "worker.run_completed"
	EventRunFailed         = "worker.run_failed"
	EventRetryScheduled    = "worker.retry_scheduled"
	EventOperationStarted  = "operation.started"
	EventOperationSuccess  = "operation.succeeded"
	EventOperationFailed   = "operation.failed"
	EventOperationRetry    = "operation.retry_scheduled"
	EventOperationLease    = "operation.lease_lost"
	EventOperationConflict = "operation.conflict"
	EventStepStarted       = "step.started"
	EventStepSucceeded     = "step.succeeded"
	EventStepWaiting       = "step.waiting"
	EventStepSkipped       = "step.skipped"
	EventStepFailed        = "step.failed"
	EventPersistFailed     = "state.persist_failed"
	EventDependencyFailed  = "dependency.failed"
	EventResourceMutated   = "resource.mutated"
	EventResourceReplayed  = "resource.replayed"
)

// LeaseLost is the durable reason recorded when another owner already holds the
// record; the worker keeps it out of the error path.
const LeaseLost Reason = "LEASE_LOST"

// networkEvent records one contract event. Empty string values are dropped so a
// record never carries a blank field, and a nil logger keeps the lane running
// without telemetry. A standalone slog.Attr (the grouped target) passes through
// unchanged.
//
// The first occurrence of a key wins. Callers compose a helper's own fields with
// a lane identity that often repeats action/operation_id/resource_*, and the JSON
// contract forbids duplicate keys in one record.
func networkEvent(ctx context.Context, logger *slog.Logger, level slog.Level, event string, kv ...any) {
	if logger == nil {
		return
	}
	attrs := make([]any, 0, len(kv)+2)
	seen := make(map[string]struct{}, len(kv)/2+1)
	attrs = append(attrs, "event", event)
	seen["event"] = struct{}{}
	for i := 0; i < len(kv); {
		if attr, ok := kv[i].(slog.Attr); ok {
			if _, dup := seen[attr.Key]; !dup {
				seen[attr.Key] = struct{}{}
				attrs = append(attrs, attr)
			}
			i++
			continue
		}
		if i+1 >= len(kv) {
			break
		}
		key, ok := kv[i].(string)
		if !ok {
			i += 2
			continue
		}
		value := kv[i+1]
		i += 2
		if s, isString := value.(string); isString && s == "" {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		attrs = append(attrs, key, value)
	}
	logger.Log(ctx, level, eventMessage(event), attrs...)
}

func eventMessage(event string) string {
	return strings.ReplaceAll(strings.TrimPrefix(event, "network."), ".", " ")
}

// stepStarted and stepSucceeded bracket every worker stage; the contract makes
// the success side mandatory even for stages that only used to log failures.
func stepStarted(ctx context.Context, logger *slog.Logger, worker, step string, identity []any) {
	networkEvent(ctx, logger, slog.LevelInfo, EventStepStarted, append([]any{"worker", worker, "step", step}, identity...)...)
}

func stepSucceeded(ctx context.Context, logger *slog.Logger, worker, step string, identity []any) {
	networkEvent(ctx, logger, slog.LevelInfo, EventStepSucceeded, append([]any{"worker", worker, "step", step}, identity...)...)
}

// stepFailed keeps the real cause; an empty cause still records the reason so a
// failure is never invisible.
func stepFailed(ctx context.Context, logger *slog.Logger, worker, step string, identity []any, t k8sTarget, cause error, reason Reason) {
	// step.failed requires a non-blank reason and fixes result=failed.
	if reason == "" {
		reason = ProviderNotReady
	}
	attrs := append([]any{"worker", worker, "step", step, "reason", string(reason), "result", "failed"}, identity...)
	attrs = append(attrs, t.kv()...)
	if cause != nil {
		attrs = append(attrs, logfields.ErrorFields(cause)...)
	}
	networkEvent(ctx, logger, slog.LevelError, EventStepFailed, attrs...)
}

// dependencyFailed records the original cause before it is reduced to a reason.
func dependencyFailed(ctx context.Context, logger *slog.Logger, worker, dependency, action string, identity []any, t k8sTarget, cause error) {
	attrs := append([]any{"worker", worker, "dependency", dependency, "action", action}, identity...)
	attrs = append(attrs, t.kv()...)
	attrs = append(attrs, logfields.ErrorFields(cause)...)
	networkEvent(ctx, logger, slog.LevelError, EventDependencyFailed, attrs...)
}

func persistFailed(ctx context.Context, logger *slog.Logger, worker, action, step string, identity []any, cause error) {
	attrs := append([]any{"worker", worker, "dependency", "postgresql", "action", action, "step", step}, identity...)
	attrs = append(attrs, logfields.ErrorFields(cause)...)
	networkEvent(ctx, logger, slog.LevelError, EventPersistFailed, attrs...)
}

func operationConflict(ctx context.Context, logger *slog.Logger, worker, step string, identity []any, reason Reason, cause error) {
	attrs := append([]any{"worker", worker, "step", step, "reason", string(reason)}, identity...)
	if cause != nil {
		attrs = append(attrs, logfields.ErrorFields(cause)...)
	}
	networkEvent(ctx, logger, slog.LevelWarn, EventOperationConflict, attrs...)
}

func operationLeaseLost(ctx context.Context, logger *slog.Logger, worker, step string, identity []any) {
	// operation.lease_lost requires an operation_id. A lane pass that owns no
	// operation (a pure terminal reconciliation) still lost a lease, and that is
	// a concurrency outcome rather than a fault, so it is recorded as the
	// contract's conflict event instead of fabricating an operation.
	if !identityHas(identity, "operation_id") {
		operationConflict(ctx, logger, worker, step, identity, LeaseLost, nil)
		return
	}
	networkEvent(ctx, logger, slog.LevelWarn, EventOperationLease, append([]any{"worker", worker, "step", step, "reason", string(LeaseLost)}, identity...)...)
}

// identityHas reports whether the identity list carries a non-empty value for
// key. The event helper drops empty strings later, so the check must happen on
// the source values.
func identityHas(identity []any, key string) bool {
	for i := 0; i+1 < len(identity); i += 2 {
		if name, ok := identity[i].(string); ok && name == key {
			if value, ok := identity[i+1].(string); ok && value != "" {
				return true
			}
		}
	}
	return false
}

// operationFailed keeps the real cause: an observed failure without an error is
// still allowed, but a cause that exists must never be replaced by its reason.
func operationFailed(ctx context.Context, logger *slog.Logger, worker string, identity []any, operationID string, reason Reason, cause error) {
	attrs := append([]any{"worker", worker, "operation_id", operationID, "result", "failed"}, identity...)
	if reason != "" {
		attrs = append(attrs, "reason", string(reason))
	}
	if cause != nil {
		attrs = append(attrs, logfields.ErrorFields(cause)...)
	} else {
		attrs = append(attrs, "error_unavailable", true)
	}
	networkEvent(ctx, logger, slog.LevelError, EventOperationFailed, attrs...)
}

func operationSucceeded(ctx context.Context, logger *slog.Logger, worker string, identity []any, operationID string) {
	networkEvent(ctx, logger, slog.LevelInfo, EventOperationSuccess, append([]any{"worker", worker, "operation_id", operationID, "result", "succeeded"}, identity...)...)
}

func stepWaiting(ctx context.Context, logger *slog.Logger, worker, step string, identity []any, reason Reason) {
	// step.waiting requires a non-blank reason and fixes result=waiting.
	if reason == "" {
		reason = ProviderNotReady
	}
	networkEvent(ctx, logger, slog.LevelInfo, EventStepWaiting,
		append([]any{"worker", worker, "step", step, "reason", string(reason), "result", "waiting"}, identity...)...)
}

// target is the known Kubernetes identity, filled only from already loaded
// values. It never triggers a lookup.
type k8sTarget struct{ clusterID, kind, namespace, name, uid string }

func (t k8sTarget) kv() []any {
	return telemetry.Target{
		ClusterID: t.clusterID, Kind: t.kind, Namespace: t.namespace,
		Name: t.name, UID: t.uid,
	}.KV()
}

// runStarted and runCompleted bracket one claimed pass. Only found=true passes
// emit them, so an empty poll stays silent.
func runStarted(ctx context.Context, logger *slog.Logger, worker string, identity []any) {
	networkEvent(ctx, logger, slog.LevelInfo, EventRunStarted, append([]any{"worker", worker}, identity...)...)
}

func runCompleted(ctx context.Context, logger *slog.Logger, worker string, identity []any, elapsed time.Duration) {
	attrs := append([]any{"worker", worker, "result", "completed", "duration_ms", durationMillis(elapsed)}, identity...)
	networkEvent(ctx, logger, slog.LevelInfo, EventRunCompleted, attrs...)
}

// runFailed records the fatal cause that stopped a lane, then the caller keeps
// its original exit behavior.
func runFailed(ctx context.Context, logger *slog.Logger, worker string, identity []any, elapsed time.Duration, cause error) {
	attrs := append([]any{"worker", worker, "duration_ms", durationMillis(elapsed)}, identity...)
	attrs = append(attrs, logfields.ErrorFields(cause)...)
	networkEvent(ctx, logger, slog.LevelError, EventRunFailed, attrs...)
}

func durationMillis(d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return float64(d.Microseconds()) / 1000.0
}

// eventIdentity renders the identity fields a lane actually holds. Platform
// resources have no tenant and attachments have no operation, so absent values
// stay out of the record instead of being fabricated.
func (w Work) eventIdentity() []any {
	kv := []any{"action", w.Operation.Kind, "resource_type", w.Resource.Kind, "resource_id", w.Resource.ID,
		"operation_id", w.Operation.ID, "tenant_id", w.Resource.TenantID, "binding_id", w.BindingID}
	if w.Epoch != 0 {
		kv = append(kv, "epoch", int(w.Epoch))
	}
	if w.ClaimedGeneration > 0 {
		kv = append(kv, "generation", int(w.ClaimedGeneration))
	}
	return kv
}

func (w AttachmentWork) eventIdentity() []any {
	return []any{"resource_type", "attachment", "resource_id", w.Attachment.ID, "attachment_id", w.Attachment.ID,
		"submission_id", w.Attachment.SubmissionID, "tenant_id", w.Attachment.TenantID}
}

func (w VirtualSwitchWork) eventIdentity() []any {
	return []any{"resource_type", "virtual_switch", "resource_id", w.Switch.ID,
		"operation_id", w.Switch.LastOperationID, "binding_id", w.Switch.BindingID, "cluster_id", w.Switch.ClusterID}
}

func (w SecurityGroupWork) eventIdentity() []any {
	kv := []any{"action", w.Operation.Kind, "resource_type", w.TargetType, "resource_id", w.TargetID,
		"tenant_id", w.TenantID, "operation_id", w.Operation.ID}
	if w.Epoch != 0 {
		kv = append(kv, "epoch", int(w.Epoch))
	}
	return kv
}

func (w DefaultEgressWork) eventIdentity() []any {
	return []any{"resource_type", "default_vpc_egress", "resource_id", w.Egress.ID,
		"operation_id", w.Egress.LastOperationID, "cluster_id", w.Egress.Identity.ClusterID}
}

// reportStageOutcome emits the event that matches what the stage actually
// observed. Persistence succeeding says nothing about the provider result: a
// saved backoff is still a failed or waiting observation, and calling it
// step.succeeded contradicts the error recorded on the same trace.
// StepForResourceKind names the provider mutation stage that owns a resource
// kind. The step vocabulary is fixed, so an unknown kind falls back to the VPC
// create stage rather than inventing a name.
func StepForResourceKind(kind string) string {
	switch kind {
	case "subnet":
		return StepEnsureSubnet
	case "eip", "snat":
		return StepEnsureEgress
	default:
		return StepEnsureVPC
	}
}

func reportStageOutcome(ctx context.Context, logger *slog.Logger, worker, step string, identity []any, t k8sTarget, progress Progress, stageErr error) {
	switch {
	case stageErr != nil && errors.Is(stageErr, ErrLeaseLost):
		// The write that lost the lease already recorded operation.lease_lost;
		// repeating it here would double-count one concurrency outcome, and
		// step.failed (fixed at ERROR) would misreport it as a fault.
		return
	case stageErr != nil:
		stepFailed(ctx, logger, worker, step, identity, t, stageErr, ReasonOf(stageErr))
	case progress.ProviderError != nil:
		if providerKind(progress.ProviderError) == ProviderConflict || providerKind(progress.ProviderError) == ProviderInUse {
			operationConflict(ctx, logger, worker, step, identity, progress.OperationReason, progress.ProviderError)
			return
		}
		stepFailed(ctx, logger, worker, step, identity, t, progress.ProviderError, progress.OperationReason)
	case progress.Backoff:
		stepWaiting(ctx, logger, worker, step, identity, stageReason(progress))
	default:
		stepSucceeded(ctx, logger, worker, step, identity)
	}
}

func stageReason(p Progress) Reason {
	if p.OperationReason != "" {
		return p.OperationReason
	}
	if p.Reason != "" {
		return p.Reason
	}
	return ProviderNotReady
}

// OrReason normalises an optional reason so required fields are never blank.
func OrReason(r Reason) Reason {
	if r == "" {
		return ProviderNotReady
	}
	return r
}

// reportPreparationFailure records a VM network preparation failure under the
// dependency that actually produced it. The stage reads PostgreSQL, validates
// locally and only then calls Kubernetes, so attributing every error to
// kubernetes would point an operator at the wrong system.
func reportPreparationFailure(ctx context.Context, logger *slog.Logger, worker, action string, identity []any, cause error) {
	if logger == nil || cause == nil {
		return
	}
	// The data layer already tags each failure with its origin: Kubernetes reads
	// come back as *ProviderError and PostgreSQL ones carry a DependencyUnavailable
	// biz reason. Reading those keeps the domain free of driver/k8s imports.
	dependency, source := "local", "local"
	var provider *ProviderError
	switch {
	case errors.As(cause, &provider):
		dependency, source = "kubernetes", "kubernetes"
	case ReasonOf(cause) == DependencyUnavailable:
		dependency, source = "postgresql", "postgresql"
	}
	attrs := append([]any{"event", EventDependencyFailed, "worker", worker, "dependency", dependency,
		"action", action, "step", StepPrepareVMNetwork, "error_source", source}, identity...)
	attrs = append(attrs, logfields.ErrorFields(cause)...)
	networkEvent(ctx, logger, slog.LevelError, EventDependencyFailed, attrs...)
}

// terminalOutcome reports the operation's terminal state, and only after the
// write that records it succeeded. Two things must never happen here:
//
//   - a persistence failure must not be reported as a business failure: when
//     finishErr is set the operation is still retrying in the database, so the
//     only correct record is the state.persist_failed the caller already wrote;
//   - a failed terminal must carry the real provider cause. The provider error
//     lives on the progress, not on finishErr, so an observer that reads only
//     the return value loses it.
func terminalOutcome(ctx context.Context, logger *slog.Logger, worker, operationID string, identity []any, progress Progress, finishErr error) {
	if logger == nil || finishErr != nil {
		return
	}
	switch progress.OperationState {
	case OpFailed:
		// The provider cause is required so the event keeps the actionable
		// message; a terminal failure without one is reported as unavailable
		// rather than as a blank error.
		cause := progress.ProviderError
		operationFailed(ctx, logger, worker, identity, operationID, progress.OperationReason, cause)
	case Succeeded:
		operationSucceeded(ctx, logger, worker, identity, operationID)
	}
}
