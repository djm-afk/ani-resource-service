package vm

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	pb "github.com/zhangzhe-ctrl/ani-resource-service/api/vm/v1"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// ImageUploadConfig is the service-facing projection of the deployment block.
// Every field is fixed by configuration; none may arrive from a request.
// ImageUploadConfig mirrors the deployment block written in ANI_VM_CONFIG_FILE.
// Durations stay strings here so the JSON keeps the same human form the rest of
// the file uses instead of nanosecond integers.
type ImageUploadConfig struct {
	Enabled              bool
	ExternalUploadOrigin string
	ValidatorImage       string
	UploadDeadline       string
	TokenTTL             string
	MaxFileBytes         int64
	MaxSourceGB          int64
}

// Durations resolves the two deadline inputs with the documented defaults.
func (c ImageUploadConfig) Durations() (time.Duration, time.Duration, error) {
	deadline, e := time.ParseDuration(c.UploadDeadline)
	if c.UploadDeadline == "" {
		deadline, e = 24*time.Hour, nil
	}
	if e != nil || deadline <= 0 {
		return 0, 0, biz.ErrImageInvalid
	}
	ttl, e := time.ParseDuration(c.TokenTTL)
	if c.TokenTTL == "" {
		ttl, e = 5*time.Minute, nil
	}
	if e != nil || ttl <= 0 || ttl >= deadline {
		return 0, 0, biz.ErrImageInvalid
	}
	return deadline, ttl, nil
}

// ImageService is the trusted governance adapter for the image catalog.
type ImageService struct {
	pb.UnimplementedVMImageServiceServer
	repo           biz.ImageRepository
	native         biz.ImageNativePort
	environment    biz.EnvironmentPort
	upload         ImageUploadConfig
	tokenTTL       time.Duration
	uploadDeadline time.Duration
	cluster        string
	logger         *slog.Logger
}

// imageCommandOf builds one write's replay record. The digest and the stored intent
// must come from the same normalized value: the data layer refuses a command whose
// intent is not valid JSON, and a replay is only provable against the original payload.
func imageCommandOf(action, tenant, id, key string, fields any, version int64, mask []string) (biz.ImageCommand, error) {
	digest, e := biz.ImageRequestDigest(action, tenant, id, fields, version, mask)
	if e != nil {
		return biz.ImageCommand{}, e
	}
	intent, e := json.Marshal(fields)
	if e != nil {
		return biz.ImageCommand{}, biz.ErrImageInvalid
	}
	return biz.ImageCommand{TenantID: tenant, ImageID: id, Action: action, Key: key, RequestDigest: digest,
		ExpectedVersion: version, Mask: mask, Intent: intent}, nil
}

func NewImageService(repo biz.ImageRepository, native biz.ImageNativePort, environment biz.EnvironmentPort, upload ImageUploadConfig, cluster string, logger *slog.Logger) (*ImageService, error) {
	if repo == nil || native == nil || environment == nil || cluster == "" || logger == nil {
		return nil, biz.ErrImageInvalid
	}
	if upload.Enabled && (!strings.HasPrefix(upload.ExternalUploadOrigin, "https://") || !dataImageNative(upload.ValidatorImage)) {
		return nil, biz.ErrImageInvalid
	}
	deadline, ttl, e := upload.Durations()
	if e != nil {
		return nil, e
	}
	upload.UploadDeadline, upload.TokenTTL = deadline.String(), ttl.String()
	return &ImageService{repo: repo, native: native, environment: environment, upload: upload, tokenTTL: ttl,
		uploadDeadline: deadline, cluster: cluster, logger: logger}, nil
}

func dataImageNative(image string) bool {
	at := strings.LastIndex(image, "@")
	// "@sha256:<64 hex>" is 72 characters from the '@'; measuring from '@' instead of
	// after it demanded a 63-hex digest, so every real digest-pinned image was rejected
	// and the service refused to start as soon as ImageUpload.Enabled became true.
	return at > 0 && strings.HasPrefix(image[at+1:], "sha256:") && len(image)-at-1 == 71
}

