package data_test

import (
	"github.com/google/uuid"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLBPinsExistingAddressesAcrossBothDefaultPoolSwitchesAndClosure(t *testing.T) {
	c := &lbControllerFixture{}
	f := newLBAdmissionFixture(t, c.http)
	entryEIP, snatEIP := f.f.eip(t, "lb-pool-entry"), f.f.eip(t, "snat-pool-entry")
	request := f.request
	request.Exposure = "public_private"
	request.PublicEIPID = entryEIP.ID
	accepted, err := f.lbs.Create(f.f.ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	lb := lbState(t, f, accepted.LoadBalancer.ID, biz.Available)
	bound, err := f.f.e.BindVPCSnat(f.f.ctx, biz.EgressIntent{VPCID: f.vpc.ID, EIPID: snatEIP.ID, IdempotencyKey: "coexisting-snat"})
	if err != nil {
		t.Fatal(err)
	}
	enabled := true
	bound = f.f.binding(t, bound.ID, biz.Available, &enabled)
	baseEIP, baseSNAT := baseIDs(t, f.f, f.vpc.ID)
	var oldIntranet string
	if err = f.f.owner.QueryRow(f.f.ctx, `SELECT pool_id FROM network_vpc_base_connectivity WHERE tenant_id=$1 AND vpc_id=$2`, f.f.tenant, f.vpc.ID).Scan(&oldIntranet); err != nil {
		t.Fatal(err)
	}
	intent := intranetIntent("lb-next-intranet", "10.233.254.0/24", "10.233.254.1")
	intent.Pool.DefaultVPCUID = f.api.Object("vpcs", "kcn-system", "kcn-cluster")["metadata"].(map[string]any)["uid"].(string)
	nextIntranet := readyIntranetPool(t, f.f, f.f.platform(t, intent))
	setIntranetPool(t, f.f, biz.PlatformIntent{Kind: "set_default_intranet_pool", ID: nextIntranet.ID, IdempotencyKey: "lb-switch-intranet"})
	nextPublic := f.f.platform(t, biz.PlatformIntent{Kind: "create_public_pool", Name: "lb-next-public", IdempotencyKey: "lb-next-public", Pool: &biz.PublicPoolConfig{Mode: "overlay", GatewayID: f.f.pool.Pool.GatewayID, CIDR: "198.51.100.0/24", OVNGatewayIP: "198.51.100.1"}})
	f.f.setPool(t, biz.PlatformIntent{Kind: "verify_public_pool", ID: nextPublic.ID, IdempotencyKey: "lb-next-public-verify", Verification: &biz.PublicPoolVerification{ProviderSourceRevision: strings.Repeat("a", 40), ProviderImageDigests: []string{"sha256:" + strings.Repeat("a", 64)}, TopologyFingerprint: strings.Repeat("c", 64), EvidenceReference: "controlled HTTP fixture only", Scope: "adapter test", VerifiedAt: time.Now().Add(-time.Second), ExpiresAt: time.Now().Add(time.Hour)}})
	f.f.setPool(t, biz.PlatformIntent{Kind: "set_default_pool", ID: nextPublic.ID, IdempotencyKey: "lb-switch-public"})
	for _, id := range []string{oldIntranet, nextIntranet.ID} {
		setIntranetPool(t, f.f, biz.PlatformIntent{Kind: "set_intranet_pool_allocation", ID: id, IdempotencyKey: "lb-close-" + id, Enabled: false})
	}
	f.f.setPool(t, biz.PlatformIntent{Kind: "set_pool_allocation", ID: f.f.pool.ID, IdempotencyKey: "lb-close-public-old", Enabled: false})
	// nextPublic was never opened: switching a default cannot open allocation.
	if _, err = f.f.e.CreateEIP(f.f.ctx, biz.EgressIntent{Name: "blocked-eip", IdempotencyKey: "blocked-eip"}); biz.ReasonOf(err) != biz.PublicEgressNotReady {
		t.Fatal("closed default accepted EIP", err)
	}
	if _, err = f.f.n.CreateVPC(f.f.ctx, biz.CreateVPC{TenantID: f.f.tenant, Name: "blocked-vpc", CIDR: "10.62.0.0/16", IdempotencyKey: "blocked-vpc"}); biz.ReasonOf(err) != biz.BaseConnectivityNotReady {
		t.Fatal("closed default accepted new base allocation", err)
	}
	lb = lbState(t, f, lb.ID, biz.Available)
	update := biz.UpdateLoadBalancer{ID: lb.ID, ExpectedVersion: lb.Version, IdempotencyKey: "update-after-pool-closure", LoadBalancerMutableInput: biz.LoadBalancerMutableInput{Health: f.request.Health, Name: "still-pinned", Backends: []biz.LoadBalancerBackendInput{{ID: lb.Backends[0].ID, SubnetID: lb.Backends[0].SubnetID, Address: lb.Backends[0].Address, Port: lb.Backends[0].Port}}}}
	updated, err := f.lbs.Update(f.f.ctx, update)
	if err != nil {
		t.Fatal("allocation closure invalidated existing LB", err)
	}
	// Only metadata changed. Deferred activation advances the accepted version
	// while retaining the already applied Provider configuration and addresses.
	if updated.LoadBalancer.DesiredVersion != lb.DesiredVersion+1 || updated.LoadBalancer.ProviderTargetVersion != lb.ProviderTargetVersion {
		t.Fatal("metadata update changed the pinned Provider configuration", updated)
	}
	f.f.drive(t, func() bool {
		lb, err = f.lbs.Get(f.f.ctx, "", lb.ID)
		return err == nil && lb.DesiredVersion == updated.LoadBalancer.DesiredVersion &&
			lb.ProviderTargetVersion == updated.LoadBalancer.ProviderTargetVersion &&
			lb.AppliedVersion == updated.LoadBalancer.ProviderTargetVersion && lb.Name == update.Name &&
			lb.ConfigurationState == "configured" && lb.State == biz.Available
	})
	for i, on := range []bool{false, true, false} {
		f.f.drive(t, func() bool {
			e, err := f.f.e.GetEIP(f.f.ctx, "", snatEIP.ID)
			return err == nil && e.State == biz.Available && !e.ObservationStale
		})
		bound = f.f.binding(t, bound.ID, biz.Available, nil)
		if _, err = f.f.e.SetVPCSnatEnabled(f.f.ctx, biz.EgressIntent{ID: bound.ID, Enabled: on, ExpectedVersion: bound.Version, IdempotencyKey: []string{"off", "on", "off-again"}[i]}); err != nil {
			t.Fatal(err)
		}
		bound = f.f.binding(t, bound.ID, biz.Available, &on)
		baseState(t, f.f, f.vpc.ID, biz.Available)
		lbState(t, f, lb.ID, biz.Available)
		a, b := baseIDs(t, f.f, f.vpc.ID)
		if a != baseEIP || b != baseSNAT {
			t.Fatal("Public change replaced base identities")
		}
	}
	if _, err = f.lbs.Delete(f.f.ctx, "", lb.ID); err != nil {
		t.Fatal(err)
	}
	lbState(t, f, lb.ID, biz.Deleted)
	if _, err = f.f.e.DeleteVPCSnatBinding(f.f.ctx, "", bound.ID); err != nil {
		t.Fatal(err)
	}
	f.f.binding(t, bound.ID, biz.Deleted, nil)
	baseState(t, f.f, f.vpc.ID, biz.Available)
}

func TestLBAndPublicBindingCoexistWithBackfillPauseResumeAndDeleteRace(t *testing.T) {
	c := &lbControllerFixture{}
	f := newLBAdmissionFixture(t, c.http)
	accepted, err := f.lbs.Create(f.f.ctx, f.request)
	if err != nil {
		t.Fatal(err)
	}
	lb := lbState(t, f, accepted.LoadBalancer.ID, biz.Available)
	if _, err = f.f.p.SetNewVPCBaseConnectivity(f.f.ctx, false); err != nil {
		t.Fatal(err)
	}
	legacy, err := f.f.n.CreateVPC(f.f.ctx, biz.CreateVPC{TenantID: f.f.tenant, Name: "legacy-with-lb", CIDR: "10.61.0.0/16", IdempotencyKey: "legacy-with-lb"})
	if err != nil {
		t.Fatal(err)
	}
	legacy = baseState(t, f.f, legacy.ID, biz.Available)
	original, err := f.f.n.GetOperation(f.f.ctx, f.f.tenant, legacy.LastOperationID)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := f.f.p.PlanBaseBackfill(f.f.ctx, data.BaseBackfillPlanInput{RunID: uuid.NewString(), Interval: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if next, err := f.f.p.AdmitNextBaseBackfill(f.f.ctx, plan.RunID); err != nil || next.State != "paused" {
		t.Fatal("unreviewed plan admitted", next, err)
	}
	if _, err = f.f.p.SetBaseBackfillPaused(f.f.ctx, plan.RunID, false, plan.SHA256); err != nil {
		t.Fatal(err)
	}
	eip := f.f.eip(t, "backfill-concurrent-snat")
	var group sync.WaitGroup
	group.Add(2)
	results := make([]error, 2)
	var bound biz.VPCSnatBinding
	var admission data.BaseBackfillAdmission
	start := make(chan struct{})
	go func() {
		defer group.Done()
		<-start
		admission, results[0] = f.f.p.AdmitNextBaseBackfill(f.f.ctx, plan.RunID)
	}()
	go func() {
		defer group.Done()
		<-start
		bound, results[1] = f.f.e.BindVPCSnat(f.f.ctx, biz.EgressIntent{VPCID: legacy.ID, EIPID: eip.ID, IdempotencyKey: "during-backfill"})
	}()
	close(start)
	group.Wait()
	if results[0] != nil || (results[1] != nil && biz.ReasonOf(results[1]) != biz.BaseConnectivityNotReady) || admission.State != "accepted" {
		t.Fatal("backfill and concurrent binding must serialize; binding may win before backfill admission", admission, results)
	}
	if _, err = f.f.p.SetBaseBackfillPaused(f.f.ctx, plan.RunID, true, ""); err != nil {
		t.Fatal(err)
	}
	if next, err := f.f.p.AdmitNextBaseBackfill(f.f.ctx, plan.RunID); err != nil || next.State != "paused" {
		t.Fatal(next, err)
	}
	f.f.drive(t, func() bool {
		op, err := f.f.n.GetOperation(f.f.ctx, f.f.tenant, admission.OperationID)
		return err == nil && op.State == biz.Succeeded
	})
	bound, err = f.f.e.BindVPCSnat(f.f.ctx, biz.EgressIntent{VPCID: legacy.ID, EIPID: eip.ID, IdempotencyKey: "during-backfill"})
	if err != nil {
		t.Fatal("Public binding did not recover after actual backfill", err)
	}
	enabled := true
	bound = f.f.binding(t, bound.ID, biz.Available, &enabled)
	current, err := f.f.n.GetOperation(f.f.ctx, f.f.tenant, legacy.LastOperationID)
	if err != nil || current.State != original.State || !current.CompletedAt.Equal(*original.CompletedAt) {
		t.Fatal("backfill changed historical operation", current, err)
	}
	if _, err = f.f.p.SetBaseBackfillPaused(f.f.ctx, plan.RunID, false, plan.SHA256); err != nil {
		t.Fatal(err)
	}
	if _, err = f.f.p.SetNewVPCBaseConnectivity(f.f.ctx, true); err != nil {
		t.Fatal(err)
	}
	// A persistent backfill dispatcher and product delete compete on the same
	// parent. Established Public SNAT blocks deletion until explicitly unbound.
	if _, err = f.f.n.DeleteVPC(f.f.ctx, f.f.tenant, legacy.ID); biz.ReasonOf(err) != biz.VPCSnatExists {
		t.Fatal("legacy parent ignored Public claim", err)
	}
	if _, err = f.f.e.DeleteVPCSnatBinding(f.f.ctx, "", bound.ID); err != nil {
		t.Fatal(err)
	}
	f.f.binding(t, bound.ID, biz.Deleted, nil)
	group.Add(2)
	start = make(chan struct{})
	go func() { defer group.Done(); <-start; _, results[0] = f.f.p.AdmitNextBaseBackfill(f.f.ctx, plan.RunID) }()
	go func() { defer group.Done(); <-start; _, results[1] = f.f.n.DeleteVPC(f.f.ctx, f.f.tenant, legacy.ID) }()
	close(start)
	group.Wait()
	if results[0] != nil || results[1] != nil {
		t.Fatal("delete/backfill conflict did not converge", results)
	}
	baseState(t, f.f, legacy.ID, biz.Deleted)
	lbState(t, f, lb.ID, biz.Available)
	if _, err = f.lbs.Delete(f.f.ctx, "", lb.ID); err != nil {
		t.Fatal(err)
	}
	lbState(t, f, lb.ID, biz.Deleted)
}
