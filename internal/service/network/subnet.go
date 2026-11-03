package service

import (
	"context"
	networkv1 "github.com/zhangzhe-ctrl/ani-resource-service/api/network/v1"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func (s *NetworkService) CreateSubnet(ctx context.Context, r *networkv1.CreateSubnetRequest) (*networkv1.CreateSubnetResponse, error) {
	attribution := r.GetAttribution()
	value, err := s.network.CreateSubnet(ctx, biz.CreateSubnet{TenantID: r.GetTenantId(), Name: r.GetName(), CIDR: r.GetCidr(), Description: r.GetDescription(), IdempotencyKey: r.GetIdempotencyKey(),
		VPCID: r.GetVpcId(), Gateway: r.Gateway, SubnetOptions: biz.SubnetOptions{ExcludeIPs: r.GetExcludeIps(), EnableDHCP: r.GetEnableDhcp(), DNSServers: r.GetDnsServers()}, Attribution: biz.Attribution{Actor: attribution.GetActor(), DirectCaller: attribution.GetDirectCaller(), CorrelationID: attribution.GetCorrelationId()}})
	if err != nil {
		return nil, rpcError(err)
	}
	return &networkv1.CreateSubnetResponse{Subnet: wireSubnet(value)}, nil
}
func (s *NetworkService) GetSubnet(ctx context.Context, r *networkv1.GetSubnetRequest) (*networkv1.GetSubnetResponse, error) {
	value, err := s.network.GetSubnet(ctx, r.GetTenantId(), r.GetSubnetId())
	if err != nil {
		return nil, rpcError(err)
	}
	return &networkv1.GetSubnetResponse{Subnet: wireSubnet(value)}, nil
}
func (s *NetworkService) ListSubnets(ctx context.Context, r *networkv1.ListSubnetsRequest) (*networkv1.ListSubnetsResponse, error) {
	state, exists := statesFromWire[r.GetState()]
	if !exists {
		return nil, rpcError(biz.Fail(biz.InvalidArgument, "invalid Subnet state filter"))
	}
	value, err := s.network.ListSubnets(ctx, biz.ListSubnets{VPCID: r.GetVpcId(), TenantID: r.GetTenantId(), Name: r.GetName(), State: string(state), Limit: int(r.GetLimit()), Cursor: r.GetCursor()})
	if err != nil {
		return nil, rpcError(err)
	}
	result := &networkv1.ListSubnetsResponse{Total: value.Total, NextCursor: value.NextCursor, Items: make([]*networkv1.Subnet, 0, len(value.Items))}
	for _, v := range value.Items {
		result.Items = append(result.Items, wireSubnet(v))
	}
	return result, nil
}
func (s *NetworkService) DeleteSubnet(ctx context.Context, r *networkv1.DeleteSubnetRequest) (*networkv1.DeleteSubnetResponse, error) {
	value, err := s.network.DeleteSubnet(ctx, r.GetTenantId(), r.GetSubnetId())
	if err != nil {
		return nil, rpcError(err)
	}
	return &networkv1.DeleteSubnetResponse{Subnet: wireSubnet(value)}, nil
}
func wireSubnet(v biz.Subnet) *networkv1.Subnet {
	return &networkv1.Subnet{AvailableIpRange: v.AvailableIPRange, UsedIpRange: v.UsedIPRange, Id: v.ID, TenantId: v.TenantID, Name: v.Name, Description: v.Description, Cidr: v.CIDR, State: statesToWire[v.State], Reason: string(v.Reason), ReasonMessage: v.Reason.Message(), CreatedAt: timestamppb.New(v.CreatedAt), UpdatedAt: timestamppb.New(v.UpdatedAt), Version: v.Version, ObservedAt: optionalTime(v.ObservedAt), ObservationStale: v.ObservationStale, LastOperationId: v.LastOperationID, VpcId: v.VPCID, Gateway: v.Gateway, ExcludeIps: v.ExcludeIPs, EnableDhcp: v.EnableDHCP, DnsServers: v.DNSServers}
}
