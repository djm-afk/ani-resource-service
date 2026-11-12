package biz

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

type InitPlatform struct{ IdempotencyKey string }
type IssuePlatformCredential struct {
	IdempotencyKey  string
	ExpectedVersion int64
	Rotate          bool
}

// Platform is composed only by the local operator command. Its private engine
// shares bounded project/robot recovery mechanics with the tenant lifecycle.
// It is never registered on a listener or selected by a tenant scope field.
type Platform struct {
	repo   PlatformRepository
	engine *Lifecycle
}

func NewPlatform(repo PlatformRepository, lifecycle *Lifecycle) (*Platform, error) {
	if repo == nil || lifecycle == nil {
		return nil, Fail(InvalidArgument, "platform dependencies required")
	}
	engine := *lifecycle
	engine.platformRepo = repo
	return &Platform{repo: repo, engine: &engine}, nil
}
func platformCommand(caller Caller, space, kind, key string, input any) (Command, error) {
	if caller.Kind != PlatformCaller || caller.TenantID != "" {
		return Command{}, Fail(PermissionDenied, "platform operator required")
	}
	c, err := requestCommand(caller, space, kind, key, input)
	c.Scope = PlatformImages
	return c, err
}
func (p *Platform) checkSpace(s Space) error {
	if s.Scope != PlatformImages || s.TenantID != "" || s.InstallationID != p.engine.cfg.InstallationID || s.RegistryAuthority != p.engine.cfg.RegistryAuthority || s.ProjectName != p.engine.cfg.PlatformProject {
		return Fail(SpaceNotReady, "platform space configuration mismatch")
	}
	return nil
}
func (p *Platform) InitializePlatform(ctx context.Context, in InitPlatform) (Space, error) {
	caller, err := RequirePlatform(ctx)
	if err != nil {
		return Space{}, err
	}
	c, err := platformCommand(caller, "", "enable_space", in.IdempotencyKey, in)
	if err != nil {
		return Space{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	cfg := p.engine.cfg
	s := Space{ID: uuid.NewString(), Scope: PlatformImages, InstallationID: cfg.InstallationID, RegistryAuthority: cfg.RegistryAuthority, ProjectName: cfg.PlatformProject}
	s, c, err = p.repo.ReservePlatformSpace(ctx, s, c)
	if err != nil {
		return Space{}, err
	}
	err = p.engine.repo.WithSpaceWriteLock(ctx, s.ID, func(ctx context.Context) error {
		var e error
		s, e = p.repo.FindPlatformSpace(ctx)
		if e != nil {
			return e
		}
		if e = p.checkSpace(s); e != nil {
			return e
		}
		c, e = p.repo.FindPlatformCommand(ctx, s.ID, in.IdempotencyKey)
		if e != nil {
			return e
		}
		if c.State == "succeeded" {
			if c.Result.Space == nil || c.Result.Space.Scope != PlatformImages || c.Result.Space.TenantID != "" || c.Result.Space.ID != s.ID {
				return Fail(InternalError, "invalid platform initialization result")
			}
			s = *c.Result.Space
			return nil
		}
		if e = p.engine.resumeProject(ctx, &s, &c); e != nil {
			return e
		}
		s, c, e = p.repo.CompletePlatformEnable(ctx, s, c)
		return e
	})
	return s, err
}
func (p *Platform) IssuePlatformPublisher(ctx context.Context, in IssuePlatformCredential) (CredentialDelivery, error) {
	caller, err := RequirePlatform(ctx)
	if err != nil {
		return CredentialDelivery{}, err
	}
	if in.ExpectedVersion < 0 {
		return CredentialDelivery{}, Fail(InvalidArgument, "invalid expected version")
	}
	s, err := p.repo.FindPlatformSpace(ctx)
	if err != nil {
		return CredentialDelivery{}, err
	}
	if err = p.checkSpace(s); err != nil {
		return CredentialDelivery{}, err
	}
	kind := "issue_publisher"
	if in.Rotate {
		kind = "reset_publisher"
	}
	c, err := platformCommand(caller, s.ID, kind, in.IdempotencyKey, in)
	if err != nil {
		return CredentialDelivery{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	var delivery CredentialDelivery
	err = p.engine.repo.WithSpaceWriteLock(ctx, s.ID, func(ctx context.Context) error {
		var e error
		c, e = p.repo.BeginPlatformCredentialCommand(ctx, c, in.ExpectedVersion)
		if e != nil {
			return e
		}
		if c.State == "succeeded" {
			delivery, e = p.engine.replayDelivery(ctx, s, c)
			return e
		}
		if e = recoveredCandidate(c); e != nil {
			return e
		}
		if _, e = p.engine.platform(ctx); e != nil {
			return e
		}
		info, e := p.repo.GetPlatformPublisher(ctx, s.ID)
		if e != nil {
			return e
		}
		s, c, e = p.engine.runCandidate(ctx, s, c, info)
		if e != nil {
			return e
		}
		delivery, e = p.engine.replayDelivery(ctx, s, c)
		return e
	})
	return delivery, err
}
func (p *Platform) RegisterPlatformImage(ctx context.Context, in RegisterImage) (Registration, error) {
	caller, err := RequirePlatform(ctx)
	if err != nil {
		return Registration{}, err
	}
	if in.TenantID != "" {
		return Registration{}, Fail(InvalidArgument, "platform request cannot carry tenant")
	}
	in.Metadata, err = NormalizeMetadata(in.Metadata)
	if err != nil {
		return Registration{}, err
	}
	if _, err = ParseIdempotencyKey(in.IdempotencyKey); err != nil {
		return Registration{}, err
	}
	s, err := p.repo.FindPlatformSpace(ctx)
	if err != nil {
		return Registration{}, err
	}
	if err = p.checkSpace(s); err != nil {
		return Registration{}, err
	}
	if s.State != "available" || s.ProjectID <= 0 {
		return Registration{}, Fail(SpaceNotReady, "platform space not ready")
	}
	ref, err := ParseImageReference(s.RegistryAuthority, s.ProjectName, in.ImageReference)
	if err != nil {
		return Registration{}, err
	}
	in.ImageReference = ref.String()
	c, err := platformCommand(caller, s.ID, "register_image", in.IdempotencyKey, in)
	if err != nil {
		return Registration{}, err
	}
	existing, err := p.repo.FindPlatformCommand(ctx, s.ID, in.IdempotencyKey)
	if err == nil {
		if existing.Kind != c.Kind || existing.Actor != c.Actor || existing.Fingerprint != c.Fingerprint {
			return Registration{}, Fail(IdempotencyConflict, "idempotency key belongs to another request")
		}
		if existing.State != "succeeded" {
			return Registration{}, Fail(RequestInProgress, "catalog request not completed")
		}
		if existing.Result.Registration == nil {
			return Registration{}, Fail(InternalError, "missing platform registration result")
		}
		r := *existing.Result.Registration
		if r.SpaceID != s.ID {
			return Registration{}, Fail(InternalError, "stored platform space mismatch")
		}
		if err = validateRegistrationOwner(r, "", PlatformImages); err != nil {
			return Registration{}, err
		}
		return r, nil
	}
	if ReasonOf(err) != ImageNotFound {
		return Registration{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	artifact, err := p.engine.registry.ResolveArtifact(ctx, s.ProjectName, ref.Repository, ref.Reference)
	if err != nil {
		return Registration{}, err
	}
	if err = validateArtifact(artifact); err != nil {
		return Registration{}, err
	}
	if ref.IsDigest && ref.Reference != artifact.Digest {
		return Registration{}, Fail(DependencyUnavailable, "registry digest mismatch")
	}
	r := Registration{ID: "img_" + strings.ReplaceAll(uuid.NewString(), "-", ""), Scope: PlatformImages, SpaceID: s.ID, Repository: ref.Repository, SourceReference: ref.String(), Digest: artifact.Digest, MediaType: artifact.MediaType, Platforms: artifact.Platforms, Metadata: in.Metadata}
	return p.repo.ApplyPlatformRegistration(ctx, c, r)
}
func (p *Platform) UpdatePlatformImage(ctx context.Context, in UpdateImage) (Registration, error) {
	caller, err := RequirePlatform(ctx)
	if err != nil {
		return Registration{}, err
	}
	if in.TenantID != "" || in.ExpectedVersion < 1 {
		return Registration{}, Fail(InvalidArgument, "invalid platform metadata request")
	}
	if _, err = ParseImageID(in.ImageID); err != nil {
		return Registration{}, err
	}
	in.Metadata, err = NormalizeMetadata(in.Metadata)
	if err != nil {
		return Registration{}, err
	}
	s, err := p.repo.FindPlatformSpace(ctx)
	if err != nil {
		return Registration{}, err
	}
	if err = p.checkSpace(s); err != nil {
		return Registration{}, err
	}
	c, err := platformCommand(caller, s.ID, "update_image", in.IdempotencyKey, in)
	if err != nil {
		return Registration{}, err
	}
	return p.repo.ApplyPlatformMetadata(ctx, c, in)
}
func (p *Platform) UnregisterPlatformImage(ctx context.Context, in UnregisterImage) (Registration, error) {
	caller, err := RequirePlatform(ctx)
	if err != nil {
		return Registration{}, err
	}
	if in.TenantID != "" || in.ExpectedVersion < 1 {
		return Registration{}, Fail(InvalidArgument, "invalid platform unregister request")
	}
	if _, err = ParseImageID(in.ImageID); err != nil {
		return Registration{}, err
	}
	s, err := p.repo.FindPlatformSpace(ctx)
	if err != nil {
		return Registration{}, err
	}
	if err = p.checkSpace(s); err != nil {
		return Registration{}, err
	}
	c, err := platformCommand(caller, s.ID, "unregister_image", in.IdempotencyKey, in)
	if err != nil {
		return Registration{}, err
	}
	return p.repo.ApplyPlatformUnregister(ctx, c, in)
}
func (p *Platform) InspectSpace(ctx context.Context, id string) (Space, error) {
	return p.engine.InspectSpace(ctx, id)
}

// GetPlatformSpace is a pure read: it never initializes the space, never probes
// the registry and never writes. A missing record is reported as uninitialized
// with the configured binding, and a stored mismatch is reported explicitly
// while still carrying the real stored space.
func (p *Platform) GetPlatformSpace(ctx context.Context) (PlatformSpaceView, error) {
	if _, err := RequirePlatform(ctx); err != nil {
		return PlatformSpaceView{}, err
	}
	cfg := p.engine.cfg
	view := PlatformSpaceView{ConfiguredProject: cfg.PlatformProject, ConfiguredRegistryAuthority: cfg.RegistryAuthority}
	s, err := p.repo.FindPlatformSpace(ctx)
	if err != nil {
		if ReasonOf(err) == SpaceNotFound {
			view.BindingStatus = "uninitialized"
			return view, nil
		}
		return PlatformSpaceView{}, err
	}
	view.Space = &s
	if p.checkSpace(s) != nil {
		view.BindingStatus = "configuration_mismatch"
		return view, nil
	}
	switch s.State {
	case "available":
		view.BindingStatus = "ready"
	case "provisioning":
		view.BindingStatus = "provisioning"
	case "blocked":
		view.BindingStatus = "blocked"
	default:
		return PlatformSpaceView{}, Fail(InternalError, "invalid platform space state")
	}
	return view, nil
}

// GetPlatformPublisherCredential reuses the stored-space check and the
// not_issued semantics. It never reads a historical secret.
func (p *Platform) GetPlatformPublisherCredential(ctx context.Context) (CredentialInfo, error) {
	if _, err := RequirePlatform(ctx); err != nil {
		return CredentialInfo{}, err
	}
	s, err := p.repo.FindPlatformSpace(ctx)
	if err != nil {
		return CredentialInfo{}, err
	}
	if err = p.checkSpace(s); err != nil {
		return CredentialInfo{}, err
	}
	return p.repo.GetPlatformPublisher(ctx, s.ID)
}

// DisablePlatformPublisher mirrors the tenant disable entry point but reads and
// writes only platform rows. It shares the disable process with the tenant lane.
func (p *Platform) DisablePlatformPublisher(ctx context.Context, in DisableCredential) (CredentialInfo, error) {
	caller, err := RequirePlatform(ctx)
	if err != nil {
		return CredentialInfo{}, err
	}
	if in.TenantID != "" || in.ExpectedVersion < 0 {
		return CredentialInfo{}, Fail(InvalidArgument, "invalid platform disable request")
	}
	s, err := p.repo.FindPlatformSpace(ctx)
	if err != nil {
		return CredentialInfo{}, err
	}
	if err = p.checkSpace(s); err != nil {
		return CredentialInfo{}, err
	}
	c, err := platformCommand(caller, s.ID, "disable_publisher", in.IdempotencyKey, in)
	if err != nil {
		return CredentialInfo{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	var result CredentialInfo
	err = p.engine.repo.WithSpaceWriteLock(ctx, s.ID, func(ctx context.Context) error {
		var e error
		c, e = p.repo.BeginPlatformCredentialCommand(ctx, c, in.ExpectedVersion)
		if e != nil {
			return e
		}
		if c.State == "succeeded" {
			if c.Result.Credential == nil {
				return Fail(InternalError, "missing disable result")
			}
			result = *c.Result.Credential
			return nil
		}
		result, e = p.engine.runPublisherDisable(ctx, s, c)
		return e
	})
	return result, err
}

// ListPlatformImagesPage is the offset-page platform catalog. An uninitialized
// space yields an empty page; every other failure is propagated.
func (p *Platform) ListPlatformImagesPage(ctx context.Context, f ManagementFilter, page PageRequest) (RegistrationPage, error) {
	if _, err := RequirePlatform(ctx); err != nil {
		return RegistrationPage{}, err
	}
	f, err := NormalizeManagementFilter(f)
	if err != nil {
		return RegistrationPage{}, err
	}
	page, err = NormalizePageRequest(page)
	if err != nil {
		return RegistrationPage{}, err
	}
	if _, err = p.repo.FindPlatformSpace(ctx); err != nil {
		if ReasonOf(err) == SpaceNotFound {
			return RegistrationPage{Items: []Registration{}}, nil
		}
		return RegistrationPage{}, err
	}
	rows, total, err := p.repo.OffsetPlatformRegistrations(ctx, f, page.Offset(), int32(page.PageSize))
	if err != nil {
		return RegistrationPage{}, err
	}
	for _, r := range rows {
		if err = validateRegistrationOwner(r, "", PlatformImages); err != nil {
			return RegistrationPage{}, err
		}
	}
	if rows == nil {
		rows = []Registration{}
	}
	return RegistrationPage{Items: rows, Total: total}, nil
}

// GetPlatformImage reads one platform registration without borrowing a fake
// tenant UUID.
func (p *Platform) GetPlatformImage(ctx context.Context, imageID string) (Registration, error) {
	if _, err := RequirePlatform(ctx); err != nil {
		return Registration{}, err
	}
	if _, err := ParseImageID(imageID); err != nil {
		return Registration{}, err
	}
	if _, err := p.repo.FindPlatformSpace(ctx); err != nil {
		if ReasonOf(err) == SpaceNotFound {
			return Registration{}, Fail(ImageNotFound, "image not found")
		}
		return Registration{}, err
	}
	r, err := p.repo.FindPlatformRegistration(ctx, imageID)
	if err != nil {
		return Registration{}, err
	}
	if r.ID != imageID {
		return Registration{}, Fail(InternalError, "stored image ID mismatch")
	}
	if err = validateRegistrationOwner(r, "", PlatformImages); err != nil {
		return Registration{}, err
	}
	return r, nil
}

// PatchPlatformImage applies a field-mask patch to platform metadata.
func (p *Platform) PatchPlatformImage(ctx context.Context, in PatchImage) (Registration, error) {
	caller, err := RequirePlatform(ctx)
	if err != nil {
		return Registration{}, err
	}
	if in.TenantID != "" {
		return Registration{}, Fail(InvalidArgument, "platform request cannot carry tenant")
	}
	in, err = NormalizePatch(in)
	if err != nil {
		return Registration{}, err
	}
	s, err := p.repo.FindPlatformSpace(ctx)
	if err != nil {
		return Registration{}, err
	}
	if err = p.checkSpace(s); err != nil {
		return Registration{}, err
	}
	c, err := platformCommand(caller, s.ID, "update_image", in.IdempotencyKey, patchFingerprint("platform", in))
	if err != nil {
		return Registration{}, err
	}
	return p.repo.ApplyPlatformRegistrationPatch(ctx, c, in)
}
func (p *Platform) PurgeExpiredDeliverySecrets(ctx context.Context) (int64, error) {
	return p.engine.PurgeExpiredDeliverySecrets(ctx)
}
func (p *Platform) RecoverProjectBinding(ctx context.Context, id string, projectID int64, evidence string) (Space, error) {
	if _, err := RequirePlatform(ctx); err != nil {
		return Space{}, err
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(evidence) || projectID <= 0 {
		return Space{}, Fail(InvalidArgument, "explicit recovery ID and evidence hash required")
	}
	s, err := p.engine.repo.InspectSpaceForOperator(ctx, id)
	if err != nil {
		return Space{}, err
	}
	if s.Scope == TenantImages {
		return p.engine.RecoverProjectBinding(ctx, id, projectID, evidence)
	}
	if err = p.checkSpace(s); err != nil {
		return Space{}, err
	}
	err = p.engine.repo.WithSpaceWriteLock(ctx, s.ID, func(ctx context.Context) error {
		var e error
		s, e = p.engine.repo.InspectSpaceForOperator(ctx, id)
		if e != nil {
			return e
		}
		if e = p.checkSpace(s); e != nil {
			return e
		}
		c, e := p.repo.OpenPlatformExternalCommand(ctx, s.ID)
		if e != nil {
			return e
		}
		project, e := p.engine.registry.GetProjectByID(ctx, projectID)
		if e != nil {
			return e
		}
		if project.ID != projectID || project.Name != s.ProjectName || !project.Private {
			return Fail(PermissionDenied, "recovery project mismatch")
		}
		s, e = p.repo.RecoverPlatformProject(ctx, s, c, projectID, evidence)
		return e
	})
	return s, err
}
