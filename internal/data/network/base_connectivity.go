package data

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
)

// The caller holds the parent lock (or has just inserted it). All identities,
// including child operations, are committed atomically before any Provider IO.
func (p *Postgres) acceptBaseConnectivity(ctx context.Context, q *sqlcgen.Queries, tenant, vpcID, namespace, operationID, poolID string, poolRevision int64, now time.Time, required bool) error {
	if err := q.LockPlatformCluster(ctx, sqlcgen.LockPlatformClusterParams{ClusterID: p.placement.ClusterID}); err != nil {
		return databaseFailure(err)
	}
	if poolID == "" {
		def, err := q.GetDefaultIntranetPool(ctx, sqlcgen.GetDefaultIntranetPoolParams{ClusterID: p.placement.ClusterID})
		if errors.Is(err, pgx.ErrNoRows) {
			return biz.Fail(biz.BaseConnectivityNotReady, "default Intranet pool is missing")
		}
		if err != nil {
			return databaseFailure(err)
		}
		poolID = def.PoolID
	}
	pool, err := q.LockPublicPool(ctx, sqlcgen.LockPublicPoolParams{ClusterID: p.placement.ClusterID, ResourceID: poolID})
	if err != nil {
		return databaseFailure(err)
	}
	if pool.Scope != "intranet" || (poolRevision != 0 && pool.ConfigRevision != poolRevision) {
		return biz.Fail(biz.BaseConnectivityNotReady, "fixed Intranet pool revision differs")
	}
	// Admission can wait behind parent/platform/pool locks. Validate facts at
	// the database clock after those waits, never at the earlier caller time.
	now, err = q.DatabaseTime(ctx)
	if err != nil {
		return databaseFailure(err)
	}
	if err = p.poolReady(ctx, q, pool, now, p.baseFreshnessLimit(), true); err != nil {
		return err
	}
	eipID, snatID := newEgressID("eip"), newEgressID("snat")
	eipOp, snatOp := uuid.NewString(), uuid.NewString()
	if _, err = q.InsertBaseEIP(ctx, sqlcgen.InsertBaseEIPParams{TenantID: tenant, EipID: eipID, ClusterID: p.placement.ClusterID, Namespace: namespace, PoolID: poolID, PoolRevision: pool.ConfigRevision, VpcID: &vpcID, CreatedAt: now, OperationID: eipOp}); err != nil {
		return databaseFailure(err)
	}
	if _, err = q.InsertBaseSnat(ctx, sqlcgen.InsertBaseSnatParams{TenantID: tenant, SnatID: snatID, ClusterID: p.placement.ClusterID, Namespace: namespace, VpcID: vpcID, EipID: eipID, CreatedAt: now, OperationID: snatOp}); err != nil {
		return databaseFailure(err)
	}
	for _, child := range []struct{ id, op, kind string }{{eipID, eipOp, "create_eip"}, {snatID, snatOp, "bind_snat"}} {
		eip, snat := "", ""
		if child.id == eipID {
			eip = child.id
		} else {
			snat = child.id
		}
		if err = q.InsertOperation(ctx, sqlcgen.InsertOperationParams{TenantID: tenant, EipID: eip, SnatID: snat, OperationID: child.op, Kind: child.kind, CreatedAt: now}); err != nil {
			return databaseFailure(err)
		}
		if err = q.InsertBinding(ctx, sqlcgen.InsertBindingParams{TenantID: tenant, EipID: eip, SnatID: snat, BindingID: uuid.NewString(), ClusterID: p.placement.ClusterID, Namespace: namespace, ProviderName: strings.Replace(child.id, "_", "-", 1)}); err != nil {
			return databaseFailure(err)
		}
		if err = q.InsertReconciliation(ctx, sqlcgen.InsertReconciliationParams{TenantID: tenant, EipID: eip, SnatID: snat, NextRunAt: now}); err != nil {
			return databaseFailure(err)
		}
	}
	if err = affected(q.ClaimEIPForSnat(ctx, sqlcgen.ClaimEIPForSnatParams{TenantID: tenant, ClusterID: p.placement.ClusterID, Namespace: namespace, EipID: eipID, SnatID: snatID, CreatedAt: now})); err != nil {
		return err
	}
	if err = q.InsertBaseConnectivity(ctx, sqlcgen.InsertBaseConnectivityParams{TenantID: tenant, VpcID: vpcID, ClusterID: p.placement.ClusterID, Namespace: namespace, PoolID: poolID, PoolRevision: pool.ConfigRevision, EipID: eipID, SnatID: snatID, OperationID: operationID}); err != nil {
		return databaseFailure(err)
	}
	if required {
		return databaseFailure(q.RequireVPCBase(ctx, sqlcgen.RequireVPCBaseParams{TenantID: tenant, VpcID: vpcID}))
	}
	return nil
}

