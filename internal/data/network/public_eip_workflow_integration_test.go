package data_test

import (
	"context"
	"errors"
	"github.com/google/uuid"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/tenantnamespace"
	"reflect"
	"slices"
	"testing"
)

// Only Kubernetes/controller HTTP is a boundary double. Admission, persistence,
// claims, leases, Provider adapter, projections and lifecycle all run real code.
func TestPublicEIPWorkflowOwnVPCReadyWithoutBase(t *testing.T) {
	f, api, _, _ := newEgressKCFixture(t, nil)
	vpc := availableVPC(t, f.n, f.w, f.tenant, "public-without-base")
	address := f.eip(t, "public-address")
	var count int
	if err := f.owner.QueryRow(f.ctx, `SELECT count(*) FROM network_vpc_base_connectivity WHERE tenant_id=$1 AND vpc_id=$2`, f.tenant, vpc.ID).Scan(&count); err != nil || count != 0 {
		t.Fatal("fixture unexpectedly created base connectivity", count, err)
	}
	before, err := f.n.GetVPC(f.ctx, f.tenant, vpc.ID)
	if err != nil || before.PublicEgress != nil {
		t.Fatal("unbound VPC projection", before, err)
	}
	// A parent's reported generation does not gate this own SNAT declaration.
	name := "vpc-" + vpc.ID[len("vpc_"):]
	parent := api.Object("vpcs", "tenant-"+f.tenant, name)
	parent["status"].(map[string]any)["observedGeneration"] = 0
	api.Change("vpcs", parent, false)
	accepted, err := f.e.BindVPCSnat(f.ctx, biz.EgressIntent{VPCID: vpc.ID, EIPID: address.ID, IdempotencyKey: "bind-public"})
	if err != nil {
		t.Fatal("own Ready VPC must bind without base EIP", err)
	}
	assertProjection := func(state string, enabled *bool) {
		t.Helper()
		v, err := f.n.GetVPC(f.ctx, f.tenant, vpc.ID)
		if err != nil || v.PublicEgress == nil {
			t.Fatal("missing same-snapshot public association", v, err)
		}
		p := v.PublicEgress
		if p.ID != accepted.ID || p.EIPID != address.ID || p.EIPAddress != address.Address || p.BindingState != state || !reflect.DeepEqual(p.AppliedEnabled, enabled) {
			t.Fatal("wrong VPC association", p, state, enabled)
		}
		e, err := f.e.GetEIP(f.ctx, "", address.ID)
		if err != nil || e.BindingState != state || e.BindingTarget == nil || e.BindingTarget.ID != accepted.ID || e.BindingTarget.VPCID != vpc.ID {
			t.Fatal("EIP claim projection disagrees", e, err)
		}
		pageNumber, pageSize := uint32(1), uint32(1)
		page, err := f.n.ListVPCs(f.ctx, biz.ListVPCs{TenantID: f.tenant, Name: vpc.Name, Page: &pageNumber, PageSize: &pageSize})
		if err != nil || page.Total != 1 || len(page.Items) != 1 || !reflect.DeepEqual(page.Items[0].PublicEgress, p) {
			t.Fatal("VPC page does not expose same public association", page, err)
		}
	}
	assertProjection("reserved", nil)
	on := true
	current := f.binding(t, accepted.ID, biz.Available, &on)
	assertProjection("bound", &on)
	if _, err = f.e.DeleteEIP(f.ctx, "", address.ID); biz.ReasonOf(err) != biz.EIPInUse {
		t.Fatal("bound EIP delete was admitted", err)
	}
	off := false
	current, err = f.e.SetVPCSnatEnabled(f.ctx, biz.EgressIntent{ID: accepted.ID, Enabled: false, ExpectedVersion: current.Version, IdempotencyKey: "disable-public"})
	if err != nil {
		t.Fatal(err)
	}
	current = f.binding(t, current.ID, biz.Available, &off)
	assertProjection("bound", &off)
	f.drive(t, func() bool {
		v, err := f.e.GetEIP(f.ctx, "", address.ID)
		return err == nil && v.State == biz.Available
	})
	current, err = f.e.SetVPCSnatEnabled(f.ctx, biz.EgressIntent{ID: accepted.ID, Enabled: true, ExpectedVersion: current.Version, IdempotencyKey: "enable-without-base"})
	if err != nil {
		t.Fatal("re-enable depends on missing base connectivity", err)
	}
	current = f.binding(t, current.ID, biz.Available, &on)
	assertProjection("bound", &on)
	if _, err = f.owner.Exec(f.ctx, `UPDATE network_snat_bindings SET observed_at=clock_timestamp()-interval '2 minutes' WHERE tenant_id=$1 AND snat_id=$2`, f.tenant, accepted.ID); err != nil {
		t.Fatal(err)
	}
	stale, err := f.n.GetVPC(f.ctx, f.tenant, vpc.ID)
	if err != nil || stale.PublicEgress == nil || stale.PublicEgress.ObservationStale || stale.PublicEgress.AppliedEnabled == nil || !*stale.PublicEgress.AppliedEnabled || stale.PublicEgress.BindingState != "bound" {
		t.Fatal("read age changed reported applied state or binding", stale, err)
	}
	if _, err = f.e.DeleteVPCSnatBinding(f.ctx, "", accepted.ID); err != nil {
		t.Fatal(err)
	}
	removing, err := f.n.GetVPC(f.ctx, f.tenant, vpc.ID)
	if err != nil || removing.PublicEgress == nil || removing.PublicEgress.State != biz.Deleting || removing.PublicEgress.BindingState != "bound" {
		t.Fatal("DELETE acceptance prematurely released claim", removing, err)
	}
	f.binding(t, accepted.ID, biz.Deleted, nil)
	after, err := f.n.GetVPC(f.ctx, f.tenant, vpc.ID)
	e, ee := f.e.GetEIP(f.ctx, "", address.ID)
	if err != nil || after.PublicEgress != nil || ee != nil || e.BindingState != "unbound" || e.BindingTarget != nil {
		t.Fatal("confirmed unbind did not clear both projections", after, e, err, ee)
	}
	if _, err = f.e.DeleteEIP(f.ctx, "", address.ID); err != nil {
		t.Fatal(err)
	}
	f.drive(t, func() bool { e, err := f.e.GetEIP(f.ctx, "", address.ID); return err == nil && e.State == biz.Deleted })
}

