package vm

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

const (
	cloneTestTenant  = "64c5c35a-f554-432c-b4bc-7b4f36f08420"
	cloneTestTarget  = "21cf875f-781a-41db-81cf-7063f4f4cfc4"
	cloneTestSource  = "31cf875f-781a-41db-81cf-7063f4f4cfc5"
	cloneTestOp      = "b492e0ba-f34e-4143-a9dc-94427c2d3bec"
	cloneTestNativeN = "vm-target"
)

// cloneTestRepo is the durable fake: Claim returns the last saved state so one
// test can drive many worker ticks.
type cloneTestRepo struct {
	op         Operation
	vm         VM
	events     *[]string
	releases   []CloneSourceRelease
	releaseErr error
}

func (r *cloneTestRepo) AcceptCreate(context.Context, Acceptance) (Operation, error) {
	return Operation{}, ErrInvalid
}
func (r *cloneTestRepo) AcceptDelete(context.Context, Acceptance) (Operation, error) {
	return Operation{}, ErrInvalid
}
func (r *cloneTestRepo) AcceptCommand(context.Context, Command) (Operation, error) {
	return Operation{}, ErrInvalid
}
func (r *cloneTestRepo) LookupReplay(context.Context, Command) (Operation, error) {
	return Operation{}, ErrNotFound
}
func (r *cloneTestRepo) Get(context.Context, string, string) (VM, error) { return r.vm, nil }
func (r *cloneTestRepo) List(context.Context, string, ListFilter) ([]VM, int64, error) {
	return nil, 0, nil
}
func (r *cloneTestRepo) GetOperation(context.Context, string, string, string) (Operation, error) {
	return r.op, nil
}
func (r *cloneTestRepo) Claim(context.Context, string, time.Duration) (Operation, VM, error) {
	return r.op, r.vm, nil
}
func (r *cloneTestRepo) Save(_ context.Context, o Operation, v VM) error {
	r.op, r.vm = o, v
	return nil
}
func (r *cloneTestRepo) Checkpoint(_ context.Context, o Operation, v VM) error {
	r.op, r.vm = o, v
	return nil
}
func (r *cloneTestRepo) UpdateNetwork(context.Context, VM, NetworkPlan) error { return nil }
func (r *cloneTestRepo) ReleaseCloneSourceHELD(_ context.Context, release CloneSourceRelease) error {
	r.releases = append(r.releases, release)
	*r.events = append(*r.events, "release:"+release.Kind)
	return r.releaseErr
}

// cloneTestNative scripts the native clone projection and records every step.
type cloneTestNative struct {
	events       *[]string
	observations []CloneObservation
	observeErr   error
	eligibility  CloneEligibility
	eligibilityE error
	deleteResult []CloneDeleteResult
	targetGone   bool
	// targets scripts successive frozen-name target reads; target is the stable
	// answer once the queue is empty.
	targets          []ObjectRef
	target           ObjectRef
	targetObserveErr error
	volumesDone      bool
	adopted          []VolumeRef
}

func (n *cloneTestNative) note(event string) { *n.events = append(*n.events, event) }

func (n *cloneTestNative) ValidateSource(context.Context, Source) error { return nil }
func (n *cloneTestNative) EnsureVM(context.Context, VM) (ObjectRef, error) {
	return ObjectRef{}, nil
}
func (n *cloneTestNative) EnsureInitialization(context.Context, VM) (ObjectRef, error) {
	return ObjectRef{}, nil
}
func (n *cloneTestNative) DeleteInitialization(context.Context, VM) (bool, error) {
	return true, nil
}
func (n *cloneTestNative) EnsureVolume(_ context.Context, _ VM, d Disk) (Disk, error) {
	return d, nil
}
func (n *cloneTestNative) Observe(context.Context, VM) (Observation, error) {
	return Observation{}, nil
}
func (n *cloneTestNative) Start(context.Context, VM) error      { return nil }
func (n *cloneTestNative) Stop(context.Context, VM, bool) error { return nil }
func (n *cloneTestNative) Restart(context.Context, VM) error    { return nil }
func (n *cloneTestNative) Delete(context.Context, VM) error     { n.note("delete-vm"); return nil }
func (n *cloneTestNative) DeleteVolume(context.Context, VM, Disk) (bool, error) {
	return true, nil
}

