package data

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/image"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/image/sqlcgen"
)

func platformScope(scope biz.ImageScope, tenant string) error {
	if scope != biz.PlatformImages || tenant != "" {
		return biz.Fail(biz.InvalidArgument, "platform ownership required")
	}
	return nil
}
func platformVersion(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return biz.Fail(biz.VersionConflict, "platform state version changed")
	}
	return databaseError(err)
}

// Only short database work runs in this transaction; provider calls stay in biz.
func (p *Postgres) platformTransaction(ctx context.Context, fn func(*sqlcgen.Queries) error) error {
	tx, err := p.connection(ctx).Begin(ctx)
	if err != nil {
		return databaseError(err)
	}
	defer tx.Rollback(ctx)
	q := sqlcgen.New(tx)
	if err = q.LockPlatformSpace(ctx); err != nil {
		return databaseError(err)
	}
	if err = fn(q); err != nil {
		return err
	}
	return databaseError(tx.Commit(ctx))
}
func (p *Postgres) FindPlatformCommand(ctx context.Context, space, key string) (biz.Command, error) {
	row, err := sqlcgen.New(p.connection(ctx)).GetPlatformCommand(ctx, sqlcgen.GetPlatformCommandParams{SpaceID: space, IdempotencyKey: key})
	if err != nil {
		return biz.Command{}, databaseError(err)
	}
	return fromCommand(row)
}
func reservePlatformCommand(ctx context.Context, q *sqlcgen.Queries, c biz.Command) (biz.Command, error) {
	if err := platformScope(c.Scope, c.TenantID); err != nil {
		return biz.Command{}, err
	}
	if _, err := biz.ParseIdempotencyKey(c.Key); err != nil {
		return biz.Command{}, err
	}
	row, err := q.GetPlatformCommand(ctx, sqlcgen.GetPlatformCommandParams{SpaceID: c.SpaceID, IdempotencyKey: c.Key})
	if err == nil {
		if row.Kind != c.Kind || row.Actor != c.Actor || row.Fingerprint != c.Fingerprint {
			return biz.Command{}, biz.Fail(biz.IdempotencyConflict, "idempotency key belongs to another request")
		}
		return fromCommand(row)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return biz.Command{}, databaseError(err)
	}
	row, err = q.InsertPlatformCommand(ctx, sqlcgen.InsertPlatformCommandParams{CommandID: c.ID, SpaceID: c.SpaceID, IdempotencyKey: c.Key, Kind: c.Kind, Actor: c.Actor, Fingerprint: c.Fingerprint})
	if err != nil {
		return biz.Command{}, databaseError(err)
	}
	return fromCommand(row)
}
func savePlatformCommand(ctx context.Context, q *sqlcgen.Queries, c biz.Command) (biz.Command, error) {
	if err := platformScope(c.Scope, c.TenantID); err != nil {
		return biz.Command{}, err
	}
	candidate, err := json.Marshal(c.Candidate)
	if err != nil {
		return biz.Command{}, biz.Fail(biz.InternalError, "candidate encoding failed")
	}
	var key *string
	if c.DeliverySecret.KeyID != "" {
		key = &c.DeliverySecret.KeyID
	}
	row, err := q.UpdatePlatformCommandPhase(ctx, sqlcgen.UpdatePlatformCommandPhaseParams{SpaceID: c.SpaceID, CommandID: c.ID, ExpectedVersion: c.Version, Phase: c.Phase, State: c.State, Reason: string(c.Reason), Candidate: candidate, SecretCiphertext: c.DeliverySecret.Ciphertext, SecretKeyID: key})
	if err != nil {
		return biz.Command{}, platformVersion(err)
	}
	return fromCommand(row)
}
func (p *Postgres) SavePlatformCommandPhase(ctx context.Context, c biz.Command) (biz.Command, error) {
	return savePlatformCommand(ctx, sqlcgen.New(p.connection(ctx)), c)
}
func completePlatformCommand(ctx context.Context, q *sqlcgen.Queries, c biz.Command) (biz.Command, error) {
	if err := platformScope(c.Scope, c.TenantID); err != nil {
		return biz.Command{}, err
	}
	result, err := json.Marshal(c.Result)
	if err != nil {
		return biz.Command{}, biz.Fail(biz.InternalError, "platform result encoding failed")
	}
	row, err := q.CompletePlatformCommand(ctx, sqlcgen.CompletePlatformCommandParams{SpaceID: c.SpaceID, CommandID: c.ID, ExpectedVersion: c.Version, Result: result, SecretReplayUntil: c.ReplayUntil})
	if err != nil {
		return biz.Command{}, platformVersion(err)
	}
	return fromCommand(row)
}
func (p *Postgres) ReservePlatformSpace(ctx context.Context, s biz.Space, c biz.Command) (biz.Space, biz.Command, error) {
	if err := platformScope(s.Scope, s.TenantID); err != nil {
		return biz.Space{}, biz.Command{}, err
	}
	if err := platformScope(c.Scope, c.TenantID); err != nil {
		return biz.Space{}, biz.Command{}, err
	}
	if c.Kind != "enable_space" {
		return biz.Space{}, biz.Command{}, biz.Fail(biz.InvalidArgument, "invalid platform initialization")
	}
	err := p.platformTransaction(ctx, func(q *sqlcgen.Queries) error {
		row, e := q.GetPlatformSpace(ctx)
		if errors.Is(e, pgx.ErrNoRows) {
			row, e = q.ReservePlatformSpace(ctx, sqlcgen.ReservePlatformSpaceParams{SpaceID: s.ID, InstallationID: s.InstallationID, RegistryAuthority: s.RegistryAuthority, ProjectName: s.ProjectName})
		}
		if e != nil {
			return databaseError(e)
		}
		if row.InstallationID != s.InstallationID || row.RegistryAuthority != s.RegistryAuthority || row.ProjectName != s.ProjectName {
			return biz.Fail(biz.SpaceNameImmutable, "platform binding is immutable")
		}
		s = fromSpace(row)
		c.SpaceID = s.ID
		c, e = reservePlatformCommand(ctx, q, c)
		return e
	})
	return s, c, err
}
func (p *Postgres) BindPlatformProject(ctx context.Context, s biz.Space, id int64) (biz.Space, error) {
	if err := platformScope(s.Scope, s.TenantID); err != nil {
		return biz.Space{}, err
	}
	if id <= 0 {
		return biz.Space{}, biz.Fail(biz.InvalidArgument, "invalid project ID")
	}
	row, err := sqlcgen.New(p.connection(ctx)).BindPlatformProject(ctx, sqlcgen.BindPlatformProjectParams{SpaceID: s.ID, ExpectedVersion: s.Version, ProjectID: pgtype.Int8{Int64: id, Valid: true}})
	if err != nil {
		return biz.Space{}, platformVersion(err)
	}
	return fromSpace(row), nil
}
func (p *Postgres) BlockPlatformSpace(ctx context.Context, s biz.Space, reason biz.Reason) (biz.Space, error) {
	if err := platformScope(s.Scope, s.TenantID); err != nil {
		return biz.Space{}, err
	}
	row, err := sqlcgen.New(p.connection(ctx)).SetPlatformSpaceReason(ctx, sqlcgen.SetPlatformSpaceReasonParams{SpaceID: s.ID, ExpectedVersion: s.Version, State: "blocked", Reason: string(reason)})
	if err != nil {
		return biz.Space{}, platformVersion(err)
	}
	return fromSpace(row), nil
}
func (p *Postgres) CompletePlatformEnable(ctx context.Context, s biz.Space, c biz.Command) (biz.Space, biz.Command, error) {
	if err := platformScope(s.Scope, s.TenantID); err != nil {
		return biz.Space{}, biz.Command{}, err
	}
	if c.SpaceID != s.ID || c.Scope != s.Scope || c.TenantID != "" || c.Kind != "enable_space" {
		return biz.Space{}, biz.Command{}, biz.Fail(biz.InvalidArgument, "invalid platform initialization")
	}
	err := p.platformTransaction(ctx, func(q *sqlcgen.Queries) error {
		row, e := q.SetPlatformSpaceAvailable(ctx, sqlcgen.SetPlatformSpaceAvailableParams{SpaceID: s.ID, ExpectedVersion: s.Version})
		if e != nil {
			return platformVersion(e)
		}
		s = fromSpace(row)
		c.Result = biz.CommandResult{Space: &s}
		c.ReplayUntil = nil
		c, e = completePlatformCommand(ctx, q, c)
		return e
	})
	return s, c, err
}
func platformPublisher(ctx context.Context, q *sqlcgen.Queries, space string) (biz.CredentialInfo, error) {
	row, err := q.GetPlatformPublisherCredential(ctx, sqlcgen.GetPlatformPublisherCredentialParams{SpaceID: space})
	if errors.Is(err, pgx.ErrNoRows) {
		return biz.CredentialInfo{SpaceID: space, Scope: biz.PlatformImages, Purpose: "publisher", State: "not_issued"}, nil
	}
	if err != nil {
		return biz.CredentialInfo{}, databaseError(err)
	}
	return credentialInfo(row), nil
}
func (p *Postgres) GetPlatformPublisher(ctx context.Context, space string) (biz.CredentialInfo, error) {
	s, err := p.FindPlatformSpace(ctx)
	if err != nil {
		return biz.CredentialInfo{}, err
	}
	if s.ID != space {
		return biz.CredentialInfo{}, biz.Fail(biz.SpaceNotFound, "platform space not found")
	}
	return platformPublisher(ctx, sqlcgen.New(p.connection(ctx)), space)
}
func (p *Postgres) BeginPlatformCredentialCommand(ctx context.Context, c biz.Command, expected int64) (biz.Command, error) {
	if err := platformScope(c.Scope, c.TenantID); err != nil {
		return biz.Command{}, err
	}
	if expected < 0 || (c.Kind != "issue_publisher" && c.Kind != "reset_publisher" && c.Kind != "disable_publisher") {
		return biz.Command{}, biz.Fail(biz.InvalidArgument, "invalid platform credential request")
	}
	var stored biz.Command
	err := p.platformTransaction(ctx, func(q *sqlcgen.Queries) error {
		s, e := q.GetPlatformSpace(ctx)
		if e != nil {
			return databaseError(e)
		}
		if s.SpaceID != c.SpaceID {
			return biz.Fail(biz.SpaceNotFound, "platform space not found")
		}
		// A replay of a finished command wins over every version or state check,
		// so a retry after a committed write is never reported as a conflict.
		existing, e := q.GetPlatformCommand(ctx, sqlcgen.GetPlatformCommandParams{SpaceID: c.SpaceID, IdempotencyKey: c.Key})
		if e == nil {
			if existing.Kind != c.Kind || existing.Actor != c.Actor || existing.Fingerprint != c.Fingerprint {
				return biz.Fail(biz.IdempotencyConflict, "idempotency key belongs to another request")
			}
			stored, e = fromCommand(existing)
			return e
		}
		if !errors.Is(e, pgx.ErrNoRows) {
			return databaseError(e)
		}
		if s.State != "available" {
			return biz.Fail(biz.SpaceNotReady, "platform space not ready")
		}
		info, e := platformPublisher(ctx, q, c.SpaceID)
		if e != nil {
			return e
		}
		if info.Version != expected {
			return biz.Fail(biz.VersionConflict, "credential version changed")
		}
		switch c.Kind {
		case "issue_publisher":
			if info.State == "active" {
				return biz.Fail(biz.CredentialAlreadyActive, "platform publisher already active")
			}
		case "reset_publisher":
			if info.Generation == 0 {
				return biz.Fail(biz.InvalidArgument, "publisher has not been issued")
			}
		case "disable_publisher":
			if info.Generation == 0 {
				return biz.Fail(biz.CredentialNotIssued, "publisher has not been issued")
			}
		}
		stored, e = reservePlatformCommand(ctx, q, c)
		return e
	})
	return stored, err
}

