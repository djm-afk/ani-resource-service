package biz

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/zhangzhe-ctrl/ani-resource-service/internal/telemetry/logfields"
)

// This worker owns one platform aggregate and its durable lease. It uses the
// existing bounded worker policy; it never manufactures tenant work records.
type DefaultVPCEgressWorker struct {
	repository DefaultEgressWorkRepository
	provider   DefaultEgressProvider
	owner      string
	policy     WorkerPolicy
	logger     *slog.Logger
}

// SetLogger attaches the structured logger for telemetry-context events.
func (w *DefaultVPCEgressWorker) SetLogger(logger *slog.Logger) { w.logger = logger }

func NewDefaultVPCEgressWorker(r DefaultEgressWorkRepository, p DefaultEgressProvider, owner string, policy WorkerPolicy) (*DefaultVPCEgressWorker, error) {
	id, err := uuid.Parse(owner)
	if r == nil || p == nil || err != nil || id == uuid.Nil || policy.RequestTimeout <= 0 || policy.Lease < 3*policy.RequestTimeout || policy.ObserveEvery <= 0 || policy.RetryMin <= 0 {
		return nil, Fail(InvalidArgument, "invalid platform default egress worker dependencies")
	}
	return &DefaultVPCEgressWorker{repository: r, provider: p, owner: id.String(), policy: policy}, nil
}

// logProviderFailure records the real Kubernetes cause before the worker turns
// it into a durable reason and backoff.
func (w *DefaultVPCEgressWorker) logProviderFailure(ctx context.Context, x DefaultEgressWork, action string, err error, reason Reason) {
	if w.logger == nil || err == nil {
		return
	}
	attrs := []any{
		"event", "dependency.failed",
		"dependency", "kubernetes",
		"action", action,
		"resource_type", "default_vpc_egress",
		"resource_id", x.Egress.ID,
		"cluster_id", x.Egress.Identity.ClusterID,
		"reason", string(reason),
	}
	if x.Egress.LastOperationID != "" {
		attrs = append(attrs, "operation_id", x.Egress.LastOperationID)
	}
	attrs = append(attrs, logfields.ErrorFields(err)...)
	w.logger.ErrorContext(ctx, "default vpc egress provider failed", attrs...)
}

// logPersistFailed records a failed progress write separately from the provider
// failure that preceded it.
func (w *DefaultVPCEgressWorker) logPersistFailed(ctx context.Context, x DefaultEgressWork, err error) {
	if w.logger == nil || err == nil {
		return
	}
	attrs := []any{
		"event", "state.persist_failed",
		"action", "finish_default_vpc_egress",
		"step", "persist_progress",
		"resource_type", "default_vpc_egress",
		"resource_id", x.Egress.ID,
		"cluster_id", x.Egress.Identity.ClusterID,
	}
	if x.Egress.LastOperationID != "" {
		attrs = append(attrs, "operation_id", x.Egress.LastOperationID)
	}
	attrs = append(attrs, logfields.ErrorFields(err)...)
	w.logger.ErrorContext(ctx, "default vpc egress state save failed", attrs...)
}

