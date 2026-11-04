package service

import (
	"context"
	"errors"
	"time"

	networkv1 "github.com/zhangzhe-ctrl/ani-resource-service/api/network/v1"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// NetworkService only translates the wire contract; domain decisions and
// transactions remain behind Network's use cases.
type NetworkService struct {
	vnicBindingTargets func(context.Context, biz.VNic, string, string, uint32, uint32) ([]biz.VNicConsumerScope, int64, error)
	networkv1.UnimplementedNetworkServiceServer
	network     *biz.Network
	attachments *biz.Attachments
}

func NewNetworkService(network *biz.Network, attachments ...*biz.Attachments) *NetworkService {
	s := &NetworkService{network: network}
	if len(attachments) > 0 {
		s.attachments = attachments[0]
	}
	return s
}

func (s *NetworkService) CreateVPC(ctx context.Context, r *networkv1.CreateVPCRequest) (*networkv1.CreateVPCResponse, error) {
	attribution := r.GetAttribution()
	value, err := s.network.CreateVPC(ctx, biz.CreateVPC{TenantID: r.GetTenantId(), Name: r.GetName(), CIDR: r.GetCidr(), Description: r.GetDescription(), IdempotencyKey: r.GetIdempotencyKey(),
		Attribution: biz.Attribution{Actor: attribution.GetActor(), DirectCaller: attribution.GetDirectCaller(), CorrelationID: attribution.GetCorrelationId()}})
	if err != nil {
		return nil, rpcError(err)
	}
	return &networkv1.CreateVPCResponse{Vpc: wireVPC(value)}, nil
}
func (s *NetworkService) GetVPC(ctx context.Context, r *networkv1.GetVPCRequest) (*networkv1.GetVPCResponse, error) {
	value, err := s.network.GetVPC(ctx, r.GetTenantId(), r.GetVpcId())
	if err != nil {
		return nil, rpcError(err)
	}
	return &networkv1.GetVPCResponse{Vpc: wireVPC(value)}, nil
}
func (s *NetworkService) ListVPCs(ctx context.Context, r *networkv1.ListVPCsRequest) (*networkv1.ListVPCsResponse, error) {
	if r == nil {
		return nil, rpcError(biz.Fail(biz.InvalidArgument, "request required"))
	}
	state, exists := statesFromWire[r.GetState()]
	if !exists {
		return nil, rpcError(biz.Fail(biz.InvalidArgument, "invalid VPC state filter"))
	}
	value, err := s.network.ListVPCs(ctx, biz.ListVPCs{TenantID: r.GetTenantId(), Name: r.GetName(), State: string(state), Limit: int(r.GetLimit()), Cursor: r.GetCursor(), Page: r.Page, PageSize: r.PageSize, CanUse: r.CanUse, CanManage: r.CanManage})
	if err != nil {
		return nil, rpcError(err)
	}
	result := &networkv1.ListVPCsResponse{Total: value.Total, NextCursor: value.NextCursor, Items: make([]*networkv1.VPC, 0, len(value.Items))}
	for _, v := range value.Items {
		result.Items = append(result.Items, wireVPC(v))
	}
	return result, nil
}
func (s *NetworkService) DeleteVPC(ctx context.Context, r *networkv1.DeleteVPCRequest) (*networkv1.DeleteVPCResponse, error) {
	value, err := s.network.DeleteVPC(ctx, r.GetTenantId(), r.GetVpcId())
	if err != nil {
		return nil, rpcError(err)
	}
	return &networkv1.DeleteVPCResponse{Vpc: wireVPC(value)}, nil
}
func (s *NetworkService) GetOperation(ctx context.Context, r *networkv1.GetOperationRequest) (*networkv1.GetOperationResponse, error) {
	v, err := s.network.GetOperation(ctx, r.GetTenantId(), r.GetOperationId())
	if err != nil {
		return nil, rpcError(err)
	}
	return &networkv1.GetOperationResponse{Operation: &networkv1.Operation{Id: v.ID, TenantId: v.TenantID, ResourceId: v.ResourceID, ResourceType: resourceTypesToWire[v.ResourceType], Kind: kindsToWire[v.Kind], State: operationsToWire[v.State], Reason: string(v.Reason), ReasonMessage: v.Reason.Message(), CreatedAt: timestamppb.New(v.CreatedAt), UpdatedAt: timestamppb.New(v.UpdatedAt), CompletedAt: optionalTime(v.CompletedAt), NextAttemptAt: optionalTime(v.NextAttemptAt)}}, nil
}
func wireVPC(v biz.VPC) *networkv1.VPC {
	var base *networkv1.VPCBaseConnectivity
	if b := v.BaseConnectivity; b != nil {
		base = &networkv1.VPCBaseConnectivity{State: b.State, Reason: string(b.Reason), ReasonMessage: b.Reason.Message(), ObservedAt: optionalTime(b.ObservedAt), ObservationStale: b.ObservationStale}
	}
	var public *networkv1.VPCPublicEgress
	if e := v.PublicEgress; e != nil {
		public = &networkv1.VPCPublicEgress{BindingId: e.ID, EipId: e.EIPID, EipAddress: e.EIPAddress, BindingState: e.BindingState, State: statesToWire[e.State], Reason: string(e.Reason), ReasonMessage: e.Reason.Message(), DesiredEnabled: e.DesiredEnabled, AppliedEnabled: e.AppliedEnabled, ObservedAt: optionalTime(e.ObservedAt), ObservationStale: e.ObservationStale, Version: e.Version, LastOperationId: e.LastOperationID}
	}
	return &networkv1.VPC{OwnerScope: ownerScopeToWire(v.OwnerScope, v.TenantID), CanManage: v.CanManage, CanUse: v.CanUse, PublicEgress: public, BaseConnectivity: base, Id: v.ID, TenantId: v.TenantID, Name: v.Name, Description: v.Description, Cidr: v.CIDR, State: statesToWire[v.State], Reason: string(v.Reason), ReasonMessage: v.Reason.Message(), CreatedAt: timestamppb.New(v.CreatedAt), UpdatedAt: timestamppb.New(v.UpdatedAt), Version: v.Version, ObservedAt: optionalTime(v.ObservedAt), ObservationStale: v.ObservationStale, LastOperationId: v.LastOperationID, SubnetCount: v.SubnetCount}
}
func optionalTime(v *time.Time) *timestamppb.Timestamp {
	if v == nil {
		return nil
	}
	return timestamppb.New(*v)
}

var statesToWire = map[biz.ResourceState]networkv1.ResourceState{
	biz.Provisioning: networkv1.ResourceState_RESOURCE_STATE_PROVISIONING, biz.Available: networkv1.ResourceState_RESOURCE_STATE_AVAILABLE,
	biz.Degraded: networkv1.ResourceState_RESOURCE_STATE_DEGRADED, biz.Failed: networkv1.ResourceState_RESOURCE_STATE_FAILED,
	biz.Deleting: networkv1.ResourceState_RESOURCE_STATE_DELETING, biz.Deleted: networkv1.ResourceState_RESOURCE_STATE_DELETED,
}
var statesFromWire = map[networkv1.ResourceState]biz.ResourceState{
	networkv1.ResourceState_RESOURCE_STATE_UNSPECIFIED: "", networkv1.ResourceState_RESOURCE_STATE_PROVISIONING: biz.Provisioning,
	networkv1.ResourceState_RESOURCE_STATE_AVAILABLE: biz.Available, networkv1.ResourceState_RESOURCE_STATE_DEGRADED: biz.Degraded,
	networkv1.ResourceState_RESOURCE_STATE_FAILED: biz.Failed, networkv1.ResourceState_RESOURCE_STATE_DELETING: biz.Deleting, networkv1.ResourceState_RESOURCE_STATE_DELETED: biz.Deleted,
}
var operationsToWire = map[biz.OperationState]networkv1.OperationState{
	biz.Queued: networkv1.OperationState_OPERATION_STATE_QUEUED, biz.Running: networkv1.OperationState_OPERATION_STATE_RUNNING,
	biz.Retrying: networkv1.OperationState_OPERATION_STATE_RETRYING, biz.Blocked: networkv1.OperationState_OPERATION_STATE_BLOCKED,
	biz.Succeeded: networkv1.OperationState_OPERATION_STATE_SUCCEEDED, biz.OpFailed: networkv1.OperationState_OPERATION_STATE_FAILED,
}
var kindsToWire = map[string]networkv1.OperationKind{"create_vnic": networkv1.OperationKind_OPERATION_KIND_CREATE_VNIC, "delete_vnic": networkv1.OperationKind_OPERATION_KIND_DELETE_VNIC, "ensure_vpc_base_connectivity": networkv1.OperationKind_OPERATION_KIND_ENSURE_VPC_BASE_CONNECTIVITY, "create_subnet": networkv1.OperationKind_OPERATION_KIND_CREATE_SUBNET, "delete_subnet": networkv1.OperationKind_OPERATION_KIND_DELETE_SUBNET, "create_vpc": networkv1.OperationKind_OPERATION_KIND_CREATE_VPC, "delete_vpc": networkv1.OperationKind_OPERATION_KIND_DELETE_VPC,
	"create_eip":                   networkv1.OperationKind_OPERATION_KIND_CREATE_EIP,
	"delete_eip":                   networkv1.OperationKind_OPERATION_KIND_DELETE_EIP,
	"bind_snat":                    networkv1.OperationKind_OPERATION_KIND_BIND_SNAT,
	"set_snat_enabled":             networkv1.OperationKind_OPERATION_KIND_SET_SNAT_ENABLED,
	"delete_snat":                  networkv1.OperationKind_OPERATION_KIND_DELETE_SNAT,
	"adopt_device":                 networkv1.OperationKind_OPERATION_KIND_ADOPT_DEVICE,
	"create_vlan":                  networkv1.OperationKind_OPERATION_KIND_CREATE_VLAN,
	"delete_vlan":                  networkv1.OperationKind_OPERATION_KIND_DELETE_VLAN,
	"create_egress_gateway":        networkv1.OperationKind_OPERATION_KIND_CREATE_EGRESS_GATEWAY,
	"delete_egress_gateway":        networkv1.OperationKind_OPERATION_KIND_DELETE_EGRESS_GATEWAY,
	"create_public_pool":           networkv1.OperationKind_OPERATION_KIND_CREATE_PUBLIC_POOL,
	"delete_public_pool":           networkv1.OperationKind_OPERATION_KIND_DELETE_PUBLIC_POOL,
	"set_pool_allocation":          networkv1.OperationKind_OPERATION_KIND_SET_POOL_ALLOCATION,
	"set_default_pool":             networkv1.OperationKind_OPERATION_KIND_SET_DEFAULT_POOL,
	"verify_public_pool":           networkv1.OperationKind_OPERATION_KIND_VERIFY_PUBLIC_POOL,
	"create_intranet_pool":         networkv1.OperationKind_OPERATION_KIND_CREATE_INTRANET_POOL,
	"delete_intranet_pool":         networkv1.OperationKind_OPERATION_KIND_DELETE_INTRANET_POOL,
	"set_intranet_pool_allocation": networkv1.OperationKind_OPERATION_KIND_SET_INTRANET_POOL_ALLOCATION,
	"set_default_intranet_pool":    networkv1.OperationKind_OPERATION_KIND_SET_DEFAULT_INTRANET_POOL,
	"verify_intranet_pool":         networkv1.OperationKind_OPERATION_KIND_VERIFY_INTRANET_POOL,
	"create_load_balancer":         networkv1.OperationKind_OPERATION_KIND_CREATE_LOAD_BALANCER,
	"update_load_balancer":         networkv1.OperationKind_OPERATION_KIND_UPDATE_LOAD_BALANCER,
	"delete_load_balancer":         networkv1.OperationKind_OPERATION_KIND_DELETE_LOAD_BALANCER,
	"create_virtual_switch":        networkv1.OperationKind_OPERATION_KIND_CREATE_VIRTUAL_SWITCH,
	"delete_virtual_switch":        networkv1.OperationKind_OPERATION_KIND_DELETE_VIRTUAL_SWITCH,
}

func rpcError(err error) error {
	reason := biz.ReasonOf(err)
	if reason == "" && errors.Is(err, context.Canceled) {
		return status.Error(codes.Canceled, "request canceled")
	}
	if reason == "" && errors.Is(err, context.DeadlineExceeded) {
		return status.Error(codes.DeadlineExceeded, "request deadline exceeded; replay creation with the same key")
	}
	code := codes.Internal
	message := "Network request failed"
	var failure *biz.Error
	if errors.As(err, &failure) {
		message = failure.Message
	}
	switch reason {
	case biz.InvalidArgument, biz.TenantRequired, biz.InvalidCursor:
		code = codes.InvalidArgument
	case biz.ResourceNotFound:
		code = codes.NotFound
	case biz.IdempotencyConflict, biz.CIDROverlap, biz.AttachmentConflict, biz.AddressConflict:
		code = codes.AlreadyExists
	case biz.NetworkNewAdmissionClosed, biz.EIPInUse, biz.VIPInUse, biz.BackendIdentityMismatch, biz.VPCSnatExists, biz.ProviderOwnership, biz.ProviderStateMismatch, biz.ResourceInUse, biz.ResourceBusy, biz.ParentNotReady, biz.NetworkNotReady, biz.PlacementMismatch, biz.VersionConflict:
		code = codes.FailedPrecondition
	case biz.PermissionDenied:
		code = codes.PermissionDenied
	case biz.DependencyUnavailable, biz.PublicEgressNotReady, biz.BaseConnectivityNotReady, biz.LoadBalancerNotReady:
		code = codes.Unavailable
	default:
		reason = "INTERNAL_ERROR"
		message = "Network request failed"
	}
	value := status.New(code, message)
	withDetails, e := value.WithDetails(&errdetails.ErrorInfo{Reason: string(reason), Domain: "network.ani.io"})
	if e == nil {
		value = withDetails
	}
	return value.Err()

}

var resourceTypesToWire = map[string]networkv1.ResourceType{"vnic": networkv1.ResourceType_RESOURCE_TYPE_VNIC, "load_balancer": networkv1.ResourceType_RESOURCE_TYPE_LOAD_BALANCER, "vpc": networkv1.ResourceType_RESOURCE_TYPE_VPC, "subnet": networkv1.ResourceType_RESOURCE_TYPE_SUBNET, "eip": networkv1.ResourceType_RESOURCE_TYPE_EIP, "snat": networkv1.ResourceType_RESOURCE_TYPE_VPC_SNAT_BINDING, "device": networkv1.ResourceType_RESOURCE_TYPE_NETWORK_DEVICE, "vlan": networkv1.ResourceType_RESOURCE_TYPE_VLAN_NETWORK, "egress_gateway": networkv1.ResourceType_RESOURCE_TYPE_EGRESS_GATEWAY, "public_pool": networkv1.ResourceType_RESOURCE_TYPE_PUBLIC_ADDRESS_POOL}

func (s *NetworkService) ListVPCCIDRPresets(ctx context.Context, r *networkv1.ListVPCCIDRPresetsRequest) (*networkv1.ListVPCCIDRPresetsResponse, error) {
	if r == nil {
		return nil, rpcError(biz.Fail(biz.InvalidArgument, "request required"))
	}
	values, err := s.network.ListVPCCIDRPresets(ctx, r.TenantId)
	if err != nil {
		return nil, rpcError(err)
	}
	return &networkv1.ListVPCCIDRPresetsResponse{Cidrs: values}, nil
}

func ownerScopeToWire(scope biz.OwnerScope, tenant string) networkv1.OwnerScope {
	switch scope {
	case biz.OwnerScopePlatform:
		return networkv1.OwnerScope_OWNER_SCOPE_PLATFORM
	case biz.OwnerScopeTenant:
		return networkv1.OwnerScope_OWNER_SCOPE_TENANT
	}
	// Old immutable private acceptance snapshots predate the explicit field.
	if tenant != "" {
		return networkv1.OwnerScope_OWNER_SCOPE_TENANT
	}
	return networkv1.OwnerScope_OWNER_SCOPE_UNSPECIFIED
}
