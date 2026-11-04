package biz

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/google/uuid"
	"regexp"
	"strings"
	"time"
)

type AttachmentState string

const (
	Reserved            AttachmentState = "reserved"
	Attached            AttachmentState = "attached"
	Releasing           AttachmentState = "releasing"
	Released            AttachmentState = "released"
	AttachmentConflict  Reason          = "ATTACHMENT_CONFLICT"
	VersionConflict     Reason          = "VERSION_CONFLICT"
	ConsumerUnclosed    Reason          = "CONSUMER_NOT_CLOSED"
	ConsumerUnavailable Reason          = "CONSUMER_UNAVAILABLE"
	AttachmentProtocol  Reason          = "ATTACHMENT_PROTOCOL_VIOLATION"
)

// PodPrimaryPlan is a closed delivery DTO. Infrastructure owns the mapping to
// annotation keys; business logic neither builds nor merges arbitrary patches.
type PodPrimaryPlan struct {
	BasicIsolationName                               string `json:",omitempty"`
	FormatVersion                                    int32
	WorkloadMode                                     string `json:",omitempty"`
	Namespace, PrimaryNetworkRef                     string
	TenantID, InstanceID, AttachmentID, SubmissionID string
	Generation                                       int64
	VMPrimary                                        *VMPrimaryPlan `json:",omitempty"`
}
type Attachment struct {
	BasicIsolationName                                            string
	Reference                                                     NetworkReference
	CurrentVNicName, CurrentVNicUID, CurrentMACAddress            string
	CurrentIPs                                                    []string
	VMPrimary                                                     *VMPrimaryPlan
	VMCurrentAddresses                                            []VMCurrentAddress
	WorkloadMode                                                  string
	ConsumerType                                                  string
	SnapshotRevision                                              int64
	ConfirmedSnapshotRevision                                     int64
	ConfirmedPodUID                                               string
	ConsumerSnapshot                                              *ConsumerSubmission
	ID, TenantID, VPCID, SubnetID, InstanceID, Slot, SubmissionID string
	Generation                                                    int64
	ClusterID, Namespace, BindingRevision                         string
	State                                                         AttachmentState
	Reason                                                        Reason
	Version                                                       int64
	Plan                                                          *PodPrimaryPlan
	PodName, PodUID, ConfirmUID, FinalizationID                   string
	CreatedAt, UpdatedAt                                          time.Time
	ObservedAt, ReleasedAt                                        *time.Time
}
type PrepareAttachment struct {
	Accepted                                                bool             `json:"-"`
	NamespaceUID                                            string           `json:"-"`
	AcceptedReference                                       NetworkReference `json:"-"`
	WorkloadMode                                            string           `json:",omitempty"`
	TenantID, InstanceID, SubnetID, VPCID, Slot, RequestKey string
	SubmissionID                                            string
	Generation                                              int64
	ClusterID, Namespace                                    string
	VMExpectation                                           *VMNetworkExpectation `json:",omitempty"`
	// ConsumerType is derived by the caller's domain, never part of the intent
	// digest: excluding it keeps replay of a stored attachment idempotent.
	ConsumerType string `json:"-"`
}