func (n *cloneTestNative) EnsureClone(context.Context, VM, CloneRequest) (CloneObservation, error) {
	n.note("ensure-clone")
	return CloneObservation{Exists: true, UID: "clone-uid", Phase: "SnapshotInProgress", SnapshotName: "snap-1"}, nil
}
func (n *cloneTestNative) ObserveClone(context.Context, VM, CloneRequest) (CloneObservation, error) {
	if n.observeErr != nil {
		return CloneObservation{}, n.observeErr
	}
	if len(n.observations) == 0 {
		return CloneObservation{Exists: false}, nil
	}
	next := n.observations[0]
	n.observations = n.observations[1:]
	return next, nil
}
func (n *cloneTestNative) DeleteClone(context.Context, VM, CloneRequest, string, string) (CloneDeleteResult, error) {
	n.note("delete-clone")
	if len(n.deleteResult) == 0 {
		return CloneDeleteResult{Gone: true}, nil
	}
	next := n.deleteResult[0]
	n.deleteResult = n.deleteResult[1:]
	return next, nil
}
func (n *cloneTestNative) ObserveSourceCloneEligibility(context.Context, VM, VM) (CloneEligibility, error) {
	return n.eligibility, n.eligibilityE
}
func (n *cloneTestNative) RecheckCloneSource(context.Context, VM) (CloneEligibility, error) {
	return n.eligibility, n.eligibilityE
}
func (n *cloneTestNative) ApplyCloneTargetConfiguration(context.Context, VM, CloneRequest) ([]Disk, ObjectRef, error) {
	return nil, ObjectRef{}, ErrInvalid
}
func (n *cloneTestNative) DeleteCloneTarget(context.Context, VM, ObjectRef) (bool, error) {
	n.note("delete-target")
	return n.targetGone, nil
}

// ObserveCloneTarget scripts the frozen-name target read used by a cancel whose
// clone already disappeared.
func (n *cloneTestNative) ObserveCloneTarget(context.Context, VM, CloneRequest) (ObjectRef, error) {
	n.note("observe-target")
	if n.targetObserveErr != nil {
		return ObjectRef{}, n.targetObserveErr
	}
	if len(n.targets) > 0 {
		next := n.targets[0]
		n.targets = n.targets[1:]
		return next, nil
	}
	return n.target, nil
}
func (n *cloneTestNative) DeleteNamedVolumes(context.Context, VM, []VolumeRef) ([]VolumeRef, bool, error) {
	n.note("delete-volumes")
	return n.adopted, n.volumesDone, nil
}

// cloneTestNetwork is both the network and the security-group port.
type cloneTestNetwork struct {
	events *[]string
}

func (n *cloneTestNetwork) Prepare(context.Context, VM) (NetworkPlan, error) {
	*n.events = append(*n.events, "prepare-network")
	return NetworkPlan{AttachmentID: "att-1", SubmissionID: "sub-1", SnapshotRevision: 1,
		Consumer: &NetworkConsumerState{State: "open", DesiredRunState: "stopped"}}, nil
}
func (n *cloneTestNetwork) Confirm(context.Context, VM, Observation) error { return nil }
func (n *cloneTestNetwork) CurrentAddresses(context.Context, VM) ([]Address, error) {
	return nil, nil
}
func (n *cloneTestNetwork) Release(_ context.Context, v VM) (bool, error) {
	*n.events = append(*n.events, "release-network")
	// The real adapter requires a persisted closed consumer; record whether this
	// call saw one so a rollback that skips the hand-off cannot pass silently.
	if v.Network.Consumer == nil || v.Network.Consumer.State != "closed" {
		*n.events = append(*n.events, "release-network-without-closed-consumer")
	}
	return true, nil
}
func (n *cloneTestNetwork) ResolveSecurityGroups(context.Context, string, string, string, []string) ([]string, error) {
	return nil, nil
}
func (n *cloneTestNetwork) SyncSecurityGroupBinding(context.Context, string, string, string, string, string, int64, []string) ([]string, error) {
	return nil, nil
}
func (n *cloneTestNetwork) ConfirmSecurityGroupBinding(context.Context, string, string, string, int64, []string) error {
	return nil
}
func (n *cloneTestNetwork) ReleaseSecurityGroupBinding(context.Context, string, string, string, int64) error {
	*n.events = append(*n.events, "release-security-groups")
	return nil
}

