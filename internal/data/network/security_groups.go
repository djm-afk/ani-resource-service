package data

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	db "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
)

func (p *Postgres) UseSecurityGroupObserver(o biz.SecurityGroupConsumerObserver) {
	p.securityGroupObserver = o
}
func sgTenantLock(ctx context.Context, q *db.Queries, tenant string) error {
	return databaseFailure(q.LockSecurityGroupRequest(ctx, db.LockSecurityGroupRequestParams{TenantID: tenant, Action: "tenant-security-groups", RequestKey: "mutation"}))
}
func sgJSON(v any) []byte { b, _ := json.Marshal(v); return b }
func sgRead(row db.NetworkSecurityGroup) (biz.SecurityGroup, error) {
	var v biz.SecurityGroup
	e := json.Unmarshal(row.Payload, &v)
	if e == nil && (v.TenantID != row.TenantID || v.ID != row.SecurityGroupID || v.Version != row.Version || string(v.State) != row.State) {
		e = errors.New("invalid security group snapshot")
	}
	return v, databaseFailure(e)
}
func sgTarget(row db.NetworkSecurityGroupTarget) (biz.SecurityGroupBinding, error) {
	var v biz.SecurityGroupBinding
	e := json.Unmarshal(row.Payload, &v)
	if e == nil && (v.TenantID != row.TenantID || v.TargetType != row.TargetType || v.TargetID != row.TargetID || v.Version != row.Version) {
		e = errors.New("invalid security group target snapshot")
	}
	return v, databaseFailure(e)
}
func sgSaveTarget(ctx context.Context, q *db.Queries, v biz.SecurityGroupBinding) error {
	return q.UpsertSecurityGroupTarget(ctx, db.UpsertSecurityGroupTargetParams{TenantID: v.TenantID, TargetType: v.TargetType, TargetID: v.TargetID, Version: v.Version, Payload: sgJSON(v)})
}
func (p *Postgres) GetSecurityGroup(ctx context.Context, tenant, id string) (biz.SecurityGroup, error) {
	r, e := p.queries.GetSecurityGroupRow(ctx, db.GetSecurityGroupRowParams{TenantID: tenant, SecurityGroupID: id})
	if e != nil {
		return biz.SecurityGroup{}, databaseFailure(e)
	}
	v, e := sgRead(r)
	if e != nil {
		return v, e
	}
	v.BoundResourceCount, e = p.queries.SecurityGroupReferenceCount(ctx, db.SecurityGroupReferenceCountParams{TenantID: tenant, SecurityGroupID: id})
	return v, databaseFailure(e)
}
func (p *Postgres) ListSecurityGroups(ctx context.Context, f biz.SecurityGroupFilter) ([]biz.SecurityGroup, int64, error) {
	tx, e := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if e != nil {
		return nil, 0, databaseFailure(e)
	}
	defer tx.Rollback(ctx)
	q := p.queries.WithTx(tx)
	total, e := q.CountSecurityGroupRows(ctx, db.CountSecurityGroupRowsParams{TenantID: f.TenantID, NameFilter: f.Name, StateFilter: f.State})
	if e != nil {
		return nil, 0, databaseFailure(e)
	}
	rows, e := q.ListSecurityGroupRows(ctx, db.ListSecurityGroupRowsParams{TenantID: f.TenantID, NameFilter: f.Name, StateFilter: f.State, MaxResults: int32(f.PageSize), PageOffset: int64(f.Page-1) * int64(f.PageSize)})
	if e != nil {
		return nil, 0, databaseFailure(e)
	}
	values := make([]biz.SecurityGroup, 0, len(rows))
	for _, r := range rows {
		v, e := sgRead(r)
		if e != nil {
			return nil, 0, e
		}
		v.BoundResourceCount, e = q.SecurityGroupReferenceCount(ctx, db.SecurityGroupReferenceCountParams{TenantID: f.TenantID, SecurityGroupID: v.ID})
		if e != nil {
			return nil, 0, databaseFailure(e)
		}
		values = append(values, v)
	}
	return values, total, databaseFailure(tx.Commit(ctx))
}
func (p *Postgres) ListSecurityGroupBindings(ctx context.Context, f biz.SecurityGroupFilter) ([]biz.SecurityGroupBinding, int64, error) {
	rows, e := p.queries.ListSecurityGroupTargetRows(ctx, db.ListSecurityGroupTargetRowsParams{TenantID: f.TenantID, SecurityGroupID: f.ID, TargetFilter: f.TargetType})
	if e != nil {
		return nil, 0, databaseFailure(e)
	}
	values := make([]biz.SecurityGroupBinding, 0)
	start := int64(f.Page-1) * int64(f.PageSize)
	for k, r := range rows {
		if int64(k) < start || int64(k) >= start+int64(f.PageSize) {
			continue
		}
		v, e := sgTarget(r)
		if e != nil {
			return nil, 0, e
		}
		values = append(values, v)
	}
	return values, int64(len(rows)), nil
}

