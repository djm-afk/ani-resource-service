package data

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
	"strings"
	"time"
)

type DefaultVPCEgressPostgres struct {
	base     *Postgres
	identity biz.DefaultVPCIdentity
}

func NewDefaultVPCEgressRepository(p *Postgres, i biz.DefaultVPCIdentity) (*DefaultVPCEgressPostgres, error) {
	if err := i.Validate(); err != nil {
		return nil, err
	}
	if p == nil || i.ClusterID != p.placement.ClusterID {
		return nil, biz.Fail(biz.InvalidArgument, "fixed default VPC and matching placement are required")
	}
	return &DefaultVPCEgressPostgres{base: p, identity: i}, nil
}
func defaultEgressSnapshot(r sqlcgen.NetworkDefaultVpcEgress, operationState string) biz.DefaultVPCEgress {
	v := biz.DefaultVPCEgress{EgressMetadata: biz.EgressMetadata{ID: r.ResourceID, Name: r.Name, State: biz.ResourceState(r.State), Reason: biz.Reason(r.Reason), Version: r.Version, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, ObservedAt: r.ObservedAt, ObservationStale: r.ObservedAt == nil || r.ObservationError != "", LastOperationID: r.LastOperationID}, PoolID: r.PoolID, Phase: r.Phase, OperationState: operationState, Address: r.Address, EIPID: r.EipID,
		Identity:        biz.DefaultVPCIdentity{ClusterID: r.ClusterID, ClusterUID: r.ClusterUid, Namespace: r.DefaultVpcNamespace, Name: r.DefaultVpcName, UID: r.DefaultVpcUid},
		EIPProviderName: r.EipProviderName, EIPProviderUID: r.EipProviderUid, SnatProviderName: r.SnatProviderName, SnatProviderUID: r.SnatProviderUid, PendingAction: r.PendingAction, PendingUnknown: r.PendingUnknown, DesiredEnabled: true}
	if r.AppliedEnabled.Valid {
		enabled := r.AppliedEnabled.Bool
		v.AppliedEnabled = &enabled
	}
	return v
}
func defaultEgressFingerprint(i biz.DefaultVPCEgressIntent) string {
	return contentHash(struct{ Name, PoolID string }{i.Name, i.PoolID})
}
func (p *DefaultVPCEgressPostgres) replay(ctx context.Context, q *sqlcgen.Queries, i biz.DefaultVPCEgressIntent) (biz.DefaultVPCEgress, bool, error) {
	r, err := q.GetDefaultEgressReplay(ctx, sqlcgen.GetDefaultEgressReplayParams{ClusterID: p.identity.ClusterID, IdempotencyKey: i.IdempotencyKey})
	if errors.Is(err, pgx.ErrNoRows) {
		return biz.DefaultVPCEgress{}, false, nil
	}
	if err != nil {
		return biz.DefaultVPCEgress{}, false, databaseFailure(err)
	}
	if r.Fingerprint != defaultEgressFingerprint(i) {
		return biz.DefaultVPCEgress{}, true, biz.Fail(biz.IdempotencyConflict, "key already accepted another default VPC egress")
	}
	var v biz.DefaultVPCEgress
	if err = json.Unmarshal(r.Response, &v); err != nil {
		return v, true, databaseFailure(err)
	}
	if v.ID != r.ResourceID || v.LastOperationID != r.OperationID {
		return v, true, biz.Fail(biz.DependencyUnavailable, "invalid platform egress acceptance snapshot")
	}
	return v, true, nil
}
func (p *DefaultVPCEgressPostgres) CreateDefaultVPCEgress(ctx context.Context, i biz.DefaultVPCEgressIntent, a biz.Attribution) (biz.DefaultVPCEgress, error) {
	// Exact accepted keys replay before current external prerequisites are read.
	if v, found, err := p.replay(ctx, p.base.queries, i); found || err != nil {
		return v, err
	}

	tx, err := p.base.pool.Begin(ctx)
	if err != nil {
		return biz.DefaultVPCEgress{}, databaseFailure(err)
	}
	defer tx.Rollback(ctx)
	q := p.base.queries.WithTx(tx)
	if err = q.LockPlatformCluster(ctx, sqlcgen.LockPlatformClusterParams{ClusterID: p.identity.ClusterID}); err != nil {
		return biz.DefaultVPCEgress{}, databaseFailure(err)
	}
	if err = q.LockDefaultEgressKey(ctx, sqlcgen.LockDefaultEgressKeyParams{ClusterID: p.identity.ClusterID, IdempotencyKey: i.IdempotencyKey}); err != nil {
		return biz.DefaultVPCEgress{}, databaseFailure(err)
	}
	if v, found, err := p.replay(ctx, q, i); found || err != nil {
		return v, err
	}
	pool, err := q.LockPublicPool(ctx, sqlcgen.LockPublicPoolParams{ClusterID: p.identity.ClusterID, ResourceID: i.PoolID})
	if err != nil {
		return biz.DefaultVPCEgress{}, databaseFailure(err)
	}
	now, err := q.DatabaseTime(ctx)
	if err != nil {
		return biz.DefaultVPCEgress{}, databaseFailure(err)
	}
	if pool.Scope != "public" {
		return biz.DefaultVPCEgress{}, biz.Fail(biz.ResourceNotFound, "public address pool not found")
	}
	if err = p.base.poolAllocationAllowed(ctx, q, pool, false); err != nil {
		return biz.DefaultVPCEgress{}, err
	}
	id, op := newEgressID("dve"), uuid.NewString()
	suffix := strings.TrimPrefix(id, "dve_")
	err = q.InsertDefaultEgress(ctx, sqlcgen.InsertDefaultEgressParams{ResourceID: id, EipID: newEgressID("eip"), ClusterID: p.identity.ClusterID, ClusterUid: p.identity.ClusterUID, DefaultVpcNamespace: p.identity.Namespace, DefaultVpcName: p.identity.Name, DefaultVpcUid: p.identity.UID, Name: i.Name, PoolID: i.PoolID, PoolRevision: pool.ConfigRevision, CreatedAt: now, LastOperationID: op, EipProviderName: "ani-default-eip-" + suffix, EipBindingID: uuid.NewString(), SnatProviderName: "ani-default-snat-" + suffix, SnatBindingID: uuid.NewString()})
	if err != nil {
		return biz.DefaultVPCEgress{}, switchAdmissionFailure(err)
	}
	if err = q.InsertDefaultEgressOperation(ctx, sqlcgen.InsertDefaultEgressOperationParams{OperationID: op, ResourceID: id, Kind: "create_default_vpc_egress", CreatedAt: now}); err != nil {
		return biz.DefaultVPCEgress{}, databaseFailure(err)
	}
	r, err := q.GetDefaultEgress(ctx, sqlcgen.GetDefaultEgressParams{ClusterID: p.identity.ClusterID, ResourceID: id})
	if err != nil {
		return biz.DefaultVPCEgress{}, databaseFailure(err)
	}
	v := defaultEgressSnapshot(r.NetworkDefaultVpcEgress, r.OperationState)
	body, err := json.Marshal(v)
	if err != nil {
		return v, err
	}
	if err = q.InsertDefaultEgressReplay(ctx, sqlcgen.InsertDefaultEgressReplayParams{ClusterID: p.identity.ClusterID, IdempotencyKey: i.IdempotencyKey, Fingerprint: defaultEgressFingerprint(i), ResourceID: id, OperationID: op, Response: body, CreatedAt: now}); err != nil {
		return v, databaseFailure(err)
	}
	if err = q.InsertDefaultEgressHistory(ctx, sqlcgen.InsertDefaultEgressHistoryParams{HistoryID: uuid.NewString(), ResourceID: id, OperationID: op, Event: "create_accepted", ActorRef: a.Actor, CallerRef: a.DirectCaller, Detail: body}); err != nil {
		return v, databaseFailure(err)
	}
	return v, databaseFailure(tx.Commit(ctx))
}
func (p *DefaultVPCEgressPostgres) GetDefaultVPCEgress(ctx context.Context, id string) (biz.DefaultVPCEgress, error) {
	r, err := p.base.queries.GetDefaultEgress(ctx, sqlcgen.GetDefaultEgressParams{ClusterID: p.identity.ClusterID, ResourceID: id})
	if err != nil {
		return biz.DefaultVPCEgress{}, databaseFailure(err)
	}
	return p.observedDefaultEgress(ctx, r.NetworkDefaultVpcEgress, r.OperationState)
}
func (p *DefaultVPCEgressPostgres) observedDefaultEgress(ctx context.Context, row sqlcgen.NetworkDefaultVpcEgress, state string) (biz.DefaultVPCEgress, error) {
	return defaultEgressSnapshot(row, state), nil
}
func (p *DefaultVPCEgressPostgres) GetCurrentDefaultVPCEgress(ctx context.Context) (biz.DefaultVPCEgress, error) {
	r, err := p.base.queries.GetCurrentDefaultEgress(ctx, sqlcgen.GetCurrentDefaultEgressParams{
		ClusterID: p.identity.ClusterID, DefaultVpcNamespace: p.identity.Namespace, DefaultVpcName: p.identity.Name,
	})
	if err != nil {
		return biz.DefaultVPCEgress{}, databaseFailure(err)
	}
	return p.observedDefaultEgress(ctx, r.NetworkDefaultVpcEgress, r.OperationState)
}
func (p *DefaultVPCEgressPostgres) DeleteDefaultVPCEgress(ctx context.Context, id string, a biz.Attribution) (biz.DefaultVPCEgress, error) {
	tx, err := p.base.pool.Begin(ctx)
	if err != nil {
		return biz.DefaultVPCEgress{}, databaseFailure(err)
	}
	defer tx.Rollback(ctx)
	q := p.base.queries.WithTx(tx)
	if err = q.LockPlatformCluster(ctx, sqlcgen.LockPlatformClusterParams{ClusterID: p.identity.ClusterID}); err != nil {
		return biz.DefaultVPCEgress{}, databaseFailure(err)
	}
	r, err := q.LockDefaultEgress(ctx, sqlcgen.LockDefaultEgressParams{ClusterID: p.identity.ClusterID, ResourceID: id})
	if err != nil {
		return biz.DefaultVPCEgress{}, databaseFailure(err)
	}
	row := r.NetworkDefaultVpcEgress
	if row.State == "deleted" || row.State == "deleting" {
		return defaultEgressSnapshot(row, r.OperationState), nil
	}
	now, err := q.DatabaseTime(ctx)
	if err != nil {
		return biz.DefaultVPCEgress{}, databaseFailure(err)
	}
	op := uuid.NewString()
	if err = q.SupersedeDefaultEgressOperation(ctx, sqlcgen.SupersedeDefaultEgressOperationParams{ResourceID: id, OperationID: row.LastOperationID}); err != nil {
		return biz.DefaultVPCEgress{}, databaseFailure(err)
	}
	if err = q.InsertDefaultEgressOperation(ctx, sqlcgen.InsertDefaultEgressOperationParams{OperationID: op, ResourceID: id, Kind: "delete_default_vpc_egress", CreatedAt: now}); err != nil {
		return biz.DefaultVPCEgress{}, databaseFailure(err)
	}
	changed, err := q.AdmitDefaultEgressDelete(ctx, sqlcgen.AdmitDefaultEgressDeleteParams{ClusterID: p.identity.ClusterID, ResourceID: id, Version: row.Version, OperationID: op})
	if err = affected(changed, err); err != nil {
		return biz.DefaultVPCEgress{}, err
	}
	if err = q.InsertDefaultEgressHistory(ctx, sqlcgen.InsertDefaultEgressHistoryParams{HistoryID: uuid.NewString(), ResourceID: id, OperationID: op, Event: "delete_accepted", ActorRef: a.Actor, CallerRef: a.DirectCaller, Detail: []byte(`{}`)}); err != nil {
		return biz.DefaultVPCEgress{}, databaseFailure(err)
	}
	v, err := q.GetDefaultEgress(ctx, sqlcgen.GetDefaultEgressParams{ClusterID: p.identity.ClusterID, ResourceID: id})
	if err != nil {
		return biz.DefaultVPCEgress{}, databaseFailure(err)
	}
	return defaultEgressSnapshot(v.NetworkDefaultVpcEgress, v.OperationState), databaseFailure(tx.Commit(ctx))
}