func workerCloneVM() VM {
	v := VM{
		TenantID: cloneTestTenant,
		ID:       cloneTestTarget,
		Version:  3,
		Native:   ObjectRef{Kind: "VirtualMachine", Namespace: "tenant-a", Name: cloneTestNativeN, UID: "target-uid"},
		Spec: Spec{
			Namespace:  "tenant-a",
			Digest:     "digest",
			BootDevice: "SYSTEM_DISK",
			CloneSource: &CloneSource{
				SourceVMID: cloneTestSource,
				SourceName: "vm-source",
				Native:     ObjectRef{Kind: "VirtualMachine", Namespace: "tenant-a", Name: "vm-source", UID: "source-uid"},
				Disks: []CloneDisk{
					{SourceDiskID: "disk-1", VolumeName: "disk-sys", Role: "system", Name: "system", SizeGB: 8},
					{SourceDiskID: "disk-2", VolumeName: "disk-data", Role: "data", Name: "data", SizeGB: 4},
				},
			},
		},
		Network: NetworkPlan{AttachmentID: "att-1", SubmissionID: "sub-1", SnapshotRevision: 1,
			Consumer: &NetworkConsumerState{State: "open", DesiredRunState: "stopped"}},
	}
	v.Disks = []Disk{{VolumeName: "disk-sys", Role: "system", Attached: true, Serial: "serial-sys"}}
	return v
}

func workerCloneOp(v VM, action string) Operation {
	return Operation{TenantID: v.TenantID, ID: cloneTestOp, VMID: v.ID, Action: action, State: "running", Key: "key"}
}

func newCloneWorker(t *testing.T, v VM, o Operation, native *cloneTestNative) (*Worker, *cloneTestRepo, *[]string) {
	t.Helper()
	events := &[]string{}
	native.events = events
	repo := &cloneTestRepo{op: o, vm: v, events: events}
	network := &cloneTestNetwork{events: events}
	worker, err := NewWorker(repo, native, network)
	if err != nil {
		t.Fatal(err)
	}
	if err = worker.UseSecurityGroups(network); err != nil {
		t.Fatal(err)
	}
	return worker, repo, events
}

func hasEvent(events []string, want string) bool {
	for _, event := range events {
		if event == want {
			return true
		}
	}
	return false
}

