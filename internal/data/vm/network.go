package vm

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/google/uuid"
	network "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
)

// Network uses public domain ports; it never reads Network tables or CRs.
type Network struct {
	attachments   *network.Attachments
	vnics         *network.Network
	repo          biz.Repository
	prepareTenant func(context.Context, biz.VM) (biz.NetworkPlan, error)
}

func NewNetwork(a *network.Attachments, r biz.Repository) (*Network, error) {
	if a == nil || r == nil {
		return nil, biz.ErrInvalid
	}
	return &Network{attachments: a, repo: r}, nil
}

// UseTenantPreparation wires the first round from the explicit composition
// root. Prepare itself still talks only to Network's use cases and domain ports.
func (n *Network) UseTenantPreparation(prepare func(context.Context, biz.VM) (biz.NetworkPlan, error)) {
	n.prepareTenant = prepare
}

func (n *Network) Prepare(ctx context.Context, v biz.VM) (biz.NetworkPlan, error) {
	if v.Network.NADUID == "" {
		if n.prepareTenant == nil {
			return biz.NetworkPlan{}, biz.ErrDependency
		}
		// The worker saves this partial plan before issuing any Attachment.
		return n.prepareTenant(ctx, v)
	}
	submission := uuid.NewSHA1(uuid.NameSpaceOID, []byte("ani-vm-network/"+v.ID)).String()
	actual := v.Network
	r := network.PrepareAttachment{AcceptedReference: v.Spec.Network.Reference, NamespaceUID: v.Spec.NamespaceUID, WorkloadMode: "vm_managed", ConsumerType: network.ConsumerVirtualMachine, TenantID: v.TenantID, InstanceID: v.ID, VPCID: v.Spec.Network.VPCID, SubnetID: v.Spec.Network.SubnetID, Slot: "primary", RequestKey: "vm-" + v.ID, SubmissionID: submission, Generation: 1, ClusterID: v.Spec.ClusterID, Namespace: v.Spec.Namespace, VMExpectation: &network.VMNetworkExpectation{NADName: actual.NADName, NADUID: actual.NADUID, NADConfigDigest: actual.NADDigest}}
	// Permanent Attachment replay is the first I/O in this second round. Never
	// reopen namespace, BNI, template or catalog admission before that replay.
	a, err := n.attachments.PrepareAccepted(ctx, r)
	if err != nil {
		return biz.NetworkPlan{}, MapNetworkError(err)
	}
	p := a.VMPrimary
	// A replay from before binding simplification retains its opaque revision.
	// Compare its binding ID with the first-round identity, then persist the
	// original replay token below without treating its suffix as a new gate.
	attachmentBindingID, _, _ := strings.Cut(a.BindingRevision, ":")
	actualBindingID, _, _ := strings.Cut(actual.BindingRevision, ":")
	if a.WorkloadMode != "vm_managed" || a.TenantID != v.TenantID || a.InstanceID != v.ID || a.SubmissionID != submission || a.Generation != 1 || attachmentBindingID != actualBindingID || p == nil || p.Namespace != v.Spec.Namespace || p.NADName != actual.NADName || p.NADUID != actual.NADUID || p.NADConfigDigest != actual.NADDigest || p.KCNSubnetRef != actual.KCNSubnetRef {
		return biz.NetworkPlan{}, biz.ErrChanged
	}
	labels := map[string]string{"network.ani.io/tenant-id": v.TenantID, "network.ani.io/instance-id": v.ID, "network.ani.io/attachment-id": a.ID, "network.ani.io/submission-id": submission, "network.ani.io/generation": "1"}
	return biz.NetworkPlan{SnapshotRevision: 1, Consumer: &biz.NetworkConsumerState{State: "open", DesiredRunState: strings.ToLower(v.Desired)}, AttachmentID: a.ID, SubmissionID: submission, Generation: 1, Version: a.Version, BindingRevision: a.BindingRevision, Namespace: p.Namespace, NADName: p.NADName, NADUID: p.NADUID, NADDigest: p.NADConfigDigest, KCNSubnetRef: p.KCNSubnetRef, BasicIsolationName: actual.BasicIsolationName, Labels: labels}, nil
}

// MapNetworkError preserves the VM domain's public error vocabulary instead
// of allowing an admission or original-identity error to become Unavailable.
func MapNetworkError(err error) error {
	if err == nil {
		return nil
	}
	var provider *network.ProviderError
	if errors.As(err, &provider) && provider.Kind == network.ProviderConflict {
		return biz.ErrOwnership
	}
	switch network.ReasonOf(err) {
	case network.NetworkNewAdmissionClosed:
		return biz.ErrNetworkAdmissionClosed
	case network.ProviderOwnership:
		return biz.ErrOwnership
	case network.AttachmentConflict, network.AttachmentProtocol, network.PlacementMismatch:
		return biz.ErrChanged
	case network.ResourceNotFound:
		return biz.ErrNotFound
	case network.InvalidArgument:
		return biz.ErrInvalid
	case network.NetworkNotReady, network.DependencyUnavailable:
		return biz.ErrDependency
	default:
		return err
	}
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