func (p *DefaultVPCEgressPostgres) ClaimDefaultVPCEgress(ctx context.Context, owner string, duration time.Duration) (biz.DefaultEgressWork, bool, error) {
	tx, err := p.base.pool.Begin(ctx)
	if err != nil {
		return biz.DefaultEgressWork{}, false, databaseFailure(err)
	}
	defer tx.Rollback(ctx)
	q := p.base.queries.WithTx(tx)
	r, err := q.DueDefaultEgress(ctx, sqlcgen.DueDefaultEgressParams{ClusterID: p.identity.ClusterID})
	if errors.Is(err, pgx.ErrNoRows) {
		return biz.DefaultEgressWork{}, false, nil
	}
	if err != nil {
		return biz.DefaultEgressWork{}, false, databaseFailure(err)
	}
	epoch, err := q.ClaimDefaultEgressLease(ctx, sqlcgen.ClaimDefaultEgressLeaseParams{Owner: owner, LeaseSeconds: int64(duration / time.Second), ClusterID: p.identity.ClusterID, ResourceID: r.NetworkDefaultVpcEgress.ResourceID})
	if err != nil {
		return biz.DefaultEgressWork{}, false, databaseFailure(err)
	}
	now, err := q.DatabaseTime(ctx)
	if err != nil {
		return biz.DefaultEgressWork{}, false, databaseFailure(err)
	}
	x := biz.DefaultEgressWork{Egress: defaultEgressSnapshot(r.NetworkDefaultVpcEgress, r.OperationState), Owner: owner, Epoch: epoch.LeaseEpoch, ClaimedGeneration: epoch.RequestedGeneration, PoolRevision: r.NetworkDefaultVpcEgress.PoolRevision, EIPBindingID: r.NetworkDefaultVpcEgress.EipBindingID, SnatBindingID: r.NetworkDefaultVpcEgress.SnatBindingID, Now: now}
	return x, true, databaseFailure(tx.Commit(ctx))
}
func (p *DefaultVPCEgressPostgres) AssertDefaultVPCEgressLease(ctx context.Context, x biz.DefaultEgressWork) error {
	ok, err := p.base.queries.AssertDefaultEgressLease(ctx, sqlcgen.AssertDefaultEgressLeaseParams{ClusterID: p.identity.ClusterID, ResourceID: x.Egress.ID, Version: x.Egress.Version, OperationID: x.Egress.LastOperationID, Owner: x.Owner, Epoch: x.Epoch})
	if err != nil {
		return databaseFailure(err)
	}
	if !ok {
		return biz.ErrLeaseLost
	}
	return nil
}
func (p *DefaultVPCEgressPostgres) BeginDefaultVPCEgressMutation(ctx context.Context, x biz.DefaultEgressWork, action string) error {
	n, err := p.base.queries.BeginDefaultEgressMutation(ctx, sqlcgen.BeginDefaultEgressMutationParams{Action: action, ClusterID: p.identity.ClusterID, ResourceID: x.Egress.ID, Version: x.Egress.Version, OperationID: x.Egress.LastOperationID, Owner: x.Owner, Epoch: x.Epoch})
	if err != nil {
		return databaseFailure(err)
	}
	if n != 1 {
		return biz.ErrLeaseLost
	}
	return nil
}
func (p *DefaultVPCEgressPostgres) FinishDefaultVPCEgress(ctx context.Context, x biz.DefaultEgressWork, v biz.DefaultEgressProgress) error {
	tx, err := p.base.pool.Begin(ctx)
	if err != nil {
		return databaseFailure(err)
	}
	defer tx.Rollback(ctx)
	q := p.base.queries.WithTx(tx)
	now, err := q.DatabaseTime(ctx)
	if err != nil {
		return databaseFailure(err)
	}
	o := v.Observation
	if !v.Observed {
		o.EIPUID, o.SnatUID, o.Address, o.AppliedEnabled = x.Egress.EIPProviderUID, x.Egress.SnatProviderUID, x.Egress.Address, x.Egress.AppliedEnabled
	}
	var applied pgtype.Bool
	if o.AppliedEnabled != nil {
		applied = pgtype.Bool{Bool: *o.AppliedEnabled, Valid: true}
	}
	terminal := v.State == biz.Deleted
	active := x.Egress.OperationState != "succeeded" && x.Egress.OperationState != "failed"
	completed := !active || v.OperationState == biz.Succeeded || v.OperationState == biz.OpFailed
	park := completed && (x.Egress.PendingAction == "" || v.ClearPending) && !v.PendingUnknown && v.ObservationError == ""
	n, err := q.FinishDefaultEgress(ctx, sqlcgen.FinishDefaultEgressParams{State: string(v.State), Phase: v.Phase, Reason: string(v.Reason), EipUid: o.EIPUID, SnatUid: o.SnatUID, Address: o.Address, AppliedEnabled: applied, Observed: v.Observed, Now: now, ObservationError: string(v.ObservationError), ClaimedGeneration: x.ClaimedGeneration, Backoff: v.Backoff, Park: park, Terminal: terminal, ClearPending: v.ClearPending, PendingUnknown: v.PendingUnknown, DelayMs: v.Delay.Milliseconds(), ClusterID: p.identity.ClusterID, ResourceID: x.Egress.ID, Version: x.Egress.Version, OperationID: x.Egress.LastOperationID, Owner: x.Owner, Epoch: x.Epoch})
	if err != nil {
		return databaseFailure(err)
	}
	if n != 1 {
		return biz.ErrLeaseLost
	}
	if active && v.OperationState != "" {
		if err = q.FinishDefaultEgressOperation(ctx, sqlcgen.FinishDefaultEgressOperationParams{State: string(v.OperationState), Reason: string(v.OperationReason), ResourceID: x.Egress.ID, OperationID: x.Egress.LastOperationID}); err != nil {
			return databaseFailure(err)
		}
	}
	if active || v.State != x.Egress.State || v.Reason != x.Egress.Reason {
		body, _ := json.Marshal(struct {
			State  biz.ResourceState
			Phase  string
			Reason biz.Reason
		}{v.State, v.Phase, v.Reason})
		if err = q.InsertDefaultEgressHistory(ctx, sqlcgen.InsertDefaultEgressHistoryParams{HistoryID: uuid.NewString(), ResourceID: x.Egress.ID, OperationID: x.Egress.LastOperationID, Event: "worker_progress", Detail: body}); err != nil {
			return databaseFailure(err)
		}
	}
	return databaseFailure(tx.Commit(ctx))
}