// CompletePlatformDisable finishes a platform publisher disable in one short
// transaction: the credential CAS, the expired delivery-secret scrub and the
// command result share the same commit. The credentials table cannot hold a
// platform publisher ciphertext (image_publisher_no_longterm_secret), so only
// the command-side one-time delivery secrets are cleaned here.
func (p *Postgres) CompletePlatformDisable(ctx context.Context, c biz.Command) (biz.Command, error) {
	if err := platformScope(c.Scope, c.TenantID); err != nil {
		return biz.Command{}, err
	}
	if c.Kind != "disable_publisher" {
		return biz.Command{}, biz.Fail(biz.InvalidArgument, "invalid disable command")
	}
	var stored biz.Command
	err := p.platformTransaction(ctx, func(q *sqlcgen.Queries) error {
		space, e := q.GetPlatformSpace(ctx)
		if e != nil {
			return databaseError(e)
		}
		if space.SpaceID != c.SpaceID {
			return biz.Fail(biz.SpaceNotFound, "platform space not found")
		}
		info, e := platformPublisher(ctx, q, c.SpaceID)
		if e != nil {
			return e
		}
		if info.Version != c.Candidate.CredentialVersion || info.Generation != c.Candidate.Generation || info.RobotID != c.Candidate.RobotID {
			return biz.Fail(biz.VersionConflict, "credential changed")
		}
		if info.Generation == 0 {
			return biz.Fail(biz.CredentialNotIssued, "publisher has not been issued")
		}
		if info.State == "active" {
			row, e := q.DisablePlatformCredential(ctx, sqlcgen.DisablePlatformCredentialParams{SpaceID: c.SpaceID, ExpectedVersion: info.Version, Generation: info.Generation})
			if e != nil {
				return databaseError(e)
			}
			info = credentialInfo(row)
		}
		// Only already expired one-time publisher deliveries are removed; the
		// replay window of a live delivery is never shortened here.
		if _, e = q.ScrubExpiredSpaceDeliverySecrets(ctx, sqlcgen.ScrubExpiredSpaceDeliverySecretsParams{SpaceID: c.SpaceID, NowAt: timePtr(time.Now())}); e != nil {
			return databaseError(e)
		}
		c.Result = biz.CommandResult{Credential: &info}
		c.ReplayUntil = nil
		stored, e = completePlatformCommand(ctx, q, c)
		return e
	})
	return stored, err
}

