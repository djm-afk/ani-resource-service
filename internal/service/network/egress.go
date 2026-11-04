package service

import (
	"context"
	networkv1 "github.com/zhangzhe-ctrl/ani-resource-service/api/network/v1"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type TenantEgressService struct {
	networkv1.UnimplementedTenantEgressServiceServer
	egress *biz.Egress
}
type PlatformNetworkService struct {
	networkv1.UnimplementedPlatformNetworkServiceServer
	egress *biz.Egress
}

func NewTenantEgressService(e *biz.Egress) *TenantEgressService {
	return &TenantEgressService{egress: e}
}
func NewPlatformNetworkService(e *biz.Egress) *PlatformNetworkService {
	return &PlatformNetworkService{egress: e}
}
func wireEIP(v biz.EIP) *networkv1.EIP {
	// Persisted pre-U00 acceptance snapshots have no scope fields. Their source
	// resources were exclusively tenant Public EIPs; preserve that replay.
	if v.Scope == "" {
		v.Scope = "public"
	}
	if v.ManagedBy == "" {
		v.ManagedBy = "tenant"
	}
	r := &networkv1.EIP{Id: v.ID, TenantId: v.TenantID, Name: v.Name, Description: v.Description, State: statesToWire[v.State], Reason: string(v.Reason), ReasonMessage: v.Reason.Message(), Version: v.Version, CreatedAt: timestamppb.New(v.CreatedAt), UpdatedAt: timestamppb.New(v.UpdatedAt), ObservedAt: optionalTime(v.ObservedAt), ObservationStale: v.ObservationStale, LastOperationId: v.LastOperationID, Address: v.Address, BindingId: v.BindingID, BindingState: v.BindingState, Scope: v.Scope, ManagedBy: v.ManagedBy}
	if v.BindingTarget != nil {
		r.BindingTarget = &networkv1.EIPBindingTarget{Kind: v.BindingTarget.Kind, Id: v.BindingTarget.ID, State: v.BindingTarget.State, VpcId: v.BindingTarget.VPCID}
	}
	return r
}
func wireSnat(v biz.VPCSnatBinding) *networkv1.VPCSnatBinding {
	if v.Purpose == "" {
		v.Purpose = "public"
	}
	return &networkv1.VPCSnatBinding{Id: v.ID, TenantId: v.TenantID, VpcId: v.VPCID, EipId: v.EIPID, EipAddress: v.EIPAddress, State: statesToWire[v.State], Reason: string(v.Reason), ReasonMessage: v.Reason.Message(), Version: v.Version, CreatedAt: timestamppb.New(v.CreatedAt), UpdatedAt: timestamppb.New(v.UpdatedAt), ObservedAt: optionalTime(v.ObservedAt), ObservationStale: v.ObservationStale, LastOperationId: v.LastOperationID, DesiredEnabled: v.DesiredEnabled, AppliedEnabled: v.AppliedEnabled, Purpose: v.Purpose}
}
func wirePlatform(v biz.PlatformResource) *networkv1.PlatformResource {
	r := &networkv1.PlatformResource{Id: v.ID, ClusterId: v.ClusterID, Name: v.Name, Description: v.Description, State: statesToWire[v.State], Reason: string(v.Reason), ReasonMessage: v.Reason.Message(), Version: v.Version, CreatedAt: timestamppb.New(v.CreatedAt), UpdatedAt: timestamppb.New(v.UpdatedAt), ObservedAt: optionalTime(v.ObservedAt), ObservationStale: v.ObservationStale, LastOperationId: v.LastOperationID}
	if v.Vlan != nil {
		r.Configuration = &networkv1.PlatformResource_Vlan{Vlan: &networkv1.VlanNetwork{DeviceId: v.Vlan.DeviceID, VlanId: v.Vlan.VlanID, VirtualSwitchId: v.Vlan.VirtualSwitchID, DeviceName: v.Vlan.DeviceName, GroupName: v.Vlan.GroupName, Nodes: wireSwitchNodes(v.Vlan.Nodes)}}
	}
	if v.Kind == "egress_gateway" {
		r.Configuration = &networkv1.PlatformResource_Gateway{Gateway: &networkv1.EgressGateway{}}
	}
	if v.Pool != nil && v.Pool.Scope != "intranet" {
		p := v.Pool
		mode := networkv1.PublicPoolMode_PUBLIC_POOL_MODE_OVERLAY
		if p.Mode == "underlay" {
			mode = networkv1.PublicPoolMode_PUBLIC_POOL_MODE_UNDERLAY
		}
		pool := &networkv1.PublicAddressPool{Scope: "public", Mode: mode, GatewayId: p.GatewayID, Cidr: p.CIDR, OvnGatewayIp: p.OVNGatewayIP, ExcludedIps: p.ExcludedIPs, VlanNetworkId: p.VlanNetworkID, UpstreamGatewayIp: p.UpstreamGatewayIP, AllocationEnabled: v.AllocationEnabled, IsDefault: v.IsDefault, ConfigRevision: v.ConfigRevision, ObservedProviderImages: []string{}}
		if v.Verification != nil {
			ev := v.Verification
			pool.Verification = &networkv1.PublicPoolVerification{ProviderSourceRevision: ev.ProviderSourceRevision, ProviderImageDigests: ev.ProviderImageDigests, TopologyFingerprint: ev.TopologyFingerprint, EvidenceReference: ev.EvidenceReference, Scope: ev.Scope, VerifiedAt: timestamppb.New(ev.VerifiedAt), ExpiresAt: timestamppb.New(ev.ExpiresAt)}
		}
		r.Configuration = &networkv1.PlatformResource_Pool{Pool: pool}
	}
	if v.Pool != nil && v.Pool.Scope == "intranet" {
		p := v.Pool
		pool := &networkv1.IntranetAddressPool{Cidr: p.CIDR, OvnGatewayIp: p.OVNGatewayIP, ExcludedIps: p.ExcludedIPs, DefaultVpcName: p.DefaultVPCName, DefaultVpcUid: p.DefaultVPCUID, IntranetNetworks: p.IntranetNetworks, AllocationEnabled: v.AllocationEnabled, IsDefault: v.IsDefault, ConfigRevision: v.ConfigRevision, ObservedProviderImages: []string{}, Scope: "intranet", NewVpcsEnabled: v.NewVPCsEnabled}
		if ev := v.Verification; ev != nil {
			pool.Verification = &networkv1.IntranetPoolVerification{ProviderSourceRevision: ev.ProviderSourceRevision, ProviderImageDigests: ev.ProviderImageDigests, TopologyFingerprint: ev.TopologyFingerprint, EvidenceReference: ev.EvidenceReference, Scope: ev.Scope, VerifiedAt: timestamppb.New(ev.VerifiedAt), ExpiresAt: timestamppb.New(ev.ExpiresAt)}
		}
		r.Configuration = &networkv1.PlatformResource_IntranetPool{IntranetPool: pool}
	}
	return r
}
func (s *TenantEgressService) CreateEIP(ctx context.Context, r *networkv1.CreateEIPRequest) (*networkv1.CreateEIPResponse, error) {
	v, err := s.egress.CreateEIP(ctx, biz.EgressIntent{TenantID: r.GetTargetTenantId(), Name: r.GetName(), Description: r.GetDescription(), IdempotencyKey: r.GetIdempotencyKey(), SecurityGroupIDs: r.GetSecurityGroupIds()})
	if err != nil {
		return nil, rpcError(err)
	}
	return &networkv1.CreateEIPResponse{Eip: wireEIP(v)}, nil
}
func (s *TenantEgressService) GetEIP(ctx context.Context, r *networkv1.GetEIPRequest) (*networkv1.GetEIPResponse, error) {
	v, err := s.egress.GetEIP(ctx, r.GetTargetTenantId(), r.GetEipId())
	if err != nil {
		return nil, rpcError(err)
	}
	return &networkv1.GetEIPResponse{Eip: wireEIP(v)}, nil
}
func (s *TenantEgressService) DeleteEIP(ctx context.Context, r *networkv1.DeleteEIPRequest) (*networkv1.DeleteEIPResponse, error) {
	v, err := s.egress.DeleteEIP(ctx, r.GetTargetTenantId(), r.GetEipId())
	if err != nil {
		return nil, rpcError(err)
	}
	return &networkv1.DeleteEIPResponse{Eip: wireEIP(v)}, nil
}
func (s *TenantEgressService) BindVPCSnat(ctx context.Context, r *networkv1.BindVPCSnatRequest) (*networkv1.BindVPCSnatResponse, error) {
	v, err := s.egress.BindVPCSnat(ctx, biz.EgressIntent{TenantID: r.GetTargetTenantId(), VPCID: r.GetVpcId(), EIPID: r.GetEipId(), IdempotencyKey: r.GetIdempotencyKey()})
	if err != nil {
		return nil, rpcError(err)
	}
	return &networkv1.BindVPCSnatResponse{Binding: wireSnat(v)}, nil
}
func (s *TenantEgressService) GetVPCSnat(ctx context.Context, r *networkv1.GetVPCSnatRequest) (*networkv1.GetVPCSnatResponse, error) {
	v, err := s.egress.GetVPCSnat(ctx, r.GetTargetTenantId(), r.GetVpcId(), true)
	if err != nil {
		return nil, rpcError(err)
	}
	return &networkv1.GetVPCSnatResponse{Binding: wireSnat(v)}, nil
}
func (s *TenantEgressService) GetVPCSnatBinding(ctx context.Context, r *networkv1.GetVPCSnatBindingRequest) (*networkv1.GetVPCSnatBindingResponse, error) {
	v, err := s.egress.GetVPCSnat(ctx, r.GetTargetTenantId(), r.GetBindingId(), false)
	if err != nil {
		return nil, rpcError(err)
	}
	return &networkv1.GetVPCSnatBindingResponse{Binding: wireSnat(v)}, nil
}
func (s *TenantEgressService) SetVPCSnatEnabled(ctx context.Context, r *networkv1.SetVPCSnatEnabledRequest) (*networkv1.SetVPCSnatEnabledResponse, error) {
	v, err := s.egress.SetVPCSnatEnabled(ctx, biz.EgressIntent{TenantID: r.GetTargetTenantId(), ID: r.GetBindingId(), Enabled: r.GetEnabled(), ExpectedVersion: r.GetExpectedVersion(), IdempotencyKey: r.GetIdempotencyKey()})
	if err != nil {
		return nil, rpcError(err)
	}
	return &networkv1.SetVPCSnatEnabledResponse{Binding: wireSnat(v)}, nil
}
func (s *TenantEgressService) DeleteVPCSnatBinding(ctx context.Context, r *networkv1.DeleteVPCSnatBindingRequest) (*networkv1.DeleteVPCSnatBindingResponse, error) {
	v, err := s.egress.DeleteVPCSnatBinding(ctx, r.GetTargetTenantId(), r.GetBindingId())
	if err != nil {
		return nil, rpcError(err)
	}
	return &networkv1.DeleteVPCSnatBindingResponse{Binding: wireSnat(v)}, nil
}
func (s *TenantEgressService) ListEIPs(ctx context.Context, r *networkv1.ListEIPsRequest) (*networkv1.ListEIPsResponse, error) {
	state, ok := statesFromWire[r.GetState()]
	if !ok {
		return nil, rpcError(biz.Fail(biz.InvalidArgument, "invalid state filter"))
	}
	values, cursor, total, err := s.egress.ListEIPs(ctx, biz.ListVPCs{TenantID: r.GetTargetTenantId(), Name: r.GetName(), State: string(state), Limit: int(r.GetLimit()), Cursor: r.GetCursor()})
	if err != nil {
		return nil, rpcError(err)
	}
	reply := &networkv1.ListEIPsResponse{NextCursor: cursor, Total: total}
	for _, v := range values {
		reply.Items = append(reply.Items, wireEIP(v))
	}
	return reply, nil
}
func (s *PlatformNetworkService) CreateVlanNetwork(ctx context.Context, r *networkv1.CreateVlanNetworkRequest) (*networkv1.CreateVlanNetworkResponse, error) {
	v, err := s.egress.CreatePlatform(ctx, biz.PlatformIntent{Kind: "create_vlan", Name: r.GetName(), Description: r.GetDescription(), IdempotencyKey: r.GetIdempotencyKey(), Vlan: &biz.VlanConfig{DeviceID: r.GetDeviceId(), VlanID: r.GetVlanId(), ClusterID: r.GetClusterId(), VirtualSwitchID: r.GetVirtualSwitchId(), DeviceName: r.GetDeviceName()}})
	if err != nil {
		return nil, rpcError(err)
	}
	return &networkv1.CreateVlanNetworkResponse{Resource: wirePlatform(v)}, nil
}
func (s *PlatformNetworkService) CreateEgressGateway(ctx context.Context, r *networkv1.CreateEgressGatewayRequest) (*networkv1.CreateEgressGatewayResponse, error) {
	v, err := s.egress.CreatePlatform(ctx, biz.PlatformIntent{Kind: "create_egress_gateway", Name: r.GetName(), Description: r.GetDescription(), IdempotencyKey: r.GetIdempotencyKey()})
	if err != nil {
		return nil, rpcError(err)
	}
	return &networkv1.CreateEgressGatewayResponse{Resource: wirePlatform(v)}, nil
}
func (s *PlatformNetworkService) CreatePublicAddressPool(ctx context.Context, r *networkv1.CreatePublicAddressPoolRequest) (*networkv1.CreatePublicAddressPoolResponse, error) {
	mode := ""
	switch r.GetMode() {
	case networkv1.PublicPoolMode_PUBLIC_POOL_MODE_OVERLAY:
		mode = "overlay"
	case networkv1.PublicPoolMode_PUBLIC_POOL_MODE_UNDERLAY:
		mode = "underlay"
	}
	v, err := s.egress.CreatePlatform(ctx, biz.PlatformIntent{Kind: "create_public_pool", Name: r.GetName(), Description: r.GetDescription(), IdempotencyKey: r.GetIdempotencyKey(), Pool: &biz.PublicPoolConfig{Mode: mode, GatewayID: r.GetGatewayId(), CIDR: r.GetCidr(), OVNGatewayIP: r.GetOvnGatewayIp(), ExcludedIPs: r.GetExcludedIps(), VlanNetworkID: r.GetVlanNetworkId(), UpstreamGatewayIP: r.GetUpstreamGatewayIp()}})
	if err != nil {
		return nil, rpcError(err)
	}
	return &networkv1.CreatePublicAddressPoolResponse{Resource: wirePlatform(v)}, nil
}
func (s *PlatformNetworkService) GetVlanNetwork(ctx context.Context, r *networkv1.GetVlanNetworkRequest) (*networkv1.GetVlanNetworkResponse, error) {
	v, err := s.egress.GetPlatform(ctx, "vlan", r.GetVlanNetworkId())
	if err != nil {
		return nil, rpcError(err)
	}
	return &networkv1.GetVlanNetworkResponse{Resource: wirePlatform(v)}, nil
}
func (s *PlatformNetworkService) DeleteVlanNetwork(ctx context.Context, r *networkv1.DeleteVlanNetworkRequest) (*networkv1.DeleteVlanNetworkResponse, error) {
	v, err := s.egress.DeletePlatform(ctx, "vlan", r.GetVlanNetworkId())
	if err != nil {
		return nil, rpcError(err)
	}
	return &networkv1.DeleteVlanNetworkResponse{Resource: wirePlatform(v)}, nil
}
func (s *PlatformNetworkService) ListVlanNetworks(ctx context.Context, r *networkv1.ListVlanNetworksRequest) (*networkv1.ListVlanNetworksResponse, error) {
	state, ok := statesFromWire[r.GetState()]
	if !ok {
		return nil, rpcError(biz.Fail(biz.InvalidArgument, "invalid state filter"))
	}
	values, cursor, total, err := s.egress.ListPlatform(ctx, "vlan", biz.ListVPCs{Name: r.GetName(), State: string(state), Limit: int(r.GetLimit()), Cursor: r.GetCursor()})
	if err != nil {
		return nil, rpcError(err)
	}
	reply := &networkv1.ListVlanNetworksResponse{NextCursor: cursor, Total: total}
	for _, v := range values {
		reply.Items = append(reply.Items, wirePlatform(v))
	}
	return reply, nil
}
func (s *PlatformNetworkService) GetEgressGateway(ctx context.Context, r *networkv1.GetEgressGatewayRequest) (*networkv1.GetEgressGatewayResponse, error) {
	v, err := s.egress.GetPlatform(ctx, "egress_gateway", r.GetGatewayId())
	if err != nil {
		return nil, rpcError(err)
	}
	return &networkv1.GetEgressGatewayResponse{Resource: wirePlatform(v)}, nil
}
func (s *PlatformNetworkService) DeleteEgressGateway(ctx context.Context, r *networkv1.DeleteEgressGatewayRequest) (*networkv1.DeleteEgressGatewayResponse, error) {
	v, err := s.egress.DeletePlatform(ctx, "egress_gateway", r.GetGatewayId())
	if err != nil {
		return nil, rpcError(err)
	}
	return &networkv1.DeleteEgressGatewayResponse{Resource: wirePlatform(v)}, nil
}
func (s *PlatformNetworkService) ListEgressGateways(ctx context.Context, r *networkv1.ListEgressGatewaysRequest) (*networkv1.ListEgressGatewaysResponse, error) {
	state, ok := statesFromWire[r.GetState()]
	if !ok {
		return nil, rpcError(biz.Fail(biz.InvalidArgument, "invalid state filter"))
	}
	values, cursor, total, err := s.egress.ListPlatform(ctx, "egress_gateway", biz.ListVPCs{Name: r.GetName(), State: string(state), Limit: int(r.GetLimit()), Cursor: r.GetCursor()})
	if err != nil {
		return nil, rpcError(err)
	}
	reply := &networkv1.ListEgressGatewaysResponse{NextCursor: cursor, Total: total}
	for _, v := range values {
		reply.Items = append(reply.Items, wirePlatform(v))
	}
	return reply, nil
}
func (s *PlatformNetworkService) GetPublicAddressPool(ctx context.Context, r *networkv1.GetPublicAddressPoolRequest) (*networkv1.GetPublicAddressPoolResponse, error) {
	v, err := s.egress.GetPlatform(ctx, "public_pool", r.GetPoolId())
	if err != nil {
		return nil, rpcError(err)
	}
	return &networkv1.GetPublicAddressPoolResponse{Resource: wirePlatform(v)}, nil
}
func (s *PlatformNetworkService) DeletePublicAddressPool(ctx context.Context, r *networkv1.DeletePublicAddressPoolRequest) (*networkv1.DeletePublicAddressPoolResponse, error) {
	v, err := s.egress.DeletePlatform(ctx, "public_pool", r.GetPoolId())
	if err != nil {
		return nil, rpcError(err)
	}
	return &networkv1.DeletePublicAddressPoolResponse{Resource: wirePlatform(v)}, nil
}
func (s *PlatformNetworkService) ListPublicAddressPools(ctx context.Context, r *networkv1.ListPublicAddressPoolsRequest) (*networkv1.ListPublicAddressPoolsResponse, error) {
	state, ok := statesFromWire[r.GetState()]
	if !ok {
		return nil, rpcError(biz.Fail(biz.InvalidArgument, "invalid state filter"))
	}
	values, cursor, total, err := s.egress.ListPlatform(ctx, "public_pool", biz.ListVPCs{Name: r.GetName(), State: string(state), Limit: int(r.GetLimit()), Cursor: r.GetCursor()})
	if err != nil {
		return nil, rpcError(err)
	}
	reply := &networkv1.ListPublicAddressPoolsResponse{NextCursor: cursor, Total: total}
	for _, v := range values {
		reply.Items = append(reply.Items, wirePlatform(v))
	}
	return reply, nil
}
func (s *PlatformNetworkService) SetPublicPoolAllocationEnabled(ctx context.Context, r *networkv1.SetPublicPoolAllocationEnabledRequest) (*networkv1.SetPublicPoolAllocationEnabledResponse, error) {
	v, err := s.egress.SetPool(ctx, biz.PlatformIntent{Kind: "set_pool_allocation", ID: r.GetPoolId(), Enabled: r.GetEnabled(), ExpectedVersion: r.GetExpectedVersion(), IdempotencyKey: r.GetIdempotencyKey()})
	if err != nil {
		return nil, rpcError(err)
	}
	return &networkv1.SetPublicPoolAllocationEnabledResponse{Resource: wirePlatform(v)}, nil
}
func (s *PlatformNetworkService) SetDefaultPublicPool(ctx context.Context, r *networkv1.SetDefaultPublicPoolRequest) (*networkv1.SetDefaultPublicPoolResponse, error) {
	v, err := s.egress.SetPool(ctx, biz.PlatformIntent{Kind: "set_default_pool", ID: r.GetPoolId(), Enabled: false, ExpectedVersion: r.GetExpectedVersion(), IdempotencyKey: r.GetIdempotencyKey()})
	if err != nil {
		return nil, rpcError(err)
	}
	return &networkv1.SetDefaultPublicPoolResponse{Resource: wirePlatform(v)}, nil
}
func (s *PlatformNetworkService) RecordPublicPoolVerification(ctx context.Context, r *networkv1.RecordPublicPoolVerificationRequest) (*networkv1.RecordPublicPoolVerificationResponse, error) {
	ev := r.GetVerification()
	if ev == nil || ev.GetVerifiedAt() == nil || ev.GetExpiresAt() == nil || ev.GetVerifiedAt().CheckValid() != nil || ev.GetExpiresAt().CheckValid() != nil {
		return nil, rpcError(biz.Fail(biz.InvalidArgument, "valid evidence timestamps are required"))
	}
	v, err := s.egress.SetPool(ctx, biz.PlatformIntent{Kind: "verify_public_pool", ID: r.GetPoolId(), ExpectedVersion: r.GetExpectedVersion(), IdempotencyKey: r.GetIdempotencyKey(), Verification: &biz.PublicPoolVerification{ProviderSourceRevision: ev.GetProviderSourceRevision(), ProviderImageDigests: ev.GetProviderImageDigests(), TopologyFingerprint: ev.GetTopologyFingerprint(), EvidenceReference: ev.GetEvidenceReference(), Scope: ev.GetScope(), VerifiedAt: ev.GetVerifiedAt().AsTime(), ExpiresAt: ev.GetExpiresAt().AsTime()}})
	if err != nil {
		return nil, rpcError(err)
	}
	return &networkv1.RecordPublicPoolVerificationResponse{Resource: wirePlatform(v)}, nil
}
func (s *PlatformNetworkService) GetPlatformOperation(ctx context.Context, r *networkv1.GetPlatformOperationRequest) (*networkv1.GetPlatformOperationResponse, error) {
	v, err := s.egress.GetPlatformOperation(ctx, r.GetOperationId())
	if err != nil {
		return nil, rpcError(err)
	}
	return &networkv1.GetPlatformOperationResponse{Operation: &networkv1.PlatformOperation{Id: v.ID, ResourceId: v.ResourceID, Kind: kindsToWire[v.Kind], State: operationsToWire[v.State], Reason: string(v.Reason), ReasonMessage: v.Reason.Message(), CreatedAt: timestamppb.New(v.CreatedAt), UpdatedAt: timestamppb.New(v.UpdatedAt), CompletedAt: optionalTime(v.CompletedAt), NextAttemptAt: optionalTime(v.NextAttemptAt)}}, nil
}