// TestCloneWorkerFailedCleanup covers T1: a failed clone with a usable target
// deletes the clone, releases the HELD, then removes VM, volumes and network.
func TestCloneWorkerFailedCleanup(t *testing.T) {
	ctx := context.Background()
	v := workerCloneVM()
	v.Native = ObjectRef{}
	v.CloneProgress = &CloneProgress{
		Name: CloneName(v.ID), UID: "clone-uid", CloneDeleteRequested: true,
		PendingVolumes: []VolumeRef{
			{VolumeName: "disk-sys", PVCName: "pvc-sys", PVCUID: "pvc-uid-1"},
			{VolumeName: "disk-data", PVCName: "pvc-data", PVCUID: "pvc-uid-2"},
		},
	}
	outputs := []VolumeRef{
		{VolumeName: "disk-sys", PVCName: "pvc-sys", PVCUID: "pvc-uid-1"},
		{VolumeName: "disk-data", PVCName: "pvc-data", PVCUID: "pvc-uid-2"},
	}
	native := &cloneTestNative{
		observations: []CloneObservation{{
			Exists: true, UID: "clone-uid", ResourceVersion: "4", Failed: true, Reason: "boom",
			Target:  ObjectRef{Kind: "VirtualMachine", Namespace: "tenant-a", Name: cloneTestNativeN, UID: "target-uid"},
			Outputs: outputs,
		}},
		targetGone:  true,
		volumesDone: true,
	}
	worker, repo, events := newCloneWorker(t, v, workerCloneOp(v, CreateAction), native)
	for i := 0; i < 12 && repo.op.State != "failed"; i++ {
		if err := worker.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if repo.op.State != "failed" {
		t.Fatalf("failed cleanup did not finish: %s %v", repo.op.State, *events)
	}
	want := []string{"delete-clone", "release:REGISTERED", "delete-target", "delete-volumes", "release-network", "release-security-groups"}
	if strings.Join(*events, ",") != strings.Join(want, ",") {
		t.Fatalf("cleanup order = %v", *events)
	}
	if len(repo.releases) != 1 || repo.releases[0].Kind != "REGISTERED" {
		t.Fatalf("release = %v", repo.releases)
	}
}

// TestCloneWorkerFailedWithoutTarget covers T2: no target and incomplete named
// output keeps the producer and the HELD reference.
func TestCloneWorkerFailedWithoutTarget(t *testing.T) {
	ctx := context.Background()
	v := workerCloneVM()
	v.Native = ObjectRef{}
	v.CloneProgress = &CloneProgress{Name: CloneName(v.ID), UID: "clone-uid", CloneDeleteRequested: true}
	native := &cloneTestNative{observations: []CloneObservation{{
		Exists: true, UID: "clone-uid", Failed: true, Reason: "boom",
		Outputs: []VolumeRef{{VolumeName: "disk-sys", PVCName: "pvc-sys", PVCUID: "pvc-uid-1"}},
	}}}
	worker, repo, events := newCloneWorker(t, v, workerCloneOp(v, CreateAction), native)
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if repo.op.State != "unknown" {
		t.Fatalf("state = %s", repo.op.State)
	}
	if hasEvent(*events, "delete-clone") || len(repo.releases) != 0 {
		t.Fatalf("residual case was disposed of: %v %v", *events, repo.releases)
	}
}

// TestCloneWorkerVanishedBeforeRegistration covers T3: an unregistered clone
// that vanished without outputs keeps the HELD reference.
func TestCloneWorkerVanishedBeforeRegistration(t *testing.T) {
	ctx := context.Background()
	v := workerCloneVM()
	v.Native = ObjectRef{}
	v.CloneProgress = &CloneProgress{Name: CloneName(v.ID), UID: "clone-uid"}
	native := &cloneTestNative{observations: []CloneObservation{{Exists: false}}}
	worker, repo, events := newCloneWorker(t, v, workerCloneOp(v, CreateAction), native)
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if repo.op.State != "unknown" || hasEvent(*events, "delete-clone") || len(repo.releases) != 0 {
		t.Fatalf("vanished clone was released: %s %v %v", repo.op.State, *events, repo.releases)
	}
}

// TestCloneWorkerEarlyCancel covers T4: a pre-registration delete removes the
// producer before releasing the source as CANCELLED.
func TestCloneWorkerEarlyCancel(t *testing.T) {
	ctx := context.Background()
	v := workerCloneVM()
	v.Closing = true
	v.Native = ObjectRef{}
	v.CloneProgress = &CloneProgress{Name: CloneName(v.ID), CloneDeleteRequested: true}
	native := &cloneTestNative{observations: []CloneObservation{{
		Exists: true, UID: "clone-uid", ResourceVersion: "2", Pending: true,
	}}}
	worker, repo, events := newCloneWorker(t, v, workerCloneOp(v, DeleteAction), native)
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if !hasEvent(*events, "delete-clone") {
		t.Fatalf("early cancel did not delete the clone: %v", *events)
	}
	if len(repo.releases) != 1 || repo.releases[0].Kind != "CANCELLED" {
		t.Fatalf("release = %v", repo.releases)
	}
	if repo.vm.CloneProgress == nil || !repo.vm.CloneProgress.CancelHandled {
		t.Fatal("cancel hand-off was not recorded")
	}
}

// TestCloneWorkerCancelNegatives covers T5/T6: every condition that is not a
// provable early cancel must wait, and a foreign UID is an ownership failure.
func TestCloneWorkerCancelNegatives(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name        string
		state       CloneObservation
		observeErr  error
		wantState   string
		pending     []VolumeRef
		wantNoClone bool
	}{
		{name: "restore exists", state: CloneObservation{Exists: true, UID: "clone-uid", Pending: true, RestoreName: "restore-1"}, wantState: "running"},
		{name: "snapshot in progress", state: CloneObservation{Exists: true, UID: "clone-uid", Pending: true, SnapshotName: "snap-1"}, wantState: "running"},
		{name: "outputs incomplete", state: CloneObservation{Exists: true, UID: "clone-uid", Pending: true, Outputs: []VolumeRef{{VolumeName: "disk-sys", PVCName: "pvc-sys"}}}, wantState: "running"},
		{name: "failed undetermined", state: CloneObservation{Exists: true, UID: "clone-uid", Failed: true}, wantState: "unknown"},
		{name: "api unreadable", observeErr: ErrDependency, wantState: "running"},
		{name: "foreign uid", observeErr: ErrOwnership, wantState: "failed"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			v := workerCloneVM()
			v.Closing = true
			v.Native = ObjectRef{}
			v.CloneProgress = &CloneProgress{Name: CloneName(v.ID), CloneDeleteRequested: true, PendingVolumes: test.pending}
			native := &cloneTestNative{observations: []CloneObservation{test.state}, observeErr: test.observeErr}
			worker, repo, events := newCloneWorker(t, v, workerCloneOp(v, DeleteAction), native)
			if err := worker.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			if repo.op.State != test.wantState {
				t.Fatalf("state = %s, want %s (%v)", repo.op.State, test.wantState, *events)
			}
			if hasEvent(*events, "delete-clone") || len(repo.releases) != 0 {
				t.Fatalf("cancel advanced on a negative case: %v %v", *events, repo.releases)
			}
		})
	}
}

