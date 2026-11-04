package biz

import (
	"context"
	"errors"
)

// Components are steps of one LB task, fenced by its existing lease and
// resource version. They are never separately scheduled resources.
type LoadBalancerComponent struct {
	ID, Kind, MemberID, ListenerID, Name, Identity, PendingAction string
	ListenerName                                                  string
	CreateDispatched, Deleted                                     bool
	TargetVersion, AppliedVersion                                 int64
}
type LoadBalancerMemberIdentity struct {
	LoadBalancerBackend
	PodUID string
}
type LoadBalancerWork struct {
	LoadBalancer LoadBalancer
	Members      []LoadBalancerMemberIdentity
	Components   []LoadBalancerComponent
}
type LoadBalancerComponentObservation struct {
	Conflict                  bool
	ID, Identity              string
	Exists, Matches, Deleting bool
	Ready                     *bool
	StatusReason              Reason
	ClearPending, Deleted     bool
	AppliedVersion            int64
}
type LoadBalancerMemberObservation struct {
	ID       string
	Eligible bool
	Reason   Reason
}

type LoadBalancerObservation struct {
	Components         []LoadBalancerComponentObservation
	Members            []LoadBalancerMemberObservation
	ConfigurationState string
	AppliedVersion     int64
}
type LoadBalancerProvider interface {
	ObserveLoadBalancer(context.Context, Work) (LoadBalancerObservation, error)
	MutateLoadBalancer(context.Context, Work, LoadBalancerComponent, string, LoadBalancerObservation) (LoadBalancerComponentObservation, error)
}
type LoadBalancerWorkRepository interface {
	BeginLoadBalancerMutation(context.Context, Work, LoadBalancerComponent, string, string) error
}

