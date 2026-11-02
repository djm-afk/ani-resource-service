package data

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
)

func newEgressID(prefix string) string {
	return prefix + "_" + strings.ReplaceAll(uuid.NewString(), "-", "")
}
func eipResource(r sqlcgen.NetworkEip) biz.EIP {
	return biz.EIP{EgressMetadata: biz.EgressMetadata{ID: r.EipID, Name: r.Name, Description: r.Description, State: biz.ResourceState(r.State), Reason: biz.Reason(r.Reason), Version: r.Version, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, ObservedAt: r.ObservedAt, LastOperationID: r.LastOperationID}, TenantID: r.TenantID, Address: r.Address, BindingState: "unbound", Scope: r.Scope, ManagedBy: r.ManagedBy}
}
func snatResource(r sqlcgen.NetworkSnatBinding, address string) biz.VPCSnatBinding {
	v := biz.VPCSnatBinding{EgressMetadata: biz.EgressMetadata{ID: r.SnatID, Name: r.Name, Description: r.Description, State: biz.ResourceState(r.State), Reason: biz.Reason(r.Reason), Version: r.Version, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, ObservedAt: r.ObservedAt, LastOperationID: r.LastOperationID}, TenantID: r.TenantID, VPCID: r.VpcID, EIPID: r.EipID, EIPAddress: address, DesiredEnabled: r.DesiredEnabled, Purpose: r.Purpose}
	if r.AppliedEnabled.Valid {
		value := r.AppliedEnabled.Bool
		v.AppliedEnabled = &value
	}
	return v
}
func freshResource(state string, observed *time.Time, now time.Time, limit time.Duration) bool {
	return state == string(biz.Available) && observed != nil && !observed.After(now) && now.Sub(*observed) <= limit
}