func sgReplay(ctx context.Context, q *db.Queries, i biz.SecurityGroupIntent) (biz.SecurityGroupMutation, bool, error) {
	if e := q.LockSecurityGroupRequest(ctx, db.LockSecurityGroupRequestParams{TenantID: i.TenantID, Action: i.Action, RequestKey: i.IdempotencyKey}); e != nil {
		return biz.SecurityGroupMutation{}, false, databaseFailure(e)
	}
	r, e := q.GetSecurityGroupReplay(ctx, db.GetSecurityGroupReplayParams{TenantID: i.TenantID, Action: i.Action, IdempotencyKey: i.IdempotencyKey})
	if errors.Is(e, pgx.ErrNoRows) {
		return biz.SecurityGroupMutation{}, false, nil
	}
	if e != nil {
		return biz.SecurityGroupMutation{}, false, databaseFailure(e)
	}
	if r.Fingerprint != contentHash(i) {
		return biz.SecurityGroupMutation{}, false, biz.Fail(biz.IdempotencyConflict, "key accepted another security group intent")
	}
	var v biz.SecurityGroupMutation
	e = json.Unmarshal(r.Response, &v)
	return v, true, databaseFailure(e)
}
func sgAccept(ctx context.Context, q *db.Queries, i biz.SecurityGroupIntent, v biz.SecurityGroupMutation, a biz.Attribution) error {
	if e := q.InsertSecurityGroupOperation(ctx, db.InsertSecurityGroupOperationParams{TenantID: i.TenantID, OperationID: v.Operation.ID, Payload: sgJSON(v.Operation), ActorRef: a.Actor, CallerRef: a.DirectCaller, Intent: sgJSON(i)}); e != nil {
		return e
	}
	return q.InsertSecurityGroupReplay(ctx, db.InsertSecurityGroupReplayParams{TenantID: i.TenantID, Action: i.Action, IdempotencyKey: i.IdempotencyKey, Fingerprint: contentHash(i), Response: sgJSON(v)})
}
func sgNewOperation(tenant, id, kind, target string, now time.Time) biz.Operation {
	return biz.Operation{ID: uuid.NewString(), TenantID: tenant, ResourceID: id, ResourceType: target, Kind: kind, State: biz.Queued, CreatedAt: now, UpdatedAt: now}
}

// Replacing an accepted desired revision must not strand its original operation
// forever. Replay still returns that original identity and honest failed result.
func sgSupersede(ctx context.Context, q *db.Queries, tenant, id string, now time.Time) error {
	if id == "" {
		return nil
	}
	body, e := q.GetSecurityGroupOperation(ctx, db.GetSecurityGroupOperationParams{TenantID: tenant, OperationID: id})
	if e != nil {
		return databaseFailure(e)
	}
	var op biz.Operation
	if e = json.Unmarshal(body, &op); e != nil {
		return databaseFailure(e)
	}
	if op.CompletedAt != nil {
		return nil
	}
	op.State = biz.OpFailed
	op.Reason = "OPERATION_SUPERSEDED"
	op.UpdatedAt = now
	op.CompletedAt = &now
	_, e = q.UpdateSecurityGroupOperation(ctx, db.UpdateSecurityGroupOperationParams{TenantID: tenant, OperationID: id, Payload: sgJSON(op)})
	return databaseFailure(e)
}
func sgNoActiveLease(ctx context.Context, q *db.Queries, tenant, kind, id string) error {
	r, e := q.LockSecurityGroupTask(ctx, db.LockSecurityGroupTaskParams{TenantID: tenant, TargetType: kind, TargetID: id})
	if errors.Is(e, pgx.ErrNoRows) {
		return nil
	}
	if e != nil {
		return databaseFailure(e)
	}
	now, e := q.DatabaseTime(ctx)
	if e != nil {
		return databaseFailure(e)
	}
	if r.LeaseUntil != nil && r.LeaseUntil.After(now) {
		return biz.Fail(biz.ResourceBusy, "security group change is executing; retry with the same key")
	}
	return nil
}

