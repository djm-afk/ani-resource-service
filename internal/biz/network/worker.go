package biz

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	ProviderUnavailable      Reason = "PROVIDER_UNAVAILABLE"
	ProviderNotReady         Reason = "PROVIDER_NOT_READY"
	ProviderStatusUnreported Reason = "PROVIDER_STATUS_UNREPORTED"
	ProviderRejected         Reason = "PROVIDER_REJECTED"
	ProviderOwnership        Reason = "PROVIDER_OWNERSHIP_CONFLICT"
	ProviderUnknown          Reason = "PROVIDER_RESULT_UNKNOWN"
	ProviderMissing          Reason = "PROVIDER_OBJECT_MISSING"
	CleanupPending           Reason = "CLEANUP_PENDING"
	AddressConflict          Reason = "ADDRESS_CONFLICT"
)

type ProviderFailure string

const (
	ProviderTemporary ProviderFailure = "temporary"
	ProviderInUse     ProviderFailure = "in_use"
	ProviderReject    ProviderFailure = "rejected"
	ProviderConflict  ProviderFailure = "conflict"
	ProviderUncertain ProviderFailure = "unknown"
)

type ProviderError struct {
	Kind  ProviderFailure
	Cause error
}

func (e *ProviderError) Error() string { return "network provider: " + string(e.Kind) }
func (e *ProviderError) Unwrap() error { return e.Cause }

// ProviderTarget contains only domain references and an opaque provider object
// identity. The adapter resolves namespace/name/cluster from its persisted map.
type ProviderTarget struct {
	PlatformNetwork                                      *PlatformNetworkWorkSpec
	Reference                                            NetworkReference
	BasicIsolationName                                   string
	SubnetOptions                                        *SubnetOptions
	Deleting                                             bool
	Egress                                               *EgressWorkSpec
	TenantID, ResourceID, BindingID, KnownIdentity, CIDR string
	SubnetID                                             string
	Kind, VPCID, Gateway                                 string
	RequestedIP, RequestedMAC                            string
}

// Exists refers to the original object saved by Res, not any object that later
// reuses its name. Ready and StatusReason are the target's native status.
type ProviderObservation struct {
	PlatformNetwork *PlatformNetworkFacts
	Ready           *bool
	StatusReason    Reason
	VNic            *VNicFacts
	SubnetIPRanges  SubnetIPRanges
	Egress          *EgressAppliedFacts
	NeedsUpdate     bool
	Exists          bool
	Identity        string
}

type ResourceProvider interface {
	Observe(context.Context, ProviderTarget) (ProviderObservation, error)
	EnsureVPC(context.Context, ProviderTarget) (ProviderObservation, error)
	EnsureSubnet(context.Context, ProviderTarget) (ProviderObservation, error)
	Delete(context.Context, ProviderTarget) error
}

var ErrLeaseLost = errors.New("resource execution lease lost")

// ResourceWork is the immutable resource snapshot used by the shared lifecycle
// worker. Resource kinds are closed at the repository boundary.
type ResourceWork struct {
	Reference                                                 NetworkReference
	SubnetOptions                                             *SubnetOptions
	LoadBalancer                                              *LoadBalancerWork
	SystemManaged                                             bool
	Egress                                                    *EgressWorkSpec
	ID, TenantID, Kind, VPCID, CIDR, Gateway, LastOperationID string
	SubnetID                                                  string
	RequestedIP, RequestedMAC                                 string
	State                                                     ResourceState
	Reason                                                    Reason
	Version                                                   int64
	UpdatedAt                                                 time.Time
	ObservedAt                                                *time.Time
}

type Work struct {
	PlatformNetwork *PlatformNetworkWorkSpec
	// CleanupPending only orders deletion of Res-owned child products. It
	// does not prevent reading this target or inspect lower-layer resources.
	CleanupPending    bool
	CancelUnsent      bool
	ClaimedGeneration int64
	Resource          ResourceWork
	Operation         Operation
	ActiveOperation   bool
	BindingID         string
	KnownIdentity     string
	PendingAction     string
	Owner             string
	Epoch             int64
	Attempt           int32
	Now               time.Time
}

