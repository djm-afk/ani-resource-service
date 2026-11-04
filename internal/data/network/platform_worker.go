package data

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
	"time"
)

func (p *Postgres) claimPlatform(ctx context.Context, tx pgx.Tx, q *sqlcgen.Queries, owner string, duration time.Duration) (biz.Work, bool, error) {
	r, err := q.DuePlatformCandidate(ctx, sqlcgen.DuePlatformCandidateParams{ClusterID: p.placement.ClusterID})
	if errors.Is(err, pgx.ErrNoRows) {
		return biz.Work{}, false, nil
	}
	if err != nil {
		return biz.Work{}, false, databaseFailure(err)
	}
	if r.ClusterID != p.placement.ClusterID {
		return biz.Work{}, false, biz.Fail(biz.DependencyUnavailable, "platform placement does not match executor")
	}
	lease, err := q.AcquirePlatformLease(ctx, sqlcgen.AcquirePlatformLeaseParams{ResourceID: r.ResourceID, Owner: owner, LeaseMicros: duration.Microseconds()})
	if errors.Is(err, pgx.ErrNoRows) {
		return biz.Work{}, false, nil
	}
	if err != nil {
		return biz.Work{}, false, databaseFailure(err)
	}
	op, err := q.GetPlatformOperation(ctx, sqlcgen.GetPlatformOperationParams{ClusterID: p.placement.ClusterID, OperationID: r.LastOperationID})
	if err != nil {
		return biz.Work{}, false, databaseFailure(err)
	}
	active := op.CompletedAt == nil
	if active {
		op, err = q.RunPlatformOperation(ctx, sqlcgen.RunPlatformOperationParams{ResourceID: r.ResourceID, OperationID: op.OperationID, Epoch: lease.LeaseEpoch})
		if err != nil {
			return biz.Work{}, false, databaseFailure(err)
		}
	}
	value, err := platformSnapshot(ctx, q, r)
	if err != nil {
		return biz.Work{}, false, err
	}
	now, err := q.DatabaseTime(ctx)
	if err != nil {
		return biz.Work{}, false, databaseFailure(err)
	}
	work := biz.Work{Resource: biz.ResourceWork{ID: r.ResourceID, Kind: r.Kind, State: biz.ResourceState(r.State), Reason: biz.Reason(r.Reason), Version: r.Version, ObservedAt: r.ObservedAt, UpdatedAt: r.UpdatedAt, LastOperationID: r.LastOperationID, Egress: &biz.EgressWorkSpec{Platform: &value}}, Operation: biz.Operation{ID: op.OperationID, ResourceID: r.ResourceID, ResourceType: r.Kind, Kind: op.Kind, State: biz.OperationState(op.State), Reason: biz.Reason(op.Reason)}, ActiveOperation: active, BindingID: r.BindingID, KnownIdentity: r.ProviderUid, PendingAction: r.PendingAction, Owner: owner, Epoch: lease.LeaseEpoch, Attempt: op.Attempt, Now: now, ClaimedGeneration: lease.RequestedGeneration}
	if err = tx.Commit(ctx); err != nil {
		return biz.Work{}, false, databaseFailure(err)
	}
	return work, true, nil
}
func (p *Postgres) lockedPlatform(ctx context.Context, q *sqlcgen.Queries, w biz.Work) error {
	r, err := q.LockPlatform(ctx, sqlcgen.LockPlatformParams{ClusterID: p.placement.ClusterID, Kind: w.Resource.Kind, ResourceID: w.Resource.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		return biz.ErrLeaseLost
	}
	if err != nil {
		return databaseFailure(err)
	}
	if r.Version != w.Resource.Version || r.BindingID != w.BindingID {
		return biz.ErrLeaseLost
	}
	_, err = q.CheckPlatformLease(ctx, sqlcgen.CheckPlatformLeaseParams{ResourceID: r.ResourceID, Owner: w.Owner, Epoch: w.Epoch})
	if errors.Is(err, pgx.ErrNoRows) {
		return biz.ErrLeaseLost
	}
	return databaseFailure(err)
}
func (p *Postgres) beginPlatformMutation(ctx context.Context, w biz.Work, action, identity string) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return databaseFailure(err)
	}
	defer tx.Rollback(ctx)
	q := p.queries.WithTx(tx)
	if err = p.lockedPlatform(ctx, q, w); err != nil {
		return err
	}
	if err = affected(q.BeginPlatformMutation(ctx, sqlcgen.BeginPlatformMutationParams{ResourceID: w.Resource.ID, BindingID: w.BindingID, Action: action, Identity: identity})); err != nil {
		return err
	}
	return databaseFailure(tx.Commit(ctx))
}
func (p *Postgres) finishPlatform(ctx context.Context, w biz.Work, progress biz.Progress) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return databaseFailure(err)
	}
	defer tx.Rollback(ctx)
	q := p.queries.WithTx(tx)
	if err = p.lockedPlatform(ctx, q, w); err != nil {
		return err
	}
	now, err := q.DatabaseTime(ctx)
	if err != nil {
		return databaseFailure(err)
	}
	var observedAt *time.Time
	if progress.Observed {
		observedAt = &now
	}
	if w.Resource.Kind == "public_pool" && progress.State == biz.Deleted {
		progress, err = p.finishManagedPoolCleanup(ctx, q, w, progress)
		if err != nil {
			return err
		}
	}
	r, err := q.AdvancePlatform(ctx, sqlcgen.AdvancePlatformParams{ResourceID: w.Resource.ID, Version: w.Resource.Version, State: string(progress.State), Reason: string(progress.Reason), Identity: progress.Identity, Observed: progress.Observed, ObservedAt: observedAt, ClearPending: progress.ClearPending})
	if err != nil {
		return databaseFailure(err)
	}
	if progress.State == biz.Deleted {
		switch r.Kind {
		case "vlan":
			err = affected(q.RetireVlanSlot(ctx, sqlcgen.RetireVlanSlotParams{ClusterID: r.ClusterID, ResourceID: r.ResourceID}))
		case "public_pool":
			err = affected(q.RetirePublicPoolSlot(ctx, sqlcgen.RetirePublicPoolSlotParams{ClusterID: r.ClusterID, ResourceID: r.ResourceID}))
		}
		if err != nil {
			return err
		}
	}

	if w.ActiveOperation {
		if err = affected(q.CompletePlatformAttempt(ctx, sqlcgen.CompletePlatformAttemptParams{ResourceID: r.ResourceID, OperationID: w.Operation.ID, Epoch: w.Epoch, State: string(progress.OperationState), Reason: string(progress.OperationReason), DelayMicros: progress.NextDelay.Microseconds()})); err != nil {
			return err
		}
	}
	if w.ActiveOperation || w.Resource.State != progress.State || w.Resource.Reason != progress.Reason {
		record := sqlcgen.InsertPlatformHistoryParams{HistoryID: uuid.NewString(), ResourceID: r.ResourceID, Event: "reconciled", ResourceState: r.State, Reason: r.Reason, CreatedAt: r.UpdatedAt}
		if w.ActiveOperation {
			record.OperationID = &w.Operation.ID
			record.OperationState = string(progress.OperationState)
		}
		if err = q.InsertPlatformHistory(ctx, record); err != nil {
			return databaseFailure(err)
		}
	}
	if err = affected(q.ReleasePlatformLease(ctx, sqlcgen.ReleasePlatformLeaseParams{ResourceID: r.ResourceID, Owner: w.Owner, Epoch: w.Epoch, DelayMicros: progress.NextDelay.Microseconds(), Now: now, ClaimedGeneration: w.ClaimedGeneration, Observed: progress.Observed, ObservationError: string(progress.ObservationError), Backoff: progress.Backoff, Park: p.parkResource(w, progress), Terminal: progress.State == biz.Deleted})); err != nil {
		return err
	}
	return databaseFailure(tx.Commit(ctx))
}