func (p *Postgres) MutateSecurityGroup(ctx context.Context, i biz.SecurityGroupIntent, a biz.Attribution) (biz.SecurityGroupMutation, error) {
	identity, identityErr := p.ResolveTenantNamespace(ctx, i.TenantID)
	if identityErr != nil {
		return biz.SecurityGroupMutation{}, identityErr
	}

	tx, e := p.pool.Begin(ctx)
	if e != nil {
		return biz.SecurityGroupMutation{}, databaseFailure(e)
	}
	defer tx.Rollback(ctx)
	q := p.queries.WithTx(tx)
	if e = sgTenantLock(ctx, q, i.TenantID); e != nil {
		return biz.SecurityGroupMutation{}, e
	}
	if i.Action == "delete" || i.Action == "delete_rule" {
		i.IdempotencyKey = i.ID + ":" + i.RuleID
	}
	if v, found, e := sgReplay(ctx, q, i); e != nil || found {
		return v, e
	}
	now, e := q.DatabaseTime(ctx)
	if e != nil {
		return biz.SecurityGroupMutation{}, databaseFailure(e)
	}
	var v biz.SecurityGroup
	var changedRule biz.SecurityGroupRule
	if i.Action == "create" {
		if e = p.acceptTenantNamespace(ctx, q, identity); e != nil {
			return biz.SecurityGroupMutation{}, e
		}
		ns, e := q.GetTenantNamespace(ctx, db.GetTenantNamespaceParams{TenantID: i.TenantID, ClusterID: p.placement.ClusterID})
		if e != nil {
			return biz.SecurityGroupMutation{}, databaseFailure(e)
		}
		v = biz.SecurityGroup{EgressMetadata: biz.EgressMetadata{ID: newEgressID("sg"), Name: i.Name, Description: i.Description, State: biz.Provisioning, Version: 1, CreatedAt: now, UpdatedAt: now}, TenantID: i.TenantID, ClusterID: p.placement.ClusterID, Namespace: ns, BindingID: uuid.NewString(), Rules: slices.Clone(i.Rules)}
		v.ProviderName = "ani-" + strings.ReplaceAll(v.ID, "_", "-")
		for k := range v.Rules {
			v.Rules[k].ID = newEgressID("sgr")
		}
		v.Rules = append(v.Rules, biz.DefaultSecurityGroupRule())
	} else {
		r, e := q.LockSecurityGroupRow(ctx, db.LockSecurityGroupRowParams{TenantID: i.TenantID, SecurityGroupID: i.ID})
		if e != nil {
			return biz.SecurityGroupMutation{}, databaseFailure(e)
		}
		v, e = sgRead(r)
		if e != nil {
			return biz.SecurityGroupMutation{}, e
		}
		if v.State == biz.Deleted || v.State == biz.Deleting {
			return biz.SecurityGroupMutation{}, biz.Fail(biz.ResourceInUse, "security group is deleting or deleted")
		}
		if e = sgNoActiveLease(ctx, q, i.TenantID, "security_group", i.ID); e != nil {
			return biz.SecurityGroupMutation{}, e
		}
		if i.ExpectedVersion > 0 && i.ExpectedVersion != v.Version {
			return biz.SecurityGroupMutation{}, biz.Fail(biz.VersionConflict, "security group version changed")
		}
		switch i.Action {
		case "update":
			v.Name, v.Description = i.Name, i.Description
		case "create_rule":
			changedRule = i.Rule
			changedRule.ID = newEgressID("sgr")
			v.Rules = append(v.Rules, changedRule)
		case "update_rule", "delete_rule":
			at := slices.IndexFunc(v.Rules, func(r biz.SecurityGroupRule) bool { return r.ID == i.RuleID })
			if at < 0 {
				return biz.SecurityGroupMutation{}, biz.Fail(biz.ResourceNotFound, "security group rule not found")
			}
			if v.Rules[at].IsDefault {
				return biz.SecurityGroupMutation{}, biz.Fail(biz.InvalidArgument, "default ingress rule cannot be changed or removed")
			}
			if i.Action == "delete_rule" {
				v.Rules = slices.Delete(v.Rules, at, at+1)
			} else {
				changedRule = i.Rule
				changedRule.ID = i.RuleID
				v.Rules[at] = changedRule
			}
		case "delete":
			count, e := q.SecurityGroupReferenceCount(ctx, db.SecurityGroupReferenceCountParams{TenantID: i.TenantID, SecurityGroupID: i.ID})
			if e != nil {
				return biz.SecurityGroupMutation{}, databaseFailure(e)
			}
			if count > 0 {
				return biz.SecurityGroupMutation{}, biz.Fail(biz.ResourceInUse, "security group is referenced; unbind consumers and EIPs first")
			}
			v.State = biz.Deleting
		default:
			return biz.SecurityGroupMutation{}, biz.Fail(biz.InvalidArgument, "unsupported security group operation")
		}
		v.Version++
		v.UpdatedAt = now
		if v.State != biz.Deleting {
			v.State = biz.Provisioning
		}
		v.Reason = ""
	}
	if e = biz.ValidateSecurityGroupRules(v.Rules); e != nil {
		return biz.SecurityGroupMutation{}, e
	}
	if i.Action != "create" && i.Action != "delete" {
		targets, e := q.ListSecurityGroupTargetRows(ctx, db.ListSecurityGroupTargetRowsParams{TenantID: i.TenantID, SecurityGroupID: v.ID})
		if e != nil {
			return biz.SecurityGroupMutation{}, databaseFailure(e)
		}
		for _, row := range targets {
			binding, e := sgTarget(row)
			if e != nil {
				return biz.SecurityGroupMutation{}, e
			}
			all := append(slices.Clone(binding.SecurityGroupIDs), binding.AppliedSecurityGroupIDs...)
			all = append(all, v.ID)
			if e = sgCombination(ctx, q, i.TenantID, all, &v); e != nil {
				return biz.SecurityGroupMutation{}, e
			}
		}
	}
	if i.Action != "create" {
		if e = sgSupersede(ctx, q, i.TenantID, v.LastOperationID, now); e != nil {
			return biz.SecurityGroupMutation{}, e
		}
	}
	kind := "update_security_group"
	if i.Action == "create" {
		kind = "create_security_group"
	}
	if i.Action == "delete" {
		kind = "delete_security_group"
	}
	op := sgNewOperation(i.TenantID, v.ID, kind, "security_group", now)
	v.LastOperationID = op.ID
	if i.Action == "create" {
		e = q.InsertSecurityGroupRow(ctx, db.InsertSecurityGroupRowParams{TenantID: i.TenantID, SecurityGroupID: v.ID, Version: v.Version, State: string(v.State), Payload: sgJSON(v)})
	} else {
		e = affected(q.UpdateSecurityGroupRow(ctx, db.UpdateSecurityGroupRowParams{TenantID: i.TenantID, SecurityGroupID: v.ID, ExpectedVersion: v.Version - 1, NewVersion: v.Version, State: string(v.State), Payload: sgJSON(v)}))
	}
	if e != nil {
		return biz.SecurityGroupMutation{}, databaseFailure(e)
	}
	result := biz.SecurityGroupMutation{SecurityGroup: v, Rule: changedRule, Operation: op}
	if e = sgAccept(ctx, q, i, result, a); e != nil {
		return result, databaseFailure(e)
	}
	if e = q.ScheduleSecurityGroup(ctx, db.ScheduleSecurityGroupParams{TenantID: i.TenantID, TargetType: "security_group", TargetID: v.ID, RequestedVersion: v.Version}); e != nil {
		return result, databaseFailure(e)
	}
	return result, databaseFailure(tx.Commit(ctx))
}