type Progress struct {
	PlatformNetwork *PlatformNetworkFacts
	VNic            *VNicFacts
	// Diagnostic only; never persisted or returned in the public resource model.
	ProviderError    error `json:"-"`
	SubnetIPRanges   SubnetIPRanges
	LoadBalancer     *LoadBalancerObservation
	Egress           *EgressAppliedFacts
	Backoff          bool
	State            ResourceState
	OperationState   OperationState
	Reason           Reason
	ObservationError Reason
	OperationReason  Reason
	Observed         bool
	Identity         string
	ClearPending     bool
	NextDelay        time.Duration
}

type WorkRepository interface {
	Claim(context.Context, string, time.Duration) (Work, bool, error)
	BeginMutation(context.Context, Work, string, string) error
	Finish(context.Context, Work, Progress) error
}

type WorkerPolicy struct {
	Lease, RequestTimeout, ObserveEvery, RetryMin, RetryMax time.Duration
}

func DefaultWorkerPolicy() WorkerPolicy {
	return WorkerPolicy{
		Lease: 20 * time.Second, RequestTimeout: 5 * time.Second,
		ObserveEvery: 10 * time.Second,
		RetryMin:     time.Second, RetryMax: time.Minute,
	}
}

type Worker struct {
	repository WorkRepository
	provider   ResourceProvider
	owner      string
	policy     WorkerPolicy
	observer   func(context.Context, Work, Progress, error)
}

func NewWorker(repository WorkRepository, provider ResourceProvider, owner string, policy WorkerPolicy, observers ...func(context.Context, Work, Progress, error)) (*Worker, error) {
	if len(observers) > 1 || repository == nil || provider == nil || policy.RequestTimeout <= 0 || policy.Lease < 3*policy.RequestTimeout ||
		policy.ObserveEvery <= 0 || policy.RetryMin <= 0 || policy.RetryMax < policy.RetryMin {
		return nil, fmt.Errorf("invalid worker dependencies or bounded timing policy")
	}
	parsed, err := uuid.Parse(owner)
	if err != nil || parsed == uuid.Nil {
		return nil, fmt.Errorf("worker owner must be a UUID")
	}
	worker := &Worker{repository: repository, provider: provider, owner: parsed.String(), policy: policy}
	if len(observers) == 1 {
		worker.observer = observers[0]
	}
	return worker, nil
}