func (p *Postgres) requireBaseConnectivity(ctx context.Context, q *sqlcgen.Queries, tenant, vpcID string, now time.Time, freshness time.Duration) error {
	b, err := q.GetBaseConnectivity(ctx, sqlcgen.GetBaseConnectivityParams{TenantID: tenant, VpcID: vpcID})
	if errors.Is(err, pgx.ErrNoRows) {
		return biz.Fail(biz.BaseConnectivityNotReady, "VPC base connectivity has not been ensured")
	}
	if err != nil {
		return databaseFailure(err)
	}
	if b.Terminating || b.State != "ready" || !freshTime(b.ObservedAt, now, freshness) {
		return biz.Fail(biz.BaseConnectivityNotReady, "VPC base connectivity is not fresh and ready")
	}
	e, err := q.GetEIPInternal(ctx, sqlcgen.GetEIPInternalParams{TenantID: tenant, EipID: b.EipID})
	if err != nil {
		return databaseFailure(err)
	}
	s, err := q.GetSnatInternal(ctx, sqlcgen.GetSnatInternalParams{TenantID: tenant, SnatID: b.SnatID})
	if err != nil {
		return databaseFailure(err)
	}
	if !b.ProviderReady || !freshTime(b.ProviderObservedAt, now, freshness) || !freshResource(e.State, e.ObservedAt, now, freshness) || !freshResource(s.State, s.ObservedAt, now, freshness) || !s.AppliedEnabled.Valid || !s.AppliedEnabled.Bool {
		return biz.Fail(biz.BaseConnectivityNotReady, "base dependencies are not ready")
	}
	for _, id := range []string{vpcID, b.EipID, b.SnatID} {
		binding, err := q.GetBinding(ctx, sqlcgen.GetBindingParams{TenantID: tenant, ResourceID: id})
		if err != nil {
			return databaseFailure(err)
		}
		if binding.ProviderUid == "" || binding.PendingAction != "" {
			return biz.Fail(biz.BaseConnectivityNotReady, "base identity or mutation unresolved")
		}
	}
	claim, err := q.GetEIPClaim(ctx, sqlcgen.GetEIPClaimParams{TenantID: tenant, EipID: b.EipID})
	if err != nil {
		return databaseFailure(err)
	}
	if claim.BindingID != b.SnatID || claim.BindingState != "bound" {
		return biz.Fail(biz.BaseConnectivityNotReady, "base claim has not been confirmed")
	}
	return nil
}
func freshTime(t *time.Time, now time.Time, limit time.Duration) bool {
	return t != nil && !t.After(now) && now.Sub(*t) <= limit
}

// Called inside Claim under the same parent and resource locks. Gated work
// remains in the existing durable reconciliation lane, including after restart.
func (p *Postgres) baseWorkGate(ctx context.Context, q *sqlcgen.Queries, r biz.ResourceWork, now time.Time) (biz.Reason, error) {
	if r.Kind != "vpc" && !r.SystemManaged {
		return "", nil
	}
	parent := r.VPCID
	if r.Kind == "vpc" {
		parent = r.ID
	}
	b, err := q.GetBaseConnectivity(ctx, sqlcgen.GetBaseConnectivityParams{TenantID: r.TenantID, VpcID: parent})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", databaseFailure(err)
	}
	if r.Kind == "vpc" {
		if r.State != biz.Deleting && r.State != biz.Deleted {
			return "", nil
		}
		done, err := p.prepareBaseCleanup(ctx, q, b, now)
		if err != nil {
			return "", err
		}
		if !done {
			return biz.CleanupPending, nil
		}
		return "", nil
	}
	if r.State == biz.Deleting || r.State == biz.Deleted {
		return "", nil
	}
	if b.Terminating || !b.ProviderReady || !freshTime(b.ProviderObservedAt, now, p.baseFreshnessLimit()) {
		return biz.BaseConnectivityNotReady, nil
	}
	if r.Kind == "snat" {
		e, err := q.GetEIPInternal(ctx, sqlcgen.GetEIPInternalParams{TenantID: r.TenantID, EipID: b.EipID})
		if err != nil {
			return "", databaseFailure(err)
		}
		eb, err := q.GetBinding(ctx, sqlcgen.GetBindingParams{TenantID: r.TenantID, ResourceID: b.EipID})
		if err != nil {
			return "", databaseFailure(err)
		}
		if !freshResource(e.State, e.ObservedAt, now, p.baseFreshnessLimit()) || eb.ProviderUid == "" || eb.PendingAction != "" {
			return biz.BaseConnectivityNotReady, nil
		}
	}
	return "", nil
}