// imageRPCError keeps one stable reason per business failure and never exposes
// a Kubernetes status string or a native object identity.
func imageRPCError(e error) error {
	if e == nil {
		return nil
	}
	switch {
	case errors.Is(e, biz.ErrImageInvalid):
		return status.Error(codes.InvalidArgument, e.Error())
	case errors.Is(e, biz.ErrImageNotFound), errors.Is(e, biz.ErrNotFound):
		return status.Error(codes.NotFound, biz.ErrImageNotFound.Error())
	case errors.Is(e, biz.ErrDenied):
		return status.Error(codes.PermissionDenied, e.Error())
	case errors.Is(e, biz.ErrImageIdempotency):
		return status.Error(codes.AlreadyExists, e.Error())
	case errors.Is(e, biz.ErrImageVersion), errors.Is(e, biz.ErrVersion):
		return status.Error(codes.FailedPrecondition, e.Error())
	case errors.Is(e, biz.ErrImageInUse), errors.Is(e, biz.ErrBusy):
		return status.Error(codes.FailedPrecondition, biz.ErrImageInUse.Error())
	case errors.Is(e, biz.ErrImageNotReady), errors.Is(e, biz.ErrImageNotUploadable), errors.Is(e, biz.ErrImageKindMismatch), errors.Is(e, biz.ErrImageIdentity):
		return status.Error(codes.FailedPrecondition, e.Error())
	case errors.Is(e, biz.ErrImageUploadOff):
		// The capability is switched off, not the dependency: a 503 here would be
		// indistinguishable from Resource being unreachable, for the BFF and the operator.
		return status.Error(codes.FailedPrecondition, e.Error())
	case errors.Is(e, biz.ErrImageDependency), errors.Is(e, biz.ErrDependency), errors.Is(e, biz.ErrChanged), errors.Is(e, biz.ErrOwnership):
		return status.Error(codes.Unavailable, biz.ErrImageDependency.Error())
	}
	return status.Error(codes.Unavailable, biz.ErrImageDependency.Error())
}

func (s *ImageService) enabled() bool { return s.upload.Enabled }

// ValidateStaticMediaKeys fails startup when the static catalog already uses the
// reserved image prefix, and reports the offending key instead of renaming it.
func ValidateStaticMediaKeys(media []*pb.MediaReference) error {
	for _, item := range media {
		if item == nil {
			return biz.ErrImageInvalid
		}
		if strings.HasPrefix(item.MediaKey, biz.ImageMediaKeyPrefix) {
			return biz.ImageCatalogConflict(item.MediaKey)
		}
	}
	return nil
}

func wireImage(i biz.Image) *pb.VMImage {
	return &pb.VMImage{ImageId: i.ID, MediaKey: i.MediaKey, Name: i.Name, Description: i.Description,
		Kind: i.Kind, Format: i.Format, State: i.State, Reason: i.Reason, CleanupState: i.CleanupState,
		SourceCapacityGb: i.SourceCapacityGB, SourceRequestedBytes: i.SourceRequestedBytes,
		ObservedSourceCapacityBytes: i.ObservedSourceCapacityBytes, SourceChecksumBytes: i.SourceChecksumBytes,
		SourceSha256: i.SourceSHA256, Version: i.Version, CreatedBy: i.CreatedBy,
		InitializationCapability: biz.ImageInitializationCapabilityFor(i.InitializationCapability),
		UploadDeadline:           imageTimestamp(i.UploadDeadline),
		CreatedAt:                i.CreatedAt.UTC().Format(time.RFC3339Nano), UpdatedAt: i.UpdatedAt.UTC().Format(time.RFC3339Nano),
		Os: &pb.ImageOS{OsFamily: i.OS.OSFamily, OsVersion: i.OS.OSVersion, Architecture: i.OS.Architecture,
			Firmware: i.OS.Firmware, RootBus: i.OS.RootBus, DataBus: i.OS.DataBus, NicModel: i.OS.NICModel}}
}

