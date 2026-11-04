package biz

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/google/uuid"
)

const (
	ProviderUnavailable Reason = "PROVIDER_UNAVAILABLE"
	ProviderNotReady    Reason = "PROVIDER_NOT_READY"
	ProviderRejected    Reason = "PROVIDER_REJECTED"
	ProviderOwnership   Reason = "PROVIDER_OWNERSHIP_CONFLICT"
	ProviderUnknown     Reason = "PROVIDER_RESULT_UNKNOWN"
	ProviderMissing     Reason = "PROVIDER_OBJECT_MISSING"
	CleanupPending      Reason = "CLEANUP_PENDING"
	AddressConflict     Reason = "ADDRESS_CONFLICT"
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
	SubnetOptions *SubnetOptions
	Deleting      bool
	Egress        *EgressWorkSpec
	Requirement   ObservationRequirement
	Direct        bool
	// Tombstone permits reuse of complete shared absence/dependency evidence
	// only after deletion and every uncertain mutation have already completed.
	Tombstone                                            bool
	TenantID, ResourceID, BindingID, KnownIdentity, CIDR string
	SubnetID                                             string
	Kind, VPCID, Gateway                                 string
	RequestedIP, RequestedMAC                            string
}

type ProviderObservation struct {
	RejectionReason                Reason
	VNic                           *VNicFacts
	Generation, ObservedGeneration int64
	SubnetIPRanges                 SubnetIPRanges
	Egress                         *EgressAppliedFacts
	NeedsUpdate                    bool
	Proof                          ObservationProof
	Exists, Ready, HasDependencies bool
	Identity                       string
}

type ResourceProvider interface {
	Observe(context.Context, ProviderTarget) (ProviderObservation, error)
	EnsureVPC(context.Context, ProviderTarget) (ProviderObservation, error)
	EnsureSubnet(context.Context, ProviderTarget) (ProviderObservation, error)
	Delete(context.Context, ProviderTarget) error
}

var ErrLeaseLost = errors.New("resource execution lease lost")

// ErrObservationPending means the local shared observation has not yet covered
// a read. It carries no new Provider fact and must not renew existing evidence.
var ErrObservationPending = errors.New("shared resource observation pending")

// ResourceWork is the immutable resource snapshot used by the shared lifecycle
// worker. Resource kinds are closed at the repository boundary.
type ResourceWork struct {
	SubnetOptions                                             *SubnetOptions
	LoadBalancer                                              *LoadBalancerWork
	BaseRequired                                              bool
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
	DependencyFailed bool
	GateReason       Reason
	CancelUnsent     bool
	Requirement      ObservationRequirement
	Resource         ResourceWork
	Operation        Operation
	ActiveOperation  bool
	BindingID        string
	KnownIdentity    string
	PendingAction    string
	Owner            string
	Epoch            int64
	Attempt          int32
	Now              time.Time
}

type Progress struct {
	VNic                                           *VNicFacts
	ProviderGeneration, ProviderObservedGeneration int64
	// Diagnostic only; never persisted or returned in the public resource model.
	ProviderError  error `json:"-"`
	SubnetIPRanges SubnetIPRanges
	LoadBalancer   *LoadBalancerObservation
	Egress         *EgressAppliedFacts
	Proof          ObservationProof
	Backoff        bool
	State          ResourceState
	OperationState OperationState
	Reason         Reason
	Observed       bool
	Identity       string
	ClearPending   bool
	NextDelay      time.Duration
}

type WorkRepository interface {
	Claim(context.Context, string, time.Duration) (Work, bool, error)
	BeginMutation(context.Context, Work, string, string) error
	Finish(context.Context, Work, Progress) error
}

type WorkerPolicy struct {
	Lease, RequestTimeout, ObserveEvery, StaleAfter, RetryMin, RetryMax time.Duration
}

