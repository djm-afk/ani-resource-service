package instance

import (
	"context"
	"strconv"
	"strings"
	"time"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/instance"
	networkbiz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
)

// InstanceNetwork calls Network's use cases in process. It neither reads
// Network tables nor shares an Instance transaction with Network.
type InstanceNetwork struct {
	network                    *networkbiz.Network
	attachments                *networkbiz.Attachments
	clusterID, namespacePrefix string
	freshness                  time.Duration
}

func NewInstanceNetwork(network *networkbiz.Network, attachments *networkbiz.Attachments, clusterID, namespacePrefix string, freshness ...time.Duration) (*InstanceNetwork, error) {
	if network == nil || attachments == nil || !biz.UUID(clusterID) || len(freshness) > 1 {
		return nil, biz.ErrInvalid
	}
	staleAfter := 30 * time.Second
	if len(freshness) == 1 {
		staleAfter = freshness[0]
	}
	if staleAfter <= 0 {
		return nil, biz.ErrInvalid
	}
	if _, err := biz.ResolveNamespace("11111111-1111-4111-8111-111111111111", "", namespacePrefix); err != nil {
		return nil, biz.ErrInvalid
	}
	return &InstanceNetwork{network: network, attachments: attachments, clusterID: clusterID, namespacePrefix: namespacePrefix, freshness: staleAfter}, nil
}
func (n *InstanceNetwork) placement(tenant, cluster, namespace string) error {
	expected, err := biz.ResolveNamespace(tenant, "", n.namespacePrefix)
	if err != nil {
		return biz.ErrInvalid
	}
	if cluster != n.clusterID || namespace != expected {
		return biz.ErrNetworkConflict
	}
	return nil
}
func (n *InstanceNetwork) ValidateSelection(ctx context.Context, tenant, cluster, namespace string, selection biz.NetworkSelection) error {
	if err := n.placement(tenant, cluster, namespace); err != nil {
		return err
	}
	subnet, err := n.network.GetSubnet(ctx, tenant, selection.SubnetID)
	if err != nil {
		return instanceNetworkError(err)
	}
	if subnet.TenantID != tenant || subnet.ID != selection.SubnetID {
		return biz.ErrNetworkNotFound
	}
	if subnet.VPCID != selection.VPCID {
		return biz.ErrNetworkConflict
	}
	if subnet.State != networkbiz.Available || subnet.ObservationStale {
		return biz.ErrNetworkUnavailable
	}
	vpc, err := n.network.GetVPC(ctx, tenant, selection.VPCID)
	if err != nil {
		return instanceNetworkError(err)
	}
	if vpc.TenantID != tenant || vpc.ID != selection.VPCID {
		return biz.ErrNetworkNotFound
	}
	if vpc.State != networkbiz.Available || vpc.ObservationStale {
		return biz.ErrNetworkUnavailable
	}
	return nil
}
func (n *InstanceNetwork) runtime(i biz.Instance) error {
	if i.Network == nil || i.Spec.Network == nil || !biz.UUID(i.Network.SubmissionID) || i.Network.RequestKey == "" || i.Network.Generation < 1 {
		return biz.ErrNetworkUnavailable
	}
	return n.placement(i.TenantID, i.Spec.ClusterID, i.Spec.Namespace)
}
func networkAttachmentMatches(i biz.Instance, a networkbiz.Attachment) bool {
	return i.Network != nil && i.Spec.Network != nil && a.WorkloadMode == "controller_managed" && a.TenantID == i.TenantID && a.InstanceID == i.ID && a.SubmissionID == i.Network.SubmissionID && a.Generation == i.Network.Generation && a.ClusterID == i.Spec.ClusterID && a.Namespace == i.Spec.Namespace && a.VPCID == i.Spec.Network.VPCID && a.SubnetID == i.Spec.Network.SubnetID && (i.Network.AttachmentID == "" || a.ID == i.Network.AttachmentID)
}
func (n *InstanceNetwork) Prepare(ctx context.Context, i biz.Instance) (biz.NetworkBinding, error) {
	if err := n.runtime(i); err != nil {
		return biz.NetworkBinding{}, err
	}
	// Permanent replay must precede dynamic availability checks. Prepare owns
	// that decision and verifies the actual provider binding on first accept.
	a, err := n.attachments.Prepare(ctx, networkbiz.PrepareAttachment{TenantID: i.TenantID, InstanceID: i.ID, VPCID: i.Spec.Network.VPCID, SubnetID: i.Spec.Network.SubnetID, Slot: "primary", RequestKey: i.Network.RequestKey, SubmissionID: i.Network.SubmissionID, Generation: i.Network.Generation, ClusterID: i.Spec.ClusterID, Namespace: i.Spec.Namespace, WorkloadMode: "controller_managed", ConsumerType: networkbiz.ConsumerContainerInstance})
	if err != nil {
		return biz.NetworkBinding{}, instanceNetworkError(err)
	}
	if !networkAttachmentMatches(i, a) {
		return biz.NetworkBinding{}, biz.ErrNetworkConflict
	}
	if a.State == networkbiz.Releasing || a.State == networkbiz.Released {
		return biz.NetworkBinding{}, biz.ErrNetworkConflict
	}
	if a.Plan == nil {
		return biz.NetworkBinding{}, biz.ErrNetworkUnavailable
	}
	p := a.Plan
	if p.FormatVersion != 1 || p.TenantID != i.TenantID || p.InstanceID != i.ID || p.AttachmentID != a.ID || p.SubmissionID != i.Network.SubmissionID || p.Generation != i.Network.Generation || p.Namespace != i.Spec.Namespace || p.PrimaryNetworkRef == "" {
		return biz.NetworkBinding{}, biz.ErrNetworkConflict
	}
	return biz.NetworkBinding{AttachmentID: a.ID, BindingRevision: a.BindingRevision, PrimaryNetworkRef: p.PrimaryNetworkRef, Version: a.Version}, nil
}
func (n *InstanceNetwork) Observe(ctx context.Context, i biz.Instance) (biz.NetworkObservation, error) {
	if err := n.runtime(i); err != nil {
		return biz.NetworkObservation{}, err
	}
	if i.Network.AttachmentID == "" {
		return biz.NetworkObservation{}, biz.ErrNetworkUnavailable
	}
	a, err := n.attachments.Get(ctx, i.TenantID, i.Network.AttachmentID)
	if err != nil {
		return biz.NetworkObservation{}, instanceNetworkError(err)
	}
	if !networkAttachmentMatches(i, a) {
		return biz.NetworkObservation{}, biz.ErrNetworkConflict
	}
	if a.ObservedAt == nil || time.Since(*a.ObservedAt) > n.freshness {
		return biz.NetworkObservation{}, biz.ErrNetworkUnavailable
	}
	mode := "manual"
	if strings.HasPrefix(a.CurrentVNicName, "auto-") {
		mode = "automatic"
	}
	var observed time.Time
	if a.ObservedAt != nil {
		observed = *a.ObservedAt
	}
	return biz.NetworkObservation{Confirmed: a.State == networkbiz.Attached && a.Reason == "" && a.ConfirmedPodUID == a.PodUID && a.PodUID == i.PodUID, ObservedAt: observed, CurrentMode: mode, CurrentVNicUID: a.CurrentVNicUID, CurrentIPs: a.CurrentIPs, CurrentVNicName: a.CurrentVNicName, CurrentMACAddress: a.CurrentMACAddress, State: string(a.State), Reason: string(a.Reason), PodUID: a.PodUID, Version: a.Version, SnapshotRevision: a.SnapshotRevision}, nil
}
func (n *InstanceNetwork) Confirm(ctx context.Context, i biz.Instance) error {
	if err := n.runtime(i); err != nil {
		return err
	}
	if i.Network.AttachmentID == "" || i.Network.AttachmentVersion < 1 || i.Network.SnapshotRevision < 1 || i.PodUID == "" || i.PodName == "" {
		return biz.ErrNetworkUnavailable
	}
	_, err := n.attachments.Confirm(ctx, networkbiz.ConfirmAttachment{TenantID: i.TenantID, AttachmentID: i.Network.AttachmentID, ClusterID: i.Spec.ClusterID, Namespace: i.Spec.Namespace, PodName: i.PodName, PodUID: i.PodUID, ExpectedVersion: i.Network.AttachmentVersion, ExpectedSnapshotRevision: i.Network.SnapshotRevision})
	return instanceNetworkError(err)
}
func (n *InstanceNetwork) Release(ctx context.Context, i biz.Instance) error {
	if err := n.runtime(i); err != nil {
		return err
	}
	if i.Network.AttachmentID == "" || i.Network.AttachmentVersion < 1 || !biz.UUID(i.Network.FinalizationID) || (i.Network.State != "closing" && i.Network.State != "closed") {
		return biz.ErrNetworkUnavailable
	}
	_, err := n.attachments.Release(ctx, networkbiz.ReleaseAttachment{TenantID: i.TenantID, AttachmentID: i.Network.AttachmentID, FinalizationID: i.Network.FinalizationID, ExpectedVersion: i.Network.AttachmentVersion})
	return instanceNetworkError(err)
}
func instanceNetworkError(err error) error {
	if err == nil {
		return nil
	}
	switch networkbiz.ReasonOf(err) {
	case networkbiz.ResourceNotFound:
		return biz.ErrNetworkNotFound
	case networkbiz.AttachmentConflict, networkbiz.AttachmentProtocol, networkbiz.VersionConflict, networkbiz.IdempotencyConflict, networkbiz.PlacementMismatch, networkbiz.InvalidArgument:
		return biz.ErrNetworkConflict
	default:
		return biz.ErrNetworkUnavailable
	}
}

