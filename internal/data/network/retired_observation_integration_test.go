package data_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/tenantnamespace"
)

func TestConfirmedDeleteRetiresTimerAndIgnoresLaterEvents(t *testing.T) {
	p, db := database(t)
	n := newNetwork(t, p, time.Minute)
	provider := &memoryProvider{}
	policy := biz.DefaultWorkerPolicy()
	policy.ObserveEvery, policy.RetryMin, policy.RetryMax = time.Millisecond, time.Millisecond, 2*time.Millisecond
	w, err := biz.NewWorker(p, provider, uuid.NewString(), policy)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tenant := uuid.NewString()
	identity := tenantnamespace.Identity{TenantID: tenant, ClusterID: "test-cluster", Name: "tenant-" + tenant, UID: uuid.NewString()}
	p.ConfigureTenantNamespaceResolver(func(context.Context, string) (tenantnamespace.Identity, error) { return identity, nil })
	v := availableVPC(t, n, w, tenant, "retired")
	original := provider.objects[v.ID]
	if _, err = n.DeleteVPC(ctx, v.TenantID, v.ID); err != nil {
		t.Fatal(err)
	}
	awaitNET05A(t, time.Second, func() bool {
		if _, err := w.Step(ctx); err != nil {
			t.Fatal(err)
		}
		current, err := n.GetVPC(ctx, v.TenantID, v.ID)
		return err == nil && current.State == biz.Deleted
	})
	var before, after int64
	var parked bool
	if err := db.QueryRow(ctx, "SELECT lease_epoch,next_run_at='infinity'::timestamptz FROM network_reconciliations WHERE tenant_id=$1 AND vpc_id=$2", v.TenantID, v.ID).Scan(&before, &parked); err != nil || !parked {
		t.Fatal("completed cleanup did not retire its timer", parked, err)
	}
	for range 4 {
		if found, err := w.Step(ctx); err != nil || found {
			t.Fatal("terminal timer was still claimable", found, err)
		}
	}
	// Later events cannot revive a completed lifetime or mutate a replacement.
	provider.objects[v.ID] = original
	if err := p.NotifyResource(ctx, v.TenantID, v.ID); err != nil {
		t.Fatal(err)
	}
	if found, err := w.Step(ctx); err != nil || found {
		t.Fatal("retired task was revived", found, err)
	}
	if err := db.QueryRow(ctx, "SELECT lease_epoch,next_run_at='infinity'::timestamptz FROM network_reconciliations WHERE tenant_id=$1 AND vpc_id=$2", v.TenantID, v.ID).Scan(&after, &parked); err != nil || after != before || !parked || !provider.objects[v.ID].Exists {
		t.Fatal("terminal notification changed original task or external object", after, parked, err)
	}
}
