package data

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
)

// ListAttachmentConsumers reads the consumers holding a network slot in one
// scope. Count and page run in the same read-only snapshot with one predicate,
// so a page never reports a total that its own filter cannot produce.
//
// Addresses and staleness come from Network's own persisted observation. Names
// and run states are resolved outside this transaction because Instance and VM
// records live in different databases.
func (p *Postgres) ListAttachmentConsumers(ctx context.Context, tenant string, f biz.AttachmentConsumerFilter, freshness time.Duration) ([]biz.AttachmentConsumerRow, int64, error) {
	if freshness <= 0 {
		return nil, 0, biz.Fail(biz.DependencyUnavailable, "attachment observation freshness is not configured")
	}
	tx, e := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if e != nil {
		return nil, 0, databaseFailure(e)
	}
	defer tx.Rollback(ctx)
	q := p.queries.WithTx(tx)
	if e = verifyAttachmentConsumerScope(ctx, q, tenant, f); e != nil {
		return nil, 0, e
	}
	total, e := q.CountAttachmentConsumers(ctx, sqlcgen.CountAttachmentConsumersParams{TenantID: tenant, SubnetID: f.SubnetID, VpcID: f.VPCID})
	if e != nil {
		return nil, 0, databaseFailure(e)
	}
	rows, e := q.ListAttachmentConsumers(ctx, sqlcgen.ListAttachmentConsumersParams{
		TenantID: tenant, SubnetID: f.SubnetID, VpcID: f.VPCID, MaxResults: f.Limit, PageOffset: f.Offset,
	})
	if e != nil {
		return nil, 0, databaseFailure(e)
	}
	now, e := q.DatabaseTime(ctx)
	if e != nil {
		return nil, 0, databaseFailure(e)
	}
	out := make([]biz.AttachmentConsumerRow, 0, len(rows))
	for _, row := range rows {
		if row.TenantID != tenant {
			// The predicate is tenant-scoped, so a mismatch means the storage
			// contract broke. Fail closed instead of forwarding a foreign row.
			return nil, 0, biz.Fail(biz.DependencyUnavailable, "network consumer row carries another tenant")
		}
		attachment := biz.Attachment{
			ID: row.AttachmentID, TenantID: tenant, VPCID: row.VpcID, SubnetID: row.SubnetID, InstanceID: row.InstanceID,
			ConsumerType: row.ConsumerType, Slot: row.Slot, WorkloadMode: row.WorkloadMode,
			State: biz.AttachmentState(row.State), Reason: biz.Reason(row.Reason),
			PodUID: row.PodUid, CreatedAt: row.CreatedAt, ObservedAt: row.ObservedAt,
		}
		if len(row.ConsumerSnapshot) > 0 {
			var snapshot biz.ConsumerSubmission
			if e = json.Unmarshal(row.ConsumerSnapshot, &snapshot); e != nil {
				return nil, 0, databaseFailure(e)
			}
			attachment.ConsumerSnapshot = &snapshot
		}
		if attachment.WorkloadMode == "" {
			attachment.WorkloadMode = "single_pod"
		}
		value := biz.AttachmentConsumerRow{
			TenantID: row.TenantID, AttachmentID: row.AttachmentID, VPCID: row.VpcID, SubnetID: row.SubnetID, SubnetName: row.SubnetName,
			Slot: row.Slot, ConsumerType: row.ConsumerType, ConsumerID: row.InstanceID, PodUID: row.PodUid,
			State: row.State, Reason: row.Reason, ProtocolBlocked: row.ProtocolBlocked,
			CreatedAt: row.CreatedAt, ObservedAt: timeValue(row.ObservedAt), HasObservation: row.ObservedAt != nil,
			ObservationStale: row.ObservedAt != nil && now.Sub(*row.ObservedAt) > freshness,
		}
		if attachment.State == biz.Attached && attachment.Reason == "" && attachment.ObservedAt != nil && now.Sub(*attachment.ObservedAt) <= freshness {
			if attachment.ConsumerType == biz.ConsumerVirtualMachine || attachment.WorkloadMode == "vm_managed" {
				for _, address := range vmCurrentAddressRelations(attachment, row.ProviderRelations) {
					value.Addresses = append(value.Addresses, biz.AttachmentAddress{NICName: address.NICName, Address: address.Address})
				}
			} else {
				value.Addresses = containerAddressRelations(attachment, row.ProviderRelations)
			}
		}
		if value.Addresses == nil {
			value.Addresses = []biz.AttachmentAddress{}
		}
		out = append(out, value)
	}
	return out, total, nil
}

// verifyAttachmentConsumerScope keeps "no such network" distinguishable from
// "no consumer attached". A subnet row is the authority for VPC membership; the
// attachment foreign key already guarantees a stored pair is consistent.
func verifyAttachmentConsumerScope(ctx context.Context, q *sqlcgen.Queries, tenant string, f biz.AttachmentConsumerFilter) error {
	if f.SubnetID != "" {
		subnet, e := q.GetSubnet(ctx, sqlcgen.GetSubnetParams{TenantID: tenant, SubnetID: f.SubnetID})
		if errors.Is(e, pgx.ErrNoRows) {
			return biz.Fail(biz.ResourceNotFound, "subnet not found")
		}
		if e != nil {
			return databaseFailure(e)
		}
		if f.VPCID != "" && subnet.VpcID != f.VPCID {
			return biz.Fail(biz.InvalidArgument, "subnet does not belong to requested VPC")
		}
		return nil
	}
	found, e := q.AttachmentVPCScopeExists(ctx, sqlcgen.AttachmentVPCScopeExistsParams{TenantID: tenant, VpcID: f.VPCID})
	if e != nil {
		return databaseFailure(e)
	}
	if !found {
		return biz.Fail(biz.ResourceNotFound, "vpc not found")
	}
	return nil
}

func timeValue(value *time.Time) time.Time {
	if value == nil {
		return time.Time{}
	}
	return *value
}
