package biz

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

type LifecycleConfig struct {
	InstallationID, RegistryAuthority, PlatformProject string
	PublisherDays, PullDays                            int64
	AllowNeverExpires                                  bool
}
type Lifecycle struct {
	repo         LifecycleRepository
	platformRepo PlatformRepository
	registry     Registry
	cipher       SecretCipher
	cfg          LifecycleConfig
	now          func() time.Time
}

func NewLifecycle(repo LifecycleRepository, registry Registry, cipher SecretCipher, cfg LifecycleConfig, now func() time.Time) (*Lifecycle, error) {
	if _, err := ParseTenant(cfg.InstallationID); err != nil {
		return nil, Fail(InvalidArgument, "invalid installation ID")
	}
	if cfg.RegistryAuthority == "" || strings.ContainsAny(cfg.RegistryAuthority, "/@?#%\\ \r\n") || !regexp.MustCompile(`^[a-z][a-z0-9-]{1,46}[a-z0-9]$`).MatchString(cfg.PlatformProject) {
		return nil, Fail(InvalidArgument, "invalid Image registry binding")
	}
	for _, days := range []int64{cfg.PublisherDays, cfg.PullDays} {
		if days == -1 && cfg.AllowNeverExpires {
			continue
		}
		if days < 1 || days > 3650 {
			return nil, Fail(InvalidArgument, "explicit finite credential duration required")
		}
	}
	if repo == nil || registry == nil || cipher == nil {
		return nil, Fail(InvalidArgument, "Image dependencies required")
	}
	if now == nil {
		now = time.Now
	}
	return &Lifecycle{repo: repo, registry: registry, cipher: cipher, cfg: cfg, now: now}, nil
}
func requestCommand(caller Caller, space, kind, key string, input any) (Command, error) {
	if _, err := ParseIdempotencyKey(key); err != nil {
		return Command{}, err
	}
	body, err := json.Marshal(input)
	if err != nil {
		return Command{}, Fail(InternalError, "request encoding failed")
	}
	hash := sha256.Sum256(body)
	return Command{ID: uuid.NewString(), SpaceID: space, TenantID: caller.TenantID, Scope: TenantImages, Kind: kind, Key: key, Actor: caller.Actor, Fingerprint: hex.EncodeToString(hash[:])}, nil
}
func (l *Lifecycle) checkSpace(s Space) error {
	if s.Scope != TenantImages || s.InstallationID != l.cfg.InstallationID || s.RegistryAuthority != l.cfg.RegistryAuthority {
		return Fail(SpaceNotReady, "image space configuration mismatch")
	}
	return nil
}
func (l *Lifecycle) platform(ctx context.Context) (Space, error) {
	s, err := l.repo.FindPlatformSpace(ctx)
	if err != nil {
		return Space{}, err
	}
	if s.Scope != PlatformImages || s.TenantID != "" || s.State != "available" || s.InstallationID != l.cfg.InstallationID || s.RegistryAuthority != l.cfg.RegistryAuthority || s.ProjectName != l.cfg.PlatformProject || s.ProjectID <= 0 {
		return Space{}, Fail(SpaceNotReady, "platform image space not ready")
	}
	p, err := l.registry.GetProjectByID(ctx, s.ProjectID)
	if err != nil {
		return Space{}, err
	}
	if p.ID != s.ProjectID || p.Name != s.ProjectName || !p.Private {
		return Space{}, Fail(SpaceNotReady, "platform registry project mismatch")
	}
	return s, nil
}
func (l *Lifecycle) GetImageSpace(ctx context.Context, tenant string) (Space, error) {
	if _, err := RequireTenant(ctx, tenant); err != nil {
		return Space{}, err
	}
	s, err := l.repo.FindTenantSpace(ctx, tenant)
	if err != nil {
		return Space{}, err
	}
	if err = l.checkSpace(s); err != nil {
		return Space{}, err
	}
	pull, err := l.repo.GetTenantPull(ctx, tenant, s.ID)
	if err == nil {
		s.PullCredentialGeneration = pull.Info.Generation
		if !l.credentialActive(pull.Info) && s.State == "available" {
			s.State = "blocked"
			s.Reason = SpaceNotReady
		}
	} else if ReasonOf(err) != ImageNotFound {
		return Space{}, err
	}
	return s, nil
}
func (l *Lifecycle) EnsureImageSpace(ctx context.Context, in EnableSpace) (Space, error) {
	caller, err := RequireTenant(ctx, in.TenantID)
	if err != nil {
		return Space{}, err
	}
	slug, err := ParseSlug(in.Slug)
	if err != nil {
		return Space{}, err
	}
	c, err := requestCommand(caller, "", "enable_space", in.IdempotencyKey, in)
	if err != nil {
		return Space{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	s := Space{ID: uuid.NewString(), TenantID: in.TenantID, Scope: TenantImages, InstallationID: l.cfg.InstallationID, RegistryAuthority: l.cfg.RegistryAuthority, ProjectName: "t-" + slug}
	s, c, err = l.repo.ReserveTenantSpace(ctx, s, c)
	if err != nil {
		return Space{}, err
	}
	err = l.repo.WithSpaceWriteLock(ctx, s.ID, func(ctx context.Context) error {
		var e error
		s, e = l.repo.FindTenantSpace(ctx, in.TenantID)
		if e != nil {
			return e
		}
		c, e = l.repo.FindTenantCommand(ctx, in.TenantID, s.ID, in.IdempotencyKey)
		if e != nil {
			return e
		}
		if c.State == "succeeded" {
			if c.Result.Space == nil {
				return Fail(InternalError, "missing enable result")
			}
			s = *c.Result.Space
			return nil
		}
		if e = l.checkSpace(s); e != nil {
			return e
		}
		if e = recoveredCandidate(c); e != nil {
			return e
		}
		if _, e = l.platform(ctx); e != nil {
			return e
		}
		if e = l.resumeProject(ctx, &s, &c); e != nil {
			return e
		}
		pull, e := l.repo.GetTenantPull(ctx, s.TenantID, s.ID)
		if e == nil && pull.Info.Generation > 0 {
			if l.credentialActive(pull.Info) {
				s, c, e = l.repo.CompleteTenantEnable(ctx, s, c)
				return e
			}
			// An expired pull credential cannot be repaired by waiting: the
			// space is reported as blocked on every read until a successor
			// exists. Fall through so runCandidate mints the next generation
			// instead of failing the only repair entry point.
		}
		if e != nil && ReasonOf(e) != ImageNotFound {
			return e
		}
		info := CredentialInfo{TenantID: s.TenantID, SpaceID: s.ID, Scope: TenantImages, Purpose: "pull", State: "not_issued"}
		if e == nil {
			info = pull.Info
		}
		s, c, e = l.runCandidate(ctx, s, c, info)
		return e
	})
	return s, err
}
func (l *Lifecycle) phase(ctx context.Context, c *Command, phase string) error {
	c.Phase = phase
	c.State = "running"
	c.Reason = ""
	saved, err := l.saveCommandPhase(ctx, *c)
	if err == nil {
		*c = saved
	}
	return err
}
func (l *Lifecycle) unconfirmedProject(ctx context.Context, s *Space, c *Command) error {
	c.State = "blocked"
	c.Reason = SpaceOwnershipUnconfirmed
	saved, err := l.saveCommandPhase(ctx, *c)
	if err != nil {
		return err
	}
	*c = saved
	blocked, err := l.blockSpace(ctx, *s, SpaceOwnershipUnconfirmed)
	if err != nil {
		return err
	}
	*s = blocked
	return Fail(SpaceOwnershipUnconfirmed, "registry project requires explicit ownership recovery")
}
func (l *Lifecycle) resumeProject(ctx context.Context, s *Space, c *Command) error {
	if s.ProjectID == 0 {
		if c.State == "blocked" && c.Reason == SpaceOwnershipUnconfirmed {
			return Fail(SpaceOwnershipUnconfirmed, "registry project requires explicit ownership recovery")
		}
		// A previous sent request may have created the project even when the
		// current lookup says not found. Only a durable rejection receipt (or
		// a request never sent) permits another POST under this same command.
		if c.Phase != "reserved" && c.Phase != "project_rejected" {
			return l.unconfirmedProject(ctx, s, c)
		}
		_, err := l.registry.FindProjectByName(ctx, s.ProjectName)
		if err == nil {
			if c.Phase == "reserved" || c.Phase == "project_rejected" {
				if err = l.phase(ctx, c, "project_sent"); err != nil {
					return err
				}
			}
			return l.unconfirmedProject(ctx, s, c)
		}
		if ReasonOf(err) != ImageNotFound {
			return err
		}
		if err = l.phase(ctx, c, "project_sent"); err != nil {
			return err
		}
		project, err := l.registry.CreatePrivateProject(ctx, s.ProjectName)
		if err != nil {
			if isProjectCreationRejected(err) {
				c.Phase, c.State, c.Reason = "project_rejected", "retryable", ReasonOf(err)
				saved, saveErr := l.saveCommandPhase(ctx, *c)
				if saveErr != nil {
					return saveErr
				}
				*c = saved
				return err
			}
			return l.unconfirmedProject(ctx, s, c)
		}
		// Persist the returned ID before the next provider read. A lost commit is
		// resolved by a fresh command/space read on the next same-key request.
		bound, err := l.bindProject(ctx, *s, project.ID)
		if err != nil {
			return err
		}
		*s = bound
	}
	project, err := l.registry.GetProjectByID(ctx, s.ProjectID)
	if err != nil {
		return err
	}
	if project.ID != s.ProjectID || project.Name != s.ProjectName || !project.Private {
		return Fail(SpaceNotReady, "tenant registry project mismatch")
	}
	if c.Phase == "project_sent" || c.Phase == "reserved" {
		return l.phase(ctx, c, "project_bound")
	}
	return nil
}
func (l *Lifecycle) GetPublisherCredential(ctx context.Context, tenant string) (CredentialInfo, error) {
	if _, err := RequireTenant(ctx, tenant); err != nil {
		return CredentialInfo{}, err
	}
	s, err := l.repo.FindTenantSpace(ctx, tenant)
	if err != nil {
		return CredentialInfo{}, err
	}
	if err = l.checkSpace(s); err != nil {
		return CredentialInfo{}, err
	}
	return l.repo.GetTenantPublisher(ctx, tenant, s.ID)
}
func (l *Lifecycle) IssuePublisherCredential(ctx context.Context, in IssueCredential) (CredentialDelivery, error) {
	return l.changePublisher(ctx, in, "issue_publisher")
}
func (l *Lifecycle) ResetPublisherCredential(ctx context.Context, in ResetCredential) (CredentialDelivery, error) {
	return l.changePublisher(ctx, in, "reset_publisher")
}
func (l *Lifecycle) changePublisher(ctx context.Context, in IssueCredential, kind string) (CredentialDelivery, error) {
	caller, err := RequireTenant(ctx, in.TenantID)
	if err != nil {
		return CredentialDelivery{}, err
	}
	if in.ExpectedVersion < 0 {
		return CredentialDelivery{}, Fail(InvalidArgument, "invalid expected version")
	}
	s, err := l.repo.FindTenantSpace(ctx, in.TenantID)
	if err != nil {
		return CredentialDelivery{}, err
	}
	if err = l.checkSpace(s); err != nil {
		return CredentialDelivery{}, err
	}
	c, err := requestCommand(caller, s.ID, kind, in.IdempotencyKey, in)
	if err != nil {
		return CredentialDelivery{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	var delivery CredentialDelivery
	err = l.repo.WithSpaceWriteLock(ctx, s.ID, func(ctx context.Context) error {
		var e error
		c, e = l.repo.BeginTenantCredentialCommand(ctx, c, in.ExpectedVersion)
		if e != nil {
			return e
		}
		if c.State == "succeeded" {
			delivery, e = l.replayDelivery(ctx, s, c)
			return e
		}
		if e = recoveredCandidate(c); e != nil {
			return e
		}
		if _, e = l.platform(ctx); e != nil {
			return e
		}
		p, e := l.registry.GetProjectByID(ctx, s.ProjectID)
		if e != nil {
			return e
		}
		if p.ID != s.ProjectID || p.Name != s.ProjectName || !p.Private {
			return Fail(SpaceNotReady, "tenant registry project mismatch")
		}
		info, e := l.repo.GetTenantPublisher(ctx, in.TenantID, s.ID)
		if e != nil {
			return e
		}
		s, c, e = l.runCandidate(ctx, s, c, info)
		if e != nil {
			return e
		}
		delivery, e = l.replayDelivery(ctx, s, c)
		return e
	})
	return delivery, err
}
func (l *Lifecycle) credentialActive(c CredentialInfo) bool {
	return c.State == "active" && c.Generation > 0 && (c.ExpiresAt == nil || c.ExpiresAt.After(l.now()))
}

// recoveredCandidate rejects a resume of a command whose expired candidate was
// already terminated by the local operator recovery. The same idempotency key
// may never create a second identity after that decision.
func recoveredCandidate(c Command) error {
	if c.State == "failed" && c.Reason == CandidateExpiredRecovered {
		return Fail(CandidateRecoveryCompleted, "expired candidate already recovered")
	}
	return nil
}
func (l *Lifecycle) candidateAAD(s Space, c Command) SecretAAD {
	return SecretAAD{InstallationID: s.InstallationID, TenantID: s.TenantID, SpaceID: s.ID, Scope: s.Scope, Purpose: c.Candidate.Purpose, Generation: c.Candidate.Generation, CommandID: c.ID}
}
func (l *Lifecycle) prepareCandidate(ctx context.Context, s Space, c Command, previous CredentialInfo) (Command, error) {
	// A live pull credential backs running workloads, so replacing it requires
	// the coordinated path (new material bound first, then retire the old
	// identity). An expired one backs nothing: no token minted from it can be
	// honored, so minting its successor in place is the only repair available.
	if previous.Generation > 0 && previous.Purpose == "pull" && l.credentialActive(previous) {
		return Command{}, Fail(SpaceNotReady, "running credentials require coordinated replacement")
	}
	generation := previous.Generation + 1
	if generation <= 0 {
		return Command{}, Fail(InternalError, "credential generation exhausted")
	}
	ownership := fmt.Sprintf("ani-image:v1:%s:%s:%s:%d:%s", s.InstallationID, s.ID, previous.Purpose, generation, c.ID)
	digest := sha256.Sum256([]byte(ownership))
	c.Candidate = Candidate{RecoveryEvidence: c.Candidate.RecoveryEvidence, Generation: generation, CredentialVersion: previous.Version, PreviousRobotID: previous.RobotID, Purpose: previous.Purpose, RobotName: "i-" + hex.EncodeToString(digest[:20]), Ownership: ownership}
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return Command{}, Fail(InternalError, "credential entropy unavailable")
	}
	plain := Secret("Aa1" + hex.EncodeToString(random))
	clear(random)
	defer clear(plain)
	encrypted, err := l.cipher.Seal(ctx, l.candidateAAD(s, c), plain)
	if err != nil {
		return Command{}, err
	}
	c.DeliverySecret = encrypted
	c.Phase = "candidate_prepared"
	c.State = "running"
	return l.prepareStoredCandidate(ctx, c)
}
func (l *Lifecycle) candidateRequest(s Space, c Command) (RobotRequest, error) {
	perms, err := RobotPermissions(s.ProjectName, l.cfg.PlatformProject, c.Candidate.Purpose, s.Scope)
	days := l.cfg.PublisherDays
	if c.Candidate.Purpose == "pull" {
		days = l.cfg.PullDays
	}
	return RobotRequest{Name: logicalRobotName(c.Candidate.Ownership), Description: c.Candidate.Ownership, DurationDays: days, Permissions: perms}, err
}
func logicalRobotName(ownership string) string {
	digest := sha256.Sum256([]byte(ownership))
	return "i-" + hex.EncodeToString(digest[:20])
}
func (l *Lifecycle) verifyCandidate(r Robot, c Command, want RobotRequest) error {
	if err := l.verifyCandidateIdentity(r, c, want); err != nil {
		return err
	}
	if r.Disabled {
		return Fail(PermissionDenied, "candidate robot identity mismatch")
	}
	return nil
}

// verifyCandidateIdentity 只校验候选身份、描述、过期时长与权限，不关心是否已
// 启用。过期候选恢复需要它：上一次恢复可能已停用 robot 但事务随后回滚，重试时
// 仍要能认出这就是同一条候选，否则这条命令永远无法终结。
func (l *Lifecycle) verifyCandidateIdentity(r Robot, c Command, want RobotRequest) error {
	if r.ID != c.Candidate.RobotID || r.Name != c.Candidate.RobotName || r.Username != c.Candidate.Username || r.Description != want.Description || r.DurationDays != want.DurationDays {
		return Fail(PermissionDenied, "candidate robot identity mismatch")
	}
	return ValidateRobotPermissions(r.Permissions, want.Permissions)
}
func (l *Lifecycle) currentCandidate(ctx context.Context, s Space, c Command) error {
	latest, err := l.findCommand(ctx, s, c.Key)
	if err != nil {
		return err
	}
	if latest.ID != c.ID || latest.Version != c.Version || latest.State == "succeeded" || latest.State == "failed" {
		return Fail(VersionConflict, "command changed before provider write")
	}
	var info CredentialInfo
	if c.Candidate.Purpose == "publisher" {
		info, err = l.publisher(ctx, s)
	} else {
		var stored StoredCredential
		stored, err = l.repo.GetTenantPull(ctx, s.TenantID, s.ID)
		info = stored.Info
	}
	if err != nil {
		return err
	}
	if info.Version != c.Candidate.CredentialVersion || info.Generation >= c.Candidate.Generation || info.RobotID != c.Candidate.PreviousRobotID {
		return Fail(VersionConflict, "credential generation changed")
	}
	return nil
}
func (l *Lifecycle) runCandidate(ctx context.Context, s Space, c Command, previous CredentialInfo) (Space, Command, error) {
	var err error
	if c.Candidate.Generation == 0 {
		c, err = l.prepareCandidate(ctx, s, c, previous)
		if err != nil {
			return s, c, err
		}
	}
	want, err := l.candidateRequest(s, c)
	if err != nil {
		return s, c, err
	}
	if err = l.currentCandidate(ctx, s, c); err != nil {
		return s, c, err
	}
	if c.Phase == "candidate_prepared" {
		robot, e := l.registry.FindOwnedRobot(ctx, want)
		if ReasonOf(e) == ImageNotFound {
			robot, e = l.registry.CreateRobot(ctx, want)
		}
		if e != nil {
			return s, c, e
		}
		c.Candidate.RobotID = robot.ID
		c.Candidate.RobotName = robot.Name
		c.Candidate.Username = robot.Username
		if err = l.phase(ctx, &c, "robot_created"); err != nil {
			return s, c, err
		}
	}
	robot, err := l.registry.GetRobot(ctx, c.Candidate.RobotID)
	if err != nil {
		return s, c, err
	}
	if err = l.verifyCandidate(robot, c, want); err != nil {
		return s, c, err
	}
	if c.Phase == "robot_created" {
		plain, e := l.cipher.Open(ctx, l.candidateAAD(s, c), c.DeliverySecret)
		if e != nil {
			return s, c, e
		}
		e = l.registry.SetRobotSecret(ctx, robot, plain)
		clear(plain)
		if e != nil {
			return s, c, e
		}
		if err = l.phase(ctx, &c, "secret_set"); err != nil {
			return s, c, err
		}
	}
	if c.Phase == "secret_set" {
		if err = l.currentCandidate(ctx, s, c); err != nil {
			return s, c, err
		}
		if c.Candidate.PreviousRobotID > 0 {
			old, e := l.registry.GetRobot(ctx, c.Candidate.PreviousRobotID)
			if e != nil {
				return s, c, e
			}
			if e = l.verifyPrevious(old, s, previous, want.Permissions); e != nil {
				return s, c, e
			}
			if e = l.registry.SetRobotDisabled(ctx, old, true); e != nil {
				return s, c, e
			}
		}
		if err = l.phase(ctx, &c, "previous_disabled"); err != nil {
			return s, c, err
		}
	}
	if c.Phase != "previous_disabled" {
		return s, c, Fail(InternalError, "unsupported credential recovery phase")
	}
	robot, err = l.registry.GetRobot(ctx, c.Candidate.RobotID)
	if err != nil {
		return s, c, err
	}
	if err = l.verifyCandidate(robot, c, want); err != nil {
		return s, c, err
	}
	if robot.ExpiresAt == -1 {
		if !l.cfg.AllowNeverExpires || want.DurationDays != -1 {
			return s, c, Fail(PermissionDenied, "unexpected unlimited credential")
		}
		c.Candidate.ExpiresAt = nil
	} else {
		expiry := time.Unix(robot.ExpiresAt, 0).UTC()
		if !expiry.After(l.now()) {
			return s, c, Fail(SpaceNotReady, "candidate credential expired")
		}
		c.Candidate.ExpiresAt = &expiry
	}
	var pull EncryptedSecret
	if c.Candidate.Purpose == "pull" {
		plain, e := l.cipher.Open(ctx, l.candidateAAD(s, c), c.DeliverySecret)
		if e != nil {
			return s, c, e
		}
		aad := l.candidateAAD(s, c)
		aad.CommandID = ""
		pull, e = l.cipher.Seal(ctx, aad, plain)
		clear(plain)
		if e != nil {
			return s, c, e
		}
		c.ReplayUntil = nil
	} else {
		until := l.now().Add(10 * time.Minute)
		c.ReplayUntil = &until
	}
	return l.activateCandidate(ctx, s, c, pull)
}
func (l *Lifecycle) verifyPrevious(r Robot, s Space, previous CredentialInfo, permissions []RobotPermission) error {
	prefix := fmt.Sprintf("ani-image:v1:%s:%s:%s:%d:", s.InstallationID, s.ID, previous.Purpose, previous.Generation)
	if r.ID != previous.RobotID || r.Name != previous.RobotName || r.Username != previous.Username || !strings.HasPrefix(r.Description, prefix) {
		return Fail(PermissionDenied, "previous robot ownership unconfirmed")
	}
	if _, err := ParseTenant(strings.TrimPrefix(r.Description, prefix)); err != nil {
		return Fail(PermissionDenied, "previous robot command unconfirmed")
	}
	return ValidateRobotPermissions(r.Permissions, permissions)
}
func (l *Lifecycle) replayDelivery(ctx context.Context, s Space, c Command) (CredentialDelivery, error) {
	if c.State != "succeeded" || c.Result.Credential == nil || c.ReplayUntil == nil || !l.now().Before(*c.ReplayUntil) || len(c.DeliverySecret.Ciphertext) == 0 {
		return CredentialDelivery{}, Fail(CredentialDeliveryExpired, "credential delivery window expired; reset required")
	}
	info, err := l.publisher(ctx, s)
	if err != nil {
		return CredentialDelivery{}, err
	}
	if !l.credentialActive(info) || info.Generation != c.Candidate.Generation || info.RobotID != c.Candidate.RobotID || info.Version != c.Result.Credential.Version {
		return CredentialDelivery{}, Fail(CredentialDeliveryExpired, "credential generation no longer current")
	}
	plain, err := l.cipher.Open(ctx, l.candidateAAD(s, c), c.DeliverySecret)
	if err != nil {
		return CredentialDelivery{}, err
	}
	return CredentialDelivery{Credential: info, Secret: plain, ReplayUntil: *c.ReplayUntil}, nil
}
func (l *Lifecycle) DisablePublisherCredential(ctx context.Context, in DisableCredential) (CredentialInfo, error) {
	caller, err := RequireTenant(ctx, in.TenantID)
	if err != nil {
		return CredentialInfo{}, err
	}
	if in.ExpectedVersion < 0 {
		return CredentialInfo{}, Fail(InvalidArgument, "invalid expected version")
	}
	s, err := l.repo.FindTenantSpace(ctx, in.TenantID)
	if err != nil {
		return CredentialInfo{}, err
	}
	if err = l.checkSpace(s); err != nil {
		return CredentialInfo{}, err
	}
	c, err := requestCommand(caller, s.ID, "disable_publisher", in.IdempotencyKey, in)
	if err != nil {
		return CredentialInfo{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	var result CredentialInfo
	err = l.repo.WithSpaceWriteLock(ctx, s.ID, func(ctx context.Context) error {
		var e error
		c, e = l.beginPublisherDisable(ctx, c, in.ExpectedVersion)
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
		result, e = l.runPublisherDisable(ctx, s, c)
		return e
	})
	return result, err
}

// beginPublisherDisable reserves the disable command through the port that
// matches the stored space scope. The platform surface supplies its own
// repository, so a tenant Lifecycle never touches platform rows.
func (l *Lifecycle) beginPublisherDisable(ctx context.Context, c Command, expected int64) (Command, error) {
	if c.Scope == TenantImages {
		return l.repo.BeginTenantCredentialCommand(ctx, c, expected)
	}
	if err := l.platformWrite(ctx, c.Scope, c.TenantID); err != nil {
		return Command{}, err
	}
	return l.platformRepo.BeginPlatformCredentialCommand(ctx, c, expected)
}

// runPublisherDisable disables the current publisher robot and completes the
// command. It is shared by the tenant and platform entry points and dispatches
// every read and write through the stored space scope.
func (l *Lifecycle) runPublisherDisable(ctx context.Context, s Space, c Command) (CredentialInfo, error) {
	info, err := l.publisher(ctx, s)
	if err != nil {
		return CredentialInfo{}, err
	}
	if c.Phase == "reserved" {
		c.Candidate = Candidate{CredentialVersion: info.Version, Generation: info.Generation, RobotID: info.RobotID, Purpose: "publisher"}
		if err = l.phase(ctx, &c, "disable_prepared"); err != nil {
			return CredentialInfo{}, err
		}
	}
	if info.Version != c.Candidate.CredentialVersion || info.Generation != c.Candidate.Generation || info.RobotID != c.Candidate.RobotID {
		return CredentialInfo{}, Fail(VersionConflict, "credential changed")
	}
	// The permission set is derived from the stored scope, never from the
	// request, so a platform space can only ever verify platform permissions.
	perms, err := RobotPermissions(s.ProjectName, l.cfg.PlatformProject, "publisher", s.Scope)
	if err != nil {
		return CredentialInfo{}, err
	}
	if info.State == "active" {
		robot, e := l.registry.GetRobot(ctx, info.RobotID)
		if e != nil {
			return CredentialInfo{}, e
		}
		if e = l.verifyPrevious(robot, s, info, perms); e != nil {
			return CredentialInfo{}, e
		}
		if e = l.registry.SetRobotDisabled(ctx, robot, true); e != nil {
			return CredentialInfo{}, e
		}
	}
	c, err = l.completePublisherDisable(ctx, s, c)
	if err != nil {
		return CredentialInfo{}, err
	}
	if c.Result.Credential == nil {
		return CredentialInfo{}, Fail(InternalError, "missing disable result")
	}
	return *c.Result.Credential, nil
}

// completePublisherDisable finishes the disable command in the scope's own
// short transaction.
func (l *Lifecycle) completePublisherDisable(ctx context.Context, s Space, c Command) (Command, error) {
	if s.Scope == TenantImages {
		return l.repo.CompleteTenantDisable(ctx, c)
	}
	if err := l.platformWrite(ctx, s.Scope, s.TenantID); err != nil {
		return Command{}, err
	}
	return l.platformRepo.CompletePlatformDisable(ctx, c)
}

// Recovery is a local operator action with an externally reviewed evidence hash.
// It never infers ownership from a matching name on a tenant request.
func (l *Lifecycle) RecoverProjectBinding(ctx context.Context, space string, projectID int64, evidenceHash string) (Space, error) {
	if _, err := RequirePlatform(ctx); err != nil {
		return Space{}, err
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(evidenceHash) || projectID <= 0 {
		return Space{}, Fail(InvalidArgument, "explicit recovery ID and evidence hash required")
	}
	s, err := l.repo.InspectSpaceForOperator(ctx, space)
	if err != nil {
		return Space{}, err
	}
	if err = l.checkSpace(s); err != nil {
		return Space{}, err
	}
	err = l.repo.WithSpaceWriteLock(ctx, s.ID, func(ctx context.Context) error {
		var e error
		s, e = l.repo.InspectSpaceForOperator(ctx, space)
		if e != nil {
			return e
		}
		c, e := l.repo.OpenTenantExternalCommand(ctx, s.TenantID, s.ID)
		if e != nil {
			return e
		}
		p, e := l.registry.GetProjectByID(ctx, projectID)
		if e != nil {
			return e
		}
		if p.ID != projectID || p.Name != s.ProjectName || !p.Private {
			return Fail(PermissionDenied, "recovery project mismatch")
		}
		s, e = l.repo.RecoverTenantProject(ctx, s, c, projectID, evidenceHash)
		return e
	})
	return s, err
}
func (l *Lifecycle) InspectSpace(ctx context.Context, space string) (Space, error) {
	if _, err := RequirePlatform(ctx); err != nil {
		return Space{}, err
	}
	return l.repo.InspectSpaceForOperator(ctx, space)
}
func (l *Lifecycle) PurgeExpiredDeliverySecrets(ctx context.Context) (int64, error) {
	if _, err := RequirePlatform(ctx); err != nil {
		return 0, err
	}
	return l.repo.PurgeExpiredDeliverySecrets(ctx, l.now())
}
