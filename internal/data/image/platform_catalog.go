package data

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/image"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/image/sqlcgen"
)

func (p *Postgres) applyPlatformCatalog(ctx context.Context, c biz.Command, change func(*sqlcgen.Queries, biz.Space) (biz.Registration, error)) (biz.Registration, error) {
	if err := platformScope(c.Scope, c.TenantID); err != nil {
		return biz.Registration{}, err
	}
	var result biz.Registration
	err := p.platformTransaction(ctx, func(q *sqlcgen.Queries) error {
		row, e := q.GetPlatformSpace(ctx)
		if e != nil {
			return databaseError(e)
		}
		if row.SpaceID != c.SpaceID {
			return biz.Fail(biz.ImageNotFound, "platform space not found")
		}
		stored, e := reservePlatformCommand(ctx, q, c)
		if e != nil {
			return e
		}
		if stored.State == "succeeded" {
			r := stored.Result.Registration
			if r == nil || r.Scope != biz.PlatformImages || r.TenantID != "" || r.SpaceID != c.SpaceID {
				return biz.Fail(biz.InternalError, "invalid platform catalog result")
			}
			result = *r
			return nil
		}
		if stored.State != "pending" {
			return biz.Fail(biz.RequestInProgress, "catalog request is not pending")
		}
		result, e = change(q, fromSpace(row))
		if e != nil {
			return e
		}
		stored.Result = biz.CommandResult{Registration: &result}
		_, e = completePlatformCommand(ctx, q, stored)
		return e
	})
	return result, err
}
func (p *Postgres) ApplyPlatformRegistration(ctx context.Context, c biz.Command, r biz.Registration) (biz.Registration, error) {
	if c.Kind != "register_image" || r.Scope != biz.PlatformImages || r.TenantID != "" || r.SpaceID != c.SpaceID {
		return biz.Registration{}, biz.Fail(biz.InvalidArgument, "invalid platform registration ownership")
	}
	return p.applyPlatformCatalog(ctx, c, func(q *sqlcgen.Queries, s biz.Space) (biz.Registration, error) {
		if s.State != "available" {
			return biz.Registration{}, biz.Fail(biz.SpaceNotReady, "platform space not ready")
		}
		platforms, err := json.Marshal(r.Platforms)
		if err != nil {
			return biz.Registration{}, biz.Fail(biz.InternalError, "platform encoding failed")
		}
		row, err := q.InsertPlatformRegistration(ctx, sqlcgen.InsertPlatformRegistrationParams{ImageID: r.ID, SpaceID: r.SpaceID, DisplayName: r.Metadata.DisplayName, Description: r.Metadata.Description, Repository: r.Repository, SourceReference: r.SourceReference, Digest: r.Digest, MediaType: r.MediaType, Platforms: platforms, Purposes: r.Metadata.Purposes, Accelerator: r.Metadata.Accelerator, Actor: c.Actor})
		if err != nil {
			return biz.Registration{}, databaseError(err)
		}
		return fromRegistration(row, s)
	})
}
func (p *Postgres) ApplyPlatformMetadata(ctx context.Context, c biz.Command, r biz.UpdateImage) (biz.Registration, error) {
	if c.Kind != "update_image" || r.TenantID != "" {
		return biz.Registration{}, biz.Fail(biz.InvalidArgument, "invalid platform metadata ownership")
	}
	return p.applyPlatformCatalog(ctx, c, func(q *sqlcgen.Queries, s biz.Space) (biz.Registration, error) {
		row, err := q.UpdatePlatformMetadata(ctx, sqlcgen.UpdatePlatformMetadataParams{ImageID: r.ImageID, DisplayName: r.Metadata.DisplayName, Description: r.Metadata.Description, Purposes: r.Metadata.Purposes, Accelerator: r.Metadata.Accelerator, Actor: c.Actor, ExpectedVersion: r.ExpectedVersion})
		if errors.Is(err, pgx.ErrNoRows) {
			return biz.Registration{}, platformCASFailure(ctx, q, r.ImageID)
		}
		if err != nil {
			return biz.Registration{}, databaseError(err)
		}
		return fromRegistration(row, s)
	})
}
func (p *Postgres) ApplyPlatformUnregister(ctx context.Context, c biz.Command, r biz.UnregisterImage) (biz.Registration, error) {
	if c.Kind != "unregister_image" || r.TenantID != "" {
		return biz.Registration{}, biz.Fail(biz.InvalidArgument, "invalid platform unregister ownership")
	}
	return p.applyPlatformCatalog(ctx, c, func(q *sqlcgen.Queries, s biz.Space) (biz.Registration, error) {
		existing, err := q.GetPlatformRegistration(ctx, sqlcgen.GetPlatformRegistrationParams{ImageID: r.ImageID})
		if err != nil {
			return biz.Registration{}, databaseError(err)
		}
		if existing.UnregisteredAt != nil {
			return fromRegistration(existing, s)
		}
		row, err := q.UnregisterPlatformRegistration(ctx, sqlcgen.UnregisterPlatformRegistrationParams{ImageID: r.ImageID, Actor: c.Actor, ExpectedVersion: r.ExpectedVersion})
		if errors.Is(err, pgx.ErrNoRows) {
			return biz.Registration{}, platformCASFailure(ctx, q, r.ImageID)
		}
		if err != nil {
			return biz.Registration{}, databaseError(err)
		}
		return fromRegistration(row, s)
	})
}
func platformCASFailure(ctx context.Context, q *sqlcgen.Queries, id string) error {
	row, err := q.GetPlatformRegistration(ctx, sqlcgen.GetPlatformRegistrationParams{ImageID: id})
	if err != nil {
		return databaseError(err)
	}
	if row.UnregisteredAt != nil {
		return biz.Fail(biz.ImageNotFound, "image is unregistered")
	}
	return biz.Fail(biz.VersionConflict, "image version changed")
}

