package data_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/tenantnamespace"
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

func TestNetworkChildAdmissionUsesOnlyRegisteredManagementFacts(t *testing.T) {
	f := newAttachmentFixture(t)
	staleNetworkProjection(t, f)
	// No child acceptance may probe the Namespace or KCN again.
	f.p.ConfigureTenantNamespaceResolver(func(context.Context, string) (tenantnamespace.Identity, error) {
		return tenantnamespace.Identity{}, errors.New("unexpected namespace read")
	})
	f.api.Mu.Lock()
	for key, obj := range f.api.Objects {
		if strings.HasPrefix(key, "subnets/") || strings.HasPrefix(key, "vpcs/") {
			obj["metadata"].(map[string]any)["uid"] = "different-live-object"
		}
	}
	f.api.Mu.Unlock()
	ctx := context.Background()
	if _, err := f.n.CreateSubnet(ctx, biz.CreateSubnet{TenantID: f.r.TenantID, VPCID: f.r.VPCID, Name: "new-subnet", CIDR: "10.42.2.0/24", IdempotencyKey: "new-subnet"}); err != nil {
		t.Fatal(err)
	}
	v, err := f.n.CreateVNic(ctx, biz.CreateVNic{TenantID: f.r.TenantID, SubnetID: f.r.SubnetID, Name: "unreported-vnic", IdempotencyKey: "unreported-vnic"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.n.ReserveVNic(ctx, biz.VNicReservation{TenantID: v.TenantID, VNicID: v.ID, ConsumerKind: "vm", ConsumerID: "managed-vm", Slot: "primary", OperationID: uuid.NewString(), SubnetID: v.SubnetID, Revision: 1}); err != nil {
		t.Fatal("missing UID/MAC/IP became a reservation gate", err)
	}
	if _, err = f.a.Prepare(ctx, f.r); err != nil {
		t.Fatal("Attachment repeated provider readiness", err)
	}
	var unchanged bool
	if err = f.owner.QueryRow(ctx, `SELECT state='degraded' AND reason='BASE_CONNECTIVITY_NOT_READY' AND observed_at<clock_timestamp()-interval '60 seconds' FROM network_vpcs WHERE tenant_id=$1 AND vpc_id=$2`, f.r.TenantID, f.r.VPCID).Scan(&unchanged); err != nil || !unchanged {
		t.Fatal("admission changed provider status", err)
	}
}

func TestNetworkChildAdmissionStillRejectsClosedParents(t *testing.T) {
	for _, table := range []string{"network_vpcs", "network_subnets"} {
		t.Run(table, func(t *testing.T) {
			f := newAttachmentFixture(t)
			ctx := context.Background()
			if _, err := f.owner.Exec(ctx, "UPDATE "+table+" SET state='deleting' WHERE tenant_id=$1", f.r.TenantID); err != nil {
				t.Fatal(err)
			}
			if _, err := f.n.CreateVNic(ctx, biz.CreateVNic{TenantID: f.r.TenantID, SubnetID: f.r.SubnetID, Name: "closed", IdempotencyKey: "closed"}); biz.ReasonOf(err) != biz.ParentNotReady {
				t.Fatal(err)
			}
			if _, err := f.a.Prepare(ctx, f.r); biz.ReasonOf(err) != biz.NetworkNotReady {
				t.Fatal(err)
			}
		})
	}
}

func TestVMNetworkSelectionIgnoresProviderProjectionAge(t *testing.T) {
	f, _, _ := newAutoVMNetwork(t)
	policy := awaitAutoVMNetwork(t, f)
	f.r.WorkloadMode, f.r.ConsumerType = "vm_managed", biz.ConsumerVirtualMachine
	f.r.VMExpectation = &biz.VMNetworkExpectation{NADName: policy.NADName, NADUID: policy.NADUID, NADConfigDigest: policy.NADConfigDigest}
	staleNetworkProjection(t, f)
	if _, err := f.p.ResolveVMNetworkSelection(context.Background(), f.r); err != nil {
		t.Fatal(err)
	}
	if _, err := f.a.Prepare(context.Background(), f.r); err != nil {
		t.Fatal(err)
	}
}
