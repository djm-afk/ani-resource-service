package biz

import (
	"context"
	"errors"
	"github.com/google/uuid"
)

// This worker owns one platform aggregate and its durable lease. It uses the
// existing bounded worker policy; it never manufactures tenant work records.
type DefaultVPCEgressWorker struct {
	repository DefaultEgressWorkRepository
	provider   DefaultEgressProvider
	owner      string
	policy     WorkerPolicy
}

func NewDefaultVPCEgressWorker(r DefaultEgressWorkRepository, p DefaultEgressProvider, owner string, policy WorkerPolicy) (*DefaultVPCEgressWorker, error) {
	id, err := uuid.Parse(owner)
	if r == nil || p == nil || err != nil || id == uuid.Nil || policy.RequestTimeout <= 0 || policy.Lease < 3*policy.RequestTimeout || policy.ObserveEvery <= 0 || policy.RetryMin <= 0 {
		return nil, Fail(InvalidArgument, "invalid platform default egress worker dependencies")
	}
	return &DefaultVPCEgressWorker{r, p, id.String(), policy}, nil
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
	e := x.Egress
	active := e.OperationState != "succeeded" && e.OperationState != "failed"
	p := DefaultEgressProgress{State: e.State, Phase: e.Phase, Reason: e.Reason, PendingUnknown: e.PendingUnknown, Delay: w.policy.ObserveEvery}
	if active {
		p.OperationState = Retrying
	}
	finish := func() (bool, error) {
		call, stop := context.WithTimeout(context.WithoutCancel(ctx), w.policy.RequestTimeout)
		defer stop()
		return true, w.repository.FinishDefaultVPCEgress(call, x, p)
	}
	call, stop := context.WithTimeout(ctx, w.policy.RequestTimeout)
	o, readErr := w.provider.ObserveDefaultVPCEgress(call, x)
	stop()
	if readErr != nil {
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
