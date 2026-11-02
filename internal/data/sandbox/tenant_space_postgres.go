package sandbox

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/sandbox"
)

// The row lock orders close with every provider-write intent, including a
// close received before an open or after a lost provider response.
func (p *Postgres) OpenTenantSpace(ctx context.Context, tenant, operation string) (biz.TenantSpace, error) {
	return p.setTenantSpacePhase(ctx, tenant, operation, false)
}

func (p *Postgres) CloseTenantSpace(ctx context.Context, tenant, operation string) (biz.TenantSpace, error) {
	return p.setTenantSpacePhase(ctx, tenant, operation, true)
}

func (p *Postgres) setTenantSpacePhase(ctx context.Context, tenant, operation string, close bool) (biz.TenantSpace, error) {
	if err := biz.RequireTenant(ctx, tenant); err != nil {
		return biz.TenantSpace{}, err
	}
	if !biz.UUID(operation) {
		return biz.TenantSpace{}, biz.ErrInvalid
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return biz.TenantSpace{}, err
	}
	defer tx.Rollback(ctx)
	phase := "open"
	if close {
		phase = "closed"
	}
	// Advisory locking serializes the absent-row case, so a close received first
	// installs a permanent tombstone before a concurrent or delayed open.
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1::text,84015011))", tenant); err != nil {
		return biz.TenantSpace{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO sandbox.tenant_spaces(tenant_id,operation_id,phase) VALUES($1,$2,$3) ON CONFLICT (tenant_id) DO NOTHING`, tenant, operation, phase); err != nil {
		return biz.TenantSpace{}, err
	}
	space, err := readTenantSpace(ctx, tx, tenant, true)
	if err != nil {
		return biz.TenantSpace{}, err
	}
	if space.OperationID != operation {
		return biz.TenantSpace{}, biz.ErrConflict
	}
	if close {
		// A closed fence must not strand live or incompletely cleaned sandboxes.
		var active bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sandbox.sandboxes WHERE tenant_id=$1 AND (desired <> 'deleted' OR observed <> 'deleted' OR pending_mutation IS NOT NULL OR COALESCE(provider_refs->>'CleanupReceipt','')=''))`, tenant).Scan(&active); err != nil {
			return biz.TenantSpace{}, err
		}
		if active {
			return biz.TenantSpace{}, biz.ErrConflict
		}
		phase = "closed"
		if space.PendingID != "" {
			phase = "closing"
		}
		if _, err = tx.Exec(ctx, `UPDATE sandbox.tenant_spaces SET phase=$2,updated_at=statement_timestamp() WHERE tenant_id=$1`, tenant, phase); err != nil {
			return biz.TenantSpace{}, err
		}
		space.Phase = phase
	} else if space.Phase != "open" && space.Phase != "ready" {
		return biz.TenantSpace{}, biz.ErrConflict
	}
	if err = tx.Commit(ctx); err != nil {
		return biz.TenantSpace{}, err
	}
	return space, nil
}

type tenantSpaceQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func readTenantSpace(ctx context.Context, q tenantSpaceQuerier, tenant string, lock bool) (biz.TenantSpace, error) {
	query := `SELECT tenant_id::text,operation_id::text,phase,COALESCE(pending_id::text,''),COALESCE(pending_kind,''),COALESCE(namespace_uid,'') FROM sandbox.tenant_spaces WHERE tenant_id=$1`
	if lock {
		query += " FOR UPDATE"
	}
	var space biz.TenantSpace
	err := q.QueryRow(ctx, query, tenant).Scan(&space.TenantID, &space.OperationID, &space.Phase, &space.PendingID, &space.PendingKind, &space.NamespaceUID)
	if errors.Is(err, pgx.ErrNoRows) {
		return biz.TenantSpace{}, biz.ErrNotFound
	}
	return space, err
}

func (p *Postgres) GetTenantSpace(ctx context.Context, tenant string) (biz.TenantSpace, error) {
	if err := biz.RequireTenant(ctx, tenant); err != nil {
		return biz.TenantSpace{}, err
	}
	return readTenantSpace(ctx, p.pool, tenant, false)
}

