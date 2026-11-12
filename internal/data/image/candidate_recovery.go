package data

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/image"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/image/sqlcgen"
)

// timePtr is a small helper for the sqlc nullable timestamptz parameters.
func timePtr(t time.Time) *time.Time { return &t }

// GetTenantCandidateRecovery reads the durable recovery snapshot of one tenant
// command together with the credential row of its candidate purpose.
func (p *Postgres) GetTenantCandidateRecovery(ctx context.Context, tenant, space, command string) (biz.Command, biz.CredentialInfo, error) {
	if _, err := biz.ParseTenant(tenant); err != nil {
		return biz.Command{}, biz.CredentialInfo{}, err
	}
	q := sqlcgen.New(p.connection(ctx))
	row, err := q.GetTenantCandidateRecoveryCommand(ctx, sqlcgen.GetTenantCandidateRecoveryCommandParams{TenantID: &tenant, SpaceID: space, CommandID: command})
	if err != nil {
		return biz.Command{}, biz.CredentialInfo{}, databaseError(err)
	}
	stored, err := fromCommand(row)
	if err != nil {
		return biz.Command{}, biz.CredentialInfo{}, err
	}
	info, err := tenantCredential(ctx, q, tenant, space, stored.Candidate.Purpose)
	if err != nil {
		return biz.Command{}, biz.CredentialInfo{}, err
	}
	return stored, info, nil
}