func (p *Postgres) ResolveSecurityGroups(ctx context.Context, tenant, vpcID, namespace string, ids []string) ([]string, error) {
	return resolveSecurityGroups(ctx, p.queries, tenant, p.placement.ClusterID, namespace, ids)
}
func resolveSecurityGroups(ctx context.Context, q *db.Queries, tenant, clusterID, namespace string, ids []string) ([]string, error) {
	if _, e := biz.ParseTenant(tenant); e != nil {
		return nil, e
	}
	if e := biz.ValidateSecurityGroupIDs(ids); e != nil {
		return nil, e
	}
	names := make([]string, 0, len(ids))
	ordered := slices.Clone(ids)
	slices.Sort(ordered)
	for _, id := range ordered {
		r, e := q.LockSecurityGroupRow(ctx, db.LockSecurityGroupRowParams{TenantID: tenant, SecurityGroupID: id})
		if e != nil {
			return nil, databaseFailure(e)
		}
		v, e := sgRead(r)
		if e != nil {
			return nil, e
		}
		if v.Namespace != namespace || v.ClusterID != clusterID {
			return nil, biz.Fail(biz.PlacementMismatch, "security group placement differs from consumer")
		}
		if v.State != biz.Available {
			return nil, biz.Fail(biz.NetworkNotReady, "security group is not available")
		}
		names = append(names, v.ProviderName)
	}
	if e := sgCombination(ctx, q, tenant, ordered, nil); e != nil {
		return nil, e
	}
	return names, nil
}
func (p *Postgres) SyncSecurityGroupBinding(ctx context.Context, tenant, kind, id, vpcID, namespace string, version int64, ids []string) ([]string, error) {
	if kind != "container" && kind != "vm" {
		return nil, biz.Fail(biz.InvalidArgument, "unsupported security group consumer")
	}
	tx, e := p.pool.Begin(ctx)
	if e != nil {
		return nil, databaseFailure(e)
	}
	defer tx.Rollback(ctx)
	q := p.queries.WithTx(tx)
	if e = sgTenantLock(ctx, q, tenant); e != nil {
		return nil, e
	}
	if e = q.LockSecurityGroupRequest(ctx, db.LockSecurityGroupRequestParams{TenantID: tenant, Action: kind, RequestKey: id}); e != nil {
		return nil, databaseFailure(e)
	}
	old, e := q.LockSecurityGroupTarget(ctx, db.LockSecurityGroupTargetParams{TenantID: tenant, TargetType: kind, TargetID: id})
	if e != nil && !errors.Is(e, pgx.ErrNoRows) {
		return nil, databaseFailure(e)
	}
	v := biz.SecurityGroupBinding{TenantID: tenant, TargetType: kind, TargetID: id, VPCID: vpcID, Namespace: namespace, Version: version, State: "applying", SecurityGroupIDs: slices.Clone(ids)}
	if e == nil {
		previous, e := sgTarget(old)
		if e != nil {
			return nil, e
		}
		if previous.Version > version || (previous.State == "released" && previous.Version >= version) {
			return nil, biz.Fail(biz.VersionConflict, "consumer security group intent is retired")
		}
		if previous.Version == version && !sameStrings(previous.SecurityGroupIDs, ids) {
			return nil, biz.Fail(biz.VersionConflict, "consumer version already binds another security group intent")
		}
		v.AppliedSecurityGroupIDs = previous.AppliedSecurityGroupIDs
	}
	names, e := resolveSecurityGroups(ctx, q, tenant, p.placement.ClusterID, namespace, ids)
	if e != nil {
		return nil, e
	}
	if e = sgSaveTarget(ctx, q, v); e != nil {
		return nil, databaseFailure(e)
	}
	for _, sg := range ids {
		if e = q.ReserveSecurityGroupReference(ctx, db.ReserveSecurityGroupReferenceParams{TenantID: tenant, TargetType: kind, TargetID: id, SecurityGroupID: sg}); e != nil {
			return nil, databaseFailure(e)
		}
	}
	return names, databaseFailure(tx.Commit(ctx))
}
func sameStrings(a, b []string) bool {
	a = slices.Clone(a)
	b = slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}
