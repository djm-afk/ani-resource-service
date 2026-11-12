package vm

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/zhangzhe-ctrl/ani-resource-service/internal/telemetry"
)

type Worker struct {
	repo           Repository
	native         NativePort
	network        NetworkPort
	securityGroups SecurityGroupPort
	logger         *slog.Logger
}

func NewWorker(r Repository, n NativePort, network NetworkPort, logger *slog.Logger) (*Worker, error) {
	if r == nil || n == nil || network == nil {
		return nil, ErrInvalid
	}
	return &Worker{repo: r, native: n, network: network, logger: logger}, nil
}
func (w *Worker) RunOnce(ctx context.Context) error {
	o, v, e := w.repo.Claim(ctx, uuid.NewString(), 30*time.Second)
	if e != nil {
		return e
	}
	// Restore the accepted request's trace into a fresh worker context after the
	// claim. The original HTTP deadline/cancel/credentials are not inherited.
	restored, telemetryEvent, telemetryErr := w.restoreTelemetry(ctx, o, v)
	// The restored trace belongs to the finished accepting request, so this pass
	// opens its own span and keeps the same trace_id with a new span_id. A
	// missing/invalid persisted context lands on a fresh span before it is
	// reported, so the ERROR still carries a valid trace_id/span_id.
	ctx, runSpan := telemetry.StartRun(restored, "vm.operation."+o.Action)
	defer runSpan.End()
	w.logTelemetryEvent(ctx, o, v, telemetryEvent, telemetryErr)
	e = w.advance(ctx, &o, &v)
	if e != nil {
		// A known wait is logged as step.waiting where it is detected, never as a
		// step failure. Record every other real failure at the point it becomes
		// durable state: a Save that then succeeds returns nil below.
		if !errors.Is(e, errWaitingDependency) {
			w.logStepFailed(ctx, o, v, stepForAction(o.Action), e)
		}
		// The durable business reason keeps its existing text; logs use a stable
		// classification and the redacted error fields for the cause.
		reason := e.Error()
		logReason := operationReason(e)
		o.Reason = reason
		if errors.Is(e, ErrUnknown) {
			o.State = "unknown"
			w.logOperationFailed(ctx, o, v, logReason, e)
		} else if errors.Is(e, ErrOwnership) || errors.Is(e, ErrInvalid) || errors.Is(e, ErrChanged) || errors.Is(e, ErrStartFailed) || errors.Is(e, ErrCloneSourceUnavailable) {
			o.State = "failed"
			w.logOperationFailed(ctx, o, v, logReason, e)
		} else {
			o.State = "running"
			w.logRetryScheduled(ctx, o, v, logReason, o.LeaseUntil)
		}
	} else {
		o.Reason = ""
	}
	x := w.repo.Save(ctx, o, v)
	if x != nil {
		if errors.Is(x, ErrVersion) || errors.Is(x, ErrLease) {
			// A newer intent or lease already owns this work. Its durable
			// state wins; losing the fence does not stop other VM workers.
			w.logConflict(ctx, o, v, "VM_LEASE_LOST")
			return nil
		}
		// The state write itself failed; keep it distinct from the business error.
		w.logPersistFailed(ctx, o, v, x)
		return x
	}
	// Native reconciliation failures are now durable operation outcomes.
	// Returning them to the service supervisor would terminate the shared
	// service whenever a dependency is still preparing or a result is unknown.
	return nil
}

// restoreTelemetry restores o's saved trace onto ctx, binds the operation
// identity, and returns the single telemetry.context_* event to report once the
// run span exists ("" when the metadata is usable) together with the real
// decode/validate error. The caller continues the original business path in
// every case.
func (w *Worker) restoreTelemetry(ctx context.Context, o Operation, v VM) (context.Context, string, error) {
	business := telemetry.BusinessContext{
		OperationID: o.ID, ResourceType: "vm", ResourceID: v.ID, TenantID: o.TenantID, Action: o.Action,
	}
	if o.TelemetryContextError != nil {
		return telemetry.WithBusiness(ctx, business), "telemetry.context_invalid", o.TelemetryContextError
	}
	if !o.TelemetryContext.Valid() {
		return telemetry.WithBusiness(ctx, business), "telemetry.context_missing", nil
	}
	restored, e := telemetry.Restore(ctx, o.TelemetryContext)
	if e != nil {
		return telemetry.WithBusiness(ctx, business), "telemetry.context_invalid", e
	}
	return telemetry.WithBusiness(restored, business), "", nil
}