func (r PrepareAttachment) Fingerprint() string {
	r.RequestKey = ""
	b, _ := json.Marshal(struct {
		Version int
		Intent  PrepareAttachment
	}{1, r})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

type ConfirmAttachment struct {
	ExpectedSnapshotRevision                                      int64
	TenantID, AttachmentID, ClusterID, Namespace, PodName, PodUID string
	ExpectedVersion                                               int64
}
type ReleaseAttachment struct {
	TenantID, AttachmentID, FinalizationID string
	ExpectedVersion                        int64
}
type AttachmentRepository interface {
	PrepareAttachment(context.Context, PrepareAttachment) (Attachment, error)
	GetAttachment(context.Context, string, string) (Attachment, error)
	ConfirmAttachment(context.Context, ConfirmAttachment) (Attachment, error)
	ReleaseAttachment(context.Context, ReleaseAttachment) (Attachment, error)
	ListAttachmentConsumers(context.Context, string, AttachmentConsumerFilter) ([]AttachmentConsumerRow, int64, error)
}
type Attachments struct {
	repository AttachmentRepository
	consumers  ConsumerDirectory
}

func NewAttachments(r AttachmentRepository) *Attachments {
	return &Attachments{repository: r}
}

// Notify schedules an already registered Attachment after its consumer owner
// has committed a new current snapshot. It does not inspect provider state.
func (a *Attachments) Notify(ctx context.Context, tenant, id string) error {
	notifier, ok := a.repository.(interface {
		NotifyAttachment(context.Context, string, string) error
	})
	if !ok {
		return Fail(DependencyUnavailable, "attachment notifications unavailable")
	}
	return notifier.NotifyAttachment(ctx, tenant, id)
}

var attachmentName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`)
var namespaceName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// validConsumerIntent binds a product identity to the attachment protocol that
// carries it. The legacy external single_pod intent predates this column and
// owns no ANI resource record, so it stays untyped rather than borrowing a type
// inferred from whichever table happens to hold a matching key.
func validConsumerIntent(mode, consumerType string) error {
	switch mode {
	case "":
		if consumerType != "" {
			return Fail(InvalidArgument, "external single_pod attachment carries no consumer type")
		}
	case "controller_managed":
		if consumerType != ConsumerContainerInstance {
			return Fail(InvalidArgument, "controller managed attachment requires a container instance consumer")
		}
	case "vm_managed":
		if consumerType != ConsumerVirtualMachine {
			return Fail(InvalidArgument, "managed VM attachment requires a virtual machine consumer")
		}
	}
	switch consumerType {
	case "", ConsumerContainerInstance, ConsumerVirtualMachine:
		return nil
	}
	return Fail(InvalidArgument, "invalid consumer type")
}

func (a *Attachments) Prepare(ctx context.Context, r PrepareAttachment) (Attachment, error) {
	r.Accepted = false
	r.AcceptedReference = NetworkReference{}
	return a.prepare(ctx, r)
}

// PrepareAccepted is an internal owner-worker entry point, never a wire flag.
func (a *Attachments) PrepareAccepted(ctx context.Context, r PrepareAttachment) (Attachment, error) {
	if r.WorkloadMode != "vm_managed" && r.WorkloadMode != "controller_managed" {
		return Attachment{}, Fail(InvalidArgument, "accepted attachment requires a persisted owner")
	}
	r.Accepted = true
	return a.prepare(ctx, r)
}

func (a *Attachments) prepare(ctx context.Context, r PrepareAttachment) (Attachment, error) {
	tenant, err := ParseTenant(r.TenantID)
	if err != nil {
		return Attachment{}, err
	}
	r.TenantID = tenant
	if r.WorkloadMode != "" && r.WorkloadMode != "single_pod" && r.WorkloadMode != "controller_managed" && r.WorkloadMode != "vm_managed" {
		return Attachment{}, Fail(InvalidArgument, "invalid attachment workload mode")
	}
	// Empty is the immutable v1 spelling and remains omitted from its digest.
	if r.WorkloadMode == "single_pod" {
		r.WorkloadMode = ""
	}
	if e := validConsumerIntent(r.WorkloadMode, r.ConsumerType); e != nil {
		return Attachment{}, e
	}
	if (r.WorkloadMode == "vm_managed" && !ValidVMNetworkExpectation(r.VMExpectation)) || (r.WorkloadMode != "vm_managed" && r.VMExpectation != nil) {
		return Attachment{}, Fail(InvalidArgument, "VM network expectation differs from workload mode")
	}
	submission, err := uuid.Parse(r.SubmissionID)
	if err != nil || submission == uuid.Nil || r.SubmissionID != submission.String() || r.Generation < 1 || !attachmentName.MatchString(r.InstanceID) || r.Slot != "primary" || strings.TrimSpace(r.RequestKey) != r.RequestKey || len(r.RequestKey) < 1 || len(r.RequestKey) > 128 || !validSubnetID(r.SubnetID) || (r.VPCID != "" && !validVPCID(r.VPCID)) || strings.TrimSpace(r.ClusterID) == "" || len(r.ClusterID) > 128 || !namespaceName.MatchString(r.Namespace) {
		return Attachment{}, Fail(InvalidArgument, "invalid attachment intent")
	}
	return a.repository.PrepareAttachment(ctx, r)
}
func attachmentScope(tenant, id string) (string, error) {
	t, e := ParseTenant(tenant)
	if e != nil {
		return "", e
	}
	if !strings.HasPrefix(id, "att_") || len(id) != 36 {
		return "", Fail(ResourceNotFound, "attachment not found")
	}
	if _, e := hex.DecodeString(id[4:]); e != nil {
		return "", Fail(ResourceNotFound, "attachment not found")
	}
	return t, nil
}
func (a *Attachments) Get(ctx context.Context, t, id string) (Attachment, error) {
	t, e := attachmentScope(t, id)
	if e != nil {
		return Attachment{}, e
	}
	return a.repository.GetAttachment(ctx, t, id)
}
func (a *Attachments) Confirm(ctx context.Context, r ConfirmAttachment) (Attachment, error) {
	t, e := attachmentScope(r.TenantID, r.AttachmentID)
	if e != nil {
		return Attachment{}, e
	}
	r.TenantID = t
	if r.PodUID == "" || len(r.PodUID) > 128 || !namespaceName.MatchString(r.PodName) || r.ExpectedVersion < 1 {
		return Attachment{}, Fail(InvalidArgument, "immutable Pod identity and version required")
	}
	return a.repository.ConfirmAttachment(ctx, r)
}
func (a *Attachments) Release(ctx context.Context, r ReleaseAttachment) (Attachment, error) {
	t, e := attachmentScope(r.TenantID, r.AttachmentID)
	if e != nil {
		return Attachment{}, e
	}
	r.TenantID = t
	id, e := uuid.Parse(r.FinalizationID)
	if e != nil || id == uuid.Nil || r.FinalizationID != id.String() || r.ExpectedVersion < 1 {
		return Attachment{}, Fail(InvalidArgument, "stable finalization identity and version required")
	}
	return a.repository.ReleaseAttachment(ctx, r)
}