// PinTenantSpaceNamespace fixes an already observed owned namespace before any
// role or binding write. A later same-name namespace is never adopted.
func (p *Postgres) PinTenantSpaceNamespace(ctx context.Context, tenant, operation, uid string) (biz.TenantSpace, error) {
	if err := biz.RequireTenant(ctx, tenant); err != nil {
		return biz.TenantSpace{}, err
	}
	if !biz.UUID(operation) || !validTenantSpaceNamespaceUID(uid) {
		return biz.TenantSpace{}, biz.ErrInvalid
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return biz.TenantSpace{}, err
	}
	defer tx.Rollback(ctx)
	space, err := readTenantSpace(ctx, tx, tenant, true)
	if err != nil {
		return biz.TenantSpace{}, err
	}
	if space.OperationID != operation || space.Phase != "open" || space.PendingID != "" {
		return biz.TenantSpace{}, biz.ErrConflict
	}
	if space.NamespaceUID != "" && space.NamespaceUID != uid {
		return biz.TenantSpace{}, biz.ErrOwnership
	}
	if space.NamespaceUID == "" {
		if _, err = tx.Exec(ctx, `UPDATE sandbox.tenant_spaces SET namespace_uid=$2,updated_at=statement_timestamp() WHERE tenant_id=$1`, tenant, uid); err != nil {
			return biz.TenantSpace{}, err
		}
		space.NamespaceUID = uid
	}
	if err = tx.Commit(ctx); err != nil {
		return biz.TenantSpace{}, err
	}
	return space, nil
}

func validTenantSpaceNamespaceUID(uid string) bool {
	return uid != "" && len(uid) <= 128 && strings.TrimSpace(uid) == uid
}

func validTenantSpaceMutationKind(kind string) bool {
	switch kind {
	case "namespace_create", "namespace_marker", "pull_secret_role", "pull_secret_binding", "tenant_role_binding":
		return true
	default:
		return false
	}
}