// Step advances at most one durable resource. Callers may stop forever after
// this returns; unfinished work and its due time remain in the repository.
func (w *Worker) Step(ctx context.Context) (worked bool, stepErr error) {
	defer func() {
		if errors.Is(stepErr, ErrLeaseLost) {
			stepErr = nil
		}
	}()
	ctx, cancelStep := context.WithTimeout(ctx, w.policy.Lease)
	defer cancelStep()
	work, found, err := w.repository.Claim(ctx, w.owner, w.policy.Lease)
	if err != nil || !found {
		return found, err
	}
	if work.PlatformNetwork != nil {
		return true, w.stepPlatformNetwork(ctx, work)
	}
	if work.Resource.Kind == "load_balancer" {
		return true, w.stepLoadBalancer(ctx, work)
	}
	progress := Progress{State: work.Resource.State, Reason: work.Resource.Reason, Identity: work.KnownIdentity, NextDelay: w.policy.ObserveEvery}
	defer func() {
		if w.observer != nil {
			w.observer(ctx, work, progress, stepErr)
		}
	}()
	if work.ActiveOperation {
		progress.OperationState = Retrying
	}
	// Only a durable record of a never-dispatched declaration permits this
	// local cancellation. An uncertain POST must first be read by name.
	if work.CancelUnsent {
		progress.State, progress.OperationState, progress.ClearPending = Deleted, Succeeded, true
		progress.Reason = ""
		return true, w.finish(ctx, work, progress)
	}
	if work.Resource.State == Deleted && !work.ActiveOperation && work.PendingAction == "" {
		progress.ClearPending = true
		return true, w.finish(ctx, work, progress)
	}
	target := ProviderTarget{Reference: work.Resource.Reference,
		TenantID: work.Resource.TenantID, ResourceID: work.Resource.ID, BindingID: work.BindingID, Egress: work.Resource.Egress,
		KnownIdentity: work.KnownIdentity, CIDR: work.Resource.CIDR,
		Deleting: work.Resource.State == Deleting || work.Resource.State == Deleted,
		Kind:     work.Resource.Kind, VPCID: work.Resource.VPCID, SubnetID: work.Resource.SubnetID, Gateway: work.Resource.Gateway,
		RequestedIP: work.Resource.RequestedIP, RequestedMAC: work.Resource.RequestedMAC,
		SubnetOptions: work.Resource.SubnetOptions,
	}
	callCtx, cancel := context.WithTimeout(ctx, w.policy.RequestTimeout)
	observation, observeErr := w.provider.Observe(callCtx, target)
	cancel()
	if observeErr != nil {
		progress.ProviderError = observeErr
		progress.OperationReason = observationReason(observeErr)
		if providerKind(observeErr) == ProviderConflict {
			// An object-identity conflict is a Res management error, not a
			// new KCN health fact or an API connectivity failure.
			if work.ActiveOperation {
				progress.OperationState = Blocked
			}
		} else {
			progress.ObservationError = observationReason(observeErr)
		}
		progress.Backoff = true
	} else if observation.Exists && (observation.Identity == "" || (work.KnownIdentity != "" && observation.Identity != work.KnownIdentity)) {
		progress.ProviderError = &ProviderError{Kind: ProviderConflict}
		progress.OperationReason = ProviderOwnership
		if work.ActiveOperation {
			progress.OperationState = Blocked
		}
		progress.Backoff = true
	} else {
		progress = observedProgress(progress, observation)
		if observation.Exists && work.PendingAction == "create" {
			progress.ClearPending = true
		}
		if target.Deleting {
			progress, err = w.delete(ctx, work, target, observation, progress)
		} else {
			progress, err = w.ensure(ctx, work, target, observation, progress)
		}
	}
	if progress.Backoff {
		progress.NextDelay = w.retryDelay(work.Attempt)
	}
	if err == nil {
		err = w.finish(ctx, work, progress)
	}
	if err == nil && work.Resource.Kind == "subnet" && progress.State != Deleting && progress.State != Deleted {
		if preparer, ok := w.provider.(VMNetworkPreparer); ok {
			// NAD is a declaration owned by Res. Its result is persisted by
			// that adapter and does not replace the Subnet's native status.
			prepareCtx, cancel := context.WithTimeout(ctx, w.policy.RequestTimeout)
			prepareErr := preparer.PrepareVMNetwork(prepareCtx, target)
			cancel()
			if prepareErr != nil {
				if retry, ok := w.repository.(VMNetworkPreparationRepository); ok {
					retryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), w.policy.RequestTimeout)
					err = retry.RetryVMNetworkPreparation(retryCtx, work, w.retryDelay(work.Attempt))
					cancel()
				} else {
					err = prepareErr
				}
			}
		}
	}
	return true, err
}

// Completion still uses the original resource version and execution lease even
// when service shutdown cancelled the provider call.
func (w *Worker) finish(ctx context.Context, work Work, progress Progress) error {
	completionCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), w.policy.RequestTimeout)
	defer cancel()
	return w.repository.Finish(completionCtx, work, progress)
}

func observedProgress(progress Progress, observed ProviderObservation) Progress {
	progress.Observed = true
	progress.ObservationError = ""
	progress.SubnetIPRanges = observed.SubnetIPRanges
	progress.VNic = observed.VNic
	progress.Egress = observed.Egress
	if observed.Identity != "" {
		progress.Identity = observed.Identity
	}
	return progress
}

func activeCreate(work Work) bool {
	return work.ActiveOperation && (strings.HasPrefix(work.Operation.Kind, "create_") || work.Operation.Kind == "bind_snat")
}

