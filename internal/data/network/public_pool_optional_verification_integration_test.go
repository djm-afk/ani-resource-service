package data_test

import (
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"testing"
)

func TestPublicPoolAllocationWithoutManualVerification(t *testing.T) {
	f := newEgressFixtureWithPublicVerification(t, false)
	pool, err := f.e.GetPlatform(f.ctx, "public_pool", f.pool.ID)
	if err != nil || pool.Verification != nil || !pool.AllocationEnabled || !pool.IsDefault {
		t.Fatalf("unverified public pool did not open: %v %v", pool, err)
	}
	caps, err := f.e.GetPlatformCapabilities(f.ctx)
	if err != nil || !caps.PublicAddress.Ready {
		t.Fatalf("unverified default public pool unavailable: %v %v", caps, err)
	}
	eip := f.eip(t, "no-manual-egress-evidence")
	if eip.State != biz.Available || eip.Address == "" {
		t.Fatalf("EIP allocation failed: %v", eip)
	}
	f.setPool(t, biz.PlatformIntent{Kind: "set_pool_allocation", ID: pool.ID, Enabled: false, IdempotencyKey: "close-without-evidence"})
	if _, err = f.e.CreateEIP(f.ctx, biz.EgressIntent{Name: "closed", IdempotencyKey: "closed-no-evidence"}); err == nil {
		t.Fatal("closed pool accepted a new EIP")
	}
	intranet := f.platform(t, intranetIntent("intranet-without-manual-verification", "10.232.254.0/24", "10.232.254.1"))
	opened, err := f.e.SetPool(f.ctx, biz.PlatformIntent{Kind: "set_intranet_pool_allocation", ID: intranet.ID, ExpectedVersion: intranet.Version, Enabled: true, IdempotencyKey: "open-intranet-without-evidence"})
	if err != nil || !opened.AllocationEnabled || opened.Verification != nil {
		t.Fatalf("ready intranet pool still requires manual evidence: %v %v", opened, err)
	}
	caps, err = f.e.GetPlatformCapabilities(f.ctx)
	if err != nil || caps.PublicAddress.Ready {
		t.Fatalf("opening intranet pool reopened closed public allocation: %v %v", caps, err)
	}
}
