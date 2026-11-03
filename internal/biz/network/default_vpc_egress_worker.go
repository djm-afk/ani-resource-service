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
	p := DefaultEgressProgress{State: e.State, Phase: e.Phase, Reason: e.Reason, PendingUnknown: e.PendingUnknown, Delay: w.policy.ObserveEvery}
	if e.OperationState != "succeeded" && e.OperationState != "failed" {
		p.OperationState = Retrying
	}
	finish := func() (bool, error) {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), w.policy.RequestTimeout)
		defer cancel()
		return true, w.repository.FinishDefaultVPCEgress(c, x, p)
	}
	c, stop := context.WithTimeout(ctx, w.policy.RequestTimeout)
	o, inspectErr := w.provider.ObserveDefaultVPCEgress(c, x)
	stop()
	if inspectErr != nil {
		p.Reason = providerReason(inspectErr)
		if providerKind(inspectErr) == ProviderUncertain {
			p.Reason = ProviderUnavailable // A failed read is not an unknown mutation.
		}
		if p.Reason != ProviderUnavailable {
			p.OperationState = Blocked
		}
		if e.State == Available && p.Reason != ProviderUnavailable {
			p.State = Degraded
		}
		return finish()
	}
	p.Observation, p.Observed = o, true
	if (o.EIPExists && o.EIPUID == "") || (o.SnatExists && o.SnatUID == "") {
		p.Reason = ProviderOwnership
		p.OperationState = Blocked
		return finish()
	}
	if (e.EIPProviderUID != "" && o.EIPExists && e.EIPProviderUID != o.EIPUID) || (e.SnatProviderUID != "" && o.SnatExists && e.SnatProviderUID != o.SnatUID) {
		p.Reason = ProviderOwnership
		p.OperationState = Blocked
		return finish()
	}
	if e.PendingAction == "create_eip" || e.PendingAction == "create_snat" {
		exists := o.EIPExists
		if e.PendingAction == "create_snat" {
			exists = o.SnatExists
		}
		if !exists {
			p.Reason = ProviderUnknown
			p.OperationState = Blocked
			return finish()
		}
		p.ClearPending, p.PendingUnknown = true, false
		p.Reason = CleanupPending
		p.Delay = w.policy.RetryMin
		return finish() // Save the observed UID and clear this marker before the next mutation.
	}
	mutate := func(action string) (bool, error) {
		// Persist before dispatch and recheck the same lease immediately before I/O.
		if err := w.repository.BeginDefaultVPCEgressMutation(ctx, x, action); err != nil {
			return true, err
		}
		if err := w.repository.AssertDefaultVPCEgressLease(ctx, x); err != nil {
			return true, err
		}
		c, stop := context.WithTimeout(ctx, w.policy.RequestTimeout)
		mutationErr := w.provider.MutateDefaultVPCEgress(c, x, action)
		stop()
		p.ClearPending = false
		p.PendingUnknown = mutationErr != nil
		p.Delay = w.policy.RetryMin
		if mutationErr != nil {
			p.Reason = providerReason(mutationErr)
			p.OperationState = Blocked
			if providerKind(mutationErr) != ProviderUncertain {
				p.ClearPending = true
				p.PendingUnknown = false
			}
			if providerKind(mutationErr) == ProviderReject && e.State != Deleting {
				p.State = Failed
				p.OperationState = OpFailed
			}
		} else {
			p.Reason = CleanupPending
		}
		return finish()
	}
	if e.State == Deleting {
		if o.SnatExists {
			p.Phase = "delete_snat"
			return mutate("delete_snat")
		}
		if !o.Unbound {
			p.Reason = ResourceInUse
			p.OperationState = Blocked
			return finish()
		}
		if e.PendingAction == "delete_snat" {
			p.ClearPending, p.PendingUnknown = true, false
			p.Phase = "delete_eip"
			p.Delay = w.policy.RetryMin
			return finish()
		}
		if o.EIPExists {
			p.Phase = "delete_eip"
			return mutate("delete_eip")
		}
		p.State, p.Phase, p.OperationState, p.Reason = Deleted, "deleted", Succeeded, ""
		p.ClearPending, p.PendingUnknown = true, false
		return finish()
	}
	if e.State == Failed {
		return finish()
	}
	if !o.EIPExists {
		if e.EIPProviderUID != "" {
			p.State, p.Reason, p.OperationState = Degraded, ProviderMissing, Blocked
			return finish()
		}
		p.Phase = "create_eip"
		return mutate("create_eip")
	}
	if !o.EIPReady {
		p.Reason = ProviderNotReady
		return finish()
	}
	if !o.SnatExists {
		if e.SnatProviderUID != "" {
			p.State, p.Reason, p.OperationState = Degraded, ProviderMissing, Blocked
			return finish()
		}
		p.Phase = "create_snat"
		return mutate("create_snat")
	}
	if o.Ready {
		p.State, p.Phase, p.OperationState, p.Reason = Available, "ready", Succeeded, ""
		p.ClearPending, p.PendingUnknown = true, false
	} else {
		p.Reason = ProviderNotReady
		if e.State == Available {
			p.State = Degraded
		}
	}
	return finish()
}
