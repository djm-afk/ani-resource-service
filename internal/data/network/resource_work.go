package data

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
	"time"
)

func vpcWork(v sqlcgen.NetworkVpc) biz.ResourceWork {
	return biz.ResourceWork{ID: v.VpcID, TenantID: v.TenantID, Kind: "vpc", CIDR: v.Cidr, State: biz.ResourceState(v.State), Reason: biz.Reason(v.Reason), Version: v.Version, UpdatedAt: v.UpdatedAt, ObservedAt: v.ObservedAt, LastOperationID: v.LastOperationID}
}
func subnetWork(s sqlcgen.NetworkSubnet) biz.ResourceWork {
	return biz.ResourceWork{ID: s.SubnetID, TenantID: s.TenantID, Kind: "subnet", VPCID: s.VpcID, CIDR: s.Cidr, Gateway: s.Gateway, State: biz.ResourceState(s.State), Reason: biz.Reason(s.Reason), Version: s.Version, UpdatedAt: s.UpdatedAt, ObservedAt: s.ObservedAt, LastOperationID: s.LastOperationID, SubnetOptions: &biz.SubnetOptions{ExcludeIPs: s.ExcludeIps, EnableDHCP: s.EnableDhcp, DNSServers: s.DnsServers}}
}
func claimResource(ctx context.Context, q *sqlcgen.Queries) (biz.ResourceWork, error) {
	rows, err := q.DueResourceCandidates(ctx)
	if err != nil {
		return biz.ResourceWork{}, err
	}
	for _, c := range rows {
		var v sqlcgen.NetworkVpc
		if c.ParentVpcID != "" {
			v, err = q.TryLockWorkVPC(ctx, sqlcgen.TryLockWorkVPCParams{TenantID: c.TenantID, VpcID: c.ParentVpcID})
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			if err != nil {
				return biz.ResourceWork{}, err
			}
		}
		var e sqlcgen.NetworkEip
		if c.ParentEipID != "" {
			e, err = q.TryLockWorkEIP(ctx, sqlcgen.TryLockWorkEIPParams{TenantID: c.TenantID, EipID: c.ParentEipID})
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			if err != nil {
				return biz.ResourceWork{}, err
			}
		}
		switch c.ResourceKind {
		case "vnic":
			r, err := q.TryLockWorkVNic(ctx, sqlcgen.TryLockWorkVNicParams{TenantID: c.TenantID, VnicID: c.ResourceID})
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			return vnicWork(r), err
		case "load_balancer":
			return lockLBWork(ctx, q, c.TenantID, c.ResourceID)
		case "vpc":
			return vpcWork(v), nil
		case "subnet":
			s, err := q.LockSubnet(ctx, sqlcgen.LockSubnetParams{TenantID: c.TenantID, SubnetID: c.ResourceID})
			return subnetWork(s), err
		case "eip":
			return eipWork(e), nil
		case "snat":
			s, err := q.TryLockWorkSnat(ctx, sqlcgen.TryLockWorkSnatParams{TenantID: c.TenantID, SnatID: c.ResourceID})
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			if err != nil {
				return biz.ResourceWork{}, err
			}
			return snatWork(s), nil
		default:
			return biz.ResourceWork{}, biz.ErrLeaseLost
		}
	}
	return biz.ResourceWork{}, pgx.ErrNoRows
}
func eipWork(e sqlcgen.NetworkEip) biz.ResourceWork {
	return biz.ResourceWork{ID: e.EipID, TenantID: e.TenantID, Kind: "eip", VPCID: textValue(e.SystemOwnerVpc), SystemManaged: e.ManagedBy == "system", State: biz.ResourceState(e.State), Reason: biz.Reason(e.Reason), Version: e.Version, UpdatedAt: e.UpdatedAt, ObservedAt: e.ObservedAt, LastOperationID: e.LastOperationID, Egress: &biz.EgressWorkSpec{PoolID: e.PoolID}}
}
func snatWork(s sqlcgen.NetworkSnatBinding) biz.ResourceWork {
	return biz.ResourceWork{ID: s.SnatID, TenantID: s.TenantID, Kind: "snat", VPCID: s.VpcID, SystemManaged: s.Purpose == "intranet", State: biz.ResourceState(s.State), Reason: biz.Reason(s.Reason), Version: s.Version, UpdatedAt: s.UpdatedAt, ObservedAt: s.ObservedAt, LastOperationID: s.LastOperationID, Egress: &biz.EgressWorkSpec{EIPID: s.EipID, DesiredEnabled: s.DesiredEnabled}}
}
func lockResource(ctx context.Context, q *sqlcgen.Queries, r biz.ResourceWork) (biz.ResourceWork, error) {
	if r.Kind == "vnic" {
		if r.Reference.OwnerScope != biz.OwnerScopePlatform {
			if _, err := q.LockVPC(ctx, sqlcgen.LockVPCParams{TenantID: r.TenantID, VpcID: r.VPCID}); err != nil {
				return biz.ResourceWork{}, err
			}
		}
		v, err := q.LockVNicRow(ctx, sqlcgen.LockVNicRowParams{TenantID: r.TenantID, VnicID: r.ID})
		return vnicWork(v), err
	}
	if r.Kind == "load_balancer" {
		return lockLBWork(ctx, q, r.TenantID, r.ID)
	}
	if r.Kind == "eip" {
		if r.VPCID != "" {
			if _, err := q.LockVPC(ctx, sqlcgen.LockVPCParams{TenantID: r.TenantID, VpcID: r.VPCID}); err != nil {
				return biz.ResourceWork{}, err
			}
		}
		e, err := q.LockEIP(ctx, sqlcgen.LockEIPParams{TenantID: r.TenantID, EipID: r.ID})
		return eipWork(e), err
	}
	if r.Kind == "snat" {
		if r.Egress == nil {
			return biz.ResourceWork{}, biz.ErrLeaseLost
		}
		if _, err := q.LockVPC(ctx, sqlcgen.LockVPCParams{TenantID: r.TenantID, VpcID: r.VPCID}); err != nil {
			return biz.ResourceWork{}, err
		}
		if _, err := q.LockEIP(ctx, sqlcgen.LockEIPParams{TenantID: r.TenantID, EipID: r.Egress.EIPID}); err != nil {
			return biz.ResourceWork{}, err
		}
		s, err := q.LockSnat(ctx, sqlcgen.LockSnatParams{TenantID: r.TenantID, SnatID: r.ID})
		if err == nil && (s.VpcID != r.VPCID || s.EipID != r.Egress.EIPID) {
			return biz.ResourceWork{}, biz.ErrLeaseLost
		}
		return snatWork(s), err
	}

	parent := r.ID
	if r.Kind == "subnet" {
		parent = r.VPCID
	}
	v, err := q.LockVPC(ctx, sqlcgen.LockVPCParams{TenantID: r.TenantID, VpcID: parent})
	if err != nil {
		return biz.ResourceWork{}, err
	}
	if r.Kind == "vpc" {
		return vpcWork(v), nil
	}
	if r.Kind != "subnet" {
		return biz.ResourceWork{}, biz.ErrLeaseLost
	}
	s, err := q.LockSubnet(ctx, sqlcgen.LockSubnetParams{TenantID: r.TenantID, SubnetID: r.ID})
	if err == nil && s.VpcID != parent {
		return biz.ResourceWork{}, biz.ErrLeaseLost
	}
	return subnetWork(s), err
}
func advanceResource(ctx context.Context, q *sqlcgen.Queries, r biz.ResourceWork, p biz.Progress, observedAt *time.Time) (biz.ResourceWork, error) {
	if r.Kind == "vnic" {
		f := biz.VNicFacts{}
		if p.VNic != nil {
			f = *p.VNic
		}
		v, err := q.AdvanceVNic(ctx, sqlcgen.AdvanceVNicParams{TenantID: r.TenantID, VnicID: r.ID, Version: r.Version, State: string(p.State), Reason: string(p.Reason), Observed: p.Observed, ObservedAt: observedAt, MacAddress: f.MACAddress, PrimaryIp: f.PrimaryIP, Ips: append([]string{}, f.IPs...)})
		return vnicWork(v), err
	}

	if r.Kind == "load_balancer" {
		state := "unknown"
		applied := int64(0)
		if p.LoadBalancer != nil && p.Observed {
			state = p.LoadBalancer.ConfigurationState
			applied = p.LoadBalancer.AppliedVersion
		}
		v, err := q.AdvanceLB(ctx, sqlcgen.AdvanceLBParams{TenantID: r.TenantID, LbID: r.ID, Version: r.Version, State: string(p.State), Reason: string(p.Reason), Observed: p.Observed, ObservedAt: observedAt, ConfigurationState: state, AppliedVersion: applied})
		return lbWork(v), err
	}
	facts := biz.EgressAppliedFacts{}
	if p.Egress != nil && p.Observed {
		facts = *p.Egress
	}
	if r.Kind == "eip" {
		e, err := q.AdvanceEIP(ctx, sqlcgen.AdvanceEIPParams{TenantID: r.TenantID, EipID: r.ID, Version: r.Version, State: string(p.State), Reason: string(p.Reason), Observed: p.Observed, ObservedAt: observedAt, Address: facts.Address})
		return eipWork(e), err
	}
	if r.Kind == "snat" {
		applied := pgtype.Bool{}
		if facts.AppliedEnabled != nil {
			applied = pgtype.Bool{Bool: *facts.AppliedEnabled, Valid: true}
		}
		s, err := q.AdvanceSnat(ctx, sqlcgen.AdvanceSnatParams{TenantID: r.TenantID, SnatID: r.ID, Version: r.Version, State: string(p.State), Reason: string(p.Reason), Observed: p.Observed, ObservedAt: observedAt, AppliedEnabled: applied})
		return snatWork(s), err
	}

	if r.Kind == "subnet" {
		s, err := q.AdvanceSubnet(ctx, sqlcgen.AdvanceSubnetParams{TenantID: r.TenantID, SubnetID: r.ID, Version: r.Version, State: string(p.State), Reason: string(p.Reason), Observed: p.Observed, ObservedAt: observedAt, AvailableIpRange: p.SubnetIPRanges.Available, UsedIpRange: p.SubnetIPRanges.Used})
		return subnetWork(s), err
	}
	v, err := q.AdvanceVPC(ctx, sqlcgen.AdvanceVPCParams{TenantID: r.TenantID, VpcID: r.ID, Version: r.Version, State: string(p.State), Reason: string(p.Reason), Observed: p.Observed, ObservedAt: observedAt})
	return vpcWork(v), err
}