func (w *Worker) stepLoadBalancer(ctx context.Context, work Work) error {
	provider, ok := w.provider.(LoadBalancerProvider)
	repository, repoOK := w.repository.(LoadBalancerWorkRepository)
	if !ok || !repoOK || work.Resource.LoadBalancer == nil {
		return errors.New("LB lifecycle dependencies unavailable")
	}
	p := Progress{State: work.Resource.State, Reason: work.Resource.Reason, Identity: work.KnownIdentity, NextDelay: w.policy.ObserveEvery}
	if work.ActiveOperation {
		p.OperationState = Retrying
	}
	finish := func() error {
		if p.Backoff {
			p.NextDelay = w.retryDelay(work.Attempt)
		}
		err := w.finish(ctx, work, p)
		if w.observer != nil {
			w.observer(ctx, work, p, err)
		}
		return lbLeaseResult(err)
	}
	if work.CancelUnsent {
		p.State, p.OperationState, p.ClearPending, p.Reason = Deleted, Succeeded, true, ""
		return finish()
	}
	call, cancel := context.WithTimeout(ctx, w.policy.RequestTimeout)
	o, err := provider.ObserveLoadBalancer(call, work)
	cancel()
	if err != nil {
		p.ProviderError, p.OperationReason, p.Backoff = err, observationReason(err), true
		if providerKind(err) != ProviderConflict {
			p.ObservationError = observationReason(err)
		}
		if work.ActiveOperation {
			p.OperationState = Blocked
		}
		return finish()
	}
	p.Observed = true
	lb := work.Resource.LoadBalancer.LoadBalancer
	o.ConfigurationState, o.AppliedVersion = lb.ConfigurationState, lb.AppliedVersion
	deleting := p.State == Deleting || p.State == Deleted
	runtime := ProjectLoadBalancerListeners(EffectiveLoadBalancerListeners(lb))
	draining := !deleting && len(runtime) == 0
	removing := deleting || draining
	eligible := lbRuntimeMembersEligible(runtime, o)
	order := []string{"retired_route", "retired_policy", "backend", "envoy_proxy", "gateway", "policy", "route"}
	if !eligible {
		order = []string{"retired_route", "retired_policy", "route", "backend", "envoy_proxy", "gateway", "policy"}
	}
	if removing {
		order = []string{"route", "policy", "gateway", "backend", "envoy_proxy"}
	}
	allReady, acted, reportedFalse, managementBlocked := true, false, false, false
	pendingReason := Reason("")
	for _, kind := range order {
		for _, c := range work.Resource.LoadBalancer.Components {
			retiredListener := c.ListenerID != "" && !lbDesiredListenerComponent(work, c)
			selected := c.Kind
			if LoadBalancerIsRoute(c.Kind) {
				selected = "route"
			} else if c.Kind == "client_policy" || c.Kind == "patch_policy" {
				selected = "policy"
			}
			if retiredListener && !removing {
				selected = "retired_" + selected
			}
			if selected != kind {
				continue
			}
			v, found := lbComponentObservation(o, c.ID)
			if !found {
				allReady = false
				continue
			}
			retired := c.Deleted || retiredListener || (c.Kind == "backend" && !lbDesiredMember(work, c.MemberID))
			remove := removing || retired
			// A different UID ends the original lifecycle. Never delete its replacement.
			if remove && c.Identity != "" && v.Exists && v.Identity != c.Identity {
				v = LoadBalancerComponentObservation{ID: c.ID, Identity: c.Identity}
			}
			if v.Conflict || (v.Exists && (v.Identity == "" || (c.Identity != "" && c.Identity != v.Identity))) {
				allReady, managementBlocked = false, true
				p.OperationReason = ProviderOwnership
				break
			}
			if retired && c.Kind == "backend" && !removing && !lbRouteCurrent(work, o) {
				allReady = false
				continue
			}
			action := ""
			switch {
			case remove:
				if !v.Exists {
					v.Deleted, v.ClearPending = true, true
					lbReplaceObservation(&o, v)
					continue
				}
				allReady = false
				if !v.Deleting {
					action = "delete"
				} else {
					pendingReason = CleanupPending
				}
			case !v.Exists:
				allReady = false
				if c.Identity != "" || c.Deleted || !work.ActiveOperation {
					pendingReason, reportedFalse = ProviderMissing, true
					break
				}
				if c.Kind == "backend" && !lbMemberEligibleForWorker(o, c.MemberID) {
					continue
				}
				action = "create"
			case !v.Matches:
				allReady = false
				if c.Kind == "backend" {
					p.OperationReason, managementBlocked = ProviderOwnership, true
					break
				}
				action = "update"
			default:
				v.ClearPending = c.PendingAction != "delete"
				if v.Ready != nil && *v.Ready {
					v.AppliedVersion = lb.ProviderTargetVersion
				} else {
					allReady = false
					if v.Ready != nil {
						reportedFalse = true
						pendingReason = v.StatusReason
					}
				}
				lbReplaceObservation(&o, v)
			}
			if managementBlocked {
				break
			}
			if action == "" {
				continue
			}
			if action == "update" && c.Identity == "" {
				// Save the recovered CREATE identity before the next lease can
				// begin an update against that registered object.
				v.ClearPending = true
				lbReplaceObservation(&o, v)
				acted, p.NextDelay = true, w.policy.RetryMin
				break
			}
			if err = repository.BeginLoadBalancerMutation(ctx, work, c, action, v.Identity); err != nil {
				return lbLeaseResult(err)
			}
			if v.Identity != "" {
				c.Identity = v.Identity
			}
			call, cancel = context.WithTimeout(ctx, w.policy.RequestTimeout)
			result, mutateErr := provider.MutateLoadBalancer(call, work, c, action, o)
			cancel()
			if mutateErr != nil {
				p.ProviderError, p.OperationReason, p.Backoff = mutateErr, providerReason(mutateErr), true
				v.ClearPending = providerKind(mutateErr) != ProviderUncertain
				lbReplaceObservation(&o, v)
				if work.ActiveOperation && providerKind(mutateErr) == ProviderReject {
					if deleting {
						p.OperationState = Blocked
					} else {
						p.OperationState = OpFailed
					}
				}
			} else {
				result.ClearPending = action != "delete"
				lbReplaceObservation(&o, result)
			}
			allReady, acted = false, true
			p.NextDelay = w.policy.RetryMin
			break
		}
		if acted || managementBlocked || (removing && !allReady) {
			break
		}
	}
	if managementBlocked {
		if work.ActiveOperation {
			p.OperationState = Blocked
		}
		p.Backoff = true
	} else if deleting {
		if allReady {
			p.State, p.OperationState, p.Reason = Deleted, Succeeded, ""
		}
	} else if draining {
		if allReady {
			p.State, p.OperationState, p.Reason = Provisioning, Succeeded, ""
			o.ConfigurationState, o.AppliedVersion = "waiting_for_backends", lb.ProviderTargetVersion
		}
	} else if allReady && eligible && lbRuntimeComponentsCovered(work, runtime) {
		p.State, p.Reason = Available, ""
		if work.ActiveOperation {
			p.OperationState = Succeeded
		}
		o.ConfigurationState, o.AppliedVersion = "configured", lb.ProviderTargetVersion
	} else {
		if !eligible {
			reportedFalse, pendingReason = true, BackendIdentityMismatch
		}
		if reportedFalse {
			p.Reason = pendingReason
			if p.Reason == "" {
				p.Reason = ProviderNotReady
			}
			if work.Resource.State == Available || work.Resource.State == Degraded {
				p.State, o.ConfigurationState = Degraded, "degraded"
			}
		} else if work.ActiveOperation {
			p.Reason = ProviderStatusUnreported
			o.ConfigurationState = "applying"
		}
	}
	for _, v := range o.Components {
		if v.ID == work.BindingID {
			if v.Identity != "" && !v.Conflict {
				p.Identity = v.Identity
			}
			p.ClearPending = v.ClearPending
		}
	}
	p.LoadBalancer = &o
	return finish()
}
func lbLeaseResult(err error) error {
	if errors.Is(err, ErrLeaseLost) {
		return nil
	}
	return err
}
func lbComponentObservation(o LoadBalancerObservation, id string) (LoadBalancerComponentObservation, bool) {
	for _, v := range o.Components {
		if v.ID == id {
			return v, true
		}
	}
	return LoadBalancerComponentObservation{}, false
}
func lbReplaceObservation(o *LoadBalancerObservation, v LoadBalancerComponentObservation) {
	for i := range o.Components {
		if o.Components[i].ID == v.ID {
			o.Components[i] = v
			return
		}
	}
}
func lbDesiredMember(w Work, id string) bool {
	for _, l := range ProjectLoadBalancerListeners(EffectiveLoadBalancerListeners(w.Resource.LoadBalancer.LoadBalancer)) {
		for _, m := range l.Backends {
			if m.ID == id {
				return true
			}
		}
	}
	return false
}
func lbRouteCurrent(w Work, o LoadBalancerObservation) bool {
	found := false
	for _, c := range w.Resource.LoadBalancer.Components {
		if LoadBalancerIsRoute(c.Kind) && !c.Deleted && lbDesiredListenerComponent(w, c) {
			found = true
			v, ok := lbComponentObservation(o, c.ID)
			if !ok || !v.Exists || !v.Matches || v.Ready == nil || !*v.Ready {
				return false
			}
		}
	}
	return found
}

