//go:build instanceintegration

package vm

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	network "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	"github.com/zhangzhe-ctrl/ani-resource-service/tests/testenv"
)

func TestVMPGAdmissionReplayAndDeleteCarryOnlyPendingPrepare(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(map[bool]string{false: "first-round-saved", true: "second-round-checkpoint"}[pending], func(t *testing.T) {
			ctx := context.Background()
			db := testenv.NewDatabase(t)
			if err := Migrate(ctx, db.Owner.Config().ConnString(), db.RuntimeRole); err != nil {
				t.Fatal(err)
			}
			p, err := OpenPostgres(ctx, db.RuntimeDSN)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			tenant, id, createID := uuid.NewString(), uuid.NewString(), uuid.NewString()
			reference := network.NetworkReference{OwnerScope: network.OwnerScopePlatform, VPCID: "vpc_" + strings.Repeat("a", 32), SubnetID: "subnet_" + strings.Repeat("b", 32), SubnetBindingID: uuid.NewString()}
			closed, checks := false, 0
			p.UseNetworkAdmission(func(context.Context, string, biz.Spec) (biz.NetworkSelection, error) {
				checks++
				if closed {
					return biz.NetworkSelection{}, biz.ErrNetworkAdmissionClosed
				}
				return biz.NetworkSelection{Reference: reference, NADConfig: `{"type":"kc-networking","server_socket":"/run/original.sock"}`}, nil
			})
			a := biz.Acceptance{TenantID: tenant, VMID: id, OperationID: createID, RequestDigest: strings.Repeat("c", 64), Spec: biz.Spec{Name: "network-acceptance", Digest: strings.Repeat("d", 64), Canonical: []byte(`{"network":{"nad_uid":""}}`), Network: biz.NetworkSelection{VPCID: reference.VPCID, SubnetID: reference.SubnetID, NADName: "ani-vm-primary-v1", NADDigest: strings.Repeat("e", 64)}}}
			if _, err = p.AcceptCreate(ctx, a); err != nil {
				t.Fatal(err)
			}
			closed = true
			if _, err = p.AcceptCreate(ctx, a); err != nil || checks != 1 {
				t.Fatal("accepted replay reopened the directory", err, checks)
			}
			newRequest := a
			newRequest.VMID, newRequest.OperationID = uuid.NewString(), uuid.NewString()
			if _, err = p.AcceptCreate(ctx, newRequest); !errors.Is(err, biz.ErrNetworkAdmissionClosed) {
				t.Fatal("closed directory admitted a new VM", err)
			}
			if _, err = p.Get(ctx, tenant, newRequest.VMID); !errors.Is(err, biz.ErrNotFound) {
				t.Fatal("rejected admission left an owner", err)
			}
			op, v, err := p.Claim(ctx, uuid.NewString(), time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if v.Spec.Network.Reference != reference || v.Spec.Network.NADConfig == "" || v.Spec.Network.NADUID != "" || string(v.Spec.Canonical) != `{"network":{"nad_uid":""}}` {
				t.Fatal("accepted snapshot lost internal data or rewrote canonical")
			}
			v.Network = biz.NetworkPlan{NADUID: "actual-nad", NADName: v.Spec.Network.NADName, NADDigest: v.Spec.Network.NADDigest, BindingRevision: reference.SubnetBindingID}
			if pending {
				op.Pending = &biz.Mutation{Action: "prepare-network", Submitted: true}
				err = p.Checkpoint(ctx, op, v)
			} else {
				err = p.Save(ctx, op, v)
			}
			if err != nil {
				t.Fatal(err)
			}
			deleted, err := p.AcceptDelete(ctx, biz.Acceptance{TenantID: tenant, VMID: id, OperationID: uuid.NewString(), CreateOperationID: createID, ExpectedVersion: 1, RequestDigest: strings.Repeat("f", 64)})
			if err != nil {
				t.Fatal(err)
			}
			if pending != (deleted.Pending != nil) || (pending && (deleted.Pending.Action != "prepare-network" || !deleted.Pending.Submitted)) {
				t.Fatal("delete checkpoint did not match the possible second-round send", deleted.Pending)
			}
		})
	}
}
