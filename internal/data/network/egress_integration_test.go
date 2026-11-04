package data_test

import (
	"context"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/tenantnamespace"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// Test dependency only. All acceptance, leases, constraints and histories below
// use PostgreSQL and the actual shared Worker. No runtime in-memory backend.
type egressProvider struct {
	lostUpdate bool
	updates    int
	memoryProvider
	creates int
	failure error
	images  []string
}

func (p *egressProvider) Observe(ctx context.Context, t biz.ProviderTarget) (biz.ProviderObservation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failure != nil {
		return biz.ProviderObservation{}, p.failure
	}
	v := p.objects[t.ResourceID]
	if v.Exists && t.Kind == "snat" && t.Egress != nil && v.Egress != nil {
		v.NeedsUpdate = v.Egress.AppliedEnabled == nil || *v.Egress.AppliedEnabled != t.Egress.DesiredEnabled
		v.Ready = egressReportedReady(!v.NeedsUpdate)
	}
	return v, nil
}
func (p *egressProvider) EnsureEgress(ctx context.Context, t biz.ProviderTarget) (biz.ProviderObservation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.objects == nil {
		p.objects = map[string]biz.ProviderObservation{}
	}
	v, exists := p.objects[t.ResourceID]
	if !exists {
		p.creates++
		v = biz.ProviderObservation{Exists: true, Ready: egressReportedReady(true), Identity: uuid.NewString(), Egress: &biz.EgressAppliedFacts{}}
		switch t.Kind {
		case "eip":
			v.Egress.Address = fmt.Sprintf("192.0.2.%d", p.creates+10)
		case "snat":
			enabled := t.Egress.DesiredEnabled
			v.Egress.AppliedEnabled = &enabled
		}
		p.objects[t.ResourceID] = v
	}
	return v, nil
}
func (p *egressProvider) UpdateEgress(ctx context.Context, t biz.ProviderTarget) (biz.ProviderObservation, error) {
	p.mu.Lock()
	v := p.objects[t.ResourceID]
	if !v.Exists || v.Identity != t.KnownIdentity {
		p.mu.Unlock()
		return v, &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	enabled := t.Egress.DesiredEnabled
	v.Egress = &biz.EgressAppliedFacts{AppliedEnabled: &enabled}
	p.updates++
	v.Ready = egressReportedReady(true)
	v.NeedsUpdate = false
	p.objects[t.ResourceID] = v
	lost := p.lostUpdate
	p.lostUpdate = false
	p.mu.Unlock()
	if lost {
		return biz.ProviderObservation{}, &biz.ProviderError{Kind: biz.ProviderUncertain}
	}
	return p.Observe(ctx, t)
}

type testEgressInfrastructure struct{}

func (testEgressInfrastructure) GetIntranetPoolDefaults(context.Context) (biz.PublicPoolConfig, error) {
	return biz.PublicPoolConfig{DefaultVPCName: "kcn-cluster", DefaultVPCUID: "fixture-default"}, nil
}

type egressFixture struct {
	stopObservation func()
	p               *data.Postgres
	owner           *pgxpool.Pool
	e               *biz.Egress
	n               *biz.Network
	provider        *egressProvider
	w               *biz.Worker
	ctx             context.Context
	tenant          string
	pool            biz.PlatformResource
}

func egressWorker(t *testing.T, p *data.Postgres, provider *egressProvider) *biz.Worker {
	t.Helper()
	policy := biz.DefaultWorkerPolicy()
	policy.ObserveEvery = 100 * time.Millisecond
	policy.RetryMin = time.Millisecond
	policy.RetryMax = 10 * time.Millisecond
	w, err := biz.NewWorker(p, provider, uuid.NewString(), policy)
	if err != nil {
		t.Fatal(err)
	}
	return w
}
func (f *egressFixture) drive(t *testing.T, fn func() bool) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if _, err := f.w.Step(f.ctx); err != nil {
			t.Fatal(err)
		}
		if fn() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	for _, table := range []string{"network_platform_resources", "network_eips", "network_snat_bindings"} {
		var records string
		if err := f.owner.QueryRow(f.ctx, "SELECT coalesce(jsonb_agg(to_jsonb(x)),'[]')::text FROM (SELECT state,reason,version,last_operation_id FROM "+table+" LIMIT 8) x").Scan(&records); err == nil {
			t.Log(table, records)
		}
	}
	t.Fatal("egress worker did not converge")
}
func (f *egressFixture) platform(t *testing.T, i biz.PlatformIntent) biz.PlatformResource {
	t.Helper()
	v, err := f.e.CreatePlatform(f.ctx, i)
	if err != nil {
		t.Fatalf("create platform: %v; cause: %v", err, errors.Unwrap(err))
	}
	f.drive(t, func() bool {
		v, err = f.e.GetPlatform(f.ctx, v.Kind, v.ID)
		if err != nil {
			t.Fatal(err)
		}
		return v.State == biz.Available
	})
	return v
}
func (f *egressFixture) setPool(t *testing.T, i biz.PlatformIntent) biz.PlatformResource {
	t.Helper()
	v, err := f.e.GetPlatform(f.ctx, "public_pool", i.ID)
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
func newEgressFixture(t *testing.T) *egressFixture {
	return newEgressFixtureWithPublicVerification(t, true)
}

func newEgressFixtureWithPublicVerification(t *testing.T, verify bool) *egressFixture {
	t.Helper()
	p, owner := database(t)
	infra := testEgressInfrastructure{}
	e, err := biz.NewEgress(p, infra, biz.ContextEgressAuthorization{}, []byte("0123456789abcdef0123456789abcdef"), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	f := &egressFixture{p: p, owner: owner, e: e, n: newNetwork(t, p, time.Minute), tenant: uuid.NewString(), provider: &egressProvider{images: []string{"sha256:" + strings.Repeat("a", 64)}}}
	// The external platform has already provisioned this one tenant Namespace.
	// Admission still persists and checks its original identity in PostgreSQL.
	identity := tenantnamespace.Identity{TenantID: f.tenant, ClusterID: "test-cluster", Name: "tenant-" + f.tenant, UID: uuid.NewString()}
	p.ConfigureTenantNamespaceResolver(func(_ context.Context, tenant string) (tenantnamespace.Identity, error) {
		if tenant != identity.TenantID {
			return tenantnamespace.Identity{}, errors.New("unknown fixture tenant")
		}
		return identity, nil
	})
	f.w = egressWorker(t, p, f.provider)
	f.ctx = biz.WithEgressCaller(context.Background(), biz.EgressCaller{TenantID: f.tenant, PlatformAdministrator: true, Attribution: biz.Attribution{Actor: "controlled-fixture", DirectCaller: "egress-test"}})
	gateway := f.platform(t, biz.PlatformIntent{Kind: "create_egress_gateway", Name: "fixture-gateway", IdempotencyKey: "gateway"})
	f.pool = f.platform(t, biz.PlatformIntent{Kind: "create_public_pool", Name: "fixture-pool", IdempotencyKey: "pool", Pool: &biz.PublicPoolConfig{Mode: "overlay", GatewayID: gateway.ID, CIDR: "192.0.2.0/24", OVNGatewayIP: "192.0.2.1"}})
	if verify {
		evidence := &biz.PublicPoolVerification{ProviderSourceRevision: strings.Repeat("a", 40), ProviderImageDigests: f.provider.images, TopologyFingerprint: strings.Repeat("c", 64), EvidenceReference: "controlled unit provider only, no live data plane claim", Scope: "automated repository test", VerifiedAt: time.Now().Add(-time.Second), ExpiresAt: time.Now().Add(time.Hour)}
		f.setPool(t, biz.PlatformIntent{Kind: "verify_public_pool", ID: f.pool.ID, IdempotencyKey: "verify", Verification: evidence})
	}
	f.setPool(t, biz.PlatformIntent{Kind: "set_pool_allocation", ID: f.pool.ID, Enabled: true, IdempotencyKey: "open"})
	f.setPool(t, biz.PlatformIntent{Kind: "set_default_pool", ID: f.pool.ID, IdempotencyKey: "default"})
	return f
}
func (f *egressFixture) eip(t *testing.T, key string) biz.EIP {
	t.Helper()
	v, err := f.e.CreateEIP(f.ctx, biz.EgressIntent{Name: key, IdempotencyKey: key})
	if err != nil {
		t.Fatal(err)
	}
	f.drive(t, func() bool {
		v, err = f.e.GetEIP(f.ctx, "", v.ID)
		if err != nil {
			t.Fatal(err)
		}
		return v.State == biz.Available
	})
	return v
}
func (f *egressFixture) binding(t *testing.T, id string, state biz.ResourceState, applied *bool) biz.VPCSnatBinding {
	t.Helper()
	var v biz.VPCSnatBinding
	f.drive(t, func() bool {
		var err error
		v, err = f.e.GetVPCSnat(f.ctx, "", id, false)
		if err != nil {
			t.Fatal(err)
		}
		return v.State == state && (applied == nil || (v.AppliedEnabled != nil && *v.AppliedEnabled == *applied))
	})
	return v
}
func TestEgressPostgresLifecycleIsolationAndPermanentReplay(t *testing.T) {
	f := newEgressFixture(t)
	vpc := availableEgressVPC(t, f, "vpc")
	eip := f.eip(t, "address")
	if _, err := f.e.CreateEIP(context.Background(), biz.EgressIntent{TenantID: f.tenant, Name: "untrusted", IdempotencyKey: "bad"}); biz.ReasonOf(err) != biz.PermissionDenied {
		t.Fatal("untrusted explicit tenant accepted", err)
	}
	other := uuid.NewString()
	foreign := biz.WithEgressCaller(context.Background(), biz.EgressCaller{TenantID: other})
	if _, err := f.e.GetEIP(foreign, "", eip.ID); biz.ReasonOf(err) != biz.ResourceNotFound {
		t.Fatal("cross tenant read", err)
	}
	if _, err := f.e.GetEIP(f.ctx, other, eip.ID); biz.ReasonOf(err) != biz.PermissionDenied {
		t.Fatal("admin implied delegation", err)
	}
	delegated := biz.WithEgressCaller(context.Background(), biz.EgressCaller{TenantID: other, DelegatedTenants: []string{f.tenant}, Attribution: biz.Attribution{Actor: "delegated", DirectCaller: "fixture"}})
	if v, err := f.e.GetEIP(delegated, f.tenant, eip.ID); err != nil || v.ID != eip.ID {
		t.Fatal("delegation did not resolve target", err)
	}
	f.setPool(t, biz.PlatformIntent{Kind: "set_pool_allocation", ID: f.pool.ID, IdempotencyKey: "close", Enabled: false})
	if _, err := f.e.CreateEIP(f.ctx, biz.EgressIntent{Name: "closed", IdempotencyKey: "closed"}); biz.ReasonOf(err) != biz.PublicEgressNotReady {
		t.Fatal("closed pool allocated", err)
	}
	replay, err := f.e.CreateEIP(f.ctx, biz.EgressIntent{Name: "address", IdempotencyKey: "address"})
	if err != nil || replay.ID != eip.ID || replay.State != biz.Provisioning {
		t.Fatal("permanent receipt consulted dynamic readiness", err)
	}
	var poolID string
	if err = f.owner.QueryRow(f.ctx, "SELECT pool_id FROM network_eips WHERE tenant_id=$1 AND eip_id=$2", f.tenant, eip.ID).Scan(&poolID); err != nil || poolID != f.pool.ID {
		t.Fatal("pool not pinned", err)
	}
	binding, err := f.e.BindVPCSnat(f.ctx, biz.EgressIntent{Name: "binding", VPCID: vpc.ID, EIPID: eip.ID, IdempotencyKey: "bind"})
	if err != nil {
		t.Fatal("closed allocation blocked retained address", err)
	}
	enabled := true
	binding = f.binding(t, binding.ID, biz.Available, &enabled)
	for _, stmt := range []string{"UPDATE network_snat_bindings SET tenant_id=$1 WHERE snat_id=$2", "UPDATE network_snat_bindings SET namespace=$1 WHERE snat_id=$2"} {
		_, err = f.owner.Exec(f.ctx, stmt, other, binding.ID)
		var pg *pgconn.PgError
		if !errors.As(err, &pg) || pg.Code != "23503" {
			t.Fatalf("composite FK failed to protect binding: %v", err)
		}
	}
	if _, err = f.e.DeleteEIP(f.ctx, "", eip.ID); biz.ReasonOf(err) != biz.EIPInUse {
		t.Fatal("release ignored binding", err)
	}
	if _, err = f.n.DeleteVPC(f.ctx, f.tenant, vpc.ID); biz.ReasonOf(err) != biz.VPCSnatExists {
		t.Fatal("VPC delete ignored binding", err)
	}
	_, err = f.e.SetVPCSnatEnabled(f.ctx, biz.EgressIntent{ID: binding.ID, Enabled: false, ExpectedVersion: binding.Version, IdempotencyKey: "disable"})
	if err != nil {
		t.Fatal(err)
	}
	enabled = false
	binding = f.binding(t, binding.ID, biz.Available, &enabled)
	if binding.DesiredEnabled || binding.EIPID != eip.ID {
		t.Fatal("disable changed occupancy")
	}
	f.w = egressWorker(t, f.p, f.provider) // fresh process identity, same durable data/dependency
	_, err = f.e.SetVPCSnatEnabled(f.ctx, biz.EgressIntent{ID: binding.ID, Enabled: true, ExpectedVersion: binding.Version, IdempotencyKey: "enable"})
	if err != nil {
		t.Fatal(err)
	}
	enabled = true
	binding = f.binding(t, binding.ID, biz.Available, &enabled)
	if _, err = f.owner.Exec(f.ctx, "UPDATE network_snat_bindings SET observed_at=clock_timestamp()-interval '2 minutes' WHERE tenant_id=$1 AND snat_id=$2", f.tenant, binding.ID); err != nil {
		t.Fatal(err)
	}
	stale, err := f.e.GetVPCSnat(f.ctx, "", binding.ID, false)
	if err != nil || stale.ObservationStale || stale.AppliedEnabled == nil || !*stale.AppliedEnabled {
		t.Fatal("read age changed the last reported applied fact", stale, err)
	}
	if _, err = f.e.DeleteVPCSnatBinding(f.ctx, "", binding.ID); err != nil {
		t.Fatal(err)
	}
	f.binding(t, binding.ID, biz.Deleted, nil)
	newer, err := f.e.BindVPCSnat(f.ctx, biz.EgressIntent{Name: "replacement", VPCID: vpc.ID, EIPID: eip.ID, IdempotencyKey: "replacement"})
	if err != nil {
		t.Fatal(err)
	}
	newer = f.binding(t, newer.ID, biz.Available, &enabled)
	if _, err = f.e.DeleteVPCSnatBinding(f.ctx, "", binding.ID); err != nil {
		t.Fatal(err)
	}
	current, err := f.e.GetVPCSnat(f.ctx, "", vpc.ID, true)
	if err != nil || current.ID != newer.ID || current.State != biz.Available {
		t.Fatal("old delete targeted replacement", err)
	}
	if _, err = f.e.DeleteVPCSnatBinding(f.ctx, "", newer.ID); err != nil {
		t.Fatal(err)
	}
	f.binding(t, newer.ID, biz.Deleted, nil)
	if _, err = f.e.DeleteEIP(f.ctx, "", eip.ID); err != nil {
		t.Fatal(err)
	}
	f.drive(t, func() bool { v, err := f.e.GetEIP(f.ctx, "", eip.ID); return err == nil && v.State == biz.Deleted })
	if _, err = f.n.DeleteVPC(f.ctx, f.tenant, vpc.ID); err != nil {
		t.Fatal(err)
	}
	var history int
	if err = f.owner.QueryRow(f.ctx, "SELECT count(*) FROM network_resource_history WHERE tenant_id=$1 AND snat_id=$2", f.tenant, binding.ID).Scan(&history); err != nil || history < 6 {
		t.Fatal("missing lifecycle history", history, err)
	}
}
func TestEgressConcurrentBindingAndAcceptanceRollback(t *testing.T) {
	f := newEgressFixture(t)
	v := availableEgressVPC(t, f, "race-vpc")
	e := f.eip(t, "race-eip")
	var mu sync.Mutex
	wins := []biz.VPCSnatBinding{}
	errs := []error{}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			b, err := f.e.BindVPCSnat(f.ctx, biz.EgressIntent{Name: "race", VPCID: v.ID, EIPID: e.ID, IdempotencyKey: fmt.Sprint(i)})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
			} else {
				wins = append(wins, b)
			}
		}(i)
	}
	wg.Wait()
	if len(wins) != 1 || len(errs) != 7 {
		t.Fatalf("concurrent occupancy winners=%d errors=%v", len(wins), errs)
	}
	for _, err := range errs {
		if biz.ReasonOf(err) != biz.VPCSnatExists && biz.ReasonOf(err) != biz.EIPInUse {
			t.Fatal("unexpected concurrency failure", err)
		}
	}
	for _, table := range []string{"network_snat_bindings", "network_operations", "network_idempotency"} {
		column := "snat_id"
		var count int
		if err := f.owner.QueryRow(f.ctx, "SELECT count(*) FROM "+table+" WHERE tenant_id=$1 AND "+column+" IS NOT NULL AND snat_id IN (SELECT snat_id FROM network_snat_bindings WHERE tenant_id=$1 AND purpose='public')", f.tenant).Scan(&count); err != nil || count != 1 {
			t.Fatalf("partial acceptance in %s: %d %v", table, count, err)
		}
	}
}
func TestEgressUnknownCreateRecoveryAndFencing(t *testing.T) {
	f := newEgressFixture(t)
	v, err := f.e.CreateEIP(f.ctx, biz.EgressIntent{Name: "lost", IdempotencyKey: "lost"})
	if err != nil {
		t.Fatal(err)
	}
	// Keep unrelated platform tasks out of this explicit one-resource lease schedule.
	if _, err = f.owner.Exec(f.ctx, "UPDATE network_platform_reconciliations SET next_run_at=clock_timestamp()+interval '1 hour'"); err != nil {
		t.Fatal(err)
	}
	old, ok, err := f.p.Claim(f.ctx, uuid.NewString(), time.Minute)
	if err != nil || !ok || old.Resource.ID != v.ID {
		t.Fatal("claim", ok, err)
	}
	if err = f.p.BeginMutation(f.ctx, old, "create", ""); err != nil {
		t.Fatal(err)
	}
	if _, err = f.owner.Exec(f.ctx, "UPDATE network_reconciliations SET lease_until=clock_timestamp()-interval '1 second' WHERE tenant_id=$1 AND eip_id=$2", f.tenant, v.ID); err != nil {
		t.Fatal(err)
	}
	f.w = egressWorker(t, f.p, f.provider)
	before := f.provider.creates
	if _, err = f.w.Step(f.ctx); err != nil {
		t.Fatal(err)
	}
	got, err := f.e.GetEIP(f.ctx, "", v.ID)
	if err != nil || got.State != biz.Available || f.provider.creates != before+1 {
		t.Fatal("unknown fixed-name CREATE did not recover", got, err)
	}
	if err = f.p.Finish(f.ctx, old, biz.Progress{State: biz.Available, OperationState: biz.Succeeded, Identity: "late-old-writer"}); !errors.Is(err, biz.ErrLeaseLost) {
		t.Fatal("expired executor wrote", err)
	}
	f.drive(t, func() bool {
		current, err := f.e.GetEIP(f.ctx, "", v.ID)
		return err == nil && current.State == biz.Available
	})
	replay, err := f.e.CreateEIP(f.ctx, biz.EgressIntent{Name: "lost", IdempotencyKey: "lost"})
	if err != nil || replay.ID != v.ID || replay.LastOperationID != v.LastOperationID {
		t.Fatal("recovery changed receipt", err)
	}
}

