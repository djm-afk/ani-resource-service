package biz

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
)

type AttachmentWork struct {
	ConsumerSnapshot  *ConsumerSubmission
	ClaimedGeneration int64
	Now               time.Time
	Attachment        Attachment
	Plan              PodPrimaryPlan
	Owner             string
	Epoch             int64
}

type AttachmentAddressObservation struct {
	CurrentVNicName, CurrentVNicUID, CurrentMACAddress string
	CurrentIPs                                         []string
}

// AttachmentAddressReader reads display data for the current registered
// consumer. On a read error it returns the last values for the unchanged source;
// it never determines whether the management relationship is attached/released.
type AttachmentAddressReader interface {
	ObserveAttachmentAddresses(context.Context, Attachment, ConsumerSubmission) (AttachmentAddressObservation, error)
}

type AttachmentProgress struct {
	ConsumerSnapshot                *ConsumerSubmission
	Addresses                       *AttachmentAddressObservation
	State                           AttachmentState
	Reason, ObservationError        Reason
	ProviderError                   error
	PodName, PodUID, FinalizationID string
	Observed, Backoff               bool
	NextDelay                       time.Duration
}

type AttachmentWorkRepository interface {
	ClaimAttachment(context.Context, string, time.Duration) (AttachmentWork, bool, error)
	FinishAttachment(context.Context, AttachmentWork, AttachmentProgress) error
}

type ManualVNicReference struct{ VNicID, ProviderName, ProviderUID, BindingID string }

type ConsumerSubmission struct {
	ManualVNics                                                            []ManualVNicReference `json:",omitempty"`
	SnapshotRevision                                                       int64
	DesiredRunState                                                        string
	CurrentPods                                                            []PodReference
	ProtocolVersion                                                        int32
	TenantID, InstanceID, SubmissionID, AttachmentID, ClusterID, Namespace string
	Generation                                                             int64
	State, FinalizationID                                                  string
	PodUIDs                                                                []string
	ClosedAt                                                               *time.Time
}

type InstanceConsumer interface {
	GetSubmission(context.Context, Attachment) (ConsumerSubmission, error)
}

type AttachmentWorker struct {
	repository AttachmentWorkRepository
	consumer   InstanceConsumer
	addresses  AttachmentAddressReader
	owner      string
	policy     WorkerPolicy
	observer   func(context.Context, AttachmentWork, AttachmentProgress, error)
}

func NewAttachmentWorker(r AttachmentWorkRepository, c InstanceConsumer, addresses AttachmentAddressReader, owner string, policy WorkerPolicy, observers ...func(context.Context, AttachmentWork, AttachmentProgress, error)) (*AttachmentWorker, error) {
	id, err := uuid.Parse(owner)
	if len(observers) > 1 || err != nil || id == uuid.Nil || r == nil || c == nil || addresses == nil || policy.RequestTimeout <= 0 || policy.Lease < 3*policy.RequestTimeout || policy.ObserveEvery <= 0 {
		return nil, fmt.Errorf("invalid attachment worker dependencies")
	}
	worker := &AttachmentWorker{repository: r, consumer: c, addresses: addresses, owner: owner, policy: policy}
	if len(observers) == 1 {
		worker.observer = observers[0]
	}
	return worker, nil
}