func lbDesiredListener(w Work, id string) bool {
	for _, l := range ProjectLoadBalancerListeners(EffectiveLoadBalancerListeners(w.Resource.LoadBalancer.LoadBalancer)) {
		if l.ID == id || (id == "" && len(w.Resource.LoadBalancer.LoadBalancer.Listeners) == 0) {
			return true
		}
	}
	return false
}

// Keep the contract explicit: configuration observations provide no continuous
// application health source. The repository always leaves data_plane unknown.

func lbDesiredListenerComponent(w Work, c LoadBalancerComponent) bool {
	for _, l := range ProjectLoadBalancerListeners(EffectiveLoadBalancerListeners(w.Resource.LoadBalancer.LoadBalancer)) {
		if l.ID == c.ListenerID {
			for _, kind := range LoadBalancerListenerComponentKinds(l) {
				if c.Kind == kind {
					return true
				}
			}
		}
	}
	return false
}

func lbRuntimeMembersEligible(listeners []LoadBalancerListener, observed LoadBalancerObservation) bool {
	for _, l := range listeners {
		for _, desired := range l.Backends {
			found := false
			for _, m := range observed.Members {
				if m.ID == desired.ID {
					if !m.Eligible {
						return false
					}
					found = true
				}
			}
			if !found {
				return false
			}
		}
	}
	return true
}

func lbRuntimeComponentsCovered(work Work, listeners []LoadBalancerListener) bool {
	components := work.Resource.LoadBalancer.Components
	covered := func(kind, listener, member string) bool {
		for _, c := range components {
			if c.Kind == kind && !c.Deleted && c.ListenerID == listener && c.MemberID == member {
				return true
			}
		}
		return false
	}
	if !covered("gateway", "", "") || !covered("envoy_proxy", "", "") {
		return false
	}
	for _, l := range listeners {
		for _, kind := range LoadBalancerListenerComponentKinds(l) {
			if !covered(kind, l.ID, "") {
				return false
			}
		}
		for _, m := range l.Backends {
			if !covered("backend", "", m.ID) {
				return false
			}
		}
	}
	return true
}

func lbMemberEligibleForWorker(o LoadBalancerObservation, id string) bool {
	for _, m := range o.Members {
		if m.ID == id {
			return m.Eligible
		}
	}
	return false
}
