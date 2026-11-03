package vm

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	network "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
)

// Network uses public domain ports; it never reads Network tables or CRs.
type Network struct {
	attachments *network.Attachments
	repo        biz.Repository
	native      biz.NativePort
}

func NewNetwork(a *network.Attachments, r biz.Repository, n biz.NativePort) (*Network, error) {
	if a == nil || r == nil || n == nil {
		return nil, biz.ErrInvalid
	}
	return &Network{a, r, n}, nil
}
func (n *Network) Prepare(ctx context.Context, v biz.VM) (biz.NetworkPlan, error) {
	submission := uuid.NewSHA1(uuid.NameSpaceOID, []byte("ani-vm-network/"+v.ID)).String()
	r := network.PrepareAttachment{WorkloadMode: "vm_managed", ConsumerType: network.ConsumerVirtualMachine, TenantID: v.TenantID, InstanceID: v.ID, VPCID: v.Spec.Network.VPCID, SubnetID: v.Spec.Network.SubnetID, Slot: "primary", RequestKey: "vm-" + v.ID, SubmissionID: submission, Generation: 1, ClusterID: v.Spec.ClusterID, Namespace: v.Spec.Namespace, VMExpectation: &network.VMNetworkExpectation{NADName: v.Spec.Network.NADName, NADUID: v.Spec.Network.NADUID, NADConfigDigest: v.Spec.Network.NADDigest}}
	a, e := n.attachments.Prepare(ctx, r)
	if e != nil {
		return biz.NetworkPlan{}, e
	}
	p := a.VMPrimary
	if a.WorkloadMode != "vm_managed" || a.TenantID != v.TenantID || a.InstanceID != v.ID || a.SubmissionID != submission || a.Generation != 1 || p == nil || p.Namespace != v.Spec.Namespace || p.NADUID != v.Spec.Network.NADUID || p.NADConfigDigest != v.Spec.Network.NADDigest {
		return biz.NetworkPlan{}, biz.ErrChanged
	}
	labels := map[string]string{"network.ani.io/tenant-id": v.TenantID, "network.ani.io/instance-id": v.ID, "network.ani.io/attachment-id": a.ID, "network.ani.io/submission-id": submission, "network.ani.io/generation": "1"}
	return biz.NetworkPlan{AttachmentID: a.ID, SubmissionID: submission, Generation: 1, Version: a.Version, BindingRevision: a.BindingRevision, Namespace: p.Namespace, NADName: p.NADName, NADUID: p.NADUID, NADDigest: p.NADConfigDigest, KCNSubnetRef: p.KCNSubnetRef, Labels: labels}, nil
}
func (n *Network) attachment(ctx context.Context, v biz.VM) (network.Attachment, error) {
	if v.Network.AttachmentID == "" {
		return network.Attachment{}, biz.ErrDependency
	}
	a, e := n.attachments.Get(ctx, v.TenantID, v.Network.AttachmentID)
	if e != nil {
		return a, e
	}
	if a.WorkloadMode != "vm_managed" || a.TenantID != v.TenantID || a.InstanceID != v.ID || a.SubmissionID != v.Network.SubmissionID || a.Generation != v.Network.Generation || a.BindingRevision != v.Network.BindingRevision {
		return a, biz.ErrChanged
	}
	return a, nil
}
func (n *Network) CurrentAddresses(ctx context.Context, v biz.VM) ([]biz.Address, error) {
	if v.Closing || v.Observed != "Running" {
		return nil, nil
	}
	a, e := n.attachment(ctx, v)
	if e != nil {
		return nil, e
	}
	out := []biz.Address{}
	for _, x := range a.VMCurrentAddresses {
		address := biz.Address{NIC: x.NICName, Address: x.Address, VMIUID: x.VMIUID, LauncherUID: x.LauncherPodUID}
		if a.ObservedAt != nil {
			address.ObservedAt = *a.ObservedAt
		}
		out = append(out, address)
	}
	return out, nil
}
func (n *Network) Observe(ctx context.Context, v biz.VM, x biz.Observation) ([]biz.Address, error) {
	a, e := n.attachment(ctx, v)
	if e != nil {
		return nil, e
	}
	if x.Launcher != nil && x.VMI != nil && x.Running {
		snapshot, e := n.GetVMNetworkSubmission(ctx, a)
		if e != nil {
			return nil, e
		}
		if a.ConfirmedPodUID != x.Launcher.UID || a.ConfirmedSnapshotRevision != snapshot.SnapshotRevision {
			_, e = n.attachments.Confirm(ctx, network.ConfirmAttachment{TenantID: v.TenantID, AttachmentID: a.ID, ClusterID: v.Spec.ClusterID, Namespace: v.Spec.Namespace, PodName: x.Launcher.Name, PodUID: x.Launcher.UID, ExpectedVersion: a.Version, ExpectedSnapshotRevision: snapshot.SnapshotRevision})
			if e != nil {
				return nil, e
			}
		}
	}
	if !x.Running {
		if (x.Stopped || x.Terminal) && len(a.VMCurrentAddresses) != 0 {
			return nil, biz.ErrDependency
		}
		return nil, nil
	}
	return n.CurrentAddresses(ctx, v)
}
func (n *Network) Release(ctx context.Context, v biz.VM) (bool, error) {
	if v.Network.AttachmentID == "" {
		return true, nil
	}
	a, e := n.attachment(ctx, v)
	if e != nil {
		return false, e
	}
	snapshot, e := n.GetVMNetworkSubmission(ctx, a)
	if e != nil {
		return false, e
	}
	if snapshot.State != "closed" {
		return false, biz.ErrDependency
	}
	if a.State == network.Released {
		return true, nil
	}
	_, e = n.attachments.Release(ctx, network.ReleaseAttachment{TenantID: v.TenantID, AttachmentID: a.ID, FinalizationID: snapshot.FinalizationID, ExpectedVersion: a.Version})
	return false, e
}
func nativeRef(r biz.ObjectRef, api string) network.WorkloadReference {
	return network.WorkloadReference{APIVersion: api, Kind: r.Kind, Namespace: r.Namespace, Name: r.Name, UID: r.UID}
}
func (n *Network) GetVMNetworkSubmission(ctx context.Context, a network.Attachment) (network.ConsumerSubmission, error) {
	v, e := n.repo.Get(ctx, a.TenantID, a.InstanceID)
	if e != nil {
		return network.ConsumerSubmission{}, e
	}
	if v.Network.AttachmentID != a.ID || v.Network.SubmissionID != a.SubmissionID || v.Network.Generation != a.Generation {
		return network.ConsumerSubmission{}, biz.ErrDependency
	}
	var x biz.Observation
	// Ordinary ownership snapshots need runtime identity only. Final closure
	// retains the full direct lifecycle observation and storage cleanup checks.
	if runtime, ok := n.native.(biz.ConsoleNativePort); ok && !v.Closing {
		x, e = runtime.ObserveConsole(ctx, v)
	} else {
		x, e = n.native.Observe(ctx, v)
	}
	if e != nil {
		return network.ConsumerSubmission{}, e
	}
	var old network.ConsumerSubmission
	if len(v.Network.Consumer) > 0 {
		if e = json.Unmarshal(v.Network.Consumer, &old); e != nil {
			return old, e
		}
	}
	c := network.ConsumerSubmission{ProtocolVersion: 3, TenantID: v.TenantID, InstanceID: v.ID, SubmissionID: a.SubmissionID, AttachmentID: a.ID, ClusterID: v.Spec.ClusterID, Namespace: v.Spec.Namespace, Generation: a.Generation, State: "open", DesiredRunState: strings.ToLower(v.Desired), SnapshotRevision: v.Network.SnapshotRevision, Controllers: append([]network.ControllerReference(nil), old.Controllers...), RetiredPods: append([]network.PodReference(nil), old.RetiredPods...)}
	if x.VM.UID != "" {
		c.RootController = nativeRef(x.VM, "kubevirt.io/v1")
	} else {
		c.RootController = old.RootController
	}
	if x.VMI != nil {
		ref := nativeRef(*x.VMI, "kubevirt.io/v1")
		found := false
		for _, r := range c.Controllers {
			found = found || r.Reference.UID == ref.UID
		}
		if !found {
			c.Controllers = append(c.Controllers, network.ControllerReference{Reference: ref, Owner: c.RootController})
		}
		if x.Launcher != nil {
			c.CurrentPods = []network.PodReference{{Reference: nativeRef(*x.Launcher, "v1"), Owner: ref}}
		}
	}
	for _, p := range old.CurrentPods {
		current := false
		for _, q := range c.CurrentPods {
			current = current || p.Reference.UID == q.Reference.UID
		}
		if !current {
			found := false
			for _, q := range c.RetiredPods {
				found = found || p.Reference.UID == q.Reference.UID
			}
			if !found {
				c.RetiredPods = append(c.RetiredPods, p)
			}
		}
	}
	for _, r := range c.Controllers {
		c.ControllerUIDs = append(c.ControllerUIDs, r.Reference.UID)
	}
	for _, r := range c.CurrentPods {
		c.PodUIDs = append(c.PodUIDs, r.Reference.UID)
	}
	if v.Closing {
		c.DesiredRunState = "deleted"
		c.State = "closing"
		c.FinalizationID = uuid.NewSHA1(uuid.NameSpaceOID, []byte("ani-vm-close/"+v.ID)).String()
		if x.Deleted {
			c.State = "closed"
			now := time.Now().UTC()
			c.ClosedAt = &now
			if old.ClosedAt != nil {
				c.ClosedAt = old.ClosedAt
			}
		}
	}
	if c.SnapshotRevision > 0 && network.ConsumerSnapshotHash(c) == network.ConsumerSnapshotHash(old) {
		return c, nil
	}
	c.SnapshotRevision++
	plan := v.Network
	plan.SnapshotRevision = c.SnapshotRevision
	plan.Consumer, e = json.Marshal(c)
	if e != nil {
		return c, e
	}
	if e = n.repo.UpdateNetwork(ctx, v, plan); e != nil {
		return c, e
	}
	return c, nil
}
func (n *Network) Labels(v biz.VM) map[string]string {
	return map[string]string{"network.ani.io/tenant-id": v.TenantID, "network.ani.io/instance-id": v.ID, "network.ani.io/attachment-id": v.Network.AttachmentID, "network.ani.io/submission-id": v.Network.SubmissionID, "network.ani.io/generation": strconv.FormatInt(v.Network.Generation, 10)}
}
