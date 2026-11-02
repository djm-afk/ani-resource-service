package biz

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/netip"
	"strings"
	"time"
)

const (
	CIDROverlap       Reason = "CIDR_OVERLAP"
	ParentNotReady    Reason = "PARENT_NOT_READY"
	NetworkNotReady   Reason = "NETWORK_NOT_READY"
	PlacementMismatch Reason = "PLACEMENT_MISMATCH"
)

type Subnet struct {
	AvailableIPRange, UsedIPRange                         *string
	ID, TenantID, VPCID, Name, CIDR, Gateway, Description string
	State                                                 ResourceState
	Reason                                                Reason
	Version                                               int64
	CreatedAt, UpdatedAt                                  time.Time
	ObservedAt                                            *time.Time
	ObservationStale                                      bool
	LastOperationID                                       string
}

// SubnetIPRanges are facts from the matching provider status. Nil means unknown; a present empty string is an observed empty range.
type SubnetIPRanges struct {
	Available, Used *string
}

type CreateSubnet struct {
	TenantID, VPCID, Name, CIDR, Description, IdempotencyKey string
	// nil is the default gateway; a present empty string is invalid.
	Gateway     *string
	Attribution Attribution
}

type SubnetIntent struct {
	VPCIntent
	VPCID, Gateway string
}

func NewSubnetIntent(r CreateSubnet) (SubnetIntent, error) {
	common, err := NewVPCIntent(r.TenantID, r.Name, r.CIDR, r.Description, r.IdempotencyKey)
	if err != nil {
		return SubnetIntent{}, err
	}
	if !validVPCID(r.VPCID) {
		return SubnetIntent{}, Fail(ResourceNotFound, "parent VPC not found")
	}
	prefix := netip.MustParsePrefix(common.CIDR)
	gateway := prefix.Addr().Next()
	if r.Gateway != nil {
		gateway, err = netip.ParseAddr(*r.Gateway)
		if err != nil || !gateway.Is4() {
			return SubnetIntent{}, Fail(InvalidArgument, "gateway must be an IPv4 host address")
		}
	}
	if !prefix.Contains(gateway) || gateway == prefix.Addr() || !prefix.Contains(gateway.Next()) {
		return SubnetIntent{}, Fail(InvalidArgument, "gateway must be a usable host within the subnet")
	}
	return SubnetIntent{VPCIntent: common, VPCID: r.VPCID, Gateway: gateway.String()}, nil
}

func (s SubnetIntent) Fingerprint() string {
	value, _ := json.Marshal(struct {
		Version                                 int
		Name, CIDR, Description, VPCID, Gateway string
	}{1, s.Name, s.CIDR, s.Description, s.VPCID, s.Gateway})
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func (n *Network) CreateSubnet(ctx context.Context, request CreateSubnet) (Subnet, error) {
	intent, err := NewSubnetIntent(request)
	if err != nil {
		return Subnet{}, err
	}
	if err := validateAttribution(request.Attribution); err != nil {
		return Subnet{}, err
	}
	intent.NamespaceIdentity, err = n.repository.ResolveTenantNamespace(ctx, intent.TenantID)
	if err != nil {
		return Subnet{}, err
	}
	return n.repository.AcceptSubnet(ctx, intent, request.Attribution, n.freshness)
}
func (n *Network) GetSubnet(ctx context.Context, tenant, id string) (Subnet, error) {
	tenant, err := ParseTenant(tenant)
	if err != nil {
		return Subnet{}, err
	}
	if !validSubnetID(id) {
		return Subnet{}, Fail(ResourceNotFound, "subnet not found")
	}
	value, err := n.repository.GetSubnet(ctx, tenant, id)
	return n.subnetObservation(value), err
}
func (n *Network) DeleteSubnet(ctx context.Context, tenant, id string) (Subnet, error) {
	tenant, err := ParseTenant(tenant)
	if err != nil {
		return Subnet{}, err
	}
	if !validSubnetID(id) {
		return Subnet{}, Fail(ResourceNotFound, "subnet not found")
	}
	value, err := n.repository.DeleteSubnet(ctx, tenant, id)
	return n.subnetObservation(value), err
}
func (n *Network) subnetObservation(value Subnet) Subnet {
	value.ObservationStale = value.ObservedAt == nil || n.now().Sub(*value.ObservedAt) > n.freshness
	return value
}
func validSubnetID(id string) bool {
	if len(id) != 39 || !strings.HasPrefix(id, "subnet_") {
		return false
	}
	for _, c := range id[7:] {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

type ListSubnets struct {
	TenantID, VPCID, Name, State, Cursor string
	Limit                                int
}
type SubnetFilter struct {
	VPCFilter
	VPCID string
}
type SubnetPage struct {
	Total      int64
	Items      []Subnet
	NextCursor string
}

func (n *Network) ListSubnets(ctx context.Context, r ListSubnets) (SubnetPage, error) {
	tenant, err := ParseTenant(r.TenantID)
	if err != nil {
		return SubnetPage{}, err
	}
	common, err := normalizeList(r.Name, r.State, r.Limit)
	if err != nil {
		return SubnetPage{}, err
	}
	if r.VPCID != "" && !validVPCID(r.VPCID) {
		return SubnetPage{}, Fail(InvalidArgument, "invalid VPC filter")
	}
	filter := SubnetFilter{VPCFilter: common, VPCID: r.VPCID}
	if r.Cursor != "" {
		c, err := n.decodeCursor(r.Cursor)
		if err != nil || c.Version != 1 || c.Kind != "subnet" || c.TenantID != tenant || c.VPCID != r.VPCID || c.Name != filter.Name || c.State != r.State || !validSubnetID(c.ID) || c.CreatedAt.IsZero() {
			return SubnetPage{}, Fail(InvalidCursor, "cursor does not match this query")
		}
		filter.AfterCreatedAt, filter.AfterID = c.CreatedAt, c.ID
	}
	rows, total, err := n.repository.ListSubnets(ctx, tenant, filter)
	if err != nil {
		return SubnetPage{}, err
	}
	limit := int(filter.Limit) - 1
	page := SubnetPage{Total: total, Items: make([]Subnet, 0, limit)}
	for i, value := range rows {
		if i == limit {
			last := rows[i-1]
			page.NextCursor = n.encodeCursor(vpcCursor{Version: 1, Kind: "subnet", TenantID: tenant, VPCID: r.VPCID, Name: filter.Name, State: r.State, ID: last.ID, CreatedAt: last.CreatedAt})
			break
		}
		page.Items = append(page.Items, n.subnetObservation(value))
	}
	return page, nil
}
