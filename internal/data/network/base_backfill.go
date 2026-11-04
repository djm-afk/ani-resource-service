package data

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
)

// BaseBackfillSnapshot is immutable review input. Runtime results are kept
// separately so pausing, admission or worker progress cannot change its digest.
type BaseBackfillSnapshot struct {
	TenantID      string `json:"tenant_id"`
	VPCID         string `json:"vpc_id"`
	VPCVersion    int64  `json:"vpc_version"`
	Namespace     string `json:"namespace"`
	BindingID     string `json:"binding_id"`
	ProviderName  string `json:"provider_name"`
	ProviderUID   string `json:"provider_uid"`
	InitialState  string `json:"initial_state"`
	InitialReason string `json:"initial_reason"`
}

type BaseBackfillCandidate struct {
	Snapshot       BaseBackfillSnapshot `json:"snapshot"`
	State          string               `json:"state"`
	Reason         string               `json:"reason"`
	OperationID    string               `json:"operation_id,omitempty"`
	OperationState string               `json:"operation_state,omitempty"`
	BaseState      string               `json:"base_state"`
}

type BaseBackfillPlan struct {
	RunID           string                  `json:"run_id"`
	ClusterID       string                  `json:"cluster_id"`
	PoolID          string                  `json:"pool_id"`
	PoolRevision    int64                   `json:"pool_revision"`
	IntervalMS      int64                   `json:"interval_ms"`
	SHA256          string                  `json:"plan_sha256"`
	Paused          bool                    `json:"paused"`
	ReviewedAt      *time.Time              `json:"reviewed_at,omitempty"`
	CreatedAt       time.Time               `json:"created_at"`
	NextAdmissionAt time.Time               `json:"next_admission_at"`
	Candidates      []BaseBackfillCandidate `json:"candidates"`
}

type BaseBackfillPlanInput struct {
	RunID    string
	PoolID   string // Empty chooses the default exactly once; replay uses the snapshot.
	Interval time.Duration
}

func validBaseBackfillRun(id string) error {
	parsed, err := uuid.Parse(id)
	if err != nil || parsed == uuid.Nil || parsed.String() != id {
		return biz.Fail(biz.InvalidArgument, "a canonical nonzero backfill run UUID is required")
	}
	return nil
}

