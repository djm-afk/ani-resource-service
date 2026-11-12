package biz

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

const (
	recoveryInstallationID = "22222222-2222-4222-8222-222222222222"
	recoveryTenantID       = "33333333-3333-4333-8333-333333333333"
	recoverySpaceID        = "44444444-4444-4444-8444-444444444444"
	recoveryCommandID      = "55555555-5555-4555-8555-555555555555"
	recoveryProjectName    = "t-recovery"
	recoveryPlatformName   = "ani-platform"
	recoveryAuthority      = "registry.example:5000"
	recoveryOwnership      = recoveryAuthority + "/" + recoveryProjectName + ":publisher"
)

// recoveryCipher 只是满足 NewLifecycle 的依赖检查，恢复链路不会触碰密文。
type recoveryCipher struct{ SecretCipher }

// recoveryRegistry 只提供恢复链路读取 robot 的能力；其它方法保持 nil，
// 一旦恢复链路越界调用就会立刻 panic。
type recoveryRegistry struct {
	Registry
	robots map[int64]Robot
}

func (r *recoveryRegistry) GetRobot(_ context.Context, id int64) (Robot, error) {
	robot, ok := r.robots[id]
	if !ok {
		return Robot{}, Fail(ImageNotFound, "robot not found")
	}
	return robot, nil
}

// recoveryRepository 记录终结时提交的恢复证据，便于断言处置结果。
type recoveryRepository struct {
	LifecycleRepository
	space      Space
	command    Command
	credential CredentialInfo
	record     *CandidateExpiryRecoveryRecord
}

func (r *recoveryRepository) InspectSpaceForOperator(_ context.Context, id string) (Space, error) {
	if id != r.space.ID {
		return Space{}, Fail(ImageNotFound, "image space not found")
	}
	return r.space, nil
}
func (r *recoveryRepository) WithSpaceWriteLock(ctx context.Context, _ string, run func(context.Context) error) error {
	return run(ctx)
}
func (r *recoveryRepository) GetTenantCandidateRecovery(_ context.Context, tenant, space, command string) (Command, CredentialInfo, error) {
	if tenant != r.space.TenantID || space != r.space.ID || command != r.command.ID {
		return Command{}, CredentialInfo{}, Fail(ImageNotFound, "image command not found")
	}
	return r.command, r.credential, nil
}
func (r *recoveryRepository) FailExpiredTenantCandidate(_ context.Context, c Command, record CandidateExpiryRecoveryRecord) (Command, CredentialInfo, error) {
	r.record = &record
	c.State = "failed"
	c.Reason = CandidateExpiredRecovered
	c.Candidate.ExpiryRecovery = &record
	c.Version++
	return c, r.credential, nil
}

func newRecoveryLifecycle(t *testing.T, now time.Time, robots map[int64]Robot, repo *recoveryRepository) *Lifecycle {
	t.Helper()
	l, err := NewLifecycle(repo, &recoveryRegistry{robots: robots}, &recoveryCipher{}, LifecycleConfig{
		InstallationID:    recoveryInstallationID,
		RegistryAuthority: recoveryAuthority,
		PlatformProject:   recoveryPlatformName,
		PublisherDays:     7,
		PullDays:          7,
	}, func() time.Time { return now })
	if err != nil {
		t.Fatalf("NewLifecycle: %v", err)
	}
	return l
}

func recoveryTestSpace() Space {
	return Space{ID: recoverySpaceID, TenantID: recoveryTenantID, InstallationID: recoveryInstallationID, ProjectName: recoveryProjectName, RegistryAuthority: recoveryAuthority, Scope: TenantImages, State: "available", ProjectID: 7, Version: 1}
}

func recoveryTestPermissions(t *testing.T, space Space) []RobotPermission {
	t.Helper()
	perms, err := RobotPermissions(space.ProjectName, recoveryPlatformName, "publisher", TenantImages)
	if err != nil {
		t.Fatalf("RobotPermissions: %v", err)
	}
	return perms
}