func (p *Postgres) prepareBaseCleanup(ctx context.Context, q *sqlcgen.Queries, b sqlcgen.NetworkVpcBaseConnectivity, now time.Time) (bool, error) {
	// The parent lock serializes this with child completion/admission. Acquire
	// EIP before SNAT even though the Provider cleanup order is SNAT then EIP.
	e, err := q.LockEIP(ctx, sqlcgen.LockEIPParams{TenantID: b.TenantID, EipID: b.EipID})
	if err != nil {
		return false, databaseFailure(err)
	}
	s, err := q.LockSnat(ctx, sqlcgen.LockSnatParams{TenantID: b.TenantID, SnatID: b.SnatID})
	if err != nil {
		return false, databaseFailure(err)
	}
	if s.State != "deleted" {
		if s.State != "deleting" {
			if err = q.RetireActiveOperation(ctx, sqlcgen.RetireActiveOperationParams{TenantID: b.TenantID, OperationID: s.LastOperationID}); err != nil {
				return false, databaseFailure(err)
			}
			op := uuid.NewString()
			if _, err = q.AdmitSnatDeletion(ctx, sqlcgen.AdmitSnatDeletionParams{TenantID: b.TenantID, SnatID: s.SnatID, Version: s.Version, OperationID: op}); err != nil {
				return false, databaseFailure(err)
			}
			if err = deletionRecords(ctx, q, b.TenantID, s.SnatID, "snat", op, biz.Attribution{}, now); err != nil {
				return false, err
			}
		}
		return false, nil
	}
	if e.State != "deleted" {
		if e.State != "deleting" {
			if err = q.RetireActiveOperation(ctx, sqlcgen.RetireActiveOperationParams{TenantID: b.TenantID, OperationID: e.LastOperationID}); err != nil {
				return false, databaseFailure(err)
			}
			op := uuid.NewString()
			if _, err = q.AdmitEIPDeletion(ctx, sqlcgen.AdmitEIPDeletionParams{TenantID: b.TenantID, EipID: e.EipID, Version: e.Version, OperationID: op}); err != nil {
				return false, databaseFailure(err)
			}
			if err = deletionRecords(ctx, q, b.TenantID, e.EipID, "eip", op, biz.Attribution{}, now); err != nil {
				return false, err
			}
		}
		return false, nil
	}
	return true, nil
}

