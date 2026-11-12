package biz

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
)

type VirtualSwitchWorker struct {
	repository VirtualSwitchWorkRepository
	provider   VirtualSwitchGroupProvider
	owner      string
	policy     WorkerPolicy
	observer   func(context.Context, VirtualSwitchWork, VirtualSwitchProgress, error)
	logger     *slog.Logger
}

// VirtualSwitchProgress describes one attempt without treating retained healthy
// state during a temporary read failure as a successful observation.
type VirtualSwitchProgress struct {
	ProviderError                             error `json:"-"`
	BindingState                              string
	Reason, ObservationError, OperationReason Reason
	Action                                    string
	NodeName                                  string
	NodeUID                                   string
	Observed                                  bool
	Backoff                                   bool
}

func NewVirtualSwitchWorker(r VirtualSwitchWorkRepository, p VirtualSwitchGroupProvider, owner string, policy WorkerPolicy, observers ...func(context.Context, VirtualSwitchWork, VirtualSwitchProgress, error)) (*VirtualSwitchWorker, error) {
	id, err := uuid.Parse(owner)
	if len(observers) > 1 || r == nil || p == nil || err != nil || id == uuid.Nil || policy.RequestTimeout <= 0 || policy.Lease < 3*policy.RequestTimeout || policy.ObserveEvery <= 0 {
		return nil, Fail(InvalidArgument, "invalid switch worker dependencies or policy")
	}
	w := &VirtualSwitchWorker{repository: r, provider: p, owner: id.String(), policy: policy}
	if len(observers) == 1 {
		w.observer = observers[0]
	}
	return w, nil
}

// SetLogger attaches the structured logger for telemetry-context events.
func (w *VirtualSwitchWorker) SetLogger(logger *slog.Logger) { w.logger = logger }