func (w *Worker) logTelemetryEvent(ctx context.Context, o Operation, v VM, event string, err error) {
	if w.logger == nil || event == "" {
		return
	}
	attrs := []any{"event", event}
	attrs = append(attrs, operationIdentity(o, v)...)
	if event == "telemetry.context_missing" {
		attrs = append(attrs, "reason", "TELEMETRY_CONTEXT_MISSING")
	} else {
		attrs = append(attrs, "reason", "TELEMETRY_CONTEXT_INVALID")
		if err != nil {
			attrs = append(attrs, errorAttrs(err)...)
		}
	}
	w.logger.ErrorContext(ctx, "VM operation telemetry context unavailable", attrs...)
}

// stepForAction names the failing stage without inventing a new state machine.
func stepForAction(action string) string {
	switch action {
	case "create":
		return "ensure_vm"
	case "delete":
		return "delete_vm"
	case "":
		return "advance"
	default:
		return action
	}
}
func (w *Worker) Step(ctx context.Context) (bool, error) {
	e := w.RunOnce(ctx)
	if errors.Is(e, ErrNotFound) {
		return false, nil
	}
	return true, e
}
func (w *Worker) checkpoint(ctx context.Context, o *Operation, v VM, action string, target ObjectRef) error {
	o.Pending = &Mutation{Action: action, Target: target, Submitted: true}
	return w.repo.Checkpoint(ctx, *o, v)
}
func (w *Worker) advance(ctx context.Context, o *Operation, v *VM) error {
	if o.Action == DeleteAction {
		return w.remove(ctx, o, v)
	}
	if v.Closing {
		return ErrBusy
	}
	if o.Action == CreateAction {
		if v.Spec.CloneSource != nil {
			return w.clone(ctx, o, v)
		}
		return w.create(ctx, o, v)
	}
	switch o.Action {
	case "start", "stop", "force-stop", "restart":
		return w.power(ctx, o, v)
	case "create-disk", "attach-disk", "detach-disk", "delete-disk":
		return w.disk(ctx, o, v)
	case "set-network-interface":
		return w.setNetworkInterface(ctx, o, v)
	case "set-security-groups":
		return w.setSecurityGroups(ctx, o, v)
	case "set-media", "eject-media", "set-boot":
		return w.media(ctx, o, v)
	case "create-snapshot", "delete-snapshot", "restore-snapshot":
		return w.snapshot(ctx, o, v)
	default:
		return ErrDependency
	}
}
func (w *Worker) create(ctx context.Context, o *Operation, v *VM) error {
	if !v.SecurityGroupsInitialized {
		if len(v.Spec.SecurityGroupIDs) > 0 {
			return w.initializeSecurityGroups(ctx, v)
		}
		v.SecurityGroupsInitialized = true
		v.SecurityGroupIDs = []string{}
	}
	if e := InitializeMedia(v); e != nil {
		return e
	}
	if v.Network.AttachmentID == "" {
		if e := w.checkpoint(ctx, o, *v, "prepare-network", ObjectRef{}); e != nil {
			return e
		}
		plan, e := w.network.Prepare(ctx, *v)
		if e != nil {
			return e
		}
		v.Network = plan
		o.Pending = nil
		return nil
	}
	if v.Native.UID == "" {
		if e := w.native.ValidateSource(ctx, v.Spec.Media); e != nil {
			return e
		}
		if e := w.checkpoint(ctx, o, *v, "create-vm", v.Native); e != nil {
			return e
		}
		ref, e := w.native.EnsureVM(ctx, *v)
		if e != nil {
			return e
		}
		v.Native = ref
		o.Pending = nil
		return nil
	}
	if v.Spec.GuestInitialization != nil && v.InitializationSecret.UID == "" {
		if e := w.checkpoint(ctx, o, *v, "create-initialization-secret", v.InitializationSecret); e != nil {
			return e
		}
		ref, e := w.native.EnsureInitialization(ctx, *v)
		if e != nil {
			return e
		}
		v.InitializationSecret = ref
		o.Pending = nil
		return nil
	}
	if changed, e := w.ensureDesiredMediaCopies(ctx, o, v); e != nil || changed {
		return e
	}
	for i := range v.Disks {
		if v.Disks[i].DV.UID == "" {
			if v.Disks[i].Role == "system" && v.Spec.Media.Kind == "DISK_IMAGE" {
				if e := w.native.ValidateSource(ctx, v.Spec.Media); e != nil {
					return e
				}
			}
			if e := w.checkpoint(ctx, o, *v, "create-volume", v.Disks[i].DV); e != nil {
				return e
			}
			d, e := w.native.EnsureVolume(ctx, *v, v.Disks[i])
			if e != nil {
				return e
			}
			v.Disks[i] = d
			o.Pending = nil
			return nil
		}
	}
	// Create owns initial inventory registration independently of power.
	// Replaying EnsureVolume on an already registered DV only observes that
	// same identity; Pending/WFFC is valid and does not delay native Start.
	for i, disk := range v.Disks {
		observed, err := w.native.EnsureVolume(ctx, *v, disk)
		if err != nil {
			return err
		}
		v.Disks[i] = observed
	}
	if !v.Spec.StartAfterCreate {
		o.State = "succeeded"
		v.Observed = "Stopped"
		v.ObservedAt = time.Now().UTC()
		return nil
	}
	return w.power(ctx, o, v)
}