func egressReplay(ctx context.Context, q *sqlcgen.Queries, i biz.EgressIntent, dst any) (bool, error) {
	if err := q.LockEgressKey(ctx, sqlcgen.LockEgressKeyParams{TenantID: i.TenantID, OperationKind: i.Kind, IdempotencyKey: i.IdempotencyKey}); err != nil {
		return false, databaseFailure(err)
	}
	prev, err := q.GetEgressIdempotency(ctx, sqlcgen.GetEgressIdempotencyParams{TenantID: i.TenantID, OperationKind: i.Kind, IdempotencyKey: i.IdempotencyKey})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, databaseFailure(err)
	}
	if prev.FingerprintVersion != 1 || prev.Fingerprint != i.Fingerprint() {
		return false, biz.Fail(biz.IdempotencyConflict, "key already accepted another intent")
	}
	if err = json.Unmarshal(prev.Response, dst); err != nil {
		return false, databaseFailure(err)
	}
	valid := false
	switch v := dst.(type) {
	case *biz.EIP:
		valid = v.TenantID == i.TenantID && v.ID == textValue(prev.EipID) && v.LastOperationID == prev.OperationID
	case *biz.VPCSnatBinding:
		valid = v.TenantID == i.TenantID && v.ID == textValue(prev.SnatID) && v.LastOperationID == prev.OperationID
	}
	if !valid {
		return false, databaseFailure(fmt.Errorf("invalid egress acceptance snapshot"))
	}
	return true, nil
}
func insertEgressAcceptance(ctx context.Context, q *sqlcgen.Queries, i biz.EgressIntent, a biz.Attribution, v any, meta biz.EgressMetadata, kind, cluster, namespace string, now time.Time, newResource bool) error {
	eip, snat := "", ""
	if kind == "eip" {
		eip = meta.ID
	} else {
		snat = meta.ID
	}
	if err := q.InsertOperation(ctx, sqlcgen.InsertOperationParams{TenantID: i.TenantID, EipID: eip, SnatID: snat, OperationID: meta.LastOperationID, Kind: i.Kind, CreatedAt: now}); err != nil {
		return databaseFailure(err)
	}
	if newResource {
		if err := q.InsertReconciliation(ctx, sqlcgen.InsertReconciliationParams{TenantID: i.TenantID, EipID: eip, SnatID: snat, NextRunAt: now}); err != nil {
			return databaseFailure(err)
		}
		if err := q.InsertBinding(ctx, sqlcgen.InsertBindingParams{TenantID: i.TenantID, EipID: eip, SnatID: snat, BindingID: uuid.NewString(), ClusterID: cluster, Namespace: namespace, ProviderName: strings.Replace(meta.ID, "_", "-", 1)}); err != nil {
			return databaseFailure(err)
		}
	} else {
		if err := affected(q.ScheduleDeletion(ctx, sqlcgen.ScheduleDeletionParams{TenantID: i.TenantID, ResourceID: meta.ID})); err != nil {
			return err
		}
	}
	snapshot, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err = q.InsertEgressIdempotency(ctx, sqlcgen.InsertEgressIdempotencyParams{TenantID: i.TenantID, OperationKind: i.Kind, IdempotencyKey: i.IdempotencyKey, Fingerprint: i.Fingerprint(), EipID: eip, SnatID: snat, OperationID: meta.LastOperationID, Response: snapshot, CreatedAt: now}); err != nil {
		return databaseFailure(err)
	}
	return databaseFailure(q.InsertHistory(ctx, sqlcgen.InsertHistoryParams{TenantID: i.TenantID, HistoryID: uuid.NewString(), EipID: eip, SnatID: snat, OperationID: &meta.LastOperationID, Event: i.Kind + "_accepted", ResourceState: string(meta.State), OperationState: string(biz.Queued), ActorRef: a.Actor, CallerRef: a.DirectCaller, CorrelationID: a.CorrelationID, CreatedAt: now}))
}
func (p *Postgres) AcceptEIP(ctx context.Context, i biz.EgressIntent, a biz.Attribution, freshness time.Duration) (biz.EIP, error) {
	identity, err := p.ResolveTenantNamespace(ctx, i.TenantID)
	if err != nil {
		return biz.EIP{}, err
	}

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return biz.EIP{}, databaseFailure(err)
	}
	defer tx.Rollback(ctx)
	q := p.queries.WithTx(tx)
	var replay biz.EIP
	if found, err := egressReplay(ctx, q, i, &replay); found || err != nil {
		return replay, err
	}
	if len(i.SecurityGroupIDs) > 0 {
		if err = sgTenantLock(ctx, q, i.TenantID); err != nil {
			return biz.EIP{}, err
		}
	}
	// Pool selection, default switching, allocation close and retirement share the
	// cluster lock. Selection is persisted before any Kubernetes request exists.
	if err = q.LockPlatformCluster(ctx, sqlcgen.LockPlatformClusterParams{ClusterID: p.placement.ClusterID}); err != nil {
		return biz.EIP{}, databaseFailure(err)
	}
	def, err := q.GetDefaultPublicPool(ctx, sqlcgen.GetDefaultPublicPoolParams{ClusterID: p.placement.ClusterID})
	if errors.Is(err, pgx.ErrNoRows) {
		return biz.EIP{}, biz.Fail(biz.PublicEgressNotReady, "default public pool is not ready")
	}
	if err != nil {
		return biz.EIP{}, databaseFailure(err)
	}
	pool, err := q.LockPublicPool(ctx, sqlcgen.LockPublicPoolParams{ClusterID: p.placement.ClusterID, ResourceID: def.PoolID})
	if err != nil {
		return biz.EIP{}, databaseFailure(err)
	}
	now, err := q.DatabaseTime(ctx)
	if err != nil {
		return biz.EIP{}, databaseFailure(err)
	}
	if err = p.poolReady(ctx, q, pool, now, freshness, true); err != nil {
		return biz.EIP{}, err
	}
	if err = p.acceptTenantNamespace(ctx, q, identity); err != nil {
		return biz.EIP{}, err
	}
	namespace, err := q.GetTenantNamespace(ctx, sqlcgen.GetTenantNamespaceParams{TenantID: i.TenantID, ClusterID: p.placement.ClusterID})
	if err != nil {
		return biz.EIP{}, databaseFailure(err)
	}
	row, err := q.InsertEIP(ctx, sqlcgen.InsertEIPParams{TenantID: i.TenantID, EipID: newEgressID("eip"), ClusterID: p.placement.ClusterID, Namespace: namespace, Name: i.Name, Description: i.Description, PoolID: pool.ResourceID, PoolRevision: pool.ConfigRevision, CreatedAt: now, LastOperationID: uuid.NewString()})
	if err != nil {
		return biz.EIP{}, databaseFailure(err)
	}
	if len(i.SecurityGroupIDs) > 0 {
		if _, err = resolveSecurityGroups(ctx, q, i.TenantID, row.ClusterID, row.Namespace, i.SecurityGroupIDs); err != nil {
			return biz.EIP{}, err
		}
		op := sgNewOperation(i.TenantID, row.EipID, "set_eip_security_groups", "eip", now)
		binding := biz.SecurityGroupBinding{TenantID: i.TenantID, TargetType: "eip", TargetID: row.EipID, Namespace: row.Namespace, Version: 1, State: "applying", SecurityGroupIDs: slices.Clone(i.SecurityGroupIDs), LastOperationID: op.ID}
		if err = sgSaveTarget(ctx, q, binding); err != nil {
			return biz.EIP{}, databaseFailure(err)
		}
		for _, id := range i.SecurityGroupIDs {
			if err = q.ReserveSecurityGroupReference(ctx, sqlcgen.ReserveSecurityGroupReferenceParams{TenantID: i.TenantID, TargetType: "eip", TargetID: row.EipID, SecurityGroupID: id}); err != nil {
				return biz.EIP{}, databaseFailure(err)
			}
		}
		intent := biz.SecurityGroupIntent{TenantID: i.TenantID, Action: "create_eip_security_groups", ID: row.EipID, SecurityGroupIDs: slices.Clone(i.SecurityGroupIDs)}
		if err = q.InsertSecurityGroupOperation(ctx, sqlcgen.InsertSecurityGroupOperationParams{TenantID: i.TenantID, OperationID: op.ID, Payload: sgJSON(op), ActorRef: a.Actor, CallerRef: a.DirectCaller, Intent: sgJSON(intent)}); err != nil {
			return biz.EIP{}, databaseFailure(err)
		}
		if err = q.ScheduleSecurityGroup(ctx, sqlcgen.ScheduleSecurityGroupParams{TenantID: i.TenantID, TargetType: "eip", TargetID: row.EipID, RequestedVersion: binding.Version}); err != nil {
			return biz.EIP{}, databaseFailure(err)
		}
	}
	v := eipResource(row)
	v.ObservationStale = true
	if err = insertEgressAcceptance(ctx, q, i, a, v, v.EgressMetadata, "eip", row.ClusterID, row.Namespace, now, true); err != nil {
		return biz.EIP{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return biz.EIP{}, databaseFailure(err)
	}
	return v, nil
}
func eipBindingTarget(kind, id, state, vpcID string) *biz.EIPBindingTarget {
	if kind == "" {
		return nil
	}
	return &biz.EIPBindingTarget{Kind: kind, ID: id, State: state, VPCID: vpcID}
}

