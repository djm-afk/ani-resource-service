package instance

import (
	"context"

	networkbiz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
)

// maxConsumerBatch bounds one Network page's worth of identities. Network caps
// its own page size, so a larger slice means the caller is not a page reader.
const maxConsumerBatch = 100

// DescribeConsumers resolves container instance names and observed states by
// exact identity. List cannot serve this need: its id filter is a substring
// match, which would turn one attachment row into a set of unrelated instances.
//
// The tenant comes from Network's authenticated request; only rows of that
// tenant are ever returned, and an unknown id is absent rather than guessed.
func (p *Postgres) DescribeConsumers(ctx context.Context, tenant string, ids []string) (map[string]networkbiz.ConsumerInfo, error) {
	if _, e := networkbiz.ParseTenant(tenant); e != nil || len(ids) == 0 || len(ids) > maxConsumerBatch {
		return nil, networkbiz.Fail(networkbiz.DependencyUnavailable, "instance consumer batch is invalid")
	}
	rows, e := p.pool.Query(ctx, `SELECT i.instance_id::text,coalesce(i.snapshot->'Spec'->>'Name',''),coalesce(i.snapshot->>'Observed','')
FROM (SELECT unnest($2::text[])::uuid AS id) wanted
JOIN instance.instances i ON i.tenant_id=$1 AND i.instance_id=wanted.id`, tenant, ids)
	if e != nil {
		return nil, networkbiz.Fail(networkbiz.DependencyUnavailable, "instance consumer read failed")
	}
	defer rows.Close()
	out := map[string]networkbiz.ConsumerInfo{}
	for rows.Next() {
		var id, name, state string
		if e = rows.Scan(&id, &name, &state); e != nil {
			return nil, networkbiz.Fail(networkbiz.DependencyUnavailable, "instance consumer read failed")
		}
		out[id] = networkbiz.ConsumerInfo{ID: id, Name: name, State: state}
	}
	if e = rows.Err(); e != nil {
		return nil, networkbiz.Fail(networkbiz.DependencyUnavailable, "instance consumer read failed")
	}
	return out, nil
}
