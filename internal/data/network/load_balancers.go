package data

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
	"net/netip"
	"slices"
	"strings"
	"time"
)

var _ biz.LoadBalancerRepository = (*Postgres)(nil)

func loadLB(ctx context.Context, q *sqlcgen.Queries, row sqlcgen.NetworkLoadBalancer) (biz.LoadBalancer, error) {
	return loadLBVersion(ctx, q, row, row.DesiredVersion)
}

// Workers hydrate the persisted Provider target while GET reads the latest
// complete user configuration. Metadata retains all three version counters.
func loadLBProviderTarget(ctx context.Context, q *sqlcgen.Queries, row sqlcgen.NetworkLoadBalancer) (biz.LoadBalancer, error) {
	target := row.DesiredVersion
	if row.ProviderTargetVersion.Valid {
		target = row.ProviderTargetVersion.Int64
	}
	return loadLBVersion(ctx, q, row, target)
}

func loadLBVersion(ctx context.Context, q *sqlcgen.Queries, row sqlcgen.NetworkLoadBalancer, configVersion int64) (biz.LoadBalancer, error) {
	if row.LastOperationID == nil {
		return biz.LoadBalancer{}, biz.Fail(biz.ResourceNotFound, "load balancer not found")
	}
	l, err := q.GetLBListener(ctx, sqlcgen.GetLBListenerParams{TenantID: row.TenantID, LbID: row.LbID})
	if err != nil {
		return biz.LoadBalancer{}, databaseFailure(err)
	}
	c, err := q.GetLBConfiguration(ctx, sqlcgen.GetLBConfigurationParams{TenantID: row.TenantID, LbID: row.LbID, ConfigVersion: configVersion})
	if err != nil {
		return biz.LoadBalancer{}, databaseFailure(err)
	}
	members, err := q.ListLBMembers(ctx, sqlcgen.ListLBMembersParams{TenantID: row.TenantID, LbID: row.LbID, ConfigVersion: configVersion})
	if err != nil {
		return biz.LoadBalancer{}, databaseFailure(err)
	}
	v := biz.LoadBalancer{EgressMetadata: biz.EgressMetadata{ID: row.LbID, Name: row.Name, Description: row.Description, State: biz.ResourceState(row.State), Reason: biz.Reason(row.Reason), Version: row.Version, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt, ObservedAt: row.ObservedAt, ObservationStale: row.ObservedAt == nil, LastOperationID: *row.LastOperationID}, TenantID: row.TenantID, VPCID: row.VpcID, SubnetID: row.SubnetID, Exposure: row.Exposure, Flavor: row.Flavor, PublicEIPID: textValue(row.PublicEipID), PrivateIP: textValue(row.PrivateIp), Listener: biz.LoadBalancerListener{ID: l.ListenerID, Port: uint32(l.Port)}, Health: biz.LoadBalancerHealth{Port: uint32(c.HealthCheckPort), IntervalSeconds: uint32(c.IntervalSeconds), TimeoutSeconds: uint32(c.TimeoutSeconds), UnhealthyThreshold: uint32(c.UnhealthyThreshold), HealthyThreshold: uint32(c.HealthyThreshold)}, DesiredVersion: row.DesiredVersion, ProviderTargetVersion: lbProviderTargetVersion(row), AppliedVersion: row.AppliedVersion, ConfigurationState: row.ConfigurationState, Backends: make([]biz.LoadBalancerBackend, 0, len(members))}
	for _, item := range members {
		m := item.NetworkLbMember
		v.Backends = append(v.Backends, biz.LoadBalancerBackend{ID: m.MemberID, SubnetID: m.SubnetID, Address: m.Address, Port: uint32(m.Port), Weight: uint32(item.Weight), AttachmentID: m.AttachmentID, State: m.State, Reason: biz.Reason(m.Reason), ObservedAt: m.ObservedAt, ObservationStale: m.ObservedAt == nil})
	}
	if err = loadLBListenersVersion(ctx, q, row, configVersion, &v); err != nil {
		return v, err
	}
	if row.PublicEipID != nil {
		e, err := q.GetEIPInternal(ctx, sqlcgen.GetEIPInternalParams{TenantID: row.TenantID, EipID: *row.PublicEipID})
		if err != nil {
			return v, databaseFailure(err)
		}
		v.PublicAddress = e.Address
	}
	components, err := q.ListLBComponents(ctx, sqlcgen.ListLBComponentsParams{TenantID: row.TenantID, LbID: row.LbID})
	if err != nil {
		return v, databaseFailure(err)
	}
	facts := make([]biz.LoadBalancerComponent, 0, len(components))
	for _, c := range components {
		facts = append(facts, biz.LoadBalancerComponent{ID: c.ComponentID, Kind: c.Kind, MemberID: textValue(c.MemberID), ListenerID: textValue(c.ListenerID), Name: c.ProviderName, Identity: c.ProviderUid, PendingAction: c.PendingAction, CreateDispatched: c.CreateDispatched, Deleted: c.DeletedAt != nil, TargetVersion: c.TargetVersion, AppliedVersion: c.AppliedVersion})
	}
	binding, err := q.GetBinding(ctx, sqlcgen.GetBindingParams{TenantID: row.TenantID, ResourceID: row.LbID})
	if err != nil {
		return v, databaseFailure(err)
	}
	facts = append(facts, biz.LoadBalancerComponent{Kind: "gateway", Name: binding.ProviderName, Identity: binding.ProviderUid, PendingAction: binding.PendingAction, CreateDispatched: binding.CreateDispatched})
	biz.SetLoadBalancerListenerDeploymentStates(&v, facts)
	return v, nil
}
func lbProviderTargetVersion(row sqlcgen.NetworkLoadBalancer) int64 {
	if !row.ProviderTargetVersion.Valid {
		return 0
	}
	return row.ProviderTargetVersion.Int64
}
func lbResult(ctx context.Context, q *sqlcgen.Queries, row sqlcgen.NetworkLoadBalancer) (biz.LoadBalancerResult, error) {
	v, err := loadLB(ctx, q, row)
	if err != nil {
		return biz.LoadBalancerResult{}, err
	}
	op, err := q.GetOperation(ctx, sqlcgen.GetOperationParams{TenantID: row.TenantID, OperationID: v.LastOperationID})
	if err != nil {
		return biz.LoadBalancerResult{}, databaseFailure(err)
	}
	if textValue(op.LbID) != row.LbID {
		return biz.LoadBalancerResult{}, biz.Fail(biz.DependencyUnavailable, "invalid load balancer operation relationship")
	}
	return biz.LoadBalancerResult{LoadBalancer: v, Operation: operation(op)}, nil
}
func lbReadDiagnostics(v biz.LoadBalancer, readError string) biz.LoadBalancer {
	v.ObservationStale = v.ObservedAt == nil || readError != ""
	for i := range v.Backends {
		v.Backends[i].ObservationStale = v.Backends[i].ObservedAt == nil || readError != ""
	}
	for i := range v.Listeners {
		for j := range v.Listeners[i].Backends {
			member := &v.Listeners[i].Backends[j]
			member.ObservationStale = member.ObservedAt == nil || readError != ""
		}
	}
	if len(v.Listeners) != 0 {
		v.Listener = v.Listeners[0]
	}
	return v
}
func (p *Postgres) GetLoadBalancer(ctx context.Context, tenant, id string) (biz.LoadBalancer, error) {
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return biz.LoadBalancer{}, databaseFailure(err)
	}
	defer tx.Rollback(ctx)
	q := p.queries.WithTx(tx)
	row, err := q.GetLB(ctx, sqlcgen.GetLBParams{TenantID: tenant, LbID: id})
	if err != nil {
		return biz.LoadBalancer{}, databaseFailure(err)
	}
	v, err := loadLB(ctx, q, row.NetworkLoadBalancer)
	return lbReadDiagnostics(v, row.ObservationError), err
}
func (p *Postgres) ListLoadBalancers(ctx context.Context, tenant string, f biz.LoadBalancerFilter) ([]biz.LoadBalancer, int64, error) {
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, 0, databaseFailure(err)
	}
	defer tx.Rollback(ctx)
	q := p.queries.WithTx(tx)
	total, err := q.CountLBs(ctx, sqlcgen.CountLBsParams{TenantID: tenant, NameFilter: f.Name, VpcFilter: f.VPCID, SubnetFilter: f.SubnetID, ExposureFilter: f.Exposure, StateFilter: f.State})
	if err != nil {
		return nil, 0, databaseFailure(err)
	}
	rows, err := q.ListLBs(ctx, sqlcgen.ListLBsParams{TenantID: tenant, NameFilter: f.Name, VpcFilter: f.VPCID, SubnetFilter: f.SubnetID, ExposureFilter: f.Exposure, StateFilter: f.State, AfterID: f.AfterID, AfterCreatedAt: f.AfterCreatedAt, MaxResults: f.Limit})
	if err != nil {
		return nil, 0, databaseFailure(err)
	}
	out := make([]biz.LoadBalancer, 0, len(rows))
	for _, row := range rows {
		v, err := loadLB(ctx, q, row.NetworkLoadBalancer)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, lbReadDiagnostics(v, row.ObservationError))
	}
	return out, total, nil
}
func (p *Postgres) GetLoadBalancerOperation(ctx context.Context, tenant, id string) (biz.Operation, error) {
	op, err := p.queries.GetOperation(ctx, sqlcgen.GetOperationParams{TenantID: tenant, OperationID: id})
	if err != nil {
		return biz.Operation{}, databaseFailure(err)
	}
	if op.LbID == nil {
		return biz.Operation{}, biz.Fail(biz.ResourceNotFound, "operation not found")
	}
	return operation(op), nil
}
func lbReplay(ctx context.Context, q *sqlcgen.Queries, i biz.LoadBalancerIntent) (biz.LoadBalancerResult, bool, error) {
	var result biz.LoadBalancerResult
	if err := q.LockEgressKey(ctx, sqlcgen.LockEgressKeyParams{TenantID: i.TenantID, OperationKind: i.Kind, IdempotencyKey: i.IdempotencyKey}); err != nil {
		return result, false, databaseFailure(err)
	}
	prev, err := q.GetEgressIdempotency(ctx, sqlcgen.GetEgressIdempotencyParams{TenantID: i.TenantID, OperationKind: i.Kind, IdempotencyKey: i.IdempotencyKey})
	if errors.Is(err, pgx.ErrNoRows) {
		return result, false, nil
	}
	if err != nil {
		return result, false, databaseFailure(err)
	}
	if prev.FingerprintVersion != 1 || prev.Fingerprint != i.Fingerprint() {
		return result, false, biz.Fail(biz.IdempotencyConflict, "key already accepted another load balancer intent")
	}
	if err = json.Unmarshal(prev.Response, &result); err != nil {
		return result, false, databaseFailure(err)
	}
	if result.LoadBalancer.ID != textValue(prev.LbID) || result.LoadBalancer.TenantID != i.TenantID || result.Operation.TenantID != i.TenantID || result.Operation.ID != prev.OperationID || result.Operation.ResourceID != result.LoadBalancer.ID || result.LoadBalancer.LastOperationID != prev.OperationID {
		return result, false, biz.Fail(biz.DependencyUnavailable, "invalid load balancer acceptance snapshot")
	}
	return result, true, nil
}