func TestPublicEIPWorkflowDegradedBaseDoesNotBlockPublic(t *testing.T) {
	f, _, _, _ := newBaseKCFixture(t, nil)
	vpc := baseState(t, f, createBaseVPC(t, f, "public-independent-of-base").ID, biz.Available)
	if _, err := f.owner.Exec(f.ctx, `UPDATE network_vpc_base_connectivity SET state='degraded' WHERE tenant_id=$1 AND vpc_id=$2`, f.tenant, vpc.ID); err != nil {
		t.Fatal(err)
	}
	eip := f.eip(t, "public-while-base-degraded")
	binding, err := f.e.BindVPCSnat(f.ctx, biz.EgressIntent{VPCID: vpc.ID, EIPID: eip.ID, IdempotencyKey: "bind-with-degraded-base"})
	if err != nil {
		t.Fatal("healthy VPC Provider was gated by aggregate base fault", err)
	}
	on, off := true, false
	current := f.binding(t, binding.ID, biz.Available, &on)
	current, err = f.e.SetVPCSnatEnabled(f.ctx, biz.EgressIntent{ID: current.ID, Enabled: false, ExpectedVersion: current.Version, IdempotencyKey: "disable-with-base-fault"})
	if err != nil {
		t.Fatal(err)
	}
	current = f.binding(t, current.ID, biz.Available, &off)
	f.drive(t, func() bool { v, err := f.e.GetEIP(f.ctx, "", eip.ID); return err == nil && v.State == biz.Available })
	_, err = f.e.SetVPCSnatEnabled(f.ctx, biz.EgressIntent{ID: current.ID, Enabled: true, ExpectedVersion: current.Version, IdempotencyKey: "enable-with-base-fault"})
	if err != nil {
		t.Fatal("re-enable still depends on base aggregate", err)
	}
	f.binding(t, current.ID, biz.Available, &on)
	after, err := f.n.GetVPC(f.ctx, f.tenant, vpc.ID)
	if err != nil || after.State != biz.Available || after.BaseConnectivity.State != "degraded" || after.PublicEgress == nil || after.PublicEgress.BindingState != "bound" {
		t.Fatal("public hid base fault or lost independent association", after, err)
	}
}

