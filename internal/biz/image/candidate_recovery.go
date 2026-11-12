package biz

import (
	"context"
	"regexp"
	"strings"
	"time"
)

var evidencePattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// TerminateExpiredCandidate is the local operator recovery of one interrupted
// credential candidate. The real scope is read from the stored space; the input
// never declares one. It never creates or re-enables a credential identity.
func (l *Lifecycle) TerminateExpiredCandidate(ctx context.Context, in ExpiredCandidateRecoveryInput) (CandidateRecoveryResult, error) {
	if _, err := RequirePlatform(ctx); err != nil {
		return CandidateRecoveryResult{}, err
	}
	if _, err := ParseTenant(in.SpaceID); err != nil {
		return CandidateRecoveryResult{}, err
	}
	if _, err := ParseTenant(in.CommandID); err != nil {
		return CandidateRecoveryResult{}, err
	}
	if in.ExpectedCommandVersion < 1 || in.ExpectedCredentialVersion < 1 || in.CandidateRobotID <= 0 || !evidencePattern.MatchString(in.EvidenceSHA256) {
		return CandidateRecoveryResult{}, Fail(InvalidArgument, "explicit recovery evidence required")
	}
	s, err := l.repo.InspectSpaceForOperator(ctx, in.SpaceID)
	if err != nil {
		return CandidateRecoveryResult{}, err
	}
	if err = l.checkRecoverySpace(s); err != nil {
		return CandidateRecoveryResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	var result CandidateRecoveryResult
	err = l.repo.WithSpaceWriteLock(ctx, s.ID, func(ctx context.Context) error {
		var e error
		result, e = l.terminateExpiredCandidate(ctx, s, in)
		return e
	})
	return result, err
}

// checkRecoverySpace validates the stored space against the deployment binding
// for its own scope. It never infers the scope from the request.
func (l *Lifecycle) checkRecoverySpace(s Space) error {
	switch s.Scope {
	case TenantImages:
		if s.TenantID == "" {
			return Fail(ImageNotFound, "image space not found")
		}
		return l.checkSpace(s)
	case PlatformImages:
		if s.TenantID != "" || s.InstallationID != l.cfg.InstallationID || s.RegistryAuthority != l.cfg.RegistryAuthority || s.ProjectName != l.cfg.PlatformProject {
			return Fail(SpaceNotReady, "platform space configuration mismatch")
		}
		if l.platformRepo == nil {
			// A tenant-only Lifecycle must never be asked to recover a platform
			// space; the CLI reaches this path through the Platform wrapper.
			return Fail(InvalidArgument, "platform persistence required")
		}
		return nil
	}
	return Fail(ImageNotFound, "image space not found")
}

func (l *Lifecycle) recoverySnapshot(ctx context.Context, s Space, command string) (Command, CredentialInfo, error) {
	if s.Scope == TenantImages {
		return l.repo.GetTenantCandidateRecovery(ctx, s.TenantID, s.ID, command)
	}
	if err := l.platformWrite(ctx, s.Scope, s.TenantID); err != nil {
		return Command{}, CredentialInfo{}, err
	}
	return l.platformRepo.GetPlatformCandidateRecovery(ctx, s.ID, command)
}

func (l *Lifecycle) failExpiredCandidate(ctx context.Context, c Command, record CandidateExpiryRecoveryRecord) (Command, CredentialInfo, error) {
	if c.Scope == TenantImages {
		return l.repo.FailExpiredTenantCandidate(ctx, c, record)
	}
	if err := l.platformWrite(ctx, c.Scope, c.TenantID); err != nil {
		return Command{}, CredentialInfo{}, err
	}
	return l.platformRepo.FailExpiredPlatformCandidate(ctx, c, record)
}

func recoveryPhaseAllowed(phase string) bool {
	switch phase {
	case "robot_created", "secret_set", "previous_disabled":
		return true
	}
	return false
}

// terminateExpiredCandidate runs inside the space write lock.
func (l *Lifecycle) terminateExpiredCandidate(ctx context.Context, s Space, in ExpiredCandidateRecoveryInput) (CandidateRecoveryResult, error) {
	c, info, err := l.recoverySnapshot(ctx, s, in.CommandID)
	if err != nil {
		return CandidateRecoveryResult{}, err
	}
	if c.ID != in.CommandID || c.SpaceID != s.ID || c.Scope != s.Scope {
		return CandidateRecoveryResult{}, Fail(ImageNotFound, "image command not found")
	}
	// A replay of the very same recovery returns the stored outcome; any other
	// terminal command is refused so a successful issuance is never rewritten.
	if c.State == "failed" {
		if c.Reason == CandidateExpiredRecovered && c.Candidate.ExpiryRecovery != nil && recoveryInputMatches(*c.Candidate.ExpiryRecovery, in) {
			return recoveryResult(c), nil
		}
		return CandidateRecoveryResult{}, Fail(VersionConflict, "command already finished")
	}
	if c.State == "succeeded" {
		return CandidateRecoveryResult{}, Fail(VersionConflict, "command already finished")
	}
	if c.Version != in.ExpectedCommandVersion {
		return CandidateRecoveryResult{}, Fail(VersionConflict, "command version changed")
	}
	if err = validateRecoverySubject(s, c); err != nil {
		return CandidateRecoveryResult{}, err
	}
	if !recoveryPhaseAllowed(c.Phase) || c.Candidate.RobotID != in.CandidateRobotID || c.Candidate.Generation <= 0 || c.Candidate.RobotName == "" || c.Candidate.Username == "" || c.Candidate.Ownership == "" {
		return CandidateRecoveryResult{}, Fail(InvalidArgument, "candidate recovery not permitted")
	}
	if info.Version != in.ExpectedCredentialVersion {
		return CandidateRecoveryResult{}, Fail(VersionConflict, "credential version changed")
	}
	if info.Generation >= c.Candidate.Generation || info.RobotID != c.Candidate.PreviousRobotID {
		return CandidateRecoveryResult{}, Fail(VersionConflict, "credential generation changed")
	}
	// The candidate must be the identity this command created, and it must be
	// strictly expired. A missing robot or an unreadable expiry is never treated
	// as "gone" evidence.
	want, err := l.candidateRequest(s, c)
	if err != nil {
		return CandidateRecoveryResult{}, err
	}
	robot, err := l.registry.GetRobot(ctx, c.Candidate.RobotID)
	if err != nil {
		return CandidateRecoveryResult{}, err
	}
	// 恢复路径允许候选已被停用：上一次恢复可能已把 robot 停用，但事务随后
	// 失败回滚，此时若仍按“必须启用”校验，这条命令永远无法终结。
	if err = l.verifyCandidateIdentity(robot, c, want); err != nil {
		return CandidateRecoveryResult{}, err
	}
	if robot.ExpiresAt == -1 || robot.ExpiresAt == 0 || !time.Unix(robot.ExpiresAt, 0).UTC().Before(l.now()) {
		return CandidateRecoveryResult{}, Fail(SpaceNotReady, "candidate credential is not expired")
	}
	if !robot.Disabled {
		if err = l.registry.SetRobotDisabled(ctx, robot, true); err != nil {
			return CandidateRecoveryResult{}, err
		}
		confirmed, e := l.registry.GetRobot(ctx, c.Candidate.RobotID)
		if e != nil {
			return CandidateRecoveryResult{}, e
		}
		if !confirmed.Disabled || confirmed.ID != robot.ID || confirmed.Name != robot.Name || confirmed.Username != robot.Username {
			return CandidateRecoveryResult{}, Fail(PermissionDenied, "candidate robot disable unconfirmed")
		}
	}
	disposition, err := l.credentialDisposition(ctx, s, c, info)
	if err != nil {
		return CandidateRecoveryResult{}, err
	}
	record := CandidateExpiryRecoveryRecord{Operator: platformOperator(ctx), EvidenceSHA256: in.EvidenceSHA256, ExpectedCommandVersion: in.ExpectedCommandVersion, ExpectedCredentialVersion: in.ExpectedCredentialVersion, CandidateRobotID: in.CandidateRobotID, PreviousRobotID: c.Candidate.PreviousRobotID, PreviousCredentialDisposition: disposition, ObservedAt: l.now().UTC()}
	stored, _, err := l.failExpiredCandidate(ctx, c, record)
	if err != nil {
		return CandidateRecoveryResult{}, err
	}
	return recoveryResult(stored), nil
}

// validateRecoverySubject checks the allowed command kinds and phases.
func validateRecoverySubject(s Space, c Command) error {
	if c.Kind != "issue_publisher" && c.Kind != "reset_publisher" && c.Kind != "enable_space" {
		return Fail(InvalidArgument, "candidate recovery not permitted")
	}
	if c.Kind == "enable_space" {
		if s.Scope != TenantImages || c.Candidate.Purpose != "pull" {
			return Fail(InvalidArgument, "candidate recovery not permitted")
		}
		if s.ProjectID <= 0 {
			return Fail(SpaceNotReady, "tenant project must be bound before recovery")
		}
		return nil
	}
	if c.Candidate.Purpose != "publisher" {
		return Fail(InvalidArgument, "candidate recovery not permitted")
	}
	return nil
}

// credentialDisposition decides, from local facts only, what happens to the
// current credential row. A healthy previous identity is never modified.
func (l *Lifecycle) credentialDisposition(ctx context.Context, s Space, c Command, info CredentialInfo) (string, error) {
	if info.Generation == 0 {
		return "blocked_empty", nil
	}
	if c.Candidate.PreviousRobotID > 0 {
		previous, err := l.registry.GetRobot(ctx, c.Candidate.PreviousRobotID)
		if err != nil {
			return "", err
		}
		perms, err := RobotPermissions(s.ProjectName, l.cfg.PlatformProject, c.Candidate.Purpose, s.Scope)
		if err != nil {
			return "", err
		}
		if err = validatePreviousIdentity(previous, s, info, c.Candidate, perms); err != nil {
			return "", err
		}
		// -1 表示永不过期（allow_never_expires 或 pull 的 PullDays=-1）：这类旧
		// 凭据依然可用，不能因为“没设过期时间”就当成失效去停用。
		if !previous.Disabled && previous.ExpiresAt != 0 && (previous.ExpiresAt == -1 || time.Unix(previous.ExpiresAt, 0).UTC().After(l.now())) {
			// The previous publisher is still valid: leave it exactly as it is.
			return "unchanged", nil
		}
	}
	return "disabled", nil
}

// validatePreviousIdentity confirms that the stored previous robot is the one
// this space's credential row describes before any state is touched.
func validatePreviousIdentity(previous Robot, s Space, info CredentialInfo, candidate Candidate, perms []RobotPermission) error {
	if previous.ID != candidate.PreviousRobotID || previous.ID != info.RobotID || previous.Name != info.RobotName || previous.Username != info.Username {
		return Fail(PermissionDenied, "previous robot ownership unconfirmed")
	}
	prefix := "ani-image:v1:" + s.InstallationID + ":" + s.ID + ":" + candidate.Purpose + ":"
	if !strings.HasPrefix(previous.Description, prefix) {
		return Fail(PermissionDenied, "previous robot ownership unconfirmed")
	}
	return ValidateRobotPermissions(previous.Permissions, perms)
}

func recoveryInputMatches(record CandidateExpiryRecoveryRecord, in ExpiredCandidateRecoveryInput) bool {
	return record.EvidenceSHA256 == in.EvidenceSHA256 && record.ExpectedCommandVersion == in.ExpectedCommandVersion && record.ExpectedCredentialVersion == in.ExpectedCredentialVersion && record.CandidateRobotID == in.CandidateRobotID
}

func recoveryResult(c Command) CandidateRecoveryResult {
	out := CandidateRecoveryResult{CommandID: c.ID, SpaceID: c.SpaceID, Scope: c.Scope, State: c.State, Phase: c.Phase, Reason: c.Reason, CommandVersion: c.Version}
	if c.Candidate.ExpiryRecovery != nil {
		out.Recovery = *c.Candidate.ExpiryRecovery
	}
	return out
}

func platformOperator(ctx context.Context) string {
	caller, err := CallerFromContext(ctx)
	if err != nil {
		return ""
	}
	return caller.Actor
}

// Platform wraps the same recovery for the platform scope. The CLI must use
// this entry point so the engine always holds the platform repository.
func (p *Platform) TerminateExpiredCandidate(ctx context.Context, in ExpiredCandidateRecoveryInput) (CandidateRecoveryResult, error) {
	if _, err := RequirePlatform(ctx); err != nil {
		return CandidateRecoveryResult{}, err
	}
	return p.engine.TerminateExpiredCandidate(ctx, in)
}
