package data_test

import (
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"testing"
	"time"
)

func TestIntranetAutomaticAllocationAtomicReplayAndSourceSwitch(t *testing.T) {
	f := newEgressFixture(t)
	first := f.platform(t, intranetIntent("auto-first", "10.232.254.0/24", "10.232.254.1"))
	if first.Verification != nil {
		t.Fatal("fixture must not supply manual evidence")
	}
	i := biz.PlatformIntent{Kind: "set_intranet_pool_auto_allocation", ID: first.ID, Enabled: true, ExpectedVersion: first.Version, IdempotencyKey: "auto-first-enable"}
	enabled, err := f.e.SetPool(f.ctx, i)
	if err != nil || !enabled.NewVPCsEnabled || !enabled.IsDefault || !enabled.AllocationEnabled {
		t.Fatal("enable must atomically configure all three settings without manual evidence", enabled, err)
	}
	replay, err := f.e.SetPool(f.ctx, i)
	if err != nil || replay.Version != enabled.Version || !replay.NewVPCsEnabled {
		t.Fatal("exact retry changed the receipt", replay, err)
	}
	i.Enabled = false
	if _, err = f.e.SetPool(f.ctx, i); err == nil {
		t.Fatal("idempotency key accepted a different command")
	}
	i.IdempotencyKey = "stale-auto-disable"
	if _, err = f.e.SetPool(f.ctx, i); err == nil {
		t.Fatal("stale pool version accepted")
	}
	second := f.platform(t, intranetIntent("auto-second", "10.233.254.0/24", "10.233.254.1"))
	second = setIntranetPool(t, f, biz.PlatformIntent{Kind: "set_default_intranet_pool", ID: second.ID, IdempotencyKey: "auto-switch"})
	if !second.NewVPCsEnabled || !second.IsDefault || !second.AllocationEnabled {
		t.Fatal("switch must retain enabled rollout and open the new source", second)
	}
	old, err := f.e.GetPlatform(f.ctx, "intranet_pool", first.ID)
	if err != nil || old.IsDefault || old.NewVPCsEnabled || old.ConfigRevision != first.ConfigRevision {
		t.Fatal("old source facts changed", old, err)
	}
	if _, err = f.e.SetPool(f.ctx, biz.PlatformIntent{Kind: "set_intranet_pool_auto_allocation", ID: old.ID, ExpectedVersion: old.Version, IdempotencyKey: "old-source-disable"}); err == nil {
		t.Fatal("old page disabled a different default source")
	}
	disabled := setIntranetPool(t, f, biz.PlatformIntent{Kind: "set_intranet_pool_auto_allocation", ID: second.ID, IdempotencyKey: "auto-disable"})
	if disabled.NewVPCsEnabled || !disabled.IsDefault || !disabled.AllocationEnabled {
		t.Fatal("disable should only close new VPC automation", disabled)
	}
	var enabledFlag bool
	if err = f.owner.QueryRow(f.ctx, `SELECT new_vpcs_enabled FROM network_connectivity_rollout WHERE cluster_id='test-cluster'`).Scan(&enabledFlag); err != nil || enabledFlag {
		t.Fatal("rollout was not disabled", enabledFlag, err)
	}
}

func TestIntranetAutomaticAllocationDoesNotGateOnReadAge(t *testing.T) {
	f := newEgressFixture(t)
	pool := f.platform(t, intranetIntent("auto-old-read", "10.232.254.0/24", "10.232.254.1"))
	if _, err := f.owner.Exec(f.ctx, `UPDATE network_platform_resources SET observed_at=$2 WHERE resource_id=$1`, pool.ID, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, err := f.e.SetPool(f.ctx, biz.PlatformIntent{Kind: "set_intranet_pool_auto_allocation", ID: pool.ID, Enabled: true, ExpectedVersion: pool.Version, IdempotencyKey: "auto-enable"})
	if err != nil || !got.AllocationEnabled || !got.IsDefault || !got.NewVPCsEnabled {
		t.Fatal(got, err)
	}
}