func baseBackfillDigest(plan BaseBackfillPlan) (string, error) {
	snapshot := struct {
		Version      int                    `json:"format_version"`
		RunID        string                 `json:"run_id"`
		ClusterID    string                 `json:"cluster_id"`
		PoolID       string                 `json:"pool_id"`
		PoolRevision int64                  `json:"pool_revision"`
		IntervalMS   int64                  `json:"interval_ms"`
		Candidates   []BaseBackfillSnapshot `json:"candidates"`
	}{Version: 1, RunID: plan.RunID, ClusterID: plan.ClusterID, PoolID: plan.PoolID, PoolRevision: plan.PoolRevision, IntervalMS: plan.IntervalMS, Candidates: []BaseBackfillSnapshot{}}
	for _, c := range plan.Candidates {
		snapshot.Candidates = append(snapshot.Candidates, c.Snapshot)
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

// PlanBaseBackfill writes only a paused review plan. No operation, resource
// allocation or Kubernetes mutation occurs. Run ID makes lost responses replayable.
func (p *Postgres) PlanBaseBackfill(ctx context.Context, in BaseBackfillPlanInput) (BaseBackfillPlan, error) {
	if err := validBaseBackfillRun(in.RunID); err != nil {
		return BaseBackfillPlan{}, err
	}
	if in.Interval < 100*time.Millisecond || in.Interval > time.Hour || in.Interval%time.Millisecond != 0 {
		return BaseBackfillPlan{}, biz.Fail(biz.InvalidArgument, "admission interval must be whole milliseconds within 100ms..1h")
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return BaseBackfillPlan{}, databaseFailure(err)
	}
	defer tx.Rollback(ctx)
	q := p.queries.WithTx(tx)
	if err = q.LockBaseBackfillPlanKey(ctx, sqlcgen.LockBaseBackfillPlanKeyParams{RunID: in.RunID}); err != nil {
		return BaseBackfillPlan{}, databaseFailure(err)
	}
	previous, err := q.GetBaseBackfillRun(ctx, sqlcgen.GetBaseBackfillRunParams{RunID: in.RunID, ClusterID: p.placement.ClusterID})
	if err == nil {
		if previous.IntervalMs != in.Interval.Milliseconds() || (in.PoolID != "" && previous.PoolID != in.PoolID) {
			return BaseBackfillPlan{}, biz.Fail(biz.IdempotencyConflict, "run ID already fixes a different pool or admission interval")
		}
		return p.baseBackfillStatus(ctx, q, previous)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return BaseBackfillPlan{}, databaseFailure(err)
	}
	poolID := in.PoolID
	if poolID == "" {
		def, e := q.GetDefaultIntranetPool(ctx, sqlcgen.GetDefaultIntranetPoolParams{ClusterID: p.placement.ClusterID})
		if e != nil {
			return BaseBackfillPlan{}, databaseFailure(e)
		}
		poolID = def.PoolID
	}
	pool, err := q.GetPublicPool(ctx, sqlcgen.GetPublicPoolParams{ClusterID: p.placement.ClusterID, ResourceID: poolID})
	if err != nil {
		return BaseBackfillPlan{}, databaseFailure(err)
	}
	if pool.Scope != "intranet" {
		return BaseBackfillPlan{}, biz.Fail(biz.InvalidArgument, "backfill requires an Intranet pool")
	}
	rows, err := q.ListBaseBackfillInventory(ctx, sqlcgen.ListBaseBackfillInventoryParams{ClusterID: p.placement.ClusterID, MaxResults: 10001})
	if err != nil {
		return BaseBackfillPlan{}, databaseFailure(err)
	}
	if len(rows) > 10000 {
		return BaseBackfillPlan{}, biz.Fail(biz.ResourceBusy, "review inventory exceeds the 10000 VPC safety bound; no partial plan was saved")
	}
	plan := BaseBackfillPlan{RunID: in.RunID, ClusterID: p.placement.ClusterID, PoolID: poolID, PoolRevision: pool.ConfigRevision, IntervalMS: in.Interval.Milliseconds(), Paused: true, Candidates: []BaseBackfillCandidate{}}
	for _, row := range rows {
		state, reason := "pending", ""
		switch {
		case row.State == "deleting" || row.State == "deleted":
			state, reason = "excluded", "VPC_DELETING_OR_DELETED"
		case row.HasBase:
			state, reason = "excluded", "BASE_CONNECTIVITY_ALREADY_MANAGED"
		case row.BindingID == nil || row.Namespace == nil || row.ProviderName == "":
			state, reason = "conflict", "PROVIDER_BINDING_MISSING"
		case row.OperationState != "succeeded" && row.OperationState != "failed":
			state, reason = "conflict", "VPC_OPERATION_NOT_COMPLETED"
		}
		snapshot := BaseBackfillSnapshot{TenantID: row.TenantID, VPCID: row.VpcID, VPCVersion: row.Version, Namespace: textValue(row.Namespace), BindingID: textValue(row.BindingID), ProviderName: row.ProviderName, ProviderUID: row.ProviderUid, InitialState: state, InitialReason: reason}
		plan.Candidates = append(plan.Candidates, BaseBackfillCandidate{Snapshot: snapshot, State: state, Reason: reason})
	}
	plan.SHA256, err = baseBackfillDigest(plan)
	if err != nil {
		return BaseBackfillPlan{}, err
	}
	if err = q.InsertBaseBackfillRun(ctx, sqlcgen.InsertBaseBackfillRunParams{RunID: plan.RunID, ClusterID: plan.ClusterID, PoolID: plan.PoolID, PoolRevision: plan.PoolRevision, IntervalMs: plan.IntervalMS, PlanSha256: plan.SHA256}); err != nil {
		return BaseBackfillPlan{}, databaseFailure(err)
	}
	for i, c := range plan.Candidates {
		s := c.Snapshot
		if err = q.InsertBaseBackfillCandidate(ctx, sqlcgen.InsertBaseBackfillCandidateParams{TenantID: s.TenantID, RunID: plan.RunID, ClusterID: plan.ClusterID, VpcID: s.VPCID, AcceptedVpcVersion: s.VPCVersion, Namespace: rows[i].Namespace, BindingID: rows[i].BindingID, ProviderName: s.ProviderName, ProviderUid: s.ProviderUID, InitialState: s.InitialState, InitialReason: s.InitialReason}); err != nil {
			return BaseBackfillPlan{}, databaseFailure(err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return BaseBackfillPlan{}, databaseFailure(err)
	}
	return p.GetBaseBackfill(ctx, in.RunID)
}

func (p *Postgres) baseBackfillStatus(ctx context.Context, q *sqlcgen.Queries, run sqlcgen.NetworkBaseBackfillRun) (BaseBackfillPlan, error) {
	rows, err := q.ListBaseBackfillCandidates(ctx, sqlcgen.ListBaseBackfillCandidatesParams{RunID: run.RunID, ClusterID: run.ClusterID})
	if err != nil {
		return BaseBackfillPlan{}, databaseFailure(err)
	}
	plan := BaseBackfillPlan{RunID: run.RunID, ClusterID: run.ClusterID, PoolID: run.PoolID, PoolRevision: run.PoolRevision, IntervalMS: run.IntervalMs, SHA256: run.PlanSha256, Paused: run.Paused, ReviewedAt: run.ReviewedAt, CreatedAt: run.CreatedAt, NextAdmissionAt: run.NextAdmissionAt, Candidates: []BaseBackfillCandidate{}}
	for _, c := range rows {
		plan.Candidates = append(plan.Candidates, BaseBackfillCandidate{Snapshot: BaseBackfillSnapshot{TenantID: c.TenantID, VPCID: c.VpcID, VPCVersion: c.AcceptedVpcVersion, Namespace: textValue(c.Namespace), BindingID: textValue(c.BindingID), ProviderName: c.ProviderName, ProviderUID: c.ProviderUid, InitialState: c.InitialState, InitialReason: c.InitialReason}, State: c.State, Reason: c.Reason, OperationID: textValue(c.OperationID), OperationState: c.OperationState, BaseState: c.BaseState})
	}
	digest, err := baseBackfillDigest(plan)
	if err != nil {
		return BaseBackfillPlan{}, err
	}
	if digest != plan.SHA256 {
		return BaseBackfillPlan{}, biz.Fail(biz.IdempotencyConflict, "persisted review snapshot does not match its digest")
	}
	return plan, nil
}

// GetBaseBackfill is a pure database read. It never wakes work or repairs state.
func (p *Postgres) GetBaseBackfill(ctx context.Context, runID string) (BaseBackfillPlan, error) {
	if err := validBaseBackfillRun(runID); err != nil {
		return BaseBackfillPlan{}, err
	}
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return BaseBackfillPlan{}, databaseFailure(err)
	}
	defer tx.Rollback(ctx)
	q := p.queries.WithTx(tx)
	run, err := q.GetBaseBackfillRun(ctx, sqlcgen.GetBaseBackfillRunParams{RunID: runID, ClusterID: p.placement.ClusterID})
	if err != nil {
		return BaseBackfillPlan{}, databaseFailure(err)
	}
	return p.baseBackfillStatus(ctx, q, run)
}

// SetBaseBackfillPaused resumes only the exact persisted reviewed snapshot.
// Pausing stops future admission; accepted operations continue on the shared worker.
func (p *Postgres) SetBaseBackfillPaused(ctx context.Context, runID string, paused bool, reviewedSHA string) (BaseBackfillPlan, error) {
	if err := validBaseBackfillRun(runID); err != nil {
		return BaseBackfillPlan{}, err
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return BaseBackfillPlan{}, databaseFailure(err)
	}
	defer tx.Rollback(ctx)
	q := p.queries.WithTx(tx)
	run, err := q.LockBaseBackfillRun(ctx, sqlcgen.LockBaseBackfillRunParams{RunID: runID, ClusterID: p.placement.ClusterID})
	if err != nil {
		return BaseBackfillPlan{}, databaseFailure(err)
	}
	if _, err = p.baseBackfillStatus(ctx, q, run); err != nil {
		return BaseBackfillPlan{}, err
	}
	if !paused && reviewedSHA != run.PlanSha256 {
		return BaseBackfillPlan{}, biz.Fail(biz.IdempotencyConflict, "resume requires the exact reviewed plan SHA-256")
	}
	if err = affected(q.SetBaseBackfillPaused(ctx, sqlcgen.SetBaseBackfillPausedParams{RunID: runID, ClusterID: p.placement.ClusterID, Paused: paused, ReviewedSha256: reviewedSHA})); err != nil {
		return BaseBackfillPlan{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return BaseBackfillPlan{}, databaseFailure(err)
	}
	return p.GetBaseBackfill(ctx, runID)
}

type BaseBackfillAdmission struct {
	RunID       string `json:"run_id"`
	State       string `json:"state"`
	TenantID    string `json:"tenant_id,omitempty"`
	VPCID       string `json:"vpc_id,omitempty"`
	OperationID string `json:"operation_id,omitempty"`
	Reason      string `json:"reason,omitempty"`
	WaitMS      int64  `json:"wait_ms,omitempty"`
}

// AdmitNextBaseBackfill performs at most one admission and no Provider IO. A
// durable run-row lock and database time enforce a shared rate across dispatchers.
func (p *Postgres) AdmitNextBaseBackfill(ctx context.Context, runID string) (BaseBackfillAdmission, error) {
	out := BaseBackfillAdmission{RunID: runID}
	if err := validBaseBackfillRun(runID); err != nil {
		return out, err
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return out, databaseFailure(err)
	}
	defer tx.Rollback(ctx)
	q := p.queries.WithTx(tx)
	run, err := q.LockBaseBackfillRun(ctx, sqlcgen.LockBaseBackfillRunParams{RunID: runID, ClusterID: p.placement.ClusterID})
	if err != nil {
		return out, databaseFailure(err)
	}
	if run.Paused {
		out.State = "paused"
		return out, nil
	}
	if run.ReviewedAt == nil {
		return out, biz.Fail(biz.PermissionDenied, "backfill plan has not been reviewed")
	}
	// Verify immutable review input even when a caller bypasses the CLI status step.
	if _, err = p.baseBackfillStatus(ctx, q, run); err != nil {
		return out, err
	}
	c, err := q.NextBaseBackfillCandidate(ctx, sqlcgen.NextBaseBackfillCandidateParams{RunID: runID, ClusterID: p.placement.ClusterID})
	if errors.Is(err, pgx.ErrNoRows) {
		out.State = "admission_complete"
		return out, nil
	}
	if err != nil {
		return out, databaseFailure(err)
	}
	now, err := q.DatabaseTime(ctx)
	if err != nil {
		return out, databaseFailure(err)
	}
	if now.Before(run.NextAdmissionAt) {
		out.State = "waiting"
		out.WaitMS = (run.NextAdmissionAt.Sub(now) + time.Millisecond - 1).Milliseconds()
		return out, nil
	}
	out.TenantID, out.VPCID = c.TenantID, c.VpcID
	// Isolate partial acceptance so a failed prerequisite cannot leave an orphan
	// operation, but its blocked result and automatic pause can still be committed.
	attempt, err := tx.Begin(ctx)
	if err != nil {
		return out, databaseFailure(err)
	}
	opID, acceptErr := p.acceptBaseBackfillCandidate(ctx, p.queries.WithTx(attempt), run, c, now)
	candidateState, reason := "accepted", ""
	blocked := false
	if acceptErr != nil {
		if err = attempt.Rollback(ctx); err != nil {
			return out, databaseFailure(err)
		}
		reason = string(biz.ReasonOf(acceptErr))
		if reason == "" {
			return out, acceptErr
		}
		candidateState = "conflict"
		if biz.ReasonOf(acceptErr) == biz.BaseConnectivityNotReady || biz.ReasonOf(acceptErr) == biz.DependencyUnavailable {
			candidateState = "pending"
			blocked = true
		}
		out.State = candidateState
		if blocked {
			out.State = "blocked"
		}
		out.Reason = reason
	} else {
		if err = attempt.Commit(ctx); err != nil {
			return out, databaseFailure(err)
		}
		out.State = "accepted"
		out.OperationID = opID
	}
	var operationID *string
	if opID != "" && acceptErr == nil {
		operationID = &opID
	}
	if err = affected(q.SetBaseBackfillCandidateResult(ctx, sqlcgen.SetBaseBackfillCandidateResultParams{TenantID: c.TenantID, RunID: runID, VpcID: c.VpcID, State: candidateState, Reason: reason, OperationID: operationID})); err != nil {
		return out, err
	}
	if err = affected(q.AdvanceBaseBackfillAdmission(ctx, sqlcgen.AdvanceBaseBackfillAdmissionParams{RunID: runID, ClusterID: p.placement.ClusterID, Blocked: blocked})); err != nil {
		return out, err
	}
	if err = tx.Commit(ctx); err != nil {
		return out, databaseFailure(err)
	}
	return out, nil
}

func (p *Postgres) acceptBaseBackfillCandidate(ctx context.Context, q *sqlcgen.Queries, run sqlcgen.NetworkBaseBackfillRun, c sqlcgen.NetworkBaseBackfillCandidate, now time.Time) (string, error) {
	conflict := func(message string) (string, error) { return "", biz.Fail(biz.ResourceBusy, message) }
	v, err := q.LockVPC(ctx, sqlcgen.LockVPCParams{TenantID: c.TenantID, VpcID: c.VpcID})
	if err != nil {
		return "", databaseFailure(err)
	}
	// Observations legitimately increment VPC version while a plan is paused.
	// The immutable review identity is rechecked below; use the current locked
	// version for the update, without invalidating review for a pure observation.
	if v.BaseConnectivityRequired || v.State == "deleting" || v.State == "deleted" {
		return conflict("VPC is no longer eligible or deletion closed admission")
	}
	binding, err := q.GetBinding(ctx, sqlcgen.GetBindingParams{TenantID: c.TenantID, ResourceID: c.VpcID})
	if err != nil {
		return "", databaseFailure(err)
	}
	if binding.ClusterID != run.ClusterID || binding.Namespace != textValue(c.Namespace) || binding.BindingID != textValue(c.BindingID) || binding.ProviderName != c.ProviderName || (c.ProviderUid != "" && binding.ProviderUid != c.ProviderUid) {
		return conflict("reviewed Provider placement or identity changed")
	}
	if _, err = q.GetBaseConnectivity(ctx, sqlcgen.GetBaseConnectivityParams{TenantID: c.TenantID, VpcID: c.VpcID}); err == nil {
		return conflict("VPC base intent already exists")
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", databaseFailure(err)
	}
	op, err := q.GetOperation(ctx, sqlcgen.GetOperationParams{TenantID: c.TenantID, OperationID: v.LastOperationID})
	if err != nil {
		return "", databaseFailure(err)
	}
	if op.CompletedAt == nil {
		return conflict("VPC operation is not completed")
	}
	opID := uuid.NewString()
	if err = q.InsertOperation(ctx, sqlcgen.InsertOperationParams{TenantID: c.TenantID, VpcID: c.VpcID, OperationID: opID, Kind: "ensure_vpc_base_connectivity", CreatedAt: now}); err != nil {
		return "", databaseFailure(err)
	}
	if err = affected(q.AdmitBaseBackfillVPC(ctx, sqlcgen.AdmitBaseBackfillVPCParams{TenantID: c.TenantID, VpcID: c.VpcID, Version: v.Version, OperationID: opID})); err != nil {
		return "", err
	}
	if err = p.acceptBaseConnectivity(ctx, q, c.TenantID, c.VpcID, binding.Namespace, opID, run.PoolID, run.PoolRevision, now, false); err != nil {
		return "", err
	}
	if err = affected(q.ScheduleBaseBackfill(ctx, sqlcgen.ScheduleBaseBackfillParams{TenantID: c.TenantID, VpcID: &c.VpcID})); err != nil {
		return "", err
	}
	if err = q.InsertHistory(ctx, sqlcgen.InsertHistoryParams{TenantID: c.TenantID, HistoryID: uuid.NewString(), VpcID: c.VpcID, OperationID: &opID, Event: "ensure_base_connectivity_accepted", ResourceState: v.State, OperationState: "queued", ActorRef: "", CallerRef: "base-connectivity-admin", CorrelationID: run.RunID, CreatedAt: now}); err != nil {
		return "", databaseFailure(err)
	}
	return opID, nil
}

type ConnectivityRollout struct {
	ClusterID                string `json:"cluster_id"`
	NewVPCsEnabled           bool   `json:"new_vpcs_enabled"`
	LegacyAggregationEnabled bool   `json:"legacy_aggregation_enabled"`
}

func (p *Postgres) GetBaseConnectivityRollout(ctx context.Context) (ConnectivityRollout, error) {
	out := ConnectivityRollout{ClusterID: p.placement.ClusterID}
	row, err := p.queries.GetConnectivityRollout(ctx, sqlcgen.GetConnectivityRolloutParams{ClusterID: p.placement.ClusterID})
	if errors.Is(err, pgx.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, databaseFailure(err)
	}
	out.NewVPCsEnabled, out.LegacyAggregationEnabled = row.NewVpcsEnabled, row.LegacyAggregationEnabled
	return out, nil
}

func (p *Postgres) SetNewVPCBaseConnectivity(ctx context.Context, enabled bool) (ConnectivityRollout, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return ConnectivityRollout{}, databaseFailure(err)
	}
	defer tx.Rollback(ctx)
	q := p.queries.WithTx(tx)
	if err = q.LockConnectivityRollout(ctx, sqlcgen.LockConnectivityRolloutParams{ClusterID: p.placement.ClusterID}); err != nil {
		return ConnectivityRollout{}, databaseFailure(err)
	}
	if err = q.EnsureConnectivityRollout(ctx, sqlcgen.EnsureConnectivityRolloutParams{ClusterID: p.placement.ClusterID}); err != nil {
		return ConnectivityRollout{}, databaseFailure(err)
	}
	rows, err := q.SetNewVPCConnectivityEnabled(ctx, sqlcgen.SetNewVPCConnectivityEnabledParams{ClusterID: p.placement.ClusterID, Enabled: enabled})
	if err != nil {
		return ConnectivityRollout{}, databaseFailure(err)
	}
	if rows != 1 {
		return ConnectivityRollout{}, biz.Fail(biz.ResourceBusy, "legacy aggregation is active; disabling the new-VPC base path would reopen legacy admission")
	}
	if err = tx.Commit(ctx); err != nil {
		return ConnectivityRollout{}, databaseFailure(err)
	}
	return p.GetBaseConnectivityRollout(ctx)
}

// ActivateLegacyBaseAggregation is one-way in this batch. It verifies that
// the reviewed Res declarations exist; provider readiness is not an input.
func (p *Postgres) ActivateLegacyBaseAggregation(ctx context.Context) (ConnectivityRollout, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return ConnectivityRollout{}, databaseFailure(err)
	}
	defer tx.Rollback(ctx)
	q := p.queries.WithTx(tx)
	if err = q.LockConnectivityRollout(ctx, sqlcgen.LockConnectivityRolloutParams{ClusterID: p.placement.ClusterID}); err != nil {
		return ConnectivityRollout{}, databaseFailure(err)
	}
	rollout, err := q.GetConnectivityRollout(ctx, sqlcgen.GetConnectivityRolloutParams{ClusterID: p.placement.ClusterID})
	if err != nil {
		return ConnectivityRollout{}, databaseFailure(err)
	}
	if !rollout.NewVpcsEnabled {
		return ConnectivityRollout{}, biz.Fail(biz.BaseConnectivityNotReady, "enable the new-VPC base path before legacy aggregation")
	}
	if rollout.LegacyAggregationEnabled {
		return ConnectivityRollout{ClusterID: p.placement.ClusterID, NewVPCsEnabled: true, LegacyAggregationEnabled: true}, nil
	}
	candidates, err := q.LockLegacyBaseAggregationVPCs(ctx, sqlcgen.LockLegacyBaseAggregationVPCsParams{ClusterID: p.placement.ClusterID})
	if err != nil {
		return ConnectivityRollout{}, databaseFailure(err)
	}
	for _, v := range candidates {
		if err = p.requireBaseDeclaration(ctx, q, v.TenantID, v.VpcID); err != nil {
			return ConnectivityRollout{}, fmt.Errorf("legacy VPC %s/%s has no active base declaration: %w", v.TenantID, v.VpcID, err)
		}
	}
	for _, v := range candidates {
		if err = q.RequireVPCBase(ctx, sqlcgen.RequireVPCBaseParams{TenantID: v.TenantID, VpcID: v.VpcID}); err != nil {
			return ConnectivityRollout{}, databaseFailure(err)
		}
	}
	if err = affected(q.ActivateLegacyBaseAggregation(ctx, sqlcgen.ActivateLegacyBaseAggregationParams{ClusterID: p.placement.ClusterID})); err != nil {
		return ConnectivityRollout{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return ConnectivityRollout{}, databaseFailure(err)
	}
	return p.GetBaseConnectivityRollout(ctx)
}
