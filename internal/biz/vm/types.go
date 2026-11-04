// Package vm owns durable VM intent and native lifecycle reconciliation.
package vm

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	network "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
)

var (
	ErrInvalid                = errors.New("VM_INVALID_REQUEST")
	ErrDenied                 = errors.New("VM_SCOPE_DENIED")
	ErrNotFound               = errors.New("VM_NOT_FOUND")
	ErrIdempotency            = errors.New("VM_IDEMPOTENCY_CONFLICT")
	ErrVersion                = errors.New("VM_VERSION_CONFLICT")
	ErrBusy                   = errors.New("VM_OPERATION_CONFLICT")
	ErrDependency             = errors.New("VM_DEPENDENCY_NOT_READY")
	ErrChanged                = errors.New("VM_DEPENDENCY_CHANGED")
	ErrOwnership              = errors.New("VM_OBJECT_OWNERSHIP_CONFLICT")
	ErrUnknown                = errors.New("VM_EXTERNAL_RESULT_UNKNOWN")
	ErrStartFailed            = errors.New("VM_NATIVE_START_FAILED")
	ErrLease                  = errors.New("VM_LEASE_LOST")
	ErrConsole                = errors.New("VM_CONSOLE_TARGET_CHANGED")
	ErrNetworkAdmissionClosed = errors.New("NETWORK_NEW_ADMISSION_CLOSED")
)

// NativeNotSubmittedError confirms that a native command did not reach its
// submission boundary. It must never describe a lost or rejected PUT response.
type NativeNotSubmittedError struct{ Err error }

func (e *NativeNotSubmittedError) Error() string { return e.Err.Error() }
func (e *NativeNotSubmittedError) Unwrap() error { return e.Err }

const Owner = "ani-resource-service"
const CreateAction = "/vm.v1.VMService/AcceptCreate"
const DeleteAction = "/vm.v1.VMService/AcceptDelete"

type Caller struct{ TenantID, Actor string }
type callerKey struct{}

func WithCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, callerKey{}, c)
}
func RequireTenant(ctx context.Context, tenant string) error {
	c, ok := ctx.Value(callerKey{}).(Caller)
	if !ok || !UUID(tenant) || c.TenantID != tenant || c.Actor == "" {
		return ErrDenied
	}
	return nil
}
func UUID(s string) bool { u, e := uuid.Parse(s); return e == nil && u != uuid.Nil && u.String() == s }

type ObjectRef struct{ Kind, Namespace, Name, UID string }
type Source struct {
	Key, Kind, ClusterID, Namespace, PVCName, PVCUID, Checksum, VolumeMode, ContentType string
	VirtualBytes, RequestedBytes                                                        int64
	OSFamily, OSVersion, Architecture, Firmware, RootBus, DataBus, NICModel             string
	PersistentTPM, PersistentUEFI                                                       bool
}
type Disk struct {
	ID, Role, Name, VolumeName, Serial            string
	SizeGB, RequestedBytes, ObservedCapacityBytes int64
	Attached, Tombstone                           bool
	DV, PVC                                       ObjectRef
	Preparation, Reason                           string
	Cleanup                                       []ObjectRef
}
type NetworkSelection struct {
	VPCID, SubnetID, NADName, NADUID, NADDigest string
	// These values belong to the owner snapshot, never the public canonical spec.
	Reference network.NetworkReference
	NADConfig string
	// Resolve returns the selected account through this transient field; the
	// accepted value lives in Spec.ServiceAccount.
	ServiceAccount string `json:"-"`
}
type DNS struct {
	Mode        string
	Nameservers []string
}
type NetworkPlan struct {
	AttachmentID, SubmissionID, BindingRevision         string
	Generation, Version, SnapshotRevision               int64
	NotifiedSnapshotRevision                            int64
	Consumer                                            *NetworkConsumerState
	Namespace, NADName, NADUID, NADDigest, KCNSubnetRef string
	BasicIsolationName                                  string
	Labels                                              map[string]string
}
type Address struct {
	NIC, Address, VMIUID, LauncherUID string
	ObservedAt                        time.Time
}
type GuestInitialization struct {
	Method, Username, SSHPublicKey, CredentialIntentDigest string
	PasswordSet                                            bool
}
type Spec struct {
	SecurityGroupIDs                                                                  []string
	Canonical                                                                         json.RawMessage
	Digest, RequestDigest, Name, PresetID, SelectionDigest                            string
	ClusterID, Namespace, NamespaceUID, ServiceAccount, StorageClass, StorageClassUID string
	BackendStorageClass, MediaStorageClass                                            string
	CPUMillis, MemoryMiB                                                              int64
	Cores                                                                             uint32
	Media                                                                             Source
	Driver                                                                            *Source
	Network                                                                           NetworkSelection
	DNS                                                                               *DNS
	BootDevice                                                                        string
	StartAfterCreate                                                                  bool
	InitialDisks                                                                      []Disk
	GuestInitialization                                                               *GuestInitialization
}
type VM struct {
	NetworkInterfaces                          []NetworkInterface
	SecurityGroupIDs, SecurityGroupNames       []string
	SecurityGroupsInitialized                  bool
	TenantID, ID, Name, CreateOperationID      string
	Spec                                       Spec
	Version                                    int64
	Closing                                    bool
	Native                                     ObjectRef
	InitializationSecret                       ObjectRef
	Network                                    NetworkPlan
	Disks                                      []Disk
	Snapshots                                  []Snapshot
	BackendStates                              []BackendState
	RetiredDisks                               []Disk
	RetiredMediaCopies                         []MediaCopy
	Media                                      []ObjectRef
	MediaSlots                                 []MediaSlot
	MediaCopies                                []MediaCopy
	CurrentBootDevice                          string
	Cleanup                                    []ObjectRef
	Desired, Observed, Reason, LastOperationID string
	Addresses                                  []Address
	ObservedAt                                 time.Time
	// CreatedAt comes from the row column; it is not part of the stored payload.
	CreatedAt time.Time `json:"-"`
}