// recoveryTestCommand 复现“签发候选已失败但命令仍 pending”的现场：
// 候选 generation 比已生效凭据大 1，等待本地操作人来终结。
func recoveryTestCommand(space Space) Command {
	return Command{ID: recoveryCommandID, SpaceID: space.ID, TenantID: space.TenantID, Scope: TenantImages, Kind: "issue_publisher", Phase: "secret_set", Version: 3, Candidate: Candidate{RobotID: 7, Generation: 2, PreviousRobotID: 5, CredentialVersion: 4, Purpose: "publisher", RobotName: "candidate", Username: "robot$candidate", Ownership: recoveryOwnership}}
}

func recoveryTestCredential() CredentialInfo {
	return CredentialInfo{Generation: 1, Version: 4, RobotID: 5, RobotName: "previous", Username: "robot$previous", Purpose: "publisher", Scope: TenantImages, State: "active"}
}

// recoveryTestPreviousRobot 构造一个仍可用的旧机器人：身份、描述前缀与权限都吻合。
func recoveryTestPreviousRobot(t *testing.T, space Space) Robot {
	t.Helper()
	return Robot{ID: 5, Name: "previous", Username: "robot$previous", DurationDays: 7, Permissions: recoveryTestPermissions(t, space), Description: fmt.Sprintf("ani-image:v1:%s:%s:publisher:20261001", space.InstallationID, space.ID)}
}

// recoveryTestCandidateRobot 构造本命令创建的候选机器人。
func recoveryTestCandidateRobot(t *testing.T, space Space) Robot {
	t.Helper()
	return Robot{ID: 7, Name: "candidate", Username: "robot$candidate", DurationDays: 7, Permissions: recoveryTestPermissions(t, space), Description: recoveryOwnership, Disabled: true}
}

