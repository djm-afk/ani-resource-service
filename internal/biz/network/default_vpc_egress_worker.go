package biz

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/uuid"

	"github.com/zhangzhe-ctrl/ani-resource-service/internal/telemetry"
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
	if err != nil || !found {
		return found, err
	}
	// Resume the active operation's trace and open this pass's own span. The
	// default egress is platform-scoped, so no tenant_id is invented.
	ctx, runSpan, telemetryEvent, telemetryErr := telemetry.Resume(ctx, x.TelemetryContext, x.TelemetryContextError, "network.default_egress")
	defer runSpan.End()
	logTelemetryEvent(ctx, w.logger, telemetryEvent, []any{"resource_type", "default_vpc_egress", "resource_id", x.Egress.ID, "cluster_id", x.Egress.Identity.ClusterID}, telemetryErr)
	e := x.Egress
	active := e.OperationState != "succeeded" && e.OperationState != "failed"
	p := DefaultEgressProgress{State: e.State, Phase: e.Phase, Reason: e.Reason, PendingUnknown: e.PendingUnknown, Delay: w.policy.ObserveEvery}
	if active {
		p.OperationState = Retrying
	}
	finish := func() (bool, error) {
		call, stop := context.WithTimeout(context.WithoutCancel(ctx), w.policy.RequestTimeout)
		defer stop()
		err := w.repository.FinishDefaultVPCEgress(call, x, p)
		if err != nil {
			w.logPersistFailed(ctx, x, err)
		}
		return true, err
	}
	call, stop := context.WithTimeout(ctx, w.policy.RequestTimeout)
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
		}
		p.Backoff, p.Delay = true, w.policy.RetryMin
		return finish()
	}
	p.Observation, p.Observed = o, true
	// Persist a recovered create UID before dispatching another command.
	if (o.EIPExists && e.EIPProviderUID == "") || (o.SnatExists && e.SnatProviderUID == "") {
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
			if active && providerKind(mutationErr) == ProviderReject {
				if e.State == Deleting {
					p.OperationState = Blocked
				} else {
					p.OperationState = OpFailed
				}
			}
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