func (p *Postgres) BeginTenantSpaceMutation(ctx context.Context, tenant, operation, id, kind string) (biz.TenantSpaceMutation, error) {
	if err := biz.RequireTenant(ctx, tenant); err != nil {
		return biz.TenantSpaceMutation{}, err
	}
	if !biz.UUID(operation) || !biz.UUID(id) || !validTenantSpaceMutationKind(kind) {
		return biz.TenantSpaceMutation{}, biz.ErrInvalid
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return biz.TenantSpaceMutation{}, err
	}
	defer tx.Rollback(ctx)
	space, err := readTenantSpace(ctx, tx, tenant, true)
	if err != nil {
		return biz.TenantSpaceMutation{}, err
	}
	if space.OperationID != operation {
		return biz.TenantSpaceMutation{}, biz.ErrConflict
	}
	previous, err := readTenantSpaceMutation(ctx, tx, tenant, id)
	if err == nil {
		if previous.Kind != kind {
			return biz.TenantSpaceMutation{}, biz.ErrConflict
		}
		// A pending replay cannot distinguish a lost ACK from a late write.
		return previous, nil
	}
	if !errors.Is(err, biz.ErrNotFound) {
		return biz.TenantSpaceMutation{}, err
	}
	if space.Phase != "open" || space.PendingID != "" {
		return biz.TenantSpaceMutation{}, biz.ErrConflict
	}
	if (kind == "namespace_create" && space.NamespaceUID != "") || (kind != "namespace_create" && space.NamespaceUID == "") {
		return biz.TenantSpaceMutation{}, biz.ErrConflict
	}
	if _, err = tx.Exec(ctx, `INSERT INTO sandbox.tenant_space_mutations(tenant_id,mutation_id,kind,outcome) VALUES($1,$2,$3,'pending')`, tenant, id, kind); err != nil {
		return biz.TenantSpaceMutation{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE sandbox.tenant_spaces SET pending_id=$2,pending_kind=$3,updated_at=statement_timestamp() WHERE tenant_id=$1`, tenant, id, kind); err != nil {
		return biz.TenantSpaceMutation{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return biz.TenantSpaceMutation{}, err
	}
	return biz.TenantSpaceMutation{ID: id, Kind: kind, Outcome: "pending", Dispatch: true}, nil
}

func readTenantSpaceMutation(ctx context.Context, q tenantSpaceQuerier, tenant, id string) (biz.TenantSpaceMutation, error) {
	var m biz.TenantSpaceMutation
	err := q.QueryRow(ctx, `SELECT mutation_id::text,kind,outcome,COALESCE(receipt,'') FROM sandbox.tenant_space_mutations WHERE tenant_id=$1 AND mutation_id=$2`, tenant, id).Scan(&m.ID, &m.Kind, &m.Outcome, &m.Receipt)
	if errors.Is(err, pgx.ErrNoRows) {
		return biz.TenantSpaceMutation{}, biz.ErrNotFound
	}
	return m, err
}

func (p *Postgres) FinishTenantSpaceMutation(ctx context.Context, tenant, operation, id, outcome, receipt string) (biz.TenantSpaceMutation, error) {
	if err := biz.RequireTenant(ctx, tenant); err != nil {
		return biz.TenantSpaceMutation{}, err
	}
	if !biz.UUID(operation) || !biz.UUID(id) || (outcome != "applied" && outcome != "not_sent") || strings.TrimSpace(receipt) == "" || len(receipt) > 4096 {
		return biz.TenantSpaceMutation{}, biz.ErrInvalid
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return biz.TenantSpaceMutation{}, err
	}
	defer tx.Rollback(ctx)
	space, err := readTenantSpace(ctx, tx, tenant, true)
	if err != nil {
		return biz.TenantSpaceMutation{}, err
	}
	if space.OperationID != operation {
		return biz.TenantSpaceMutation{}, biz.ErrConflict
	}
	m, err := readTenantSpaceMutation(ctx, tx, tenant, id)
	if err != nil {
		return biz.TenantSpaceMutation{}, err
	}
	if m.Outcome != "pending" {
		if m.Outcome != outcome || m.Receipt != receipt {
			return biz.TenantSpaceMutation{}, biz.ErrConflict
		}
		return m, nil
	}
	if space.PendingID != id || space.PendingKind != m.Kind {
		return biz.TenantSpaceMutation{}, fmt.Errorf("%w: tenant space journal differs", biz.ErrConflict)
	}
	if m.Kind == "namespace_create" && outcome == "applied" {
		if !validTenantSpaceNamespaceUID(receipt) {
			return biz.TenantSpaceMutation{}, biz.ErrInvalid
		}
		if space.NamespaceUID != "" && space.NamespaceUID != receipt {
			return biz.TenantSpaceMutation{}, biz.ErrOwnership
		}
		if _, err = tx.Exec(ctx, `UPDATE sandbox.tenant_spaces SET namespace_uid=$2 WHERE tenant_id=$1`, tenant, receipt); err != nil {
			return biz.TenantSpaceMutation{}, err
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE sandbox.tenant_space_mutations SET outcome=$3,receipt=$4 WHERE tenant_id=$1 AND mutation_id=$2`, tenant, id, outcome, receipt); err != nil {
		return biz.TenantSpaceMutation{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE sandbox.tenant_spaces SET pending_id=NULL,pending_kind=NULL,phase=CASE WHEN phase='closing' THEN 'closed' ELSE phase END,updated_at=statement_timestamp() WHERE tenant_id=$1`, tenant); err != nil {
		return biz.TenantSpaceMutation{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return biz.TenantSpaceMutation{}, err
	}
	m.Outcome, m.Receipt = outcome, receipt
	return m, nil
}

var _ biz.TenantSpaceJournal = (*Postgres)(nil)

// ClaimTenantSpace selects a due open operation. Provider writes still need
// their own durable pending record; this scheduling delay is not a fence.
func (p *Postgres) ClaimTenantSpace(ctx context.Context) (biz.TenantSpace, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return biz.TenantSpace{}, err
	}
	defer tx.Rollback(ctx)
	var tenant string
	err = tx.QueryRow(ctx, `SELECT tenant_id::text FROM sandbox.tenant_spaces
		WHERE phase='open' AND pending_id IS NULL AND retry_after <= statement_timestamp()
		ORDER BY retry_after, tenant_id FOR UPDATE SKIP LOCKED LIMIT 1`).Scan(&tenant)
	if errors.Is(err, pgx.ErrNoRows) {
		return biz.TenantSpace{}, biz.ErrNotFound
	}
	if err != nil {
		return biz.TenantSpace{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE sandbox.tenant_spaces SET retry_after=statement_timestamp()+interval '30 seconds' WHERE tenant_id=$1`, tenant); err != nil {
		return biz.TenantSpace{}, err
	}
	space, err := readTenantSpace(ctx, tx, tenant, false)
	if err != nil {
		return biz.TenantSpace{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return biz.TenantSpace{}, err
	}
	return space, nil
}

// MarkTenantSpaceReady commits readiness only while the same open operation
// still owns the verified namespace and no provider write is in flight.
func (p *Postgres) MarkTenantSpaceReady(ctx context.Context, tenant, operation, uid string) error {
	if err := biz.RequireTenantSpaceController(ctx, tenant); err != nil {
		return err
	}
	if !biz.UUID(operation) || !validTenantSpaceNamespaceUID(uid) {
		return biz.ErrInvalid
	}
	result, err := p.pool.Exec(ctx, `UPDATE sandbox.tenant_spaces SET phase='ready',updated_at=statement_timestamp()
		WHERE tenant_id=$1 AND operation_id=$2 AND phase='open' AND pending_id IS NULL AND namespace_uid=$3`, tenant, operation, uid)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return biz.ErrConflict
	}
	return nil
}
