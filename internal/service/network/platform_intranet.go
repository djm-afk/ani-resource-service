package service

import (
	"context"

	networkv1 "github.com/zhangzhe-ctrl/ani-resource-service/api/network/v1"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
)

func (s *PlatformNetworkService) CreateIntranetAddressPool(ctx context.Context, r *networkv1.CreateIntranetAddressPoolRequest) (*networkv1.CreateIntranetAddressPoolResponse, error) {
	v, err := s.egress.CreatePlatform(ctx, biz.PlatformIntent{Kind: "create_intranet_pool", Name: r.GetName(), Description: r.GetDescription(), IdempotencyKey: r.GetIdempotencyKey(), Pool: &biz.PublicPoolConfig{Scope: "intranet", CIDR: r.GetCidr(), OVNGatewayIP: r.GetOvnGatewayIp(), ExcludedIPs: r.GetExcludedIps(), DefaultVPCName: r.GetDefaultVpcName(), DefaultVPCUID: r.GetDefaultVpcUid(), IntranetNetworks: r.GetIntranetNetworks()}})
	if err != nil {
		return nil, rpcError(err)
	}
	return &networkv1.CreateIntranetAddressPoolResponse{Resource: wirePlatform(v)}, nil
}
func (s *PlatformNetworkService) GetIntranetPoolDefaults(ctx context.Context, _ *networkv1.GetIntranetPoolDefaultsRequest) (*networkv1.GetIntranetPoolDefaultsResponse, error) {
	v, err := s.egress.GetIntranetPoolDefaults(ctx)
	if err != nil {
		return nil, rpcError(err)
	}
	return &networkv1.GetIntranetPoolDefaultsResponse{DefaultVpcName: v.DefaultVPCName, DefaultVpcUid: v.DefaultVPCUID, IntranetNetworks: v.IntranetNetworks}, nil
}
func (s *PlatformNetworkService) GetIntranetAddressPool(ctx context.Context, r *networkv1.GetIntranetAddressPoolRequest) (*networkv1.GetIntranetAddressPoolResponse, error) {
	v, err := s.egress.GetPlatform(ctx, "intranet_pool", r.GetPoolId())
	if err != nil {
		return nil, rpcError(err)
	}
	return &networkv1.GetIntranetAddressPoolResponse{Resource: wirePlatform(v)}, nil
}
func (s *PlatformNetworkService) ListIntranetAddressPools(ctx context.Context, r *networkv1.ListIntranetAddressPoolsRequest) (*networkv1.ListIntranetAddressPoolsResponse, error) {
	state, ok := statesFromWire[r.GetState()]
	if !ok {
		return nil, rpcError(biz.Fail(biz.InvalidArgument, "invalid state filter"))
	}
	rows, cursor, total, err := s.egress.ListPlatform(ctx, "intranet_pool", biz.ListVPCs{Name: r.GetName(), State: string(state), Limit: int(r.GetLimit()), Cursor: r.GetCursor()})
	if err != nil {
		return nil, rpcError(err)
	}
	result := &networkv1.ListIntranetAddressPoolsResponse{NextCursor: cursor, Total: total}
	for _, v := range rows {
		result.Items = append(result.Items, wirePlatform(v))
	}
	return result, nil
}
func (s *PlatformNetworkService) DeleteIntranetAddressPool(ctx context.Context, r *networkv1.DeleteIntranetAddressPoolRequest) (*networkv1.DeleteIntranetAddressPoolResponse, error) {
	v, err := s.egress.DeletePlatform(ctx, "intranet_pool", r.GetPoolId())
	if err != nil {
		return nil, rpcError(err)
	}
	return &networkv1.DeleteIntranetAddressPoolResponse{Resource: wirePlatform(v)}, nil
}
func (s *PlatformNetworkService) SetIntranetPoolAllocationEnabled(ctx context.Context, r *networkv1.SetIntranetPoolAllocationEnabledRequest) (*networkv1.SetIntranetPoolAllocationEnabledResponse, error) {
	v, err := s.egress.SetPool(ctx, biz.PlatformIntent{Kind: "set_intranet_pool_allocation", ID: r.GetPoolId(), Enabled: r.GetEnabled(), ExpectedVersion: r.GetExpectedVersion(), IdempotencyKey: r.GetIdempotencyKey()})
	if err != nil {
		return nil, rpcError(err)
	}
	return &networkv1.SetIntranetPoolAllocationEnabledResponse{Resource: wirePlatform(v)}, nil
}
func (s *PlatformNetworkService) SetDefaultIntranetPool(ctx context.Context, r *networkv1.SetDefaultIntranetPoolRequest) (*networkv1.SetDefaultIntranetPoolResponse, error) {
	v, err := s.egress.SetPool(ctx, biz.PlatformIntent{Kind: "set_default_intranet_pool", ID: r.GetPoolId(), ExpectedVersion: r.GetExpectedVersion(), IdempotencyKey: r.GetIdempotencyKey()})
	if err != nil {
		return nil, rpcError(err)
	}
	return &networkv1.SetDefaultIntranetPoolResponse{Resource: wirePlatform(v)}, nil
}
func (s *PlatformNetworkService) RecordIntranetPoolVerification(ctx context.Context, r *networkv1.RecordIntranetPoolVerificationRequest) (*networkv1.RecordIntranetPoolVerificationResponse, error) {
	ev := r.GetVerification()
	if ev == nil || ev.GetVerifiedAt() == nil || ev.GetExpiresAt() == nil || ev.GetVerifiedAt().CheckValid() != nil || ev.GetExpiresAt().CheckValid() != nil {
		return nil, rpcError(biz.Fail(biz.InvalidArgument, "verification timestamps are required"))
	}
	v, err := s.egress.SetPool(ctx, biz.PlatformIntent{Kind: "verify_intranet_pool", ID: r.GetPoolId(), ExpectedVersion: r.GetExpectedVersion(), IdempotencyKey: r.GetIdempotencyKey(), Verification: &biz.PublicPoolVerification{ProviderSourceRevision: ev.GetProviderSourceRevision(), ProviderImageDigests: ev.GetProviderImageDigests(), TopologyFingerprint: ev.GetTopologyFingerprint(), EvidenceReference: ev.GetEvidenceReference(), Scope: ev.GetScope(), VerifiedAt: ev.GetVerifiedAt().AsTime(), ExpiresAt: ev.GetExpiresAt().AsTime()}})
	if err != nil {
		return nil, rpcError(err)
	}
	return &networkv1.RecordIntranetPoolVerificationResponse{Resource: wirePlatform(v)}, nil
}
func (s *PlatformNetworkService) SetIntranetPoolAutoAllocationEnabled(ctx context.Context, r *networkv1.SetIntranetPoolAutoAllocationEnabledRequest) (*networkv1.SetIntranetPoolAutoAllocationEnabledResponse, error) {
	v, err := s.egress.SetPool(ctx, biz.PlatformIntent{Kind: "set_intranet_pool_auto_allocation", ID: r.GetPoolId(), Enabled: r.GetEnabled(), ExpectedVersion: r.GetExpectedVersion(), IdempotencyKey: r.GetIdempotencyKey()})
	if err != nil {
		return nil, rpcError(err)
	}
	return &networkv1.SetIntranetPoolAutoAllocationEnabledResponse{Resource: wirePlatform(v)}, nil
}
func (s *PlatformNetworkService) GetPlatformNetworkCapabilities(ctx context.Context, _ *networkv1.GetPlatformNetworkCapabilitiesRequest) (*networkv1.GetPlatformNetworkCapabilitiesResponse, error) {
	v, err := s.egress.GetPlatformCapabilities(ctx)
	if err != nil {
		return nil, rpcError(err)
	}
	wire := func(o biz.CapabilityObservation) *networkv1.CapabilityObservation {
		message := o.Reason.Message()
		if o.Reason == "CAPABILITY_UNKNOWN" {
			message = "capability has not been reported"
		}
		return &networkv1.CapabilityObservation{Reason: string(o.Reason), ReasonMessage: message, ObservedAt: optionalTime(o.ObservedAt), ObservationStale: o.ObservationStale}
	}
	return &networkv1.GetPlatformNetworkCapabilitiesResponse{Capabilities: &networkv1.PlatformNetworkCapabilities{BaseConnectivityReady: v.BaseConnectivity.Ready, PublicAddressReady: v.PublicAddress.Ready, LoadBalancerReady: v.LoadBalancer.Ready, BaseConnectivity: wire(v.BaseConnectivity), PublicAddress: wire(v.PublicAddress), LoadBalancer: wire(v.LoadBalancer)}}, nil
}
