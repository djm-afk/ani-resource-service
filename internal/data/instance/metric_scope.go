package instance

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/instance"
)

// ReadMetricScope includes archived rows and projects only original identity
// and location. It never decodes a current or retired GPU execution schema.
func (p *Postgres) ReadMetricScope(ctx context.Context, tenant, id string) (biz.MetricScope, error) {
	if err := biz.RequireTenant(ctx, tenant); err != nil {
		return biz.MetricScope{}, err
	}
	var out biz.MetricScope
	err := p.pool.QueryRow(ctx, "SELECT tenant_id::text,instance_id::text,create_operation_id::text,"+
		"coalesce(snapshot #>> '{Spec,ClusterID}',''),coalesce(snapshot #>> '{Spec,Namespace}','')"+
		" FROM instance.instances WHERE tenant_id=$1 AND instance_id=$2", tenant, id).
		Scan(&out.TenantID, &out.ID, &out.CreateOperationID, &out.ClusterID, &out.Namespace)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, biz.ErrNotFound
	}
	return out, err
}
