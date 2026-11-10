package vm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	ErrImageInvalid       = errors.New("VM_IMAGE_INVALID_ARGUMENT")
	ErrImageNotFound      = errors.New("VM_IMAGE_NOT_FOUND")
	ErrImageVersion       = errors.New("VM_VERSION_CONFLICT")
	ErrImageIdempotency   = errors.New("VM_IDEMPOTENCY_CONFLICT")
	ErrImageNotUploadable = errors.New("VM_IMAGE_NOT_UPLOADABLE")
	ErrImageInUse         = errors.New("VM_IMAGE_IN_USE")
	ErrImageNotReady      = errors.New("VM_IMAGE_NOT_READY")
	ErrImageKindMismatch  = errors.New("VM_IMAGE_KIND_MISMATCH")
	ErrImageDependency    = errors.New("VM_IMAGE_DEPENDENCY_UNAVAILABLE")
	ErrImageUploadOff     = errors.New("VM_IMAGE_UPLOAD_DISABLED")
	ErrImageIdentity      = errors.New("VM_IMAGE_NATIVE_IDENTITY_CHANGED")
	ErrImageCleanup       = errors.New("NATIVE_CLEANUP_BLOCKED")
)

// ImageMediaKeyPrefix is reserved for tenant-uploaded images. A statically
// configured media key using it is a deployment error and fails startup.
const ImageMediaKeyPrefix = "vmimg:"

const (
	ImageCreateAction  = "/vm.v1.VMImageService/CreateImage"
	ImageSessionAction = "/vm.v1.VMImageService/CreateUploadSession"
	ImageUpdateAction  = "/vm.v1.VMImageService/UpdateImage"
	ImageDeleteAction  = "/vm.v1.VMImageService/DeleteImage"
)

const (
	StatePreparing = "PREPARING"
	StateImporting = "IMPORTING"
	StateVerifying = "VERIFYING"
	StateReady     = "READY"
	StateFailed    = "FAILED"
	StateUnavail   = "UNAVAILABLE"
	StateDeleting  = "DELETING"
	StateDeleted   = "DELETED"
)

// Initialization capabilities a tenant may declare for an uploaded image. The
// declaration is frozen at create time; an empty stored value (legacy rows and
// the column default) reads as InitNone everywhere it is projected.
const (
	ImageInitNone    = "NONE"
	ImageInitNoCloud = "CLOUD_INIT_NOCLOUD"
)

const (
	CleanupNone     = "NONE"
	CleanupPending  = "PENDING"
	CleanupRunning  = "RUNNING"
	CleanupComplete = "COMPLETE"
	CleanupBlocked  = "BLOCKED"
)

// imageInFlight is the only set an upload deadline may move to FAILED. Writing
// "anything but READY" would also clear UNAVAILABLE, which has no deadline.
var imageInFlight = map[string]bool{StatePreparing: true, StateImporting: true, StateVerifying: true}

var imageStates = map[string]bool{
	StatePreparing: true, StateImporting: true, StateVerifying: true, StateReady: true,
	StateFailed: true, StateUnavail: true, StateDeleting: true, StateDeleted: true,
}

// imageKinds binds the two consumption shapes to their only accepted format.
var imageKinds = map[string]string{"DISK_IMAGE": "qcow2", "INSTALL_ISO": "iso"}

// Image is one tenant-owned image plus the native identity chain behind it.
// The four capacity measures stay independent: the declared DV input, the final
// PVC request, what the cluster reports and how many bytes the hash covers.
type Image struct {
	TenantID, ID, MediaKey, Kind, Format, Name, Description string
	State, Reason, CleanupState                             string
	InitializationCapability                                string
	SourceCapacityGB, SourceRequestedBytes                  int64
	ObservedSourceCapacityBytes, SourceChecksumBytes        int64
	SourceSHA256                                            string
	OS                                                      ImageOS
	Native                                                  ImageNative
	Cleanup                                                 []ObjectRef
	Version                                                 int64
	Lease                                                   string
	LeaseUntil, NextAttemptAt                               time.Time
	UploadDeadline                                          *time.Time
	AttemptCount                                            int
	CreatedBy                                               string
	CreatedAt, UpdatedAt                                    time.Time
}

