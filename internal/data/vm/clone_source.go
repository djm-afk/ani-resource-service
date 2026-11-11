package vm

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
)

// CloneReleaseKinds is the closed audit set accepted by the reference table.
var CloneReleaseKinds = map[string]bool{
	"REGISTERED": true, "FAILED_NO_OUTPUT": true, "GONE_NO_OUTPUT": true,
	"ADMIN_RELEASED": true, "CANCELLED": true,
}

// ReleaseCloneSourceHELD writes the release audit columns. A replayed release is
// idempotent; a HELD row is never partially written because the table check
// constraint requires every audit column.
func (p *Postgres) ReleaseCloneSourceHELD(ctx context.Context, r biz.CloneSourceRelease) error {
	if !biz.UUID(r.TenantID) || !biz.UUID(r.TargetVMID) || !CloneReleaseKinds[r.Kind] || r.By == "" || r.Reason == "" || r.Evidence == "" {
		return biz.ErrInvalid
	}
	tag, e := p.pool.Exec(ctx, `UPDATE vm.clone_source_references SET state='RELEASED',release_kind=$3,release_reason=$4,released_by=$5,release_evidence=$6,released_at=now() WHERE tenant_id=$1 AND target_vm_id=$2 AND state='HELD'`, r.TenantID, r.TargetVMID, r.Kind, r.Reason, r.By, r.Evidence)
	if e != nil {
		return e
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var state string
	if e = p.pool.QueryRow(ctx, "SELECT state FROM vm.clone_source_references WHERE tenant_id=$1 AND target_vm_id=$2", r.TenantID, r.TargetVMID).Scan(&state); e != nil {
		if errors.Is(e, pgx.ErrNoRows) {
			return biz.ErrNotFound
		}
		return e
	}
	if state == "RELEASED" {
		return nil
	}
	return biz.ErrVersion
}

// CloneSourceReference returns the audit row of one target.
func (p *Postgres) CloneSourceReference(ctx context.Context, tenant, target string) (biz.CloneSourceReference, error) {
	var r biz.CloneSourceReference
	var releasedAt *time.Time
	e := p.pool.QueryRow(ctx, `SELECT tenant_id,source_vm_id,target_vm_id,create_operation_id,source_native_uid,source_version,state,release_kind,release_reason,released_by,release_evidence,released_at FROM vm.clone_source_references WHERE tenant_id=$1 AND target_vm_id=$2`, tenant, target).Scan(&r.TenantID, &r.SourceVMID, &r.TargetVMID, &r.CreateOperationID, &r.SourceNativeUID, &r.SourceVersion, &r.State, &r.ReleaseKind, &r.ReleaseReason, &r.ReleasedBy, &r.ReleaseEvidence, &releasedAt)
	if e != nil {
		return r, notFound(e)
	}
	r.ReleasedAt = releasedAt
	return r, nil
}

// SourceHeld reads the reference table by source. It is the read-only
// counterpart of the acceptance fence and never takes the HELD reference, so a
// source that is busy stays busy and a free source is not reserved by a mere
// resolve.
func (p *Postgres) SourceHeld(ctx context.Context, tenant, source string) (bool, error) {
	if !biz.UUID(tenant) || !biz.UUID(source) {
		return false, biz.ErrInvalid
	}
	var held bool
	e := p.pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM vm.clone_source_references WHERE tenant_id=$1 AND source_vm_id=$2 AND state='HELD')", tenant, source).Scan(&held)
	return held, e
}

// CloneCreateLeaseActive reports whether the create operation of a clone still
// holds a live worker lease. A manual release must not open the source while the
// original create can still send its clone submission: closing fences later
// ticks but cannot cancel a request that was already sent. The SQL lease column
// is the scheduling fact; the JSON record copy is stale because Save clears the
// column on every tick.
func (p *Postgres) CloneCreateLeaseActive(ctx context.Context, tenant, target, createOperation string) (bool, error) {
	if !biz.UUID(tenant) || !biz.UUID(target) || !biz.UUID(createOperation) {
		return false, biz.ErrInvalid
	}
	var active bool
	e := p.pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM vm.operations WHERE tenant_id=$1 AND vm_id=$2 AND id=$3 AND lease_until>now())", tenant, target, createOperation).Scan(&active)
	return active, e
}

var _ biz.CloneSourcePort = (*Postgres)(nil)
var _ biz.CloneSourceHeldPort = (*Postgres)(nil)
