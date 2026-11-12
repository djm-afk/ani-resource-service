//go:build imageintegration

package data_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/image"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/image/sqlcgen"
)

type catalogRegistry struct {
	*registryFixture
	guard           sync.Mutex
	artifacts       map[string]biz.Artifact
	tags            map[string]string
	resolves, reads int
	failure         error
}

func (r *catalogRegistry) ResolveArtifact(ctx context.Context, project, repository, ref string) (biz.Artifact, error) {
	r.guard.Lock()
	r.resolves++
	digest := ref
	if !strings.HasPrefix(ref, "sha256:") {
		digest = r.tags[project+"/"+repository+":"+ref]
	}
	r.guard.Unlock()
	return r.GetArtifactByDigest(ctx, project, repository, digest)
}
func (r *catalogRegistry) GetArtifactByDigest(_ context.Context, project, repository, digest string) (biz.Artifact, error) {
	r.guard.Lock()
	defer r.guard.Unlock()
	r.reads++
	if r.failure != nil {
		return biz.Artifact{}, r.failure
	}
	a, ok := r.artifacts[project+"/"+repository+"@"+digest]
	if !ok {
		return a, biz.Fail(biz.ImageNotFound, "fixture digest missing")
	}
	return a, nil
}
func TestCatalogUseCasesAndRuntimeIsolation(t *testing.T) {
	f, registry, cfg, ring, ctx := lifecycleFixture(t)
	tenant := tenantFrom(t, ctx)
	l := lifecycle(t, f.Repo, registry, ring, cfg, nil)
	s, err := l.EnsureImageSpace(ctx, biz.EnableSpace{TenantID: tenant, Slug: "catalog", IdempotencyKey: "catalog-enable"})
	if err != nil {
		t.Fatal(err)
	}
	r := &catalogRegistry{registryFixture: registry, artifacts: map[string]biz.Artifact{}, tags: map[string]string{}}
	codec, err := biz.NewCursorCodec([]byte(strings.Repeat("c", 32)))
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := biz.NewCatalog(f.Repo, f.Repo, f.Repo, r, codec, biz.ImageBindingConfig{InstallationID: cfg.InstallationID, RegistryAuthority: cfg.RegistryAuthority, PlatformProject: cfg.PlatformProject})
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := biz.NewRuntime(f.Repo, f.Repo, f.Repo, r, ring, nil)
	if err != nil {
		t.Fatal(err)
	}
	runtimeCtx := biz.WithCaller(context.Background(), biz.Caller{Kind: biz.RuntimeCaller, Subject: "container-owner", TenantID: tenant})
	artifact := func(ch string) biz.Artifact {
		return biz.Artifact{Digest: "sha256:" + strings.Repeat(ch, 64), MediaType: "application/vnd.oci.image.manifest.v1+json", Platforms: []biz.ImagePlatform{{OS: "linux", Architecture: "amd64"}}}
	}
	a, b := artifact("a"), artifact("b")
	r.artifacts[s.ProjectName+"/repo@"+a.Digest] = a
	r.artifacts[s.ProjectName+"/repo@"+b.Digest] = b
	r.tags[s.ProjectName+"/repo:v1"] = a.Digest
	input := biz.RegisterImage{TenantID: tenant, ImageReference: cfg.RegistryAuthority + "/" + s.ProjectName + "/repo:v1", IdempotencyKey: "register-first", Metadata: biz.Metadata{DisplayName: "100% first", Purposes: []string{"container"}, Accelerator: "none"}}
	first, err := catalog.RegisterImage(ctx, input)
	if err != nil || first.Digest != a.Digest || !strings.HasSuffix(first.ResolvedReference, "@"+a.Digest) {
		t.Fatal("register", err)
	}
	r.tags[s.ProjectName+"/repo:v1"] = b.Digest
	before := r.resolves
	replay, err := catalog.RegisterImage(ctx, input)
	if err != nil || replay.Digest != first.Digest || r.resolves != before {
		t.Fatal("replay followed moved tag", err)
	}
	changed := input
	changed.Metadata.DisplayName = "different"
	if _, err = catalog.RegisterImage(ctx, changed); biz.ReasonOf(err) != biz.IdempotencyConflict || r.resolves != before {
		t.Fatal("changed-key replay contacted provider", err)
	}
	denied := input
	denied.IdempotencyKey = "register-denied"
	denied.ImageReference = cfg.RegistryAuthority + "/t-other/repo:v1"
	if _, err = catalog.RegisterImage(ctx, denied); biz.ReasonOf(err) != biz.ImageProjectDenied || r.resolves != before {
		t.Fatal("foreign project queried", err)
	}
	query := biz.ResolveImage{TenantID: tenant, ImageID: first.ID, Scope: biz.TenantImages, TargetPlatform: biz.ImagePlatform{OS: "linux", Architecture: "amd64"}}
	resolved, err := runtime.ResolveImageForWorkload(runtimeCtx, query)
	if err != nil || resolved.Digest != a.Digest || resolved.Reference != first.ResolvedReference {
		t.Fatal("runtime followed tag", err)
	}
	mismatch := query
	mismatch.TargetPlatform.Architecture = "arm64"
	if _, err = runtime.ResolveImageForWorkload(runtimeCtx, mismatch); biz.ReasonOf(err) != biz.PlatformMismatch {
		t.Fatal("platform mismatch accepted", err)
	}
	r.failure = biz.Fail(biz.DependencyUnavailable, "fixture unavailable")
	if _, err = runtime.ResolveImageForWorkload(runtimeCtx, query); biz.ReasonOf(err) != biz.DependencyUnavailable {
		t.Fatal(err)
	}
	saved, err := f.Repo.FindTenantRegistration(ctx, tenant, first.ID)
	if err != nil || saved.UnregisteredAt != nil || saved.Version != first.Version {
		t.Fatal("provider failure changed asset")
	}
	r.failure = nil
	other := uuid.NewString()
	otherCtx := biz.WithCaller(context.Background(), biz.Caller{Kind: biz.GovernanceCaller, Subject: "governance", Actor: "user:other", TenantID: other})
	if _, err = catalog.GetImage(otherCtx, biz.ReadImage{TenantID: other, Scope: biz.TenantImages, ImageID: first.ID}); biz.ReasonOf(err) != biz.ImageNotFound {
		t.Fatal("cross tenant read", err)
	}
	if _, err = runtime.ResolveImageForWorkload(ctx, query); biz.ReasonOf(err) != biz.PermissionDenied {
		t.Fatal("human runtime access", err)
	}
	pull, err := runtime.GetTenantPullMaterial(runtimeCtx, tenant)
	if err != nil || pull.TenantID != tenant || len(pull.Secret) == 0 {
		t.Fatal("pull material", err)
	}
	if _, err = runtime.GetTenantPullMaterial(runtimeCtx, other); biz.ReasonOf(err) != biz.PermissionDenied {
		t.Fatal("cross tenant pull", err)
	}
	expired, _ := biz.NewRuntime(f.Repo, f.Repo, f.Repo, r, ring, func() time.Time { return time.Now().Add(366 * 24 * time.Hour) })
	if _, err = expired.GetTenantPullMaterial(runtimeCtx, tenant); biz.ReasonOf(err) != biz.SpaceNotReady {
		t.Fatal("expired pull delivered", err)
	}
	update := biz.UpdateImage{TenantID: tenant, ImageID: first.ID, IdempotencyKey: "metadata-first", ExpectedVersion: 1, Metadata: biz.Metadata{DisplayName: "100% renamed", Purposes: []string{"training", "container"}, Accelerator: "nvidia"}}
	updated, err := catalog.UpdateImage(ctx, update)
	if err != nil || updated.Digest != first.Digest || updated.ResolvedReference != first.ResolvedReference {
		t.Fatal("metadata changed immutable image", err)
	}
	stale := update
	stale.IdempotencyKey = "metadata-stale"
	if _, err = catalog.UpdateImage(ctx, stale); biz.ReasonOf(err) != biz.VersionConflict {
		t.Fatal("metadata CAS", err)
	}
	beforeReads := r.reads
	removed, err := catalog.UnregisterImage(ctx, biz.UnregisterImage{TenantID: tenant, ImageID: first.ID, IdempotencyKey: "unregister-first", ExpectedVersion: updated.Version})
	if err != nil || removed.UnregisteredAt == nil || r.reads != beforeReads {
		t.Fatal("unregister provider side effect", err)
	}
	detail, err := catalog.GetImage(ctx, biz.ReadImage{TenantID: tenant, Scope: biz.TenantImages, ImageID: first.ID})
	if err != nil || detail.UnregisteredAt == nil {
		t.Fatal("cancelled detail hidden", err)
	}
	if _, err = runtime.ResolveImageForWorkload(runtimeCtx, query); biz.ReasonOf(err) != biz.ImageNotFound || r.reads != beforeReads {
		t.Fatal("cancelled asset resolved", err)
	}
	input.IdempotencyKey = "register-again"
	input.ImageReference = cfg.RegistryAuthority + "/" + s.ProjectName + "/repo@" + a.Digest
	again, err := catalog.RegisterImage(ctx, input)
	if err != nil || again.ID == first.ID {
		t.Fatal("tombstone resurrected", err)
	}
	// Filtering precedes keyset LIMIT, including a literal '%' rather than wildcard.
	for i, ch := range []string{"c", "d", "e", "f"} {
		image := artifact(ch)
		r.artifacts[s.ProjectName+"/repo@"+image.Digest] = image
		name := fmt.Sprintf("100%% item %d", i)
		if i%2 == 1 {
			name = "100x nonmatching"
		}
		_, err = catalog.RegisterImage(ctx, biz.RegisterImage{TenantID: tenant, ImageReference: cfg.RegistryAuthority + "/" + s.ProjectName + "/repo@" + image.Digest, IdempotencyKey: fmt.Sprintf("catalog-item-%d", i), Metadata: biz.Metadata{DisplayName: name, Purposes: []string{"container"}, Accelerator: "none"}})
		if err != nil {
			t.Fatal(err)
		}
	}
	list := biz.ListImages{TenantID: tenant, Scope: biz.TenantImages, Filter: biz.Filter{Search: "100%", Purposes: []string{"container"}, Limit: 2}}
	page, err := catalog.ListImages(ctx, list)
	if err != nil || len(page.Items) != 2 || page.NextCursor == "" || page.Total != 3 {
		t.Fatal("page1", err)
	}
	list.Cursor = page.NextCursor
	page2, err := catalog.ListImages(ctx, list)
	if err != nil || len(page2.Items) != 1 || page2.NextCursor != "" || page2.Total != 3 {
		t.Fatal("page2", err)
	}
	for _, v := range page.Items {
		if v.ID == page2.Items[0].ID {
			t.Fatal("duplicate page item")
		}
	}

	for _, filter := range []biz.Filter{{Search: "absent", Limit: 1}, {Purposes: []string{"training"}, Limit: 1}, {Accelerator: "nvidia", Limit: 1}} {
		missing, err := catalog.ListImages(ctx, biz.ListImages{TenantID: tenant, Scope: biz.TenantImages, Filter: filter})
		if err != nil || missing.Total != 0 || len(missing.Items) != 0 {
			t.Fatal("filtered total", missing, err)
		}
	}
	list.TenantID = other
	if _, err = catalog.ListImages(otherCtx, list); biz.ReasonOf(err) != biz.InvalidCursor {
		t.Fatal("cross tenant cursor", err)
	}
	// Platform read works without a tenant space; writes still use tenant SQL.
	platform, err := f.Repo.FindPlatformSpace(ctx)
	if err != nil {
		t.Fatal(err)
	}
	platformID := "img_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	platforms, _ := json.Marshal(a.Platforms)
	_, err = sqlcgen.New(f.Runtime).InsertPlatformRegistration(ctx, sqlcgen.InsertPlatformRegistrationParams{ImageID: platformID, SpaceID: platform.ID, DisplayName: "platform image", Description: "", Repository: "repo", SourceReference: "registry.invalid/platform/repo:v1", Digest: a.Digest, MediaType: a.MediaType, Platforms: platforms, Purposes: []string{"container"}, Accelerator: "none", Actor: "operator:test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = catalog.GetImage(otherCtx, biz.ReadImage{TenantID: other, Scope: biz.PlatformImages, ImageID: platformID}); err != nil {
		t.Fatal("platform read before tenant enable", err)
	}
	platformPage, err := catalog.ListImages(otherCtx, biz.ListImages{TenantID: other, Scope: biz.PlatformImages, Filter: biz.Filter{Limit: 1}})
	if err != nil || platformPage.Total != 1 || len(platformPage.Items) != 1 {
		t.Fatal("platform total", platformPage, err)
	}
	emptyPage, err := catalog.ListImages(otherCtx, biz.ListImages{TenantID: other, Scope: biz.TenantImages, Filter: biz.Filter{Limit: 1}})
	if err != nil || emptyPage.Total != 0 || len(emptyPage.Items) != 0 {
		t.Fatal("tenant total isolation", emptyPage, err)
	}
	update.ImageID = platformID
	update.IdempotencyKey = "platform-denied"
	if _, err = catalog.UpdateImage(ctx, update); biz.ReasonOf(err) != biz.ImageNotFound {
		t.Fatal("tenant mutated platform", err)
	}
	r.artifacts["platform/repo@"+a.Digest] = a
	query.ImageID = platformID
	query.Scope = biz.PlatformImages
	resolved, err = runtime.ResolveImageForWorkload(runtimeCtx, query)
	if err != nil || resolved.TenantID != tenant || resolved.Scope != biz.PlatformImages {
		t.Fatal("platform resolve", err)
	}
}
