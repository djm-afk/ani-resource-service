package data

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
)

func subnetReplay(ctx context.Context, q *sqlcgen.Queries, intent biz.SubnetIntent) (biz.Subnet, bool, error) {
	prior, err := q.GetSubnetIdempotency(ctx, sqlcgen.GetSubnetIdempotencyParams{TenantID: intent.TenantID, IdempotencyKey: intent.IdempotencyKey})
	if errors.Is(err, pgx.ErrNoRows) {
		return biz.Subnet{}, false, nil
	}
	if err != nil {
		return biz.Subnet{}, false, databaseFailure(err)
	}
	if prior.FingerprintVersion != 1 || prior.Fingerprint != intent.Fingerprint() {
		return biz.Subnet{}, true, biz.Fail(biz.IdempotencyConflict, "key already accepted a different subnet intent")
	}
	var value biz.Subnet
	if err := json.Unmarshal(prior.Response, &value); err != nil {
		return value, true, databaseFailure(err)
	}
	if value.ID != textValue(prior.SubnetID) || value.TenantID != intent.TenantID || value.LastOperationID != prior.OperationID {
		return biz.Subnet{}, true, databaseFailure(fmt.Errorf("invalid subnet acceptance snapshot"))
	}
	return value, true, nil
}

func (p *Postgres) AcceptSubnet(ctx context.Context, intent biz.SubnetIntent, attribution biz.Attribution, freshness time.Duration) (biz.Subnet, error) {
	// The durable replay is checked before any dynamic parent or address rule.
	if value, found, err := subnetReplay(ctx, p.queries, intent); found || err != nil {
		return value, err
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return biz.Subnet{}, databaseFailure(err)
	}
	defer tx.Rollback(ctx)
	q := p.queries.WithTx(tx)
	parent, err := q.LockVPC(ctx, sqlcgen.LockVPCParams{TenantID: intent.TenantID, VpcID: intent.VPCID})
	if err != nil {
		return biz.Subnet{}, databaseFailure(err)
	}
	// A key may be reused against a different parent; serialize its unique domain
	// after the parent lock, then read the winner before checking mutable state.
	if err := q.LockSubnetCreationKey(ctx, sqlcgen.LockSubnetCreationKeyParams{TenantID: intent.TenantID, IdempotencyKey: intent.IdempotencyKey}); err != nil {
		return biz.Subnet{}, databaseFailure(err)
	}
	if value, found, err := subnetReplay(ctx, q, intent); found || err != nil {
		return value, err
	}
	if err := p.acceptTenantNamespace(ctx, q, intent.NamespaceIdentity); err != nil {
		return biz.Subnet{}, err
	}
	now, err := q.DatabaseTime(ctx)
	if err != nil {
		return biz.Subnet{}, databaseFailure(err)
	}
	if parent.State != string(biz.Available) || parent.ObservedAt == nil || now.Sub(*parent.ObservedAt) > freshness {
		return biz.Subnet{}, biz.Fail(biz.ParentNotReady, "parent VPC requires a fresh available observation")
	}
	prefix := netip.MustParsePrefix(intent.CIDR)
	parentPrefix := netip.MustParsePrefix(parent.Cidr)
	if prefix.Bits() < parentPrefix.Bits() || !parentPrefix.Contains(prefix.Addr()) {
		return biz.Subnet{}, biz.Fail(biz.InvalidArgument, "subnet CIDR must be contained in the parent VPC")
	}
	overlap, err := q.SubnetOverlaps(ctx, sqlcgen.SubnetOverlapsParams{TenantID: intent.TenantID, VpcID: intent.VPCID, Cidr: prefix})
	if err != nil {
		return biz.Subnet{}, databaseFailure(err)
	}
	if overlap {
		return biz.Subnet{}, biz.Fail(biz.CIDROverlap, "subnet CIDR overlaps an unreleased subnet")
	}
	id, opID := "subnet_"+strings.ReplaceAll(uuid.NewString(), "-", ""), uuid.NewString()
	row, err := q.InsertSubnet(ctx, sqlcgen.InsertSubnetParams{TenantID: intent.TenantID, SubnetID: id, VpcID: intent.VPCID, Name: intent.Name, Description: intent.Description, Cidr: intent.CIDR, Gateway: intent.Gateway, CreatedAt: now, LastOperationID: opID})
	if err != nil {
		return biz.Subnet{}, databaseFailure(err)
	}
	if err := q.InsertOperation(ctx, sqlcgen.InsertOperationParams{TenantID: intent.TenantID, SubnetID: id, OperationID: opID, Kind: "create_subnet", CreatedAt: now}); err != nil {
		return biz.Subnet{}, databaseFailure(err)
	}
	if err := q.InsertReconciliation(ctx, sqlcgen.InsertReconciliationParams{TenantID: intent.TenantID, SubnetID: id, NextRunAt: now}); err != nil {
		return biz.Subnet{}, databaseFailure(err)
	}
	parentBinding, err := q.GetBinding(ctx, sqlcgen.GetBindingParams{TenantID: intent.TenantID, ResourceID: intent.VPCID})
	if err != nil {
		return biz.Subnet{}, databaseFailure(err)
	}
	if err := q.InsertBinding(ctx, sqlcgen.InsertBindingParams{TenantID: intent.TenantID, SubnetID: id, BindingID: uuid.NewString(), ClusterID: parentBinding.ClusterID, Namespace: parentBinding.Namespace, ProviderName: strings.Replace(id, "_", "-", 1)}); err != nil {
		return biz.Subnet{}, databaseFailure(err)
	}
	value := subnet(row)
	value.ObservationStale = true
	snapshot, err := json.Marshal(value)
	if err != nil {
		return biz.Subnet{}, databaseFailure(err)
	}
	if err := q.InsertSubnetIdempotency(ctx, sqlcgen.InsertSubnetIdempotencyParams{TenantID: intent.TenantID, SubnetID: id, IdempotencyKey: intent.IdempotencyKey, Fingerprint: intent.Fingerprint(), OperationID: opID, Response: snapshot, CreatedAt: now}); err != nil {
		return biz.Subnet{}, databaseFailure(err)
	}
	if err := q.InsertHistory(ctx, sqlcgen.InsertHistoryParams{TenantID: intent.TenantID, SubnetID: id, HistoryID: uuid.NewString(), OperationID: &opID, Event: "create_accepted", ResourceState: string(biz.Provisioning), OperationState: string(biz.Queued), ActorRef: attribution.Actor, CallerRef: attribution.DirectCaller, CorrelationID: attribution.CorrelationID, CreatedAt: now}); err != nil {
		return biz.Subnet{}, databaseFailure(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return biz.Subnet{}, databaseFailure(err)
	}
	return value, nil
}
func (p *Postgres) GetSubnet(ctx context.Context, tenant, id string) (biz.Subnet, error) {
	row, err := p.queries.GetSubnet(ctx, sqlcgen.GetSubnetParams{TenantID: tenant, SubnetID: id})
	return subnet(row), databaseFailure(err)
}
func (p *Postgres) ListSubnets(ctx context.Context, tenant string, f biz.SubnetFilter) ([]biz.Subnet, int64, error) {
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, 0, databaseFailure(err)
	}
	defer tx.Rollback(ctx)
	q := p.queries.WithTx(tx)
	total, err := q.CountListedSubnets(ctx, sqlcgen.CountListedSubnetsParams{TenantID: tenant, VpcFilter: f.VPCID, NameFilter: f.Name, StateFilter: f.State})
	if err != nil {
		return nil, 0, databaseFailure(err)
	}
	rows, err := q.ListSubnets(ctx, sqlcgen.ListSubnetsParams{TenantID: tenant, VpcFilter: f.VPCID, NameFilter: f.Name, StateFilter: f.State, AfterID: f.AfterID, AfterCreatedAt: f.AfterCreatedAt, MaxResults: f.Limit})
	if err != nil {
		return nil, 0, databaseFailure(err)
	}
	values := make([]biz.Subnet, 0, len(rows))
	for _, row := range rows {
		values = append(values, subnet(row))
	}
	return values, total, nil
}
func (p *Postgres) DeleteSubnet(ctx context.Context, tenant, id string) (biz.Subnet, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return biz.Subnet{}, databaseFailure(err)
	}
	defer tx.Rollback(ctx)
	q := p.queries.WithTx(tx)
	// Read immutable parent identity, then lock in the global parent-first order.
	initial, err := q.GetSubnet(ctx, sqlcgen.GetSubnetParams{TenantID: tenant, SubnetID: id})
	if err != nil {
		return biz.Subnet{}, databaseFailure(err)
	}
	if _, err := q.LockVPC(ctx, sqlcgen.LockVPCParams{TenantID: tenant, VpcID: initial.VpcID}); err != nil {
		return biz.Subnet{}, databaseFailure(err)
	}
	row, err := q.LockSubnet(ctx, sqlcgen.LockSubnetParams{TenantID: tenant, SubnetID: id})
	if err != nil {
		return biz.Subnet{}, databaseFailure(err)
	}
	if row.State == string(biz.Deleting) || row.State == string(biz.Deleted) {
		return subnet(row), nil
	}
	lbCount, err := q.BlockingLBForSubnet(ctx, sqlcgen.BlockingLBForSubnetParams{TenantID: tenant, SubnetID: id})
	if err != nil {
		return biz.Subnet{}, databaseFailure(err)
	}
	if lbCount > 0 {
		return biz.Subnet{}, biz.Fail(biz.ResourceInUse, "subnet has an unreleased load balancer")
	}
	count, err := q.CountAttachments(ctx, sqlcgen.CountAttachmentsParams{TenantID: tenant, SubnetID: id})
	if err != nil {
		return biz.Subnet{}, databaseFailure(err)
	}
	if count != 0 {
		return biz.Subnet{}, biz.Fail(biz.ResourceInUse, "subnet has unreleased attachments or protocol violations")
	}
	op, err := q.GetOperation(ctx, sqlcgen.GetOperationParams{TenantID: tenant, OperationID: row.LastOperationID})
	if err != nil {
		return biz.Subnet{}, databaseFailure(err)
	}
	if op.CompletedAt == nil {
		return biz.Subnet{}, biz.Fail(biz.ResourceBusy, "subnet creation is still active")
	}
	now, err := q.DatabaseTime(ctx)
	if err != nil {
		return biz.Subnet{}, databaseFailure(err)
	}
	opID := uuid.NewString()
	if err := q.InsertOperation(ctx, sqlcgen.InsertOperationParams{TenantID: tenant, SubnetID: id, OperationID: opID, Kind: "delete_subnet", CreatedAt: now}); err != nil {
		return biz.Subnet{}, databaseFailure(err)
	}
	row, err = q.AdmitSubnetDeletion(ctx, sqlcgen.AdmitSubnetDeletionParams{TenantID: tenant, SubnetID: id, Version: row.Version, OperationID: opID})
	if err != nil {
		return biz.Subnet{}, databaseFailure(err)
	}
	if err := affected(q.ScheduleDeletion(ctx, sqlcgen.ScheduleDeletionParams{TenantID: tenant, ResourceID: id})); err != nil {
		return biz.Subnet{}, err
	}
	if err := q.InsertHistory(ctx, sqlcgen.InsertHistoryParams{TenantID: tenant, SubnetID: id, OperationID: &opID, HistoryID: uuid.NewString(), Event: "delete_accepted", ResourceState: row.State, OperationState: string(biz.Queued), CreatedAt: now}); err != nil {
		return biz.Subnet{}, databaseFailure(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return biz.Subnet{}, databaseFailure(err)
	}
	return subnet(row), nil
}
func subnet(row sqlcgen.NetworkSubnet) biz.Subnet {
	value := biz.Subnet{ID: row.SubnetID, TenantID: row.TenantID, VPCID: row.VpcID, Name: row.Name, Description: row.Description, CIDR: row.Cidr, Gateway: row.Gateway, State: biz.ResourceState(row.State), Reason: biz.Reason(row.Reason), Version: row.Version, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt, ObservedAt: row.ObservedAt, LastOperationID: row.LastOperationID}
	value.AvailableIPRange, value.UsedIPRange = row.AvailableIpRange, row.UsedIpRange
	return value
}
