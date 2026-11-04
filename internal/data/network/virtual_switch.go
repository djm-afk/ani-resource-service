package data

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
)

func switchAdmissionFailure(err error) error {
	var domain *biz.Error
	if errors.As(err, &domain) {
		return err
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" {
		return biz.Fail(biz.ResourceInUse, "name, device or VLAN is already reserved")
	}
	var provider *biz.ProviderError
	if errors.As(err, &provider) {
		if provider.Kind == biz.ProviderConflict || provider.Kind == biz.ProviderInUse {
			return biz.Fail(biz.ResourceInUse, "switch or device ownership conflicts with current facts")
		}
		return biz.Fail(biz.DependencyUnavailable, "node metadata is unavailable")
	}
	return databaseFailure(err)
}

func switchSnapshot(r sqlcgen.NetworkVirtualSwitch) (biz.VirtualSwitch, error) {
	v := biz.VirtualSwitch{EgressMetadata: biz.EgressMetadata{ID: r.ResourceID, Name: r.Name, State: biz.ResourceState(r.State), Reason: biz.Reason(r.Reason), Version: r.Version, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, LastOperationID: r.LastOperationID}, ClusterID: r.ClusterID, GroupName: r.Name, BindingID: textValue(r.BindingID), BindingVersion: r.BindingVersion, BindingState: r.BindingState, MembershipFingerprint: r.MembershipFingerprint}
	if err := json.Unmarshal(r.Nodes, &v.Nodes); err != nil {
		return v, databaseFailure(err)
	}
	return v, nil
}

func switchReplay(ctx context.Context, q *sqlcgen.Queries, i biz.VirtualSwitchIntent) (biz.VirtualSwitch, bool, error) {
	r, err := q.GetVirtualSwitchReplay(ctx, sqlcgen.GetVirtualSwitchReplayParams{IdempotencyKey: i.IdempotencyKey})
	if errors.Is(err, pgx.ErrNoRows) {
		return biz.VirtualSwitch{}, false, nil
	}
	if err != nil {
		return biz.VirtualSwitch{}, false, databaseFailure(err)
	}
	if r.Fingerprint != contentHash(struct{ Name string }{i.Name}) {
		return biz.VirtualSwitch{}, false, biz.Fail(biz.IdempotencyConflict, "key already accepted another switch")
	}
	var v biz.VirtualSwitch
	if err = json.Unmarshal(r.Response, &v); err != nil {
		return v, false, databaseFailure(err)
	}
	if v.ID != r.ResourceID || v.LastOperationID != r.OperationID {
		return v, false, biz.Fail(biz.DependencyUnavailable, "invalid switch acceptance snapshot")
	}
	return v, true, nil
}

func (p *Postgres) CreateVirtualSwitch(ctx context.Context, i biz.VirtualSwitchIntent, a biz.Attribution) (biz.VirtualSwitch, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return biz.VirtualSwitch{}, databaseFailure(err)
	}
	defer tx.Rollback(ctx)
	q := p.queries.WithTx(tx)
	if err = q.LockVirtualSwitchKey(ctx, sqlcgen.LockVirtualSwitchKeyParams{IdempotencyKey: i.IdempotencyKey}); err != nil {
		return biz.VirtualSwitch{}, databaseFailure(err)
	}
	if v, found, err := switchReplay(ctx, q, i); found || err != nil {
		return v, err
	}
	now, err := q.DatabaseTime(ctx)
	if err != nil {
		return biz.VirtualSwitch{}, databaseFailure(err)
	}
	id, op := newEgressID("vsw"), uuid.NewString()
	r, err := q.InsertVirtualSwitch(ctx, sqlcgen.InsertVirtualSwitchParams{ResourceID: id, Name: i.Name, OperationID: op, CreatedAt: now})
	if err != nil {
		return biz.VirtualSwitch{}, switchAdmissionFailure(err)
	}
	if err = q.InsertVirtualSwitchOperation(ctx, sqlcgen.InsertVirtualSwitchOperationParams{ResourceID: id, OperationID: op, Kind: "create_virtual_switch", State: "succeeded", CreatedAt: now, CompletedAt: &now}); err != nil {
		return biz.VirtualSwitch{}, databaseFailure(err)
	}
	v, err := switchSnapshot(r)
	if err != nil {
		return v, err
	}
	body, err := json.Marshal(v)
	if err != nil {
		return v, err
	}
	if err = q.InsertVirtualSwitchReplay(ctx, sqlcgen.InsertVirtualSwitchReplayParams{IdempotencyKey: i.IdempotencyKey, Fingerprint: contentHash(struct{ Name string }{i.Name}), ResourceID: id, OperationID: op, Response: body, CreatedAt: now}); err != nil {
		return v, databaseFailure(err)
	}
	if err = q.InsertVirtualSwitchHistory(ctx, sqlcgen.InsertVirtualSwitchHistoryParams{HistoryID: uuid.NewString(), ResourceID: id, OperationID: &op, Event: "created", ActorRef: a.Actor, CallerRef: a.DirectCaller, Detail: []byte(`{}`)}); err != nil {
		return v, databaseFailure(err)
	}
	return v, databaseFailure(tx.Commit(ctx))
}