// The parent VPC lock serializes acceptance with all Attachment writes. These
// match the current Res consumer and its address projection. No remote IO
// occurs in the transaction.
func lbBackendIdentity(ctx context.Context, q *sqlcgen.Queries, tenant, cluster, namespace, vpc string, b biz.LoadBalancerBackend) (sqlcgen.InsertLBMemberParams, error) {
	rows, err := q.ListLBBackendAttachments(ctx, sqlcgen.ListLBBackendAttachmentsParams{TenantID: tenant, SubnetID: b.SubnetID})
	if err != nil {
		return sqlcgen.InsertLBMemberParams{}, databaseFailure(err)
	}
	var found []sqlcgen.InsertLBMemberParams
	for _, a := range rows {
		reference := normalizeNetworkReference(a.VpcID, a.SubnetID, a.PlatformVpcID, a.PlatformSubnetID, a.BindingID)
		if reference.OwnerScope != biz.OwnerScopeTenant || a.State != "attached" || a.ClusterID != cluster || a.Namespace != namespace || reference.VPCID != vpc || a.PodUid == "" {
			continue
		}
		for _, address := range a.CurrentIps {
			if address != b.Address {
				continue
			}
			found = append(found, sqlcgen.InsertLBMemberParams{TenantID: tenant, ClusterID: cluster, Namespace: namespace, VpcID: vpc, SubnetID: b.SubnetID, AttachmentID: a.AttachmentID, Address: b.Address, Port: int32(b.Port), PodUid: a.PodUid, ObservedAt: a.ObservedAt})
			break
		}
	}
	if len(found) != 1 {
		return sqlcgen.InsertLBMemberParams{}, biz.Fail(biz.BackendIdentityMismatch, "backend requires one current Attachment address")
	}
	return found[0], nil
}
func sameLBMember(m sqlcgen.NetworkLbMember, c sqlcgen.InsertLBMemberParams) bool {
	return m.SubnetID == c.SubnetID && m.Address == c.Address && m.Port == c.Port && m.AttachmentID == c.AttachmentID && m.PodUid == c.PodUid
}
func lockLBSubnets(ctx context.Context, q *sqlcgen.Queries, tenant, vpc string, ids []string) (map[string]sqlcgen.NetworkSubnet, error) {
	slices.Sort(ids)
	ids = slices.Compact(ids)
	out := map[string]sqlcgen.NetworkSubnet{}
	for _, id := range ids {
		s, err := q.LockSubnet(ctx, sqlcgen.LockSubnetParams{TenantID: tenant, SubnetID: id})
		if err != nil {
			return nil, databaseFailure(err)
		}
		if s.VpcID != vpc {
			return nil, biz.Fail(biz.PlacementMismatch, "subnet belongs to another VPC")
		}
		out[id] = s
	}
	return out, nil
}
func (p *Postgres) AcceptLoadBalancer(ctx context.Context, i biz.LoadBalancerIntent, a biz.Attribution) (biz.LoadBalancerResult, error) {
	acceptedFingerprint := i.Fingerprint()
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return biz.LoadBalancerResult{}, databaseFailure(err)
	}
	defer tx.Rollback(ctx)
	q := p.queries.WithTx(tx)
	ctx = context.WithValue(ctx, admissionQueriesKey{}, q)
	if result, found, err := lbReplay(ctx, q, i); found || err != nil {
		return result, err
	}
	creating := i.Kind == "create_load_balancer"
	var prior sqlcgen.NetworkLoadBalancer
	vpcID, subnetID, eipID := i.VPCID, i.SubnetID, i.PublicEIPID
	if !creating {
		prior, err = q.GetLBInternal(ctx, sqlcgen.GetLBInternalParams{TenantID: i.TenantID, LbID: i.ID})
		if err != nil {
			return biz.LoadBalancerResult{}, databaseFailure(err)
		}
		if prior.LastOperationID == nil {
			return biz.LoadBalancerResult{}, biz.Fail(biz.ResourceNotFound, "load balancer not found")
		}
		vpcID, subnetID, eipID = prior.VpcID, prior.SubnetID, textValue(prior.PublicEipID)
	}
	for _, id := range []string{vpcID, subnetID} {
		if id != "" {
			if err = p.rejectPlatformManagement(ctx, id); err != nil {
				return biz.LoadBalancerResult{}, err
			}
		}
	}
	parent, err := q.LockVPC(ctx, sqlcgen.LockVPCParams{TenantID: i.TenantID, VpcID: vpcID})
	if err != nil {
		return biz.LoadBalancerResult{}, databaseFailure(err)
	}
	var priorListeners *sqlcgen.NetworkLoadBalancer
	if !creating {
		priorListeners = &prior
	}
	if err = prepareLBListeners(ctx, q, &i, priorListeners); err != nil {
		return biz.LoadBalancerResult{}, err
	}
	projection := biz.ProjectLoadBalancerListeners(i.Listeners)
	projectionChanged := len(projection) > 0
	if !creating {
		current, e := loadLB(ctx, q, prior)
		if e != nil {
			return biz.LoadBalancerResult{}, e
		}
		projectionChanged = !biz.LoadBalancerProjectionEqual(current.Listeners, i.Listeners)
	}
	ids := []string{subnetID}
	for _, b := range i.Backends {
		ids = append(ids, b.SubnetID)
	}
	if !creating {
		refs, err := q.ListLBSubnetRefs(ctx, sqlcgen.ListLBSubnetRefsParams{TenantID: i.TenantID, LbID: i.ID})
		if err != nil {
			return biz.LoadBalancerResult{}, databaseFailure(err)
		}
		for _, r := range refs {
			ids = append(ids, r.SubnetID)
		}
	}
	subnets, err := lockLBSubnets(ctx, q, i.TenantID, vpcID, ids)
	if err != nil {
		return biz.LoadBalancerResult{}, err
	}
	var eip sqlcgen.NetworkEip
	if eipID != "" {
		eip, err = q.LockEIP(ctx, sqlcgen.LockEIPParams{TenantID: i.TenantID, EipID: eipID})
		if err != nil {
			return biz.LoadBalancerResult{}, databaseFailure(err)
		}
		if err = requireTenantPublicEIP(eip); err != nil {
			return biz.LoadBalancerResult{}, err
		}
	}
	if !creating {
		prior, err = q.LockLB(ctx, sqlcgen.LockLBParams{TenantID: i.TenantID, LbID: i.ID})
		if err != nil {
			return biz.LoadBalancerResult{}, databaseFailure(err)
		}
		if prior.State == "deleting" || prior.State == "deleted" {
			return biz.LoadBalancerResult{}, biz.Fail(biz.ResourceBusy, "load balancer deletion has closed updates")
		}
		if prior.Version != i.ExpectedVersion {
			return biz.LoadBalancerResult{}, biz.Fail(biz.VersionConflict, "load balancer version changed")
		}
		if err = completedOperation(ctx, q, i.TenantID, textValue(prior.LastOperationID)); err != nil {
			return biz.LoadBalancerResult{}, err
		}
	}
	now, err := q.DatabaseTime(ctx)
	if err != nil {
		return biz.LoadBalancerResult{}, databaseFailure(err)
	}
	if parent.State == "deleting" || parent.State == "deleted" {
		return biz.LoadBalancerResult{}, biz.Fail(biz.ParentNotReady, "VPC has closed admission")
	}
	pb, err := q.GetBinding(ctx, sqlcgen.GetBindingParams{TenantID: i.TenantID, ResourceID: vpcID})
	if err != nil {
		return biz.LoadBalancerResult{}, databaseFailure(err)
	}
	if pb.ClusterID != p.placement.ClusterID {
		return biz.LoadBalancerResult{}, biz.Fail(biz.ProviderOwnership, "parent placement differs")
	}
	// Only entry and new desired backend subnets must be available. Retired
	// configuration references remain locked/occupied until Provider cleanup.
	needed := map[string]bool{subnetID: true}
	for _, b := range i.Backends {
		needed[b.SubnetID] = true
	}
	for id := range needed {
		s := subnets[id]
		sb, err := q.GetBinding(ctx, sqlcgen.GetBindingParams{TenantID: i.TenantID, ResourceID: id})
		if err != nil {
			return biz.LoadBalancerResult{}, databaseFailure(err)
		}
		if s.State == "deleting" || s.State == "deleted" {
			return biz.LoadBalancerResult{}, biz.Fail(biz.ParentNotReady, "subnet has closed admission")
		}
		if sb.ClusterID != pb.ClusterID || sb.Namespace != pb.Namespace {
			return biz.LoadBalancerResult{}, biz.Fail(biz.ProviderOwnership, "subnet placement or identity differs")
		}
	}
	if eipID != "" {
		if eip.ClusterID != pb.ClusterID || eip.Namespace != pb.Namespace {
			return biz.LoadBalancerResult{}, biz.Fail(biz.ProviderOwnership, "EIP placement differs")
		}
		eb, err := q.GetBinding(ctx, sqlcgen.GetBindingParams{TenantID: i.TenantID, ResourceID: eipID})
		if err != nil {
			return biz.LoadBalancerResult{}, databaseFailure(err)
		}
		if eb.ClusterID != pb.ClusterID || eb.Namespace != pb.Namespace || eip.State == "deleting" || eip.State == "deleted" {
			return biz.LoadBalancerResult{}, biz.Fail(biz.PublicEgressNotReady, "Public EIP placement differs or admission is closed")
		}
		pool, err := q.GetPublicPool(ctx, sqlcgen.GetPublicPoolParams{ClusterID: eip.ClusterID, ResourceID: eip.PoolID})
		if err != nil {
			return biz.LoadBalancerResult{}, databaseFailure(err)
		}
		if pool.ConfigRevision != eip.PoolRevision {
			return biz.LoadBalancerResult{}, biz.Fail(biz.PublicEgressNotReady, "fixed exit configuration changed")
		}
		if creating {
			count, err := q.BlockingSnatForEIP(ctx, sqlcgen.BlockingSnatForEIPParams{TenantID: i.TenantID, EipID: eipID})
			if err != nil {
				return biz.LoadBalancerResult{}, databaseFailure(err)
			}
			if count > 0 {
				return biz.LoadBalancerResult{}, biz.Fail(biz.EIPInUse, "EIP is reserved by a binding")
			}
		}
	}
	if creating && i.PrivateIP != "" {
		s := subnets[subnetID]
		cidr, ce := netip.ParsePrefix(s.Cidr)
		ip, ie := netip.ParseAddr(i.PrivateIP)
		if ce != nil || ie != nil || !cidr.Contains(ip) || ip == cidr.Addr() || ip == lastAddress(cidr) || i.PrivateIP == s.Gateway {
			return biz.LoadBalancerResult{}, biz.Fail(biz.InvalidArgument, "VIP is not a usable address in the entry subnet")
		}
	}
	// Resolve every backend before writing any product intent. Retained member
	// IDs preserve the endpoint and its accepted current consumer.
	identities := make([]sqlcgen.InsertLBMemberParams, len(i.Backends))
	current := map[string]bool{}
	if !creating {
		members, err := q.ListLBMembers(ctx, sqlcgen.ListLBMembersParams{TenantID: i.TenantID, LbID: i.ID, ConfigVersion: prior.DesiredVersion})
		if err != nil {
			return biz.LoadBalancerResult{}, databaseFailure(err)
		}
		for _, m := range members {
			current[m.NetworkLbMember.MemberID] = true
		}
	}
	for j, b := range i.Backends {
		cidr, ce := netip.ParsePrefix(subnets[b.SubnetID].Cidr)
		ip, ie := netip.ParseAddr(b.Address)
		if ce != nil || ie != nil || !cidr.Contains(ip) {
			return biz.LoadBalancerResult{}, biz.Fail(biz.BackendIdentityMismatch, "backend address does not match its subnet identity")
		}
		if !projectionChanged {
			if b.ID == "" || !current[b.ID] {
				return biz.LoadBalancerResult{}, biz.Fail(biz.BackendIdentityMismatch, "unchanged runtime projection requires retained member identities")
			}
			continue
		}
		c, err := lbBackendIdentity(ctx, q, i.TenantID, pb.ClusterID, pb.Namespace, vpcID, b)
		if err != nil {
			return biz.LoadBalancerResult{}, err
		}
		if b.ID != "" {
			if !current[b.ID] {
				return biz.LoadBalancerResult{}, biz.Fail(biz.BackendIdentityMismatch, "member is not part of the current configuration")
			}
			m, err := q.GetLBMember(ctx, sqlcgen.GetLBMemberParams{TenantID: i.TenantID, LbID: i.ID, MemberID: b.ID})
			if err != nil {
				return biz.LoadBalancerResult{}, databaseFailure(err)
			}
			if !sameLBMember(m, c) {
				return biz.LoadBalancerResult{}, biz.Fail(biz.BackendIdentityMismatch, "retained member identity changed")
			}
		}
		identities[j] = c
	}
	if !creating && projectionChanged && len(biz.ProjectLoadBalancerListeners(i.Listeners)) > 0 && prior.ConfigurationState == "waiting_for_backends" && lbProviderTargetVersion(prior) > 0 {
		if err = reincarnateLB(ctx, q, prior); err != nil {
			return biz.LoadBalancerResult{}, err
		}
	}
	opID := uuid.NewString()
	var row sqlcgen.NetworkLoadBalancer
	if creating {
		row, err = q.InsertLB(ctx, sqlcgen.InsertLBParams{TenantID: i.TenantID, LbID: newEgressID("lb"), ClusterID: pb.ClusterID, Namespace: pb.Namespace, VpcID: vpcID, SubnetID: subnetID, Exposure: i.Exposure, Flavor: i.Flavor, PublicEipID: eipID, PrivateIp: i.PrivateIP, Name: i.Name, Description: i.Description, CreatedAt: now, OperationID: &opID})
	} else {
		row, err = q.UpdateLBIntent(ctx, sqlcgen.UpdateLBIntentParams{TenantID: i.TenantID, LbID: i.ID, Version: i.ExpectedVersion, Name: i.Name, Description: i.Description, ProjectionChanged: projectionChanged, OperationID: &opID})
	}
	if err != nil {
		return biz.LoadBalancerResult{}, databaseFailure(err)
	}
	if projectionChanged {
		row, err = q.AdvanceLBProviderTarget(ctx, sqlcgen.AdvanceLBProviderTargetParams{TenantID: row.TenantID, LbID: row.LbID, Version: row.Version})
		if err != nil {
			return biz.LoadBalancerResult{}, databaseFailure(err)
		}
	} else if creating {
		row, err = q.MarkLBWaitingForBackends(ctx, sqlcgen.MarkLBWaitingForBackendsParams{TenantID: row.TenantID, LbID: row.LbID, Version: row.Version})
		if err != nil {
			return biz.LoadBalancerResult{}, databaseFailure(err)
		}
	}
	if creating {
		if eipID != "" {
			n, err := q.ClaimEIPForLB(ctx, sqlcgen.ClaimEIPForLBParams{TenantID: i.TenantID, ClusterID: pb.ClusterID, Namespace: pb.Namespace, LbID: &row.LbID, EipID: eipID, CreatedAt: now})
			if err != nil {
				return biz.LoadBalancerResult{}, databaseFailure(err)
			}
			if n != 1 {
				return biz.LoadBalancerResult{}, biz.Fail(biz.EIPInUse, "EIP is reserved by another target")
			}
		}
		if i.PrivateIP != "" {
			n, err := q.ReserveLBVIP(ctx, sqlcgen.ReserveLBVIPParams{TenantID: i.TenantID, ClusterID: pb.ClusterID, Namespace: pb.Namespace, LbID: row.LbID, VpcID: vpcID, SubnetID: subnetID, Address: i.PrivateIP})
			if err != nil {
				return biz.LoadBalancerResult{}, databaseFailure(err)
			}
			if n != 1 {
				return biz.LoadBalancerResult{}, biz.Fail(biz.VIPInUse, "VIP is reserved by another load balancer")
			}
		}
	}
	h := i.Health
	if err = q.InsertLBConfiguration(ctx, sqlcgen.InsertLBConfigurationParams{TenantID: i.TenantID, ClusterID: pb.ClusterID, Namespace: pb.Namespace, LbID: row.LbID, ConfigVersion: row.DesiredVersion, Name: i.Name, Description: i.Description, IntervalSeconds: int64(h.IntervalSeconds), TimeoutSeconds: int64(h.TimeoutSeconds), UnhealthyThreshold: int64(h.UnhealthyThreshold), HealthyThreshold: int64(h.HealthyThreshold), HealthCheckPort: int32(h.Port)}); err != nil {
		return biz.LoadBalancerResult{}, databaseFailure(err)
	}
	memberIDs := map[string]string{}
	for j, b := range i.Backends {
		memberID := b.ID
		if memberID == "" {
			memberID = uuid.NewString()
			c := identities[j]
			c.LbID, c.MemberID = row.LbID, memberID
			if err = q.InsertLBMember(ctx, c); err != nil {
				return biz.LoadBalancerResult{}, databaseFailure(err)
			}
			if err = insertLBComponent(ctx, q, row, "backend", &memberID); err != nil {
				return biz.LoadBalancerResult{}, err
			}
		}
		if err = q.InsertLBConfigurationMember(ctx, sqlcgen.InsertLBConfigurationMemberParams{TenantID: i.TenantID, ClusterID: pb.ClusterID, Namespace: pb.Namespace, LbID: row.LbID, ConfigVersion: row.DesiredVersion, MemberID: memberID, Weight: int32(b.Weight)}); err != nil {
			return biz.LoadBalancerResult{}, databaseFailure(err)
		}
		memberIDs[lbBackendKey(b)] = memberID
	}
	if err = saveLBListeners(ctx, q, row, i.Listeners, memberIDs); err != nil {
		return biz.LoadBalancerResult{}, err
	}
	for id := range needed {
		if err = q.ReserveLBSubnet(ctx, sqlcgen.ReserveLBSubnetParams{TenantID: i.TenantID, ClusterID: pb.ClusterID, Namespace: pb.Namespace, LbID: row.LbID, VpcID: vpcID, SubnetID: id}); err != nil {
			return biz.LoadBalancerResult{}, databaseFailure(err)
		}
	}
	if err = q.InsertOperation(ctx, sqlcgen.InsertOperationParams{TenantID: i.TenantID, LbID: row.LbID, OperationID: opID, Kind: i.Kind, CreatedAt: now}); err != nil {
		return biz.LoadBalancerResult{}, databaseFailure(err)
	}
	if creating {
		if err = q.InsertBinding(ctx, sqlcgen.InsertBindingParams{TenantID: i.TenantID, LbID: row.LbID, BindingID: uuid.NewString(), ClusterID: pb.ClusterID, Namespace: pb.Namespace, ProviderName: strings.Replace(row.LbID, "_", "-", 1)}); err != nil {
			return biz.LoadBalancerResult{}, databaseFailure(err)
		}
		if err = q.InsertReconciliation(ctx, sqlcgen.InsertReconciliationParams{TenantID: i.TenantID, LbID: row.LbID, NextRunAt: now}); err != nil {
			return biz.LoadBalancerResult{}, databaseFailure(err)
		}
	} else if projectionChanged {
		if err = affected(q.ScheduleDeletion(ctx, sqlcgen.ScheduleDeletionParams{TenantID: i.TenantID, ResourceID: row.LbID})); err != nil {
			return biz.LoadBalancerResult{}, err
		}
	}
	if !projectionChanged {
		if err = affected(q.CompleteLBDatabaseOperation(ctx, sqlcgen.CompleteLBDatabaseOperationParams{TenantID: row.TenantID, LbID: row.LbID, OperationID: opID})); err != nil {
			return biz.LoadBalancerResult{}, err
		}
		if creating {
			if err = affected(q.ParkLBReconciliation(ctx, sqlcgen.ParkLBReconciliationParams{TenantID: row.TenantID, LbID: row.LbID})); err != nil {
				return biz.LoadBalancerResult{}, err
			}
		}
	}
	result, err := lbResult(ctx, q, row)
	if err != nil {
		return result, err
	}
	snapshot, err := json.Marshal(result)
	if err != nil {
		return result, err
	}
	if err = q.InsertLBIdempotency(ctx, sqlcgen.InsertLBIdempotencyParams{TenantID: i.TenantID, OperationKind: i.Kind, IdempotencyKey: i.IdempotencyKey, Fingerprint: acceptedFingerprint, LbID: &row.LbID, OperationID: opID, Response: snapshot, CreatedAt: now}); err != nil {
		return result, databaseFailure(err)
	}
	if err = lbHistory(ctx, q, row, a, i.Kind+"_accepted", now); err != nil {
		return result, err
	}
	if err = tx.Commit(ctx); err != nil {
		return biz.LoadBalancerResult{}, databaseFailure(err)
	}
	return result, nil
}
func lastAddress(cidr netip.Prefix) netip.Addr {
	a := cidr.Addr().As4()
	v := uint32(a[0])<<24 | uint32(a[1])<<16 | uint32(a[2])<<8 | uint32(a[3])
	v |= ^uint32(0) >> cidr.Bits()
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}
func insertLBComponent(ctx context.Context, q *sqlcgen.Queries, row sqlcgen.NetworkLoadBalancer, kind string, member *string) error {
	return insertLBListenerComponent(ctx, q, row, kind, member, nil)
}
func lbHistory(ctx context.Context, q *sqlcgen.Queries, row sqlcgen.NetworkLoadBalancer, a biz.Attribution, event string, now time.Time) error {
	op, err := q.GetOperation(ctx, sqlcgen.GetOperationParams{TenantID: row.TenantID, OperationID: textValue(row.LastOperationID)})
	if err != nil {
		return databaseFailure(err)
	}
	return databaseFailure(q.InsertHistory(ctx, sqlcgen.InsertHistoryParams{TenantID: row.TenantID, LbID: row.LbID, HistoryID: uuid.NewString(), OperationID: row.LastOperationID, Event: event, ResourceState: row.State, OperationState: op.State, ActorRef: a.Actor, CallerRef: a.DirectCaller, CorrelationID: a.CorrelationID, CreatedAt: now}))
}
func (p *Postgres) DeleteLoadBalancer(ctx context.Context, tenant, id string, a biz.Attribution) (biz.LoadBalancerResult, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return biz.LoadBalancerResult{}, databaseFailure(err)
	}
	defer tx.Rollback(ctx)
	q := p.queries.WithTx(tx)
	row, err := q.GetLBInternal(ctx, sqlcgen.GetLBInternalParams{TenantID: tenant, LbID: id})
	if err != nil {
		return biz.LoadBalancerResult{}, databaseFailure(err)
	}
	if row.LastOperationID == nil {
		return biz.LoadBalancerResult{}, biz.Fail(biz.ResourceNotFound, "load balancer not found")
	}
	if _, err = q.LockVPC(ctx, sqlcgen.LockVPCParams{TenantID: tenant, VpcID: row.VpcID}); err != nil {
		return biz.LoadBalancerResult{}, databaseFailure(err)
	}
	refs, err := q.ListLBSubnetRefs(ctx, sqlcgen.ListLBSubnetRefsParams{TenantID: tenant, LbID: id})
	if err != nil {
		return biz.LoadBalancerResult{}, databaseFailure(err)
	}
	ids := []string{row.SubnetID}
	for _, ref := range refs {
		ids = append(ids, ref.SubnetID)
	}
	if _, err = lockLBSubnets(ctx, q, tenant, row.VpcID, ids); err != nil {
		return biz.LoadBalancerResult{}, err
	}
	if row.PublicEipID != nil {
		if _, err = q.LockEIP(ctx, sqlcgen.LockEIPParams{TenantID: tenant, EipID: *row.PublicEipID}); err != nil {
			return biz.LoadBalancerResult{}, databaseFailure(err)
		}
	}
	row, err = q.LockLB(ctx, sqlcgen.LockLBParams{TenantID: tenant, LbID: id})
	if err != nil {
		return biz.LoadBalancerResult{}, databaseFailure(err)
	}
	if row.State == "deleting" || row.State == "deleted" {
		return lbResult(ctx, q, row)
	}
	// Capture completed retirement while last_operation_id still identifies
	// the saved waiting operation. A queued delete is not that proof. Never-
	// dispatched products keep the independent guard and query below.
	retiredDeletion := false
	if row.ConfigurationState == "waiting_for_backends" && lbProviderTargetVersion(row) > 0 && row.AppliedVersion == lbProviderTargetVersion(row) {
		binding, bindingErr := q.GetBinding(ctx, sqlcgen.GetBindingParams{TenantID: tenant, ResourceID: id})
		if bindingErr != nil {
			return biz.LoadBalancerResult{}, databaseFailure(bindingErr)
		}
		if binding.ProviderUid != "" && binding.PendingAction == "" {
			retiredDeletion, err = q.CanParkLBWaitingWithoutProvider(ctx, sqlcgen.CanParkLBWaitingWithoutProviderParams{TenantID: tenant, LbID: id})
			if err != nil {
				return biz.LoadBalancerResult{}, databaseFailure(err)
			}
		}
	}
	if err = q.RetireLBMutation(ctx, sqlcgen.RetireLBMutationParams{TenantID: tenant, LbID: id, OperationID: *row.LastOperationID}); err != nil {
		return biz.LoadBalancerResult{}, databaseFailure(err)
	}
	now, err := q.DatabaseTime(ctx)
	if err != nil {
		return biz.LoadBalancerResult{}, databaseFailure(err)
	}
	opID := uuid.NewString()
	if err = q.InsertOperation(ctx, sqlcgen.InsertOperationParams{TenantID: tenant, LbID: id, OperationID: opID, Kind: "delete_load_balancer", CreatedAt: now}); err != nil {
		return biz.LoadBalancerResult{}, databaseFailure(err)
	}
	row, err = q.AdmitLBDeletion(ctx, sqlcgen.AdmitLBDeletionParams{TenantID: tenant, LbID: id, Version: row.Version, OperationID: &opID})
	if err != nil {
		return biz.LoadBalancerResult{}, databaseFailure(err)
	}
	deletionProof, err := q.CanDeleteLBWithoutProvider(ctx, sqlcgen.CanDeleteLBWithoutProviderParams{TenantID: tenant, LbID: id})
	if err != nil {
		return biz.LoadBalancerResult{}, databaseFailure(err)
	}
	if retiredDeletion || (deletionProof.Valid && deletionProof.Bool) {
		if retiredDeletion {
			row, err = q.CompleteLBRetiredDatabaseDeletion(ctx, sqlcgen.CompleteLBRetiredDatabaseDeletionParams{TenantID: tenant, LbID: id, Version: row.Version})
		} else {
			row, err = q.CompleteLBDatabaseDeletion(ctx, sqlcgen.CompleteLBDatabaseDeletionParams{TenantID: tenant, LbID: id, Version: row.Version})
		}
		if err != nil {
			return biz.LoadBalancerResult{}, databaseFailure(err)
		}
		if err = affected(q.CompleteLBDatabaseOperation(ctx, sqlcgen.CompleteLBDatabaseOperationParams{TenantID: tenant, LbID: id, OperationID: opID})); err != nil {
			return biz.LoadBalancerResult{}, err
		}
		if err = q.RetireLBUndispatchedComponents(ctx, sqlcgen.RetireLBUndispatchedComponentsParams{TenantID: tenant, LbID: id}); err != nil {
			return biz.LoadBalancerResult{}, databaseFailure(err)
		}
		if err = q.ReleaseLBClaim(ctx, sqlcgen.ReleaseLBClaimParams{TenantID: tenant, LbID: &id}); err != nil {
			return biz.LoadBalancerResult{}, databaseFailure(err)
		}
		if err = q.ReleaseLBVIP(ctx, sqlcgen.ReleaseLBVIPParams{TenantID: tenant, LbID: id}); err != nil {
			return biz.LoadBalancerResult{}, databaseFailure(err)
		}
		if err = q.ReleaseLBSubnetRefs(ctx, sqlcgen.ReleaseLBSubnetRefsParams{TenantID: tenant, LbID: id, Deleted: true, EntrySubnet: row.SubnetID, DesiredVersion: row.DesiredVersion}); err != nil {
			return biz.LoadBalancerResult{}, databaseFailure(err)
		}
		if err = affected(q.ParkLBReconciliation(ctx, sqlcgen.ParkLBReconciliationParams{TenantID: tenant, LbID: id})); err != nil {
			return biz.LoadBalancerResult{}, err
		}
	} else if err = affected(q.ScheduleDeletion(ctx, sqlcgen.ScheduleDeletionParams{TenantID: tenant, ResourceID: id})); err != nil {
		return biz.LoadBalancerResult{}, err
	}
	if err = lbHistory(ctx, q, row, a, "delete_load_balancer_accepted", now); err != nil {
		return biz.LoadBalancerResult{}, err
	}
	result, err := lbResult(ctx, q, row)
	if err != nil {
		return result, err
	}
	if err = tx.Commit(ctx); err != nil {
		return biz.LoadBalancerResult{}, databaseFailure(err)
	}
	return result, nil
}