// observeLifecycle reads only runtime facts; inventory is reconciled by its
// owning create/disk/media/restore paths and independent preparation observation.
func (w *Worker) observeLifecycle(ctx context.Context, v VM) (Observation, error) {
	if n, ok := w.native.(interface {
		ObserveLifecycle(context.Context, VM) (Observation, error)
	}); ok {
		return n.ObserveLifecycle(ctx, v)
	}
	return w.native.Observe(ctx, v)
}
func (w *Worker) power(ctx context.Context, o *Operation, v *VM) error {
	if v.SecurityGroupsInitialized && len(v.SecurityGroupIDs) > 0 {
		provider, ok := w.native.(SecurityGroupNativePort)
		if !ok {
			return ErrDependency
		}
		if err := provider.SetSecurityGroups(ctx, *v, v.SecurityGroupNames); err != nil {
			return err
		}
	}
	x, e := w.observeLifecycle(ctx, *v)
	if e != nil {
		return e
	}
	v.Observed = x.State
	v.Reason = x.Reason
	v.ObservedAt = time.Now().UTC()
	if x.VM.UID != "" && v.Native.UID != x.VM.UID {
		return ErrOwnership
	}
	if x.MediaSlots != nil {
		v.MediaSlots = x.MediaSlots
	}
	if ReconcileNetworkSubmission(v, x) {
		// Save this current management revision before confirming it with Network.
		return nil
	}
	if len(x.Volumes) > 0 {
		for i := range v.Disks {
			for _, d := range x.Volumes {
				if v.Disks[i].ID == d.ID {
					v.Disks[i] = d
				}
			}
		}
	}
	if o.Action == "restart" && (PrimaryNetworkInterface(*v).PendingRestart() || o.Step == "network-stopping" || o.Step == "network-starting") {
		if o.Step != "network-starting" {
			if !x.Stopped || x.VMI != nil || x.Launcher != nil || x.RelatedPodsPresent || len(x.StateChangeRequests) != 0 {
				if o.Step != "network-stopping" {
					o.Step = "network-stopping"
					if e = w.checkpoint(ctx, o, *v, "network-stop", v.Native); e != nil {
						return e
					}
					return w.native.Stop(ctx, *v, false)
				}
				w.logStepWaiting(ctx, *o, *v, o.Step, "VM_RESTART_WAITING_OLD_INSTANCE")
				return errWaitingDependency
			}
			if e = w.applyNetworkTemplate(ctx, o, v); e != nil {
				return e
			}
			o.Step = "network-starting"
			o.Pending = nil
			return nil
		}
	}
	if (o.Action == "start" || o.Step == "network-starting") && PrimaryNetworkInterface(*v).PendingRestart() {
		if !x.Stopped || x.VMI != nil || x.Launcher != nil || x.RelatedPodsPresent || len(x.StateChangeRequests) != 0 {
			step := o.Step
			if step == "" {
				step = "network-starting"
			}
			w.logStepWaiting(ctx, *o, *v, step, "VM_START_WAITING_OLD_INSTANCE")
			return errWaitingDependency
		}
		if e = w.applyNetworkTemplate(ctx, o, v); e != nil {
			return e
		}
		return nil
	}
	confirmationErr := w.network.Confirm(ctx, *v, x)
	if addresses, addressErr := w.network.CurrentAddresses(ctx, *v); addressErr == nil {
		v.Addresses = addresses
	}
	if x.Stopped || x.Deleted || (x.Terminal && x.Launcher == nil) {
		v.Addresses = nil
	}
	action := o.Action
	if o.Step == "network-starting" {
		action = "start"
	}
	if action == CreateAction {
		action = "start"
	}
	switch action {
	case "start":
		v.Desired = "Running"
		if x.Running {
			if confirmationErr != nil {
				return confirmationErr
			}
			if e = w.confirmNetworkInterface(ctx, v, x); e != nil {
				return e
			}
			o.State = "succeeded"
			o.Pending = nil
			return nil
		}
		// Only a VMI different from the identity observed before this Start can
		// prove its terminal failure. Legacy checkpoints lack that observation;
		// an unchanged old VMI or an outstanding native request remains unknown.
		if o.Pending != nil && o.Pending.Action == "start" && o.Pending.Submitted && o.Pending.Target == v.Native &&
			x.State == "Failed" && x.VMI != nil && x.VMI.UID != "" && v.Native.UID != "" && x.VM.UID == v.Native.UID &&
			o.Pending.BeforeVMIUID != nil && x.VMI.UID != *o.Pending.BeforeVMIUID &&
			len(x.StateChangeRequests) == 0 {
			o.Pending = nil
			v.Reason = ErrStartFailed.Error()
			return ErrStartFailed
		}
		// Start converges to one VMI for this VM. After observing an actually
		// stopped VM with no VMI, launcher or native request, retrying Start is
		// safe. Restart remains fenced below because it creates a new VMI.
		if o.Pending != nil && o.Pending.Submitted && !(o.Pending.Action == "start" && x.Stopped) {
			return ErrUnknown
		}
		beforeVMIUID := ""
		if x.VMI != nil {
			beforeVMIUID = x.VMI.UID
		}
		o.Pending = &Mutation{Action: "start", Target: v.Native, Submitted: true, BeforeVMIUID: &beforeVMIUID}
		if e = w.repo.Checkpoint(ctx, *o, *v); e != nil {
			return e
		}
		err := w.native.Start(ctx, *v)
		var notSubmitted *NativeNotSubmittedError
		if errors.As(err, &notSubmitted) {
			// The adapter returned before submitting Start. Persist that receipt
			// so the same operation can retry after its preconditions recover.
			// A crash or lost native response still retains the checkpoint.
			o.Pending = nil
		}
		return err
	case "stop", "force-stop":
		v.Desired = "Stopped"
		ended := x.Terminal && x.VMI != nil && x.VMI.UID != "" && x.VM.UID == v.Native.UID && v.Native.UID != ""
		if ((x.Stopped && x.VMI == nil) || ended) && x.Launcher == nil && !x.RelatedPodsPresent && len(x.StateChangeRequests) == 0 {
			o.State = "succeeded"
			o.Pending = nil
			return nil
		}
		if o.Pending != nil && o.Pending.Submitted {
			return ErrUnknown
		}
		if e = w.checkpoint(ctx, o, *v, action, v.Native); e != nil {
			return e
		}
		return w.native.Stop(ctx, *v, action == "force-stop")
	case "restart":
		v.Desired = "Running"
		if o.OldVMIUID != "" && x.VMI != nil && x.VMI.UID != o.OldVMIUID && x.Running {
			if confirmationErr != nil {
				return confirmationErr
			}
			if e = w.confirmNetworkInterface(ctx, v, x); e != nil {
				return e
			}
			o.State = "succeeded"
			o.Pending = nil
			return nil
		}
		if o.Pending != nil && o.Pending.Submitted {
			return ErrUnknown
		}
		if x.VMI == nil || !x.Running {
			return ErrDependency
		}
		o.OldVMIUID = x.VMI.UID
		if e = w.checkpoint(ctx, o, *v, "restart", *x.VMI); e != nil {
			return e
		}
		return w.native.Restart(ctx, *v)
	}
	return ErrInvalid
}
func (w *Worker) remove(ctx context.Context, o *Operation, v *VM) error {
	if !v.Closing {
		return ErrBusy
	}
	// A clone target cancelled before registration keeps its native producer and
	// the source HELD until that producer has terminated.
	if v.CloneProgress != nil {
		done, e := w.cancelClone(ctx, o, v)
		if e != nil {
			return e
		}
		if !done {
			return nil
		}
	}
	if v.Network.AttachmentID == "" && v.Network.NADUID != "" && o.Pending != nil && o.Pending.Action == "prepare-network" && o.Pending.Submitted {
		// The second prepare may have committed its Attachment before its VM
		// save lost a race with Delete. The saved first-round plan reconstructs
		// that exact request and reaches permanent replay before any new reads.
		plan, err := w.network.Prepare(ctx, *v)
		if err != nil {
			return err
		}
		v.Network = plan
		o.Pending = nil
		ReconcileNetworkSubmission(v, Observation{})
		return nil
	}
	if done, e := w.removeSnapshots(ctx, o, v); e != nil || !done {
		return e
	}
	x, e := w.observeLifecycle(ctx, *v)
	if e != nil {
		return e
	}
	if ReconcileNetworkSubmission(v, x) {
		return nil
	}
	for i := range v.Disks {
		for _, d := range x.Volumes {
			if v.Disks[i].ID == d.ID {
				v.Disks[i] = d
			}
		}
	}
	if !x.Deleted {
		// Unknown creation is resolved by the frozen name and ownership before
		// deletion. An absent root never authorizes deleting a new same-name UID.
		if v.Native.UID == "" && x.VM.UID != "" {
			v.Native = x.VM
			return nil
		}
		if e = w.checkpoint(ctx, o, *v, "delete-vm", v.Native); e != nil {
			return e
		}
		return w.native.Delete(ctx, *v)
	}
	if e := InitializeMedia(v); e != nil {
		return e
	}
	v.Addresses = nil
	if v.Spec.GuestInitialization != nil {
		if e = w.checkpoint(ctx, o, *v, "delete-initialization-secret", v.InitializationSecret); e != nil {
			return e
		}
		done, e := w.native.DeleteInitialization(ctx, *v)
		if e != nil {
			return e
		}
		if !done {
			return nil
		}
		v.InitializationSecret = ObjectRef{}
	}
	for i := range v.Disks {
		if v.Disks[i].Tombstone {
			continue
		}
		if e = w.checkpoint(ctx, o, *v, "delete-volume", v.Disks[i].DV); e != nil {
			return e
		}
		done, x := w.native.DeleteVolume(ctx, *v, v.Disks[i])
		if x != nil {
			return x
		}
		if !done {
			return nil
		}
		v.Disks[i].Tombstone = true
		return nil
	}
	if done, e := w.removeMedia(ctx, o, v); e != nil || !done {
		return e
	}
	if done, e := w.removeRetiredVolumes(ctx, o, v); e != nil || !done {
		return e
	}
	if len(v.BackendStates) != 0 {
		native, ok := w.native.(BackendNativePort)
		if !ok {
			return ErrDependency
		}
		for _, backend := range v.BackendStates {
			done, e := native.BackendStateRemoved(ctx, *v, backend)
			if e != nil {
				return e
			}
			if !done {
				return ErrDependency
			}
		}
	}
	if len(v.Media) > 0 || len(v.Cleanup) > 0 {
		return ErrDependency
	}
	if e = w.checkpoint(ctx, o, *v, "release-network", ObjectRef{}); e != nil {
		return e
	}
	done, e := w.network.Release(ctx, *v)
	if e != nil {
		return e
	}
	if !done {
		return nil
	}
	if port, ok := w.network.(ManualNetworkPort); ok {
		if err := port.ReleaseNetworkInterfaces(ctx, *v, nil); err != nil {
			return err
		}
	} else if len(v.NetworkInterfaces) > 0 {
		return ErrDependency
	}
	if w.securityGroups != nil {
		if err := w.securityGroups.ReleaseSecurityGroupBinding(ctx, v.TenantID, "vm", v.ID, v.Version); err != nil {
			return err
		}
	} else if len(v.SecurityGroupIDs) > 0 || len(v.Spec.SecurityGroupIDs) > 0 {
		return ErrDependency
	}
	o.State = "succeeded"
	o.Pending = nil
	v.Observed = "Deleted"
	v.Reason = ""
	v.ObservedAt = time.Now().UTC()
	return nil
}
func NewInitialDisk(vmID, key, name, role string, size int64) Disk {
	id := uuid.NewSHA1(uuid.NameSpaceOID, []byte("ani-vm-disk/"+vmID+"/"+role+"/"+key)).String()
	return Disk{ID: id, Role: role, Name: name, SizeGB: size, RequestedBytes: size * 1000000000, VolumeName: "disk-" + strings.ReplaceAll(id, "-", ""), Serial: id, Attached: true, DV: ObjectRef{Kind: "DataVolume", Name: "vd-" + id}, Preparation: "PendingPopulation"}
}
