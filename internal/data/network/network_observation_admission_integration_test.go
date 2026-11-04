package data_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	data "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network"
)

func staleNetworkProjection(t *testing.T, f attachmentFixture) {
	t.Helper()
	if _, err := f.owner.Exec(context.Background(), `UPDATE network_vpcs SET state='degraded',reason='BASE_CONNECTIVITY_NOT_READY',observed_at=clock_timestamp()-interval '5 minutes' WHERE tenant_id=$1 AND vpc_id=$2`, f.r.TenantID, f.r.VPCID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.owner.Exec(context.Background(), `UPDATE network_subnets SET reason='PROVIDER_UNAVAILABLE',observed_at=clock_timestamp()-interval '5 minutes' WHERE tenant_id=$1 AND subnet_id=$2`, f.r.TenantID, f.r.SubnetID); err != nil {
		t.Fatal(err)
	}
}

func TestVNicAdmissionPGSurvivesBaseObservationFailure(t *testing.T) {
	f := newAttachmentFixture(t)
	staleNetworkProjection(t, f)
	ctx := context.Background()
	r := biz.CreateVNic{TenantID: f.r.TenantID, SubnetID: f.r.SubnetID, Name: "current dependencies", IdempotencyKey: "current-dependencies"}
	v, err := f.n.CreateVNic(ctx, r)
	if err != nil {
		t.Fatalf("current original VPC/Subnet rejected because of base projection: %v", err)
	}
	if replay, err := f.n.CreateVNic(ctx, r); err != nil || replay.ID != v.ID {
		t.Fatal("lost admission replay", replay, err)
	}
	var unchanged bool
	if err := f.owner.QueryRow(ctx, `SELECT state='degraded' AND reason='BASE_CONNECTIVITY_NOT_READY' AND observed_at<clock_timestamp()-interval '60 seconds' FROM network_vpcs WHERE tenant_id=$1 AND vpc_id=$2`, f.r.TenantID, f.r.VPCID).Scan(&unchanged); err != nil || !unchanged {
		t.Fatal("admission renewed observation or hid base degradation", err)
	}
}

func TestVNicReservationPGSurvivesBaseObservationFailure(t *testing.T) {
	f := newAttachmentFixture(t)
	v := manualVNic(t, f, "retained-manual")
	staleNetworkProjection(t, f)
	r := biz.VNicReservation{TenantID: v.TenantID, VNicID: v.ID, ConsumerKind: "vm", ConsumerID: "stopped-vm", Slot: "primary", OperationID: uuid.NewString(), SubnetID: v.SubnetID, Revision: 1}
	if _, err := f.n.ReserveVNic(context.Background(), r); err != nil {
		t.Fatalf("base projection blocked original manual VNIC reservation: %v", err)
	}
	f.api.Mu.Lock()
	f.api.Objects["vnics/"+v.Namespace+"/"+v.ProviderName]["metadata"].(map[string]any)["uid"] = "replacement-uid"
	f.api.Mu.Unlock()
	r.OperationID = uuid.NewString()
	if _, err := f.n.ReserveVNic(context.Background(), r); err == nil {
		t.Fatal("same-name VNIC replacement admitted")
	}
}

func TestVNicAdmissionPGStillRejectsClosedUnresolvedAndChangedNetworks(t *testing.T) {
	for _, fault := range []string{"parent-deleting", "parent-ownership", "subnet-deleting", "pending-parent", "pending-subnet", "replace-parent", "replace-subnet", "subnet-not-ready"} {
		t.Run(fault, func(t *testing.T) {
			f := newAttachmentFixture(t)
			staleNetworkProjection(t, f)
			ctx := context.Background()
			var err error
			switch fault {
			case "parent-deleting":
				_, err = f.owner.Exec(ctx, `UPDATE network_vpcs SET state='deleting' WHERE tenant_id=$1`, f.r.TenantID)
			case "parent-ownership":
				_, err = f.owner.Exec(ctx, `UPDATE network_vpcs SET reason='PROVIDER_OWNERSHIP_CONFLICT' WHERE tenant_id=$1`, f.r.TenantID)
			case "subnet-deleting":
				_, err = f.owner.Exec(ctx, `UPDATE network_subnets SET state='deleting' WHERE tenant_id=$1`, f.r.TenantID)
			case "pending-parent", "pending-subnet":
				id := f.r.VPCID
				if fault == "pending-subnet" {
					id = f.r.SubnetID
				}
				_, err = f.owner.Exec(ctx, `UPDATE network_provider_bindings SET pending_action='create',pending_since=clock_timestamp() WHERE tenant_id=$1 AND coalesce(vpc_id,subnet_id)=$2`, f.r.TenantID, id)
			default:
				f.api.Mu.Lock()
				prefix := "subnets/"
				if fault == "replace-parent" {
					prefix = "vpcs/"
				}
				for key, obj := range f.api.Objects {
					if strings.HasPrefix(key, prefix) {
						if fault == "subnet-not-ready" {
							obj["status"].(map[string]any)["observedGeneration"] = 0
						} else {
							obj["metadata"].(map[string]any)["uid"] = "replacement-uid"
						}
					}
				}
				f.api.Mu.Unlock()
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.n.CreateVNic(ctx, biz.CreateVNic{TenantID: f.r.TenantID, SubnetID: f.r.SubnetID, Name: fault, IdempotencyKey: fault}); err == nil {
				t.Fatal("unsafe current network admitted")
			}
			var count int
			if err = f.owner.QueryRow(ctx, `SELECT count(*) FROM network_vnics WHERE tenant_id=$1`, f.r.TenantID).Scan(&count); err != nil || count != 0 {
				t.Fatal("rejected request persisted a VNIC", count, err)
			}
		})
	}
}

func TestVMNetworkPreflightPGSurvivesBaseObservationFailure(t *testing.T) {
	f, _, _ := newAutoVMNetwork(t)
	policy := awaitAutoVMNetwork(t, f)
	f.r.WorkloadMode = "vm_managed"
	f.r.ConsumerType = biz.ConsumerVirtualMachine
	f.r.VMExpectation = &biz.VMNetworkExpectation{NADName: policy.NADName, NADUID: policy.NADUID, NADConfigDigest: policy.NADConfigDigest}
	staleNetworkProjection(t, f)
	if _, err := f.p.ResolveVMNetworkSelection(context.Background(), f.r, time.Minute); err != nil {
		t.Fatalf("base projection blocked current VM network: %v", err)
	}
	if _, err := f.a.Prepare(context.Background(), f.r); err != nil {
		t.Fatalf("Prepare retained base aggregate gate: %v", err)
	}
}

func TestVNicAdmissionPGVerifiesPersistedSubnetOptions(t *testing.T) {
	f := newAttachmentFixture(t)
	ctx := context.Background()
	s, err := f.n.CreateSubnet(ctx, biz.CreateSubnet{TenantID: f.r.TenantID, VPCID: f.r.VPCID, Name: "dhcp", CIDR: "10.42.2.0/24", IdempotencyKey: "dhcp", SubnetOptions: biz.SubnetOptions{EnableDHCP: true, ExcludeIPs: []string{"10.42.2.10..10.42.2.20"}, DNSServers: []string{"10.96.0.10"}}})
	if err != nil {
		t.Fatal(err)
	}
	s = driveSubnet(t, f.n, f.resourceWorker, s, biz.Available)
	r := biz.CreateVNic{TenantID: f.r.TenantID, SubnetID: s.ID, Name: "dhcp-nic", IdempotencyKey: "dhcp-nic"}
	if _, err = f.n.CreateVNic(ctx, r); err != nil {
		t.Fatalf("accepted Subnet options rejected by current proof: %v", err)
	}
	f.api.Mu.Lock()
	for key, obj := range f.api.Objects {
		if strings.HasPrefix(key, "subnets/") && obj["spec"].(map[string]any)["cidrBlock"] == s.CIDR {
			obj["spec"].(map[string]any)["enableDHCP"] = false
		}
	}
	f.api.Mu.Unlock()
	r.IdempotencyKey = "changed-dhcp"
	if _, err = f.n.CreateVNic(ctx, r); err == nil {
		t.Fatal("changed Subnet configuration admitted")
	}
}

func TestVNicAdmissionPGDuringActualBaseObservationGap(t *testing.T) {
	f, _, _, kube := newBaseKCFixture(t, nil)
	v := baseState(t, f, createBaseVPC(t, f, "observation-gap-admission").ID, biz.Available)
	eipID, snatID := baseIDs(t, f, v.ID)
	s, err := f.n.CreateSubnet(f.ctx, biz.CreateSubnet{TenantID: f.tenant, VPCID: v.ID, Name: "inside", CIDR: "10.42.1.0/24", IdempotencyKey: "inside"})
	if err != nil {
		t.Fatal(err)
	}
	s = driveSubnet(t, f.n, f.w, s, biz.Available)
	f.stopObservation()
	provider, err := data.OpenKCProvider(f.p, kube, data.KCClientPolicy{QPS: 1000, Burst: 1000})
	if err != nil {
		t.Fatal(err)
	}
	f.p.UseEgressInfrastructure(provider)
	options := data.DefaultObservationOptions()
	options.AuditInterval = 50 * time.Millisecond
	options.AuditJitter = time.Millisecond
	observer, err := provider.EnableObservation(options)
	if err != nil {
		t.Fatal(err)
	}
	target := biz.ProviderTarget{TenantID: f.tenant, ResourceID: snatID, Kind: "snat", VPCID: v.ID, Egress: &biz.EgressWorkSpec{EIPID: eipID, DesiredEnabled: true}, Direct: true}
	if err = f.owner.QueryRow(f.ctx, `SELECT binding_id,provider_uid FROM network_provider_bindings WHERE tenant_id=$1 AND snat_id=$2`, f.tenant, snatID).Scan(&target.BindingID, &target.KnownIdentity); err != nil {
		t.Fatal(err)
	}
	if proof, err := provider.Observe(f.ctx, target); err != nil || !proof.Ready {
		t.Fatal("baseline current proof", proof, err)
	}
	policy := biz.DefaultWorkerPolicy()
	f.w, err = biz.NewWorker(f.p, provider, uuid.NewString(), policy)
	if err != nil {
		t.Fatal(err)
	}
	// Pause only the external shared observation lifecycle. Keep original KCN
	// objects and all business facts; shortened thresholds make the gap fast.
	time.Sleep(120 * time.Millisecond)
	baseFactsStep(t, f, eipID)
	f.p.UseBaseConnectivityFreshness(100 * time.Millisecond)
	baseFactsStep(t, f, v.ID)
	baseFactsStep(t, f, snatID)
	var exactSymptom bool
	if err = f.owner.QueryRow(f.ctx, `SELECT v.state='degraded' AND v.reason='BASE_CONNECTIVITY_NOT_READY' AND v.observed_at>clock_timestamp()-interval '1 second' AND b.observed_at IS NULL AND e.reason='PROVIDER_UNAVAILABLE' AND s.applied_enabled IS TRUE FROM network_vpcs v JOIN network_vpc_base_connectivity b USING(tenant_id,vpc_id) JOIN network_eips e ON e.tenant_id=v.tenant_id AND e.eip_id=b.eip_id JOIN network_snat_bindings s ON s.tenant_id=v.tenant_id AND s.snat_id=b.snat_id WHERE v.tenant_id=$1 AND v.vpc_id=$2`, f.tenant, v.ID).Scan(&exactSymptom); err != nil || !exactSymptom {
		t.Fatal("actual gap did not reach target aggregate condition", exactSymptom, err)
	}
	before := observer.Snapshot()["requests_LIST_eips_total"]
	if _, err = f.n.CreateVNic(f.ctx, biz.CreateVNic{TenantID: f.tenant, SubnetID: s.ID, Name: "survives-gap", IdempotencyKey: "survives-gap"}); err != nil {
		t.Fatalf("observation failure blocked actual admission: %v", err)
	}
	if after := observer.Snapshot()["requests_LIST_eips_total"]; after != before {
		t.Fatal("admission triggered another full topology scan", before, after)
	}
}