func TestPublicEIPWorkflowAdmissionUsesManagementState(t *testing.T) {
	for _, scenario := range []string{"degraded", "old-read", "pending-parent", "deleting"} {
		t.Run(scenario, func(t *testing.T) {
			f := newEgressFixture(t)
			vpc := availableEgressVPC(t, f, "public-admission")
			eip := f.eip(t, "public-admission-address")
			query := map[string]string{
				"degraded":       `UPDATE network_vpcs SET state='degraded' WHERE tenant_id=$1 AND vpc_id=$2`,
				"old-read":       `UPDATE network_vpcs SET observed_at=clock_timestamp()-interval '2 hour' WHERE tenant_id=$1 AND vpc_id=$2`,
				"pending-parent": `UPDATE network_provider_bindings SET pending_action='create',pending_since=clock_timestamp() WHERE tenant_id=$1 AND vpc_id=$2`,
				"deleting":       `UPDATE network_vpcs SET state='deleting' WHERE tenant_id=$1 AND vpc_id=$2`,
			}[scenario]
			if _, err := f.owner.Exec(f.ctx, query, f.tenant, vpc.ID); err != nil {
				t.Fatal(err)
			}
			_, err := f.e.BindVPCSnat(f.ctx, biz.EgressIntent{VPCID: vpc.ID, EIPID: eip.ID, IdempotencyKey: scenario})
			wantClaims := 1
			if scenario == "deleting" {
				wantClaims = 0
				if biz.ReasonOf(err) != biz.ResourceBusy {
					t.Fatal("deletion did not close admission", err)
				}
			} else if err != nil {
				t.Fatal("parent runtime facts gated own declaration", err)
			}
			var claims int
			if err := f.owner.QueryRow(f.ctx, `SELECT count(*) FROM network_eip_claims WHERE tenant_id=$1 AND eip_id=$2 AND released_at IS NULL`, f.tenant, eip.ID).Scan(&claims); err != nil || claims != wantClaims {
				t.Fatal(claims, err)
			}
		})
	}
}

