package service

import (
	"context"
	networkv1 "github.com/zhangzhe-ctrl/ani-resource-service/api/network/v1"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func (s *NetworkService) PrepareAttachment(ctx context.Context, r *networkv1.PrepareAttachmentRequest) (*networkv1.PrepareAttachmentResponse, error) {
	if s.attachments == nil {
		return nil, rpcError(biz.Fail(biz.DependencyUnavailable, "attachment service unavailable"))
	}
	var expectation *biz.VMNetworkExpectation
	if x := r.GetVmExpectation(); x != nil {
		expectation = &biz.VMNetworkExpectation{NADName: x.NadName, NADUID: x.NadUid, NADConfigDigest: x.NadConfigDigest}
	}
	a, e := s.attachments.Prepare(ctx, biz.PrepareAttachment{TenantID: r.GetTenantId(), InstanceID: r.GetInstanceId(), SubnetID: r.GetSubnetId(), VPCID: r.GetVpcId(), Slot: r.GetSlot(), RequestKey: r.GetRequestKey(), SubmissionID: r.GetSubmissionId(), Generation: r.GetGeneration(), ClusterID: r.GetClusterId(), Namespace: r.GetNamespace(), WorkloadMode: r.GetWorkloadMode(), VMExpectation: expectation})
	v, e := wireAttachment(a, e)
	if e != nil {
		return nil, e
	}
	return &networkv1.PrepareAttachmentResponse{Attachment: v}, nil
}
func (s *NetworkService) GetAttachment(ctx context.Context, r *networkv1.GetAttachmentRequest) (*networkv1.GetAttachmentResponse, error) {
	if s.attachments == nil {
		return nil, rpcError(biz.Fail(biz.DependencyUnavailable, "attachment service unavailable"))
	}
	a, e := s.attachments.Get(ctx, r.GetTenantId(), r.GetAttachmentId())
	v, e := wireAttachment(a, e)
	if e != nil {
		return nil, e
	}
	return &networkv1.GetAttachmentResponse{Attachment: v}, nil
}
func (s *NetworkService) ConfirmAttachment(ctx context.Context, r *networkv1.ConfirmAttachmentRequest) (*networkv1.ConfirmAttachmentResponse, error) {
	if s.attachments == nil {
		return nil, rpcError(biz.Fail(biz.DependencyUnavailable, "attachment service unavailable"))
	}
	a, e := s.attachments.Confirm(ctx, biz.ConfirmAttachment{TenantID: r.GetTenantId(), AttachmentID: r.GetAttachmentId(), ExpectedVersion: r.GetExpectedVersion(), ClusterID: r.GetClusterId(), Namespace: r.GetNamespace(), PodName: r.GetPodName(), PodUID: r.GetPodUid(), ExpectedSnapshotRevision: r.GetExpectedSnapshotRevision()})
	v, e := wireAttachment(a, e)
	if e != nil {
		return nil, e
	}
	return &networkv1.ConfirmAttachmentResponse{Attachment: v}, nil
}
func (s *NetworkService) ReleaseAttachment(ctx context.Context, r *networkv1.ReleaseAttachmentRequest) (*networkv1.ReleaseAttachmentResponse, error) {
	if s.attachments == nil {
		return nil, rpcError(biz.Fail(biz.DependencyUnavailable, "attachment service unavailable"))
	}
	a, e := s.attachments.Release(ctx, biz.ReleaseAttachment{TenantID: r.GetTenantId(), AttachmentID: r.GetAttachmentId(), ExpectedVersion: r.GetExpectedVersion(), FinalizationID: r.GetConsumerFinalizationId()})
	v, e := wireAttachment(a, e)
	if e != nil {
		return nil, e
	}
	return &networkv1.ReleaseAttachmentResponse{Attachment: v}, nil
}
func (s *NetworkService) ListNetworkInstances(ctx context.Context, r *networkv1.ListNetworkInstancesRequest) (*networkv1.ListNetworkInstancesResponse, error) {
	if s.attachments == nil {
		return nil, rpcError(biz.Fail(biz.DependencyUnavailable, "attachment service unavailable"))
	}
	if r == nil {
		return nil, rpcError(biz.Fail(biz.InvalidArgument, "request required"))
	}
	page, e := s.attachments.ListInstances(ctx, biz.ListAttachmentConsumers{TenantID: r.GetTenantId(), VPCID: r.GetVpcId(), SubnetID: r.GetSubnetId(), Page: r.Page, PageSize: r.PageSize})
	if e != nil {
		return nil, rpcError(e)
	}
	result := &networkv1.ListNetworkInstancesResponse{Total: page.Total, Items: make([]*networkv1.AssociatedInstance, 0, len(page.Items))}
	for _, item := range page.Items {
		value := &networkv1.AssociatedInstance{TenantId: item.TenantID, ConsumerType: item.ConsumerType, ConsumerId: item.ConsumerID, ConsumerName: item.ConsumerName, ConsumerState: item.ConsumerState, SubnetId: item.SubnetID, SubnetName: item.SubnetName, Slot: item.Slot, AttachmentId: item.AttachmentID, AttachmentState: item.AttachmentState, AttachmentReason: item.AttachmentReason, ObservationStale: item.ObservationStale, ObservedAt: optionalTime(item.ObservedAt), Addresses: make([]*networkv1.NetworkAddress, 0, len(item.Addresses))}
		for _, address := range item.Addresses {
			value.Addresses = append(value.Addresses, &networkv1.NetworkAddress{NicName: address.NICName, Address: address.Address})
		}
		result.Items = append(result.Items, value)
	}
	return result, nil
}
func wireAttachment(a biz.Attachment, e error) (*networkv1.Attachment, error) {
	if e != nil {
		return nil, rpcError(e)
	}
	v := &networkv1.Attachment{WorkloadMode: a.WorkloadMode, SnapshotRevision: a.SnapshotRevision, RootController: wireWorkloadReference(a.RootController), Id: a.ID, TenantId: a.TenantID, VpcId: a.VPCID, SubnetId: a.SubnetID, InstanceId: a.InstanceID, Slot: a.Slot, SubmissionId: a.SubmissionID, Generation: a.Generation, ClusterId: a.ClusterID, Namespace: a.Namespace, BindingRevision: a.BindingRevision, State: attachmentStates[a.State], Reason: string(a.Reason), Version: a.Version, PodName: a.PodName, PodUid: a.PodUID, ConsumerFinalizationId: a.FinalizationID, CreatedAt: timestamppb.New(a.CreatedAt), UpdatedAt: timestamppb.New(a.UpdatedAt), ObservedAt: optionalTime(a.ObservedAt), ReleasedAt: optionalTime(a.ReleasedAt)}
	if a.Plan != nil {
		p := a.Plan
		v.PodPrimary = &networkv1.PodPrimaryPlan{FormatVersion: p.FormatVersion, Namespace: p.Namespace, SubnetAnnotation: p.PrimaryNetworkRef, Labels: &networkv1.AttachmentLabels{TenantId: p.TenantID, InstanceId: p.InstanceID, AttachmentId: p.AttachmentID, SubmissionId: p.SubmissionID, Generation: p.Generation}}
	}
	if p := a.VMPrimary; p != nil {
		v.VmPrimary = &networkv1.VMPrimaryPlan{FormatVersion: p.FormatVersion, Namespace: p.Namespace, NadName: p.NADName, NadUid: p.NADUID, NadConfigDigest: p.NADConfigDigest, KcnSubnetRef: p.KCNSubnetRef, Labels: &networkv1.AttachmentLabels{TenantId: p.TenantID, InstanceId: p.InstanceID, AttachmentId: p.AttachmentID, SubmissionId: p.SubmissionID, Generation: p.Generation}}
	}
	for _, a := range a.VMCurrentAddresses {
		v.VmCurrentAddresses = append(v.VmCurrentAddresses, &networkv1.VMCurrentAddress{NicName: a.NICName, Address: a.Address, LauncherPodUid: a.LauncherPodUID, VmiUid: a.VMIUID})
	}
	return v, nil
}

var attachmentStates = map[biz.AttachmentState]networkv1.AttachmentState{biz.Reserved: networkv1.AttachmentState_ATTACHMENT_STATE_RESERVED, biz.Attached: networkv1.AttachmentState_ATTACHMENT_STATE_ATTACHED, biz.Releasing: networkv1.AttachmentState_ATTACHMENT_STATE_RELEASING, biz.Released: networkv1.AttachmentState_ATTACHMENT_STATE_RELEASED}

func wireWorkloadReference(r biz.WorkloadReference) *networkv1.WorkloadReference {
	if r.UID == "" {
		return nil
	}
	return &networkv1.WorkloadReference{ApiVersion: r.APIVersion, Kind: r.Kind, Namespace: r.Namespace, Name: r.Name, Uid: r.UID}
}
