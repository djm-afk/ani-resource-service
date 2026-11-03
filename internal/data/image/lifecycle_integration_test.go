//go:build imageintegration

package data_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/image"
	imagedata "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/image"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/image/sqlcgen"
)

// This is an isolated provider fault fixture, never real Harbor evidence. Its
// durable side effects outlive a use-case instance while real PG stores commands.
type registryFixture struct {
	mu                                sync.Mutex
	projects                          map[int64]biz.Project
	robots                            map[int64]biz.Robot
	secrets                           map[int64]string
	next                              int64
	creates, robotCreates, secretSets int
	disableSets                       int
	failAfter                         string
	// now keeps provider-side robot lifetimes on the same clock the lifecycle
	// under test uses, so advancing time to expire a credential also decides
	// the lifetime of its successor.
	now func() time.Time
}

func (r *registryFixture) clock() time.Time {
	if r.now == nil {
		return time.Now()
	}
	return r.now()
}

func newRegistry() *registryFixture {
	return &registryFixture{projects: map[int64]biz.Project{1: {ID: 1, Name: "platform", Private: true}}, robots: map[int64]biz.Robot{}, secrets: map[int64]string{}, next: 10}
}
func (r *registryFixture) GetProjectByID(_ context.Context, id int64) (biz.Project, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.projects[id]
	if !ok {
		return p, biz.Fail(biz.ImageNotFound, "fixture missing")
	}
	return p, nil
}
func (r *registryFixture) FindProjectByName(_ context.Context, name string) (biz.Project, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.projects {
		if p.Name == name {
			return p, nil
		}
	}
	return biz.Project{}, biz.Fail(biz.ImageNotFound, "fixture missing")
}
func (r *registryFixture) CreatePrivateProject(_ context.Context, name string) (biz.Project, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.projects {
		if p.Name == name {
			return biz.Project{}, biz.Fail(biz.IdempotencyConflict, "fixture exists")
		}
	}
	r.next++
	p := biz.Project{ID: r.next, Name: name, Private: true}
	r.projects[p.ID] = p
	r.creates++
	if r.failAfter == "project" {
		r.failAfter = ""
		return biz.Project{}, biz.Fail(biz.DependencyUnavailable, "injected lost response")
	}
	return p, nil
}
func (r *registryFixture) GetRobot(_ context.Context, id int64) (biz.Robot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.robots[id]
	if !ok {
		return v, biz.Fail(biz.ImageNotFound, "fixture missing")
	}
	return v, nil
}
func (r *registryFixture) FindOwnedRobot(_ context.Context, want biz.RobotRequest) (biz.Robot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, v := range r.robots {
		if v.Name == "fixture$"+want.Name {
			if v.Description != want.Description || biz.ValidateRobotPermissions(v.Permissions, want.Permissions) != nil {
				return biz.Robot{}, biz.Fail(biz.PermissionDenied, "fixture ownership mismatch")
			}
			return v, nil
		}
	}
	return biz.Robot{}, biz.Fail(biz.ImageNotFound, "fixture missing")
}
func (r *registryFixture) CreateRobot(_ context.Context, want biz.RobotRequest) (biz.Robot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, v := range r.robots {
		if v.Name == "fixture$"+want.Name {
			return biz.Robot{}, biz.Fail(biz.IdempotencyConflict, "fixture exists")
		}
	}
	r.next++
	expiry := r.clock().Add(time.Duration(want.DurationDays) * 24 * time.Hour).Unix()
	if want.DurationDays == -1 {
		expiry = -1
	}
	v := biz.Robot{ID: r.next, Name: "fixture$" + want.Name, Username: "fixture$" + want.Name, Description: want.Description, Permissions: want.Permissions, ExpiresAt: expiry, DurationDays: want.DurationDays}
	r.robots[v.ID] = v
	r.robotCreates++
	if r.failAfter == "robot" {
		r.failAfter = ""
		return biz.Robot{}, biz.Fail(biz.DependencyUnavailable, "injected lost response")
	}
	return v, nil
}
func (r *registryFixture) SetRobotSecret(_ context.Context, want biz.Robot, secret biz.Secret) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.robots[want.ID]
	if !ok || v.Name != want.Name || v.Description != want.Description {
		return biz.Fail(biz.PermissionDenied, "fixture ownership mismatch")
	}
	if previous := r.secrets[v.ID]; previous != "" && previous != string(secret) {
		return biz.Fail(biz.InternalError, "same generation secret changed")
	}
	r.secrets[v.ID] = string(secret)
	r.secretSets++
	if r.failAfter == "secret" {
		r.failAfter = ""
		return biz.Fail(biz.DependencyUnavailable, "injected lost response")
	}
	return nil
}
func (r *registryFixture) SetRobotDisabled(_ context.Context, want biz.Robot, disabled bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.robots[want.ID]
	if !ok || v.Name != want.Name || v.Description != want.Description {
		return biz.Fail(biz.PermissionDenied, "fixture ownership mismatch")
	}
	v.Disabled = disabled
	r.robots[v.ID] = v
	r.disableSets++
	return nil
}
func (r *registryFixture) ResolveArtifact(context.Context, string, string, string) (biz.Artifact, error) {
	return biz.Artifact{}, biz.Fail(biz.UnsupportedArtifact, "unused fixture method")
}
func (r *registryFixture) GetArtifactByDigest(context.Context, string, string, string) (biz.Artifact, error) {
	return biz.Artifact{}, biz.Fail(biz.UnsupportedArtifact, "unused fixture method")
}
func lifecycleFixture(t *testing.T) (*fixture, *registryFixture, biz.LifecycleConfig, *imagedata.AESGCMKeyring, context.Context) {
	t.Helper()
	f := database(t)
	r := newRegistry()
	cfg := biz.LifecycleConfig{InstallationID: uuid.NewString(), RegistryAuthority: "registry.invalid", PlatformProject: "platform", PublisherDays: 30, PullDays: 365}
	q := sqlcgen.New(f.Runtime)
	ctx := context.Background()
	space, err := q.ReservePlatformSpace(ctx, sqlcgen.ReservePlatformSpaceParams{SpaceID: uuid.NewString(), InstallationID: cfg.InstallationID, RegistryAuthority: cfg.RegistryAuthority, ProjectName: cfg.PlatformProject})
	if err != nil {
		t.Fatal(err)
	}
	space, err = q.BindPlatformProject(ctx, sqlcgen.BindPlatformProjectParams{SpaceID: space.SpaceID, ExpectedVersion: space.Version, ProjectID: pgtype.Int8{Int64: 1, Valid: true}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = q.SetPlatformSpaceAvailable(ctx, sqlcgen.SetPlatformSpaceAvailableParams{SpaceID: space.SpaceID, ExpectedVersion: space.Version}); err != nil {
		t.Fatal(err)
	}
	ring, err := imagedata.NewAESGCMKeyring("test", map[string][]byte{"test": []byte(strings.Repeat("k", 32))})
	if err != nil {
		t.Fatal(err)
	}
	ctx = biz.WithCaller(ctx, biz.Caller{Kind: biz.GovernanceCaller, Subject: "governance", Actor: "user:test", TenantID: uuid.NewString()})
	return f, r, cfg, ring, ctx
}
func lifecycle(t *testing.T, repo biz.LifecycleRepository, r biz.Registry, ring biz.SecretCipher, cfg biz.LifecycleConfig, clock func() time.Time) *biz.Lifecycle {
	t.Helper()
	l, err := biz.NewLifecycle(repo, r, ring, cfg, clock)
	if err != nil {
		t.Fatal(err)
	}
	return l
}
func tenantFrom(t *testing.T, ctx context.Context) string {
	t.Helper()
	c, e := biz.CallerFromContext(ctx)
	if e != nil {
		t.Fatal(e)
	}
	return c.TenantID
}
func TestLifecycleEnableIssueResetDisableReplay(t *testing.T) {
	f, r, cfg, ring, ctx := lifecycleFixture(t)
	now := time.Now()
	l := lifecycle(t, f.Repo, r, ring, cfg, func() time.Time { return now })
	tenant := tenantFrom(t, ctx)
	in := biz.EnableSpace{TenantID: tenant, Slug: "alpha", IdempotencyKey: "enable-key"}
	s, err := l.EnsureImageSpace(ctx, in)
	if err != nil || s.State != "available" || s.PullCredentialGeneration != 1 {
		t.Fatal(s, err)
	}
	for _, key := range []string{"enable-key", "enable-key-2"} {
		in.IdempotencyKey = key
		if _, err = l.EnsureImageSpace(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	if r.creates != 1 || r.robotCreates != 1 {
		t.Fatal("enable duplicated provider identities")
	}
	before, err := l.GetPublisherCredential(ctx, tenant)
	if err != nil || before.State != "not_issued" || before.Version != 0 {
		t.Fatal(before, err)
	}
	request := biz.IssueCredential{TenantID: tenant, IdempotencyKey: "issue-key", ExpectedVersion: 0}
	issued, err := l.IssuePublisherCredential(ctx, request)
	if err != nil || len(issued.Secret) < 32 || issued.Credential.Generation != 1 {
		t.Fatal("issue", err)
	}
	replay, err := l.IssuePublisherCredential(ctx, request)
	if err != nil || string(replay.Secret) != string(issued.Secret) || r.robotCreates != 2 {
		t.Fatal("replay", err)
	}
	other := biz.WithCaller(ctx, biz.Caller{Kind: biz.GovernanceCaller, Subject: "governance", Actor: "user:other", TenantID: tenant})
	if _, err = l.IssuePublisherCredential(other, request); biz.ReasonOf(err) != biz.IdempotencyConflict {
		t.Fatal("actor replay", err)
	}
	stale := request
	stale.IdempotencyKey = "issue-stale"
	if _, err = l.IssuePublisherCredential(ctx, stale); biz.ReasonOf(err) != biz.VersionConflict {
		t.Fatal("stale version", err)
	}
	reset, err := l.ResetPublisherCredential(ctx, biz.ResetCredential{TenantID: tenant, IdempotencyKey: "reset-key", ExpectedVersion: issued.Credential.Version})
	if err != nil || reset.Credential.Generation != 2 || string(reset.Secret) == string(issued.Secret) {
		t.Fatal("reset", err)
	}
	old, _ := r.GetRobot(ctx, issued.Credential.RobotID)
	if !old.Disabled {
		t.Fatal("old generation not disabled")
	}
	if _, err = l.IssuePublisherCredential(ctx, request); biz.ReasonOf(err) != biz.CredentialDeliveryExpired {
		t.Fatal("old generation replay", err)
	}
	now = now.Add(11 * time.Minute)
	if _, err = l.ResetPublisherCredential(ctx, biz.ResetCredential{TenantID: tenant, IdempotencyKey: "reset-key", ExpectedVersion: issued.Credential.Version}); biz.ReasonOf(err) != biz.CredentialDeliveryExpired {
		t.Fatal("late replay", err)
	}
	disabled, err := l.DisablePublisherCredential(ctx, biz.DisableCredential{TenantID: tenant, IdempotencyKey: "disable-key", ExpectedVersion: reset.Credential.Version})
	if err != nil || disabled.State != "disabled" {
		t.Fatal("disable", err)
	}
	writes := r.robotCreates
	if _, err = l.DisablePublisherCredential(ctx, biz.DisableCredential{TenantID: tenant, IdempotencyKey: "disable-key", ExpectedVersion: reset.Credential.Version}); err != nil || r.robotCreates != writes {
		t.Fatal("disable replay", err)
	}
	pull, err := f.Repo.GetTenantPull(ctx, tenant, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := ring.Open(ctx, pull.AAD, pull.Secret)
	if err != nil || string(plain) != r.secrets[pull.Info.RobotID] {
		t.Fatal("durable encrypted pull", err)
	}
	var leaked bool
	if err = f.Runtime.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM image.commands WHERE candidate::text LIKE $1 OR result::text LIKE $1)`, "%"+string(issued.Secret)+"%").Scan(&leaked); err != nil || leaked {
		t.Fatal("plaintext in command snapshot", err)
	}
	var retained int
	if err = f.Runtime.QueryRow(ctx, `SELECT count(*) FROM image.commands WHERE kind='enable_space' AND secret_ciphertext IS NOT NULL`).Scan(&retained); err != nil || retained != 0 {
		t.Fatal("enable retained delivery secret", err)
	}
	operator := biz.WithCaller(context.Background(), biz.Caller{Kind: biz.PlatformCaller, Subject: "local-cli", Actor: "operator:test"})
	n, err := l.PurgeExpiredDeliverySecrets(operator)
	if err != nil || n != 2 {
		t.Fatal("expiry purge", n, err)
	}
	if _, err = l.PurgeExpiredDeliverySecrets(ctx); biz.ReasonOf(err) != biz.PermissionDenied {
		t.Fatal("tenant purge accepted", err)
	}
}
func TestLifecycleConcurrentCredentialCommand(t *testing.T) {
	f, r, cfg, ring, ctx := lifecycleFixture(t)
	tenant := tenantFrom(t, ctx)
	l := lifecycle(t, f.Repo, r, ring, cfg, nil)
	if _, err := l.EnsureImageSpace(ctx, biz.EnableSpace{TenantID: tenant, Slug: "concurrent", IdempotencyKey: "enable-key"}); err != nil {
		t.Fatal(err)
	}
	request := biz.IssueCredential{TenantID: tenant, IdempotencyKey: "same-key", ExpectedVersion: 0}
	ch := make(chan biz.CredentialDelivery, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); d, e := l.IssuePublisherCredential(ctx, request); ch <- d; errs <- e }()
	}
	wg.Wait()
	a, b := <-ch, <-ch
	for range 2 {
		if e := <-errs; e != nil {
			t.Fatal(e)
		}
	}
	if string(a.Secret) != string(b.Secret) || r.robotCreates != 2 {
		t.Fatal("concurrent command issued twice")
	}
}

type lostAckRepository struct {
	biz.LifecycleRepository
	at    string
	fired bool
}

func (r *lostAckRepository) after(at string) error {
	if r.at == at && !r.fired {
		r.fired = true
		return biz.Fail(biz.DependencyUnavailable, "injected persistence acknowledgement loss")
	}
	return nil
}
func (r *lostAckRepository) BindTenantProject(ctx context.Context, s biz.Space, id int64) (biz.Space, error) {
	v, e := r.LifecycleRepository.BindTenantProject(ctx, s, id)
	if e == nil {
		e = r.after("project_bind")
	}
	return v, e
}
func (r *lostAckRepository) PrepareTenantCandidate(ctx context.Context, c biz.Command) (biz.Command, error) {
	v, e := r.LifecycleRepository.PrepareTenantCandidate(ctx, c)
	if e == nil {
		e = r.after("candidate_prepared")
	}
	return v, e
}
func (r *lostAckRepository) SaveTenantCommandPhase(ctx context.Context, c biz.Command) (biz.Command, error) {
	v, e := r.LifecycleRepository.SaveTenantCommandPhase(ctx, c)
	if e == nil {
		e = r.after(c.Phase)
	}
	return v, e
}
func (r *lostAckRepository) ActivateTenantCandidate(ctx context.Context, s biz.Space, c biz.Command, secret biz.EncryptedSecret) (biz.Space, biz.Command, error) {
	v, k, e := r.LifecycleRepository.ActivateTenantCandidate(ctx, s, c, secret)
	if e == nil {
		e = r.after("activate")
	}
	return v, k, e
}
func TestLifecycleResumeAtPersistenceAndProviderBoundaries(t *testing.T) {
	for _, at := range []string{"project_bind", "candidate_prepared", "robot_created", "secret_set", "previous_disabled", "activate", "provider_robot", "provider_secret"} {
		t.Run(at, func(t *testing.T) {
			f, r, cfg, ring, ctx := lifecycleFixture(t)
			repo := &lostAckRepository{LifecycleRepository: f.Repo, at: at}
			if strings.HasPrefix(at, "provider_") {
				r.failAfter = strings.TrimPrefix(at, "provider_")
			}
			l := lifecycle(t, repo, r, ring, cfg, nil)
			in := biz.EnableSpace{TenantID: tenantFrom(t, ctx), Slug: "resume", IdempotencyKey: "resume-key"}
			if _, err := l.EnsureImageSpace(ctx, in); err == nil {
				t.Fatal("fault not reached")
			}
			l = lifecycle(t, f.Repo, r, ring, cfg, nil)
			s, err := l.EnsureImageSpace(ctx, in)
			if err != nil || s.State != "available" || r.creates != 1 || r.robotCreates != 1 {
				t.Fatal("resume", s, err)
			}
			if at == "provider_secret" && r.secretSets != 2 {
				t.Fatal("stable secret retry missing")
			}
		})
	}
}
func TestLifecycleProjectOwnershipRecovery(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(fmt.Sprint(lost), func(t *testing.T) {
			f, r, cfg, ring, ctx := lifecycleFixture(t)
			if lost {
				r.failAfter = "project"
			} else {
				r.projects[701] = biz.Project{ID: 701, Name: "t-owned", Private: true}
			}
			l := lifecycle(t, f.Repo, r, ring, cfg, nil)
			in := biz.EnableSpace{TenantID: tenantFrom(t, ctx), Slug: "owned", IdempotencyKey: "ownership-key"}
			if _, err := l.EnsureImageSpace(ctx, in); biz.ReasonOf(err) != biz.SpaceOwnershipUnconfirmed {
				t.Fatal("unknown project adopted", err)
			}
			s, err := f.Repo.FindTenantSpace(ctx, in.TenantID)
			if err != nil || s.ProjectID != 0 || r.robotCreates != 0 {
				t.Fatal("uncertain state", err)
			}
			p, _ := r.FindProjectByName(ctx, "t-owned")
			if _, err = l.RecoverProjectBinding(ctx, s.ID, p.ID, strings.Repeat("a", 64)); biz.ReasonOf(err) != biz.PermissionDenied {
				t.Fatal("tenant recovered ownership", err)
			}
			operator := biz.WithCaller(context.Background(), biz.Caller{Kind: biz.PlatformCaller, Subject: "local-cli", Actor: "operator:test"})
			if _, err = l.RecoverProjectBinding(operator, s.ID, p.ID, strings.Repeat("a", 64)); err != nil {
				t.Fatal(err)
			}
			s, err = l.EnsureImageSpace(ctx, in)
			if err != nil || s.State != "available" {
				t.Fatal("recovered enable", err)
			}
			c, err := f.Repo.FindTenantCommand(ctx, s.TenantID, s.ID, in.IdempotencyKey)
			if err != nil || c.Candidate.RecoveryEvidence != strings.Repeat("a", 64) {
				t.Fatal("ownership evidence lost", err)
			}
		})
	}
}

// An expired pull credential backs no running workload: every token minted
// from it is already invalid. Enable must therefore mint the next generation
// instead of refusing, otherwise the only repair entry point stays blocked
// forever and every image consumer fails with SPACE_NOT_READY.
func TestLifecycleEnableRepairsExpiredPullCredential(t *testing.T) {
	f, r, cfg, ring, ctx := lifecycleFixture(t)
	now := time.Now()
	r.now = func() time.Time { return now }
	l := lifecycle(t, f.Repo, r, ring, cfg, func() time.Time { return now })
	tenant := tenantFrom(t, ctx)
	in := biz.EnableSpace{TenantID: tenant, Slug: "expired-pull", IdempotencyKey: "enable-key"}
	s, err := l.EnsureImageSpace(ctx, in)
	if err != nil || s.State != "available" || s.PullCredentialGeneration != 1 {
		t.Fatal(s, err)
	}
	first, err := f.Repo.GetTenantPull(ctx, tenant, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Advance past the credential lifetime without touching the provider, so
	// only expiry (not a disable) is what stops the current generation.
	now = now.Add(time.Duration(cfg.PullDays+1) * 24 * time.Hour)
	space, err := l.GetImageSpace(ctx, tenant)
	if err != nil || space.State != "blocked" {
		t.Fatalf("expired pull credential should read as blocked: %+v %v", space, err)
	}
	repaired, err := l.EnsureImageSpace(ctx, biz.EnableSpace{TenantID: tenant, Slug: "expired-pull", IdempotencyKey: "repair-key"})
	if err != nil {
		t.Fatalf("enable must repair an expired pull credential: %v", err)
	}
	if repaired.State != "available" || repaired.PullCredentialGeneration <= first.Info.Generation {
		t.Fatal("pull credential generation did not advance", repaired)
	}
	second, err := f.Repo.GetTenantPull(ctx, tenant, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The repaired generation must be the one the space reports as current.
	if repaired.PullCredentialGeneration != second.Info.Generation {
		t.Fatal("space does not report the repaired generation", repaired.PullCredentialGeneration, second.Info.Generation)
	}
	if second.Info.State != "active" || second.Info.Generation <= first.Info.Generation {
		t.Fatal("repaired pull credential is not an active successor", second.Info)
	}
	// Reads must stop reporting the space as blocked once a live successor exists.
	afterRepair, err := l.GetImageSpace(ctx, tenant)
	if err != nil || afterRepair.State != "available" {
		t.Fatal("space still blocked after repair", afterRepair, err)
	}
	if second.Info.RobotID == first.Info.RobotID {
		t.Fatal("repair reused the expired robot identity")
	}
	// The retired identity must not be left usable behind the new generation.
	old, err := r.GetRobot(ctx, first.Info.RobotID)
	if err != nil || !old.Disabled {
		t.Fatal("expired generation was not retired", err)
	}
	plain, err := ring.Open(ctx, second.AAD, second.Secret)
	if err != nil || string(plain) != r.secrets[second.Info.RobotID] {
		t.Fatal("repaired credential secret mismatch", err)
	}
}

// A credential that is still live backs running workloads, so enable must
// keep succeeding idempotently instead of rotating it behind their back.
func TestLifecycleEnableKeepsLivePullCredential(t *testing.T) {
	f, r, cfg, ring, ctx := lifecycleFixture(t)
	now := time.Now()
	r.now = func() time.Time { return now }
	l := lifecycle(t, f.Repo, r, ring, cfg, func() time.Time { return now })
	tenant := tenantFrom(t, ctx)
	in := biz.EnableSpace{TenantID: tenant, Slug: "live-pull", IdempotencyKey: "enable-key"}
	s, err := l.EnsureImageSpace(ctx, in)
	if err != nil || s.State != "available" {
		t.Fatal(s, err)
	}
	first, err := f.Repo.GetTenantPull(ctx, tenant, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Still well inside the credential lifetime: repeated enable must neither
	// fail nor mint a replacement identity.
	now = now.Add(24 * time.Hour)
	again, err := l.EnsureImageSpace(ctx, biz.EnableSpace{TenantID: tenant, Slug: "live-pull", IdempotencyKey: "enable-key-2"})
	if err != nil || again.State != "available" {
		t.Fatal("live credential must keep enable succeeding", again, err)
	}
	second, err := f.Repo.GetTenantPull(ctx, tenant, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if second.Info.Generation != first.Info.Generation || second.Info.RobotID != first.Info.RobotID {
		t.Fatal("enable rotated a live pull credential")
	}
	if r.robotCreates != 1 {
		t.Fatal("enable created an extra provider identity", r.robotCreates)
	}
}