// Internal workers can load all address purposes. Tenant entry points must
// enforce the Public boundary before accepting or returning a resource.
func requireTenantPublicEIP(row sqlcgen.NetworkEip) error {
	if row.Scope != "public" || row.ManagedBy != "tenant" {
		return biz.Fail(biz.ResourceNotFound, "EIP not found")
	}
	return nil
}
func requirePublicSnat(row sqlcgen.NetworkSnatBinding) error {
	if row.Purpose != "public" {
		return biz.Fail(biz.ResourceNotFound, "SNAT binding not found")
	}
	return nil
}

func (p *Postgres) GetEIP(ctx context.Context, tenant, id string) (biz.EIP, error) {
	r, err := p.queries.GetEIP(ctx, sqlcgen.GetEIPParams{TenantID: tenant, EipID: id})
	v := eipResource(r.NetworkEip)
	v.BindingID = r.BindingID
	v.BindingState = r.BindingState
	v.BindingTarget = eipBindingTarget(r.BindingTargetKind, r.BindingTargetID, r.BindingState, r.BindingVpcID)
	return v, databaseFailure(err)
}
func (p *Postgres) ListEIPs(ctx context.Context, tenant string, f biz.VPCFilter) ([]biz.EIP, int64, error) {
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, 0, databaseFailure(err)
	}
	defer tx.Rollback(ctx)
	q := p.queries.WithTx(tx)
	total, err := q.CountEIPs(ctx, sqlcgen.CountEIPsParams{TenantID: tenant, NameFilter: f.Name, StateFilter: f.State})
	if err != nil {
		return nil, 0, databaseFailure(err)
	}
	rows, err := q.ListEIPs(ctx, sqlcgen.ListEIPsParams{TenantID: tenant, NameFilter: f.Name, StateFilter: f.State, AfterID: f.AfterID, AfterCreatedAt: f.AfterCreatedAt, MaxResults: f.Limit})
	if err != nil {
		return nil, 0, databaseFailure(err)
	}
	values := make([]biz.EIP, 0, len(rows))
	for _, r := range rows {
		v := eipResource(r.NetworkEip)
		v.BindingID = r.BindingID
		v.BindingState = r.BindingState
		v.BindingTarget = eipBindingTarget(r.BindingTargetKind, r.BindingTargetID, r.BindingState, r.BindingVpcID)
		values = append(values, v)
	}
	return values, total, nil
}
func (p *Postgres) GetSnat(ctx context.Context, tenant, id string, byVPC bool) (biz.VPCSnatBinding, error) {
	r, err := p.queries.GetSnat(ctx, sqlcgen.GetSnatParams{TenantID: tenant, ID: id, ByVpc: byVPC})
	return snatResource(r.NetworkSnatBinding, r.Address), databaseFailure(err)
}
func (p *Postgres) AcceptSnat(ctx context.Context, i biz.EgressIntent, a biz.Attribution, freshness time.Duration) (biz.VPCSnatBinding, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return biz.VPCSnatBinding{}, databaseFailure(err)
	}
	defer tx.Rollback(ctx)
	q := p.queries.WithTx(tx)
	var replay biz.VPCSnatBinding
	if found, err := egressReplay(ctx, q, i, &replay); found || err != nil {
		return replay, err
	}
	var prior sqlcgen.NetworkSnatBinding
	if i.Kind == "set_snat_enabled" {
		prior, err = q.GetSnatInternal(ctx, sqlcgen.GetSnatInternalParams{TenantID: i.TenantID, SnatID: i.ID})
		if err != nil {
			return biz.VPCSnatBinding{}, databaseFailure(err)
		}
		if err = requirePublicSnat(prior); err != nil {
			return biz.VPCSnatBinding{}, err
		}
	}
	vpcID, eipID := i.VPCID, i.EIPID
	if i.Kind == "set_snat_enabled" {
		vpcID, eipID = prior.VpcID, prior.EipID
	}
	parent, err := q.LockVPC(ctx, sqlcgen.LockVPCParams{TenantID: i.TenantID, VpcID: vpcID})
	if err != nil {
		return biz.VPCSnatBinding{}, databaseFailure(err)
	}
	eip, err := q.LockEIP(ctx, sqlcgen.LockEIPParams{TenantID: i.TenantID, EipID: eipID})
	if err != nil {
		return biz.VPCSnatBinding{}, databaseFailure(err)
	}
	if err = requireTenantPublicEIP(eip); err != nil {
		return biz.VPCSnatBinding{}, err
	}
	if i.Kind == "set_snat_enabled" {
		prior, err = q.LockSnat(ctx, sqlcgen.LockSnatParams{TenantID: i.TenantID, SnatID: i.ID})
		if err != nil {
			return biz.VPCSnatBinding{}, databaseFailure(err)
		}
		if prior.VpcID != vpcID || prior.EipID != eipID {
			return biz.VPCSnatBinding{}, biz.Fail(biz.ResourceBusy, "binding parents changed")
		}
		if prior.State == "deleted" || prior.State == "deleting" {
			return biz.VPCSnatBinding{}, biz.Fail(biz.ResourceBusy, "binding is being removed")
		}
		if prior.Version != i.ExpectedVersion {
			return biz.VPCSnatBinding{}, biz.Fail(biz.ResourceBusy, "binding version changed")
		}
		if err = completedOperation(ctx, q, i.TenantID, prior.LastOperationID); err != nil {
			return biz.VPCSnatBinding{}, err
		}
	} else {
		occupied, err := q.BlockingSnatForVPC(ctx, sqlcgen.BlockingSnatForVPCParams{TenantID: i.TenantID, VpcID: vpcID})
		if err != nil {
			return biz.VPCSnatBinding{}, databaseFailure(err)
		}
		if occupied > 0 {
			return biz.VPCSnatBinding{}, biz.Fail(biz.VPCSnatExists, "VPC already has a binding")
		}
		occupied, err = q.BlockingSnatForEIP(ctx, sqlcgen.BlockingSnatForEIPParams{TenantID: i.TenantID, EipID: eipID})
		if err != nil {
			return biz.VPCSnatBinding{}, databaseFailure(err)
		}
		if occupied > 0 {
			return biz.VPCSnatBinding{}, biz.Fail(biz.EIPInUse, "EIP is reserved by a binding")
		}
	}
	now, err := q.DatabaseTime(ctx)
	if err != nil {
		return biz.VPCSnatBinding{}, databaseFailure(err)
	}
	var parentProof sqlcgen.GetVPCProviderReadinessRow
	// Disabling an existing binding remains possible when an exit is degraded.
	// Creating/enabling requires applied, fresh parents and exit configuration.
	if i.Kind == "bind_snat" || i.Enabled {
		// Public egress depends on this VPC's own fenced Provider observation,
		// never the aggregate lifecycle or optional Intranet EIP/SNAT readiness.
		proof, proofErr := q.GetVPCProviderReadiness(ctx, sqlcgen.GetVPCProviderReadinessParams{TenantID: i.TenantID, VpcID: &vpcID})
		if proofErr != nil {
			return biz.VPCSnatBinding{}, databaseFailure(proofErr)
		}
		parentProof = proof
		if parent.State == "deleting" || parent.State == "deleted" || parent.State == "failed" || !proof.ProviderReady || !freshTime(proof.ProviderObservedAt, now, freshness) || (proof.ProviderGeneration <= 0 || proof.ProviderObservedGeneration != proof.ProviderGeneration) {
			return biz.VPCSnatBinding{}, biz.Fail(biz.PublicEgressNotReady, "VPC Provider is not fresh and ready")
		}
		if !freshResource(eip.State, eip.ObservedAt, now, freshness) {
			return biz.VPCSnatBinding{}, biz.Fail(biz.PublicEgressNotReady, "EIP is not fresh and ready")
		}
		if err = completedOperation(ctx, q, i.TenantID, eip.LastOperationID); err != nil {
			return biz.VPCSnatBinding{}, err
		}
		pool, err := q.GetPublicPool(ctx, sqlcgen.GetPublicPoolParams{ClusterID: eip.ClusterID, ResourceID: eip.PoolID})
		if err != nil {
			return biz.VPCSnatBinding{}, databaseFailure(err)
		}
		if pool.ConfigRevision != eip.PoolRevision {
			return biz.VPCSnatBinding{}, biz.Fail(biz.PublicEgressNotReady, "fixed exit configuration changed")
		}
		if err = p.poolReady(ctx, q, pool, now, freshness, false); err != nil {
			return biz.VPCSnatBinding{}, err
		}
	}
	parentBinding, err := q.GetBinding(ctx, sqlcgen.GetBindingParams{TenantID: i.TenantID, ResourceID: vpcID})
	if err != nil {
		return biz.VPCSnatBinding{}, databaseFailure(err)
	}
	eipBinding, err := q.GetBinding(ctx, sqlcgen.GetBindingParams{TenantID: i.TenantID, ResourceID: eipID})
	if err != nil {
		return biz.VPCSnatBinding{}, databaseFailure(err)
	}
	if i.Kind == "bind_snat" || i.Enabled {
		if parentBinding.PendingAction != "" || eipBinding.PendingAction != "" {
			return biz.VPCSnatBinding{}, biz.Fail(biz.ResourceBusy, "network parent has an unresolved Provider mutation")
		}
		if parentProof.ProviderObservedUid != parentBinding.ProviderUid {
			return biz.VPCSnatBinding{}, biz.Fail(biz.ProviderOwnership, "VPC Provider observation identity changed")
		}
	}
	if eip.ClusterID != p.placement.ClusterID || parentBinding.ClusterID != eip.ClusterID || parentBinding.Namespace != eip.Namespace || eipBinding.Namespace != eip.Namespace || eipBinding.ClusterID != eip.ClusterID || parentBinding.ProviderUid == "" || eipBinding.ProviderUid == "" {
		return biz.VPCSnatBinding{}, biz.Fail(biz.ProviderOwnership, "resource placement or identity does not match")
	}
	opID := uuid.NewString()
	var row sqlcgen.NetworkSnatBinding
	if i.Kind == "set_snat_enabled" {
		row, err = q.SetSnatIntent(ctx, sqlcgen.SetSnatIntentParams{TenantID: i.TenantID, SnatID: i.ID, Enabled: i.Enabled, Version: i.ExpectedVersion, OperationID: opID})
	} else {
		row, err = q.InsertSnat(ctx, sqlcgen.InsertSnatParams{TenantID: i.TenantID, SnatID: newEgressID("snat"), ClusterID: eip.ClusterID, Namespace: eip.Namespace, VpcID: vpcID, EipID: eipID, CreatedAt: now, LastOperationID: opID})
	}
	if err != nil {
		return biz.VPCSnatBinding{}, databaseFailure(err)
	}
	if i.Kind == "bind_snat" {
		claimed, err := q.ClaimEIPForSnat(ctx, sqlcgen.ClaimEIPForSnatParams{TenantID: i.TenantID, EipID: eipID, ClusterID: eip.ClusterID, Namespace: eip.Namespace, SnatID: row.SnatID, CreatedAt: now})
		if err != nil {
			return biz.VPCSnatBinding{}, databaseFailure(err)
		}
		if claimed != 1 {
			return biz.VPCSnatBinding{}, biz.Fail(biz.EIPInUse, "EIP is reserved by another target")
		}
	}
	v := snatResource(row, eip.Address)
	v.ObservationStale = true
	if err = insertEgressAcceptance(ctx, q, i, a, v, v.EgressMetadata, "snat", row.ClusterID, row.Namespace, now, i.Kind == "bind_snat"); err != nil {
		return biz.VPCSnatBinding{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return biz.VPCSnatBinding{}, databaseFailure(err)
	}
	return v, nil
}
func completedOperation(ctx context.Context, q *sqlcgen.Queries, tenant, id string) error {
	op, err := q.GetOperation(ctx, sqlcgen.GetOperationParams{TenantID: tenant, OperationID: id})
	if err != nil {
		return databaseFailure(err)
	}
	if op.CompletedAt == nil {
		return biz.Fail(biz.ResourceBusy, "resource has an unfinished operation")
	}
	return nil
}
func deletionRecords(ctx context.Context, q *sqlcgen.Queries, tenant, id, kind, opID string, a biz.Attribution, now time.Time) error {
	eip, snat := "", ""
	if kind == "eip" {
		eip = id
	} else {
		snat = id
	}
	if err := q.InsertOperation(ctx, sqlcgen.InsertOperationParams{TenantID: tenant, EipID: eip, SnatID: snat, OperationID: opID, Kind: "delete_" + kind, CreatedAt: now}); err != nil {
		return databaseFailure(err)
	}
	if err := affected(q.ScheduleDeletion(ctx, sqlcgen.ScheduleDeletionParams{TenantID: tenant, ResourceID: id})); err != nil {
		return err
	}
	return databaseFailure(q.InsertHistory(ctx, sqlcgen.InsertHistoryParams{TenantID: tenant, HistoryID: uuid.NewString(), EipID: eip, SnatID: snat, OperationID: &opID, Event: "delete_accepted", ResourceState: string(biz.Deleting), OperationState: string(biz.Queued), ActorRef: a.Actor, CallerRef: a.DirectCaller, CorrelationID: a.CorrelationID, CreatedAt: now}))
}
func (p *Postgres) DeleteEIP(ctx context.Context, tenant, id string, a biz.Attribution) (biz.EIP, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return biz.EIP{}, databaseFailure(err)
	}
	defer tx.Rollback(ctx)
	q := p.queries.WithTx(tx)
	row, err := q.LockEIP(ctx, sqlcgen.LockEIPParams{TenantID: tenant, EipID: id})
	if err != nil {
		return biz.EIP{}, databaseFailure(err)
	}
	if err = requireTenantPublicEIP(row); err != nil {
		return biz.EIP{}, err
	}
	if row.State == "deleting" || row.State == "deleted" {
		return eipResource(row), nil
	}
	count, err := q.BlockingSnatForEIP(ctx, sqlcgen.BlockingSnatForEIPParams{TenantID: tenant, EipID: id})
	if err != nil {
		return biz.EIP{}, databaseFailure(err)
	}
	if count > 0 {
		return biz.EIP{}, biz.Fail(biz.EIPInUse, "binding must be removed before releasing the EIP")
	}
	if err = completedOperation(ctx, q, tenant, row.LastOperationID); err != nil {
		return biz.EIP{}, err
	}
	now, err := q.DatabaseTime(ctx)
	if err != nil {
		return biz.EIP{}, databaseFailure(err)
	}
	op := uuid.NewString()
	row, err = q.AdmitEIPDeletion(ctx, sqlcgen.AdmitEIPDeletionParams{TenantID: tenant, EipID: id, Version: row.Version, OperationID: op})
	if err != nil {
		return biz.EIP{}, databaseFailure(err)
	}
	if err = deletionRecords(ctx, q, tenant, id, "eip", op, a, now); err != nil {
		return biz.EIP{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return biz.EIP{}, databaseFailure(err)
	}
	return eipResource(row), nil
}
func (p *Postgres) DeleteSnat(ctx context.Context, tenant, id string, a biz.Attribution) (biz.VPCSnatBinding, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return biz.VPCSnatBinding{}, databaseFailure(err)
	}
	defer tx.Rollback(ctx)
	q := p.queries.WithTx(tx)
	ref, err := q.GetSnatInternal(ctx, sqlcgen.GetSnatInternalParams{TenantID: tenant, SnatID: id})
	if err != nil {
		return biz.VPCSnatBinding{}, databaseFailure(err)
	}
	if err = requirePublicSnat(ref); err != nil {
		return biz.VPCSnatBinding{}, err
	}
	if _, err = q.LockVPC(ctx, sqlcgen.LockVPCParams{TenantID: tenant, VpcID: ref.VpcID}); err != nil {
		return biz.VPCSnatBinding{}, databaseFailure(err)
	}
	eip, err := q.LockEIP(ctx, sqlcgen.LockEIPParams{TenantID: tenant, EipID: ref.EipID})
	if err != nil {
		return biz.VPCSnatBinding{}, databaseFailure(err)
	}
	if err = requireTenantPublicEIP(eip); err != nil {
		return biz.VPCSnatBinding{}, err
	}
	row, err := q.LockSnat(ctx, sqlcgen.LockSnatParams{TenantID: tenant, SnatID: id})
	if err != nil {
		return biz.VPCSnatBinding{}, databaseFailure(err)
	}
	if row.State == "deleting" || row.State == "deleted" {
		return snatResource(row, eip.Address), nil
	}
	if row.VpcID != ref.VpcID || row.EipID != ref.EipID {
		return biz.VPCSnatBinding{}, biz.Fail(biz.ResourceBusy, "binding parents changed")
	}
	if err = completedOperation(ctx, q, tenant, row.LastOperationID); err != nil {
		return biz.VPCSnatBinding{}, err
	}
	now, err := q.DatabaseTime(ctx)
	if err != nil {
		return biz.VPCSnatBinding{}, databaseFailure(err)
	}
	op := uuid.NewString()
	row, err = q.AdmitSnatDeletion(ctx, sqlcgen.AdmitSnatDeletionParams{TenantID: tenant, SnatID: id, Version: row.Version, OperationID: op})
	if err != nil {
		return biz.VPCSnatBinding{}, databaseFailure(err)
	}
	if err = deletionRecords(ctx, q, tenant, id, "snat", op, a, now); err != nil {
		return biz.VPCSnatBinding{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return biz.VPCSnatBinding{}, databaseFailure(err)
	}
	return snatResource(row, eip.Address), nil
}

func publicEgress(payload []byte) (*biz.VPCPublicEgress, error) {
	if len(payload) == 0 || string(payload) == "null" {
		return nil, nil
	}
	var value biz.VPCPublicEgress
	if err := json.Unmarshal(payload, &value); err != nil {
		return nil, err
	}
	return &value, nil
}
