package vm

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// ErrObservationUnavailable describes a temporary source failure without
// evidence contradicting the last observation. Identity conflicts, missing
// objects and expired evidence must remain distinct from this condition.
var ErrObservationUnavailable = errors.New("VM_OBSERVATION_UNAVAILABLE")

// ObservationWork owns a read-only observation lease, independently from the
// lease of any user operation. Generation fences notifications arriving while
// an observation is in flight; Version and Native fence newer VM intent.
type ObservationWork struct {
	TenantID, VMID, Lease string
	LeaseUntil, StartedAt time.Time
	Generation, Version   int64
	Native                ObjectRef
	Closing               bool
}

type ObservationRepository interface {
	ClaimObservation(context.Context, string, time.Duration) (ObservationWork, VM, error)
	SaveObservation(context.Context, ObservationWork, Observation, error) error
}

// ObservationSource exposes only state reads. It cannot mutate native objects,
// prepare networking, or advance a user operation.
type ObservationSource interface {
	ObserveState(context.Context, VM) (Observation, error)
}

// ObservationNotifier wakes only previously registered VMs. Notifications do
// not create resources and cannot bypass the durable failure backoff.
type ObservationNotifier interface {
	NotifyObservation(context.Context, ObjectRef) error
}

// ObservationRescanNotifier is the bounded recovery path after a shared LIST
// or queue overflow. It wakes durable registrations, never creates VM rows.
type ObservationRescanNotifier interface {
	NotifyAllObservations(context.Context) error
}

// ObservationRecovery is a fresh, shared audit proof for one native VM. A
// normal event hint cannot stand in for this proof when releasing backoff.
type ObservationRecovery struct {
	Native     ObjectRef
	ObservedAt time.Time
}

type ObservationRecoveryNotifier interface {
	RecoverObservations(context.Context, []ObservationRecovery) error
}

type Observer struct {
	repo   ObservationRepository
	source ObservationSource
}

func NewObserver(repo ObservationRepository, source ObservationSource) (*Observer, error) {
	if repo == nil || source == nil {
		return nil, ErrInvalid
	}
	return &Observer{repo: repo, source: source}, nil
}

func (o *Observer) Step(ctx context.Context) (bool, error) {
	// Notification retries share this loop but never prevent the VM owner's
	// native state observation from running or alter its result.
	var notified bool
	var notifyErr error
	if dispatcher, ok := o.repo.(interface {
		DispatchNetworkNotification(context.Context) (bool, error)
	}); ok {
		notified, notifyErr = dispatcher.DispatchNetworkNotification(ctx)
	}
	work, v, err := o.repo.ClaimObservation(ctx, uuid.NewString(), 15*time.Second)
	if errors.Is(err, ErrNotFound) {
		return notified, notifyErr
	}
	if err != nil {
		return notified, errors.Join(notifyErr, err)
	}
	readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	observed, readErr := o.source.ObserveState(readCtx, v)
	cancel()
	// Dependency failures belong to this durable observation job. They must not
	// stop the service supervisor or cancel an independent operation worker.
	err = o.repo.SaveObservation(ctx, work, observed, readErr)
	if errors.Is(err, ErrVersion) || errors.Is(err, ErrLease) {
		return true, notifyErr
	}
	return true, errors.Join(notifyErr, err)
}