// TestCredentialDispositionKeepsUsablePreviousIdentity 覆盖 P1-2：
// ExpiresAt=-1 表示永久凭据，不能因为“没有过期时间”被判定为失效。
func TestCredentialDispositionKeepsUsablePreviousIdentity(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	space := recoveryTestSpace()
	command := recoveryTestCommand(space)
	credential := recoveryTestCredential()
	cases := []struct {
		name       string
		expiresAt  int64
		disabled   bool
		generation int64
		want       string
	}{
		{name: "永久凭据视为可用", expiresAt: -1, generation: 1, want: "unchanged"},
		{name: "未到期凭据视为可用", expiresAt: now.Add(time.Hour).Unix(), generation: 1, want: "unchanged"},
		{name: "已过期凭据判定失效", expiresAt: now.Add(-time.Hour).Unix(), generation: 1, want: "disabled"},
		{name: "缺少过期时间判定失效", expiresAt: 0, generation: 1, want: "disabled"},
		{name: "已停用凭据不再改动", expiresAt: now.Add(time.Hour).Unix(), disabled: true, generation: 1, want: "disabled"},
		{name: "未曾签发过凭据", expiresAt: -1, generation: 0, want: "blocked_empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			previous := recoveryTestPreviousRobot(t, space)
			previous.ExpiresAt = tc.expiresAt
			previous.Disabled = tc.disabled
			credential.Generation = tc.generation
			l := newRecoveryLifecycle(t, now, map[int64]Robot{previous.ID: previous}, &recoveryRepository{})
			got, err := l.credentialDisposition(context.Background(), space, command, credential)
			if err != nil {
				t.Fatalf("credentialDisposition: %v", err)
			}
			if got != tc.want {
				t.Fatalf("disposition = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRecoveryAcceptsCandidateDisabledByEarlierAttempt 覆盖 P1-1 的业务层一半：
// 上一次恢复已把候选停用但事务回滚，重试时仍要认出同一条候选并终结命令。
func TestRecoveryAcceptsCandidateDisabledByEarlierAttempt(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	space := recoveryTestSpace()
	previous := recoveryTestPreviousRobot(t, space)
	previous.ExpiresAt = now.Add(time.Hour).Unix()
	candidate := recoveryTestCandidateRobot(t, space)
	candidate.ExpiresAt = now.Add(-time.Hour).Unix()
	repo := &recoveryRepository{space: space, command: recoveryTestCommand(space), credential: recoveryTestCredential()}
	l := newRecoveryLifecycle(t, now, map[int64]Robot{previous.ID: previous, candidate.ID: candidate}, repo)

	result, err := l.TerminateExpiredCandidate(recoveryOperatorContext(), recoveryTestInput(space, repo.command, repo.credential))
	if err != nil {
		t.Fatalf("TerminateExpiredCandidate: %v", err)
	}
	if result.State != "failed" || result.Reason != CandidateExpiredRecovered {
		t.Fatalf("result = %s/%s, want failed/%s", result.State, result.Reason, CandidateExpiredRecovered)
	}
	if repo.record == nil {
		t.Fatal("recovery evidence not persisted")
	}
	if repo.record.PreviousCredentialDisposition != "unchanged" {
		t.Fatalf("previous disposition = %q, want unchanged", repo.record.PreviousCredentialDisposition)
	}
	if repo.record.CandidateRobotID != candidate.ID || repo.record.PreviousRobotID != previous.ID {
		t.Fatalf("record robots = %d/%d, want %d/%d", repo.record.CandidateRobotID, repo.record.PreviousRobotID, candidate.ID, previous.ID)
	}
	if repo.record.Operator != "operator:test" || repo.record.EvidenceSHA256 != strings.Repeat("a", 64) {
		t.Fatalf("record evidence = %q/%q", repo.record.Operator, repo.record.EvidenceSHA256)
	}
}

// TestRecoveryRefusesCandidateThatIsNotExpired 保证恢复只处理真的过期候选，
// 未过期时不得终结命令。
func TestRecoveryRefusesCandidateThatIsNotExpired(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	space := recoveryTestSpace()
	previous := recoveryTestPreviousRobot(t, space)
	previous.ExpiresAt = now.Add(time.Hour).Unix()
	candidate := recoveryTestCandidateRobot(t, space)
	candidate.ExpiresAt = now.Add(time.Hour).Unix()
	repo := &recoveryRepository{space: space, command: recoveryTestCommand(space), credential: recoveryTestCredential()}
	l := newRecoveryLifecycle(t, now, map[int64]Robot{previous.ID: previous, candidate.ID: candidate}, repo)

	if _, err := l.TerminateExpiredCandidate(recoveryOperatorContext(), recoveryTestInput(space, repo.command, repo.credential)); ReasonOf(err) != SpaceNotReady {
		t.Fatalf("reason = %v, want %v", ReasonOf(err), SpaceNotReady)
	}
	if repo.record != nil {
		t.Fatal("recovery evidence must not be written for a live candidate")
	}
}

// TestVerifyCandidateRejectsDisabledRobotOutsideRecovery 固定两条校验的差异：
// 常规签发仍要求候选处于启用状态，只有恢复链路接受已停用的候选。
func TestVerifyCandidateRejectsDisabledRobotOutsideRecovery(t *testing.T) {
	space := recoveryTestSpace()
	command := recoveryTestCommand(space)
	candidate := recoveryTestCandidateRobot(t, space)
	want := RobotRequest{Name: "candidate", Description: recoveryOwnership, DurationDays: 7, Permissions: recoveryTestPermissions(t, space)}
	l := &Lifecycle{cfg: LifecycleConfig{PlatformProject: recoveryPlatformName}}

	if err := l.verifyCandidateIdentity(candidate, command, want); err != nil {
		t.Fatalf("verifyCandidateIdentity rejected a disabled candidate: %v", err)
	}
	if err := l.verifyCandidate(candidate, command, want); ReasonOf(err) != PermissionDenied {
		t.Fatalf("verifyCandidate reason = %v, want %v", ReasonOf(err), PermissionDenied)
	}

	mismatched := candidate
	mismatched.Description = recoveryAuthority + "/other:publisher"
	if err := l.verifyCandidateIdentity(mismatched, command, want); ReasonOf(err) != PermissionDenied {
		t.Fatalf("identity mismatch reason = %v, want %v", ReasonOf(err), PermissionDenied)
	}
}

func recoveryOperatorContext() context.Context {
	return WithCaller(context.Background(), Caller{Kind: PlatformCaller, Subject: "local-operator", Actor: "operator:test"})
}

func recoveryTestInput(space Space, command Command, credential CredentialInfo) ExpiredCandidateRecoveryInput {
	return ExpiredCandidateRecoveryInput{SpaceID: space.ID, CommandID: command.ID, ExpectedCommandVersion: command.Version, ExpectedCredentialVersion: credential.Version, CandidateRobotID: command.Candidate.RobotID, EvidenceSHA256: strings.Repeat("a", 64)}
}