func (w *VirtualSwitchWorker) Step(ctx context.Context) (worked bool, err error) {
	defer func() {
		if errors.Is(err, ErrLeaseLost) {
			err = nil
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, w.policy.Lease)
	defer cancel()
	work, found, err := w.repository.ClaimVirtualSwitch(ctx, w.owner, w.policy.Lease)
	if err != nil {
		dependencyFailed(ctx, w.logger, WorkerVirtualSwitch, "postgresql", "claim", nil, k8sTarget{}, err)
		return found, err
	}
	if !found {
		return false, nil
	}
	// Resume the accepted trace only while its operation is unfinished; a
	// terminal record is an independent observation with its own trace.
	ctx, runSpan, telemetryEvent, telemetryErr := activeRun(ctx, work.ActiveOperation, work.TelemetryContext, work.TelemetryContextError, "network.virtual_switch")
	defer runSpan.End()
	identity := work.eventIdentity()
	runStarted(ctx, w.logger, WorkerVirtualSwitch, identity)
	startedAt := time.Now()
	defer func() {
		if err == nil {
			runCompleted(ctx, w.logger, WorkerVirtualSwitch, identity, time.Since(startedAt))
		}
	}()
	if telemetryEvent != "" {
		logTelemetryEvent(ctx, w.logger, telemetryEvent, []any{"resource_type", "virtual_switch", "resource_id", work.Switch.ID, "cluster_id", work.Switch.ClusterID}, telemetryErr)
	}
	if work.ActiveOperation {
		networkEvent(ctx, w.logger, slog.LevelInfo, EventOperationStarted, append([]any{
			"operation_id", work.Switch.LastOperationID, "action", work.OperationKind,
			"resource_type", "virtual_switch", "resource_id", work.Switch.ID, "cluster_id", work.Switch.ClusterID}, identity...)...)
	}
	sw := work.Switch
	sw.Nodes = append([]VirtualSwitchNode(nil), sw.Nodes...)
	deleting := sw.State == Deleting
	progress := VirtualSwitchProgress{BindingState: sw.BindingState, Reason: sw.Reason}
	defer func() {
		if w.observer != nil {
			w.observer(ctx, work, progress, err)
		}
	}()
	finish := func(state string, reason Reason, delay time.Duration) (bool, error) {
		progress.BindingState, progress.Reason = state, reason
		stepStarted(ctx, w.logger, WorkerVirtualSwitch, StepPersistSwitch, identity)
		finishErr := w.repository.FinishVirtualSwitch(ctx, work, progress, delay)
		switch {
		case errors.Is(finishErr, ErrLeaseLost):
			operationLeaseLost(ctx, w.logger, WorkerVirtualSwitch, StepPersistSwitch, identity)
		case finishErr != nil:
			persistFailed(ctx, w.logger, WorkerVirtualSwitch, "finish_virtual_switch", StepPersistSwitch, identity, finishErr)
		default:
			stepSucceeded(ctx, w.logger, WorkerVirtualSwitch, StepPersistSwitch, identity)
		}
		return true, finishErr
	}
	for index, node := range sw.Nodes {
		progress.Action, progress.NodeName, progress.NodeUID = "inspect", node.NodeName, node.NodeUID
		stepStarted(ctx, w.logger, WorkerVirtualSwitch, StepObserveSwitch, identity)
		call, cancel := context.WithTimeout(ctx, w.policy.RequestTimeout)
		current, inspectErr := w.provider.InspectVirtualSwitchNode(call, sw, node)
		cancel()
		if inspectErr != nil {
			dependencyFailed(ctx, w.logger, WorkerVirtualSwitch, "kubernetes", "inspect", identity,
				k8sTarget{clusterID: sw.ClusterID, kind: "Node", name: node.NodeName, uid: node.NodeUID}, inspectErr)
			progress.ProviderError, progress.OperationReason = inspectErr, observationReason(inspectErr)
			if providerKind(inspectErr) != ProviderConflict {
				progress.ObservationError = observationReason(inspectErr)
			} else {
				operationConflict(ctx, w.logger, WorkerVirtualSwitch, StepObserveSwitch, identity, observationReason(inspectErr), nil)
			}
			progress.Backoff = true
			return finish(work.Switch.BindingState, work.Switch.Reason, w.policy.RetryMin)
		}
		stepSucceeded(ctx, w.logger, WorkerVirtualSwitch, StepObserveSwitch, identity)
		if (!deleting && current.Applied) || (deleting && current.State == "cleaned") {
			stepStarted(ctx, w.logger, WorkerVirtualSwitch, StepApplySwitch, identity)
			if err = w.repository.SaveVirtualSwitchNode(ctx, work, index, current); err != nil {
				reportSwitchNodeFailure(ctx, w.logger, identity, err)
				return true, err
			}
			stepSucceeded(ctx, w.logger, WorkerVirtualSwitch, StepApplySwitch, identity)
			sw.Nodes[index] = current
			continue
		}
		// The exact precondition and intended owner are durable before dispatch.
		current.Prepared = true
		current.Unknown = true
		stepStarted(ctx, w.logger, WorkerVirtualSwitch, StepApplySwitch, identity)
		if err = w.repository.SaveVirtualSwitchNode(ctx, work, index, current); err != nil {
			reportSwitchNodeFailure(ctx, w.logger, identity, err)
			return true, err
		}
		call, cancel = context.WithTimeout(ctx, w.policy.RequestTimeout)
		progress.Action = "patch"
		applied, patchErr := w.provider.PatchVirtualSwitchNode(call, sw, current, deleting)
		cancel()
		if patchErr != nil {
			dependencyFailed(ctx, w.logger, WorkerVirtualSwitch, "kubernetes", "patch", identity,
				k8sTarget{clusterID: sw.ClusterID, kind: "Node", name: node.NodeName, uid: node.NodeUID}, patchErr)
			progress.ProviderError, progress.OperationReason = patchErr, providerReason(patchErr)
			if providerKind(patchErr) == ProviderConflict {
				operationConflict(ctx, w.logger, WorkerVirtualSwitch, StepApplySwitch, identity, providerReason(patchErr), nil)
			}
			progress.Backoff = true
			return finish(work.Switch.BindingState, work.Switch.Reason, w.policy.RetryMin)
		}
		if err = w.repository.SaveVirtualSwitchNode(ctx, work, index, applied); err != nil {
			reportSwitchNodeFailure(ctx, w.logger, identity, err)
			return true, err
		}
		stepSucceeded(ctx, w.logger, WorkerVirtualSwitch, StepApplySwitch, identity)
		progress.Observed = true
		state := "pending"
		if deleting {
			state = "cleaning"
		}
		return finish(state, "", w.policy.RetryMin)
	}
	progress.Observed = true
	if deleting {
		return finish("cleaned", "", w.policy.ObserveEvery)
	}
	progress.Observed = true
	progress.Action, progress.NodeName, progress.NodeUID = "", "", ""
	return finish("configured", "", w.policy.ObserveEvery)
}

// reportSwitchNodeFailure separates a lost lease from a real persistence error
// so the two never share a level.
func reportSwitchNodeFailure(ctx context.Context, logger *slog.Logger, identity []any, err error) {
	if errors.Is(err, ErrLeaseLost) {
		operationLeaseLost(ctx, logger, WorkerVirtualSwitch, StepApplySwitch, identity)
		return
	}
	persistFailed(ctx, logger, WorkerVirtualSwitch, "save_virtual_switch_node", StepApplySwitch, identity, err)
}
