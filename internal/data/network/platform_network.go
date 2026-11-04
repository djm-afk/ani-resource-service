package data

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/tenantnamespace"
)

// SyncPlatformBaseNetworkConfig only imports explicitly supplied original
// identities. It performs no provider discovery, mutation or readiness check.
func (p *Postgres) SyncPlatformBaseNetworkConfig(ctx context.Context, config *biz.PlatformBaseNetworkConfig) error {
	if err := biz.ValidatePlatformBaseNetworkConfig(config); err != nil {
		return err
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return databaseFailure(err)
	}
	defer tx.Rollback(ctx)
	q := p.queries.WithTx(tx)
	if err = q.LockPlatformCluster(ctx, sqlcgen.LockPlatformClusterParams{ClusterID: p.placement.ClusterID}); err != nil {
		return databaseFailure(err)
	}
	vpcIDs, subnetIDs := []string{}, []string{}
	if config != nil {
		entries := append([]biz.PlatformNetworkConfigEntry{config.VPC}, config.Subnets...)
		for _, entry := range entries {
			collision, e := q.PlatformProductIDCollision(ctx, sqlcgen.PlatformProductIDCollisionParams{ResourceID: entry.ProductID})
			if e != nil {
				return databaseFailure(e)
			}
			if collision {
				return biz.Fail(biz.InvalidArgument, "platform product ID belongs to a private network")
			}
		}
		v := config.VPC
		_, err = q.UpsertPlatformVPC(ctx, sqlcgen.UpsertPlatformVPCParams{VpcID: v.ProductID, ClusterID: p.placement.ClusterID, ClusterUid: config.ClusterUID, Namespace: v.NativeNamespace, ProviderName: v.NativeName, ProviderUid: v.NativeUID, Name: v.DisplayName, Description: v.Description, Published: v.Published, AcceptNew: v.AcceptNew})
		if errors.Is(err, pgx.ErrNoRows) {
			return biz.Fail(biz.InvalidArgument, "platform VPC original identity is immutable")
		}
		if err != nil {
			return databaseFailure(err)
		}
		vpcIDs = append(vpcIDs, v.ProductID)
		if err = q.EnsurePlatformNetworkObservation(ctx, sqlcgen.EnsurePlatformNetworkObservationParams{ResourceID: v.ProductID, VpcID: &v.ProductID}); err != nil {
			return databaseFailure(err)
		}
		for _, s := range config.Subnets {
			_, err = q.UpsertPlatformSubnet(ctx, sqlcgen.UpsertPlatformSubnetParams{SubnetID: s.ProductID, VpcID: v.ProductID, ClusterID: p.placement.ClusterID, Namespace: s.NativeNamespace, ProviderName: s.NativeName, ProviderUid: s.NativeUID, BindingID: uuid.NewString(), Name: s.DisplayName, Description: s.Description, Published: s.Published, AcceptNew: s.AcceptNew})
			if errors.Is(err, pgx.ErrNoRows) {
				return biz.Fail(biz.InvalidArgument, "platform Subnet original identity is immutable")
			}
			if err != nil {
				return databaseFailure(err)
			}
			subnetIDs = append(subnetIDs, s.ProductID)
			if err = q.EnsurePlatformNetworkObservation(ctx, sqlcgen.EnsurePlatformNetworkObservationParams{ResourceID: s.ProductID, SubnetID: &s.ProductID}); err != nil {
				return databaseFailure(err)
			}
		}
	}
	if err = q.CloseRemovedPlatformSubnets(ctx, sqlcgen.CloseRemovedPlatformSubnetsParams{ClusterID: p.placement.ClusterID, KeepIds: subnetIDs}); err != nil {
		return databaseFailure(err)
	}
	if err = q.CloseRemovedPlatformVPCs(ctx, sqlcgen.CloseRemovedPlatformVPCsParams{ClusterID: p.placement.ClusterID, KeepIds: vpcIDs}); err != nil {
		return databaseFailure(err)
	}
	return databaseFailure(tx.Commit(ctx))
}