// InstanceNetworkConsumer preserves external single_pod owners while routing
// only controller_managed attachments to the local durable Instance reader.
type InstanceNetworkConsumer struct {
	reader   biz.InstanceReader
	fallback networkbiz.InstanceConsumer
}

func NewInstanceNetworkConsumer(reader biz.InstanceReader, fallback networkbiz.InstanceConsumer) (*InstanceNetworkConsumer, error) {
	if reader == nil {
		return nil, biz.ErrInvalid
	}
	return &InstanceNetworkConsumer{reader: reader, fallback: fallback}, nil
}
func (c *InstanceNetworkConsumer) GetSubmission(ctx context.Context, a networkbiz.Attachment) (networkbiz.ConsumerSubmission, error) {
	if a.WorkloadMode != "controller_managed" {
		if a.WorkloadMode != "" && a.WorkloadMode != "single_pod" {
			return networkbiz.ConsumerSubmission{}, biz.ErrNetworkConflict
		}
		if c.fallback == nil {
			return networkbiz.ConsumerSubmission{}, biz.ErrNetworkUnavailable
		}
		return c.fallback.GetSubmission(ctx, a)
	}
	i, err := c.reader.Get(ctx, a.TenantID, a.InstanceID)
	if err != nil {
		return networkbiz.ConsumerSubmission{}, biz.ErrNetworkUnavailable
	}
	if i.Network == nil || i.Network.AttachmentID == "" {
		return networkbiz.ConsumerSubmission{}, biz.ErrNetworkUnavailable
	}
	if !networkAttachmentMatches(i, a) || i.Network.SnapshotRevision < 1 {
		return networkbiz.ConsumerSubmission{}, biz.ErrNetworkConflict
	}
	n := i.Network
	out := networkbiz.ConsumerSubmission{ProtocolVersion: 2, TenantID: i.TenantID, InstanceID: i.ID, SubmissionID: n.SubmissionID, Generation: n.Generation, AttachmentID: n.AttachmentID, ClusterID: i.Spec.ClusterID, Namespace: i.Spec.Namespace, State: n.State, FinalizationID: n.FinalizationID, ClosedAt: n.ClosedAt, SnapshotRevision: n.SnapshotRevision, DesiredRunState: n.DesiredRunState, RootController: networkWorkload(n.RootController)}
	for _, c := range []biz.NetworkInterfaceChoice{biz.PrimaryNetworkInterface(i).Desired, biz.PrimaryNetworkInterface(i).Applied} {
		if c.Mode == "manual" {
			found := false
			for _, ref := range out.ManualVNics {
				found = found || ref.VNicID == c.VNicID
			}
			if !found {
				out.ManualVNics = append(out.ManualVNics, networkbiz.ManualVNicReference{VNicID: c.VNicID, ProviderName: c.ProviderName, ProviderUID: c.ProviderUID})
			}
		}
	}
	for _, set := range n.Controllers {
		out.Controllers = append(out.Controllers, networkbiz.ControllerReference{Reference: networkWorkload(set.Reference), Owner: networkWorkload(set.Owner)})
		out.ControllerUIDs = append(out.ControllerUIDs, set.Reference.UID)
	}
	for _, pod := range n.CurrentPods {
		out.CurrentPods = append(out.CurrentPods, networkbiz.PodReference{Reference: networkWorkload(pod.Reference), Owner: networkWorkload(pod.Owner)})
		out.PodUIDs = append(out.PodUIDs, pod.Reference.UID)
	}
	for _, pod := range n.RetiredPods {
		out.RetiredPods = append(out.RetiredPods, networkbiz.PodReference{Reference: networkWorkload(pod.Reference), Owner: networkWorkload(pod.Owner)})
	}
	return out, nil
}
func networkWorkload(r biz.WorkloadReference) networkbiz.WorkloadReference {
	return networkbiz.WorkloadReference{APIVersion: r.APIVersion, Kind: r.Kind, Namespace: r.Namespace, Name: r.Name, UID: r.UID}
}

var _ biz.NetworkPort = (*InstanceNetwork)(nil)
var _ networkbiz.InstanceConsumer = (*InstanceNetworkConsumer)(nil)

func instanceNetworkLabels(i biz.Instance) map[string]string {
	if i.Spec.Network == nil || i.Network == nil {
		return nil
	}
	return map[string]string{"network.ani.io/tenant-id": i.TenantID, "network.ani.io/instance-id": i.ID, "network.ani.io/attachment-id": i.Network.AttachmentID, "network.ani.io/submission-id": i.Network.SubmissionID, "network.ani.io/generation": strconv.FormatInt(i.Network.Generation, 10)}
}
