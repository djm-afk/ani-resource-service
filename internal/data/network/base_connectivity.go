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
	// Use the transaction clock after acquiring the allocation locks.
	now, err = q.DatabaseTime(ctx)
	if err != nil {
		return databaseFailure(err)
	}
	if err = p.poolAllocationAllowed(ctx, q, pool, true); err != nil {
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

// Legacy activation checks only that the managed child declaration exists.
// Native readiness and observation time do not authorize product operations.
func (p *Postgres) requireBaseDeclaration(ctx context.Context, q *sqlcgen.Queries, tenant, vpcID string) error {
	base, err := q.GetBaseConnectivity(ctx, sqlcgen.GetBaseConnectivityParams{TenantID: tenant, VpcID: vpcID})
	if errors.Is(err, pgx.ErrNoRows) {
		return biz.Fail(biz.BaseConnectivityNotReady, "VPC base connectivity has not been declared")
	}
	if err != nil {
		return databaseFailure(err)
	}
	if base.Terminating {
		return biz.Fail(biz.ResourceBusy, "VPC base connectivity is being removed")
	}
	return nil
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

// Finish records this managed child relationship. Its state never overrides
// the VPC's own CR status or the VPC operation's outcome.
func (p *Postgres) finishBaseConnectivity(ctx context.Context, q *sqlcgen.Queries, work biz.Work, progress *biz.Progress, now time.Time) error {
	resource := work.Resource
	if resource.Kind == "snat" {
		if progress.State == biz.Deleted {
			if err := q.ReleaseSnatClaim(ctx, sqlcgen.ReleaseSnatClaimParams{TenantID: resource.TenantID, SnatID: &resource.ID}); err != nil {
				return databaseFailure(err)
			}
		} else if progress.Observed && progress.Egress != nil && progress.Egress.AppliedEnabled != nil {
			if err := q.ConfirmSnatClaim(ctx, sqlcgen.ConfirmSnatClaimParams{TenantID: resource.TenantID, SnatID: &resource.ID}); err != nil {
				return databaseFailure(err)
			}
		}
	}
	if resource.Kind != "vpc" && !resource.SystemManaged {
		return nil
	}
	parent := resource.VPCID
	if resource.Kind == "vpc" {
		parent = resource.ID
	}
	base, err := q.GetBaseConnectivity(ctx, sqlcgen.GetBaseConnectivityParams{TenantID: resource.TenantID, VpcID: parent})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return databaseFailure(err)
	}
	if base.Terminating {
		// Run after Advance, so a completed SNAT can schedule its own EIP
		// cleanup even if the original VPC is already gone and retired.
		done, err := p.prepareBaseCleanup(ctx, q, base, now)
		if err != nil {
			return err
		}
		vpc, err := q.LockVPC(ctx, sqlcgen.LockVPCParams{TenantID: resource.TenantID, VpcID: parent})
		if err != nil {
			return databaseFailure(err)
		}
		state, reason := "deleting", string(biz.CleanupPending)
		if done && vpc.State == string(biz.Deleted) {
			state, reason = "deleted", ""
		}
		observedAt := base.ObservedAt
		if progress.Observed {
			observedAt = &now
		}
		if err := q.SetBaseObservation(ctx, sqlcgen.SetBaseObservationParams{TenantID: resource.TenantID, VpcID: parent, State: state, Reason: reason, ObservedAt: observedAt}); err != nil {
			return databaseFailure(err)
		}
		if resource.SystemManaged && progress.State == biz.Deleted && resource.State != biz.Deleted {
			return databaseFailure(q.NotifyBaseParent(ctx, sqlcgen.NotifyBaseParentParams{TenantID: resource.TenantID, ResourceID: resource.ID}))
		}
		return nil
	}
	if resource.Kind != "snat" || !progress.Observed {
		return nil
	}
	// The SNAT's reported application result is authoritative for this
	// managed egress. Do not recheck the VPC, EIP, nodes or saved timestamps.
	state, reason := base.State, base.Reason
	if progress.Egress != nil && progress.Egress.AppliedEnabled != nil {
		if *progress.Egress.AppliedEnabled {
			state, reason = "ready", ""
		} else {
			state, reason = "pending", string(progress.Reason)
			if base.State == "ready" || base.State == "degraded" {
				state = "degraded"
			}
		}
	} else {
		reason = string(progress.Reason)
	}
	return databaseFailure(q.SetBaseObservation(ctx, sqlcgen.SetBaseObservationParams{TenantID: resource.TenantID, VpcID: parent, State: state, Reason: reason, ObservedAt: &now}))
}