// ListFilter is the tenant-scoped list query. State is the display-state group
// accepted by the public contract, not the raw Observed value.
type ListFilter struct {
	SubnetID, ClusterID, Namespace string
	BindableOnly                   bool
	Page, PageSize                 uint32
	State, Keyword                 string
}

// vmListStates is the closed set of display-state groups. "all" and the empty
// value mean the same thing: every state except deleted tombstones.
var vmListStates = map[string]bool{
	"pending": true, "provisioning": true, "starting": true, "stopping": true,
	"running": true, "stopped": true, "failed": true, "deleting": true, "deleted": true,
}

// Validated applies defaults and rejects values outside the public contract.
// It is shared by the repository and by callers that need to fail early.
func (f ListFilter) Validated() (ListFilter, error) {
	if f.Page == 0 {
		f.Page = 1
	}
	if f.PageSize == 0 {
		f.PageSize = 20
	}
	if f.PageSize > 100 {
		return f, ErrInvalid
	}
	f.State = strings.TrimSpace(f.State)
	if f.State == "all" {
		f.State = ""
	}
	if f.State != "" && !vmListStates[f.State] {
		return f, ErrInvalid
	}
	f.Keyword = strings.TrimSpace(f.Keyword)
	if utf8.RuneCountInString(f.Keyword) > 80 {
		return f, ErrInvalid
	}
	return f, nil
}

type Operation struct {
	TenantID, ID, VMID, Action, Key, RequestDigest string
	Intent                                         json.RawMessage
	IntentVersion                                  int64
	State, Reason, Step, OldVMIUID                 string
	Pending                                        *Mutation
	Snapshot                                       *SnapshotProgress
	Lease                                          string
	LeaseUntil                                     time.Time
	ObservedAt                                     time.Time
}
type Mutation struct {
	Action       string
	Target       ObjectRef
	Submitted    bool
	BeforeVMIUID *string // nil: legacy unknown; empty: observed absent; otherwise the pre-submit VMI UID
}
type Acceptance struct {
	TenantID, VMID, OperationID, RequestDigest, CreateOperationID string
	ExpectedVersion                                               int64
	Spec                                                          Spec
	Password                                                      string // transient write-only input; never serialized into a VM/spec
}
type Command struct {
	TenantID, VMID, Action, Key, RequestDigest string
	ExpectedVersion                            int64
	Intent                                     json.RawMessage
}
type Repository interface {
	AcceptCreate(context.Context, Acceptance) (Operation, error)
	AcceptDelete(context.Context, Acceptance) (Operation, error)
	AcceptCommand(context.Context, Command) (Operation, error)
	LookupReplay(context.Context, Command) (Operation, error)
	Get(context.Context, string, string) (VM, error)
	List(context.Context, string, ListFilter) ([]VM, int64, error)
	GetOperation(context.Context, string, string, string) (Operation, error)
	Claim(context.Context, string, time.Duration) (Operation, VM, error)
	Save(context.Context, Operation, VM) error
	Checkpoint(context.Context, Operation, VM) error
	UpdateNetwork(context.Context, VM, NetworkPlan) error
}
type Observation struct {
	VM                        ObjectRef
	VMI, Launcher             *ObjectRef
	Running, Stopped, Deleted bool
	// Terminal is an owned VMI whose runtime has exited. Manual VMs can
	// retain this object; it is not the stronger Stopped cleanup proof.
	Terminal            bool
	ObservedAt          time.Time
	State, Reason       string
	Volumes             []Disk
	BackendStates       []BackendState
	StateChangeRequests []string
}
type NativePort interface {
	ValidateSource(context.Context, Source) error
	EnsureVM(context.Context, VM) (ObjectRef, error)
	EnsureInitialization(context.Context, VM) (ObjectRef, error)
	DeleteInitialization(context.Context, VM) (bool, error)
	EnsureVolume(context.Context, VM, Disk) (Disk, error)
	Observe(context.Context, VM) (Observation, error)
	Start(context.Context, VM) error
	Stop(context.Context, VM, bool) error
	Restart(context.Context, VM) error
	Delete(context.Context, VM) error
	DeleteVolume(context.Context, VM, Disk) (bool, error)
}
type NetworkPort interface {
	Prepare(context.Context, VM) (NetworkPlan, error)
	Confirm(context.Context, VM, Observation) error
	CurrentAddresses(context.Context, VM) ([]Address, error)
	Release(context.Context, VM) (bool, error)
}
type EnvironmentPort interface {
	ResolveNamespace(context.Context, string, string) (ObjectRef, error)
	ValidateStorage(context.Context, string, string) error
	ValidateNetwork(context.Context, string, string, string, NetworkSelection) (NetworkSelection, error)
	ValidateExistingNetwork(context.Context, VM) error
}
