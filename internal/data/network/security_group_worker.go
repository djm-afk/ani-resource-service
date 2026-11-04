package data

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	db "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
)

func (p *Postgres) GetSecurityGroupOperation(ctx context.Context, tenant, id string) (biz.Operation, error) {
	b, e := p.queries.GetSecurityGroupOperation(ctx, db.GetSecurityGroupOperationParams{TenantID: tenant, OperationID: id})
	if e != nil {
		return biz.Operation{}, databaseFailure(e)
	}
	var v biz.Operation
	e = json.Unmarshal(b, &v)
	return v, databaseFailure(e)
}
func (p *Postgres) GetEIPSecurityGroups(ctx context.Context, tenant, id string) (biz.SecurityGroupBinding, error) {
	eip, e := p.GetEIP(ctx, tenant, id)
	if e != nil {
		return biz.SecurityGroupBinding{}, e
	}
	if eip.Scope != "public" || eip.ManagedBy != "tenant" || eip.State == biz.Deleting || eip.State == biz.Deleted {
		return biz.SecurityGroupBinding{}, biz.Fail(biz.ResourceNotFound, "tenant EIP not found")
	}
	r, e := p.queries.GetSecurityGroupTarget(ctx, db.GetSecurityGroupTargetParams{TenantID: tenant, TargetType: "eip", TargetID: id})
	if errors.Is(e, pgx.ErrNoRows) {
		return biz.SecurityGroupBinding{TenantID: tenant, TargetType: "eip", TargetID: id, Version: 1, State: "unbound", SecurityGroupIDs: []string{}, AppliedSecurityGroupIDs: []string{}}, nil
	}
	if e != nil {
		return biz.SecurityGroupBinding{}, databaseFailure(e)
	}
	return sgTarget(r)
}
func (p *Postgres) SetEIPSecurityGroups(ctx context.Context, i biz.SecurityGroupIntent, a biz.Attribution) (biz.SecurityGroupMutation, error) {
	tx, e := p.pool.Begin(ctx)
	if e != nil {
		return biz.SecurityGroupMutation{}, databaseFailure(e)
	}
	defer tx.Rollback(ctx)
	q := p.queries.WithTx(tx)
	if e = sgTenantLock(ctx, q, i.TenantID); e != nil {
		return biz.SecurityGroupMutation{}, e
	}
	if v, found, e := sgReplay(ctx, q, i); e != nil || found {
		return v, e
	}
	// The EIP row lock serializes lifecycle acceptance and SG changes.
	row, e := q.LockEIP(ctx, db.LockEIPParams{TenantID: i.TenantID, EipID: i.ID})
	if e != nil {
		return biz.SecurityGroupMutation{}, databaseFailure(e)
	}
	if row.Scope != "public" || row.ManagedBy != "tenant" || row.State == "deleting" || row.State == "deleted" {
		return biz.SecurityGroupMutation{}, biz.Fail(biz.ResourceNotFound, "tenant EIP not found")
	}
	if e = sgNoActiveLease(ctx, q, i.TenantID, "eip", i.ID); e != nil {
		return biz.SecurityGroupMutation{}, e
	}
	target, e := q.LockSecurityGroupTarget(ctx, db.LockSecurityGroupTargetParams{TenantID: i.TenantID, TargetType: "eip", TargetID: i.ID})
	v := biz.SecurityGroupBinding{TenantID: i.TenantID, TargetType: "eip", TargetID: i.ID, Namespace: row.Namespace, Version: 1}
	if e == nil {
		v, e = sgTarget(target)
	} else if errors.Is(e, pgx.ErrNoRows) {
		e = nil
	}
	if e != nil {
		return biz.SecurityGroupMutation{}, databaseFailure(e)
	}
	if v.Version != i.ExpectedVersion {
		return biz.SecurityGroupMutation{}, biz.Fail(biz.VersionConflict, "EIP security group configuration changed")
	}
	if _, e = resolveSecurityGroups(ctx, q, i.TenantID, p.placement.ClusterID, row.Namespace, i.SecurityGroupIDs); e != nil {
		return biz.SecurityGroupMutation{}, e
	}
	now, e := q.DatabaseTime(ctx)
	if e != nil {
		return biz.SecurityGroupMutation{}, databaseFailure(e)
	}
	if e = sgSupersede(ctx, q, i.TenantID, v.LastOperationID, now); e != nil {
		return biz.SecurityGroupMutation{}, e
	}
	op := sgNewOperation(i.TenantID, i.ID, "set_eip_security_groups", "eip", now)
	v.Version++
	v.SecurityGroupIDs = slices.Clone(i.SecurityGroupIDs)
	v.State = "applying"
	v.Reason = ""
	v.LastOperationID = op.ID
	if e = sgSaveTarget(ctx, q, v); e != nil {
		return biz.SecurityGroupMutation{}, databaseFailure(e)
	}
	for _, id := range i.SecurityGroupIDs {
		if e = q.ReserveSecurityGroupReference(ctx, db.ReserveSecurityGroupReferenceParams{TenantID: i.TenantID, TargetType: "eip", TargetID: i.ID, SecurityGroupID: id}); e != nil {
			return biz.SecurityGroupMutation{}, databaseFailure(e)
		}
	}
	if e = q.ReleaseSecurityGroupReferences(ctx, db.ReleaseSecurityGroupReferencesParams{TenantID: i.TenantID, TargetType: "eip", TargetID: i.ID, KeepIds: i.SecurityGroupIDs}); e != nil {
		return biz.SecurityGroupMutation{}, databaseFailure(e)
	}
	result := biz.SecurityGroupMutation{Binding: v, Operation: op}
	if e = sgAccept(ctx, q, i, result, a); e != nil {
		return result, databaseFailure(e)
	}
	if e = q.ScheduleSecurityGroup(ctx, db.ScheduleSecurityGroupParams{TenantID: i.TenantID, TargetType: "eip", TargetID: i.ID, RequestedVersion: v.Version}); e != nil {
		return result, databaseFailure(e)
	}
	return result, databaseFailure(tx.Commit(ctx))
}
func (p *Postgres) ClaimSecurityGroup(ctx context.Context, owner string, duration time.Duration) (biz.SecurityGroupWork, bool, error) {
	tx, e := p.pool.Begin(ctx)
	if e != nil {
		return biz.SecurityGroupWork{}, false, databaseFailure(e)
	}
	defer tx.Rollback(ctx)
	q := p.queries.WithTx(tx)
	r, e := q.ClaimSecurityGroupTask(ctx)
	if errors.Is(e, pgx.ErrNoRows) {
		return biz.SecurityGroupWork{}, false, nil
	}
	if e != nil {
		return biz.SecurityGroupWork{}, false, databaseFailure(e)
	}
	task, e := q.LeaseSecurityGroupTask(ctx, db.LeaseSecurityGroupTaskParams{TenantID: r.TenantID, TargetType: r.TargetType, TargetID: r.TargetID, Owner: owner, LeaseMicros: duration.Microseconds()})
	if e != nil {
		return biz.SecurityGroupWork{}, false, databaseFailure(e)
	}
	w := biz.SecurityGroupWork{TenantID: r.TenantID, TargetType: r.TargetType, TargetID: r.TargetID, Version: task.RequestedVersion, Epoch: task.LeaseEpoch, Owner: owner, ClaimedGeneration: task.RequestedGeneration}
	var op string
	if r.TargetType == "security_group" {
		row, e := q.GetSecurityGroupRow(ctx, db.GetSecurityGroupRowParams{TenantID: r.TenantID, SecurityGroupID: r.TargetID})
		if e != nil {
			return w, false, databaseFailure(e)
		}
		w.Group, e = sgRead(row)
		if e != nil {
			return w, false, e
		}
		op = w.Group.LastOperationID
	} else {
		row, e := q.GetSecurityGroupTarget(ctx, db.GetSecurityGroupTargetParams{TenantID: r.TenantID, TargetType: r.TargetType, TargetID: r.TargetID})
		if e != nil {
			return w, false, databaseFailure(e)
		}
		w.Binding, e = sgTarget(row)
		if e != nil {
			return w, false, e
		}
		op = w.Binding.LastOperationID
	}
	if op != "" {
		body, err := q.GetSecurityGroupOperation(ctx, db.GetSecurityGroupOperationParams{TenantID: r.TenantID, OperationID: op})
		if err != nil {
			return w, false, databaseFailure(err)
		}
		if err = json.Unmarshal(body, &w.Operation); err != nil {
			return w, false, databaseFailure(err)
		}
	}
	return w, true, databaseFailure(tx.Commit(ctx))
}
func (p *Postgres) FinishSecurityGroup(ctx context.Context, work biz.SecurityGroupWork, result biz.SecurityGroupResult, applyErr error) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return databaseFailure(err)
	}
	defer tx.Rollback(ctx)
	q := p.queries.WithTx(tx)
	// Management versions and the execution lease fence concurrent submissions.
	if work.TargetType == "security_group" {
		row, err := q.LockSecurityGroupRow(ctx, db.LockSecurityGroupRowParams{TenantID: work.TenantID, SecurityGroupID: work.TargetID})
		if err != nil {
			return databaseFailure(err)
		}
		if row.Version != work.Version {
			return biz.ErrLeaseLost
		}
	} else {
		row, err := q.LockSecurityGroupTarget(ctx, db.LockSecurityGroupTargetParams{TenantID: work.TenantID, TargetType: work.TargetType, TargetID: work.TargetID})
		if err != nil {
			return databaseFailure(err)
		}
		if row.Version != work.Version {
			return biz.ErrLeaseLost
		}
	}
	now, err := q.DatabaseTime(ctx)
	if err != nil {
		return databaseFailure(err)
	}
	task, err := q.LockSecurityGroupTask(ctx, db.LockSecurityGroupTaskParams{TenantID: work.TenantID, TargetType: work.TargetType, TargetID: work.TargetID})
	if err != nil {
		return databaseFailure(err)
	}
	if task.LeaseOwner == nil || *task.LeaseOwner != work.Owner || task.LeaseEpoch != work.Epoch || task.RequestedVersion != work.Version || task.LeaseUntil == nil || !task.LeaseUntil.After(now) {
		return biz.ErrLeaseLost
	}
	op := work.Operation
	activeOperation := securityGroupOperationActive(work)
	if activeOperation {
		op.UpdatedAt, op.Reason = now, result.OperationReason
		op.State = biz.Running
		op.NextAttemptAt = nil
		if applyErr != nil {
			op.State = biz.Retrying
			var provider *biz.ProviderError
			if errors.As(applyErr, &provider) {
				switch provider.Kind {
				case biz.ProviderConflict, biz.ProviderInUse:
					op.State = biz.Blocked
				case biz.ProviderReject:
					// A rejected management write finishes this operation, while
					// the resource keeps the projection of its successful read.
					if work.TargetType == "security_group" && work.Group.State == biz.Deleting {
						op.State = biz.Blocked
					} else if result.ObservationError == "" {
						op.State, op.CompletedAt = biz.OpFailed, &now
					}
				}
			}
		} else if result.Complete {
			op.State, op.Reason, op.CompletedAt = biz.Succeeded, "", &now
		} else if result.OperationReason == biz.ProviderMissing {
			op.State = biz.Blocked
		}
		if op.CompletedAt == nil {
			next := now.Add(result.NextDelay)
			op.NextAttemptAt = &next
		}
		if err = affected(q.UpdateSecurityGroupOperation(ctx, db.UpdateSecurityGroupOperationParams{TenantID: work.TenantID, OperationID: op.ID, Payload: sgJSON(op)})); err != nil {
			return err
		}
	}
	// A failed read has no replacement projection. Local configuration/release
	// results need no provider read and leave the last observed timestamp alone.
	project := result.Observed || applyErr == nil
	terminal := false
	if work.TargetType == "security_group" {
		value := work.Group
		if project {
			if result.ProviderUID != "" {
				value.ProviderUID = result.ProviderUID
			}
			if result.State != "" {
				value.State = biz.ResourceState(result.State)
			}
			value.Reason, value.UpdatedAt = biz.Reason(result.Reason), now
			if result.Observed {
				value.ObservedAt = &now
			}
			if err = affected(q.UpdateSecurityGroupRow(ctx, db.UpdateSecurityGroupRowParams{TenantID: work.TenantID, SecurityGroupID: work.TargetID, ExpectedVersion: work.Version, NewVersion: work.Version, State: string(value.State), Payload: sgJSON(value)})); err != nil {
				return err
			}
		}
		terminal = value.State == biz.Deleted
	} else {
		value := work.Binding
		if project {
			if result.State != "" {
				value.State = result.State
			}
			value.Reason = result.Reason
			value.AppliedSecurityGroupIDs = slices.Clone(result.AppliedIDs)
			if result.Observed {
				value.ObservedAt = &now
			}
			if err = sgSaveTarget(ctx, q, value); err != nil {
				return databaseFailure(err)
			}
		}
		terminal = value.State == "released" || value.State == "deleted"
		keepIDs := value.SecurityGroupIDs
		if terminal {
			keepIDs = nil
		}
		if err = q.ReleaseSecurityGroupReferences(ctx, db.ReleaseSecurityGroupReferencesParams{TenantID: work.TenantID, TargetType: work.TargetType, TargetID: work.TargetID, KeepIds: keepIDs}); err != nil {
			return databaseFailure(err)
		}
	}
	unfinishedOperation := activeOperation && op.CompletedAt == nil
	park := terminal || applyErr == nil && !result.Backoff && !unfinishedOperation && (result.Observed || result.Complete)
	if err = affected(q.FinishSecurityGroupTask(ctx, db.FinishSecurityGroupTaskParams{
		TenantID: work.TenantID, TargetType: work.TargetType, TargetID: work.TargetID, Version: work.Version,
		Owner: work.Owner, Epoch: work.Epoch, ClaimedGeneration: work.ClaimedGeneration,
		Observed: result.Observed, ObservationError: string(result.ObservationError),
		DelayMicros: result.NextDelay.Microseconds(), Backoff: result.Backoff, Park: park, Terminal: terminal, Now: now,
	})); err != nil {
		return err
	}
	return databaseFailure(tx.Commit(ctx))
}