func checkPlatformCandidate(ctx context.Context, q *sqlcgen.Queries, c biz.Command) error {
	if err := platformScope(c.Scope, c.TenantID); err != nil {
		return err
	}
	if c.Candidate.Purpose != "publisher" || (c.Kind != "issue_publisher" && c.Kind != "reset_publisher") {
		return biz.Fail(biz.InvalidArgument, "invalid platform candidate")
	}
	row, err := q.GetPlatformCommand(ctx, sqlcgen.GetPlatformCommandParams{SpaceID: c.SpaceID, IdempotencyKey: c.Key})
	if err != nil {
		return databaseError(err)
	}
	if row.CommandID != c.ID || row.Version != c.Version || row.State == "succeeded" || row.State == "failed" {
		return biz.Fail(biz.VersionConflict, "command version changed")
	}
	info, err := platformPublisher(ctx, q, c.SpaceID)
	if err != nil {
		return err
	}
	if info.Version != c.Candidate.CredentialVersion || info.Generation >= c.Candidate.Generation || info.RobotID != c.Candidate.PreviousRobotID {
		return biz.Fail(biz.VersionConflict, "credential generation changed")
	}
	return nil
}
func (p *Postgres) PreparePlatformCandidate(ctx context.Context, c biz.Command) (biz.Command, error) {
	var stored biz.Command
	err := p.platformTransaction(ctx, func(q *sqlcgen.Queries) error {
		if e := checkPlatformCandidate(ctx, q, c); e != nil {
			return e
		}
		if c.Candidate.CredentialVersion == 0 {
			row, e := q.PutPlatformCandidateState(ctx, sqlcgen.PutPlatformCandidateStateParams{SpaceID: c.SpaceID})
			if e != nil {
				return databaseError(e)
			}
			c.Candidate.CredentialVersion = row.Version
		}
		var e error
		stored, e = savePlatformCommand(ctx, q, c)
		return e
	})
	return stored, err
}
func (p *Postgres) ActivatePlatformCandidate(ctx context.Context, s biz.Space, c biz.Command) (biz.Space, biz.Command, error) {
	if err := platformScope(s.Scope, s.TenantID); err != nil {
		return biz.Space{}, biz.Command{}, err
	}
	if s.ID != c.SpaceID {
		return biz.Space{}, biz.Command{}, biz.Fail(biz.InvalidArgument, "invalid credential space")
	}
	err := p.platformTransaction(ctx, func(q *sqlcgen.Queries) error {
		if e := checkPlatformCandidate(ctx, q, c); e != nil {
			return e
		}
		v := c.Candidate
		row, e := q.ActivatePlatformCredential(ctx, sqlcgen.ActivatePlatformCredentialParams{SpaceID: s.ID, ExpectedVersion: v.CredentialVersion, Generation: v.Generation, RobotID: pgtype.Int8{Int64: v.RobotID, Valid: true}, RobotName: &v.RobotName, Username: &v.Username, ExpiresAt: v.ExpiresAt, NeverExpires: v.ExpiresAt == nil})
		if e != nil {
			return platformVersion(e)
		}
		info := credentialInfo(row)
		c.Result = biz.CommandResult{Credential: &info}
		c, e = completePlatformCommand(ctx, q, c)
		return e
	})
	return s, c, err
}
func (p *Postgres) OpenPlatformExternalCommand(ctx context.Context, space string) (biz.Command, error) {
	row, err := sqlcgen.New(p.connection(ctx)).GetOpenPlatformExternalCommand(ctx, sqlcgen.GetOpenPlatformExternalCommandParams{SpaceID: space})
	if err != nil {
		return biz.Command{}, databaseError(err)
	}
	return fromCommand(row)
}
func (p *Postgres) RecoverPlatformProject(ctx context.Context, s biz.Space, c biz.Command, id int64, evidence string) (biz.Space, error) {
	if err := platformScope(s.Scope, s.TenantID); err != nil {
		return biz.Space{}, err
	}
	if c.Scope != s.Scope || c.TenantID != "" || s.ID != c.SpaceID || s.ProjectID != 0 || c.Kind != "enable_space" || c.Phase != "project_sent" || id <= 0 || len(evidence) != 64 {
		return biz.Space{}, biz.Fail(biz.InvalidArgument, "invalid platform recovery")
	}
	err := p.platformTransaction(ctx, func(q *sqlcgen.Queries) error {
		row, e := q.BindPlatformProject(ctx, sqlcgen.BindPlatformProjectParams{SpaceID: s.ID, ExpectedVersion: s.Version, ProjectID: pgtype.Int8{Int64: id, Valid: true}})
		if e != nil {
			return platformVersion(e)
		}
		c.Candidate.RecoveryEvidence = evidence
		c.State = "retryable"
		c.Reason = ""
		if _, e = savePlatformCommand(ctx, q, c); e != nil {
			return e
		}
		s = fromSpace(row)
		return nil
	})
	return s, err
}

var _ biz.PlatformRepository = (*Postgres)(nil)