func TestEgressUnderlayPlatformOccupancyAndRetirement(t *testing.T) {
	f := newEgressFixture(t)
	boundary := &switchBoundary{}
	f.p.UseVirtualSwitchInfrastructure(boundary)
	sw, err := f.e.CreateVirtualSwitch(f.ctx, biz.VirtualSwitchIntent{Name: "underlay-occupancy", IdempotencyKey: "underlay-switch"})
	if err != nil {
		t.Fatal(err)
	}
	intent := biz.PlatformIntent{Kind: "create_vlan", Name: "untagged", IdempotencyKey: "vlan0", Vlan: &biz.VlanConfig{ClusterID: "test-cluster", VirtualSwitchID: sw.ID, DeviceName: "fixture0", VlanID: 0}}
	vlan, err := f.e.CreatePlatform(f.ctx, intent)
	if err != nil {
		t.Fatal(err)
	}
	policy := biz.DefaultWorkerPolicy()
	policy.ObserveEvery = 10 * time.Millisecond
	switchWorker, err := biz.NewVirtualSwitchWorker(f.p, boundary, uuid.NewString(), policy)
	if err != nil {
		t.Fatal(err)
	}
	f.drive(t, func() bool {
		if _, err := switchWorker.Step(f.ctx); err != nil {
			t.Fatal(err)
		}
		v, err := f.e.GetPlatform(f.ctx, "vlan", vlan.ID)
		return err == nil && v.State == biz.Available
	})
	duplicate := intent
	duplicate.Name, duplicate.IdempotencyKey = "duplicate", "duplicate-vlan"
	if _, err := f.e.CreatePlatform(f.ctx, duplicate); err == nil {
		t.Fatal("duplicate device/VLAN accepted")
	}
	gateway := f.platform(t, biz.PlatformIntent{Kind: "create_egress_gateway", Name: "underlay-gateway", IdempotencyKey: "underlay-gateway"})
	config := &biz.PublicPoolConfig{Mode: "underlay", GatewayID: gateway.ID, CIDR: "198.51.100.0/24", OVNGatewayIP: "198.51.100.1", VlanNetworkID: vlan.ID, UpstreamGatewayIP: "198.51.100.254"}
	pool := f.platform(t, biz.PlatformIntent{Kind: "create_public_pool", Name: "underlay", IdempotencyKey: "underlay", Pool: config})
	if pool.Pool.Mode != "underlay" || pool.AllocationEnabled || pool.Pool.VlanNetworkID != vlan.ID || pool.Pool.UpstreamGatewayIP != config.UpstreamGatewayIP {
		t.Fatal("underlay topology lost", pool)
	}
	if _, err := f.e.DeletePlatform(f.ctx, "vlan", vlan.ID); biz.ReasonOf(err) != biz.ResourceInUse {
		t.Fatal("VLAN deletion ignored pool", err)
	}
	if _, err := f.e.DeletePlatform(f.ctx, "egress_gateway", gateway.ID); biz.ReasonOf(err) != biz.ResourceInUse {
		t.Fatal("gateway deletion ignored pool", err)
	}
	if _, err := f.e.DeletePlatform(f.ctx, "public_pool", pool.ID); err != nil {
		t.Fatal(err)
	}
	f.drive(t, func() bool {
		v, err := f.e.GetPlatform(f.ctx, "public_pool", pool.ID)
		return err == nil && v.State == biz.Deleted
	})
	replacement := f.platform(t, biz.PlatformIntent{Kind: "create_public_pool", Name: "replacement", IdempotencyKey: "pool-replacement", Pool: config})
	if replacement.ID == pool.ID {
		t.Fatal("retired pool reused identity")
	}
	if _, err := f.e.DeletePlatform(f.ctx, "public_pool", replacement.ID); err != nil {
		t.Fatal(err)
	}
	f.drive(t, func() bool {
		v, err := f.e.GetPlatform(f.ctx, "public_pool", replacement.ID)
		return err == nil && v.State == biz.Deleted
	})
	if _, err := f.e.DeletePlatform(f.ctx, "vlan", vlan.ID); err != nil {
		t.Fatal(err)
	}
	f.drive(t, func() bool {
		v, err := f.e.GetPlatform(f.ctx, "vlan", vlan.ID)
		return err == nil && v.State == biz.Deleted
	})
	recreate := intent
	recreate.Name, recreate.IdempotencyKey = "next", "vlan-replacement"
	next := f.platform(t, recreate)
	if next.ID == vlan.ID {
		t.Fatal("retired VLAN reused identity")
	}
	// Administrator materials remain an archive even when their source is old.
	before, err := f.e.GetPlatform(f.ctx, "public_pool", f.pool.ID)
	if err != nil || before.Verification == nil {
		t.Fatal(before, err)
	}
	if _, err = f.e.SetPool(f.ctx, biz.PlatformIntent{Kind: "verify_public_pool", ID: before.ID, ExpectedVersion: before.Version, IdempotencyKey: "archive-again", Verification: before.Verification}); err != nil {
		t.Fatal(err)
	}
	after, err := f.e.GetPlatform(f.ctx, "public_pool", f.pool.ID)
	if err != nil || !reflect.DeepEqual(after.Verification, before.Verification) {
		t.Fatal(after, err)
	}

}