func (w *Worker) ensure(ctx context.Context, work Work, target ProviderTarget, observed ProviderObservation, progress Progress) (Progress, error) {
	if !observed.Exists {
		if work.KnownIdentity != "" || !activeCreate(work) {
			progress.Reason = ProviderMissing
			if work.ActiveOperation {
				progress.OperationState, progress.OperationReason = Blocked, ProviderMissing
			}
			if work.Resource.State == Available || work.Resource.State == Degraded {
				progress.State = Degraded
			}
			return progress, nil
		}
		// The name and binding are fixed. Retrying an unknown same-name POST
		// is idempotent; a saved original UID is never recreated this way.
		if err := w.repository.BeginMutation(ctx, work, "create", ""); err != nil {
			return progress, err
		}
		callCtx, cancel := context.WithTimeout(ctx, w.policy.RequestTimeout)
		var value ProviderObservation
		var err error
		if isEgressKind(target.Kind) {
			value, err = w.mutateEgress(callCtx, target, false)
		} else if target.Kind == "vnic" {
			if provider, ok := w.provider.(VNicProvider); ok {
				value, err = provider.EnsureVNic(callCtx, target)
			} else {
				err = &ProviderError{Kind: ProviderReject}
			}
		} else if target.Kind == "subnet" {
			value, err = w.provider.EnsureSubnet(callCtx, target)
		} else {
			value, err = w.provider.EnsureVPC(callCtx, target)
		}
		cancel()
		if err != nil {
			progress.ProviderError = err
			progress.OperationReason = providerReason(err)
			kind := providerKind(err)
			progress.ClearPending = kind != ProviderUncertain
			progress.Backoff = true
			if kind == ProviderReject {
				// No original object was ever created. Failed describes this
				// Res declaration, not an existing object's network health.
				progress.State, progress.Reason = Failed, ProviderRejected
				progress.OperationState = OpFailed
				progress.Backoff = false
			} else if kind == ProviderConflict || kind == ProviderInUse {
				progress.OperationState = Blocked
			}
			return progress, nil
		}
		if !value.Exists || value.Identity == "" {
			progress.OperationReason = ProviderUnknown
			progress.Backoff = true
			return progress, nil
		}
		observed = value
		progress = observedProgress(progress, value)
		progress.ClearPending = true
	}
	if observed.NeedsUpdate && work.ActiveOperation {
		manualCreate := target.Kind == "vnic" && work.Operation.Kind == "create_vnic"
		if manualCreate && (work.KnownIdentity == "" || work.PendingAction == "create") {
			// Save our parent UID before sending our second RequestedIP
			// declaration under the next lease.
			progress.OperationReason = ProviderNotReady
			return progress, nil
		}
		if manualCreate || (work.Operation.Kind == "set_snat_enabled" && target.Kind == "snat") {
			if err := w.repository.BeginMutation(ctx, work, "update", observed.Identity); err != nil {
				return progress, err
			}
			target.KnownIdentity = observed.Identity
			callCtx, cancel := context.WithTimeout(ctx, w.policy.RequestTimeout)
			var value ProviderObservation
			var err error
			if manualCreate {
				if provider, ok := w.provider.(VNicProvider); ok {
					value, err = provider.EnsureVNic(callCtx, target)
				} else {
					err = &ProviderError{Kind: ProviderReject}
				}
			} else {
				value, err = w.mutateEgress(callCtx, target, true)
			}
			cancel()
			if err != nil {
				progress.ProviderError = err
				progress.OperationReason = providerReason(err)
				kind := providerKind(err)
				progress.ClearPending = kind != ProviderUncertain
				progress.Backoff = true
				if kind == ProviderReject {
					progress.OperationState = OpFailed
					progress.Backoff = false
				} else if kind == ProviderConflict || kind == ProviderInUse {
					progress.OperationState = Blocked
				}
				return progress, nil
			}
			if !value.Exists || value.Identity != observed.Identity {
				progress.ClearPending = false
				progress.OperationReason = ProviderUnknown
				progress.Backoff = true
				return progress, nil
			}
			observed = value
			progress = observedProgress(progress, value)
			progress.ClearPending = !value.NeedsUpdate
		}
	} else if !observed.NeedsUpdate && work.PendingAction == "update" {
		progress.ClearPending = true
	}
	if observed.Ready == nil {
		progress.Reason = observed.StatusReason
		if progress.Reason == "" {
			progress.Reason = ProviderStatusUnreported
		}
		if work.ActiveOperation {
			progress.OperationReason = progress.Reason
		}
		return progress, nil
	}
	if *observed.Ready {
		progress.State, progress.Reason = Available, ""
		if work.ActiveOperation {
			if !observed.NeedsUpdate {
				progress.OperationState, progress.OperationReason = Succeeded, ""
			} else {
				progress.OperationReason = ProviderNotReady
			}
		}
	} else {
		progress.State = Provisioning
		if work.Resource.State == Available || work.Resource.State == Degraded {
			progress.State = Degraded
		}
		progress.Reason = observed.StatusReason
		if progress.Reason == "" {
			progress.Reason = ProviderNotReady
		}
		if work.ActiveOperation {
			progress.OperationReason = progress.Reason
		}
	}
	return progress, nil
}

