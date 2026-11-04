package data_test

import (
	"context"
	"fmt"
	"github.com/google/uuid"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"strings"
	"testing"
	"time"
)

type egressBaseFixture struct{ EIPID, SnatID string }

// Historical Public tests exercise their original provider boundary. Their
// prerequisite is explicit persisted, fresh base evidence, not a bypass in
// Public admission. This fixture does not prove the real base lifecycle; the
// NET-U03 tests independently exercise that lifecycle and the process boundary.
func availableEgressVPC(t *testing.T, f *egressFixture, key string) biz.VPC {
	t.Helper()
	v := availableVPC(t, f.n, f.w, f.tenant, key)
	seedEgressBaseConnectivity(t, f, v)
	return v
}
func seedEgressBaseConnectivity(t *testing.T, f *egressFixture, v biz.VPC) egressBaseFixture {
	t.Helper()
	ids := egressBaseFixture{EIPID: "eip_" + strings.ReplaceAll(uuid.NewString(), "-", ""), SnatID: "snat_" + strings.ReplaceAll(uuid.NewString(), "-", "")}
	poolID := "pool_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	poolOp, eipOp, snatOp := uuid.NewString(), uuid.NewString(), uuid.NewString()
	tx, err := f.owner.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	exec := func(stmt string, args ...any) {
		t.Helper()
		if _, err := tx.Exec(f.ctx, stmt, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO network_platform_resources(resource_id,kind,cluster_id,name,state,created_at,updated_at,observed_at,last_operation_id,provider_name,provider_uid,binding_id)
 SELECT $1,'public_pool',cluster_id,'Public regression base fixture','available',clock_timestamp(),clock_timestamp(),clock_timestamp(),$2,$1,'fixture-intranet-pool-uid',gen_random_uuid()
 FROM network_platform_resources WHERE resource_id=$3`, poolID, poolOp, f.pool.ID)
	exec(`INSERT INTO network_platform_operations(operation_id,resource_id,kind,state,created_at,updated_at,completed_at)
 VALUES($1,$2,'create_intranet_pool','succeeded',clock_timestamp(),clock_timestamp(),clock_timestamp())`, poolOp, poolID)
	exec(`INSERT INTO network_public_pools(resource_id,cluster_id,mode,cidr,ovn_gateway_ip,excluded_ips,scope,default_vpc_name,default_vpc_uid,intranet_networks)
 SELECT $1,cluster_id,'overlay','198.18.0.0/24','198.18.0.1',ARRAY['198.18.0.1'],'intranet','default','fixture-default-vpc-uid',ARRAY['10.96.0.0/12']
 FROM network_platform_resources WHERE resource_id=$1`, poolID)
	exec(`INSERT INTO network_eips(tenant_id,eip_id,cluster_id,namespace,name,pool_id,pool_revision,address,state,created_at,updated_at,observed_at,last_operation_id,scope,managed_by,system_owner_vpc)
 SELECT tenant_id,$2,cluster_id,namespace,'System base fixture',$3,1,'198.18.0.10','available',clock_timestamp(),clock_timestamp(),clock_timestamp(),$4,'intranet','system',vpc_id
 FROM network_provider_bindings WHERE tenant_id=$1 AND vpc_id=$5`, f.tenant, ids.EIPID, poolID, eipOp, v.ID)
	exec(`INSERT INTO network_snat_bindings(tenant_id,snat_id,cluster_id,namespace,name,vpc_id,eip_id,desired_enabled,applied_enabled,state,created_at,updated_at,observed_at,last_operation_id,purpose)
 SELECT tenant_id,$2,cluster_id,namespace,'System base fixture',vpc_id,$3,true,true,'available',clock_timestamp(),clock_timestamp(),clock_timestamp(),$4,'intranet'
 FROM network_provider_bindings WHERE tenant_id=$1 AND vpc_id=$5`, f.tenant, ids.SnatID, ids.EIPID, snatOp, v.ID)
	for _, child := range []struct{ kind, id, op, operation string }{{"eip", ids.EIPID, eipOp, "create_eip"}, {"snat", ids.SnatID, snatOp, "bind_snat"}} {
		exec(`INSERT INTO network_operations(tenant_id,operation_id,`+child.kind+`_id,kind,state,created_at,updated_at,completed_at)
  VALUES($1,$2,$3,$4,'succeeded',clock_timestamp(),clock_timestamp(),clock_timestamp())`, f.tenant, child.op, child.id, child.operation)
		exec(`INSERT INTO network_provider_bindings(tenant_id,`+child.kind+`_id,resource_kind,binding_id,cluster_id,namespace,provider_name,provider_uid)
  SELECT tenant_id,$2,$3,gen_random_uuid(),cluster_id,namespace,$4,$5 FROM network_provider_bindings WHERE tenant_id=$1 AND vpc_id=$6`, f.tenant, child.id, child.kind, strings.Replace(child.id, "_", "-", 1), "fixture-"+child.id, v.ID)
	}
	exec(`INSERT INTO network_eip_claims(tenant_id,cluster_id,namespace,eip_id,target_kind,snat_id,state)
 SELECT tenant_id,cluster_id,namespace,eip_id,'vpc_snat',$2,'bound' FROM network_eips WHERE tenant_id=$1 AND eip_id=$3`, f.tenant, ids.SnatID, ids.EIPID)
	exec(`INSERT INTO network_vpc_base_connectivity(tenant_id,vpc_id,cluster_id,namespace,pool_id,pool_revision,eip_id,snat_id,operation_id,state,observed_at)
 SELECT tenant_id,vpc_id,cluster_id,namespace,$2,1,$3,$4,$5,'ready',clock_timestamp()
 FROM network_provider_bindings WHERE tenant_id=$1 AND vpc_id=$6`, f.tenant, poolID, ids.EIPID, ids.SnatID, v.LastOperationID, v.ID)
	if err = tx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	return ids
}

func TestPublicTenantInterfacesHideSystemResourcesAndPreserveBaseIdentity(t *testing.T) {
	f := newEgressFixture(t)
	vpc := availableVPC(t, f.n, f.w, f.tenant, "public-isolation")
	base := seedEgressBaseConnectivity(t, f, vpc)
	snapshot := func() string {
		var s string
		err := f.owner.QueryRow(f.ctx, `SELECT jsonb_build_object('eip_id',e.eip_id,'address',e.address,'pool_id',e.pool_id,'pool_revision',e.pool_revision,'snat_id',s.snat_id,'vpc_id',s.vpc_id,'eip_uid',eb.provider_uid,'snat_uid',sb.provider_uid,'desired',s.desired_enabled)::text
  FROM network_eips e JOIN network_snat_bindings s ON s.tenant_id=e.tenant_id AND s.eip_id=e.eip_id
  JOIN network_provider_bindings eb ON eb.tenant_id=e.tenant_id AND eb.eip_id=e.eip_id
  JOIN network_provider_bindings sb ON sb.tenant_id=s.tenant_id AND sb.snat_id=s.snat_id
  WHERE e.tenant_id=$1 AND e.eip_id=$2`, f.tenant, base.EIPID).Scan(&s)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := snapshot()
	for name, call := range map[string]func() error{
		"get-system-eip":    func() error { _, err := f.e.GetEIP(f.ctx, "", base.EIPID); return err },
		"delete-system-eip": func() error { _, err := f.e.DeleteEIP(f.ctx, "", base.EIPID); return err },
		"bind-system-eip": func() error {
			_, err := f.e.BindVPCSnat(f.ctx, biz.EgressIntent{VPCID: vpc.ID, EIPID: base.EIPID, IdempotencyKey: "forbidden-bind"})
			return err
		},
		"get-system-snat": func() error { _, err := f.e.GetVPCSnat(f.ctx, "", base.SnatID, false); return err },
		"toggle-system-snat": func() error {
			_, err := f.e.SetVPCSnatEnabled(f.ctx, biz.EgressIntent{ID: base.SnatID, ExpectedVersion: 1, Enabled: false, IdempotencyKey: "forbidden-toggle"})
			return err
		},
		"delete-system-snat": func() error { _, err := f.e.DeleteVPCSnatBinding(f.ctx, "", base.SnatID); return err },
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(); biz.ReasonOf(err) != biz.ResourceNotFound {
				t.Fatal("system resource became a Public target", err)
			}
		})
	}
	if _, err := f.e.GetVPCSnat(f.ctx, "", vpc.ID, true); biz.ReasonOf(err) != biz.ResourceNotFound {
		t.Fatal("by-VPC lookup exposed intranet", err)
	}
	eip := f.eip(t, "public-visible")
	rows, _, total, err := f.e.ListEIPs(f.ctx, biz.ListVPCs{Limit: 100})
	if err != nil || total != 1 || len(rows) != 1 || rows[0].ID != eip.ID {
		t.Fatal("Public list contains system resource", rows, err)
	}
	bound, err := f.e.BindVPCSnat(f.ctx, biz.EgressIntent{VPCID: vpc.ID, EIPID: eip.ID, IdempotencyKey: "public-bind"})
	if err != nil {
		t.Fatal(err)
	}
	enabled := true
	bound = f.binding(t, bound.ID, biz.Available, &enabled)
	byVPC, err := f.e.GetVPCSnat(f.ctx, "", vpc.ID, true)
	if err != nil || byVPC.ID != bound.ID || byVPC.Purpose != "public" {
		t.Fatal("Public lookup is ambiguous with two purposes", byVPC, err)
	}
	for index, on := range []bool{false, true} {
		_, err = f.e.SetVPCSnatEnabled(f.ctx, biz.EgressIntent{ID: bound.ID, ExpectedVersion: bound.Version, Enabled: on, IdempotencyKey: fmt.Sprint("public-toggle-", index)})
		if err != nil {
			t.Fatal(err)
		}
		bound = f.binding(t, bound.ID, biz.Available, &on)
		if snapshot() != before {
			t.Fatal("Public toggle changed base identity or desired state")
		}
	}
	if _, err = f.e.DeleteVPCSnatBinding(f.ctx, "", bound.ID); err != nil {
		t.Fatal(err)
	}
	f.binding(t, bound.ID, biz.Deleted, nil)
	if _, err = f.e.DeleteEIP(f.ctx, "", eip.ID); err != nil {
		t.Fatal(err)
	}
	f.drive(t, func() bool { v, err := f.e.GetEIP(f.ctx, "", eip.ID); return err == nil && v.State == biz.Deleted })
	if snapshot() != before {
		t.Fatal("Public detach or release changed base identity or desired state")
	}
	var active int
	if err = f.owner.QueryRow(f.ctx, `SELECT count(*) FROM network_eip_claims WHERE tenant_id=$1 AND eip_id=$2 AND released_at IS NULL`, f.tenant, base.EIPID).Scan(&active); err != nil || active != 1 {
		t.Fatal("Public detach released the base claim", active, err)
	}
}

func TestPublicBindingAndEnableIgnoreBaseReadAge(t *testing.T) {
	f := newEgressFixture(t)
	v := availableVPC(t, f.n, f.w, f.tenant, "base-independent")
	e := f.eip(t, "base-independent-eip")
	b, err := f.e.BindVPCSnat(f.ctx, biz.EgressIntent{VPCID: v.ID, EIPID: e.ID, IdempotencyKey: "before-base"})
	if err != nil {
		t.Fatal("missing base gated public binding", err)
	}
	on := true
	b = f.binding(t, b.ID, biz.Available, &on)
	seedEgressBaseConnectivity(t, f, v)
	if _, err = f.owner.Exec(f.ctx, `UPDATE network_vpc_base_connectivity SET observed_at=clock_timestamp()-interval '2 minutes' WHERE tenant_id=$1 AND vpc_id=$2`, f.tenant, v.ID); err != nil {
		t.Fatal(err)
	}
	for _, enabled := range []bool{false, true} {
		b, err = f.e.SetVPCSnatEnabled(f.ctx, biz.EgressIntent{ID: b.ID, ExpectedVersion: b.Version, Enabled: enabled, IdempotencyKey: fmt.Sprintf("with-old-base-%t", enabled)})
		if err != nil {
			t.Fatal("base read age gated public update", err)
		}
		b = f.binding(t, b.ID, biz.Available, &enabled)
	}
}

// DB projection only: LB runtime admission and Provider behavior are deliberately
// outside U04. Typed fixture identity verifies an LB claim cannot look unbound.
func TestEIPUnifiedClaimProjectionForReservedAndBoundLB(t *testing.T) {
	f := newEgressFixture(t)
	v := availableVPC(t, f.n, f.w, f.tenant, "lb-identity")
	e := f.eip(t, "lb-public-eip")
	s, err := f.n.CreateSubnet(f.ctx, biz.CreateSubnet{TenantID: f.tenant, VPCID: v.ID, Name: "lb-parent", CIDR: "10.42.1.0/24", IdempotencyKey: "lb-parent"})
	if err != nil {
		t.Fatal(err)
	}
	lbID := "lb_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = f.owner.Exec(f.ctx, `INSERT INTO network_load_balancers(tenant_id,lb_id,cluster_id,namespace,vpc_id,subnet_id,exposure,public_eip_id,state)
 SELECT tenant_id,$2,cluster_id,namespace,$3,$4,'public',eip_id,'provisioning' FROM network_eips WHERE tenant_id=$1 AND eip_id=$5`, f.tenant, lbID, v.ID, s.ID, e.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = f.owner.Exec(f.ctx, `INSERT INTO network_eip_claims(tenant_id,cluster_id,namespace,eip_id,target_kind,lb_id,state)
 SELECT tenant_id,cluster_id,namespace,eip_id,'load_balancer',$2,'reserved' FROM network_eips WHERE tenant_id=$1 AND eip_id=$3`, f.tenant, lbID, e.ID); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"reserved", "bound"} {
		if _, err = f.owner.Exec(f.ctx, `UPDATE network_eip_claims SET state=$3 WHERE tenant_id=$1 AND eip_id=$2 AND released_at IS NULL`, f.tenant, e.ID, state); err != nil {
			t.Fatal(err)
		}
		got, err := f.e.GetEIP(f.ctx, "", e.ID)
		if err != nil || got.BindingID != "" || got.BindingState != state || got.BindingTarget == nil || got.BindingTarget.Kind != "load_balancer" || got.BindingTarget.ID != lbID || got.BindingTarget.State != state {
			t.Fatal("LB EIP claim projected incorrectly", got, err)
		}
		listed, _, _, err := f.e.ListEIPs(f.ctx, biz.ListVPCs{Limit: 100})
		if err != nil || len(listed) != 1 || listed[0].BindingTarget == nil || listed[0].BindingState != state || listed[0].BindingID != "" {
			t.Fatal("list lost LB claim", listed, err)
		}
		if _, err = f.e.DeleteEIP(f.ctx, "", e.ID); biz.ReasonOf(err) != biz.EIPInUse {
			t.Fatal("release ignored LB claim", err)
		}
	}
	// A later worker step is not needed for GET/List; assert they did not enqueue
	// any new work or provider mutation while constructing the claim projection.
	var lastObserved time.Time
	if err = f.owner.QueryRow(f.ctx, `SELECT observed_at FROM network_eips WHERE tenant_id=$1 AND eip_id=$2`, f.tenant, e.ID).Scan(&lastObserved); err != nil {
		t.Fatal(err)
	}
	if !lastObserved.Equal(*e.ObservedAt) {
		t.Fatal("GET/List changed observed facts")
	}
}
