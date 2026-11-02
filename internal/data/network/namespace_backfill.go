package data

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/jackc/pgx/v5"

	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/tenantnamespace"
)

type NamespaceUIDBackfillRow struct {
	TenantID, ClusterID, Namespace string
	OriginalUID                    *string
	LiveUID                        string
}
type NamespaceUIDBackfillPlan struct {
	ClusterID string
	Rows      []NamespaceUIDBackfillRow
	NullCount int
	SHA256    string
}

// PlanNamespaceUIDBackfill is read-only. Only already registered rows in the
// configured cluster are considered; live objects must match the standard
// tenant identity. Existing non-NULL UIDs are compared, never replaced.
func (p *Postgres) PlanNamespaceUIDBackfill(ctx context.Context) (NamespaceUIDBackfillPlan, error) {
	plan := NamespaceUIDBackfillPlan{ClusterID: p.placement.ClusterID, Rows: []NamespaceUIDBackfillRow{}}
	rows, err := p.queries.ListTenantNamespaceIdentities(ctx)
	if err != nil {
		return plan, databaseFailure(err)
	}
	for _, row := range rows {
		if row.ClusterID != p.placement.ClusterID {
			continue
		}
		live, err := p.ResolveTenantNamespace(ctx, row.TenantID)
		if err != nil {
			return plan, err
		}
		if live.Name != row.Namespace {
			return plan, fmt.Errorf("registered namespace differs for tenant %s", row.TenantID)
		}
		if row.NamespaceUid != nil && *row.NamespaceUid != live.UID {
			return plan, fmt.Errorf("original namespace UID differs for tenant %s", row.TenantID)
		}
		if row.NamespaceUid == nil {
			plan.NullCount++
		}
		plan.Rows = append(plan.Rows, NamespaceUIDBackfillRow{TenantID: row.TenantID, ClusterID: row.ClusterID, Namespace: row.Namespace, OriginalUID: row.NamespaceUid, LiveUID: live.UID})
	}
	body, err := json.Marshal(plan)
	if err != nil {
		return plan, err
	}
	sum := sha256.Sum256(body)
	plan.SHA256 = hex.EncodeToString(sum[:])
	return plan, nil
}

// ApplyNamespaceUIDBackfill re-reads the complete reviewed plan, then updates
// exact NULL rows in one transaction. A race or identity drift rolls it back.
func (p *Postgres) ApplyNamespaceUIDBackfill(ctx context.Context, reviewedSHA256 string) (NamespaceUIDBackfillPlan, error) {
	return p.applyNamespaceUIDBackfill(ctx, reviewedSHA256, nil)
}

// ApplyNamespaceUIDBackfillGuarded is the maintenance CLI entry. The temporary
// non-NULL constraint and exact reviewed two-row manifest are required; its
// transaction also prevents concurrent identity inserts or DDL during updates.
func (p *Postgres) ApplyNamespaceUIDBackfillGuarded(ctx context.Context, reviewedSHA256 string, window NamespaceUIDWindowInput) (NamespaceUIDBackfillPlan, error) {
	if err := namespaceUIDWindowInput(window); err != nil {
		return NamespaceUIDBackfillPlan{}, err
	}
	if window.Stage != "require-uid" || window.ClusterID != p.placement.ClusterID {
		return NamespaceUIDBackfillPlan{}, fmt.Errorf("require-uid window for this cluster required")
	}
	return p.applyNamespaceUIDBackfill(ctx, reviewedSHA256, &window)
}

func (p *Postgres) applyNamespaceUIDBackfill(ctx context.Context, reviewedSHA256 string, window *NamespaceUIDWindowInput) (NamespaceUIDBackfillPlan, error) {
	plan, err := p.PlanNamespaceUIDBackfill(ctx)
	if err != nil {
		return plan, err
	}
	if reviewedSHA256 == "" || reviewedSHA256 != plan.SHA256 {
		return plan, fmt.Errorf("namespace UID plan digest differs")
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return plan, databaseFailure(err)
	}
	defer tx.Rollback(ctx)
	q := p.queries.WithTx(tx)
	if window != nil {
		if _, err = tx.Exec(ctx, `SET LOCAL lock_timeout='5s';SET LOCAL statement_timeout='15s';LOCK TABLE public.network_tenant_namespaces IN SHARE ROW EXCLUSIVE MODE`); err != nil {
			return plan, err
		}
		if err = namespaceUIDBackfillGuard(ctx, tx, *window); err != nil {
			return plan, err
		}
		registered, err := q.ListTenantNamespaceIdentities(ctx)
		if err != nil {
			return plan, err
		}
		index := 0
		for _, row := range registered {
			if row.ClusterID != window.ClusterID {
				continue
			}
			if index >= len(window.Rows) || index >= len(plan.Rows) {
				return plan, fmt.Errorf("registered Namespace set changed")
			}
			want := window.Rows[index]
			planned := plan.Rows[index]
			if row.TenantID != want.TenantID || row.Namespace != want.Namespace || planned.LiveUID != want.UID || row.NamespaceUid != nil && *row.NamespaceUid != want.UID || (row.NamespaceUid == nil) != (planned.OriginalUID == nil) {
				return plan, fmt.Errorf("reviewed Namespace identity changed")
			}
			index++
		}
		if index != len(window.Rows) || index != len(plan.Rows) {
			return plan, fmt.Errorf("exact Namespace row count changed")
		}
	}
	for _, row := range plan.Rows {
		if row.OriginalUID != nil {
			continue
		}
		live, err := p.ResolveTenantNamespace(ctx, row.TenantID)
		if err != nil {
			return plan, err
		}
		if live.Name != row.Namespace || live.UID != row.LiveUID {
			return plan, tenantnamespace.ErrUIDChanged
		}
		changed, err := q.BackfillTenantNamespaceUID(ctx, sqlcgen.BackfillTenantNamespaceUIDParams{TenantID: row.TenantID, ClusterID: row.ClusterID, Namespace: row.Namespace, NamespaceUid: row.LiveUID})
		if err != nil {
			return plan, databaseFailure(err)
		}
		if changed != 1 {
			return plan, fmt.Errorf("namespace UID row changed during backfill for tenant %s", row.TenantID)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return plan, databaseFailure(err)
	}
	return p.PlanNamespaceUIDBackfill(ctx)
}

func namespaceUIDBackfillGuard(ctx context.Context, tx pgx.Tx, in NamespaceUIDWindowInput) error {
	var database, user string
	if err := tx.QueryRow(ctx, "SELECT current_database(),current_user").Scan(&database, &user); err != nil {
		return err
	}
	if database != in.Database || user != in.RuntimeRole {
		return fmt.Errorf("original backfill database/runtime identity differs")
	}
	var expression, comment string
	var validated bool
	if err := tx.QueryRow(ctx, `SELECT pg_get_expr(conbin,conrelid),convalidated,coalesce(obj_description(oid,'pg_constraint'),'') FROM pg_constraint WHERE conrelid='public.network_tenant_namespaces'::regclass AND conname=$1 AND contype='c'`, NamespaceUIDWindowConstraint).Scan(&expression, &validated, &comment); err != nil {
		return fmt.Errorf("original require-UID window constraint missing")
	}
	if validated || comment != "ani.resource.namespace-uid-window.v1:"+in.WindowID || !namespaceUIDWindowExpressionMatches(expression, in, true) {
		return fmt.Errorf("original require-UID window constraint differs")
	}
	return nil
}
