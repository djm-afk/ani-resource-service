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
	PodName, PodUID, VNicName, VNicUID, VNicIPName, VNicIPUID string
}
type LoadBalancerWork struct {
	VIPOccupiedRevision, VIPAbsenceRevision string
	LoadBalancer                            LoadBalancer
	Members                                 []LoadBalancerMemberIdentity
	Components                              []LoadBalancerComponent
}
type LoadBalancerComponentObservation struct {
	Conflict                         bool
	ID, Identity                     string
	Exists, Matches, Ready, Deleting bool
	ClearPending, Deleted            bool
	AppliedVersion                   int64
}
type LoadBalancerMemberObservation struct {
	ID       string
	Eligible bool
	Reason   Reason
}

// Generated resources are observed identities, not specifications owned by
// Network. The adapter proves their entire owner chain before returning them.
type LoadBalancerGeneratedResource struct {
	Kind, Namespace, Name, Identity, GatewayIdentity string
	Released                                         bool
}
type LoadBalancerObservation struct {
	GeneratedConflict                                                     bool
	VIPOccupiedRevision, VIPAbsenceRevision                               string
	Proof                                                                 ObservationProof
	Components                                                            []LoadBalancerComponentObservation
	Members                                                               []LoadBalancerMemberObservation
	Generated                                                             []LoadBalancerGeneratedResource
	DependenciesReady, GeneratedReady, GeneratedReleased, AddressReleased bool
	ConfigurationState                                                    string
	AppliedVersion                                                        int64
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
	p := Progress{State: work.Resource.State, Identity: work.KnownIdentity, NextDelay: w.policy.ObserveEvery, OperationState: Retrying}
	// The repository establishes this proof under the resource/lease fence.
	// Target zero alone never authorizes bypassing unknown CREATE recovery.
	if work.CancelUnsent {
		p.State, p.OperationState, p.ClearPending = Deleted, Succeeded, true
		return lbLeaseResult(w.finish(ctx, work, p))
	}
	if work.GateReason != "" {
		p.Reason, p.OperationState, p.Backoff = work.GateReason, Blocked, true
		p.NextDelay = w.retryDelay(work.Attempt)
		return lbLeaseResult(w.finish(ctx, work, p))
	}
	callCtx, cancel := context.WithTimeout(ctx, w.policy.RequestTimeout)
	o, err := provider.ObserveLoadBalancer(callCtx, work)
	cancel()
	if err != nil {
		p.ProviderError = err
		p.Reason = observationReason(err)
		p.OperationState, p.Backoff = Blocked, true
		if p.State == Available && p.Reason == ProviderOwnership {
			p.State = Degraded
		}
	} else {
		p.Observed, p.Proof, p.LoadBalancer = true, o.Proof, &o
		o.ConfigurationState = "applying"
		deleting := p.State == Deleting || p.State == Deleted
		runtime := ProjectLoadBalancerListeners(EffectiveLoadBalancerListeners(work.Resource.LoadBalancer.LoadBalancer))
		draining := !deleting && len(runtime) == 0
		removing := deleting || draining
		allEligible := lbRuntimeMembersEligible(runtime, o)
		// A changed/missing backend must first be removed from the Route.
		// This safety update also runs during ordinary continuous observation.
		order := []string{"retired_route", "retired_policy", "backend", "envoy_proxy", "gateway", "policy", "route"}
		if !allEligible {
			order = []string{"retired_route", "retired_policy", "route", "backend", "envoy_proxy", "gateway", "policy"}
		}
		if removing {
			order = []string{"route", "policy", "gateway", "backend", "envoy_proxy"}
		}
		if o.GeneratedConflict {
			// A foreign generated occupant blocks Gateway/address cleanup, but
			// must not prevent withdrawing an invalid IP from our owned Route.
			order = nil
			if removing {
				order = []string{"route", "policy"}
			} else if !allEligible {
				order = []string{"route"}
			}
		}
		allReady, acted := !o.GeneratedConflict, false
		for _, kind := range order {
			if (kind == "backend" || kind == "envoy_proxy") && removing && (!o.GeneratedReleased || !o.AddressReleased) {
				allReady, p.Reason = false, CleanupPending
				break
			}
			for _, c := range work.Resource.LoadBalancer.Components {
				retiredListener := c.ListenerID != "" && !lbDesiredListenerComponent(work, c)
				selectedKind := c.Kind
				if LoadBalancerIsRoute(c.Kind) {
					selectedKind = "route"
				} else if c.Kind == "client_policy" || c.Kind == "patch_policy" {
					selectedKind = "policy"
				}
				if retiredListener && !removing {
					selectedKind = "retired_" + selectedKind
				}
				if selectedKind != kind {
					continue
				}
				v, exists := lbComponentObservation(o, c.ID)
				if !exists {
					allReady, p.Reason = false, ProviderUnknown
					break
				}
				if v.Conflict {
					allReady, p.Reason = false, ProviderOwnership
					break
				}
				retired := c.Deleted || retiredListener || (c.Kind == "backend" && !lbDesiredMember(work, c.MemberID))
				remove := removing || retired
				if retired && c.Kind == "backend" && !removing && (!lbRouteCurrent(work, o) || !o.GeneratedReady) {
					allReady = false
					continue
				}
				if v.Exists && (v.Identity == "" || (c.Identity != "" && c.Identity != v.Identity)) {
					allReady, p.Reason = false, ProviderOwnership
					break
				}
				if !v.Exists && c.PendingAction == "create" {
					allReady, p.Reason = false, ProviderUnknown
					break
				}
				action := ""
				if remove {
					if !v.Exists {
						v.Deleted, v.ClearPending = true, true
						lbReplaceObservation(&o, v)
						continue
					}
					allReady = false
					if !v.Deleting {
						action = "delete"
					} else {
						p.Reason = CleanupPending
					}
				} else if !v.Exists {
					allReady = false
					if c.Identity != "" || c.Deleted || !work.ActiveOperation {
						p.Reason = ProviderMissing
						break
					}
					if !o.DependenciesReady || !allEligible {
						p.Reason = ProviderNotReady
						continue
					}
					action = "create"
				} else if !v.Matches {
					allReady = false
					if c.Kind == "backend" {
						p.Reason = ProviderOwnership
						break
					}
					// With a lost update, an authoritative read of the old spec
					// cannot prove the pending request was rejected. A new CAS
					// update is safe: only one request at the observed RV can win.
					action = "update"
				} else {
					v.ClearPending = c.PendingAction != "delete"
					if v.Ready {
						v.AppliedVersion = work.Resource.LoadBalancer.LoadBalancer.ProviderTargetVersion
					}
					lbReplaceObservation(&o, v)
					if !v.Ready {
						allReady = false
						p.Reason = ProviderNotReady
					}
				}
				if action != "" {
					if err = repository.BeginLoadBalancerMutation(ctx, work, c, action, v.Identity); err != nil {
						return lbLeaseResult(err)
					}
					if v.Identity != "" {
						c.Identity = v.Identity
					}
					callCtx, cancel = context.WithTimeout(ctx, w.policy.RequestTimeout)
					result, mutationErr := provider.MutateLoadBalancer(callCtx, work, c, action, o)
					cancel()
					if mutationErr != nil {
						p.Reason, p.Backoff = providerReason(mutationErr), true
						v.ClearPending = providerKind(mutationErr) != ProviderUncertain
						lbReplaceObservation(&o, v)
					} else {
						result.ClearPending = action != "delete"
						lbReplaceObservation(&o, result)
						p.Reason = ProviderNotReady
					}
					// Mutations are not observations of the completed configuration.
					allReady, acted = false, true
					p.NextDelay = w.policy.RetryMin
					break
				}
			}
			if acted || p.Reason == ProviderUnknown || p.Reason == ProviderOwnership || (removing && !allReady) {
				break
			}
		}
		if o.GeneratedConflict {
			p.Reason, p.OperationState = ProviderOwnership, Blocked
		}
		if deleting {
			if allReady && o.GeneratedReleased && o.AddressReleased {
				p.State, p.OperationState, p.Reason = Deleted, Succeeded, ""
			}
		} else if draining {
			if allReady && o.GeneratedReleased && o.AddressReleased {
				p.State, p.OperationState, p.Reason = Provisioning, Succeeded, ""
				o.ConfigurationState, o.AppliedVersion = "waiting_for_backends", work.Resource.LoadBalancer.LoadBalancer.ProviderTargetVersion
			} else if p.Reason == "" {
				p.Reason = CleanupPending
			}
		} else if allReady && allEligible && lbRuntimeComponentsCovered(work, runtime) && o.DependenciesReady && o.GeneratedReady {
			p.State, p.OperationState, p.Reason = Available, Succeeded, ""
			o.ConfigurationState, o.AppliedVersion = "configured", work.Resource.LoadBalancer.LoadBalancer.ProviderTargetVersion
		} else if work.Resource.LoadBalancer.LoadBalancer.AppliedVersion > 0 {
			p.State, o.ConfigurationState = Degraded, "degraded"
		}
		if !removing && p.State != Available && p.Reason == "" {
			p.Reason = ProviderNotReady
		}
		if !allEligible && !removing && p.Reason != ProviderUnknown && p.Reason != ProviderOwnership {
			p.Reason = BackendIdentityMismatch
		}
		for _, v := range o.Components {
			if v.ID == work.BindingID {
				if v.Identity != "" {
					p.Identity = v.Identity
				}
				p.ClearPending = v.ClearPending
			}
		}
		p.LoadBalancer = &o
	}
	if p.Reason == ProviderUnknown || p.Reason == ProviderOwnership || p.Reason == CleanupPending {
		p.OperationState = Blocked
	}
	if p.Reason != "" && !p.Backoff {
		p.Backoff = work.ActiveOperation && p.NextDelay != w.policy.RetryMin
	}
	if p.Backoff {
		p.NextDelay = w.retryDelay(work.Attempt)
	}
	err = w.finish(ctx, work, p)
	if w.observer != nil {
		w.observer(ctx, work, p, err)
	}
	return lbLeaseResult(err)
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
			if !ok || !v.Exists || !v.Matches || !v.Ready {
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