func DefaultWorkerPolicy() WorkerPolicy {
	return WorkerPolicy{
		Lease: 20 * time.Second, RequestTimeout: 5 * time.Second,
		ObserveEvery: 10 * time.Second, StaleAfter: time.Minute,
		RetryMin: time.Second, RetryMax: time.Minute,
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
		policy.ObserveEvery <= 0 || policy.StaleAfter <= policy.ObserveEvery || policy.RetryMin <= 0 || policy.RetryMax < policy.RetryMin {
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
	// Admission or a fresher observation may fence any completion, including
	// never-dispatched cancellation and dependency gates. The durable task
	// remains recoverable; losing ownership must not terminate the service.
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
	if work.Resource.Kind == "load_balancer" {
		return true, w.stepLoadBalancer(ctx, work)
	}
	progress := Progress{State: work.Resource.State, Identity: work.KnownIdentity, NextDelay: w.policy.ObserveEvery}
	defer func() {
		if w.observer != nil {
			w.observer(ctx, work, progress, stepErr)
		}
	}()
	if work.ActiveOperation {
		progress.OperationState = Retrying
		progress.NextDelay = w.retryDelay(work.Attempt)
	}
	// A durable never-dispatched record is the only shortcut that may retire
	// a child without resolving a parent UID or making a Provider call.
	if work.CancelUnsent {
		progress.State, progress.OperationState, progress.ClearPending = Deleted, Succeeded, true
		progress.Reason = ""
		return true, w.finish(ctx, work, progress)
	}
	if work.DependencyFailed {
		progress.State, progress.OperationState, progress.Reason = Failed, OpFailed, ProviderRejected
		return true, w.finish(ctx, work, progress)
	}
	if work.GateReason != "" {
		progress.Reason = work.GateReason
		progress.Backoff = true
		progress.NextDelay = w.policy.RetryMin
		return true, w.finish(ctx, work, progress)
	}
	// A historical Deleted row without an original identity is an exception
	// for explicit recovery, not a reason to scan every network relation forever.
	if work.Resource.State == Deleted && !work.ActiveOperation && work.PendingAction == "" && work.KnownIdentity == "" {
		progress.Reason, progress.Backoff = ProviderOwnership, true
		return true, w.finish(ctx, work, progress)
	}
	tombstone := work.Resource.State == Deleted && !work.ActiveOperation && work.PendingAction == "" && work.KnownIdentity != ""
	target := ProviderTarget{
		TenantID: work.Resource.TenantID, ResourceID: work.Resource.ID, BindingID: work.BindingID, Egress: work.Resource.Egress,
		KnownIdentity: work.KnownIdentity, CIDR: work.Resource.CIDR,
		Deleting:    work.Resource.State == Deleting || work.Resource.State == Deleted,
		Requirement: work.Requirement, Direct: work.ActiveOperation || work.PendingAction != "" || (work.Resource.State == Deleted && !tombstone), Tombstone: tombstone,
		Kind: work.Resource.Kind, VPCID: work.Resource.VPCID, Gateway: work.Resource.Gateway,
		RequestedIP: work.Resource.RequestedIP, RequestedMAC: work.Resource.RequestedMAC,
		SubnetOptions: work.Resource.SubnetOptions,
	}
	callCtx, cancel := context.WithTimeout(ctx, w.policy.RequestTimeout)
	observation, observeErr := w.provider.Observe(callCtx, target)
	cancel()
	if observeErr != nil {
		progress.ProviderError = observeErr
		progress.Reason = observationReason(observeErr)
		if work.ActiveOperation && progress.Reason == ProviderOwnership {
			progress.OperationState = Blocked
		}
		if work.Resource.State == Available && progress.Reason == ProviderOwnership {
			progress.State = Degraded
		}
	} else if observation.Exists && (observation.Identity == "" || (work.KnownIdentity != "" && observation.Identity != work.KnownIdentity)) {
		progress.Reason, progress.OperationState = ProviderOwnership, Blocked
		if work.Resource.State == Available {
			progress.State = Degraded
		}
	} else {
		progress = applyEgressFacts(progress, observation)
		progress.SubnetIPRanges = observation.SubnetIPRanges
		progress.VNic = observation.VNic
		progress.ProviderGeneration, progress.ProviderObservedGeneration = observation.Generation, observation.ObservedGeneration
		progress.Observed = true
		progress.Proof = observation.Proof
		if progress.Proof.CollectedAt.IsZero() {
			progress.Proof = ObservationProof{CollectedAt: work.Now, CoveredGeneration: work.Requirement.RequestedGeneration}
		}
		progress.Identity = observation.Identity
		if progress.Identity == "" {
			progress.Identity = work.KnownIdentity
		}
		if observation.Exists && work.PendingAction == "create" {
			progress.ClearPending = true
		}
		switch work.Resource.State {
		case Deleting, Deleted:
			progress, err = w.delete(ctx, work, target, observation, progress)
		case Failed:
			progress.Reason = work.Operation.Reason
			// Failed creation is a historical decision; only an explicit DELETE
			// starts cleanup. Observation never converts it into another create.
		default:
			progress, err = w.ensure(ctx, work, target, observation, progress)
		}
	}
	// Ordinary readers can briefly outpace the next shared audit. Preserve the
	// original facts without installing a failure backoff that would also block
	// the successful audit from waking this resource. Mutations and cleanup keep
	// their original retry and proof requirements.
	observationPending := errors.Is(observeErr, ErrObservationPending) && !work.ActiveOperation && work.PendingAction == "" &&
		(work.Resource.State == Available || work.Resource.State == Degraded)
	progress.Backoff = (observeErr != nil && !observationPending) || (work.ActiveOperation && progress.OperationState != Succeeded && progress.OperationState != OpFailed)
	if progress.Backoff {
		progress.NextDelay = w.retryDelay(work.Attempt)
	} else if tombstone && progress.State == Deleted && progress.Reason == "" && progress.Observed && progress.ClearPending {
		// Keep a durable recovery deadline and event wake-ups for retained
		// identities; clean tombstones do not need the ordinary hot poll cadence.
		progress.NextDelay = max(w.policy.ObserveEvery, w.policy.StaleAfter)
	}
	if err == nil {
		err = w.finish(ctx, work, progress)
	}
	if err == nil && work.Resource.Kind == "subnet" && progress.State == Available && progress.Reason == "" {
		if preparer, ok := w.provider.(VMNetworkPreparer); ok {
			// This runs for existing as well as newly created Subnets. The
			// preparer persists its own readiness; a failed NAD must not degrade
			// a healthy ordinary-container network or its completed operation.
			prepareCtx, cancel := context.WithTimeout(ctx, w.policy.RequestTimeout)
			_ = preparer.PrepareVMNetwork(prepareCtx, target)
			cancel()
		}
	}
	return true, err
}

// A service shutdown may cancel a Provider preflight before any write was
// sent. Persist its definitive result with a separate bounded context, or an
// already committed pending marker would lose that result forever. Completion
// still checks the original lease, epoch, resource version and observation
// fence in the repository; this does not permit another Provider request or
// resolve an uncertain mutation from absence.
func (w *Worker) finish(ctx context.Context, work Work, progress Progress) error {
	completionCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), w.policy.RequestTimeout)
	defer cancel()
	return w.repository.Finish(completionCtx, work, progress)
}