// ImageInitializationCapabilityFor is the single projection of the declared
// capability: an absent declaration reads as NONE, never as an unknown state.
func ImageInitializationCapabilityFor(declared string) string {
	if declared == ImageInitNoCloud {
		return ImageInitNoCloud
	}
	return ImageInitNone
}

type ImageOS struct {
	OSFamily, OSVersion, Architecture, Firmware, RootBus, DataBus, NICModel string
}

// ImageNative freezes the name + UID + namespace triple for each owned object.
// An empty UID means "not observed yet"; it never means "any object matches".
type ImageNative struct {
	ClusterID, Namespace, NamespaceUID            string
	DVName, DVUID, PVCName, PVCUID, PVName, PVUID string
	JobName, JobUID, PodUID                       string
	TokenUID, TokenName, SessionID                string
	DVPhase                                       string
	VerifierImage                                 string
	SourceVolumeMode, SourceContentType           string
	AnnotationResourceVersion                     string
	ReadyPublishedAt                              time.Time
}

func ImageMediaKey(id string) string { return ImageMediaKeyPrefix + id }

// ImageFormatFor is the single kind-to-format table for every layer.
func ImageFormatFor(kind string) (string, bool) {
	format, ok := imageKinds[kind]
	return format, ok
}

// Validated is the single gate for a stored or accepted image record. It never
// repairs a value that a caller should have rejected at the contract edge.
func (i Image) Validated() (Image, error) {
	if !UUID(i.ID) || !UUID(i.TenantID) {
		return Image{}, ErrImageInvalid
	}
	if i.MediaKey != ImageMediaKey(i.ID) {
		return Image{}, ErrImageInvalid
	}
	format, ok := imageKinds[i.Kind]
	if !ok || i.Format != format {
		return Image{}, ErrImageInvalid
	}
	if name := strings.TrimSpace(i.Name); name == "" || utf8.RuneCountInString(name) > 80 {
		return Image{}, ErrImageInvalid
	}
	if utf8.RuneCountInString(i.Description) > 512 {
		return Image{}, ErrImageInvalid
	}
	if i.SourceCapacityGB < 1 {
		return Image{}, ErrImageInvalid
	}
	if !imageStates[i.State] {
		return Image{}, ErrImageInvalid
	}
	switch i.CleanupState {
	case CleanupNone, CleanupPending, CleanupRunning, CleanupComplete, CleanupBlocked:
	default:
		return Image{}, ErrImageInvalid
	}
	// The declared capability is tenant-attested, not verified by the platform.
	// NoCloud stays bound to a linux disk image: an ISO or a Windows image has
	// no guest agent contract this initialization path can rely on.
	switch i.InitializationCapability {
	case "", ImageInitNone, ImageInitNoCloud:
	default:
		return Image{}, ErrImageInvalid
	}
	if i.InitializationCapability == ImageInitNoCloud && (i.Kind != "DISK_IMAGE" || i.OS.OSFamily != "linux") {
		return Image{}, ErrImageInvalid
	}
	if i.Version < 1 {
		return Image{}, ErrImageInvalid
	}
	if !ValidImageReason(i.Reason) {
		return Image{}, ErrImageInvalid
	}
	if i.State == StateReady && !IsHex(i.SourceSHA256) {
		return Image{}, ErrImageInvalid
	}
	return i, nil
}

// Source projects a READY image into the existing read-only media contract.
// The initialization capability travels with the image record itself (the
// tenant's frozen create-time declaration); this projection carries the source
// identity only, so the fingerprint stays independent of the declaration.
func (i Image) Source() Source {
	return Source{Key: i.MediaKey, Kind: i.Kind, ClusterID: i.Native.ClusterID, Namespace: i.Native.Namespace,
		PVCName: i.Native.PVCName, PVCUID: i.Native.PVCUID, Checksum: i.SourceSHA256,
		VolumeMode: SourceVolumeMode(i.Kind), ContentType: "kubevirt",
		VirtualBytes: i.ObservedSourceCapacityBytes, RequestedBytes: i.SourceRequestedBytes,
		OSFamily: i.OS.OSFamily, OSVersion: i.OS.OSVersion, Architecture: i.OS.Architecture,
		Firmware: i.OS.Firmware, RootBus: i.OS.RootBus, DataBus: i.OS.DataBus, NICModel: i.OS.NICModel}
}

