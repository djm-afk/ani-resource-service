package data

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
)

func (p *Postgres) claimPlatformNetwork(ctx context.Context, tx pgx.Tx, q *sqlcgen.Queries, owner string, duration time.Duration) (biz.Work, bool, error) {
	task, err := q.LockDuePlatformNetwork(ctx, sqlcgen.LockDuePlatformNetworkParams{ClusterID: p.placement.ClusterID})
	if errors.Is(err, pgx.ErrNoRows) {
		return biz.Work{}, false, nil
	}
	if err != nil {
		return biz.Work{}, false, databaseFailure(err)
	}
	spec := &biz.PlatformNetworkWorkSpec{ClusterID: p.placement.ClusterID}
	var resource biz.ResourceWork
	if task.VpcID != nil {
		v, e := q.GetPlatformVPC(ctx, sqlcgen.GetPlatformVPCParams{VpcID: *task.VpcID})
		if e != nil {
			return biz.Work{}, false, databaseFailure(e)
		}
		spec.Kind, spec.Namespace, spec.Name, spec.UID = "vpc", v.Namespace, v.ProviderName, v.ProviderUid
		resource = biz.ResourceWork{ID: v.VpcID, Kind: "vpc", CIDR: v.Cidr, State: biz.ResourceState(v.State), Reason: biz.Reason(v.Reason), Version: v.Version, ObservedAt: v.ObservedAt, UpdatedAt: v.UpdatedAt}
	} else {
		s, e := q.GetPlatformSubnet(ctx, sqlcgen.GetPlatformSubnetParams{SubnetID: textValue(task.SubnetID)})
		if e != nil {
			return biz.Work{}, false, databaseFailure(e)
		}
		spec.Kind, spec.Namespace, spec.Name, spec.UID = "subnet", s.Namespace, s.ProviderName, s.ProviderUid
		resource = biz.ResourceWork{ID: s.SubnetID, VPCID: s.VpcID, Kind: "subnet", CIDR: s.Cidr, Gateway: s.Gateway, State: biz.ResourceState(s.State), Reason: biz.Reason(s.Reason), Version: s.Version, ObservedAt: s.ObservedAt, UpdatedAt: s.UpdatedAt}
	}
	lease, err := q.AcquirePlatformNetworkLease(ctx, sqlcgen.AcquirePlatformNetworkLeaseParams{ResourceID: task.ResourceID, Owner: owner, LeaseMicros: duration.Microseconds()})
	if err != nil {
		return biz.Work{}, false, databaseFailure(err)
	}
	now, err := q.DatabaseTime(ctx)
	if err != nil {
		return biz.Work{}, false, databaseFailure(err)
	}
	work := biz.Work{Resource: resource, PlatformNetwork: spec, KnownIdentity: spec.UID, Owner: owner, Epoch: lease.LeaseEpoch, Attempt: lease.RetryAttempt, ClaimedGeneration: lease.RequestedGeneration, Now: now}
	if err = tx.Commit(ctx); err != nil {
		return biz.Work{}, false, databaseFailure(err)
	}
	return work, true, nil
}

func (p *Postgres) finishPlatformNetwork(ctx context.Context, w biz.Work, progress biz.Progress) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return databaseFailure(err)
	}
	defer tx.Rollback(ctx)
	q := p.queries.WithTx(tx)
	// The original product row precedes its queue lock, matching config sync.
	// Version and the final lease predicate fence both projection and parking.
	var version int64
	if w.PlatformNetwork.Kind == "vpc" {
		v, e := q.LockPlatformVPC(ctx, sqlcgen.LockPlatformVPCParams{VpcID: w.Resource.ID})
		if e != nil {
			return databaseFailure(e)
		}
		version = v.Version
	} else {
		s, e := q.LockPlatformSubnet(ctx, sqlcgen.LockPlatformSubnetParams{SubnetID: w.Resource.ID})
		if e != nil {
			return databaseFailure(e)
		}
		version = s.Version
	}
	if version != w.Resource.Version {
		return biz.ErrLeaseLost
	}
	if _, err = q.CheckPlatformNetworkLease(ctx, sqlcgen.CheckPlatformNetworkLeaseParams{ResourceID: w.Resource.ID, Owner: w.Owner, LeaseEpoch: w.Epoch}); errors.Is(err, pgx.ErrNoRows) {
		return biz.ErrLeaseLost
	} else if err != nil {
		return databaseFailure(err)
	}
	if progress.Observed {
		if progress.PlatformNetwork == nil || progress.Identity != w.KnownIdentity {
			return biz.ErrLeaseLost
		}
		facts := progress.PlatformNetwork
		if w.PlatformNetwork.Kind == "vpc" {
			err = affected(q.AdvancePlatformVPC(ctx, sqlcgen.AdvancePlatformVPCParams{VpcID: w.Resource.ID, Version: w.Resource.Version, State: string(progress.State), Reason: string(progress.Reason), Cidr: facts.CIDR}))
		} else {
			err = affected(q.AdvancePlatformSubnet(ctx, sqlcgen.AdvancePlatformSubnetParams{SubnetID: w.Resource.ID, Version: w.Resource.Version, State: string(progress.State), Reason: string(progress.Reason), Cidr: facts.CIDR, Gateway: facts.Gateway, ExcludeIps: append([]string{}, facts.ExcludeIPs...), DnsServers: append([]string{}, facts.DNSServers...), EnableDhcp: facts.EnableDHCP, AvailableIpRange: facts.Available, UsedIpRange: facts.Used}))
		}
		if err != nil {
			return err
		}
	}
	if err = affected(q.FinishPlatformNetworkObservation(ctx, sqlcgen.FinishPlatformNetworkObservationParams{ResourceID: w.Resource.ID, Owner: w.Owner, LeaseEpoch: w.Epoch, ClaimedGeneration: w.ClaimedGeneration, Observed: progress.Observed, ObservationError: string(progress.ObservationError), Backoff: progress.Backoff, DelayMicros: progress.NextDelay.Microseconds()})); err != nil {
		return err
	}
	return databaseFailure(tx.Commit(ctx))
}
