package data

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
)

func vnicRow(r sqlcgen.NetworkVnic) biz.VNic {
	return biz.VNic{ID: r.VnicID, TenantID: r.TenantID, Name: r.Name, Description: r.Description, VPCID: r.VpcID, SubnetID: r.SubnetID, State: biz.ResourceState(r.State), Reason: biz.Reason(r.Reason), Version: r.Version, MACAddress: r.MacAddress, PrimaryIP: r.PrimaryIp, RequestedIP: r.RequestedIp, RequestedMAC: r.RequestedMac, IPs: r.Ips, SecurityGroupIDs: r.SecurityGroupIds, ObservedAt: r.ObservedAt, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, LastOperationID: r.LastOperationID, Bindings: []biz.VNicBinding{}}
}
func vnicWork(r sqlcgen.NetworkVnic) biz.ResourceWork {
	return biz.ResourceWork{ID: r.VnicID, TenantID: r.TenantID, Kind: "vnic", VPCID: r.VpcID, SubnetID: r.SubnetID, State: biz.ResourceState(r.State), Reason: biz.Reason(r.Reason), Version: r.Version, UpdatedAt: r.UpdatedAt, ObservedAt: r.ObservedAt, LastOperationID: r.LastOperationID, RequestedIP: r.RequestedIp, RequestedMAC: r.RequestedMac}
}
func vnicReplay(ctx context.Context, tx interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, r biz.CreateVNic) (biz.VNic, bool, error) {
	var fp string
	var b []byte
	err := tx.QueryRow(ctx, `SELECT fingerprint,response FROM network_idempotency WHERE tenant_id=$1 AND operation_kind='create_vnic' AND idempotency_key=$2`, r.TenantID, r.IdempotencyKey).Scan(&fp, &b)
	if errors.Is(err, pgx.ErrNoRows) {
		return biz.VNic{}, false, nil
	}
	if err != nil {
		return biz.VNic{}, false, databaseFailure(err)
	}
	if fp != r.Fingerprint() {
		return biz.VNic{}, true, biz.Fail(biz.IdempotencyConflict, "key accepted a different VNIC intent")
	}
	var v biz.VNic
	err = json.Unmarshal(b, &v)
	return v, true, databaseFailure(err)
}
func (p *Postgres) AcceptVNic(ctx context.Context, r biz.CreateVNic) (biz.VNic, error) {
	if v, found, e := vnicReplay(ctx, p.pool, r); found || e != nil {
		return v, e
	}
	// Resolve namespace through the same trusted resolver as ordinary network creation.
	identity, e := p.ResolveTenantNamespace(ctx, r.TenantID)
	if e != nil {
		return biz.VNic{}, e
	}
	initial, e := p.queries.GetSubnet(ctx, sqlcgen.GetSubnetParams{TenantID: r.TenantID, SubnetID: r.SubnetID})
	if e != nil {
		return biz.VNic{}, databaseFailure(e)
	}
	tx, e := p.pool.Begin(ctx)
	if e != nil {
		return biz.VNic{}, databaseFailure(e)
	}
	defer tx.Rollback(ctx)
	q := p.queries.WithTx(tx)
	ctx = context.WithValue(ctx, admissionQueriesKey{}, q)
	parent, e := q.LockVPC(ctx, sqlcgen.LockVPCParams{TenantID: r.TenantID, VpcID: initial.VpcID})
	if e != nil {
		return biz.VNic{}, databaseFailure(e)
	}
	if _, e = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,28))`, r.TenantID+":"+r.IdempotencyKey); e != nil {
		return biz.VNic{}, databaseFailure(e)
	}
	if v, found, e := vnicReplay(ctx, tx, r); found || e != nil {
		return v, e
	}
	subnet, e := q.LockSubnet(ctx, sqlcgen.LockSubnetParams{TenantID: r.TenantID, SubnetID: r.SubnetID})
	if e != nil {
		return biz.VNic{}, databaseFailure(e)
	}
	if !networkManagementOpen(parent.State, parent.Reason) || !networkManagementOpen(subnet.State, subnet.Reason) {
		return biz.VNic{}, biz.Fail(biz.ParentNotReady, "VPC or Subnet management is closed")
	}
	if e = biz.ValidateVNicRequestedIP(r.RequestedIP, subnet.Cidr, subnet.Gateway); e != nil {
		return biz.VNic{}, e
	}
	if e = p.acceptTenantNamespace(ctx, q, identity); e != nil {
		return biz.VNic{}, e
	}
	if p.networkProvider == nil {
		return biz.VNic{}, biz.Fail(biz.ProviderUnavailable, "network provider unavailable")
	}
	// The accepted Subnet identity must still be ready at the allocation boundary.
	sb, e := q.GetBinding(ctx, sqlcgen.GetBindingParams{TenantID: r.TenantID, ResourceID: r.SubnetID})
	if e != nil {
		return biz.VNic{}, databaseFailure(e)
	}
	if sb.ProviderUid == "" || sb.Namespace != identity.Name || sb.ClusterID != identity.ClusterID {
		return biz.VNic{}, biz.Fail(biz.ParentNotReady, "Subnet provider identity unavailable")
	}
	verifyCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	currentCtx, e := p.networkProvider.namespaceContext(verifyCtx, sb)
	if e != nil {
		return biz.VNic{}, e
	}
	for _, resourceID := range []string{subnet.VpcID, subnet.SubnetID} {
		if _, e = p.networkProvider.currentNetworkResource(currentCtx, q, r.TenantID, resourceID); e != nil {
			return biz.VNic{}, e
		}
	}
	id, op := "vnic_"+strings.ReplaceAll(uuid.NewString(), "-", ""), uuid.NewString()
	now, e := q.DatabaseTime(ctx)
	if e != nil {
		return biz.VNic{}, databaseFailure(e)
	}
	_, e = tx.Exec(ctx, `INSERT INTO network_vnics(tenant_id,vnic_id,vpc_id,subnet_id,name,description,state,created_at,updated_at,last_operation_id,requested_ip,requested_mac) VALUES($1,$2,$3,$4,$5,$6,'provisioning',$7,$7,$8,$9,$10)`, r.TenantID, id, subnet.VpcID, r.SubnetID, r.Name, r.Description, now, op, r.RequestedIP, r.RequestedMAC)
	if e != nil {
		var pgErr *pgconn.PgError
		if errors.As(e, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "network_vnics_manual_mac_unique" {
			return biz.VNic{}, biz.Fail(biz.AddressConflict, "MAC address is already reserved by a manual VNIC in this Subnet")
		}
		return biz.VNic{}, databaseFailure(e)
	}
	if e = q.InsertOperation(ctx, sqlcgen.InsertOperationParams{TenantID: r.TenantID, VnicID: id, OperationID: op, Kind: "create_vnic", CreatedAt: now}); e != nil {
		return biz.VNic{}, databaseFailure(e)
	}
	if e = q.InsertReconciliation(ctx, sqlcgen.InsertReconciliationParams{TenantID: r.TenantID, VnicID: id, NextRunAt: now}); e != nil {
		return biz.VNic{}, databaseFailure(e)
	}
	if e = q.InsertBinding(ctx, sqlcgen.InsertBindingParams{TenantID: r.TenantID, VnicID: id, BindingID: uuid.NewString(), ClusterID: identity.ClusterID, Namespace: identity.Name, ProviderName: "ani-" + strings.ReplaceAll(id, "_", "-")}); e != nil {
		return biz.VNic{}, databaseFailure(e)
	}
	row, e := q.GetVNicRow(ctx, sqlcgen.GetVNicRowParams{TenantID: r.TenantID, VnicID: id})
	if e != nil {
		return biz.VNic{}, databaseFailure(e)
	}
	v := vnicRow(row)
	v.ObservationStale = true
	b, e := json.Marshal(v)
	if e != nil {
		return biz.VNic{}, e
	}
	_, e = tx.Exec(ctx, `INSERT INTO network_idempotency(tenant_id,operation_kind,idempotency_key,fingerprint,fingerprint_version,vnic_id,operation_id,response,created_at)VALUES($1,'create_vnic',$2,$3,1,$4,$5,$6,$7)`, r.TenantID, r.IdempotencyKey, r.Fingerprint(), id, op, b, now)
	if e != nil {
		return biz.VNic{}, databaseFailure(e)
	}
	if e = q.InsertHistory(ctx, sqlcgen.InsertHistoryParams{TenantID: r.TenantID, VnicID: id, HistoryID: uuid.NewString(), OperationID: &op, Event: "create_accepted", ResourceState: "provisioning", OperationState: "queued", ActorRef: r.Attribution.Actor, CallerRef: r.Attribution.DirectCaller, CorrelationID: r.Attribution.CorrelationID, CreatedAt: now}); e != nil {
		return biz.VNic{}, databaseFailure(e)
	}
	return v, databaseFailure(tx.Commit(ctx))
}
func (p *Postgres) GetVNic(ctx context.Context, tenant, id string) (biz.VNic, error) {
	row, e := p.queries.GetVNicRow(ctx, sqlcgen.GetVNicRowParams{TenantID: tenant, VnicID: id})
	if e != nil {
		return biz.VNic{}, databaseFailure(e)
	}
	v := vnicRow(row)
	b, e := p.queries.GetBinding(ctx, sqlcgen.GetBindingParams{TenantID: tenant, ResourceID: id})
	if e != nil {
		return v, databaseFailure(e)
	}
	v.ProviderName, v.ProviderUID, v.ClusterID, v.Namespace = b.ProviderName, b.ProviderUid, b.ClusterID, b.Namespace
	identity, e := p.tenantNamespaceIdentity(ctx, tenant)
	if e != nil {
		return v, e
	}
	v.NamespaceUID = identity.UID
	e = p.populateVNicBindings(ctx, p.pool, &v)
	return v, e
}

const vnicListPredicate = ` WHERE v.tenant_id=$1 AND ($2='' OR v.name ILIKE '%'||$2||'%') AND ($3='' OR v.subnet_id=$3) AND (($4='' AND v.state<>'deleted') OR v.state=$4) AND ($5='' OR ($5='free' AND NOT EXISTS(SELECT 1 FROM network_vnic_bindings b WHERE b.tenant_id=v.tenant_id AND b.vnic_id=v.vnic_id AND b.released_at IS NULL)) OR ($5='bound' AND EXISTS(SELECT 1 FROM network_vnic_bindings b WHERE b.tenant_id=v.tenant_id AND b.vnic_id=v.vnic_id AND b.released_at IS NULL AND b.applied_revision>0)) OR ($5='reserved' AND EXISTS(SELECT 1 FROM network_vnic_bindings b WHERE b.tenant_id=v.tenant_id AND b.vnic_id=v.vnic_id AND b.released_at IS NULL) AND NOT EXISTS(SELECT 1 FROM network_vnic_bindings b WHERE b.tenant_id=v.tenant_id AND b.vnic_id=v.vnic_id AND b.released_at IS NULL AND b.applied_revision>0)))`

func (p *Postgres) ListVNics(ctx context.Context, tenant string, f biz.VNicFilter) ([]biz.VNic, int64, error) {
	scope := biz.VNicConsumerScope{}
	if f.BindableToID != "" {
		if p.vnicConsumerResolver == nil {
			return nil, 0, biz.Fail(biz.ProviderUnavailable, "owner candidate resolver unavailable")
		}
		var e error
		scope, e = p.vnicConsumerResolver(ctx, tenant, f.BindableToKind, f.BindableToID)
		if e != nil {
			return nil, 0, e
		}
		if scope.TenantID != tenant || scope.ConsumerID != f.BindableToID || scope.ConsumerKind != f.BindableToKind || scope.SubnetID == "" || scope.Namespace == "" || scope.ClusterID == "" {
			return nil, 0, biz.Fail(biz.ResourceNotFound, "consumer not available for VNIC binding")
		}
		if f.SubnetID != "" && scope.SubnetID != f.SubnetID {
			return nil, 0, biz.Fail(biz.InvalidArgument, "consumer Subnet mismatch")
		}
		f.SubnetID = scope.SubnetID
	}

	tx, e := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if e != nil {
		return nil, 0, databaseFailure(e)
	}
	defer tx.Rollback(ctx)
	args := []any{tenant, f.Name, f.SubnetID, f.State, f.BindingState}
	predicate := vnicListPredicate
	if f.BindableToID != "" {
		predicate += ` AND v.state='available' AND v.mac_address<>'' AND v.primary_ip<>'' AND EXISTS(SELECT 1 FROM network_provider_bindings p WHERE p.tenant_id=v.tenant_id AND p.vnic_id=v.vnic_id AND p.cluster_id=$6 AND p.namespace=$7) AND NOT EXISTS(SELECT 1 FROM network_vnic_bindings b WHERE b.tenant_id=v.tenant_id AND b.vnic_id=v.vnic_id AND b.released_at IS NULL AND (b.consumer_kind<>$8 OR b.consumer_id<>$9))`
		args = append(args, scope.ClusterID, scope.Namespace, f.BindableToKind, f.BindableToID)
	}
	var total int64
	e = tx.QueryRow(ctx, `SELECT count(*) FROM network_vnics v`+predicate, args...).Scan(&total)
	if e != nil {
		return nil, 0, databaseFailure(e)
	}
	rows, e := tx.Query(ctx, `SELECT vnic_id FROM network_vnics v`+predicate+fmt.Sprintf(` ORDER BY created_at DESC,vnic_id DESC LIMIT $%d OFFSET $%d`, len(args)+1, len(args)+2), append(args, f.Limit, f.Offset)...)
	if e != nil {
		return nil, 0, databaseFailure(e)
	}
	var ids []string
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			rows.Close()
			return nil, 0, databaseFailure(e)
		}
		ids = append(ids, id)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, 0, databaseFailure(e)
	}
	q := p.queries.WithTx(tx)
	values := make([]biz.VNic, 0, len(ids))
	for _, id := range ids {
		row, e := q.GetVNicRow(ctx, sqlcgen.GetVNicRowParams{TenantID: tenant, VnicID: id})
		if e != nil {
			return nil, 0, databaseFailure(e)
		}
		v := vnicRow(row)
		if e = p.populateVNicBindings(ctx, tx, &v); e != nil {
			return nil, 0, e
		}
		values = append(values, v)
	}
	return values, total, databaseFailure(tx.Commit(ctx))
}
func (p *Postgres) DeleteVNic(ctx context.Context, tenant, id string) (biz.VNic, error) {
	initial, e := p.queries.GetVNicRow(ctx, sqlcgen.GetVNicRowParams{TenantID: tenant, VnicID: id})
	if e != nil {
		return biz.VNic{}, databaseFailure(e)
	}
	tx, e := p.pool.Begin(ctx)
	if e != nil {
		return biz.VNic{}, databaseFailure(e)
	}
	defer tx.Rollback(ctx)
	q := p.queries.WithTx(tx)
	if _, e = q.LockVPC(ctx, sqlcgen.LockVPCParams{TenantID: tenant, VpcID: initial.VpcID}); e != nil {
		return biz.VNic{}, databaseFailure(e)
	}
	row, e := q.LockVNicRow(ctx, sqlcgen.LockVNicRowParams{TenantID: tenant, VnicID: id})
	if e != nil {
		return biz.VNic{}, databaseFailure(e)
	}
	if row.State == "deleting" || row.State == "deleted" {
		return vnicRow(row), nil
	}
	var count int64
	e = tx.QueryRow(ctx, `SELECT count(*) FROM network_vnic_bindings WHERE tenant_id=$1 AND vnic_id=$2 AND released_at IS NULL`, tenant, id).Scan(&count)
	if e != nil {
		return biz.VNic{}, databaseFailure(e)
	}
	if count > 0 {
		return biz.VNic{}, biz.Fail(biz.ResourceBusy, "VNIC is reserved by an instance")
	}
	b, e := q.GetBinding(ctx, sqlcgen.GetBindingParams{TenantID: tenant, ResourceID: id})
	if e != nil {
		return biz.VNic{}, databaseFailure(e)
	}
	if b.PendingAction != "" {
		return biz.VNic{}, biz.Fail(biz.ResourceBusy, "VNIC has an unresolved provider mutation")
	}
	var active bool
	e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM network_operations WHERE tenant_id=$1 AND vnic_id=$2 AND completed_at IS NULL)`, tenant, id).Scan(&active)
	if e != nil {
		return biz.VNic{}, databaseFailure(e)
	}
	if active {
		return biz.VNic{}, biz.Fail(biz.ResourceBusy, "VNIC operation is still active")
	}
	op := uuid.NewString()
	now, e := q.DatabaseTime(ctx)
	if e != nil {
		return biz.VNic{}, databaseFailure(e)
	}
	if e = q.InsertOperation(ctx, sqlcgen.InsertOperationParams{TenantID: tenant, VnicID: id, OperationID: op, Kind: "delete_vnic", CreatedAt: now}); e != nil {
		return biz.VNic{}, databaseFailure(e)
	}
	_, e = tx.Exec(ctx, `UPDATE network_vnics SET state='deleting',reason='',version=version+1,last_operation_id=$3,updated_at=clock_timestamp() WHERE tenant_id=$1 AND vnic_id=$2`, tenant, id, op)
	if e != nil {
		return biz.VNic{}, databaseFailure(e)
	}
	if e = affected(q.ScheduleDeletion(ctx, sqlcgen.ScheduleDeletionParams{TenantID: tenant, ResourceID: id})); e != nil {
		return biz.VNic{}, e
	}
	row, e = q.GetVNicRow(ctx, sqlcgen.GetVNicRowParams{TenantID: tenant, VnicID: id})
	if e != nil {
		return biz.VNic{}, databaseFailure(e)
	}
	return vnicRow(row), databaseFailure(tx.Commit(ctx))
}
func (p *Postgres) ReserveVNic(ctx context.Context, r biz.VNicReservation) (biz.VNic, error) {
	resultCtx := ctx
	scope := biz.VNicConsumerScope{}
	if p.vnicConsumerResolver != nil {
		var e error
		scope, e = p.vnicConsumerResolver(ctx, r.TenantID, r.ConsumerKind, r.ConsumerID)
		if e != nil {
			return biz.VNic{}, e
		}
		if scope.TenantID != r.TenantID || scope.ConsumerID != r.ConsumerID || scope.ConsumerKind != r.ConsumerKind || scope.SubnetID != r.SubnetID || scope.State == "deleting" || scope.State == "deleted" {
			return biz.VNic{}, biz.Fail(biz.ResourceBusy, "consumer is not available for VNIC binding")
		}
	}

	initial, e := p.queries.GetVNicRow(ctx, sqlcgen.GetVNicRowParams{TenantID: r.TenantID, VnicID: r.VNicID})
	if e != nil {
		return biz.VNic{}, databaseFailure(e)
	}
	tx, e := p.pool.Begin(ctx)
	if e != nil {
		return biz.VNic{}, databaseFailure(e)
	}
	defer tx.Rollback(ctx)
	q := p.queries.WithTx(tx)
	ctx = context.WithValue(ctx, admissionQueriesKey{}, q)
	parent, e := q.LockVPC(ctx, sqlcgen.LockVPCParams{TenantID: r.TenantID, VpcID: initial.VpcID})
	if e != nil {
		return biz.VNic{}, databaseFailure(e)
	}
	subnet, e := q.LockSubnet(ctx, sqlcgen.LockSubnetParams{TenantID: r.TenantID, SubnetID: initial.SubnetID})
	if e != nil {
		return biz.VNic{}, databaseFailure(e)
	}
	row, e := q.LockVNicRow(ctx, sqlcgen.LockVNicRowParams{TenantID: r.TenantID, VnicID: r.VNicID})
	if e != nil {
		return biz.VNic{}, databaseFailure(e)
	}
	if p.vnicConsumerResolver != nil {
		b, e := q.GetBinding(ctx, sqlcgen.GetBindingParams{TenantID: r.TenantID, ResourceID: r.VNicID})
		if e != nil {
			return biz.VNic{}, databaseFailure(e)
		}
		identity, e := p.tenantNamespaceIdentity(ctx, r.TenantID)
		if e != nil {
			return biz.VNic{}, e
		}
		if b.ClusterID != scope.ClusterID || b.Namespace != scope.Namespace || (scope.NamespaceUID != "" && scope.NamespaceUID != identity.UID) {
			return biz.VNic{}, biz.Fail(biz.InvalidArgument, "consumer and VNIC placement identity differ")
		}
	}
	if row.SubnetID != r.SubnetID {
		return biz.VNic{}, biz.Fail(biz.InvalidArgument, "manual VNIC must use the instance Subnet")
	}
	if !networkManagementOpen(parent.State, parent.Reason) || !networkManagementOpen(subnet.State, subnet.Reason) || row.State != "available" || row.MacAddress == "" || row.PrimaryIp == "" {
		return biz.VNic{}, biz.Fail(biz.ProviderNotReady, "manual VNIC allocation is not ready")
	}
	if p.networkProvider == nil {
		return biz.VNic{}, biz.Fail(biz.ProviderUnavailable, "network provider unavailable")
	}
	binding, e := q.GetBinding(ctx, sqlcgen.GetBindingParams{TenantID: r.TenantID, ResourceID: r.VNicID})
	if e != nil {
		return biz.VNic{}, databaseFailure(e)
	}
	if binding.ProviderUid == "" || binding.PendingAction != "" {
		return biz.VNic{}, biz.Fail(biz.ProviderOwnership, "manual VNIC identity or mutation is unresolved")
	}
	verifyCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	currentCtx, e := p.networkProvider.namespaceContext(verifyCtx, binding)
	if e != nil {
		return biz.VNic{}, e
	}
	for _, resourceID := range []string{row.VpcID, row.SubnetID} {
		if _, e = p.networkProvider.currentNetworkResource(currentCtx, q, r.TenantID, resourceID); e != nil {
			return biz.VNic{}, e
		}
	}
	current, e := p.networkProvider.observeDirect(currentCtx, biz.ProviderTarget{TenantID: r.TenantID, ResourceID: r.VNicID, Kind: "vnic", BindingID: binding.BindingID, KnownIdentity: binding.ProviderUid, RequestedIP: row.RequestedIp, RequestedMAC: row.RequestedMac})
	if e != nil {
		return biz.VNic{}, e
	}
	if !current.Ready || current.VNic == nil || current.VNic.MACAddress != row.MacAddress || current.VNic.PrimaryIP != row.PrimaryIp {
		return biz.VNic{}, biz.Fail(biz.ProviderNotReady, "manual VNIC has not confirmed its allocated address")
	}
	var occupied bool
	e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM network_vnic_bindings WHERE tenant_id=$1 AND vnic_id=$2 AND released_at IS NULL AND (consumer_kind<>$3 OR consumer_id<>$4 OR slot<>$5))`, r.TenantID, r.VNicID, r.ConsumerKind, r.ConsumerID, r.Slot).Scan(&occupied)
	if e != nil {
		return biz.VNic{}, databaseFailure(e)
	}
	if occupied {
		return biz.VNic{}, biz.Fail(biz.ResourceBusy, "VNIC is reserved by another instance")
	}
	_, e = tx.Exec(ctx, `INSERT INTO network_vnic_bindings(tenant_id,binding_id,vnic_id,consumer_kind,consumer_id,slot,operation_id,requested_revision) SELECT $1,$2,$3,$4,$5,$6,$7,$8 WHERE NOT EXISTS(SELECT 1 FROM network_vnic_bindings WHERE tenant_id=$1 AND vnic_id=$3 AND consumer_kind=$4 AND consumer_id=$5 AND slot=$6 AND requested_revision=$8 AND released_at IS NULL) ON CONFLICT(tenant_id,consumer_kind,consumer_id,slot,operation_id,vnic_id) DO NOTHING`, r.TenantID, uuid.NewString(), r.VNicID, r.ConsumerKind, r.ConsumerID, r.Slot, r.OperationID, r.Revision)
	if e != nil {
		return biz.VNic{}, databaseFailure(e)
	}
	if e = tx.Commit(ctx); e != nil {
		return biz.VNic{}, databaseFailure(e)
	}
	return p.GetVNic(resultCtx, r.TenantID, r.VNicID)
}
func (p *Postgres) ReleaseVNicBindings(ctx context.Context, tenant, kind, consumer string, keep []string) error {
	_, e := p.pool.Exec(ctx, `UPDATE network_vnic_bindings SET released_at=clock_timestamp() WHERE tenant_id=$1 AND consumer_kind=$2 AND consumer_id=$3 AND released_at IS NULL AND vnic_id<>ALL(coalesce($4::text[],'{}'))`, tenant, kind, consumer, keep)
	return databaseFailure(e)
}
func (p *Postgres) SyncVNicSecurityGroups(ctx context.Context, tenant, id, kind, consumer string, groups []string) (biz.VNic, error) {
	if p.networkProvider == nil {
		return biz.VNic{}, biz.Fail(biz.ProviderUnavailable, "network provider unavailable")
	}
	return p.networkProvider.syncVNicSecurityGroups(ctx, tenant, id, kind, consumer, groups)
}

var _ biz.VNicRepository = (*Postgres)(nil)

func (p *Postgres) ApplyVNicBinding(ctx context.Context, tenant, kind, consumer, id string, revision int64) error {
	tx, e := p.pool.Begin(ctx)
	if e != nil {
		return databaseFailure(e)
	}
	defer tx.Rollback(ctx)
	if id != "" {
		var found bool
		e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM network_vnic_bindings WHERE tenant_id=$1 AND consumer_kind=$2 AND consumer_id=$3 AND vnic_id=$4 AND requested_revision=$5 AND released_at IS NULL)`, tenant, kind, consumer, id, revision).Scan(&found)
		if e != nil {
			return databaseFailure(e)
		}
		if !found {
			return biz.Fail(biz.ResourceBusy, "target VNIC reservation is missing")
		}
	}
	_, e = tx.Exec(ctx, `UPDATE network_vnic_bindings SET applied_revision=CASE WHEN vnic_id=$4 AND requested_revision=$5 THEN $5 ELSE applied_revision END,released_at=CASE WHEN vnic_id=$4 AND requested_revision=$5 THEN NULL ELSE clock_timestamp() END WHERE tenant_id=$1 AND consumer_kind=$2 AND consumer_id=$3 AND released_at IS NULL`, tenant, kind, consumer, id, revision)
	if e != nil {
		return databaseFailure(e)
	}
	return databaseFailure(tx.Commit(ctx))
}