// TestCloneWorkerUnsubmittedCleanup covers T7: a source rejected before submit
// releases the network hand-off first, then the HELD as CANCELLED.
func TestCloneWorkerUnsubmittedCleanup(t *testing.T) {
	ctx := context.Background()
	v := workerCloneVM()
	v.Native = ObjectRef{}
	v.Network = NetworkPlan{}
	native := &cloneTestNative{eligibility: CloneEligibility{Unsupported: []CloneBlocker{
		{Kind: "UNSUPPORTED", Reason: "source uses dataVolumeTemplates"},
	}}}
	worker, repo, events := newCloneWorker(t, v, workerCloneOp(v, CreateAction), native)
	for i := 0; i < 5 && repo.op.State != "failed"; i++ {
		if err := worker.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"prepare-network", "release-network", "release-security-groups", "release:CANCELLED"}
	if strings.Join(*events, ",") != strings.Join(want, ",") {
		t.Fatalf("unsubmitted order = %v", *events)
	}
	if repo.op.State != "failed" {
		t.Fatalf("state = %s", repo.op.State)
	}
	if len(repo.releases) != 1 {
		t.Fatalf("release = %v", repo.releases)
	}
	release := repo.releases[0]
	if !strings.HasPrefix(release.Reason, "unsubmitted: ") || release.Evidence != "no submit intent; network released" {
		t.Fatalf("release audit = %+v", release)
	}
	if hasEvent(*events, "ensure-clone") {
		t.Fatal("a rejected source reached submission")
	}
}

// TestCloneWorkerCancelAfterRegistration covers the race where a delete arrives
// before the create operation releases the source. The delete must finish the
// idempotent release instead of leaving a HELD row behind.
func TestCloneWorkerCancelAfterRegistration(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "release succeeds"},
		{name: "held row already gone", err: ErrNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			v := workerCloneVM()
			v.Closing = true
			v.Native = ObjectRef{Kind: "VirtualMachine", Namespace: "tenant-a", Name: cloneTestNativeN, UID: "target-uid"}
			v.CloneProgress = &CloneProgress{Name: CloneName(v.ID), UID: "clone-uid", RegistrationCompleted: true}
			worker, repo, events := newCloneWorker(t, v, workerCloneOp(v, DeleteAction), &cloneTestNative{})
			repo.releaseErr = test.err
			if err := worker.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			if len(repo.releases) != 1 || repo.releases[0].Kind != "REGISTERED" {
				t.Fatalf("release = %v", repo.releases)
			}
			if hasEvent(*events, "release:CANCELLED") {
				t.Fatalf("registered target was cancelled: %v", *events)
			}
			if repo.vm.CloneProgress == nil || !repo.vm.CloneProgress.CancelHandled {
				t.Fatal("registered cancel hand-off was not recorded")
			}
		})
	}
}

// TestCloneWorkerCancelRemovesNamedOutputs covers the cancel path counterpart of
// T1: a clone that never registered wrote no disk row, so the PVCs it already
// produced are only known from PendingVolumes. They must be removed before the
// source reference is released, and a pending removal must not release early.
func TestCloneWorkerCancelRemovesNamedOutputs(t *testing.T) {
	ctx := context.Background()
	outputs := []VolumeRef{
		{VolumeName: "disk-sys", PVCName: "restore-1-disk-sys", PVCUID: "pvc-uid-1"},
		{VolumeName: "disk-data", PVCName: "restore-1-disk-data", PVCUID: "pvc-uid-2"},
	}
	for _, test := range []struct {
		name        string
		volumesDone bool
		wantRelease bool
		wantState   string
	}{
		{name: "volumes still terminating", volumesDone: false, wantRelease: false, wantState: "running"},
		{name: "volumes gone", volumesDone: true, wantRelease: true, wantState: "running"},
	} {
		t.Run(test.name, func(t *testing.T) {
			v := workerCloneVM()
			v.Closing = true
			v.Native = ObjectRef{}
			v.CloneProgress = &CloneProgress{Name: CloneName(v.ID), UID: "clone-uid",
				CloneDeleteRequested: true, PendingVolumes: outputs}
			native := &cloneTestNative{
				observations: []CloneObservation{{Exists: false}},
				volumesDone:  test.volumesDone,
			}
			worker, repo, events := newCloneWorker(t, v, workerCloneOp(v, DeleteAction), native)
			if err := worker.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			if !hasEvent(*events, "delete-volumes") {
				t.Fatalf("cancel did not remove the named outputs: %v", *events)
			}
			if test.wantRelease {
				if len(repo.releases) != 1 || repo.releases[0].Kind != "CANCELLED" {
					t.Fatalf("release = %v (%v)", repo.releases, *events)
				}
				if repo.vm.CloneProgress == nil || !repo.vm.CloneProgress.CancelHandled {
					t.Fatal("cancel hand-off was not recorded")
				}
				return
			}
			if len(repo.releases) != 0 {
				t.Fatalf("released before the volumes were gone: %v", repo.releases)
			}
			if repo.vm.CloneProgress.CancelHandled {
				t.Fatal("cancel hand-off recorded while volumes remain")
			}
			if repo.op.State != test.wantState {
				t.Fatalf("state = %s, want %s", repo.op.State, test.wantState)
			}
		})
	}
}