// imageTimestamp keeps the absent-vs-zero distinction the upload window needs: an
// image that is not awaiting an upload has no deadline at all, and that must not be
// rendered to the client as the epoch.
func imageTimestamp(value *time.Time) string {
	if value == nil {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func wireImageOS(in *pb.ImageOS) (biz.ImageOS, error) {
	if in == nil {
		return biz.ImageOS{}, biz.ErrImageInvalid
	}
	return biz.ImageOSProfile(in.OsFamily, in.OsVersion, in.Architecture)
}

func (s *ImageService) ListImages(ctx context.Context, r *pb.ListImagesRequest) (*pb.ListImagesResponse, error) {
	if r == nil {
		return nil, imageRPCError(biz.ErrImageInvalid)
	}
	if e := biz.RequireTenant(ctx, r.TenantId); e != nil {
		return nil, imageRPCError(e)
	}
	items, total, e := s.repo.ListImages(ctx, r.TenantId, biz.ImageListFilter{Page: r.Page, PageSize: r.PageSize, Kind: r.Kind, State: r.State, Keyword: r.Keyword})
	if e != nil {
		return nil, imageRPCError(e)
	}
	out := &pb.ListImagesResponse{Total: total}
	for _, item := range items {
		out.Items = append(out.Items, wireImage(item))
	}
	if out.Items == nil {
		out.Items = []*pb.VMImage{}
	}
	return out, nil
}

func (s *ImageService) GetImage(ctx context.Context, r *pb.GetImageRequest) (*pb.VMImage, error) {
	if r == nil {
		return nil, imageRPCError(biz.ErrImageInvalid)
	}
	if e := biz.RequireTenant(ctx, r.TenantId); e != nil {
		return nil, imageRPCError(e)
	}
	i, e := s.repo.FindImage(ctx, r.TenantId, r.ImageId)
	if e != nil {
		return nil, imageRPCError(e)
	}
	return wireImage(i), nil
}

// CreateImage is idempotent and returns the full record. A replay wins before
// any state or version check so a retry never forks a second image.
// rejected is the single exit for a refused write. The published contract carries one
// reason per class, so on its own it cannot say which bound refused; the stage label is
// the only extra detail logged, and no request body, tenant, native name or credential
// ever reaches the log line.
func (s *ImageService) rejected(ctx context.Context, stage string, e error) error {
	s.logger.WarnContext(ctx, "VM image write refused", "stage", stage, "reason", e.Error())
	return imageRPCError(e)
}

func (s *ImageService) CreateImage(ctx context.Context, r *pb.CreateImageRequest) (*pb.VMImage, error) {
	if r == nil || r.IdempotencyKey == "" || len(r.IdempotencyKey) > 256 {
		return nil, s.rejected(ctx, "create_idempotency_key", biz.ErrImageInvalid)
	}
	if e := biz.RequireTenant(ctx, r.TenantId); e != nil {
		return nil, s.rejected(ctx, "create_tenant_scope", e)
	}
	if !s.enabled() {
		return nil, imageRPCError(biz.ErrImageUploadOff)
	}
	format, ok := biz.ImageFormatFor(r.Kind)
	if !ok || r.Format != format {
		return nil, s.rejected(ctx, "create_kind_format", biz.ErrImageInvalid)
	}
	if r.SourceCapacityGb < 1 || s.upload.MaxSourceGB > 0 && r.SourceCapacityGb > s.upload.MaxSourceGB {
		return nil, s.rejected(ctx, "create_capacity", biz.ErrImageInvalid)
	}
	profile, e := wireImageOS(r.Os)
	if e != nil {
		return nil, s.rejected(ctx, "create_os_profile", e)
	}
	// The declaration is frozen at create time and bound into the replay digest,
	// so a retried request cannot silently switch the initialization answer.
	capability := strings.TrimSpace(r.InitializationCapability)
	if capability == "" {
		capability = biz.ImageInitNone
	}
	if capability != biz.ImageInitNone && capability != biz.ImageInitNoCloud {
		return nil, s.rejected(ctx, "create_initialization_value", biz.ErrImageInvalid)
	}
	if capability == biz.ImageInitNoCloud && (r.Kind != "DISK_IMAGE" || profile.OSFamily != "linux") {
		return nil, s.rejected(ctx, "create_initialization_scope", biz.ErrImageInvalid)
	}
	id := uuid.NewString()
	fields := struct {
		Name, Description, Kind, Format, InitializationCapability string
		Size                                                      int64
		OS                                                        biz.ImageOS
	}{strings.TrimSpace(r.Name), r.Description, r.Kind, r.Format, capability, r.SourceCapacityGb, profile}
	command, e := imageCommandOf(biz.ImageCreateAction, r.TenantId, id, r.IdempotencyKey, fields, 0, nil)
	if e != nil {
		return nil, s.rejected(ctx, "create_command", e)
	}
	desired, e := newUploadedImageRecord(r, id, profile, biz.CallerFrom(ctx).Actor, time.Now().UTC().Add(s.uploadDeadline))
	if e != nil {
		return nil, s.rejected(ctx, "create_record", e)
	}
	i, e := s.repo.CreateImage(ctx, desired, command)
	if e != nil {
		return nil, s.rejected(ctx, "create_repository", e)
	}
	return wireImage(i), nil
}

// newUploadedImageRecord is the only place a tenant-uploaded image row is shaped: the
// media key is derived from the id, and the state machine starts at PREPARING with no
// cleanup work and version 1. Building the record field-by-field at the call site left
// out MediaKey/State/CleanupState/Version, so every create was rejected by Validated()
// with VM_IMAGE_INVALID_ARGUMENT while the data-layer tests, which seed a complete
// record, stayed green.
func newUploadedImageRecord(r *pb.CreateImageRequest, id string, profile biz.ImageOS, actor string, deadline time.Time) (biz.Image, error) {
	capability := strings.TrimSpace(r.InitializationCapability)
	if capability == "" {
		capability = biz.ImageInitNone
	}
	return biz.Image{TenantID: r.TenantId, ID: id, MediaKey: biz.ImageMediaKey(id), Name: strings.TrimSpace(r.Name),
		Description: r.Description, Kind: r.Kind, Format: r.Format, State: biz.StatePreparing, CleanupState: biz.CleanupNone,
		InitializationCapability: capability,
		Version:                  1, SourceCapacityGB: r.SourceCapacityGb, OS: profile, CreatedBy: actor, UploadDeadline: &deadline}.Validated()
}

// CreateUploadSession issues a short-lived ticket for one still-uploadable
// target. The token is returned once and never stored anywhere.
func (s *ImageService) CreateUploadSession(ctx context.Context, r *pb.CreateUploadSessionRequest) (*pb.UploadSession, error) {
	if r == nil || r.IdempotencyKey == "" || len(r.IdempotencyKey) > 256 || !biz.UUID(r.ImageId) {
		return nil, s.rejected(ctx, "session_key", biz.ErrImageInvalid)
	}
	if e := biz.RequireTenant(ctx, r.TenantId); e != nil {
		return nil, s.rejected(ctx, "session_tenant_scope", e)
	}
	if !s.enabled() {
		return nil, s.rejected(ctx, "session_switch_off", biz.ErrImageUploadOff)
	}
	command, e := imageCommandOf(biz.ImageSessionAction, r.TenantId, r.ImageId, r.IdempotencyKey, struct{}{}, 0, nil)
	if e != nil {
		return nil, s.rejected(ctx, "session_command", e)
	}
	session := uuid.NewString()
	i, e := s.repo.RecordSession(ctx, command, session, time.Now().UTC().Add(s.tokenTTL))
	if e != nil {
		return nil, s.rejected(ctx, "session_record", e)
	}
	if i.Native.DVUID == "" {
		// The target is registered but not provisioned yet: the caller polls the
		// record instead of the client guessing a native name.
		return nil, s.rejected(ctx, "session_target_pending", biz.ErrImageNotReady)
	}
	updated, token, expires, e := s.native.CreateUploadToken(ctx, i, session, s.tokenTTL)
	if e != nil {
		return nil, s.rejected(ctx, "session_token", e)
	}
	if e = s.repo.ConfirmImageSession(ctx, updated); e != nil {
		return nil, s.rejected(ctx, "session_confirm", e)
	}
	s.logger.Info("VM image upload session issued", "image_id", i.ID, "stage", "upload_ticket", "expires_at", expires.UTC().Format(time.RFC3339Nano))
	return &pb.UploadSession{ImageId: i.ID, UploadSessionId: session, Method: "POST", UploadUrl: s.uploadURL(),
		BearerToken: token, ExpiresAt: expires.UTC().Format(time.RFC3339Nano), ContentType: "application/octet-stream",
		MaxFileBytes: s.upload.MaxFileBytes}, nil
}

func (s *ImageService) uploadURL() string {
	return strings.TrimRight(s.upload.ExternalUploadOrigin, "/") + "/v1beta1/upload-async"
}

// UpdateImage accepts only name and description through the field mask.
func (s *ImageService) UpdateImage(ctx context.Context, r *pb.UpdateImageRequest) (*pb.VMImage, error) {
	if r == nil || !biz.UUID(r.ImageId) || r.IdempotencyKey == "" || len(r.IdempotencyKey) > 256 || r.ExpectedVersion < 1 {
		return nil, s.rejected(ctx, "update_key", biz.ErrImageInvalid)
	}
	if e := biz.RequireTenant(ctx, r.TenantId); e != nil {
		return nil, s.rejected(ctx, "update_tenant_scope", e)
	}
	mask, e := imageUpdateMask(r.UpdateMask)
	if e != nil {
		return nil, s.rejected(ctx, "update_mask", e)
	}
	name, description := r.Name, r.Description
	fields := struct{ Name, Description string }{name, description}
	command, e := imageCommandOf(biz.ImageUpdateAction, r.TenantId, r.ImageId, r.IdempotencyKey, fields, r.ExpectedVersion, mask)
	if e != nil {
		return nil, s.rejected(ctx, "update_command", e)
	}
	i, e := s.repo.UpdateImage(ctx, command, name, description, mask)
	if e != nil {
		return nil, s.rejected(ctx, "update_repository", e)
	}
	return wireImage(i), nil
}

// imageUpdateMask rejects empty, wildcard, unknown and repeated paths, and
// preserves the difference between an absent field and an explicit clear.
func imageUpdateMask(mask *fieldmaskpb.FieldMask) ([]string, error) {
	if mask == nil || len(mask.Paths) == 0 {
		return nil, biz.ErrImageInvalid
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(mask.Paths))
	for _, path := range mask.Paths {
		path = strings.TrimSpace(path)
		if path == "" || path == "*" || path != "name" && path != "description" || seen[path] {
			return nil, biz.ErrImageInvalid
		}
		seen[path] = true
		out = append(out, path)
	}
	return out, nil
}

func (s *ImageService) DeleteImage(ctx context.Context, r *pb.DeleteImageRequest) (*pb.VMImage, error) {
	if r == nil || !biz.UUID(r.ImageId) || r.IdempotencyKey == "" || len(r.IdempotencyKey) > 256 || r.ExpectedVersion < 1 {
		return nil, s.rejected(ctx, "delete_key", biz.ErrImageInvalid)
	}
	if e := biz.RequireTenant(ctx, r.TenantId); e != nil {
		return nil, s.rejected(ctx, "delete_tenant_scope", e)
	}
	// A delete replays on the stored row even after the upload switch is off, so
	// the gate deliberately does not apply here.
	command, e := imageCommandOf(biz.ImageDeleteAction, r.TenantId, r.ImageId, r.IdempotencyKey, struct{}{}, r.ExpectedVersion, nil)
	if e != nil {
		return nil, s.rejected(ctx, "delete_command", e)
	}
	i, e := s.repo.DeleteImage(ctx, command)
	if e != nil {
		return nil, s.rejected(ctx, "delete_repository", e)
	}
	return wireImage(i), nil
}

var _ pb.VMImageServiceServer = (*ImageService)(nil)