func (w *AttachmentWorker) Step(ctx context.Context) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, w.policy.Lease)
	defer cancel()
	work, found, err := w.repository.ClaimAttachment(ctx, w.owner, w.policy.Lease)
	if err != nil || !found {
		return found, err
	}
	a := work.Attachment
	progress := AttachmentProgress{State: a.State, Reason: a.Reason, PodName: a.PodName, PodUID: a.PodUID, FinalizationID: a.FinalizationID, NextDelay: w.policy.ObserveEvery}
	if a.State == Released {
		return w.finish(ctx, work, progress)
	}
	call, stop := context.WithTimeout(ctx, w.policy.RequestTimeout)
	consumer, err := w.consumer.GetSubmission(call, a)
	stop()
	if err != nil {
		progress.ObservationError, progress.ProviderError, progress.Backoff = ConsumerUnavailable, err, true
		return w.finish(ctx, work, progress)
	}
	if !validConsumer(a, consumer) {
		progress.Reason, progress.Backoff = AttachmentProtocol, true
		return w.finish(ctx, work, progress)
	}
	if a.WorkloadMode == "controller_managed" || a.WorkloadMode == "vm_managed" {
		switch CompareConsumerSnapshot(a, consumer) {
		case SnapshotStale:
			progress.Reason, progress.Backoff = VersionConflict, true
			return w.finish(ctx, work, progress)
		case SnapshotConflict:
			progress.Reason, progress.Backoff = AttachmentProtocol, true
			return w.finish(ctx, work, progress)
		}
		progress.ConsumerSnapshot = &consumer
	}
	progress.Observed, progress.Reason = true, ""
	progress.PodName, progress.PodUID = "", ""
	switch consumer.State {
	case "closing", "closed":
		progress.State, progress.FinalizationID = Releasing, consumer.FinalizationID
		if consumer.State == "closed" {
			progress.State = Released
		}
	case "open":
		if a.State == Releasing {
			progress.State, progress.Reason = Releasing, ConsumerUnclosed
			break
		}
		progress.State = Reserved
		if consumer.DesiredRunState == "stopped" || consumer.DesiredRunState == "deleted" {
			break
		}
		if len(consumer.CurrentPods) == 1 {
			pod := consumer.CurrentPods[0].Reference
			progress.PodName, progress.PodUID, progress.State = pod.Name, pod.UID, Attached
		} else if (a.WorkloadMode == "" || a.WorkloadMode == "single_pod") && a.PodName != "" && a.PodUID != "" && slices.Contains(consumer.PodUIDs, a.PodUID) {
			progress.PodName, progress.PodUID, progress.State = a.PodName, a.PodUID, Attached
		}
	}
	if progress.State != Attached {
		progress.Addresses = &AttachmentAddressObservation{CurrentIPs: []string{}}
		return w.finish(ctx, work, progress)
	}
	current := a
	current.State, current.PodName, current.PodUID = progress.State, progress.PodName, progress.PodUID
	current.ConsumerSnapshot = &consumer
	if current.PodName != a.PodName || current.PodUID != a.PodUID {
		current.CurrentVNicName, current.CurrentVNicUID, current.CurrentMACAddress = "", "", ""
		current.CurrentIPs = nil
	}
	call, stop = context.WithTimeout(ctx, w.policy.RequestTimeout)
	addresses, addressErr := w.addresses.ObserveAttachmentAddresses(call, current, consumer)
	stop()
	progress.Addresses = &addresses
	if addressErr != nil {
		progress.ObservationError, progress.ProviderError, progress.Backoff = ProviderUnavailable, addressErr, true
	}
	return w.finish(ctx, work, progress)
}

func (w *AttachmentWorker) finish(ctx context.Context, work AttachmentWork, progress AttachmentProgress) (bool, error) {
	err := w.repository.FinishAttachment(ctx, work, progress)
	if w.observer != nil {
		w.observer(ctx, work, progress, err)
	}
	if errors.Is(err, ErrLeaseLost) {
		err = nil
	}
	return true, err
}

func validConsumer(a Attachment, c ConsumerSubmission) bool {
	if a.WorkloadMode == "controller_managed" {
		return validManagedConsumer(a, c)
	}
	if a.WorkloadMode == "vm_managed" {
		return validVMConsumer(a, c)
	}
	if c.ProtocolVersion != 1 || !consumerScopeMatches(a, c) || len(c.PodUIDs) > 1 {
		return false
	}
	for _, uid := range c.PodUIDs {
		if uid == "" {
			return false
		}
	}
	return validConsumerState(a, c)
}
