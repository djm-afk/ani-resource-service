package data

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
	"time"
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
	work := biz.AttachmentWork{Now: now, Requirement: biz.ObservationRequirement{RequestedGeneration: a.RequestedGeneration, AppliedAt: a.EvidenceAppliedAt, Hash: a.EvidenceHash}, Attachment: value, Owner: owner, Epoch: a.Epoch, Relations: a.ProviderRelations, ProtocolBlocked: a.ProtocolBlocked}
	if e = json.Unmarshal(a.Plan, &work.Plan); e != nil {
		return biz.AttachmentWork{}, false, databaseFailure(e)
	}
	if e = tx.Commit(ctx); e != nil {
		return biz.AttachmentWork{}, false, databaseFailure(e)
	}
	return work, true, nil
}
func (p *Postgres) FinishAttachment(ctx context.Context, w biz.AttachmentWork, progress biz.AttachmentProgress) error {
	tx, e := p.pool.Begin(ctx)
	if e != nil {
		return databaseFailure(e)
	}
	defer tx.Rollback(ctx)
	q := p.queries.WithTx(tx)
	a, e := lockAttachmentParents(ctx, q, w.Attachment.TenantID, w.Attachment.ID)
	if e != nil {
		return databaseFailure(e)
	}
	if a.Version != w.Attachment.Version || a.Epoch != w.Epoch || textValue(a.LeaseOwner) != w.Owner {
		return biz.ErrLeaseLost
	}
	if progress.Observed && !progress.Proof.Covers(w.Requirement) {
		return biz.ErrLeaseLost
	}
	old := biz.AttachmentState(a.State)
	valid := old == progress.State || (old == biz.Reserved && (progress.State == biz.Attached || progress.State == biz.Releasing || progress.State == biz.Released)) || (old == biz.Attached && (progress.State == biz.Releasing || progress.State == biz.Released)) || (old == biz.Releasing && progress.State == biz.Released)
	managed, e := managedAttachment(ctx, tx, a)
	if e != nil {
		return e
	}
	isManaged := (managed.WorkloadMode == "controller_managed" || managed.WorkloadMode == "vm_managed")
	if isManaged && old == biz.Attached && progress.State == biz.Reserved {
		valid = true
	}
	if !valid || (!isManaged && a.PodUid != "" && a.PodUid != progress.PodUID) || (a.FinalizationID != nil && *a.FinalizationID != progress.FinalizationID) {
		return biz.ErrLeaseLost
	}
	protocolBlocked := a.ProtocolBlocked || progress.ProtocolBlocked
	if progress.ProtocolRecovered {
		// Recheck the existing high water and Confirm ACK while the attachment
		// parents/version/lease are locked. No caller can clear a fence merely
		// by setting ProtocolBlocked=false in a stale progress value.
		openRecovery, closedRecovery := false, false
		if progress.ConsumerSnapshot != nil {
			recoveryWork := w
			recoveryWork.Attachment = managed
			recoveryWork.Requirement.AppliedAt = a.EvidenceAppliedAt
			openRecovery = progress.State == biz.Attached && biz.ManagedProtocolRecoveryMatches(managed, *progress.ConsumerSnapshot, progress.PodName, progress.PodUID)
			closedRecovery = progress.State == biz.Released && progress.PodName == "" && progress.PodUID == "" && biz.ClosedProtocolRecoveryProof(recoveryWork, progress.Proof) && biz.ClosedProtocolRecoveryMatches(managed, *progress.ConsumerSnapshot)
			if closedRecovery {
				var relations []attachmentRelation
				if len(progress.Relations) == 0 || json.Unmarshal(progress.Relations, &relations) != nil {
					closedRecovery = false
				}
				for _, relation := range relations {
					if relation.Current || relation.AddressEligible {
						closedRecovery = false
					}
				}
			}
		}
		if !isManaged || !a.ProtocolBlocked || progress.ProtocolBlocked || !progress.Observed || progress.Reason != "" || (!openRecovery && !closedRecovery) {
			return biz.ErrLeaseLost
		}
		protocolBlocked = false
	}
	if isManaged {
		if progress.ConsumerSnapshot != nil {
			if e = persistConsumerSnapshot(ctx, tx, managed, *progress.ConsumerSnapshot); e != nil {
				return e
			}
		}
		if a.ConfirmUid != "" && a.ConfirmUid != progress.PodUID {
			if _, e = tx.Exec(ctx, `UPDATE network_attachments SET confirm_uid='' WHERE tenant_id=$1 AND attachment_id=$2`, a.TenantID, a.AttachmentID); e != nil {
				return databaseFailure(e)
			}
		}
	}
	var finalization *string
	if progress.FinalizationID != "" {
		finalization = &progress.FinalizationID
	}
	stoppedReservation := progress.State == biz.Reserved && w.Attachment.WorkloadMode == "vm_managed" && progress.ConsumerSnapshot != nil && progress.ConsumerSnapshot.State == "open" && progress.ConsumerSnapshot.DesiredRunState == "stopped"
	park := progress.Observed && progress.Reason == "" && !protocolBlocked && !progress.Backoff && (progress.State == biz.Released || (p.sharedObservation && (progress.State == biz.Attached || stoppedReservation)))
	row, e := q.FinishAttachment(ctx, sqlcgen.FinishAttachmentParams{TenantID: a.TenantID, AttachmentID: a.AttachmentID, Version: a.Version, Epoch: w.Epoch, Owner: w.Owner, State: string(progress.State), Reason: string(progress.Reason), ProtocolBlocked: protocolBlocked, PodName: progress.PodName, PodUid: progress.PodUID, FinalizationID: finalization, ProviderRelations: progress.Relations, Observed: progress.Observed, ObservedAt: proofTime(progress.Observed, progress.Proof), CoveredGeneration: coveredGeneration(progress.Observed, progress.Proof, w.Requirement), EvidenceHash: progress.Proof.Hash, Backoff: progress.Backoff, Park: park, DelayMicros: progress.NextDelay.Microseconds()})
	if errors.Is(e, pgx.ErrNoRows) {
		return biz.ErrLeaseLost
	}
	if e != nil {
		return databaseFailure(e)
	}
	if row.State != a.State || row.Reason != a.Reason || row.PodUid != a.PodUid || textValue(row.FinalizationID) != textValue(a.FinalizationID) || string(row.ProviderRelations) != string(a.ProviderRelations) {
		if e = attachmentHistory(ctx, q, row, "observed"); e != nil {
			return databaseFailure(e)
		}
	}
	return databaseFailure(tx.Commit(ctx))
}
