//go:build instanceintegration

package vm

import (
	"context"
	"errors"
	"testing"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
)

func TestVMCurrentSnapshotNotificationRetriesAfterOperationCompletion(t *testing.T) {
	p, _, v := observationRegisteredVM(t)
	ctx := context.Background()
	failure := errors.New("Network unavailable")
	p.UseNetworkNotifier(func(context.Context, string, string) error { return failure })
	if worked, err := p.DispatchNetworkNotification(ctx); !worked || !errors.Is(err, failure) {
		t.Fatal(worked, err)
	}
	current, err := p.Get(ctx, v.TenantID, v.ID)
	if err != nil || current.Network.NotifiedSnapshotRevision != 0 || current.Observed != v.Observed {
		t.Fatal(current, err)
	}
	p.UseNetworkNotifier(func(call context.Context, tenant, attachment string) error {
		_, err := p.pool.Exec(call, `UPDATE vm.virtual_machines SET current_configuration=jsonb_set(current_configuration,'{Network,SnapshotRevision}','2'::jsonb) WHERE tenant_id=$1 AND id=$2`, tenant, v.ID)
		return err
	})
	if worked, err := p.DispatchNetworkNotification(ctx); !worked || err != nil {
		t.Fatal(worked, err)
	}
	current, err = p.Get(ctx, v.TenantID, v.ID)
	if err != nil || current.Network.SnapshotRevision != 2 || current.Network.NotifiedSnapshotRevision != 0 {
		t.Fatal("old delivery acknowledged a newer snapshot", current.Network, err)
	}
	p.UseNetworkNotifier(func(context.Context, string, string) error { return nil })
	if worked, err := p.DispatchNetworkNotification(ctx); !worked || err != nil {
		t.Fatal(worked, err)
	}
	current, err = p.Get(ctx, v.TenantID, v.ID)
	if err != nil || current.Network.NotifiedSnapshotRevision != 2 {
		t.Fatal(current.Network, err)
	}
	if worked, err := p.DispatchNetworkNotification(ctx); worked || err != nil {
		t.Fatal("acknowledged snapshot was resent", worked, err)
	}
}

func TestVMClosedSnapshotNotificationDoesNotRequireNativeObservation(t *testing.T) {
	p, _, v := observationRegisteredVM(t)
	ctx := context.Background()
	v.Closing, v.Observed = true, "Deleted"
	v.Network.Consumer = &biz.NetworkConsumerState{State: "closed", DesiredRunState: "deleted", FinalizationID: "closed-management"}
	if _, err := p.pool.Exec(ctx, `UPDATE vm.virtual_machines SET closing=true,current_configuration=$3 WHERE tenant_id=$1 AND id=$2`, v.TenantID, v.ID, encode(v)); err != nil {
		t.Fatal(err)
	}
	var calls int
	p.UseNetworkNotifier(func(context.Context, string, string) error { calls++; return nil })
	if worked, err := p.DispatchNetworkNotification(ctx); !worked || err != nil || calls != 1 {
		t.Fatal(worked, err, calls)
	}
}