func (p *Postgres) rejectPlatformManagement(ctx context.Context, id string) error {
	found, err := p.readQueries(ctx).IsPlatformNetwork(ctx, sqlcgen.IsPlatformNetworkParams{ResourceID: id})
	if err != nil {
		return databaseFailure(err)
	}
	if found {
		return biz.Fail(biz.PermissionDenied, "platform network is managed by the platform")
	}
	return nil
}

func (p *Postgres) GetVisibleVPC(ctx context.Context, tenant, id string) (biz.VPC, error) {
	q := p.readQueries(ctx)
	v, err := q.GetPlatformVPC(ctx, sqlcgen.GetPlatformVPCParams{VpcID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return p.GetVPC(ctx, tenant, id)
	}
	if err != nil {
		return biz.VPC{}, databaseFailure(err)
	}
	if v.ClusterID != p.placement.ClusterID {
		return biz.VPC{}, biz.Fail(biz.ResourceNotFound, "VPC not found")
	}
	o, err := q.GetPlatformNetworkObservation(ctx, sqlcgen.GetPlatformNetworkObservationParams{ResourceID: id})
	if err != nil {
		return biz.VPC{}, databaseFailure(err)
	}
	count, err := q.PlatformVPCVisibleSubnetCount(ctx, sqlcgen.PlatformVPCVisibleSubnetCountParams{VpcID: id})
	if err != nil {
		return biz.VPC{}, databaseFailure(err)
	}
	usable, err := q.PlatformVPCCanUse(ctx, sqlcgen.PlatformVPCCanUseParams{VpcID: id})
	if err != nil {
		return biz.VPC{}, databaseFailure(err)
	}
	return biz.VPC{ID: v.VpcID, OwnerScope: biz.OwnerScopePlatform, Name: v.Name, Description: v.Description, CIDR: v.Cidr, State: biz.ResourceState(v.State), Reason: biz.Reason(v.Reason), Version: v.Version, CreatedAt: v.CreatedAt, UpdatedAt: v.UpdatedAt, ObservedAt: v.ObservedAt, ObservationStale: v.ObservedAt == nil || o.ObservationError != "", CanUse: usable, SubnetCount: count}, nil
}

func (p *Postgres) GetVisibleSubnet(ctx context.Context, tenant, id string) (biz.Subnet, error) {
	q := p.readQueries(ctx)
	s, err := q.GetPlatformSubnet(ctx, sqlcgen.GetPlatformSubnetParams{SubnetID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return p.GetSubnet(ctx, tenant, id)
	}
	if err != nil {
		return biz.Subnet{}, databaseFailure(err)
	}
	if s.ClusterID != p.placement.ClusterID {
		return biz.Subnet{}, biz.Fail(biz.ResourceNotFound, "Subnet not found")
	}
	admission, err := q.PlatformSubnetAdmission(ctx, sqlcgen.PlatformSubnetAdmissionParams{SubnetID: id})
	if err != nil {
		return biz.Subnet{}, databaseFailure(err)
	}
	so, err := q.GetPlatformNetworkObservation(ctx, sqlcgen.GetPlatformNetworkObservationParams{ResourceID: id})
	if err != nil {
		return biz.Subnet{}, databaseFailure(err)
	}
	return biz.Subnet{ID: s.SubnetID, VPCID: s.VpcID, OwnerScope: biz.OwnerScopePlatform, Name: s.Name, Description: s.Description, CIDR: s.Cidr, Gateway: s.Gateway, SubnetOptions: biz.SubnetOptions{ExcludeIPs: s.ExcludeIps, DNSServers: s.DnsServers, EnableDHCP: s.EnableDhcp}, AvailableIPRange: s.AvailableIpRange, State: biz.ResourceState(s.State), Reason: biz.Reason(s.Reason), Version: s.Version, CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt, ObservedAt: s.ObservedAt, ObservationStale: s.ObservedAt == nil || so.ObservationError != "", CanUse: admission.CanUse}, nil
}

func nullableBool(v *bool) pgtype.Bool {
	if v == nil {
		return pgtype.Bool{}
	}
	return pgtype.Bool{Bool: *v, Valid: true}
}

func (p *Postgres) ListVisibleVPCs(ctx context.Context, tenant string, f biz.VPCFilter) ([]biz.VPC, int64, error) {
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, 0, databaseFailure(err)
	}
	defer tx.Rollback(ctx)
	q := p.queries.WithTx(tx)
	ctx = context.WithValue(ctx, admissionQueriesKey{}, q)
	total, err := q.CountVisibleVPCs(ctx, sqlcgen.CountVisibleVPCsParams{TenantID: tenant, ClusterID: p.placement.ClusterID, NameFilter: f.Name, StateFilter: f.State, CanUse: nullableBool(f.CanUse), CanManage: nullableBool(f.CanManage)})
	if err != nil {
		return nil, 0, databaseFailure(err)
	}
	rows, err := q.ListVisibleVPCs(ctx, sqlcgen.ListVisibleVPCsParams{TenantID: tenant, ClusterID: p.placement.ClusterID, NameFilter: f.Name, StateFilter: f.State, CanUse: nullableBool(f.CanUse), CanManage: nullableBool(f.CanManage), AfterID: f.AfterID, AfterCreatedAt: f.AfterCreatedAt, MaxResults: f.Limit, PageOffset: f.Offset})
	if err != nil {
		return nil, 0, databaseFailure(err)
	}
	out := make([]biz.VPC, 0, len(rows))
	for _, r := range rows {
		v, e := p.GetVisibleVPC(ctx, tenant, r.ProductID)
		if e != nil {
			return nil, 0, e
		}
		v.CanManage, v.CanUse = r.CanManage, r.CanUse
		out = append(out, v)
	}
	return out, total, databaseFailure(tx.Commit(ctx))
}
func (p *Postgres) ListVisibleSubnets(ctx context.Context, tenant string, f biz.SubnetFilter) ([]biz.Subnet, int64, error) {
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, 0, databaseFailure(err)
	}
	defer tx.Rollback(ctx)
	q := p.queries.WithTx(tx)
	ctx = context.WithValue(ctx, admissionQueriesKey{}, q)
	total, err := q.CountVisibleSubnets(ctx, sqlcgen.CountVisibleSubnetsParams{TenantID: tenant, ClusterID: p.placement.ClusterID, NameFilter: f.Name, StateFilter: f.State, VpcFilter: f.VPCID, CanUse: nullableBool(f.CanUse), CanManage: nullableBool(f.CanManage)})
	if err != nil {
		return nil, 0, databaseFailure(err)
	}
	rows, err := q.ListVisibleSubnets(ctx, sqlcgen.ListVisibleSubnetsParams{TenantID: tenant, ClusterID: p.placement.ClusterID, NameFilter: f.Name, StateFilter: f.State, VpcFilter: f.VPCID, CanUse: nullableBool(f.CanUse), CanManage: nullableBool(f.CanManage), AfterID: f.AfterID, AfterCreatedAt: f.AfterCreatedAt, MaxResults: f.Limit, PageOffset: f.Offset})
	if err != nil {
		return nil, 0, databaseFailure(err)
	}
	out := make([]biz.Subnet, 0, len(rows))
	for _, r := range rows {
		v, e := p.GetVisibleSubnet(ctx, tenant, r.ProductID)
		if e != nil {
			return nil, 0, e
		}
		v.CanManage, v.CanUse = r.CanManage, r.CanUse
		out = append(out, v)
	}
	return out, total, databaseFailure(tx.Commit(ctx))
}