func (w *Worker) delete(ctx context.Context, work Work, target ProviderTarget, observed ProviderObservation, progress Progress) (Progress, error) {
	if !observed.Exists {
		progress.State, progress.Reason, progress.ClearPending = Deleted, "", true
		if work.ActiveOperation {
			progress.OperationState, progress.OperationReason = Succeeded, ""
		}
		return progress, nil
	}
	if work.CleanupPending {
		progress.Reason, progress.OperationReason = CleanupPending, CleanupPending
		return progress, nil
	}
	target.KnownIdentity = observed.Identity
	if err := w.repository.BeginMutation(ctx, work, "delete", observed.Identity); err != nil {
		return progress, err
	}
	callCtx, cancel := context.WithTimeout(ctx, w.policy.RequestTimeout)
	err := w.provider.Delete(callCtx, target)
	cancel()
	progress.ClearPending = false
	if err != nil {
		progress.ProviderError = err
		progress.OperationReason = providerReason(err)
		kind := providerKind(err)
		progress.ClearPending = kind != ProviderUncertain
		progress.Backoff = true
		if work.ActiveOperation && (kind == ProviderReject || kind == ProviderConflict || kind == ProviderInUse) {
			// Deletion remains the current desired command until the original
			// object is gone; its existing operation contract has no failed terminal.
			progress.OperationState = Blocked
		}
		return progress, nil
	}
	// DELETE acknowledgement is not absence. Observe this original target
	// again, letting its controller finish its normal finalizers.
	progress.Reason, progress.OperationReason = CleanupPending, CleanupPending
	return progress, nil
}

func providerKind(err error) ProviderFailure {
	var failure *ProviderError
	if errors.As(err, &failure) {
		return failure.Kind
	}
	return ProviderUncertain
}

func providerReason(err error) Reason {
	switch providerKind(err) {
	case ProviderTemporary:
		return ProviderUnavailable
	case ProviderReject:
		return ProviderRejected
	case ProviderConflict:
		return ProviderOwnership
	case ProviderInUse:
		return ResourceInUse
	default:
		return ProviderUnknown
	}
}

// An unsuccessful read carries no new resource-health fact. In particular,
// an observation timeout is not an unknown mutation result.
func observationReason(err error) Reason {
	if providerKind(err) == ProviderConflict {
		return ProviderOwnership
	}
	return ProviderUnavailable
}

func (w *Worker) retryDelay(attempt int32) time.Duration {
	delay := min(w.policy.RetryMax, w.policy.RetryMin*time.Duration(1<<min(max(attempt-1, 0), 16)))
	return min(w.policy.RetryMax, time.Duration(float64(delay)*(0.8+0.4*rand.Float64())))
}