func (p *Postgres) ConfigureVNicConsumerResolver(r biz.VNicConsumerResolver) {
	p.vnicConsumerResolver = r
}
func (p *Postgres) populateVNicBindings(ctx context.Context, db interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}, v *biz.VNic) error {
	e := db.QueryRow(ctx, `SELECT p.name,s.name,ARRAY(SELECT coalesce(g.payload->>'Name','') FROM unnest(v.security_group_ids) WITH ORDINALITY x(id,ord) JOIN network_security_groups g ON g.tenant_id=v.tenant_id AND g.security_group_id=x.id ORDER BY x.ord) FROM network_vnics v JOIN network_vpcs p ON p.tenant_id=v.tenant_id AND p.vpc_id=v.vpc_id JOIN network_subnets s ON s.tenant_id=v.tenant_id AND s.subnet_id=v.subnet_id WHERE v.tenant_id=$1 AND v.vnic_id=$2`, v.TenantID, v.ID).Scan(&v.VPCName, &v.SubnetName, &v.SecurityGroupNames)
	if e != nil {
		return databaseFailure(e)
	}
	rows, e := db.Query(ctx, `SELECT b.binding_id,b.consumer_kind,b.consumer_id,b.slot,b.requested_revision,b.applied_revision,b.requested_revision=(SELECT max(x.requested_revision) FROM network_vnic_bindings x WHERE x.tenant_id=b.tenant_id AND x.consumer_kind=b.consumer_kind AND x.consumer_id=b.consumer_id AND x.slot=b.slot AND x.released_at IS NULL) FROM network_vnic_bindings b WHERE b.tenant_id=$1 AND b.vnic_id=$2 AND b.released_at IS NULL ORDER BY b.created_at,b.binding_id`, v.TenantID, v.ID)
	if e != nil {
		return databaseFailure(e)
	}
	for rows.Next() {
		var b biz.VNicBinding
		if e = rows.Scan(&b.ID, &b.ConsumerKind, &b.ConsumerID, &b.Slot, &b.RequestedRevision, &b.AppliedRevision, &b.Target); e != nil {
			rows.Close()
			return databaseFailure(e)
		}
		b.Current = b.AppliedRevision > 0
		v.Bindings = append(v.Bindings, b)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return databaseFailure(e)
	}
	if p.vnicConsumerResolver != nil {
		for i := range v.Bindings {
			b := &v.Bindings[i]
			scope, e := p.vnicConsumerResolver(ctx, v.TenantID, b.ConsumerKind, b.ConsumerID)
			if e != nil {
				return e
			}
			if scope.TenantID != v.TenantID || scope.ConsumerID != b.ConsumerID {
				return biz.Fail(biz.ResourceNotFound, "VNIC consumer not found")
			}
			b.ConsumerName, b.ComputeType = scope.Name, scope.ComputeType
		}
	}
	return nil
}