func TestEgressUnknownUpdateKeepsOccupancyAndRecoversExactDesiredState(t *testing.T) {
	f := newEgressFixture(t)
	vpc := availableEgressVPC(t, f, "update-vpc")
	eip := f.eip(t, "update-eip")
	bound, err := f.e.BindVPCSnat(f.ctx, biz.EgressIntent{VPCID: vpc.ID, EIPID: eip.ID, IdempotencyKey: "bind"})
	if err != nil {
		t.Fatal(err)
	}
	enabled := true
	bound = f.binding(t, bound.ID, biz.Available, &enabled)
	f.provider.lostUpdate = true
	accepted, err := f.e.SetVPCSnatEnabled(f.ctx, biz.EgressIntent{ID: bound.ID, Enabled: false, ExpectedVersion: bound.Version, IdempotencyKey: "lost-disable"})
	if err != nil {
		t.Fatal(err)
	}
	f.drive(t, func() bool {
		v, err := f.e.GetVPCSnat(f.ctx, "", bound.ID, false)
		var reason string
		queryErr := f.owner.QueryRow(f.ctx, `SELECT reason FROM network_operations WHERE tenant_id=$1 AND operation_id=$2`, f.tenant, accepted.LastOperationID).Scan(&reason)
		return err == nil && queryErr == nil && reason == string(biz.ProviderUnknown) && v.AppliedEnabled != nil && *v.AppliedEnabled
	})
	if _, err = f.e.DeleteEIP(f.ctx, "", eip.ID); biz.ReasonOf(err) != biz.EIPInUse {
		t.Fatal("unknown update released EIP", err)
	}
	if _, err = f.e.DeleteVPCSnatBinding(f.ctx, "", bound.ID); biz.ReasonOf(err) != biz.ResourceBusy {
		t.Fatal("unknown update allowed conflicting deletion", err)
	}
	f.w = egressWorker(t, f.p, f.provider)
	enabled = false
	current := f.binding(t, bound.ID, biz.Available, &enabled)
	if current.LastOperationID != accepted.LastOperationID || current.DesiredEnabled || f.provider.updates != 1 {
		t.Fatal("update recovery repeated provider write or changed operation", current, f.provider.updates)
	}
}

func egressReportedReady(value bool) *bool { return &value }
