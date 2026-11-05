package instance

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/instance"
)

// VolumeReferences 返回指定 placement 下仍受保护（pending/active/retiring）的
// 卷引用，供候选查询标记 PVC_IN_USE。released 引用视为空闲，不返回。
// 这是展示用投影；受理期的权威占用校验在受理事务内执行。
func (p *Postgres) VolumeReferences(ctx context.Context, tenant, cluster, namespace string) ([]biz.VolumeReference, error) {
	if !biz.UUID(tenant) || !biz.UUID(cluster) || namespace == "" {
		return nil, biz.ErrInvalid
	}
	rows, err := p.pool.Query(ctx, `SELECT pvc_name,pvc_uid,state FROM instance.borrowed_volume_references WHERE tenant_id=$1 AND cluster_id=$2 AND namespace=$3 AND state IN ('pending','active','retiring') ORDER BY pvc_name,mount_path`, tenant, cluster, namespace)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []biz.VolumeReference{}
	for rows.Next() {
		var r biz.VolumeReference
		if err = rows.Scan(&r.PVCName, &r.PVCUID, &r.State); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err = rows.Err(); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	return out, nil
}

var _ biz.VolumeUsageReader = (*Postgres)(nil)
