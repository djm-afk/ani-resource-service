package data

import (
	"context"
	"time"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
)

// LB activation depends on the VPC itself, not the aggregate availability of
// its optional Intranet EIP/SNAT. Reuse the fenced Provider proof also consumed
// by public egress; an available product row alone is not readiness evidence.
func (p *Postgres) requireLBVPCProviderReady(ctx context.Context, q *sqlcgen.Queries, tenant, vpcID string, now time.Time, freshness time.Duration) error {
	parent, err := q.GetVPC(ctx, sqlcgen.GetVPCParams{TenantID: tenant, VpcID: vpcID})
	if err != nil {
		return databaseFailure(err)
	}
	if state := parent.NetworkVpc.State; state == "deleting" || state == "deleted" || state == "failed" {
		return biz.Fail(biz.ParentNotReady, "VPC has closed LB activation")
	}
	binding, err := q.GetBinding(ctx, sqlcgen.GetBindingParams{TenantID: tenant, ResourceID: vpcID})
	if err != nil {
		return databaseFailure(err)
	}
	if binding.ProviderUid == "" || binding.PendingAction != "" {
		return biz.Fail(biz.ProviderOwnership, "VPC Provider identity or mutation is unresolved")
	}
	proof, err := q.GetVPCProviderReadiness(ctx, sqlcgen.GetVPCProviderReadinessParams{TenantID: tenant, VpcID: &vpcID})
	if err != nil {
		return databaseFailure(err)
	}
	if proof.ProviderObservedUid != binding.ProviderUid {
		return biz.Fail(biz.ProviderOwnership, "VPC Provider observation identity changed")
	}
	// Runtime resolution does not hold the VPC admission lock. A legitimate
	// parent observation can advance while reading these rows, so collect the
	// freshness boundary after the proof rather than before its query.
	now, err = q.DatabaseTime(ctx)
	if err != nil {
		return databaseFailure(err)
	}
	if !proof.ProviderReady || !freshTime(proof.ProviderObservedAt, now, freshness) || proof.ProviderGeneration <= 0 || proof.ProviderObservedGeneration != proof.ProviderGeneration {
		return biz.Fail(biz.ParentNotReady, "VPC Provider is not fresh and ready")
	}
	return nil
}
