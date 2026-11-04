package vm

import (
	"context"
	"strconv"
	"strings"

	"github.com/google/uuid"
	network "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
)

// Network uses public domain ports; it never reads Network tables or CRs.
type Network struct {
	attachments *network.Attachments
	vnics       *network.Network
	repo        biz.Repository
}

func NewNetwork(a *network.Attachments, r biz.Repository) (*Network, error) {
	if a == nil || r == nil {
		return nil, biz.ErrInvalid
	}
	return &Network{attachments: a, repo: r}, nil
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
	return biz.NetworkPlan{SnapshotRevision: 1, Consumer: &biz.NetworkConsumerState{State: "open", DesiredRunState: strings.ToLower(v.Desired)}, AttachmentID: a.ID, SubmissionID: submission, Generation: 1, Version: a.Version, BindingRevision: a.BindingRevision, Namespace: p.Namespace, NADName: p.NADName, NADUID: p.NADUID, NADDigest: p.NADConfigDigest, KCNSubnetRef: p.KCNSubnetRef, Labels: labels}, nil
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
func (n *Network) Confirm(ctx context.Context, v biz.VM, x biz.Observation) error {
	if !x.Running || x.Launcher == nil {
		return nil
	}
	c := v.Network.Consumer
	if c == nil || c.State != "open" || c.CurrentLauncher == nil || c.CurrentLauncher.UID != x.Launcher.UID || v.Network.SnapshotRevision < 1 {
		return biz.ErrDependency
	}
	a, err := n.attachment(ctx, v)
	if err != nil {
		return err
	}
	if a.ConfirmedPodUID == x.Launcher.UID && a.ConfirmedSnapshotRevision == v.Network.SnapshotRevision {
		return nil
	}
	_, err = n.attachments.Confirm(ctx, network.ConfirmAttachment{TenantID: v.TenantID, AttachmentID: a.ID, ClusterID: v.Spec.ClusterID, Namespace: v.Spec.Namespace, PodName: x.Launcher.Name, PodUID: x.Launcher.UID, ExpectedVersion: a.Version, ExpectedSnapshotRevision: v.Network.SnapshotRevision})
	return err
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
	a, e = n.attachments.Release(ctx, network.ReleaseAttachment{TenantID: v.TenantID, AttachmentID: a.ID, FinalizationID: snapshot.FinalizationID, ExpectedVersion: a.Version})
	return a.State == network.Released, e
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
	current := v.Network.Consumer
	if current == nil || v.Network.SnapshotRevision < 1 {
		return network.ConsumerSubmission{}, biz.ErrDependency
	}
	c := network.ConsumerSubmission{
		ProtocolVersion: 3, TenantID: v.TenantID, InstanceID: v.ID, SubmissionID: a.SubmissionID, AttachmentID: a.ID,
		ClusterID: v.Spec.ClusterID, Namespace: v.Spec.Namespace, Generation: a.Generation,
		State: current.State, DesiredRunState: current.DesiredRunState, FinalizationID: current.FinalizationID,
		ClosedAt: current.ClosedAt, SnapshotRevision: v.Network.SnapshotRevision,
	}
	for _, choice := range []biz.NetworkInterfaceChoice{biz.PrimaryNetworkInterface(v).Desired, biz.PrimaryNetworkInterface(v).Applied} {
		if choice.Mode != "manual" {
			continue
		}
		found := false
		for _, ref := range c.ManualVNics {
			found = found || ref.VNicID == choice.VNicID
		}
		if !found {
			c.ManualVNics = append(c.ManualVNics, network.ManualVNicReference{VNicID: choice.VNicID, ProviderName: choice.ProviderName, ProviderUID: choice.ProviderUID})
		}
	}
	if current.CurrentLauncher != nil {
		pod := network.PodReference{Reference: nativeRef(*current.CurrentLauncher, "v1")}
		if current.CurrentVMI != nil {
			pod.Owner = nativeRef(*current.CurrentVMI, "kubevirt.io/v1")
		}
		c.CurrentPods = []network.PodReference{pod}
		c.PodUIDs = []string{pod.Reference.UID}
	}
	return c, nil
}
func (n *Network) Labels(v biz.VM) map[string]string {
	return map[string]string{"network.ani.io/tenant-id": v.TenantID, "network.ani.io/instance-id": v.ID, "network.ani.io/attachment-id": v.Network.AttachmentID, "network.ani.io/submission-id": v.Network.SubmissionID, "network.ani.io/generation": strconv.FormatInt(v.Network.Generation, 10)}
}