// The parent/LB locks and the subsequent admission version update fence old
// work. Only a confirmed, immutable retirement permits identity replacement.
func reincarnateLB(ctx context.Context, q *sqlcgen.Queries, row sqlcgen.NetworkLoadBalancer) error {
	binding, err := q.GetBinding(ctx, sqlcgen.GetBindingParams{TenantID: row.TenantID, ResourceID: row.LbID})
	if err != nil {
		return databaseFailure(err)
	}
	retired, err := q.GetLBRetiredBinding(ctx, sqlcgen.GetLBRetiredBindingParams{TenantID: row.TenantID, LbID: row.LbID, BindingID: binding.BindingID})
	if errors.Is(err, pgx.ErrNoRows) {
		return biz.Fail(biz.ResourceBusy, "previous Gateway cleanup has not completed")
	}
	if err != nil {
		return databaseFailure(err)
	}
	if retired.ProviderUid != binding.ProviderUid || retired.ProviderName != binding.ProviderName || retired.CleanupTargetVersion != row.AppliedVersion || retired.CleanupTargetVersion != lbProviderTargetVersion(row) || binding.PendingAction != "" {
		return biz.Fail(biz.ProviderOwnership, "previous Gateway cleanup record does not match current binding")
	}
	newBinding := uuid.NewString()
	return affected(q.ReincarnateLBBinding(ctx, sqlcgen.ReincarnateLBBindingParams{TenantID: row.TenantID, LbID: row.LbID, OldBindingID: binding.BindingID, OldProviderUid: binding.ProviderUid, NewBindingID: newBinding, NewProviderName: "lb-" + strings.ReplaceAll(newBinding, "-", "")}))
}
