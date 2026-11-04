package biz

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/tenantnamespace"
)

type ResourceState string
type OperationState string

const (
	Provisioning ResourceState = "provisioning"
	Available    ResourceState = "available"
	Degraded     ResourceState = "degraded"
	Failed       ResourceState = "failed"
	Deleting     ResourceState = "deleting"
	Deleted      ResourceState = "deleted"

	Queued    OperationState = "queued"
	Running   OperationState = "running"
	Retrying  OperationState = "retrying"
	Blocked   OperationState = "blocked"
	Succeeded OperationState = "succeeded"
	OpFailed  OperationState = "failed"
)

type BaseConnectivity struct {
	State            string
	Reason           Reason
	ObservedAt       *time.Time
	ObservationStale bool
}

const BaseConnectivityNotReady Reason = "BASE_CONNECTIVITY_NOT_READY"

type VPCPublicEgress struct {
	EgressMetadata
	EIPID, EIPAddress, BindingState string
	DesiredEnabled                  bool
	AppliedEnabled                  *bool
}

type VPC struct {
	OwnerScope                            OwnerScope
	CanManage, CanUse                     bool
	PublicEgress                          *VPCPublicEgress  `json:",omitempty"`
	BaseConnectivity                      *BaseConnectivity `json:",omitempty"`
	ID, TenantID, Name, CIDR, Description string
	State                                 ResourceState
	Reason                                Reason
	Version                               int64
	CreatedAt, UpdatedAt                  time.Time
	ObservedAt                            *time.Time
	ObservationStale                      bool
	LastOperationID                       string
	SubnetCount                           int64
}

type Operation struct {
	ID, TenantID, ResourceID, ResourceType string
	Kind                                   string
	State                                  OperationState
	Reason                                 Reason
	CreatedAt, UpdatedAt                   time.Time
	CompletedAt                            *time.Time
	NextAttemptAt                          *time.Time
}

// Attribution is unverified caller-supplied attribution during the agreed IAM
// deferral. It is neither tenant ownership nor authentication evidence.
type Attribution struct {
	Actor, DirectCaller, CorrelationID string
}

type CreateVPC struct {
	TenantID, Name, CIDR, Description, IdempotencyKey string
	Attribution                                       Attribution
}

type NetworkRepository interface {
	ResolveTenantNamespace(context.Context, string) (tenantnamespace.Identity, error)
	AcceptSubnet(context.Context, SubnetIntent, Attribution) (Subnet, error)
	GetSubnet(context.Context, string, string) (Subnet, error)
	ListSubnets(context.Context, string, SubnetFilter) ([]Subnet, int64, error)
	DeleteSubnet(context.Context, string, string) (Subnet, error)
	AcceptVPC(context.Context, VPCIntent, Attribution) (VPC, error)
	GetVPC(context.Context, string, string) (VPC, error)
	GetOperation(context.Context, string, string) (Operation, error)
	ListVPCs(context.Context, string, VPCFilter) ([]VPC, int64, error)
	DeleteVPC(context.Context, string, string) (VPC, error)
}

// Network owns the caller-facing use cases. Repositories never see unvalidated
// tenant scope; neither the transport nor the caller orchestrates persistence.
type Network struct {
	repository NetworkRepository
	cursorKey  []byte
}

func NewNetwork(repository NetworkRepository, cursorKey []byte) (*Network, error) {
	if repository == nil || len(cursorKey) < 32 {
		return nil, fmt.Errorf("repository and >=32 byte cursor key are required")
	}
	return &Network{repository: repository, cursorKey: append([]byte(nil), cursorKey...)}, nil
}

func (n *Network) CreateVPC(ctx context.Context, request CreateVPC) (VPC, error) {
	intent, err := NewVPCIntent(request.TenantID, request.Name, request.CIDR, request.Description, request.IdempotencyKey)
	if err != nil {
		return VPC{}, err
	}
	if err := validateAttribution(request.Attribution); err != nil {
		return VPC{}, err
	}
	intent.NamespaceIdentity, err = n.repository.ResolveTenantNamespace(ctx, intent.TenantID)
	if err != nil {
		return VPC{}, err
	}
	return n.repository.AcceptVPC(ctx, intent, request.Attribution)
}

func (n *Network) GetVPC(ctx context.Context, tenant, id string) (VPC, error) {
	tenant, err := ParseTenant(tenant)
	if err != nil {
		return VPC{}, err
	}
	if !validVPCID(id) {
		return VPC{}, Fail(ResourceNotFound, "VPC not found")
	}
	if visible, ok := n.repository.(VisibleNetworkRepository); ok {
		return visible.GetVisibleVPC(ctx, tenant, id)
	}
	return n.repository.GetVPC(ctx, tenant, id)
}

func (n *Network) GetOperation(ctx context.Context, tenant, id string) (Operation, error) {
	tenant, err := ParseTenant(tenant)
	if err != nil {
		return Operation{}, err
	}
	parsed, err := uuid.Parse(id)
	if err != nil || len(id) != 36 || parsed == uuid.Nil {
		return Operation{}, Fail(ResourceNotFound, "operation not found")
	}
	op, err := n.repository.GetOperation(ctx, tenant, parsed.String())
	if err == nil && (op.ResourceType == "eip" || op.ResourceType == "snat" || op.ResourceType == "load_balancer") {
		if _, _, err = (ContextEgressAuthorization{}).Tenant(ctx, tenant); err != nil {
			return Operation{}, err
		}
	}
	return op, err
}

func (n *Network) DeleteVPC(ctx context.Context, tenant, id string) (VPC, error) {
	tenant, err := ParseTenant(tenant)
	if err != nil {
		return VPC{}, err
	}
	if !validVPCID(id) {
		return VPC{}, Fail(ResourceNotFound, "VPC not found")
	}
	return n.repository.DeleteVPC(ctx, tenant, id)
}

func validVPCID(id string) bool {
	if len(id) != 36 || !strings.HasPrefix(id, "vpc_") {
		return false
	}
	for _, c := range id[4:] {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

func validateAttribution(attribution Attribution) error {
	for _, value := range []string{attribution.Actor, attribution.DirectCaller, attribution.CorrelationID} {
		if !utf8.ValidString(value) || utf8.RuneCountInString(value) > 256 {
			return Fail(InvalidArgument, "attribution exceeds 256 characters")
		}
		for _, character := range value {
			if unicode.IsControl(character) {
				return Fail(InvalidArgument, "attribution contains a control character")
			}
		}
	}
	return nil
}