func (p *Postgres) ConfirmSecurityGroupBinding(ctx context.Context, tenant, kind, id string, version int64, ids []string) error {
	return p.confirmSecurityGroupBinding(ctx, tenant, kind, id, version, ids, false)
}
func (p *Postgres) ReleaseSecurityGroupBinding(ctx context.Context, tenant, kind, id string, version int64) error {
	return p.confirmSecurityGroupBinding(ctx, tenant, kind, id, version, nil, true)
}
func (p *Postgres) confirmSecurityGroupBinding(ctx context.Context, tenant, kind, id string, version int64, ids []string, release bool) error {
	tx, e := p.pool.Begin(ctx)
	if e != nil {
		return databaseFailure(e)
	}
	defer tx.Rollback(ctx)
	q := p.queries.WithTx(tx)
	if e = q.LockSecurityGroupRequest(ctx, db.LockSecurityGroupRequestParams{TenantID: tenant, Action: kind, RequestKey: id}); e != nil {
		return databaseFailure(e)
	}
	row, e := q.LockSecurityGroupTarget(ctx, db.LockSecurityGroupTargetParams{TenantID: tenant, TargetType: kind, TargetID: id})
	if errors.Is(e, pgx.ErrNoRows) && release {
		v := biz.SecurityGroupBinding{TenantID: tenant, TargetType: kind, TargetID: id, Version: version, State: "released"}
		if e = sgSaveTarget(ctx, q, v); e != nil {
			return databaseFailure(e)
		}
		return databaseFailure(tx.Commit(ctx))
	}
	if e != nil {
		return databaseFailure(e)
	}
	v, e := sgTarget(row)
	if e != nil {
		return e
	}
	if (!release && (v.Version != version || !sameStrings(v.SecurityGroupIDs, ids))) || (release && v.Version > version) {
		return biz.Fail(biz.VersionConflict, "consumer security group intent changed")
	}
	applied := false
	if !release {
		if p.securityGroupObserver == nil {
			return biz.Fail(biz.DependencyUnavailable, "security group provider observation is unavailable")
		}
		observeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		var observeErr error
		applied, observeErr = p.securityGroupObserver.ObserveSecurityGroupConsumer(observeCtx, v)
		cancel()
		if observeErr != nil {
			return observeErr
		}
	}
	v.Version = version
	v.SecurityGroupIDs = slices.Clone(ids)
	v.AppliedSecurityGroupIDs = nil
	if applied {
		v.AppliedSecurityGroupIDs = slices.Clone(ids)
	}
	now, e := q.DatabaseTime(ctx)
	if e != nil {
		return databaseFailure(e)
	}
	v.ObservedAt = &now
	v.State = "configured"
	if applied {
		v.State = "applied"
	}
	if release {
		v.State = "released"
	}
	if e = sgSaveTarget(ctx, q, v); e != nil {
		return databaseFailure(e)
	}
	if e = q.ReleaseSecurityGroupReferences(ctx, db.ReleaseSecurityGroupReferencesParams{TenantID: tenant, TargetType: kind, TargetID: id, KeepIds: ids}); e != nil {
		return databaseFailure(e)
	}
	return databaseFailure(tx.Commit(ctx))
}

// Opposite actions at one priority are ambiguous when multiple SGs share the
// same OVN tier. Reject that combination on bind and on later rule changes.
func sgCombination(ctx context.Context, q *db.Queries, tenant string, ids []string, replacement *biz.SecurityGroup) error {
	actions := map[string]string{}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		var group biz.SecurityGroup
		if replacement != nil && replacement.ID == id {
			group = *replacement
		} else {
			row, e := q.GetSecurityGroupRow(ctx, db.GetSecurityGroupRowParams{TenantID: tenant, SecurityGroupID: id})
			if e != nil {
				return databaseFailure(e)
			}
			var e2 error
			group, e2 = sgRead(row)
			if e2 != nil {
				return e2
			}
		}
		for _, r := range group.Rules {
			key := fmt.Sprintf("%s/%d", r.Direction, r.Priority)
			if old, ok := actions[key]; ok && old != r.Action {
				return biz.Fail(biz.InvalidArgument, "security groups contain opposite actions at the same direction and priority")
			}
			actions[key] = r.Action
		}
	}
	return nil
}
