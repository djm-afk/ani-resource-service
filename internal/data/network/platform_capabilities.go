package data

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
)

type LoadBalancerCapabilityReader interface {
	ReadLoadBalancerCapability(context.Context) (*bool, biz.Reason, error)
}

func (p *Postgres) UseLoadBalancerCapabilityReader(reader LoadBalancerCapabilityReader) {
	p.lbCapabilityReader = reader
}

// This explicit administrator query reads the fixed GatewayClass once. Its
// diagnostic result never gates tenant resource creation or observation.
func (p *Postgres) GetPlatformCapabilities(ctx context.Context) (biz.PlatformNetworkCapabilities, error) {
	unknown := biz.CapabilityObservation{Reason: biz.Reason("CAPABILITY_UNKNOWN"), ObservationStale: true}
	result := biz.PlatformNetworkCapabilities{BaseConnectivity: unknown, PublicAddress: unknown, LoadBalancer: unknown}
	var readErr error
	if p.lbCapabilityReader != nil {
		var ready *bool
		var reason biz.Reason
		ready, reason, readErr = p.lbCapabilityReader.ReadLoadBalancerCapability(ctx)
		if readErr == nil {
			now, err := p.queries.DatabaseTime(ctx)
			if err != nil {
				return result, databaseFailure(err)
			}
			value := pgtype.Bool{}
			if ready != nil {
				value = pgtype.Bool{Bool: *ready, Valid: true}
			} else if reason == "" {
				reason = biz.ProviderStatusUnreported
			}
			if err = p.queries.SaveLBCapability(ctx, sqlcgen.SaveLBCapabilityParams{ClusterID: p.placement.ClusterID, Ready: value, Reason: string(reason), ObservedAt: now}); err != nil {
				return result, databaseFailure(err)
			}
		}
	}
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return result, databaseFailure(err)
	}
	defer tx.Rollback(ctx)
	q := p.queries.WithTx(tx)
	inspect := func(id, scope string) (biz.CapabilityObservation, error) {
		v := unknown
		pool, err := q.GetPublicPool(ctx, sqlcgen.GetPublicPoolParams{ClusterID: p.placement.ClusterID, ResourceID: id})
		if err != nil {
			return v, databaseFailure(err)
		}
		if pool.Scope != scope {
			return v, biz.Fail(biz.DependencyUnavailable, "default address pool scope mismatch")
		}
		row, err := q.GetPlatform(ctx, sqlcgen.GetPlatformParams{ClusterID: p.placement.ClusterID, Kind: "public_pool", ResourceID: id})
		if err != nil {
			return v, databaseFailure(err)
		}
		r := row.NetworkPlatformResource
		v.ObservedAt, v.ObservationStale = r.ObservedAt, r.ObservedAt == nil || row.ObservationError != ""
		v.Ready = r.State == "available" && !pool.Retired && pool.AllocationEnabled
		v.Reason = biz.Reason(r.Reason)
		if !v.Ready && v.Reason == "" {
			v.Reason = poolUnavailableReason(scope)
		}
		return v, nil
	}
	public, err := q.GetDefaultPublicPool(ctx, sqlcgen.GetDefaultPublicPoolParams{ClusterID: p.placement.ClusterID})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return result, databaseFailure(err)
	}
	if err == nil {
		result.PublicAddress, err = inspect(public.PoolID, "public")
		if err != nil {
			return result, err
		}
	}
	intranet, err := q.GetDefaultIntranetPool(ctx, sqlcgen.GetDefaultIntranetPoolParams{ClusterID: p.placement.ClusterID})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return result, databaseFailure(err)
	}
	if err == nil {
		result.BaseConnectivity, err = inspect(intranet.PoolID, "intranet")
		if err != nil {
			return result, err
		}
	}
	lb, err := q.GetLBCapability(ctx, sqlcgen.GetLBCapabilityParams{ClusterID: p.placement.ClusterID})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return result, databaseFailure(err)
	}
	if err == nil {
		result.LoadBalancer = biz.CapabilityObservation{Ready: lb.Ready, Reason: biz.Reason(lb.Reason), ObservedAt: &lb.ObservedAt}
	}
	if readErr != nil {
		result.LoadBalancer.ObservationStale, result.LoadBalancer.Reason = true, biz.ProviderUnavailable
	}
	return result, nil
}
