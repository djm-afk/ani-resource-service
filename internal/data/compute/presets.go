package compute

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/compute"
)

type scanner interface{ Scan(...any) error }

const presetColumns = "id::text,code,name,description,compute_type,cpu_millis,memory_mib,enabled,gpu,created_at,updated_at,created_by,updated_by"

func scanPreset(row scanner) (biz.Preset, error) {
	var p biz.Preset
	var gpu []byte
	e := row.Scan(&p.ID, &p.Code, &p.Name, &p.Description, &p.ComputeType, &p.CPUMillis, &p.MemoryMiB, &p.Enabled, &gpu, &p.CreatedAt, &p.UpdatedAt, &p.CreatedBy, &p.UpdatedBy)
	if errors.Is(e, pgx.ErrNoRows) {
		return p, biz.ErrNotFound
	}
	if e != nil {
		return p, e
	}
	if len(gpu) != 0 {
		decoder := json.NewDecoder(bytes.NewReader(gpu))
		decoder.DisallowUnknownFields()
		if e = decoder.Decode(&p.GPU); e != nil {
			return p, e
		}
		if decoder.Decode(new(any)) != io.EOF {
			return p, biz.ErrInvalid
		}
	}
	if e = biz.ValidatePreset(p); e != nil {
		return p, e
	}
	return p, nil
}
func presetGPU(g *biz.GPUReference) ([]byte, error) {
	if g == nil {
		return nil, nil
	}
	return json.Marshal(g)
}
func presetWriteError(e error) error {
	var pg *pgconn.PgError
	if errors.As(e, &pg) && pg.Code == "23505" {
		return biz.ErrPresetConflict
	}
	return e
}
func (p *Postgres) CreatePreset(ctx context.Context, v biz.Preset) (biz.Preset, error) {
	c, e := biz.RequirePlatform(ctx)
	if e != nil {
		return biz.Preset{}, e
	}
	v.ID = uuid.NewString()
	v.CreatedBy = c.Actor
	v.UpdatedBy = c.Actor
	if e = biz.ValidatePreset(v); e != nil {
		return biz.Preset{}, e
	}
	gpu, e := presetGPU(v.GPU)
	if e != nil {
		return biz.Preset{}, e
	}
	out, e := scanPreset(p.pool.QueryRow(ctx, "INSERT INTO compute.presets(id,code,name,description,compute_type,cpu_millis,memory_mib,enabled,gpu,created_by,updated_by) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$10) RETURNING "+presetColumns, v.ID, v.Code, v.Name, v.Description, v.ComputeType, v.CPUMillis, v.MemoryMiB, v.Enabled, gpu, c.Actor))
	return out, presetWriteError(e)
}
func (p *Postgres) GetPreset(ctx context.Context, id string) (biz.Preset, error) {
	if _, e := biz.CallerFromContext(ctx); e != nil {
		if _, e = biz.RequirePlatform(ctx); e != nil {
			return biz.Preset{}, e
		}
	}
	if !biz.UUID(id) {
		return biz.Preset{}, biz.ErrInvalid
	}
	return scanPreset(p.pool.QueryRow(ctx, "SELECT "+presetColumns+" FROM compute.presets WHERE id=$1", id))
}
func (p *Postgres) ListPresets(ctx context.Context, f biz.PresetFilter) ([]biz.Preset, int64, bool, error) {
	if f.Tenant {
		if _, e := biz.CallerFromContext(ctx); e != nil {
			return nil, 0, false, e
		}
	} else {
		if _, e := biz.RequirePlatform(ctx); e != nil {
			return nil, 0, false, e
		}
	}
	if e := biz.ValidatePresetFilter(f); e != nil {
		return nil, 0, false, e
	}
	_, size, offset, _ := biz.PageWindow(f.Page, f.PageSize)
	where := " WHERE ($1::text='' OR position(lower($1) in lower(name))>0) AND ($2::text='' OR code=$2) AND ($3::text='' OR compute_type=$3) AND ($4::text='' OR gpu->>'mode'=$4) AND ($5::boolean IS NULL OR enabled=$5)"
	enabled := f.Enabled
	if f.Tenant {
		v := true
		enabled = &v
	}
	args := []any{strings.TrimSpace(f.Name), strings.TrimSpace(f.Code), f.ComputeType, f.GPUMode, enabled}
	tx, e := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if e != nil {
		return nil, 0, false, e
	}
	defer tx.Rollback(ctx)
	var candidates bool
	if f.Tenant {
		if e = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM compute.presets"+where+" AND compute_type='gpu')", args...).Scan(&candidates); e != nil {
			return nil, 0, false, e
		}
		allowed := f.AllowedGPUProfiles
		if allowed == nil {
			allowed = []biz.GPUReference{}
		}
		raw, e := json.Marshal(allowed)
		if e != nil {
			return nil, 0, false, e
		}
		args = append(args, raw)
		where += " AND (compute_type='cpu' OR EXISTS(SELECT 1 FROM jsonb_array_elements($6::jsonb) a WHERE a->>'profile_id'=gpu->>'profile_id' AND a->>'profile_version'=gpu->>'profile_version' AND a->>'spec_digest'=gpu->>'spec_digest' AND a->>'cluster_id'=gpu->>'cluster_id' AND a->>'pool_id'=gpu->>'pool_id'))"
	}
	var total int64
	if e = tx.QueryRow(ctx, "SELECT count(*) FROM compute.presets"+where, args...).Scan(&total); e != nil {
		return nil, 0, false, e
	}
	n := len(args)
	args = append(args, int64(size), offset)
	rows, e := tx.Query(ctx, "SELECT "+presetColumns+" FROM compute.presets"+where+fmt.Sprintf(" ORDER BY created_at DESC,id DESC LIMIT $%d OFFSET $%d", n+1, n+2), args...)
	if e != nil {
		return nil, 0, false, e
	}
	out := []biz.Preset{}
	for rows.Next() {
		v, e := scanPreset(rows)
		if e != nil {
			rows.Close()
			return nil, 0, false, e
		}
		out = append(out, v)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, 0, false, e
	}
	if e = tx.Commit(ctx); e != nil {
		return nil, 0, false, e
	}
	return out, total, candidates, nil
}
func (p *Postgres) UpdatePreset(ctx context.Context, id string, changes biz.Preset, paths []string) (biz.Preset, error) {
	c, e := biz.RequirePlatform(ctx)
	if e != nil {
		return biz.Preset{}, e
	}
	if !biz.UUID(id) || biz.ValidatePresetMask(paths) != nil {
		return biz.Preset{}, biz.ErrInvalid
	}
	tx, e := p.pool.Begin(ctx)
	if e != nil {
		return biz.Preset{}, e
	}
	defer tx.Rollback(ctx)
	current, e := scanPreset(tx.QueryRow(ctx, "SELECT "+presetColumns+" FROM compute.presets WHERE id=$1 FOR UPDATE", id))
	if e != nil {
		return biz.Preset{}, e
	}
	merged, e := biz.MergePreset(current, changes, paths)
	if e != nil {
		return biz.Preset{}, e
	}
	gpu, e := presetGPU(merged.GPU)
	if e != nil {
		return biz.Preset{}, e
	}
	out, e := scanPreset(tx.QueryRow(ctx, "UPDATE compute.presets SET name=$2,description=$3,compute_type=$4,cpu_millis=$5,memory_mib=$6,enabled=$7,gpu=$8,updated_at=clock_timestamp(),updated_by=$9 WHERE id=$1 RETURNING "+presetColumns, id, merged.Name, merged.Description, merged.ComputeType, merged.CPUMillis, merged.MemoryMiB, merged.Enabled, gpu, c.Actor))
	if e != nil {
		return biz.Preset{}, e
	}
	if e = tx.Commit(ctx); e != nil {
		return biz.Preset{}, e
	}
	return out, nil
}
func (p *Postgres) SetPresetEnabled(ctx context.Context, id string, enabled bool) (biz.Preset, error) {
	return p.UpdatePreset(ctx, id, biz.Preset{Enabled: enabled}, []string{"enabled"})
}
func (p *Postgres) DeletePreset(ctx context.Context, id string) error {
	if _, e := biz.RequirePlatform(ctx); e != nil {
		return e
	}
	if !biz.UUID(id) {
		return biz.ErrInvalid
	}
	r, e := p.pool.Exec(ctx, "DELETE FROM compute.presets WHERE id=$1", id)
	if e != nil {
		return e
	}
	if r.RowsAffected() == 0 {
		return biz.ErrNotFound
	}
	return nil
}
