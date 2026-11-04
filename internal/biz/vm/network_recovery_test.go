package vm

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

type pendingAttachmentRecovery struct {
	NetworkPort
	calls int
}

func (p *pendingAttachmentRecovery) Prepare(_ context.Context, v VM) (NetworkPlan, error) {
	p.calls++
	plan := v.Network
	plan.AttachmentID = "accepted-attachment"
	plan.SnapshotRevision = 1
	plan.Consumer = &NetworkConsumerState{State: "open", DesiredRunState: "deleted"}
	return plan, nil
}

type networkRecoveryNative struct {
	NativePort
	stop         error
	observations int
}

func (n *networkRecoveryNative) Observe(context.Context, VM) (Observation, error) {
	n.observations++
	return Observation{}, n.stop
}

func TestDeleteRecoversOnlyThePendingSecondNetworkPrepare(t *testing.T) {
	for _, sent := range []bool{false, true} {
		t.Run(map[bool]string{false: "first-round-only", true: "second-round-may-have-committed"}[sent], func(t *testing.T) {
			network := &pendingAttachmentRecovery{}
			native := &networkRecoveryNative{stop: errors.New("continue own native cleanup")}
			worker := &Worker{network: network, native: native}
			v := VM{TenantID: uuid.NewString(), ID: uuid.NewString(), Closing: true, Desired: "Deleted", Spec: Spec{Media: Source{Kind: "DISK_IMAGE"}, BootDevice: "SYSTEM_DISK"}, Network: NetworkPlan{NADUID: "actual-nad", BasicIsolationName: "ani-tenant-original"}}
			op := Operation{Action: DeleteAction}
			if sent {
				op.Pending = &Mutation{Action: "prepare-network", Submitted: true}
			}
			err := worker.remove(context.Background(), &op, &v)
			if !sent {
				if !errors.Is(err, native.stop) || network.calls != 0 || native.observations != 1 || v.Network.AttachmentID != "" {
					t.Fatal("delete manufactured an Attachment after an unsent first round", err, network.calls)
				}
				return
			}
			if err != nil || network.calls != 1 || native.observations != 0 || op.Pending != nil || v.Network.AttachmentID == "" || v.Network.Consumer == nil || v.Network.Consumer.State != "closing" || v.Network.Consumer.FinalizationID == "" || v.Network.BasicIsolationName != "ani-tenant-original" {
				t.Fatal("delete lost pending Attachment recovery or did not publish closing", v.Network, err)
			}
		})
	}
}