// FailExpiredTenantCandidate terminates one tenant candidate command and applies
// the credential disposition in the same short transaction.
func (p *Postgres) FailExpiredTenantCandidate(ctx context.Context, c biz.Command, record biz.CandidateExpiryRecoveryRecord) (biz.Command, biz.CredentialInfo, error) {
	if _, err := biz.ParseTenant(c.TenantID); err != nil {
		return biz.Command{}, biz.CredentialInfo{}, err
	}
	var stored biz.Command
	var info biz.CredentialInfo
	tx, err := p.connection(ctx).Begin(ctx)
	if err != nil {
		return biz.Command{}, biz.CredentialInfo{}, databaseError(err)
	}
	defer tx.Rollback(ctx)
	q := sqlcgen.New(tx)
	if err = q.LockTenantSpace(ctx, sqlcgen.LockTenantSpaceParams{TenantID: c.TenantID}); err != nil {
		return biz.Command{}, biz.CredentialInfo{}, databaseError(err)
	}
	info, err = tenantCredential(ctx, q, c.TenantID, c.SpaceID, c.Candidate.Purpose)
	if err != nil {
		return biz.Command{}, biz.CredentialInfo{}, err
	}
	// 事务内校验的是“候选之前的旧凭据行”，与业务层 recoverySnapshot 读到的
	// 是同一行。候选自身从未写进 credentials 表（只有 phase 推进），因此这里
	// 不能要求 info 等于候选代次，否则 CAS 恒失败、命令永远无法终结。
	if info.Version != record.ExpectedCredentialVersion || info.Generation != c.Candidate.Generation-1 || info.RobotID != record.PreviousRobotID {
		return biz.Command{}, biz.CredentialInfo{}, biz.Fail(biz.VersionConflict, "credential changed")
	}
	switch record.PreviousCredentialDisposition {
	case "disabled":
		row, e := q.DisableTenantCredential(ctx, sqlcgen.DisableTenantCredentialParams{TenantID: &c.TenantID, SpaceID: c.SpaceID, Purpose: c.Candidate.Purpose, ExpectedVersion: info.Version, Generation: info.Generation})
		if e != nil {
			return biz.Command{}, biz.CredentialInfo{}, databaseError(e)
		}
		info = credentialInfo(row)
	case "blocked_empty":
		row, e := q.BlockTenantRecoveredCredential(ctx, sqlcgen.BlockTenantRecoveredCredentialParams{TenantID: &c.TenantID, SpaceID: c.SpaceID, Purpose: c.Candidate.Purpose, ExpectedVersion: info.Version, Generation: info.Generation})
		if e != nil {
			return biz.Command{}, biz.CredentialInfo{}, databaseError(e)
		}
		info = credentialInfo(row)
	case "unchanged":
	default:
		return biz.Command{}, biz.CredentialInfo{}, biz.Fail(biz.InvalidArgument, "invalid credential disposition")
	}
	c.Candidate.ExpiryRecovery = &record
	candidate, err := json.Marshal(c.Candidate)
	if err != nil {
		return biz.Command{}, biz.CredentialInfo{}, biz.Fail(biz.InternalError, "candidate encoding failed")
	}
	row, err := q.FailTenantCandidateCommand(ctx, sqlcgen.FailTenantCandidateCommandParams{TenantID: &c.TenantID, SpaceID: c.SpaceID, CommandID: c.ID, ExpectedVersion: c.Version, Phase: "expired_candidate_terminated", Reason: string(biz.CandidateExpiredRecovered), Candidate: candidate})
	if errors.Is(err, pgx.ErrNoRows) {
		return biz.Command{}, biz.CredentialInfo{}, biz.Fail(biz.VersionConflict, "command version changed")
	}
	if err != nil {
		return biz.Command{}, biz.CredentialInfo{}, databaseError(err)
	}
	stored, err = fromCommand(row)
	if err != nil {
		return biz.Command{}, biz.CredentialInfo{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return biz.Command{}, biz.CredentialInfo{}, databaseError(err)
	}
	return stored, info, nil
}

// GetPlatformCandidateRecovery is the platform counterpart; it never accepts a
// tenant and never falls back to tenant storage.
func (p *Postgres) GetPlatformCandidateRecovery(ctx context.Context, space, command string) (biz.Command, biz.CredentialInfo, error) {
	q := sqlcgen.New(p.connection(ctx))
	row, err := q.GetPlatformCandidateRecoveryCommand(ctx, sqlcgen.GetPlatformCandidateRecoveryCommandParams{SpaceID: space, CommandID: command})
	if err != nil {
		return biz.Command{}, biz.CredentialInfo{}, databaseError(err)
	}
	stored, err := fromCommand(row)
	if err != nil {
		return biz.Command{}, biz.CredentialInfo{}, err
	}
	if stored.Scope != biz.PlatformImages || stored.TenantID != "" {
		return biz.Command{}, biz.CredentialInfo{}, biz.Fail(biz.InternalError, "invalid stored platform command")
	}
	info, err := platformPublisher(ctx, q, space)
	if err != nil {
		return biz.Command{}, biz.CredentialInfo{}, err
	}
	return stored, info, nil
}

// FailExpiredPlatformCandidate terminates one platform candidate command with
// the same CAS rules as the tenant path.
func (p *Postgres) FailExpiredPlatformCandidate(ctx context.Context, c biz.Command, record biz.CandidateExpiryRecoveryRecord) (biz.Command, biz.CredentialInfo, error) {
	if err := platformScope(c.Scope, c.TenantID); err != nil {
		return biz.Command{}, biz.CredentialInfo{}, err
	}
	var stored biz.Command
	var info biz.CredentialInfo
	err := p.platformTransaction(ctx, func(q *sqlcgen.Queries) error {
		var e error
		info, e = platformPublisher(ctx, q, c.SpaceID)
		if e != nil {
			return e
		}
		// 与租户路径同一条 CAS：校验的是候选之前的旧凭据行。
		if info.Version != record.ExpectedCredentialVersion || info.Generation != c.Candidate.Generation-1 || info.RobotID != record.PreviousRobotID {
			return biz.Fail(biz.VersionConflict, "credential changed")
		}
		switch record.PreviousCredentialDisposition {
		case "disabled":
			row, e := q.DisablePlatformCredential(ctx, sqlcgen.DisablePlatformCredentialParams{SpaceID: c.SpaceID, ExpectedVersion: info.Version, Generation: info.Generation})
			if e != nil {
				return databaseError(e)
			}
			info = credentialInfo(row)
		case "blocked_empty":
			row, e := q.BlockPlatformRecoveredCredential(ctx, sqlcgen.BlockPlatformRecoveredCredentialParams{SpaceID: c.SpaceID, ExpectedVersion: info.Version, Generation: info.Generation})
			if e != nil {
				return databaseError(e)
			}
			info = credentialInfo(row)
		case "unchanged":
		default:
			return biz.Fail(biz.InvalidArgument, "invalid credential disposition")
		}
		c.Candidate.ExpiryRecovery = &record
		candidate, e := json.Marshal(c.Candidate)
		if e != nil {
			return biz.Fail(biz.InternalError, "candidate encoding failed")
		}
		row, e := q.FailPlatformCandidateCommand(ctx, sqlcgen.FailPlatformCandidateCommandParams{SpaceID: c.SpaceID, CommandID: c.ID, ExpectedVersion: c.Version, Phase: "expired_candidate_terminated", Reason: string(biz.CandidateExpiredRecovered), Candidate: candidate})
		if errors.Is(e, pgx.ErrNoRows) {
			return biz.Fail(biz.VersionConflict, "command version changed")
		}
		if e != nil {
			return databaseError(e)
		}
		stored, e = fromCommand(row)
		return e
	})
	return stored, info, err
}