// TestCloneWorkerSubmitIntentCancelsByIdentity covers R1: a delete that carries
// the persisted clone submission intent must adopt the fixed-name clone even
// though no UID was ever saved. An empty UID never proves nothing was submitted.
func TestCloneWorkerSubmitIntentCancelsByIdentity(t *testing.T) {
	ctx := context.Background()
	v := workerCloneVM()
	v.Closing = true
	v.Native = ObjectRef{}
	v.CloneProgress = &CloneProgress{Name: CloneName(v.ID)}
	native := &cloneTestNative{observations: []CloneObservation{{
		Exists: true, UID: "clone-uid", ResourceVersion: "5", Pending: true, Phase: "SnapshotInProgress", SnapshotName: "snap-1",
	}}}
	o := workerCloneOp(v, DeleteAction)
	o.Pending = &Mutation{Action: cloneStepSubmit, Submitted: true, Target: ObjectRef{Kind: "VirtualMachineClone", Namespace: "tenant-a", Name: CloneName(v.ID)}}
	worker, repo, events := newCloneWorker(t, v, o, native)
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(repo.releases) != 0 || hasEvent(*events, "release:CANCELLED") {
		t.Fatalf("submitted clone was treated as unsubmitted: %v %v", *events, repo.releases)
	}
	if hasEvent(*events, "delete-clone") {
		t.Fatalf("clone was deleted while it could still produce outputs: %v", *events)
	}
	if repo.vm.CloneProgress.UID != "clone-uid" {
		t.Fatalf("fixed-name clone was not adopted: %+v", repo.vm.CloneProgress)
	}
}

// TestCloneWorkerCancelAdoptsLateOutput covers R2: an output whose recorded UID
// is still empty is adopted by name and persisted before any release, and an
// unfinished identity hand-off never counts as cleaned.
func TestCloneWorkerCancelAdoptsLateOutput(t *testing.T) {
	ctx := context.Background()
	v := workerCloneVM()
	v.Closing = true
	v.Native = ObjectRef{}
	v.CloneProgress = &CloneProgress{Name: CloneName(v.ID), UID: "clone-uid", CloneDeleteRequested: true,
		PendingVolumes: []VolumeRef{{VolumeName: "disk-sys", PVCName: "restore-sys"}, {VolumeName: "disk-data", PVCName: "restore-data"}}}
	native := &cloneTestNative{
		observations: []CloneObservation{{Exists: false}},
		volumesDone:  false,
		adopted:      []VolumeRef{{VolumeName: "disk-sys", PVCName: "restore-sys", PVCUID: "late-uid"}},
	}
	worker, repo, events := newCloneWorker(t, v, workerCloneOp(v, DeleteAction), native)
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(repo.releases) != 0 {
		t.Fatalf("released while a late output was unresolved: %v", repo.releases)
	}
	if repo.vm.CloneProgress.CancelHandled {
		t.Fatal("cancel recorded complete while a late output was unresolved")
	}
	adopted := false
	for _, ref := range repo.vm.CloneProgress.PendingVolumes {
		adopted = adopted || (ref.PVCName == "restore-sys" && ref.PVCUID == "late-uid")
	}
	if !adopted {
		t.Fatalf("adopted identity was not persisted: %+v", repo.vm.CloneProgress.PendingVolumes)
	}
	if !hasEvent(*events, "delete-volumes") {
		t.Fatalf("cleanup was not retried: %v", *events)
	}
}

// TestCloneWorkerCancelRemovesUnregisteredTarget covers R3: a cancelled clone
// that already created its target VM removes that target before the source
// reference is released.
func TestCloneWorkerCancelRemovesUnregisteredTarget(t *testing.T) {
	ctx := context.Background()
	v := workerCloneVM()
	v.Closing = true
	v.Native = ObjectRef{}
	target := ObjectRef{Kind: "VirtualMachine", Namespace: "tenant-a", Name: cloneTestNativeN, UID: "pending-target-uid"}
	v.CloneProgress = &CloneProgress{Name: CloneName(v.ID), UID: "clone-uid",
		CloneDeleteRequested: true, PendingTargetRef: &target}
	native := &cloneTestNative{observations: []CloneObservation{{Exists: false}}, targetGone: true, volumesDone: true}
	worker, repo, events := newCloneWorker(t, v, workerCloneOp(v, DeleteAction), native)
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	want := []string{"delete-target", "release:CANCELLED"}
	if strings.Join(*events, ",") != strings.Join(want, ",") {
		t.Fatalf("cancel order = %v", *events)
	}
	if !repo.vm.CloneProgress.CancelHandled || len(repo.releases) != 1 {
		t.Fatalf("cancel did not finish: %+v %v", repo.vm.CloneProgress, repo.releases)
	}
}