func (p *Postgres) GetVirtualSwitch(ctx context.Context, id string) (biz.VirtualSwitch, error) {
	r, err := p.readQueries(ctx).GetVirtualSwitch(ctx, sqlcgen.GetVirtualSwitchParams{ResourceID: id})
	if err != nil {
		return biz.VirtualSwitch{}, databaseFailure(err)
	}
	v, err := switchSnapshot(r.NetworkVirtualSwitch)
	v.ObservationStale = r.ObservationError != ""
	return v, err
}

func (p *Postgres) ListVirtualSwitches(ctx context.Context, f biz.VirtualSwitchFilter) ([]biz.VirtualSwitch, int64, error) {
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, 0, databaseFailure(err)
	}
	defer tx.Rollback(ctx)
	q := p.queries.WithTx(tx)
	total, err := q.CountVirtualSwitches(ctx, sqlcgen.CountVirtualSwitchesParams{NameFilter: f.Name, StateFilter: f.State})
	if err != nil {
		return nil, 0, databaseFailure(err)
	}
	rows, err := q.ListVirtualSwitches(ctx, sqlcgen.ListVirtualSwitchesParams{NameFilter: f.Name, StateFilter: f.State, MaxResults: f.PageSize, PageOffset: int64(f.Page-1) * int64(f.PageSize)})
	if err != nil {
		return nil, 0, databaseFailure(err)
	}
	values := make([]biz.VirtualSwitch, 0, len(rows))
	for _, r := range rows {
		v, err := switchSnapshot(r.NetworkVirtualSwitch)
		if err != nil {
			return nil, 0, err
		}
		v.ObservationStale = r.ObservationError != ""
		values = append(values, v)
	}
	return values, total, nil
}

func (p *Postgres) DeleteVirtualSwitch(ctx context.Context, id string, a biz.Attribution) (biz.VirtualSwitch, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return biz.VirtualSwitch{}, databaseFailure(err)
	}
	defer tx.Rollback(ctx)
	q := p.queries.WithTx(tx)
	r, err := q.LockVirtualSwitch(ctx, sqlcgen.LockVirtualSwitchParams{ResourceID: id})
	if err != nil {
		return biz.VirtualSwitch{}, databaseFailure(err)
	}
	if r.State == "deleted" || r.State == "deleting" {
		return switchSnapshot(r)
	}
	blocked, err := q.VirtualSwitchDependencies(ctx, sqlcgen.VirtualSwitchDependenciesParams{ResourceID: id})
	if err != nil {
		return biz.VirtualSwitch{}, databaseFailure(err)
	}
	if blocked > 0 {
		return biz.VirtualSwitch{}, biz.Fail(biz.ResourceInUse, "switch still has active or unresolved references")
	}
	now, err := q.DatabaseTime(ctx)
	if err != nil {
		return biz.VirtualSwitch{}, databaseFailure(err)
	}
	op := uuid.NewString()
	state, bindingState, opState := "deleting", "cleaning", "queued"
	var completed *time.Time
	if r.BindingID == nil {
		state, bindingState, opState = "deleted", "cleaned", "succeeded"
		completed = &now
	}
	if err = q.InsertVirtualSwitchOperation(ctx, sqlcgen.InsertVirtualSwitchOperationParams{ResourceID: id, OperationID: op, Kind: "delete_virtual_switch", State: opState, CreatedAt: now, CompletedAt: completed}); err != nil {
		return biz.VirtualSwitch{}, databaseFailure(err)
	}
	r, err = q.DeleteVirtualSwitchMetadata(ctx, sqlcgen.DeleteVirtualSwitchMetadataParams{ResourceID: id, Version: r.Version, OperationID: op, State: state, BindingState: bindingState})
	if err != nil {
		return biz.VirtualSwitch{}, databaseFailure(err)
	}
	if completed == nil {
		if err = q.ScheduleVirtualSwitch(ctx, sqlcgen.ScheduleVirtualSwitchParams{ResourceID: id}); err != nil {
			return biz.VirtualSwitch{}, databaseFailure(err)
		}
	}
	if err = q.InsertVirtualSwitchHistory(ctx, sqlcgen.InsertVirtualSwitchHistoryParams{HistoryID: uuid.NewString(), ResourceID: id, OperationID: &op, Event: "delete_accepted", ActorRef: a.Actor, CallerRef: a.DirectCaller, Detail: []byte(`{}`)}); err != nil {
		return biz.VirtualSwitch{}, databaseFailure(err)
	}
	v, err := switchSnapshot(r)
	if err != nil {
		return v, err
	}
	return v, databaseFailure(tx.Commit(ctx))
}
