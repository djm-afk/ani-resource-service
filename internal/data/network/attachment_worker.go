package data

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
)

func (p *Postgres) ClaimAttachment(ctx context.Context, owner string, lease time.Duration) (biz.AttachmentWork, bool, error) {
	tx, e := p.pool.Begin(ctx)
	if e != nil {
		return biz.AttachmentWork{}, false, databaseFailure(e)
	}
	defer tx.Rollback(ctx)
	q := p.queries.WithTx(tx)
	v, e := q.LockDueAttachmentParent(ctx)
	if errors.Is(e, pgx.ErrNoRows) {
		return biz.AttachmentWork{}, false, nil
	}
	if e != nil {
		return biz.AttachmentWork{}, false, databaseFailure(e)
	}
	s, e := q.LockDueAttachmentSubnet(ctx, sqlcgen.LockDueAttachmentSubnetParams{TenantID: v.TenantID, VpcID: v.VpcID})
	if errors.Is(e, pgx.ErrNoRows) {
		return biz.AttachmentWork{}, false, nil
	}
	if e != nil {
		return biz.AttachmentWork{}, false, databaseFailure(e)
	}
	a, e := q.LockDueAttachment(ctx, sqlcgen.LockDueAttachmentParams{TenantID: s.TenantID, SubnetID: s.SubnetID})
	if errors.Is(e, pgx.ErrNoRows) {
		return biz.AttachmentWork{}, false, nil
	}
	if e != nil {
		return biz.AttachmentWork{}, false, databaseFailure(e)
	}
	a, e = q.ClaimAttachment(ctx, sqlcgen.ClaimAttachmentParams{TenantID: a.TenantID, AttachmentID: a.AttachmentID, Owner: owner, LeaseMicros: lease.Microseconds()})
	if e != nil {
		return biz.AttachmentWork{}, false, databaseFailure(e)
	}
	value, e := managedAttachment(ctx, tx, a)
	if e != nil {
		return biz.AttachmentWork{}, false, e
	}
	now, e := q.DatabaseTime(ctx)
	if e != nil {
		return biz.AttachmentWork{}, false, databaseFailure(e)
	}
	work := biz.AttachmentWork{Now: now, ClaimedGeneration: a.RequestedGeneration, Attachment: value, Owner: owner, Epoch: a.Epoch}
	if e = json.Unmarshal(a.Plan, &work.Plan); e != nil {
		return biz.AttachmentWork{}, false, databaseFailure(e)
	}
	if e = tx.Commit(ctx); e != nil {
		return biz.AttachmentWork{}, false, databaseFailure(e)
	}
	return work, true, nil
}
func (p *Postgres) FinishAttachment(ctx context.Context, w biz.AttachmentWork, progress biz.AttachmentProgress) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return databaseFailure(err)
	}
	defer tx.Rollback(ctx)
	q := p.queries.WithTx(tx)
	row, err := lockAttachmentParents(ctx, q, w.Attachment.TenantID, w.Attachment.ID)
	if err != nil {
		return databaseFailure(err)
	}
	if row.Version != w.Attachment.Version || row.Epoch != w.Epoch || textValue(row.LeaseOwner) != w.Owner {
		return biz.ErrLeaseLost
	}
	old := biz.AttachmentState(row.State)
	valid := old == progress.State ||
		(old == biz.Reserved && (progress.State == biz.Attached || progress.State == biz.Releasing || progress.State == biz.Released)) ||
		(old == biz.Attached && (progress.State == biz.Reserved || progress.State == biz.Releasing || progress.State == biz.Released)) ||
		(old == biz.Releasing && progress.State == biz.Released)
	if !valid || (row.FinalizationID != nil && *row.FinalizationID != progress.FinalizationID) {
		return biz.ErrLeaseLost
	}
	current, err := managedAttachment(ctx, tx, row)
	if err != nil {
		return err
	}
	if progress.ConsumerSnapshot != nil && (current.WorkloadMode == "controller_managed" || current.WorkloadMode == "vm_managed") {
		if err = persistConsumerSnapshot(ctx, tx, current, *progress.ConsumerSnapshot); err != nil {
			return err
		}
	}
	var finalization *string
	if progress.FinalizationID != "" {
		finalization = &progress.FinalizationID
	}
	now, err := q.DatabaseTime(ctx)
	if err != nil {
		return databaseFailure(err)
	}
	addresses := attachmentAddresses(current)
	if progress.Addresses != nil {
		addresses = *progress.Addresses
	}
	park := progress.State == biz.Released || (progress.Observed && !progress.Backoff && (progress.State == biz.Attached || progress.State == biz.Reserved))
	updated, err := q.FinishAttachment(ctx, sqlcgen.FinishAttachmentParams{
		TenantID: row.TenantID, AttachmentID: row.AttachmentID, Version: row.Version, Epoch: w.Epoch, Owner: w.Owner,
		State: string(progress.State), Reason: string(progress.Reason), PodName: progress.PodName, PodUid: progress.PodUID, FinalizationID: finalization,
		Observed: progress.Observed, Now: now, ObservationError: string(progress.ObservationError), ClaimedGeneration: w.ClaimedGeneration,
		UpdateAddresses: progress.Addresses != nil, CurrentVnicName: addresses.CurrentVNicName, CurrentVnicUid: addresses.CurrentVNicUID,
		CurrentMacAddress: addresses.CurrentMACAddress, CurrentIps: append([]string{}, addresses.CurrentIPs...),
		Backoff: progress.Backoff, Park: park, DelayMicros: progress.NextDelay.Microseconds(),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return biz.ErrLeaseLost
	}
	if err != nil {
		return databaseFailure(err)
	}
	changed := updated.State != row.State || updated.Reason != row.Reason || updated.PodUid != row.PodUid || textValue(updated.FinalizationID) != textValue(row.FinalizationID) ||
		updated.CurrentVnicName != row.CurrentVnicName || updated.CurrentVnicUid != row.CurrentVnicUid || updated.CurrentMacAddress != row.CurrentMacAddress || !slices.Equal(updated.CurrentIps, row.CurrentIps)
	if changed {
		if err = attachmentHistory(ctx, q, updated, "observed"); err != nil {
			return databaseFailure(err)
		}
	}
	if updated.CurrentVnicName != row.CurrentVnicName || updated.CurrentVnicUid != row.CurrentVnicUid || updated.PodUid != row.PodUid || updated.State != row.State {
		targetType := ""
		switch updated.ConsumerType {
		case biz.ConsumerContainerInstance:
			targetType = "container"
		case biz.ConsumerVirtualMachine:
			targetType = "vm"
		}
		if targetType != "" {
			if _, err = q.NotifySecurityGroupTarget(ctx, sqlcgen.NotifySecurityGroupTargetParams{TenantID: row.TenantID, TargetType: targetType, TargetID: row.InstanceID}); err != nil {
				return databaseFailure(err)
			}
		}
	}
	return databaseFailure(tx.Commit(ctx))
}