// TestCloneWorkerCancelAdoptsLateTarget covers R3: the last clone observation
// may run before the native restore creates the target, and deleting the clone
// never deletes the target. The cancel hand-off therefore reads the frozen target
// name once, adopts its real UID, and deletes it on a later tick before any disk
// cleanup or source release.
func TestCloneWorkerCancelAdoptsLateTarget(t *testing.T) {
	ctx := context.Background()
	late := ObjectRef{Kind: "VirtualMachine", Namespace: "tenant-a", Name: cloneTestNativeN, UID: "late-target-uid"}
	v := workerCloneVM()
	v.Closing = true
	v.Native = ObjectRef{}
	v.CloneProgress = &CloneProgress{Name: CloneName(v.ID), UID: "clone-uid",
		CloneDeleteRequested: true,
		PendingVolumes:       []VolumeRef{{VolumeName: "disk-sys", PVCName: "restore-sys", PVCUID: "pvc-uid-1"}}}
	native := &cloneTestNative{
		observations: []CloneObservation{{Exists: false}, {Exists: false}},
		targets:      []ObjectRef{late},
		targetGone:   true,
		volumesDone:  true,
	}
	worker, repo, events := newCloneWorker(t, v, workerCloneOp(v, DeleteAction), native)
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if !hasEvent(*events, "observe-target") {
		t.Fatalf("late target was never read: %v", *events)
	}
	if hasEvent(*events, "delete-volumes") || hasEvent(*events, "release:CANCELLED") {
		t.Fatalf("cancel continued before the late target was adopted: %v", *events)
	}
	if repo.vm.CloneProgress.PendingTargetRef == nil || repo.vm.CloneProgress.PendingTargetRef.UID != late.UID {
		t.Fatalf("late target identity was not persisted: %+v", repo.vm.CloneProgress.PendingTargetRef)
	}
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	want := []string{"observe-target", "delete-target", "delete-volumes", "release:CANCELLED"}
	if strings.Join(*events, ",") != strings.Join(want, ",") {
		t.Fatalf("cancel order = %v, want %v", *events, want)
	}
	if !repo.vm.CloneProgress.CancelHandled || len(repo.releases) != 1 {
		t.Fatalf("cancel did not finish: %+v %v", repo.vm.CloneProgress, repo.releases)
	}
}

// TestCloneWorkerCancelTargetReadErrorKeepsHeld covers the same hand-off when the
// frozen target read fails: nothing is deleted or released on a read that proves
// neither presence nor absence.
func TestCloneWorkerCancelTargetReadErrorKeepsHeld(t *testing.T) {
	ctx := context.Background()
	v := workerCloneVM()
	v.Closing = true
	v.Native = ObjectRef{}
	v.CloneProgress = &CloneProgress{Name: CloneName(v.ID), UID: "clone-uid", CloneDeleteRequested: true,
		PendingVolumes: []VolumeRef{{VolumeName: "disk-sys", PVCName: "restore-sys", PVCUID: "pvc-uid-1"}}}
	native := &cloneTestNative{
		observations:     []CloneObservation{{Exists: false}},
		targetObserveErr: errors.New("target read failed"),
		targetGone:       true,
		volumesDone:      true,
	}
	worker, repo, events := newCloneWorker(t, v, workerCloneOp(v, DeleteAction), native)
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if hasEvent(*events, "delete-volumes") || len(repo.releases) != 0 || repo.vm.CloneProgress.CancelHandled {
		t.Fatalf("failed target read still finished the cancel: %v %v", *events, repo.releases)
	}
	if repo.op.State != "running" {
		t.Fatalf("state = %s", repo.op.State)
	}
}