// SourceVolumeMode is the one storage shape CDI accepts per image kind: a qcow2
// stays a raw block device, an ISO stays a filesystem holding disk.img.
func SourceVolumeMode(kind string) string {
	if kind == "DISK_IMAGE" {
		return "Block"
	}
	return "Filesystem"
}

func (i Image) DeletionRequested() bool {
	return i.State == StateDeleting || i.State == StateDeleted
}

// Uploadable is the state window a ticket may be signed in: the record is still
// accepting bytes and no delete has been requested. A finished upload drops out
// because verification has already begun.
func (i Image) Uploadable() bool {
	return !i.DeletionRequested() && (i.State == StatePreparing || i.State == StateImporting) && i.Native.DVPhase != "Succeeded"
}

// ReadyForVerification requires the upload to have finished and the final volume
// to be provisioned. A bound PVC alone is not enough: the DV must have Succeeded.
func (i Image) ReadyForVerification() bool {
	return i.Native.DVPhase == "Succeeded" && i.Native.PVCUID != "" &&
		i.SourceRequestedBytes >= 1 && i.ObservedSourceCapacityBytes >= i.SourceRequestedBytes &&
		i.Native.SourceVolumeMode == SourceVolumeMode(i.Kind)
}

// ImageOSProfile is the platform profile frozen for one uploaded image. Windows
// images require UEFI because that is the only firmware the platform boots.
func ImageOSProfile(osFamily, osVersion, architecture string) (ImageOS, error) {
	profile := ImageOS{OSFamily: strings.TrimSpace(osFamily), OSVersion: strings.TrimSpace(osVersion),
		Architecture: strings.TrimSpace(architecture), Firmware: "bios", RootBus: "virtio", DataBus: "scsi", NICModel: "virtio"}
	if profile.Architecture == "" {
		profile.Architecture = "amd64"
	}
	if profile.OSFamily != "linux" && profile.OSFamily != "windows" {
		return ImageOS{}, ErrImageInvalid
	}
	if profile.OSVersion == "" || utf8.RuneCountInString(profile.OSVersion) > 40 {
		return ImageOS{}, ErrImageInvalid
	}
	if profile.OSFamily == "windows" {
		profile.Firmware = "uefi"
	}
	return profile, nil
}

type ImageListFilter struct {
	Page, PageSize uint32
	Kind, State    string
	Keyword        string
}

func (f ImageListFilter) Validated() (ImageListFilter, error) {
	if f.Page == 0 {
		f.Page = 1
	}
	if f.PageSize == 0 {
		f.PageSize = 20
	}
	if f.PageSize > 100 {
		return f, ErrImageInvalid
	}
	f.Kind = strings.TrimSpace(f.Kind)
	if f.Kind != "" {
		if _, ok := imageKinds[f.Kind]; !ok {
			return f, ErrImageInvalid
		}
	}
	f.State = strings.TrimSpace(f.State)
	if f.State == "all" {
		f.State = ""
	}
	if f.State != "" && !imageStates[f.State] {
		return f, ErrImageInvalid
	}
	f.Keyword = strings.TrimSpace(f.Keyword)
	if utf8.RuneCountInString(f.Keyword) > 80 {
		return f, ErrImageInvalid
	}
	return f, nil
}

// ImageCommand is the idempotent unit for create, session, update and delete.
type ImageCommand struct {
	TenantID, ImageID, Action, Key, RequestDigest string
	ExpectedVersion                               int64
	Mask                                          []string
	Intent                                        json.RawMessage
}

func IsImageCommandAction(action string) bool {
	return action == ImageCreateAction || action == ImageSessionAction || action == ImageUpdateAction || action == ImageDeleteAction
}

