package data_test

import (
	"context"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"strings"
	"testing"
	"time"
)

func (testEgressInfrastructure) ValidateIntranetPool(context.Context, biz.PublicPoolConfig) error {
	return nil
}
func intranetIntent(key, cidr, gw string) biz.PlatformIntent {
	return biz.PlatformIntent{Kind: "create_intranet_pool", Name: key, IdempotencyKey: key, Pool: &biz.PublicPoolConfig{CIDR: cidr, OVNGatewayIP: gw, DefaultVPCName: "kcn-cluster", DefaultVPCUID: "default-vpc-uid", IntranetNetworks: []string{"10.96.0.0/12"}}}
}
func setIntranetPool(t *testing.T, f *egressFixture, i biz.PlatformIntent) biz.PlatformResource {
	t.Helper()
	v, err := f.e.GetPlatform(f.ctx, "intranet_pool", i.ID)
	if err != nil {
		t.Fatal(err)
	}
	i.ExpectedVersion = v.Version
	v, err = f.e.SetPool(f.ctx, i)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func readyIntranetPool(t *testing.T, f *egressFixture, v biz.PlatformResource) biz.PlatformResource {
	t.Helper()
	ev := &biz.PublicPoolVerification{ProviderSourceRevision: strings.Repeat("a", 40), ProviderImageDigests: []string{"sha256:" + strings.Repeat("a", 64)}, TopologyFingerprint: strings.Repeat("c", 64), EvidenceReference: "controlled platform fixture only", Scope: biz.IntranetVerificationScope, VerifiedAt: time.Now().Add(-time.Second), ExpiresAt: time.Now().Add(time.Hour)}
	setIntranetPool(t, f, biz.PlatformIntent{Kind: "verify_intranet_pool", ID: v.ID, IdempotencyKey: "verify-" + v.ID, Verification: ev})
	return setIntranetPool(t, f, biz.PlatformIntent{Kind: "set_intranet_pool_allocation", ID: v.ID, IdempotencyKey: "open-" + v.ID, Enabled: true})
}
func TestIntranetPlatformScopesDefaultsAllocationAndIndependentCapabilities(t *testing.T) {
	f := newEgressFixture(t)
	public := f.eip(t, "public-before-intranet")
	caps, err := f.e.GetPlatformCapabilities(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !caps.PublicAddress.Ready || caps.BaseConnectivity.Ready || caps.LoadBalancer.Ready || caps.LoadBalancer.Reason != "CAPABILITY_UNKNOWN" {
		t.Fatal("capabilities were coupled", caps)
	}
	intent := intranetIntent("base-pool", "10.232.254.0/24", "10.232.254.1")
	first := f.platform(t, intent)
	replay, err := f.e.CreatePlatform(f.ctx, intent)
	if err != nil || replay.ID != first.ID {
		t.Fatal("create replay drift", replay, err)
	}
	if _, err := f.e.GetPlatform(f.ctx, "public_pool", first.ID); err == nil {
		t.Fatal("public GET exposed intranet pool")
	}
	rows, _, _, err := f.e.ListPlatform(f.ctx, "public_pool", biz.ListVPCs{})
	if err != nil || len(rows) != 1 || rows[0].ID != f.pool.ID {
		t.Fatal("public list scope", rows, err)
	}
	rows, _, _, err = f.e.ListPlatform(f.ctx, "intranet_pool", biz.ListVPCs{})
	if err != nil || len(rows) != 1 || rows[0].ID != first.ID {
		t.Fatal("intranet list scope", rows, err)
	}
	for _, kind := range []string{"set_default_pool", "set_pool_allocation"} {
		if _, err := f.e.SetPool(f.ctx, biz.PlatformIntent{Kind: kind, ID: first.ID, ExpectedVersion: first.Version, IdempotencyKey: "wrong-" + kind}); err == nil {
			t.Fatal("public operation changed intranet pool", kind)
		}
	}
	first = readyIntranetPool(t, f, first)
	first = setIntranetPool(t, f, biz.PlatformIntent{Kind: "set_default_intranet_pool", ID: first.ID, IdempotencyKey: "default-first"})
	caps, err = f.e.GetPlatformCapabilities(f.ctx)
	if err != nil || !caps.BaseConnectivity.Ready || !caps.PublicAddress.Ready || caps.LoadBalancer.Ready {
		t.Fatal(caps, err)
	}
	second := readyIntranetPool(t, f, f.platform(t, intranetIntent("second-base-pool", "10.233.254.0/24", "10.233.254.1")))
	setIntranetPool(t, f, biz.PlatformIntent{Kind: "set_default_intranet_pool", ID: second.ID, IdempotencyKey: "default-second"})
	firstAfter, err := f.e.GetPlatform(f.ctx, "intranet_pool", first.ID)
	if err != nil || firstAfter.IsDefault || firstAfter.ConfigRevision != first.ConfigRevision {
		t.Fatal("default switch mutated old pool", firstAfter, err)
	}
	setIntranetPool(t, f, biz.PlatformIntent{Kind: "set_intranet_pool_allocation", ID: second.ID, IdempotencyKey: "close-second", Enabled: false})
	caps, err = f.e.GetPlatformCapabilities(f.ctx)
	if err != nil || caps.BaseConnectivity.Ready || !caps.PublicAddress.Ready {
		t.Fatal("intranet allocation switch affected public", caps, err)
	}
	publicAfter, err := f.e.GetEIP(f.ctx, "", public.ID)
	if err != nil || publicAfter.ID != public.ID || publicAfter.Address != public.Address || publicAfter.State != public.State {
		t.Fatal("public allocation changed with intranet pool", publicAfter, err)
	}
	if _, err := f.e.DeletePlatform(f.ctx, "public_pool", second.ID); err == nil {
		t.Fatal("public delete accepted intranet pool")
	}
	if _, err := f.e.DeletePlatform(f.ctx, "intranet_pool", first.ID); err == nil {
		t.Fatal("open pool deleted")
	}
	if _, err := f.e.DeletePlatform(f.ctx, "intranet_pool", second.ID); err != nil {
		t.Fatal(err)
	}
	f.drive(t, func() bool {
		v, err := f.e.GetPlatform(f.ctx, "intranet_pool", second.ID)
		return err == nil && v.State == biz.Deleted
	})
}