func (w *Worker) ensure(ctx context.Context, work Work, target ProviderTarget, observed ProviderObservation, progress Progress) (Progress, error) {
	if !observed.Exists {
		switch {
		case work.PendingAction == "create":
			// A lost POST may still arrive. Absence cannot prove it was rejected;
			// never submit a second POST or permit deletion on this evidence.
			progress.Reason, progress.OperationState = ProviderUnknown, Blocked
			return progress, nil
		case work.KnownIdentity != "" || !work.ActiveOperation:
			progress.Reason, progress.OperationState = ProviderMissing, Blocked
			if work.Resource.State == Available || work.Resource.State == Degraded {
				progress.State = Degraded
			}
			return progress, nil
		}
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
			progress.Observed = false
			progress.Reason = providerReason(err)
			kind := providerKind(err)
			progress.ClearPending = kind != ProviderUncertain
			if kind == ProviderReject {
				progress.State, progress.OperationState = Failed, OpFailed
			} else if kind != ProviderTemporary {
				progress.OperationState = Blocked
			}
			return progress, nil
		}
		if !value.Exists || value.Identity == "" {
			progress.Reason, progress.OperationState = ProviderUnknown, Blocked
			progress.Observed = false
			return progress, nil
		}
		observed = value
		progress = applyEgressFacts(progress, value)
		progress.SubnetIPRanges = value.SubnetIPRanges
		progress.VNic = value.VNic
		progress.ProviderGeneration, progress.ProviderObservedGeneration = value.Generation, value.ObservedGeneration
		if !value.Proof.CollectedAt.IsZero() {
			progress.Proof = value.Proof
		}
		progress.Identity = observed.Identity
		progress.ClearPending = true
	}
	if observed.NeedsUpdate {
		manualCreate := target.Kind == "vnic" && work.Operation.Kind == "create_vnic"
		if manualCreate && work.ActiveOperation && (work.KnownIdentity == "" || work.PendingAction == "create") {
			// Commit the observed original parent UID and resolve its create
			// before a separately fenced child-allocation mutation can begin.
			progress.Reason = ProviderNotReady
			return progress, nil
		}
		if !work.ActiveOperation || (!manualCreate && (work.Operation.Kind != "set_snat_enabled" || target.Kind != "snat")) {
			progress.State, progress.Reason = Degraded, ProviderStateMismatch
			return progress, nil
		}
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
			progress.Observed = false
			progress.Reason = providerReason(err)
			progress.ClearPending = providerKind(err) != ProviderUncertain
			if providerKind(err) == ProviderConflict {
				progress.OperationState = Blocked
			} else if manualCreate && providerKind(err) == ProviderReject {
				progress.State, progress.OperationState = Failed, OpFailed
			}
			return progress, nil
		}
		if !value.Exists || value.Identity != observed.Identity {
			progress.Observed = false
			progress.ClearPending = false
			progress.Reason, progress.OperationState = ProviderUnknown, Blocked
			return progress, nil
		}
		observed = value
		progress = applyEgressFacts(progress, value)
		progress.SubnetIPRanges = value.SubnetIPRanges
		progress.VNic = value.VNic
		progress.ProviderGeneration, progress.ProviderObservedGeneration = value.Generation, value.ObservedGeneration
		if !value.Proof.CollectedAt.IsZero() {
			progress.Proof = value.Proof
		}
		progress.ClearPending = !value.NeedsUpdate
	} else if work.PendingAction == "update" {
		progress.ClearPending = true
	}
	if observed.RejectionReason != "" {
		progress.Reason = observed.RejectionReason
		if work.ActiveOperation {
			progress.State, progress.OperationState = Failed, OpFailed
		} else {
			progress.State = Degraded
		}
	} else if observed.Ready {
		progress.State, progress.Reason = Available, ""
		if work.ActiveOperation {
			progress.OperationState = Succeeded
		}
		progress.NextDelay = w.policy.ObserveEvery
	} else {
		progress.Reason = ProviderNotReady
		if work.Resource.State == Available || work.Resource.State == Degraded {
			progress.State = Degraded
		}
	}
	return progress, nil
}

func (w *Worker) delete(ctx context.Context, work Work, target ProviderTarget, observed ProviderObservation, progress Progress) (Progress, error) {
	if observed.HasDependencies {
		progress.Reason, progress.OperationState = ResourceInUse, Blocked
		return progress, nil
	}
	if !observed.Exists {
		if work.PendingAction == "create" {
			progress.Reason, progress.OperationState = ProviderUnknown, Blocked
			return progress, nil
		}
		progress.State, progress.OperationState, progress.Reason = Deleted, Succeeded, ""
		progress.ClearPending = true
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
		progress.Reason = providerReason(err)
		if providerKind(err) != ProviderTemporary {
			progress.OperationState = Blocked
		}
		return progress, nil
	}
	// DELETE acceptance is not deletion confirmation. A later Observe must
	// see absence, including finalizer completion, before recording deleted.
	progress.Reason = CleanupPending
	progress.NextDelay = w.policy.RetryMin
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
