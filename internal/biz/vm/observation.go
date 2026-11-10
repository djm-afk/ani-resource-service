package vm

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/telemetry"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/telemetry/logfields"
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

// DiskPreparationSource is an optional read on the existing observation lane.
// Partial successful reads may advance inventory; its error is independent of
// runtime state and never becomes a user operation result.
type DiskPreparationSource interface {
	ObserveDiskPreparation(context.Context, VM) ([]Disk, error)
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

type Observer struct {
	repo   ObservationRepository
	source ObservationSource
	logger *slog.Logger
}

func NewObserver(repo ObservationRepository, source ObservationSource, logger *slog.Logger) (*Observer, error) {
	if repo == nil || source == nil {
		return nil, ErrInvalid
	}
	return &Observer{repo: repo, source: source, logger: logger}, nil
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
	var inventoryErr error
	if readErr == nil {
		if inventory, ok := o.source.(DiskPreparationSource); ok {
			inventoryCtx, stop := context.WithTimeout(ctx, 2*time.Second)
			observed.Volumes, inventoryErr = inventory.ObserveDiskPreparation(inventoryCtx, v)
			stop()
		}
	}
	// Record each real failure at its own point with the existing identity: the
	// state read, the disk inventory read, and the observation save are distinct.
	if readErr != nil {
		o.logObserveFailed(ctx, v, "observe_state", readErr)
	} else if inventoryErr != nil {
		o.logObserveFailed(ctx, v, "observe_disk_preparation", inventoryErr)
	}
	// Dependency failures belong to this durable observation job. They must not
	// stop the service supervisor or cancel an independent operation worker.
	err = o.repo.SaveObservation(ctx, work, observed, readErr)
	if errors.Is(err, ErrVersion) || errors.Is(err, ErrLease) {
		return true, notifyErr
	}
	if err != nil {
		o.logObserveFailed(ctx, v, "persist_observation", err)
	}
	return true, errors.Join(notifyErr, inventoryErr, err)
}

// logObserveFailed records a real observation failure. The observer is an
// independent periodic lane with its own run trace, so no operation_id is
// fabricated; resource_type/resource_id come from the observed VM.
func (o *Observer) logObserveFailed(ctx context.Context, v VM, step string, err error) {
	if o.logger == nil || err == nil {
		return
	}
	attrs := []any{"event", "step.failed", "step", step, "result", "failed",
		"resource_type", "vm", "resource_id", v.ID}
	if v.TenantID != "" {
		attrs = append(attrs, "tenant_id", v.TenantID)
	}
	if v.Native.Namespace != "" {
		attrs = append(attrs, telemetry.Target{
			ClusterID: v.Spec.ClusterID, Kind: v.Native.Kind, Namespace: v.Native.Namespace,
			Name: v.Native.Name, UID: v.Native.UID,
		}.KV()...)
	}
	attrs = append(attrs, logfields.ErrorFields(err)...)
	o.logger.ErrorContext(ctx, "VM observation failed", attrs...)
}