func TestPublicEIPWorkflowCreateSecurityGroupsAtomicAndReplay(t *testing.T) {
	e := newEgressFixture(t)
	groups := biz.NewSecurityGroups(e.p)
	worker := biz.NewSecurityGroupWorker(e.p, securityGroupProviderBoundary{}, uuid.NewString(), biz.DefaultWorkerPolicy())
	f := &securityGroupContractFixture{p: e.p, owner: e.owner, groups: groups, worker: worker, ctx: e.ctx, tenant: e.tenant}
	first, second := f.create(t, "initial-filter"), f.create(t, "replacement-filter")
	// Both tenants have externally provisioned Namespaces. Keep the original
	// identity so the negative request reaches tenant ownership admission.
	originalNamespace, err := e.p.ResolveTenantNamespace(e.ctx, e.tenant)
	if err != nil {
		t.Fatal(err)
	}
	foreignTenant := uuid.NewString()
	foreignNamespace := tenantnamespace.Identity{TenantID: foreignTenant, ClusterID: originalNamespace.ClusterID, Name: "tenant-" + foreignTenant, UID: uuid.NewString()}
	e.p.ConfigureTenantNamespaceResolver(func(_ context.Context, tenant string) (tenantnamespace.Identity, error) {
		switch tenant {
		case originalNamespace.TenantID:
			return originalNamespace, nil
		case foreignTenant:
			return foreignNamespace, nil
		default:
			return tenantnamespace.Identity{}, errors.New("unknown fixture tenant")
		}
	})
	foreignCtx := biz.WithEgressCaller(context.Background(), biz.EgressCaller{TenantID: foreignTenant, Attribution: biz.Attribution{Actor: "foreign", DirectCaller: "test"}})
	foreign, err := groups.Mutate(foreignCtx, biz.SecurityGroupIntent{Action: "create", Name: "foreign-filter", IdempotencyKey: "foreign-filter"})
	if err != nil {
		t.Fatal(err)
	}
	failed := biz.EgressIntent{Name: "atomic-address", IdempotencyKey: "atomic-address", SecurityGroupIDs: []string{first.ID, foreign.SecurityGroup.ID}}
	if _, err = e.e.CreateEIP(e.ctx, failed); biz.ReasonOf(err) != biz.ResourceNotFound {
		t.Fatal("foreign SG was accepted", err)
	}
	var rows int
	if err = e.owner.QueryRow(e.ctx, `SELECT (SELECT count(*) FROM network_eips WHERE tenant_id=$1)+(SELECT count(*) FROM network_idempotency WHERE tenant_id=$1 AND operation_kind='create_eip')`, e.tenant).Scan(&rows); err != nil || rows != 0 {
		t.Fatal("rejected create left a partial EIP or accepted key", rows, err)
	}
	input := failed
	input.SecurityGroupIDs = []string{first.ID, second.ID}
	address, err := e.e.CreateEIP(e.ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := groups.GetEIP(e.ctx, "", address.ID)
	if err != nil || binding.Version != 1 || binding.State != "applying" || !sameStringSet(binding.SecurityGroupIDs, input.SecurityGroupIDs) {
		t.Fatal("initial configuration was not persisted with EIP", binding, err)
	}
	input.SecurityGroupIDs = []string{second.ID, first.ID}
	replay, err := e.e.CreateEIP(e.ctx, input)
	if !replay.CreatedAt.Equal(address.CreatedAt) || !replay.UpdatedAt.Equal(address.UpdatedAt) {
		t.Fatal("reordered SG replay changed persisted timestamps", replay, address, err)
	}
	// PostgreSQL and the durable JSON reply may use different time locations
	// for the same instant. Compare every remaining business field exactly.
	replay.CreatedAt, replay.UpdatedAt = address.CreatedAt, address.UpdatedAt
	if err != nil || !reflect.DeepEqual(replay, address) {
		t.Fatal("reordered SG set changed replay", replay, address, err)
	}
	input.SecurityGroupIDs = []string{first.ID}
	if _, err = e.e.CreateEIP(e.ctx, input); biz.ReasonOf(err) != biz.IdempotencyConflict {
		t.Fatal("same key accepted another initial SG set", err)
	}
	if _, err = groups.Mutate(e.ctx, biz.SecurityGroupIntent{Action: "delete", ID: first.ID}); biz.ReasonOf(err) != biz.ResourceInUse {
		t.Fatal("atomic initial SG reference missing", err)
	}
	for range 8 {
		if _, err = worker.Step(e.ctx); err != nil {
			t.Fatal(err)
		}
		binding, err = groups.GetEIP(e.ctx, "", address.ID)
		if err == nil && binding.State == "configured" {
			break
		}
	}
	if binding.State != "configured" || !sameStringSet(binding.AppliedSecurityGroupIDs, []string{first.ID, second.ID}) {
		t.Fatal("initial SG worker did not apply", binding, err)
	}
	next, err := groups.SetEIP(e.ctx, biz.SecurityGroupIntent{ID: address.ID, ExpectedVersion: binding.Version, IdempotencyKey: "replace-initial", SecurityGroupIDs: []string{second.ID}})
	if err != nil || next.Binding.Version != 2 {
		t.Fatal("existing EIP replacement lost initial version", next, err)
	}
}

func sameStringSet(a, b []string) bool {
	a = slices.Clone(a)
	b = slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}
