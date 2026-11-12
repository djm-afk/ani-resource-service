package biz

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
)

type Catalog struct {
	repo     CatalogRepository
	spaces   SpaceRepository
	commands CommandRepository
	registry Registry
	cursor   *CursorCodec
	binding  ImageBindingConfig
}

// NewCatalog requires the deployment binding. There is deliberately no zero
// value fallback: a missing binding must fail composition, not skip the check.
func NewCatalog(repo CatalogRepository, spaces SpaceRepository, commands CommandRepository, registry Registry, cursor *CursorCodec, binding ImageBindingConfig) (*Catalog, error) {
	if repo == nil || spaces == nil || commands == nil || registry == nil || cursor == nil {
		return nil, Fail(InvalidArgument, "catalog dependencies required")
	}
	if _, err := ParseTenant(binding.InstallationID); err != nil || binding.RegistryAuthority == "" || binding.PlatformProject == "" {
		return nil, Fail(InvalidArgument, "catalog binding required")
	}
	return &Catalog{repo: repo, spaces: spaces, commands: commands, registry: registry, cursor: cursor, binding: binding}, nil
}
func (c *Catalog) RegisterImage(ctx context.Context, in RegisterImage) (Registration, error) {
	caller, err := RequireTenant(ctx, in.TenantID)
	if err != nil {
		return Registration{}, err
	}
	in.Metadata, err = NormalizeMetadata(in.Metadata)
	if err != nil {
		return Registration{}, err
	}
	if _, err = ParseIdempotencyKey(in.IdempotencyKey); err != nil {
		return Registration{}, err
	}
	s, err := c.spaces.FindTenantSpace(ctx, in.TenantID)
	if err != nil {
		return Registration{}, err
	}
	// The binding is checked before any registry call and before the command
	// lookup, so a mismatched deployment cannot create a registration or replay
	// one silently.
	if err = ValidateTenantCatalogBinding(s, c.binding, in.TenantID); err != nil {
		return Registration{}, err
	}
	if s.State != "available" || s.ProjectID <= 0 {
		return Registration{}, Fail(SpaceNotReady, "image space not ready")
	}
	ref, err := ParseImageReference(s.RegistryAuthority, s.ProjectName, in.ImageReference)
	if err != nil {
		return Registration{}, err
	}
	in.ImageReference = ref.String()
	command, err := requestCommand(caller, s.ID, "register_image", in.IdempotencyKey, in)
	if err != nil {
		return Registration{}, err
	}
	// Read a completed command before resolving a mutable tag or contacting the
	// registry. The data transaction repeats this guard for concurrent requests.
	existing, err := c.commands.FindTenantCommand(ctx, in.TenantID, s.ID, in.IdempotencyKey)
	if err == nil {
		if existing.Kind != command.Kind || existing.Actor != command.Actor || existing.Fingerprint != command.Fingerprint {
			return Registration{}, Fail(IdempotencyConflict, "idempotency key belongs to another request")
		}
		if existing.State != "succeeded" {
			return Registration{}, Fail(RequestInProgress, "catalog request not completed")
		}
		if existing.Result.Registration == nil {
			return Registration{}, Fail(InternalError, "missing registration result")
		}
		r := *existing.Result.Registration
		if err = validateRegistrationOwner(r, in.TenantID, TenantImages); err != nil {
			return Registration{}, err
		}
		return r, nil
	}
	if ReasonOf(err) != ImageNotFound {
		return Registration{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	artifact, err := c.registry.ResolveArtifact(ctx, s.ProjectName, ref.Repository, ref.Reference)
	if err != nil {
		return Registration{}, err
	}
	if err = validateArtifact(artifact); err != nil {
		return Registration{}, err
	}
	if ref.IsDigest && ref.Reference != artifact.Digest {
		return Registration{}, Fail(DependencyUnavailable, "registry digest mismatch")
	}
	r := Registration{ID: "img_" + strings.ReplaceAll(uuid.NewString(), "-", ""), TenantID: in.TenantID, Scope: TenantImages, SpaceID: s.ID, Repository: ref.Repository, SourceReference: ref.String(), Digest: artifact.Digest, MediaType: artifact.MediaType, Platforms: artifact.Platforms, Metadata: in.Metadata}
	return c.repo.ApplyTenantRegistration(ctx, command, r)
}
func validateRegistrationOwner(r Registration, tenant string, scope ImageScope) error {
	if r.Scope != scope || (scope == TenantImages && r.TenantID != tenant) || (scope == PlatformImages && r.TenantID != "") {
		return Fail(InternalError, "stored image ownership mismatch")
	}
	return nil
}
func readRegistration(ctx context.Context, repo CatalogRepository, in ReadImage) (Registration, error) {
	if _, err := ParseTenant(in.TenantID); err != nil {
		return Registration{}, err
	}
	if err := ParseScope(in.Scope); err != nil {
		return Registration{}, err
	}
	if _, err := ParseImageID(in.ImageID); err != nil {
		return Registration{}, err
	}
	var r Registration
	var err error
	if in.Scope == TenantImages {
		r, err = repo.FindTenantRegistration(ctx, in.TenantID, in.ImageID)
	} else {
		r, err = repo.FindPlatformRegistration(ctx, in.ImageID)
	}
	if err != nil {
		return Registration{}, err
	}
	if r.ID != in.ImageID {
		return Registration{}, Fail(InternalError, "stored image ID mismatch")
	}
	if err = validateRegistrationOwner(r, in.TenantID, in.Scope); err != nil {
		return Registration{}, err
	}
	return r, nil
}
func (c *Catalog) GetImage(ctx context.Context, in ReadImage) (Registration, error) {
	if _, err := RequireTenant(ctx, in.TenantID); err != nil {
		return Registration{}, err
	}
	return readRegistration(ctx, c.repo, in)
}
func (c *Catalog) ListImages(ctx context.Context, in ListImages) (RegistrationPage, error) {
	if _, err := RequireTenant(ctx, in.TenantID); err != nil {
		return RegistrationPage{}, err
	}
	if err := ParseScope(in.Scope); err != nil {
		return RegistrationPage{}, err
	}
	var err error
	in.Filter, err = NormalizeFilter(in.Filter)
	if err != nil {
		return RegistrationPage{}, err
	}
	after, err := c.cursor.DecodeCursor(in.Cursor, in.TenantID, in.Scope, in.Filter)
	if err != nil {
		return RegistrationPage{}, err
	}
	var rows []Registration
	var total int64
	if in.Scope == TenantImages {
		rows, total, err = c.repo.PageTenantRegistrations(ctx, in.TenantID, in.Filter, after)
	} else {
		rows, total, err = c.repo.PagePlatformRegistrations(ctx, in.Filter, after)
	}
	if err != nil {
		return RegistrationPage{}, err
	}
	for _, r := range rows {
		if err = validateRegistrationOwner(r, in.TenantID, in.Scope); err != nil {
			return RegistrationPage{}, err
		}
	}
	page := RegistrationPage{Items: rows, Total: total}
	if len(rows) > in.Filter.Limit {
		page.Items = rows[:in.Filter.Limit]
		last := page.Items[len(page.Items)-1]
		page.NextCursor, err = c.cursor.EncodeCursor(in.TenantID, in.Scope, in.Filter, PageKey{CreatedAt: last.CreatedAt, ImageID: last.ID})
		if err != nil {
			return RegistrationPage{}, err
		}
	}
	if page.Items == nil {
		page.Items = []Registration{}
	}
	return page, nil
}
func (c *Catalog) UpdateImage(ctx context.Context, in UpdateImage) (Registration, error) {
	caller, err := RequireTenant(ctx, in.TenantID)
	if err != nil {
		return Registration{}, err
	}
	if _, err = ParseImageID(in.ImageID); err != nil {
		return Registration{}, err
	}
	if in.ExpectedVersion < 1 {
		return Registration{}, Fail(InvalidArgument, "positive expected version required")
	}
	in.Metadata, err = NormalizeMetadata(in.Metadata)
	if err != nil {
		return Registration{}, err
	}
	if _, err = ParseIdempotencyKey(in.IdempotencyKey); err != nil {
		return Registration{}, err
	}
	// The space ID comes from the tenant-scoped registration, not from
	// FindTenantSpace: a mismatched or unreadable space binding must not block a
	// purely local metadata change.
	r, err := c.repo.FindTenantRegistrationOwned(ctx, in.TenantID, in.ImageID)
	if err != nil {
		return Registration{}, err
	}
	if err = ValidateTenantOwnership(r.Scope, r.TenantID, in.TenantID); err != nil {
		return Registration{}, err
	}
	command, err := requestCommand(caller, r.SpaceID, "update_image", in.IdempotencyKey, in)
	if err != nil {
		return Registration{}, err
	}
	return c.repo.ApplyTenantMetadata(ctx, command, in)
}
func (c *Catalog) UnregisterImage(ctx context.Context, in UnregisterImage) (Registration, error) {
	caller, err := RequireTenant(ctx, in.TenantID)
	if err != nil {
		return Registration{}, err
	}
	if _, err = ParseImageID(in.ImageID); err != nil {
		return Registration{}, err
	}
	if in.ExpectedVersion < 1 {
		return Registration{}, Fail(InvalidArgument, "positive expected version required")
	}
	if _, err = ParseIdempotencyKey(in.IdempotencyKey); err != nil {
		return Registration{}, err
	}
	r, err := c.repo.FindTenantRegistrationOwned(ctx, in.TenantID, in.ImageID)
	if err != nil {
		return Registration{}, err
	}
	if err = ValidateTenantOwnership(r.Scope, r.TenantID, in.TenantID); err != nil {
		return Registration{}, err
	}
	command, err := requestCommand(caller, r.SpaceID, "unregister_image", in.IdempotencyKey, in)
	if err != nil {
		return Registration{}, err
	}
	return c.repo.ApplyTenantUnregister(ctx, command, in)
}

// ListTenantImagesPage is the offset-page tenant list. It reports an empty page
// for a tenant that has no image space at all, and never downgrades a database
// failure into an empty page.
func (c *Catalog) ListTenantImagesPage(ctx context.Context, tenant string, f ManagementFilter, p PageRequest) (RegistrationPage, error) {
	if _, err := RequireTenant(ctx, tenant); err != nil {
		return RegistrationPage{}, err
	}
	f, err := NormalizeManagementFilter(f)
	if err != nil {
		return RegistrationPage{}, err
	}
	p, err = NormalizePageRequest(p)
	if err != nil {
		return RegistrationPage{}, err
	}
	rows, total, err := c.repo.OffsetTenantRegistrations(ctx, tenant, f, p.Offset(), int32(p.PageSize))
	if err != nil {
		if ReasonOf(err) == SpaceNotFound {
			return RegistrationPage{Items: []Registration{}}, nil
		}
		return RegistrationPage{}, err
	}
	for _, r := range rows {
		if err = validateRegistrationOwner(r, tenant, TenantImages); err != nil {
			return RegistrationPage{}, err
		}
	}
	if rows == nil {
		rows = []Registration{}
	}
	return RegistrationPage{Items: rows, Total: total}, nil
}

// PatchTenantImage applies a field-mask patch to tenant-scoped metadata. It
// deliberately does not read the space: ownership comes from the registration.
func (c *Catalog) PatchTenantImage(ctx context.Context, in PatchImage) (Registration, error) {
	caller, err := RequireTenant(ctx, in.TenantID)
	if err != nil {
		return Registration{}, err
	}
	in, err = NormalizePatch(in)
	if err != nil {
		return Registration{}, err
	}
	r, err := c.repo.FindTenantRegistrationOwned(ctx, in.TenantID, in.ImageID)
	if err != nil {
		return Registration{}, err
	}
	if err = ValidateTenantOwnership(r.Scope, r.TenantID, in.TenantID); err != nil {
		return Registration{}, err
	}
	command, err := requestCommand(caller, r.SpaceID, "update_image", in.IdempotencyKey, patchFingerprint("tenant", in))
	if err != nil {
		return Registration{}, err
	}
	return c.repo.ApplyTenantRegistrationPatch(ctx, command, in)
}

// patchFingerprint is the idempotency input of a management patch. It binds the
// requested presence and values only, never the merged current record, and the
// discriminator keeps it distinct from the legacy full-metadata update.
type patchFingerprintBody struct {
	Discriminator   string
	Scope, Target   string
	ImageID         string
	ExpectedVersion int64
	Mask            []string
	DisplayName     *string
	Description     *string
	Purposes        *[]string
	Accelerator     *string
}

func patchFingerprint(scope string, in PatchImage) patchFingerprintBody {
	return patchFingerprintBody{Discriminator: "image-management-patch/v1", Scope: scope, Target: in.TenantID, ImageID: in.ImageID, ExpectedVersion: in.ExpectedVersion, Mask: in.Mask, DisplayName: in.Patch.DisplayName, Description: in.Patch.Description, Purposes: in.Patch.Purposes, Accelerator: in.Patch.Accelerator}
}