// Both resolvers are read-only. Accepted references never reopen admission;
// all namespaces here describe consumers, not the imported network object.
func (p *Postgres) ResolveNewNetworkSelection(ctx context.Context, s biz.NetworkSelection) (biz.ResolvedNetworkSelection, error) {
	return p.resolveNetworkSelection(ctx, s, false)
}
func (p *Postgres) ResolveAcceptedNetworkSelection(ctx context.Context, s biz.NetworkSelection) (biz.ResolvedNetworkSelection, error) {
	return p.resolveNetworkSelection(ctx, s, true)
}
func (p *Postgres) resolveNetworkSelection(ctx context.Context, s biz.NetworkSelection, accepted bool) (biz.ResolvedNetworkSelection, error) {
	if _, err := biz.ParseTenant(s.TenantID); err != nil {
		return biz.ResolvedNetworkSelection{}, err
	}
	if accepted && s.Reference.OwnerScope != "" {
		if s.VPCID != "" && s.VPCID != s.Reference.VPCID || s.SubnetID != "" && s.SubnetID != s.Reference.SubnetID {
			return biz.ResolvedNetworkSelection{}, biz.Fail(biz.PlacementMismatch, "accepted network product reference differs")
		}
		s.VPCID, s.SubnetID = s.Reference.VPCID, s.Reference.SubnetID
	}
	name, err := tenantnamespace.Name(s.TenantID)
	if err != nil {
		return biz.ResolvedNetworkSelection{}, biz.Fail(biz.TenantRequired, "canonical tenant is required")
	}
	if s.ClusterID != "" && s.ClusterID != p.placement.ClusterID || s.Namespace != "" && s.Namespace != name {
		return biz.ResolvedNetworkSelection{}, biz.Fail(biz.PlacementMismatch, "consumer placement differs")
	}
	s.ClusterID, s.Namespace = p.placement.ClusterID, name
	q := p.readQueries(ctx)
	platform, err := q.GetPlatformSubnet(ctx, sqlcgen.GetPlatformSubnetParams{SubnetID: s.SubnetID})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return biz.ResolvedNetworkSelection{}, databaseFailure(err)
	}
	out := biz.ResolvedNetworkSelection{NetworkSelection: s}
	if err == nil {
		if accepted && s.Reference.OwnerScope != biz.OwnerScopePlatform {
			return out, biz.Fail(biz.PlacementMismatch, "accepted private network reference differs")
		}
		if platform.ClusterID != p.placement.ClusterID {
			return out, biz.Fail(biz.PlacementMismatch, "network cluster differs")
		}
		if s.VPCID != "" && s.VPCID != platform.VpcID {
			return out, biz.Fail(biz.InvalidArgument, "Subnet does not belong to requested VPC")
		}
		admission, e := q.PlatformSubnetAdmission(ctx, sqlcgen.PlatformSubnetAdmissionParams{SubnetID: platform.SubnetID})
		if e != nil {
			return out, databaseFailure(e)
		}
		if admission.OwnershipConflict {
			return out, biz.Fail(biz.ProviderOwnership, "registered platform network original UID differs")
		}
		if !accepted && !admission.CanUse {
			return out, biz.Fail(biz.NetworkNewAdmissionClosed, "NETWORK_NEW_ADMISSION_CLOSED")
		}
		out.VPCID = platform.VpcID
		out.Reference = biz.NetworkReference{OwnerScope: biz.OwnerScopePlatform, VPCID: platform.VpcID, SubnetID: platform.SubnetID, SubnetBindingID: platform.BindingID}
		out.NativeNamespace, out.NativeName, out.NativeUID, out.BindingID = platform.Namespace, platform.ProviderName, platform.ProviderUid, platform.BindingID
		out.BasicIsolationName = "ani-tenant-" + s.TenantID
	} else {
		if accepted && s.Reference.OwnerScope != "" && s.Reference.OwnerScope != biz.OwnerScopeTenant {
			return out, biz.Fail(biz.ResourceNotFound, "accepted platform network not found")
		}
		row, e := q.GetSubnet(ctx, sqlcgen.GetSubnetParams{TenantID: s.TenantID, SubnetID: s.SubnetID})
		if e != nil {
			return out, databaseFailure(e)
		}
		subnet := row.NetworkSubnet
		if s.VPCID != "" && s.VPCID != subnet.VpcID {
			return out, biz.Fail(biz.InvalidArgument, "Subnet does not belong to requested VPC")
		}
		parent, e := q.GetVPC(ctx, sqlcgen.GetVPCParams{TenantID: s.TenantID, VpcID: subnet.VpcID})
		if e != nil {
			return out, databaseFailure(e)
		}
		if !networkManagementOpen(parent.NetworkVpc.State, parent.NetworkVpc.Reason) || !networkManagementOpen(subnet.State, subnet.Reason) {
			return out, biz.Fail(biz.NetworkNotReady, "VPC or Subnet management is closed")
		}
		b, e := q.GetBinding(ctx, sqlcgen.GetBindingParams{TenantID: s.TenantID, ResourceID: s.SubnetID})
		if e != nil {
			return out, databaseFailure(e)
		}
		if b.ClusterID != s.ClusterID || b.Namespace != s.Namespace || b.ProviderName == "" {
			return out, biz.Fail(biz.PlacementMismatch, "consumer placement differs from network binding")
		}
		out.VPCID = subnet.VpcID
		out.Reference = biz.NetworkReference{OwnerScope: biz.OwnerScopeTenant, VPCID: subnet.VpcID, SubnetID: subnet.SubnetID, SubnetBindingID: b.BindingID}
		out.NativeNamespace, out.NativeName, out.NativeUID, out.BindingID = b.Namespace, b.ProviderName, b.ProviderUid, b.BindingID
	}
	if accepted && s.Reference.SubnetBindingID != "" && s.Reference.SubnetBindingID != out.BindingID {
		return out, biz.Fail(biz.ProviderOwnership, "accepted Subnet binding differs")
	}
	// Preflight may run before Network has registered the existing Namespace.
	// Reading the original consumer identity never creates a Namespace or BNI.
	identity, e := p.tenantNamespaceIdentity(ctx, s.TenantID)
	if errors.Is(e, pgx.ErrNoRows) {
		identity, e = p.ResolveTenantNamespace(ctx, s.TenantID)
	}
	if e != nil {
		return out, e
	}
	if s.NamespaceUID != "" && s.NamespaceUID != identity.UID {
		return out, biz.Fail(biz.PlacementMismatch, "consumer Namespace original UID differs")
	}
	out.NamespaceUID = identity.UID
	return out, nil
}