// OffsetPlatformRegistrations pages the platform catalog. COUNT, the page and
// the space reference share one REPEATABLE READ snapshot.
func (p *Postgres) OffsetPlatformRegistrations(ctx context.Context, f biz.ManagementFilter, offset int64, limit int32) ([]biz.Registration, int64, error) {
	if offset < 0 || limit < 1 || limit > 100 {
		return nil, 0, biz.Fail(biz.InvalidArgument, "invalid page request")
	}
	tx, err := p.connection(ctx).BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, 0, databaseError(err)
	}
	defer tx.Rollback(ctx)
	q := sqlcgen.New(tx)
	stored, err := q.GetPlatformSpace(ctx)
	if err != nil {
		return nil, 0, databaseError(err)
	}
	total, err := q.CountPlatformRegistrations(ctx, sqlcgen.CountPlatformRegistrationsParams{SearchText: f.Search, SearchPattern: searchPattern(f.Search), Purposes: f.Purposes, Accelerator: f.Accelerator})
	if err != nil {
		return nil, 0, databaseError(err)
	}
	rows, err := q.ListPlatformRegistrationsOffset(ctx, sqlcgen.ListPlatformRegistrationsOffsetParams{SearchText: f.Search, SearchPattern: searchPattern(f.Search), Purposes: f.Purposes, Accelerator: f.Accelerator, PageLimit: int64(limit), PageOffset: offset})
	if err != nil {
		return nil, 0, databaseError(err)
	}
	values, err := registrations(rows, fromSpace(stored))
	if err != nil {
		return nil, 0, err
	}
	return values, total, nil
}

// ApplyPlatformRegistrationPatch applies a management patch to platform
// metadata in one short transaction, replaying before the version check.
func (p *Postgres) ApplyPlatformRegistrationPatch(ctx context.Context, c biz.Command, r biz.PatchImage) (biz.Registration, error) {
	if c.Kind != "update_image" || c.Scope != biz.PlatformImages || c.TenantID != "" || r.TenantID != "" || r.ImageID == "" {
		return biz.Registration{}, biz.Fail(biz.InvalidArgument, "invalid platform patch ownership")
	}
	var result biz.Registration
	err := p.platformTransaction(ctx, func(q *sqlcgen.Queries) error {
		space, e := q.GetPlatformSpace(ctx)
		if e != nil {
			return databaseError(e)
		}
		if space.SpaceID != c.SpaceID {
			return biz.Fail(biz.ImageNotFound, "platform space not found")
		}
		locked, e := q.LockPlatformRegistration(ctx, sqlcgen.LockPlatformRegistrationParams{ImageID: r.ImageID})
		if e != nil {
			return databaseError(e)
		}
		stored, e := reservePlatformCommand(ctx, q, c)
		if e != nil {
			return e
		}
		if stored.State == "succeeded" {
			saved := stored.Result.Registration
			if saved == nil || saved.Scope != biz.PlatformImages || saved.TenantID != "" || saved.SpaceID != c.SpaceID || saved.ID != r.ImageID {
				return biz.Fail(biz.InternalError, "invalid stored platform patch result")
			}
			result = *saved
			return nil
		}
		// 已注销只拒绝新写入。同 key 重放必须先返回首次结果：PATCH 可能已提交
		// 但响应丢失，随后镜像被注销，此时重试不应变成 404。
		if locked.UnregisteredAt != nil {
			return biz.Fail(biz.ImageNotFound, "image not found")
		}
		if stored.State != "pending" {
			return biz.Fail(biz.RequestInProgress, "catalog request is not pending")
		}
		current, e := fromRegistration(locked, fromSpace(space))
		if e != nil {
			return e
		}
		if current.Version != r.ExpectedVersion {
			return biz.Fail(biz.VersionConflict, "image version changed")
		}
		merged, e := biz.ApplyPatch(current.Metadata, r)
		if e != nil {
			return e
		}
		row, e := q.UpdatePlatformMetadata(ctx, sqlcgen.UpdatePlatformMetadataParams{ImageID: r.ImageID, DisplayName: merged.DisplayName, Description: merged.Description, Purposes: merged.Purposes, Accelerator: merged.Accelerator, Actor: c.Actor, ExpectedVersion: r.ExpectedVersion})
		if errors.Is(e, pgx.ErrNoRows) {
			return biz.Fail(biz.VersionConflict, "image version changed")
		}
		if e != nil {
			return databaseError(e)
		}
		result, e = fromRegistration(row, fromSpace(space))
		if e != nil {
			return e
		}
		stored.Result = biz.CommandResult{Registration: &result}
		_, e = completePlatformCommand(ctx, q, stored)
		return e
	})
	return result, err
}