// Finish owns the aggregate product decision. Provider readiness is saved
// separately so internal SNAT admission never waits for product available.
func (p *Postgres) finishBaseConnectivity(ctx context.Context, q *sqlcgen.Queries, w biz.Work, progress *biz.Progress, now time.Time) error {
	r := w.Resource
	if r.Kind == "snat" {
		if progress.State == biz.Deleted {
			if err := q.ReleaseSnatClaim(ctx, sqlcgen.ReleaseSnatClaimParams{TenantID: r.TenantID, SnatID: &r.ID}); err != nil {
				return databaseFailure(err)
			}
		} else if progress.Observed && progress.Egress != nil && progress.Egress.AppliedEnabled != nil {
			if err := q.ConfirmSnatClaim(ctx, sqlcgen.ConfirmSnatClaimParams{TenantID: r.TenantID, SnatID: &r.ID}); err != nil {
				return databaseFailure(err)
			}
		}
	}
	if r.Kind != "vpc" {
		// A child can recover from an observation gap without changing state,
		// reason or identity. Wake its parent on that freshness transition too;
		// fresh-to-fresh audits do not need another parent notification.
		becameFresh := progress.Observed &&
			!freshResource(string(r.State), r.ObservedAt, now, p.baseFreshnessLimit()) &&
			freshResource(string(progress.State), &progress.Proof.CollectedAt, now, p.baseFreshnessLimit())
		if r.SystemManaged && (r.State != progress.State || r.Reason != progress.Reason || w.KnownIdentity != progress.Identity || becameFresh) {
			return databaseFailure(q.NotifyBaseParent(ctx, sqlcgen.NotifyBaseParentParams{TenantID: r.TenantID, ResourceID: r.ID}))
		}
		return nil
	}
	b, err := q.GetBaseConnectivity(ctx, sqlcgen.GetBaseConnectivityParams{TenantID: r.TenantID, VpcID: r.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return databaseFailure(err)
	}
	if b.Terminating {
		state := "deleting"
		if progress.State == biz.Deleted {
			state = "deleted"
		}
		return databaseFailure(q.SetBaseObservation(ctx, sqlcgen.SetBaseObservationParams{TenantID: r.TenantID, VpcID: r.ID, State: state, Reason: string(progress.Reason), ProviderReady: false}))
	}
	providerReady := progress.Observed && progress.State == biz.Available && progress.Identity != ""
	providerTime := b.ProviderObservedAt
	if progress.Observed {
		providerTime = &progress.Proof.CollectedAt
	} else if progress.Reason == biz.ProviderUnavailable && (r.State == biz.Available || r.State == biz.Degraded) && !w.ActiveOperation && w.PendingAction == "" && w.KnownIdentity != "" {
		// A failed read supplies no new health fact. Reuse only the original,
		// unexpired readiness proof for this exact persisted provider identity.
		// Actual UID conflicts, pending mutations and deletion never use it.
		prior, err := q.GetVPCProviderReadiness(ctx, sqlcgen.GetVPCProviderReadinessParams{TenantID: r.TenantID, VpcID: &r.ID})
		if err != nil {
			return databaseFailure(err)
		}
		providerReady = prior.ProviderReady && prior.ProviderObservedUid == w.KnownIdentity &&
			freshTime(prior.ProviderObservedAt, now, p.baseFreshnessLimit())
		if providerReady {
			providerTime = prior.ProviderObservedAt
		}
	}
	e, err := q.GetEIPInternal(ctx, sqlcgen.GetEIPInternalParams{TenantID: r.TenantID, EipID: b.EipID})
	if err != nil {
		return databaseFailure(err)
	}
	s, err := q.GetSnatInternal(ctx, sqlcgen.GetSnatInternalParams{TenantID: r.TenantID, SnatID: b.SnatID})
	if err != nil {
		return databaseFailure(err)
	}
	ready := providerReady && freshTime(providerTime, now, p.baseFreshnessLimit()) && freshResource(e.State, e.ObservedAt, now, p.baseFreshnessLimit()) && freshResource(s.State, s.ObservedAt, now, p.baseFreshnessLimit()) && s.AppliedEnabled.Valid && s.AppliedEnabled.Bool
	for _, id := range []string{b.EipID, b.SnatID} {
		binding, err := q.GetBinding(ctx, sqlcgen.GetBindingParams{TenantID: r.TenantID, ResourceID: id})
		if err != nil {
			return databaseFailure(err)
		}
		ready = ready && binding.ProviderUid != "" && binding.PendingAction == ""
	}
	state, reason := "pending", biz.BaseConnectivityNotReady
	if !providerReady && progress.Reason != "" {
		reason = progress.Reason
	} else if e.State != "available" && e.Reason != "" {
		reason = biz.Reason(e.Reason)
	} else if s.State != "available" && s.Reason != "" {
		reason = biz.Reason(s.Reason)
	}
	var observed *time.Time
	if ready {
		state, reason = "ready", ""
		t := *providerTime
		if e.ObservedAt.Before(t) {
			t = *e.ObservedAt
		}
		if s.ObservedAt.Before(t) {
			t = *s.ObservedAt
		}
		observed = &t
	} else if b.State == "ready" || b.State == "degraded" {
		state = "degraded"
	}
	if err = q.SetBaseObservation(ctx, sqlcgen.SetBaseObservationParams{TenantID: r.TenantID, VpcID: r.ID, State: state, Reason: string(reason), ProviderReady: providerReady, ProviderObservedAt: providerTime, ObservedAt: observed}); err != nil {
		return databaseFailure(err)
	}
	if b.ProviderReady != providerReady || (providerReady && !freshTime(b.ProviderObservedAt, now, p.baseFreshnessLimit())) {
		if err = q.NotifyBaseChildren(ctx, sqlcgen.NotifyBaseChildrenParams{TenantID: r.TenantID, VpcID: r.ID}); err != nil {
			return databaseFailure(err)
		}
	}
	aggregate := r.BaseRequired
	if !aggregate {
		rollout, err := q.GetConnectivityRollout(ctx, sqlcgen.GetConnectivityRolloutParams{ClusterID: b.ClusterID})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return databaseFailure(err)
		}
		aggregate = rollout.LegacyAggregationEnabled
	}
	if !ready {
		if w.ActiveOperation && progress.State != biz.Failed && (w.Operation.Kind == "create_vpc" || w.Operation.Kind == "ensure_vpc_base_connectivity") {
			if progress.OperationState != biz.Blocked {
				progress.OperationState = biz.Retrying
			}
			progress.Reason = reason
			progress.Backoff = true
		}
		if aggregate && progress.State == biz.Available {
			progress.State = biz.Provisioning
			if r.State == biz.Available || r.State == biz.Degraded {
				progress.State = biz.Degraded
			}
			progress.Reason = reason
		}
	}
	return nil
}

// Configured once in the composition root before serving requests or workers.
func (p *Postgres) UseBaseConnectivityFreshness(limit time.Duration) { p.baseFreshness = limit }
func (p *Postgres) baseFreshnessLimit() time.Duration {
	if p.baseFreshness > 0 {
		return p.baseFreshness
	}
	return time.Minute
}