// PrepareAcceptedTenantNetwork is invoked only after an owner is durable.
// It preserves the original Namespace UID and submits the shared-tenant BNI.
func (p *Postgres) PrepareAcceptedTenantNetwork(ctx context.Context, s biz.NetworkSelection) (biz.ResolvedNetworkSelection, error) {
	resolved, err := p.ResolveAcceptedNetworkSelection(ctx, s)
	if err != nil {
		return resolved, err
	}
	identity, err := p.ResolveTenantNamespace(ctx, s.TenantID)
	if err != nil {
		return resolved, err
	}
	if identity.UID != resolved.NamespaceUID {
		return resolved, biz.Fail(biz.PlacementMismatch, "consumer Namespace original UID differs")
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return resolved, databaseFailure(err)
	}
	defer tx.Rollback(ctx)
	if err = p.acceptTenantNamespace(ctx, p.queries.WithTx(tx), identity); err != nil {
		return resolved, err
	}
	if err = tx.Commit(ctx); err != nil {
		return resolved, databaseFailure(err)
	}
	if resolved.Reference.OwnerScope == biz.OwnerScopePlatform {
		resolved.BasicIsolationName, err = p.EnsureTenantBasicIsolation(ctx, s.TenantID)
	}
	return resolved, err
}

func normalizeNetworkReference(vpc, subnet, platformVPC, platformSubnet *string, binding string) biz.NetworkReference {
	if platformSubnet != nil {
		return biz.NetworkReference{OwnerScope: biz.OwnerScopePlatform, VPCID: textValue(platformVPC), SubnetID: *platformSubnet, SubnetBindingID: binding}
	}
	return biz.NetworkReference{OwnerScope: biz.OwnerScopeTenant, VPCID: textValue(vpc), SubnetID: textValue(subnet), SubnetBindingID: binding}
}

var _ biz.NetworkSelectionResolver = (*Postgres)(nil)
var _ biz.VisibleNetworkRepository = (*Postgres)(nil)
