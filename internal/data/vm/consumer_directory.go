package vm

import (
	"context"

	networkbiz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
)

// maxConsumerBatch bounds one Network page's worth of identities, matching the
// page cap Network applies before it calls this reader.
const maxConsumerBatch = 100

// DescribeConsumers resolves virtual machine names and display states by exact
// identity. The state comes from the same vmDisplayState expression the VM list
// uses, so a machine cannot be "running" in one page and "starting" in another.
//
// The table is deliberately unaliased: vmDisplayState correlates on
// virtual_machines, and an alias would make that expression invalid.
func (p *Postgres) DescribeConsumers(ctx context.Context, tenant string, ids []string) (map[string]networkbiz.ConsumerInfo, error) {
	if _, e := networkbiz.ParseTenant(tenant); e != nil || len(ids) == 0 || len(ids) > maxConsumerBatch {
		return nil, networkbiz.Fail(networkbiz.DependencyUnavailable, "virtual machine consumer batch is invalid")
	}
	rows, e := p.pool.Query(ctx, `SELECT vm.virtual_machines.id::text,coalesce(vm.virtual_machines.current_configuration->>'Name',''),(`+vmDisplayState+`)
FROM (SELECT unnest($2::text[])::uuid AS id) wanted
JOIN vm.virtual_machines ON vm.virtual_machines.tenant_id=$1 AND vm.virtual_machines.id=wanted.id`, tenant, ids)
	if e != nil {
		return nil, networkbiz.Fail(networkbiz.DependencyUnavailable, "virtual machine consumer read failed")
	}
	defer rows.Close()
	out := map[string]networkbiz.ConsumerInfo{}
	for rows.Next() {
		var id, name, state string
		if e = rows.Scan(&id, &name, &state); e != nil {
			return nil, networkbiz.Fail(networkbiz.DependencyUnavailable, "virtual machine consumer read failed")
		}
		out[id] = networkbiz.ConsumerInfo{ID: id, Name: name, State: state}
	}
	if e = rows.Err(); e != nil {
		return nil, networkbiz.Fail(networkbiz.DependencyUnavailable, "virtual machine consumer read failed")
	}
	return out, nil
}