// TestCloneWorkerCancelGoneAfterOwnDelete covers R4: our own delete completing
// is a legitimate early cancel, while an unrecorded disappearance keeps HELD.
func TestCloneWorkerCancelGoneAfterOwnDelete(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		name         string
		deleteAsked  bool
		pending      []VolumeRef
		wantRelease  bool
		wantCanceled bool
	}{
		{name: "our own delete completed", deleteAsked: true, wantRelease: true, wantCanceled: true},
		{name: "recorded outputs without delete intent", pending: []VolumeRef{{VolumeName: "disk-sys", PVCName: "pvc-sys", PVCUID: "uid-1"}}, wantRelease: true, wantCanceled: true},
		{name: "unrecorded disappearance", wantRelease: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			v := workerCloneVM()
			v.Closing = true
			v.Native = ObjectRef{}
			v.CloneProgress = &CloneProgress{Name: CloneName(v.ID), UID: "clone-uid",
				CloneDeleteRequested: test.deleteAsked, PendingVolumes: test.pending}
			native := &cloneTestNative{observations: []CloneObservation{{Exists: false}}, volumesDone: true}
			worker, repo, _ := newCloneWorker(t, v, workerCloneOp(v, DeleteAction), native)
			if err := worker.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			if test.wantRelease {
				if len(repo.releases) != 1 || repo.releases[0].Kind != "CANCELLED" {
					t.Fatalf("release = %v", repo.releases)
				}
				if repo.vm.CloneProgress.CancelHandled != test.wantCanceled {
					t.Fatalf("cancel flag = %v", repo.vm.CloneProgress.CancelHandled)
				}
				return
			}
			if len(repo.releases) != 0 || repo.op.State != "unknown" {
				t.Fatalf("unrecorded disappearance was released: %v %s", repo.releases, repo.op.State)
			}
		})
	}
}

// TestCloneWorkerRollbackClosesConsumer covers R5: both rollback paths hand the
// Network consumer off as closed before releasing the Attachment, because the
// real adapter refuses a release while the persisted consumer is still open.
func TestCloneWorkerRollbackClosesConsumer(t *testing.T) {
	ctx := context.Background()
	check := func(t *testing.T, events []string) {
		t.Helper()
		for _, event := range events {
			if event == "release-network-without-closed-consumer" {
				t.Fatalf("release ran before the consumer was closed: %v", events)
			}
		}
		if !hasEvent(events, "release-network") {
			t.Fatalf("network was never released: %v", events)
		}
	}
	t.Run("unsubmitted rollback", func(t *testing.T) {
		v := workerCloneVM()
		v.Native = ObjectRef{}
		native := &cloneTestNative{eligibility: CloneEligibility{Unsupported: []CloneBlocker{
			{Kind: "UNSUPPORTED", Reason: "source uses dataVolumeTemplates"},
		}}}
		worker, repo, events := newCloneWorker(t, v, workerCloneOp(v, CreateAction), native)
		for i := 0; i < 5 && repo.op.State != "failed"; i++ {
			if err := worker.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
		}
		check(t, *events)
	})
	t.Run("failed clone cleanup", func(t *testing.T) {
		v := workerCloneVM()
		v.Native = ObjectRef{}
		outputs := []VolumeRef{
			{VolumeName: "disk-sys", PVCName: "pvc-sys", PVCUID: "uid-1"},
			{VolumeName: "disk-data", PVCName: "pvc-data", PVCUID: "uid-2"},
		}
		v.CloneProgress = &CloneProgress{Name: CloneName(v.ID), UID: "clone-uid", CloneDeleteRequested: true,
			PendingVolumes: outputs}
		native := &cloneTestNative{
			observations: []CloneObservation{{Exists: true, UID: "clone-uid", Failed: true, Reason: "boom", Outputs: outputs}},
			targetGone:   true, volumesDone: true,
		}
		worker, repo, events := newCloneWorker(t, v, workerCloneOp(v, CreateAction), native)
		for i := 0; i < 12 && repo.op.State != "failed"; i++ {
			if err := worker.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
		}
		if repo.op.State != "failed" {
			t.Fatalf("failed cleanup did not finish: %s %v", repo.op.State, *events)
		}
		check(t, *events)
	})
}

// TestCloneEligibilityErrorKinds keeps the two admission answers distinct.
func TestCloneEligibilityErrorKinds(t *testing.T) {
	unsupported := CloneEligibility{Unsupported: []CloneBlocker{{VolumeName: "disk-sys", Reason: "source PVC volumeMode is not Block"}}}
	if err := unsupported.UnsupportedError(); !errors.Is(err, ErrCloneSourceUnavailable) || !strings.Contains(err.Error(), "disk-sys") {
		t.Fatalf("unsupported error = %v", err)
	}
	notReady := CloneEligibility{NotReady: []CloneBlocker{{VolumeName: "disk-sys", Reason: "source PVC is not Bound"}}}
	if err := notReady.NotReadyError(); !errors.Is(err, ErrCloneSourceNotReady) || !strings.Contains(err.Error(), "not Bound") {
		t.Fatalf("not-ready error = %v", err)
	}
	if unsupported.UnsupportedError() == nil || unsupported.NotReadyError() != nil {
		t.Fatal("eligibility error kinds are not separate")
	}
}
