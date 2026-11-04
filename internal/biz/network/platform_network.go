package biz

import (
	"context"
	"strings"
	"unicode/utf8"
)

// OwnerScope identifies the network product's owner. Consumers always retain
// their actual tenant, including when their parent network is platform owned.
type OwnerScope string

const (
	OwnerScopeTenant          OwnerScope = "tenant"
	OwnerScopePlatform        OwnerScope = "platform"
	NetworkNewAdmissionClosed Reason     = "NETWORK_NEW_ADMISSION_CLOSED"
)

type NetworkReference struct {
	OwnerScope                       OwnerScope
	VPCID, SubnetID, SubnetBindingID string
}

type NetworkSelection struct {
	TenantID, VPCID, SubnetID, ClusterID, Namespace, NamespaceUID string
	Reference                                                     NetworkReference
}

type ResolvedNetworkSelection struct {
	NetworkSelection
	NativeNamespace, NativeName, NativeUID, BindingID, BasicIsolationName string
}

type NetworkSelectionResolver interface {
	ResolveNewNetworkSelection(context.Context, NetworkSelection) (ResolvedNetworkSelection, error)
	ResolveAcceptedNetworkSelection(context.Context, NetworkSelection) (ResolvedNetworkSelection, error)
}

// PlatformBaseNetworkConfig is supplied by trusted deployment configuration.
// It contains fixed original identities, not a discovery or adoption request.
type PlatformBaseNetworkConfig struct {
	ClusterUID string
	VPC        PlatformNetworkConfigEntry
	Subnets    []PlatformNetworkConfigEntry
}

type PlatformNetworkConfigEntry struct {
	ProductID, NativeNamespace, NativeName, NativeUID, DisplayName, Description string
	Published, AcceptNew                                                        bool
}

// VisibleNetworkRepository is separate from private management queries so a
// readable platform product can never become a tenant-owned mutation target.
type VisibleNetworkRepository interface {
	GetVisibleVPC(context.Context, string, string) (VPC, error)
	GetVisibleSubnet(context.Context, string, string) (Subnet, error)
	ListVisibleVPCs(context.Context, string, VPCFilter) ([]VPC, int64, error)
	ListVisibleSubnets(context.Context, string, SubnetFilter) ([]Subnet, int64, error)
}

func sameOptionalBool(a, b *bool) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

func (n *Network) ResolveNewNetworkSelection(ctx context.Context, s NetworkSelection) (ResolvedNetworkSelection, error) {
	r, ok := n.repository.(NetworkSelectionResolver)
	if !ok {
		return ResolvedNetworkSelection{}, Fail(DependencyUnavailable, "network selection resolver unavailable")
	}
	return r.ResolveNewNetworkSelection(ctx, s)
}

func (n *Network) ResolveAcceptedNetworkSelection(ctx context.Context, s NetworkSelection) (ResolvedNetworkSelection, error) {
	r, ok := n.repository.(NetworkSelectionResolver)
	if !ok {
		return ResolvedNetworkSelection{}, Fail(DependencyUnavailable, "network selection resolver unavailable")
	}
	return r.ResolveAcceptedNetworkSelection(ctx, s)
}

func ValidatePlatformBaseNetworkConfig(c *PlatformBaseNetworkConfig) error {
	if c == nil {
		return nil
	}
	if strings.TrimSpace(c.ClusterUID) == "" || c.ClusterUID != strings.TrimSpace(c.ClusterUID) || !validVPCID(c.VPC.ProductID) || c.VPC.NativeNamespace != "kcn-system" || c.VPC.NativeName != "kcn-cluster" {
		return Fail(InvalidArgument, "fixed platform VPC and original cluster UID are required")
	}
	ids, native := map[string]bool{}, map[string]bool{}
	entries := append([]PlatformNetworkConfigEntry{c.VPC}, c.Subnets...)
	for i, e := range entries {
		if (i > 0 && !validSubnetID(e.ProductID)) || ids[e.ProductID] || !namespaceName.MatchString(e.NativeNamespace) || !namespaceName.MatchString(e.NativeName) || strings.TrimSpace(e.NativeUID) == "" || strings.TrimSpace(e.NativeUID) != e.NativeUID || utf8.RuneCountInString(e.DisplayName) < 1 || utf8.RuneCountInString(e.DisplayName) > 128 || utf8.RuneCountInString(e.Description) > 1024 {
			return Fail(InvalidArgument, "invalid or duplicate platform network declaration")
		}
		ids[e.ProductID] = true
		if i > 0 {
			key := e.NativeNamespace + "/" + e.NativeName
			if native[key] {
				return Fail(InvalidArgument, "native Subnet cannot have multiple product IDs")
			}
			native[key] = true
		}
	}
	return nil
}