func (w *DefaultVPCEgressWorker) Step(ctx context.Context) (worked bool, err error) {
	defer func() {
		if errors.Is(err, ErrLeaseLost) {
			err = nil
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, w.policy.Lease)
	defer cancel()
	x, found, err := w.repository.ClaimDefaultVPCEgress(ctx, w.owner, w.policy.Lease)
	if err != nil {
		dependencyFailed(ctx, w.logger, WorkerDefaultEgress, "postgresql", "claim", nil, k8sTarget{}, err)
		return found, err
	}
	if !found {
		return false, nil
	}
	e := x.Egress
	active := e.OperationState != "succeeded" && e.OperationState != "failed"
	// The active decision precedes trace restoration: a terminal record is an
	// independent observation, so it must not inherit the finished operation's
	// trace and must not report absent metadata. The platform scope invents no
	// tenant.
	ctx, runSpan, telemetryEvent, telemetryErr := activeRun(ctx, x.ActiveOperation, x.TelemetryContext, x.TelemetryContextError, "network.default_egress")
	defer runSpan.End()
	identity := x.eventIdentity()
	startedAt := time.Now()
	runStarted(ctx, w.logger, WorkerDefaultEgress, identity)
	if telemetryEvent != "" {
		logTelemetryEvent(ctx, w.logger, telemetryEvent, []any{"resource_type", "default_vpc_egress", "resource_id", e.ID, "cluster_id", e.Identity.ClusterID}, telemetryErr)
	}
	p := DefaultEgressProgress{State: e.State, Phase: e.Phase, Reason: e.Reason, PendingUnknown: e.PendingUnknown, Delay: w.policy.ObserveEvery}
	// providerCause keeps the original mutation error for the terminal event,
	// which is emitted only after the write that records OpFailed succeeds.
	var providerCause error
	if active {
		p.OperationState = Retrying
		networkEvent(ctx, w.logger, slog.LevelInfo, EventOperationStarted,
			"operation_id", e.LastOperationID, "action", x.OperationKind,
			"resource_type", "default_vpc_egress", "resource_id", e.ID, "cluster_id", e.Identity.ClusterID)
	}
	finish := func() (bool, error) {
		call, stop := context.WithTimeout(context.WithoutCancel(ctx), w.policy.RequestTimeout)
		defer stop()
		stepStarted(ctx, w.logger, WorkerDefaultEgress, StepPersistDefaultEG, identity)
		err := w.repository.FinishDefaultVPCEgress(call, x, p)
		switch {
		case errors.Is(err, ErrLeaseLost):
			operationLeaseLost(ctx, w.logger, WorkerDefaultEgress, StepPersistDefaultEG, identity)
		case err != nil:
			w.logPersistFailed(ctx, x, err)
		default:
			stepSucceeded(ctx, w.logger, WorkerDefaultEgress, StepPersistDefaultEG, identity)
			// Only now is the failed terminal a fact: the row is committed and
			// the lease was not lost.
			if active && p.OperationState == OpFailed {
				operationFailed(ctx, w.logger, WorkerDefaultEgress, identity, e.LastOperationID, p.OperationReason, providerCause)
			}
		}
		runCompleted(ctx, w.logger, WorkerDefaultEgress, identity, time.Since(startedAt))
		return true, err
	}
	call, stop := context.WithTimeout(ctx, w.policy.RequestTimeout)
	stepStarted(ctx, w.logger, WorkerDefaultEgress, StepObserveDefaultEG, identity)
	o, readErr := w.provider.ObserveDefaultVPCEgress(call, x)
	stop()
	if readErr != nil {
		// Keep the real provider cause before it is reduced to a reason/backoff.
		w.logProviderFailure(ctx, x, "observe_default_vpc_egress", readErr, observationReason(readErr))
		p.OperationReason = observationReason(readErr)
		if providerKind(readErr) != ProviderConflict {
			p.ObservationError = observationReason(readErr)
		}
		if active && providerKind(readErr) == ProviderConflict {
			p.OperationState = Blocked
			operationConflict(ctx, w.logger, WorkerDefaultEgress, StepObserveDefaultEG, identity, observationReason(readErr), nil)
		}
		p.Backoff, p.Delay = true, w.policy.RetryMin
		if active {
			networkEvent(ctx, w.logger, slog.LevelWarn, EventOperationRetry,
				"operation_id", e.LastOperationID, "reason", string(p.OperationReason))
		}
		return finish()
	}
	stepSucceeded(ctx, w.logger, WorkerDefaultEgress, StepObserveDefaultEG, identity)
	p.Observation, p.Observed = o, true
	// Persist a recovered create UID before dispatching another command.
	if (o.EIPExists && e.EIPProviderUID == "") || (o.SnatExists && e.SnatProviderUID == "") {
		stepWaiting(ctx, w.logger, WorkerDefaultEgress, StepApplyDefaultEG, identity, ProviderNotReady)
		p.ClearPending, p.PendingUnknown, p.Delay = true, false, w.policy.RetryMin
		return finish()
	}
	mutate := func(action string) (bool, error) {
		if err := w.repository.BeginDefaultVPCEgressMutation(ctx, x, action); err != nil {
			return true, err
		}
		if err := w.repository.AssertDefaultVPCEgressLease(ctx, x); err != nil {
			return true, err
		}
		call, stop := context.WithTimeout(ctx, w.policy.RequestTimeout)
		stepStarted(ctx, w.logger, WorkerDefaultEgress, StepApplyDefaultEG, identity)
		mutationErr := w.provider.MutateDefaultVPCEgress(call, x, action)
		stop()
		p.ClearPending, p.PendingUnknown, p.Delay = false, mutationErr != nil, w.policy.RetryMin
		if mutationErr != nil {
			// The mutation failure is the original cause; persist failures below
			// are a separate state.persist_failed on the same trace.
			w.logProviderFailure(ctx, x, action, mutationErr, providerReason(mutationErr))
			p.OperationReason, p.Backoff = providerReason(mutationErr), true
			if providerKind(mutationErr) != ProviderUncertain {
				p.ClearPending, p.PendingUnknown = true, false
			}
			if providerKind(mutationErr) == ProviderConflict {
				operationConflict(ctx, w.logger, WorkerDefaultEgress, StepApplyDefaultEG, identity, providerReason(mutationErr), nil)
			}
			if active && providerKind(mutationErr) == ProviderReject {
				if e.State == Deleting {
					p.OperationState = Blocked
				} else {
					p.OperationState = OpFailed
					providerCause = mutationErr
				}
			}
		} else {
			stepSucceeded(ctx, w.logger, WorkerDefaultEgress, StepApplyDefaultEG, identity)
		}
		return finish()
	}
	if e.State == Deleting {
		if o.SnatExists {
			if e.PendingAction != "" && e.PendingAction != "delete_snat" {
				p.ClearPending, p.PendingUnknown, p.Delay = true, false, w.policy.RetryMin
				return finish()
			}
			p.Phase = "delete_snat"
			return mutate("delete_snat")
		}
		if o.EIPExists {
			if e.PendingAction != "" && e.PendingAction != "delete_eip" {
				p.ClearPending, p.PendingUnknown, p.Delay = true, false, w.policy.RetryMin
				return finish()
			}
			p.Phase = "delete_eip"
			return mutate("delete_eip")
		}
		p.State, p.Phase, p.OperationState, p.Reason, p.OperationReason = Deleted, "deleted", Succeeded, "", ""
		p.ClearPending, p.PendingUnknown = true, false
		return finish()
	}
	if !o.EIPExists {
		if active && e.EIPProviderUID == "" {
			p.Phase = "create_eip"
			return mutate("create_eip")
		}
		p.Reason = ProviderMissing
		if e.State == Available || e.State == Degraded {
			p.State = Degraded
		}
		return finish()
	}
	if !o.SnatExists {
		if active && e.SnatProviderUID == "" {
			if e.PendingAction != "" && e.PendingAction != "create_snat" {
				p.ClearPending, p.PendingUnknown, p.Delay = true, false, w.policy.RetryMin
				return finish()
			}
			p.Phase = "create_snat"
			return mutate("create_snat")
		}
		p.Reason = ProviderMissing
		if e.State == Available || e.State == Degraded {
			p.State = Degraded
		}
		return finish()
	}
	p.ClearPending, p.PendingUnknown = true, false
	if o.Ready != nil && *o.Ready {
		p.State, p.Phase, p.Reason, p.OperationReason = Available, "ready", "", ""
		if active {
			p.OperationState = Succeeded
		}
	} else if active {
		p.Reason = o.StatusReason
		if p.Reason == "" {
			p.Reason = ProviderStatusUnreported
		}
	}
	return finish()
}
