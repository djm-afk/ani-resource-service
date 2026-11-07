//go:build instanceintegration

package vm

import (
	"context"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	"github.com/zhangzhe-ctrl/ani-resource-service/tests/testenv"
	"testing"
)

// The Network v3 management snapshot is a separate durable write before an
// operation can use it. Tests advance that publication without wall-clock sleeps;
// the real worker and all version/lease checks still execute on PostgreSQL.
func stepPastNetworkPublication(t *testing.T, ctx context.Context, db *testenv.Database, p *Postgres, w *biz.Worker, tenant, id string) (bool, error) {
	t.Helper()
	for range 4 {
		before, err := p.Get(ctx, tenant, id)
		if err != nil {
			return false, err
		}
		worked, err := w.Step(ctx)
		if err != nil || !worked {
			return worked, err
		}
		after, err := p.Get(ctx, tenant, id)
		if err != nil {
			return true, err
		}
		if before.Network.SnapshotRevision == after.Network.SnapshotRevision {
			return true, nil
		}
		if _, err := db.Owner.Exec(ctx, "UPDATE vm.operations SET next_attempt_at=now()-interval '1 second' WHERE tenant_id=$1 AND vm_id=$2 AND state IN ('accepted','running','unknown')", tenant, id); err != nil {
			return true, err
		}
	}
	t.Fatal("network snapshot did not converge")
	return false, nil
}
