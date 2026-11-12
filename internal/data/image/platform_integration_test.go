//go:build imageintegration

package data_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/image"
	imagedata "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/image"
)

func platformFixture(t *testing.T) (*fixture, *catalogRegistry, biz.LifecycleConfig, *biz.Lifecycle, *biz.Platform, context.Context) {
	t.Helper()
	f := database(t)
	r := &catalogRegistry{registryFixture: newRegistry(), artifacts: map[string]biz.Artifact{}, tags: map[string]string{}}
	// This is the private protocol fixture, not an existing Harbor project.
	delete(r.projects, 1)
	cfg := biz.LifecycleConfig{InstallationID: uuid.NewString(), RegistryAuthority: "registry.invalid", PlatformProject: "platform", PublisherDays: 30, PullDays: 365}
	ring, err := imagedata.NewAESGCMKeyring("test", map[string][]byte{"test": []byte(strings.Repeat("k", 32))})
	if err != nil {
		t.Fatal(err)
	}
	l := lifecycle(t, f.Repo, r, ring, cfg, nil)
	p, err := biz.NewPlatform(f.Repo, l)
	if err != nil {
		t.Fatal(err)
	}
	ctx := biz.WithCaller(context.Background(), biz.Caller{Kind: biz.PlatformCaller, Subject: "local-operator", Actor: "operator:image-test"})
	return f, r, cfg, l, p, ctx
}
func TestPlatformLifecycleCatalogAndTenantBoundaries(t *testing.T) {
	f, r, cfg, l, p, ctx := platformFixture(t)
	init := biz.InitPlatform{IdempotencyKey: "platform-init"}
	s, err := p.InitializePlatform(ctx, init)
	if err != nil || s.State != "available" || s.TenantID != "" || s.Scope != biz.PlatformImages {
		t.Fatal("platform initialize", err)
	}
	replay, err := p.InitializePlatform(ctx, init)
	if err != nil || replay.ID != s.ID || r.creates != 1 || r.robotCreates != 0 {
		t.Fatal("initialization replay or unexpected robot", err)
	}
	issuer := biz.IssuePlatformCredential{IdempotencyKey: "platform-issue", ExpectedVersion: 0}
	delivery, err := p.IssuePlatformPublisher(ctx, issuer)
	if err != nil || len(delivery.Secret) == 0 {
		t.Fatal("platform publisher", err)
	}
	defer clear(delivery.Secret)
	if delivery.Credential.Scope != biz.PlatformImages || delivery.Credential.TenantID != "" || delivery.Credential.Generation != 1 {
		t.Fatal("credential ownership")
	}
	robot := r.robots[delivery.Credential.RobotID]
	perms, err := biz.RobotPermissions(cfg.PlatformProject, cfg.PlatformProject, "publisher", biz.PlatformImages)
	if err != nil {
		t.Fatal(err)
	}
	if biz.ValidateRobotPermissions(robot.Permissions, perms) != nil {
		t.Fatal("platform publisher permissions too broad")
	}
	again, err := p.IssuePlatformPublisher(ctx, issuer)
	if err != nil || string(again.Secret) != string(delivery.Secret) || r.robotCreates != 1 {
		t.Fatal("delivery replay changed generation", err)
	}
	clear(again.Secret)
	otherActor := biz.WithCaller(context.Background(), biz.Caller{Kind: biz.PlatformCaller, Subject: "local-operator", Actor: "operator:other"})
	if _, err = p.IssuePlatformPublisher(otherActor, issuer); biz.ReasonOf(err) != biz.IdempotencyConflict {
		t.Fatal("cross actor replay", err)
	}
	issuer.IdempotencyKey = "platform-rotate"
	issuer.ExpectedVersion = delivery.Credential.Version
	issuer.Rotate = true
	rotated, err := p.IssuePlatformPublisher(ctx, issuer)
	if err != nil || rotated.Credential.Generation != 2 || !r.robots[robot.ID].Disabled {
		t.Fatal("platform rotate", err)
	}
	defer clear(rotated.Secret)
	if _, err = p.IssuePlatformPublisher(ctx, biz.IssuePlatformCredential{IdempotencyKey: "platform-issue"}); biz.ReasonOf(err) != biz.CredentialDeliveryExpired {
		t.Fatal("old generation delivered", err)
	}
	var plaintext bool
	if err = f.Runtime.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM image.commands WHERE candidate::text LIKE '%' || $1 || '%' OR result::text LIKE '%' || $1 || '%')", string(rotated.Secret)).Scan(&plaintext); err != nil || plaintext {
		t.Fatal("plaintext command snapshot", err)
	}
	var longterm int
	if err = f.Runtime.QueryRow(ctx, "SELECT count(*) FROM image.credentials WHERE owner_scope='platform' AND secret_ciphertext IS NOT NULL").Scan(&longterm); err != nil || longterm != 0 {
		t.Fatal("platform longterm publisher secret", err)
	}
	tenant := uuid.NewString()
	tenantCtx := biz.WithCaller(context.Background(), biz.Caller{Kind: biz.GovernanceCaller, Subject: "governance", Actor: "user:test", TenantID: tenant})
	if _, err = p.InitializePlatform(tenantCtx, init); biz.ReasonOf(err) != biz.PermissionDenied {
		t.Fatal("tenant platform initialization")
	}
	if _, err = p.IssuePlatformPublisher(tenantCtx, issuer); biz.ReasonOf(err) != biz.PermissionDenied {
		t.Fatal("tenant platform publisher")
	}
	if _, err = p.InspectSpace(tenantCtx, s.ID); biz.ReasonOf(err) != biz.PermissionDenied {
		t.Fatal("tenant platform inspect")
	}
	if _, err = p.PurgeExpiredDeliverySecrets(tenantCtx); biz.ReasonOf(err) != biz.PermissionDenied {
		t.Fatal("tenant platform purge")
	}
	if _, err = l.EnsureImageSpace(ctx, biz.EnableSpace{TenantID: tenant, Slug: "wrong", IdempotencyKey: "operator-tenant"}); biz.ReasonOf(err) != biz.PermissionDenied {
		t.Fatal("operator masqueraded as tenant")
	}
	tenantSpace, err := l.EnsureImageSpace(tenantCtx, biz.EnableSpace{TenantID: tenant, Slug: "platform-test", IdempotencyKey: "tenant-enable"})
	if err != nil {
		t.Fatal("tenant cannot use initialized platform", err)
	}
	artifact := biz.Artifact{Digest: "sha256:" + strings.Repeat("a", 64), MediaType: "application/vnd.oci.image.manifest.v1+json", Platforms: []biz.ImagePlatform{{OS: "linux", Architecture: "amd64"}}}
	r.artifacts["platform/app@"+artifact.Digest] = artifact
	r.tags["platform/app:v1"] = artifact.Digest
	input := biz.RegisterImage{ImageReference: "registry.invalid/platform/app:v1", IdempotencyKey: "platform-register", Metadata: biz.Metadata{DisplayName: "Shared app", Purposes: []string{"container", "training"}, Accelerator: "none"}}
	registered, err := p.RegisterPlatformImage(ctx, input)
	if err != nil || registered.Scope != biz.PlatformImages || registered.Digest != artifact.Digest {
		t.Fatal("register platform", err)
	}
	reads := r.reads
	r.tags["platform/app:v1"] = "sha256:" + strings.Repeat("b", 64)
	same, err := p.RegisterPlatformImage(ctx, input)
	if err != nil || same.ID != registered.ID || r.reads != reads {
		t.Fatal("platform replay followed mutable tag", err)
	}
	if _, err = p.RegisterPlatformImage(tenantCtx, input); biz.ReasonOf(err) != biz.PermissionDenied {
		t.Fatal("tenant registered platform")
	}
	invalid := input
	invalid.TenantID = tenant
	if _, err = p.RegisterPlatformImage(ctx, invalid); biz.ReasonOf(err) != biz.InvalidArgument {
		t.Fatal("operator tenant field accepted")
	}
	invalid = input
	invalid.IdempotencyKey = "foreign-reference"
	invalid.ImageReference = "registry.invalid/" + tenantSpace.ProjectName + "/app:v1"
	if _, err = p.RegisterPlatformImage(ctx, invalid); biz.ReasonOf(err) != biz.ImageProjectDenied || r.reads != reads {
		t.Fatal("foreign platform project queried", err)
	}
	cursor, _ := biz.NewCursorCodec([]byte(strings.Repeat("c", 32)))
	catalog, _ := biz.NewCatalog(f.Repo, f.Repo, f.Repo, r, cursor, biz.ImageBindingConfig{InstallationID: cfg.InstallationID, RegistryAuthority: cfg.RegistryAuthority, PlatformProject: cfg.PlatformProject})
	read, err := catalog.GetImage(tenantCtx, biz.ReadImage{TenantID: tenant, Scope: biz.PlatformImages, ImageID: registered.ID})
	if err != nil || read.Digest != artifact.Digest {
		t.Fatal("tenant platform read", err)
	}
	update := biz.UpdateImage{ImageID: registered.ID, IdempotencyKey: "platform-update", ExpectedVersion: 1, Metadata: biz.Metadata{DisplayName: "Updated", Purposes: []string{"container"}, Accelerator: "amd"}}
	forged := update
	forged.TenantID = tenant
	if _, err = catalog.UpdateImage(tenantCtx, forged); biz.ReasonOf(err) != biz.ImageNotFound {
		t.Fatal("tenant wrote platform asset", err)
	}
	updated, err := p.UpdatePlatformImage(ctx, update)
	if err != nil || updated.Digest != registered.Digest || updated.Version != 2 {
		t.Fatal("platform metadata", err)
	}
	update.IdempotencyKey = "platform-stale"
	if _, err = p.UpdatePlatformImage(ctx, update); biz.ReasonOf(err) != biz.VersionConflict {
		t.Fatal("platform CAS", err)
	}
	removed, err := p.UnregisterPlatformImage(ctx, biz.UnregisterImage{ImageID: registered.ID, IdempotencyKey: "platform-remove", ExpectedVersion: 2})
	if err != nil || removed.UnregisteredAt == nil || r.reads != reads {
		t.Fatal("platform unregister contacted provider", err)
	}
	// A real tenant row must not be modified by the platform query path.
	tenantRow := registration(tenantSpace, "Tenant app", "c")
	tenantRow, err = f.Repo.ApplyTenantRegistration(ctx, command(tenant, tenantSpace.ID, "register_image"), tenantRow)
	if err != nil {
		t.Fatal(err)
	}
	update.ImageID = tenantRow.ID
	update.ExpectedVersion = tenantRow.Version
	update.IdempotencyKey = "platform-cross-tenant"
	if _, err = p.UpdatePlatformImage(ctx, update); biz.ReasonOf(err) != biz.ImageNotFound {
		t.Fatal("platform wrote tenant registration", err)
	}
	if _, err = p.UnregisterPlatformImage(ctx, biz.UnregisterImage{ImageID: tenantRow.ID, ExpectedVersion: tenantRow.Version, IdempotencyKey: "platform-cross-remove"}); biz.ReasonOf(err) != biz.ImageNotFound {
		t.Fatal("platform removed tenant registration", err)
	}
	expired := time.Now().Add(11 * time.Minute)
	if _, err = f.Repo.PurgeExpiredDeliverySecrets(ctx, expired); err != nil {
		t.Fatal(err)
	}
	if _, err = p.IssuePlatformPublisher(ctx, issuer); biz.ReasonOf(err) != biz.CredentialDeliveryExpired {
		t.Fatal("scrubbed delivery replayed", err)
	}
}
func TestPlatformLostProjectAndRobotRecovery(t *testing.T) {
	for _, phase := range []string{"project", "robot", "secret"} {
		t.Run(phase, func(t *testing.T) {
			f, r, _, _, p, ctx := platformFixture(t)
			init := biz.InitPlatform{IdempotencyKey: "platform-init-recovery"}
			if phase == "project" {
				r.failAfter = "project"
			}
			s, err := p.InitializePlatform(ctx, init)
			if phase == "project" {
				if biz.ReasonOf(err) != biz.SpaceOwnershipUnconfirmed {
					t.Fatal("lost project response claimed", err)
				}
				s, err = f.Repo.FindPlatformSpace(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = p.InitializePlatform(ctx, init); biz.ReasonOf(err) != biz.SpaceOwnershipUnconfirmed {
					t.Fatal("unknown project automatically claimed", err)
				}
				if _, err = p.RecoverProjectBinding(ctx, s.ID, r.next, strings.Repeat("a", 64)); err != nil {
					t.Fatal("explicit project recovery", err)
				}
				s, err = p.InitializePlatform(ctx, init)
			}
			if err != nil || s.State != "available" {
				t.Fatal(err)
			}
			if phase != "project" {
				r.failAfter = phase
			}
			issue := biz.IssuePlatformCredential{IdempotencyKey: "platform-recovery-publisher"}
			first, err := p.IssuePlatformPublisher(ctx, issue)
			if phase != "project" && biz.ReasonOf(err) != biz.DependencyUnavailable {
				t.Fatal("missing injected fault", err)
			}
			clear(first.Secret)
			result, err := p.IssuePlatformPublisher(ctx, issue)
			if err != nil || result.Credential.Generation != 1 || r.robotCreates != 1 {
				t.Fatal("platform recovery duplicated generation", err)
			}
			clear(result.Secret)
		})
	}
}