// ImageRequestDigest binds the action, the trusted tenant, the path target, the
// normalized business fields, the expected version and the sorted update mask.
// A repeated mask item is rejected before hashing, not silently deduplicated.
func ImageRequestDigest(action, tenant, imageID string, fields any, expectedVersion int64, mask []string) (string, error) {
	if !IsImageCommandAction(action) {
		return "", ErrImageInvalid
	}
	seen := map[string]bool{}
	for _, item := range mask {
		if item == "" || seen[item] {
			return "", ErrImageInvalid
		}
		seen[item] = true
	}
	normalized := append([]string(nil), mask...)
	sort.Strings(normalized)
	if imageID != "" && !UUID(imageID) {
		return "", ErrImageInvalid
	}
	// A create names no target in its request: the server allocates the id. Binding
	// that allocation into the digest made every identical retry look like a different
	// request, so a lost response could never be replayed. Updates and deletes keep
	// binding it: there the id is the client's path target.
	target := imageID
	if action == ImageCreateAction {
		target = ""
	}
	payload, e := json.Marshal(struct {
		Action          string   `json:"action"`
		Tenant          string   `json:"tenant"`
		Target          string   `json:"target"`
		Fields          any      `json:"fields"`
		ExpectedVersion int64    `json:"expected_version"`
		Mask            []string `json:"update_mask"`
	}{action, tenant, target, fields, expectedVersion, normalized})
	if e != nil {
		return "", ErrImageInvalid
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

// UploadDeadlineElapsed applies only to the three in-flight states.
func UploadDeadlineElapsed(i Image, now time.Time) bool {
	return i.UploadDeadline != nil && imageInFlight[i.State] && now.After(*i.UploadDeadline)
}

// ImageTransition reports whether one state move is legal. Staying is legal
// because every worker step is re-entrant and may repeat before it advances.
func ImageTransition(from, to string) bool {
	if from == to {
		return true
	}
	switch from {
	case StatePreparing:
		return to == StateImporting || to == StateFailed || to == StateDeleting
	case StateImporting:
		return to == StateVerifying || to == StateFailed || to == StateDeleting
	case StateVerifying:
		return to == StateReady || to == StateFailed || to == StateDeleting
	case StateReady:
		return to == StateUnavail || to == StateDeleting
	case StateFailed, StateUnavail:
		return to == StateDeleting
	case StateDeleting:
		return to == StateDeleted
	}
	return false
}

func (i *Image) Move(to string) error {
	if !ImageTransition(i.State, to) {
		return ErrImageIdentity
	}
	i.State = to
	return nil
}

// PublishReady is the only path to READY and demands all five evidence values:
// the final PVC UID, its request, the observed capacity, the covered length and
// the full-length checksum. A late result after delete or deadline wins is
// rejected here, not merely ignored by the caller.
func (i *Image) PublishReady(pvcUID string, requested, observed, checksumBytes int64, checksum string, now time.Time) error {
	if i.State != StateVerifying || i.DeletionRequested() {
		return ErrImageNotReady
	}
	if pvcUID == "" || i.Native.PVCUID != "" && i.Native.PVCUID != pvcUID ||
		requested < 1 || observed < requested || checksumBytes < 1 || !IsHex(checksum) {
		return ErrImageIdentity
	}
	i.Native.PVCUID = pvcUID
	i.SourceRequestedBytes = requested
	i.ObservedSourceCapacityBytes = observed
	i.SourceChecksumBytes = checksumBytes
	i.SourceSHA256 = checksum
	i.Native.ReadyPublishedAt = now
	i.Reason = ""
	i.UploadDeadline = nil
	i.State = StateReady
	return nil
}

func IsHex(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, e := hex.DecodeString(s)
	return e == nil
}

// ImageReference is one HELD/RELEASED pin against a frozen source fingerprint.
type ImageReference struct {
	TenantID, VMID, ConsumerKind, ConsumerID, ImageID string
	SourceFingerprint                                 string
	State, Reason                                     string
	CreatedAt, UpdatedAt                              time.Time
}

func (r ImageReference) Validated() error {
	if !UUID(r.TenantID) || !UUID(r.VMID) || !UUID(r.ConsumerID) || !UUID(r.ImageID) {
		return ErrImageInvalid
	}
	if r.ConsumerKind != "SYSTEM_DISK" && r.ConsumerKind != "MEDIA_COPY" {
		return ErrImageInvalid
	}
	if !IsHex(r.SourceFingerprint) {
		return ErrImageInvalid
	}
	if r.State != "HELD" && r.State != "RELEASED" {
		return ErrImageInvalid
	}
	return nil
}

// ImageSourceFingerprint binds a pin to the exact verified source identity so a
// later catalog change cannot silently re-point an already admitted VM.
func ImageSourceFingerprint(i Image) string { return imageFingerprint(i.Source()) }

// ImageFingerprintOf is the same function used by the acceptance path, where
// only the already frozen source is available.
func ImageFingerprintOf(s Source) string { return imageFingerprint(s) }

func imageFingerprint(s Source) string {
	payload, _ := json.Marshal(struct {
		Namespace, PVCName, PVCUID, Checksum, VolumeMode, Kind string
	}{s.Namespace, s.PVCName, s.PVCUID, s.Checksum, s.VolumeMode, s.Kind})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// ImageIDFromMediaKey recovers the image identity from the reserved key form.
// Anything else is not an image reference and fails instead of guessing.
func ImageIDFromMediaKey(key string) (string, error) {
	if !strings.HasPrefix(key, ImageMediaKeyPrefix) {
		return "", ErrImageInvalid
	}
	id := strings.TrimPrefix(key, ImageMediaKeyPrefix)
	if !UUID(id) {
		return "", ErrImageInvalid
	}
	return id, nil
}

// NewImageReference builds one pin for a consumer volume of an admitted VM.
func NewImageReference(v VM, consumerID, kind string, source Source) (ImageReference, error) {
	if kind != "SYSTEM_DISK" && kind != "MEDIA_COPY" {
		return ImageReference{}, ErrImageInvalid
	}
	id, e := ImageIDFromMediaKey(source.Key)
	if e != nil {
		return ImageReference{}, e
	}
	if source.VolumeMode == "" || source.PVCUID == "" || !IsHex(source.Checksum) {
		return ImageReference{}, ErrImageIdentity
	}
	return ImageReference{TenantID: v.TenantID, VMID: v.ID, ConsumerKind: kind, ConsumerID: consumerID,
		ImageID: id, SourceFingerprint: imageFingerprint(source), State: "HELD"}, nil
}

// ImageCatalogPort resolves media keys for one tenant. Static configuration and
// tenant READY images share this one port so no second lookup path can appear.
type ImageCatalogPort interface {
	ResolveImageMedia(context.Context, string, string) (Image, error)
	ListReadyImages(context.Context, string) ([]Image, error)
}

// ImageRepository is the durable management port for the image catalog.
type ImageRepository interface {
	ImageCatalogPort
	ListImages(context.Context, string, ImageListFilter) ([]Image, int64, error)
	FindImage(context.Context, string, string) (Image, error)
	CreateImage(context.Context, Image, ImageCommand) (Image, error)
	RecordSession(context.Context, ImageCommand, string, time.Time) (Image, error)
	ConfirmImageSession(context.Context, Image) error
	UpdateImage(context.Context, ImageCommand, string, string, []string) (Image, error)
	DeleteImage(context.Context, ImageCommand) (Image, error)
	SaveImage(context.Context, Image) error
	ClaimImage(context.Context, string, time.Duration) (Image, bool, error)
	CountHeldReferences(context.Context, string, string) (int, error)
	ListImageReferences(context.Context, string, string) ([]ImageReference, error)
	ReleaseImageReference(context.Context, ImageReference, string) error
}

// ImageNativePort is the CDI and cluster side of the upload lifecycle.
type ImageNativePort interface {
	EnsureImageSource(context.Context, Image) (Image, error)
	CreateUploadToken(context.Context, Image, string, time.Duration) (Image, string, time.Time, error)
	ObserveImageSource(context.Context, Image) (Image, error)
	EnsureImageVerification(context.Context, Image) (Image, error)
	ObserveImageVerification(context.Context, Image) (Image, VerificationResult, error)
	PublishSourceAnnotation(context.Context, Image) error
	ObserveImageSourceIdentity(context.Context, Image) (Image, error)
	// ObserveManagedSources reports only platform source DV/PVC, verification
	// Job and upload requests, including interrupted registrations. CDI children
	// and legacy PV diagnostics are outside this business completion set.
	ObserveManagedSources(context.Context, Image) ([]ObjectRef, error)
	DeleteImageObjects(context.Context, Image) (Image, bool, error)
	// ObserveImageClone reports the consumer's own copy: completed, still pending, or
	// proven absent from the cluster. Absence is real evidence only for a retiring VM.
	ObserveImageClone(context.Context, VM, Image, ImageReference) (CloneState, error)
}

// CloneState is the three answers a clone probe may give. "Not finished" and "gone"
// are different facts: only the second one can retire a consumer's pin.
type CloneState int

const (
	ClonePending CloneState = iota
	CloneCompleted
	CloneAbsent
)

// VerificationResult is what the fixed read-only validator reports back through
// the bounded termination message. Res re-checks every identity field.
type VerificationResult struct {
	Pass           bool
	Checksum       string
	Bytes          int64
	VolumeMode     string
	PVCUID         string
	RequestedBytes int64
	ObservedBytes  int64
	FSType         string
	ReadOnlyProof  string // legacy diagnostic; not required for content validation
	JobUID         string
	PodUID         string
	VerifierImage  string
	ImageID        string
	ObservedAt     time.Time
	FailureReason  string
}

// ValidatedFor checks the result against one image before anything is adopted.
func (v VerificationResult) ValidatedFor(i Image) error {
	if v.ImageID != i.ID {
		return ErrImageIdentity
	}
	if !v.Pass {
		if !ValidImageReason(v.FailureReason) || v.FailureReason == "" {
			return ErrImageIdentity
		}
		return nil
	}
	if v.JobUID == "" || v.JobUID != i.Native.JobUID || v.PodUID == "" || v.VerifierImage == "" ||
		v.VerifierImage != i.Native.VerifierImage || !IsHex(v.Checksum) || v.Bytes < 1 ||
		v.PVCUID == "" || v.RequestedBytes < 1 || v.ObservedBytes < v.RequestedBytes {
		return ErrImageIdentity
	}
	if v.VolumeMode != SourceVolumeMode(i.Kind) {
		return ErrImageIdentity
	}
	if i.Kind == "INSTALL_ISO" && v.FSType != "iso9660" && v.FSType != "udf" {
		return ErrImageIdentity
	}
	if i.Kind == "DISK_IMAGE" && v.FSType != "" {
		return ErrImageIdentity
	}
	return nil
}

var imageReasons = map[string]bool{
	"": true, "VM_IMAGE_UPLOAD_DISABLED": true, "UPLOAD_DEADLINE_EXCEEDED": true, "UPLOAD_REJECTED": true,
	"CAPACITY_INSUFFICIENT": true, "SOURCE_VALIDATION_FAILED": true, "SOURCE_IDENTITY_CHANGED": true,
	"NAMESPACE_INVALID": true, "NATIVE_CLEANUP_BLOCKED": true, "VM_IMAGE_DEPENDENCY_UNAVAILABLE": true,
	"VM_IMAGE_NATIVE_IDENTITY_CHANGED": true, "VM_IMAGE_UNAVAILABLE": true, "VM_SUPERSEDED_BY_DELETE": true,
}

func ValidImageReason(reason string) bool { return imageReasons[reason] }

// ImageReadState is the closed set the public list filter accepts.
func ImageStateKnown(state string) bool { return state == "" || imageStates[state] }

// ImageNotFound keeps absent and cross-tenant identical so a guessed id cannot
// distinguish "no such image" from "someone else's image".
func ImageNotFound() error { return ErrImageNotFound }

// ImageCatalogConflict is raised when a static key collides with the reserved
// prefix; the deployment must be fixed rather than the key renamed.
func ImageCatalogConflict(key string) error {
	return fmt.Errorf("%w: statically configured media key %q uses the reserved image prefix", ErrImageInvalid, key)
}

// ImageStateList orders the states for deterministic index scans and tests.
func ImageStateList() []string {
	out := make([]string, 0, len(imageStates))
	for state := range imageStates {
		out = append(out, state)
	}
	sort.Strings(out)
	return out
}
