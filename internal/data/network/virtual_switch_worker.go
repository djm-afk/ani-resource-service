package data

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/telemetry"
)

func (p *Postgres) ClaimVirtualSwitch(ctx context.Context, owner string, duration time.Duration) (biz.VirtualSwitchWork, bool, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return biz.VirtualSwitchWork{}, false, databaseFailure(err)
	}
	defer tx.Rollback(ctx)
	q := p.queries.WithTx(tx)
	r, err := q.ClaimVirtualSwitch(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return biz.VirtualSwitchWork{}, false, nil
	}
	if err != nil {
		return biz.VirtualSwitchWork{}, false, databaseFailure(err)
	}
	l, err := q.LeaseVirtualSwitch(ctx, sqlcgen.LeaseVirtualSwitchParams{ResourceID: r.NetworkVirtualSwitch.ResourceID, Owner: owner, LeaseMicros: duration.Microseconds()})
	if errors.Is(err, pgx.ErrNoRows) {
		return biz.VirtualSwitchWork{}, false, nil
	}
	if err != nil {
		return biz.VirtualSwitchWork{}, false, databaseFailure(err)
	}
	v, err := switchSnapshot(r.NetworkVirtualSwitch)
	if err != nil {
		return biz.VirtualSwitchWork{}, false, err
	}
	now, err := q.DatabaseTime(ctx)
	if err != nil {
		return biz.VirtualSwitchWork{}, false, databaseFailure(err)
	}
	w := biz.VirtualSwitchWork{Switch: v, Owner: owner, Epoch: l.LeaseEpoch, ClaimedGeneration: l.RequestedGeneration, Now: now}
	// A NULL operation projection means the last operation already completed, so
	// this pass is an independent observation and must not reuse its trace.
	if r.OperationID != nil {
		w.ActiveOperation = true
		w.OperationKind = r.OperationKind
	}
	// Logging metadata decodes independently of the business claim.
	if tc, te := telemetry.Decode(r.OperationTelemetryContext); te != nil {
		w.TelemetryContextError = te
	} else {
		w.TelemetryContext = tc
	}
	return w, true, databaseFailure(tx.Commit(ctx))
}

func lockSwitchWork(ctx context.Context, q *sqlcgen.Queries, w biz.VirtualSwitchWork) (sqlcgen.NetworkVirtualSwitch, error) {
	r, err := q.LockVirtualSwitch(ctx, sqlcgen.LockVirtualSwitchParams{ResourceID: w.Switch.ID})
	if err != nil {
		return r, databaseFailure(err)
	}
	if r.Version != w.Switch.Version || textValue(r.BindingID) != w.Switch.BindingID {
		return r, biz.ErrLeaseLost
	}
	_, err = q.CheckVirtualSwitchLease(ctx, sqlcgen.CheckVirtualSwitchLeaseParams{ResourceID: r.ResourceID, Owner: w.Owner, Epoch: w.Epoch})
	if errors.Is(err, pgx.ErrNoRows) {
		return r, biz.ErrLeaseLost
	}
	return r, databaseFailure(err)
}

func (p *Postgres) SaveVirtualSwitchNode(ctx context.Context, w biz.VirtualSwitchWork, index int, node biz.VirtualSwitchNode) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return databaseFailure(err)
	}
	defer tx.Rollback(ctx)
	q := p.queries.WithTx(tx)
	r, err := lockSwitchWork(ctx, q, w)
	if err != nil {
		return err
	}
	var nodes []biz.VirtualSwitchNode
	if err = json.Unmarshal(r.Nodes, &nodes); err != nil {
		return databaseFailure(err)
	}
	if index < 0 || index >= len(nodes) || nodes[index].NodeUID != node.NodeUID || nodes[index].NodeName != node.NodeName || nodes[index].Priority != node.Priority || nodes[index].Allocated != node.Allocated {
		return biz.ErrLeaseLost
	}
	old := nodes[index]
	if old.DeleteFenced && !node.DeleteFenced {
		return biz.ErrLeaseLost
	}
	if old.Prepared && (!node.Prepared || old.OriginalPresent != node.OriginalPresent || old.OriginalValue != node.OriginalValue || old.OriginalOwner != node.OriginalOwner) {
		return biz.ErrLeaseLost
	}
	nodes[index] = node
	body, err := json.Marshal(nodes)
	if err != nil {
		return err
	}
	if err = affected(q.SaveVirtualSwitchNodes(ctx, sqlcgen.SaveVirtualSwitchNodesParams{ResourceID: r.ResourceID, Version: r.Version, BindingID: r.BindingID, Nodes: body})); err != nil {
		return err
	}
	return databaseFailure(tx.Commit(ctx))
}

func (p *Postgres) FinishVirtualSwitch(ctx context.Context, w biz.VirtualSwitchWork, progress biz.VirtualSwitchProgress, delay time.Duration) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return databaseFailure(err)
	}
	defer tx.Rollback(ctx)
	q := p.queries.WithTx(tx)
	r, err := lockSwitchWork(ctx, q, w)
	if err != nil {
		return err
	}
	if progress.BindingState == "cleaned" {
		blocked, err := q.VirtualSwitchDependencies(ctx, sqlcgen.VirtualSwitchDependenciesParams{ResourceID: r.ResourceID})
		if err != nil {
			return databaseFailure(err)
		}
		if blocked > 0 {
			return biz.ErrLeaseLost
		}
		if _, err = q.RetireSwitchDevices(ctx, sqlcgen.RetireSwitchDevicesParams{VirtualSwitchID: &r.ResourceID}); err != nil {
			return databaseFailure(err)
		}
		if err = q.CompleteVirtualSwitchDeletion(ctx, sqlcgen.CompleteVirtualSwitchDeletionParams{ResourceID: r.ResourceID}); err != nil {
			return databaseFailure(err)
		}
	}
	if err = affected(q.AdvanceVirtualSwitch(ctx, sqlcgen.AdvanceVirtualSwitchParams{ResourceID: r.ResourceID, Version: r.Version, BindingState: progress.BindingState, Reason: string(progress.Reason)})); err != nil {
		return err
	}
	now, err := q.DatabaseTime(ctx)
	if err != nil {
		return databaseFailure(err)
	}
	terminal := progress.BindingState == "cleaned"
	park := !progress.Backoff && progress.ObservationError == "" && progress.OperationReason == "" && (progress.BindingState == "configured" || r.BindingID == nil)

	if err = affected(q.ReleaseVirtualSwitchLease(ctx, sqlcgen.ReleaseVirtualSwitchLeaseParams{ResourceID: r.ResourceID, Owner: w.Owner, Epoch: w.Epoch, DelayMicros: delay.Microseconds(), Now: now, ClaimedGeneration: w.ClaimedGeneration, ObservationError: string(progress.ObservationError), Observed: progress.Observed, Backoff: progress.Backoff, Park: park, Terminal: terminal})); err != nil {
		return err
	}
	return databaseFailure(tx.Commit(ctx))
}
