package service

import (
	"context"
	networkv1 "github.com/zhangzhe-ctrl/ani-resource-service/api/network/v1"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func (s *NetworkService) CreateVNic(ctx context.Context, r *networkv1.CreateVNicRequest) (*networkv1.CreateVNicResponse, error) {
	tenant, attribution, e := vnicTenant(ctx, r.GetTargetTenantId())
	if e != nil {
		return nil, rpcError(e)
	}
	v, e := s.network.CreateVNic(ctx, biz.CreateVNic{TenantID: tenant, Attribution: attribution, Name: r.GetName(), Description: r.GetDescription(), SubnetID: r.GetSubnetId(), IdempotencyKey: r.GetIdempotencyKey(), RequestedIP: r.GetRequestedIp(), RequestedMAC: r.GetRequestedMac()})
	if e != nil {
		return nil, rpcError(e)
	}
	return &networkv1.CreateVNicResponse{Vnic: wireVNic(v)}, nil
}
func (s *NetworkService) GetVNic(ctx context.Context, r *networkv1.GetVNicRequest) (*networkv1.GetVNicResponse, error) {
	tenant, _, e := vnicTenant(ctx, r.GetTargetTenantId())
	if e != nil {
		return nil, rpcError(e)
	}
	v, e := s.network.GetVNic(ctx, tenant, r.GetVnicId())
	if e != nil {
		return nil, rpcError(e)
	}
	return &networkv1.GetVNicResponse{Vnic: wireVNic(v)}, nil
}
func (s *NetworkService) DeleteVNic(ctx context.Context, r *networkv1.DeleteVNicRequest) (*networkv1.DeleteVNicResponse, error) {
	tenant, _, e := vnicTenant(ctx, r.GetTargetTenantId())
	if e != nil {
		return nil, rpcError(e)
	}
	v, e := s.network.DeleteVNic(ctx, tenant, r.GetVnicId())
	if e != nil {
		return nil, rpcError(e)
	}
	return &networkv1.DeleteVNicResponse{Vnic: wireVNic(v)}, nil
}
func (s *NetworkService) ListVNics(ctx context.Context, r *networkv1.ListVNicsRequest) (*networkv1.ListVNicsResponse, error) {
	tenant, _, e := vnicTenant(ctx, r.GetTargetTenantId())
	if e != nil {
		return nil, rpcError(e)
	}
	w, e := biz.ResolvePage(r.Page, r.PageSize)
	if e != nil {
		return nil, rpcError(e)
	}
	vs, total, e := s.network.ListVNics(ctx, tenant, biz.VNicFilter{PageWindow: w, Name: r.GetName(), SubnetID: r.GetSubnetId(), State: r.GetState(), BindingState: r.GetBindingState(), BindableToKind: r.GetBindableToKind(), BindableToID: r.GetBindableToId()})
	if e != nil {
		return nil, rpcError(e)
	}
	out := &networkv1.ListVNicsResponse{Items: make([]*networkv1.VNic, 0, len(vs)), Total: total}
	for _, v := range vs {
		out.Items = append(out.Items, wireVNic(v))
	}
	return out, nil
}
func wireVNic(v biz.VNic) *networkv1.VNic {
	out := &networkv1.VNic{Id: v.ID, TenantId: v.TenantID, Name: v.Name, Description: v.Description, VpcId: v.VPCID, SubnetId: v.SubnetID, VpcName: v.VPCName, SubnetName: v.SubnetName, SecurityGroupNames: v.SecurityGroupNames, State: statesToWire[v.State], Version: v.Version, MacAddress: v.MACAddress, PrimaryIp: v.PrimaryIP, Ips: v.IPs, SecurityGroupIds: v.SecurityGroupIDs, ObservedAt: optionalTime(v.ObservedAt), ObservationStale: v.ObservationStale, Reason: string(v.Reason), LastOperationId: v.LastOperationID, CreatedAt: timestamppb.New(v.CreatedAt), UpdatedAt: timestamppb.New(v.UpdatedAt), BindingState: "free", Bindings: make([]*networkv1.VNicBinding, 0, len(v.Bindings))}
	out.RequestedIp, out.RequestedMac = v.RequestedIP, v.RequestedMAC
	for _, b := range v.Bindings {
		if b.Current {
			out.BindingState = "bound"
		} else if out.BindingState != "bound" {
			out.BindingState = "reserved"
		}
		out.Bindings = append(out.Bindings, &networkv1.VNicBinding{BindingId: b.ID, ConsumerName: b.ConsumerName, ComputeType: b.ComputeType, ConsumerKind: b.ConsumerKind, ConsumerId: b.ConsumerID, Slot: b.Slot, RequestedRevision: b.RequestedRevision, AppliedRevision: b.AppliedRevision, Current: b.Current, Target: b.Target})
	}
	return out
}

func (s *NetworkService) UseVNicBindingTargets(reader func(context.Context, biz.VNic, string, string, uint32, uint32) ([]biz.VNicConsumerScope, int64, error)) {
	s.vnicBindingTargets = reader
}
func (s *NetworkService) ListVNicBindingTargets(ctx context.Context, r *networkv1.ListVNicBindingTargetsRequest) (*networkv1.ListVNicBindingTargetsResponse, error) {
	tenant, _, e := vnicTenant(ctx, r.GetTargetTenantId())
	if e != nil {
		return nil, rpcError(e)
	}
	if r.GetConsumerKind() != "vm" && r.GetConsumerKind() != "container" {
		return nil, rpcError(biz.Fail(biz.InvalidArgument, "unsupported VNIC consumer kind"))
	}
	w, e := biz.ResolvePage(r.Page, r.PageSize)
	if e != nil {
		return nil, rpcError(e)
	}
	page := uint32(w.Offset/int64(w.Limit) + 1)
	v, e := s.network.GetVNic(ctx, tenant, r.GetVnicId())
	if e != nil {
		return nil, rpcError(e)
	}
	if s.vnicBindingTargets == nil {
		return nil, rpcError(biz.Fail(biz.ProviderUnavailable, "binding target reader unavailable"))
	}
	values, total, e := s.vnicBindingTargets(ctx, v, r.GetConsumerKind(), r.GetName(), page, uint32(w.Limit))
	if e != nil {
		return nil, rpcError(e)
	}
	out := &networkv1.ListVNicBindingTargetsResponse{Items: make([]*networkv1.VNicBindingTarget, 0, len(values)), Total: total}
	for _, v := range values {
		out.Items = append(out.Items, &networkv1.VNicBindingTarget{ConsumerKind: v.ConsumerKind, ConsumerId: v.ConsumerID, Name: v.Name, Version: v.Version, State: v.State, SecurityGroupIds: v.SecurityGroupIDs, ComputeType: v.ComputeType})
	}
	return out, nil
}

func vnicTenant(ctx context.Context, target string) (string, biz.Attribution, error) {
	if _, e := biz.ParseTenant(target); e != nil {
		return "", biz.Attribution{}, e
	}
	return (biz.ContextEgressAuthorization{}).Tenant(ctx, target)
}
