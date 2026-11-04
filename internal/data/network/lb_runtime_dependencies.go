package data

import (
	"context"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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
	if p.networkProvider != nil {
		_, err := p.networkProvider.currentNetworkResource(ctx, q, tenant, vpcID)
		return err
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

// The original placement/UID and current configuration acknowledgement are
// the admission authority. No unrelated topology inventory is needed.
func (p *KCProvider) currentNetworkResource(ctx context.Context, q *sqlcgen.Queries, tenant, id string) (biz.ProviderObservation, error) {
	b, err := q.GetBinding(ctx, sqlcgen.GetBindingParams{TenantID: tenant, ResourceID: id})
	if err != nil {
		return biz.ProviderObservation{}, databaseFailure(err)
	}
	if b.ClusterID != p.repository.placement.ClusterID || b.ProviderUid == "" || b.PendingAction != "" {
		return biz.ProviderObservation{}, biz.Fail(biz.ProviderOwnership, "network binding is unresolved")
	}
	t := biz.ProviderTarget{TenantID: tenant, ResourceID: id, BindingID: b.BindingID, KnownIdentity: b.ProviderUid}
	parentRef := ""
	if b.ResourceKind == "vpc" {
		v, err := q.GetVPC(ctx, sqlcgen.GetVPCParams{TenantID: tenant, VpcID: id})
		if err != nil {
			return biz.ProviderObservation{}, databaseFailure(err)
		}
		if !networkManagementOpen(v.NetworkVpc.State, v.NetworkVpc.Reason) {
			return biz.ProviderObservation{}, biz.Fail(biz.ParentNotReady, "VPC management has not completed or is closed")
		}
		t.CIDR = v.NetworkVpc.Cidr
	} else if b.ResourceKind == "subnet" {
		s, err := q.GetSubnet(ctx, sqlcgen.GetSubnetParams{TenantID: tenant, SubnetID: id})
		if err != nil {
			return biz.ProviderObservation{}, databaseFailure(err)
		}
		if !networkManagementOpen(s.State, s.Reason) {
			return biz.ProviderObservation{}, biz.Fail(biz.ParentNotReady, "Subnet management has not completed or is closed")
		}
		t.CIDR, t.Gateway = s.Cidr, s.Gateway
		t.SubnetOptions = &biz.SubnetOptions{ExcludeIPs: s.ExcludeIps, EnableDHCP: s.EnableDhcp, DNSServers: s.DnsServers}
		parent, err := q.GetBinding(ctx, sqlcgen.GetBindingParams{TenantID: tenant, ResourceID: s.VpcID})
		if err != nil {
			return biz.ProviderObservation{}, databaseFailure(err)
		}
		if parent.ResourceKind != "vpc" || parent.ClusterID != b.ClusterID || parent.Namespace != b.Namespace || parent.ProviderUid == "" || parent.PendingAction != "" {
			return biz.ProviderObservation{}, biz.Fail(biz.ProviderOwnership, "parent network binding is unresolved")
		}
		parentRef = parent.Namespace + "/" + parent.ProviderName
	} else {
		return biz.ProviderObservation{}, biz.Fail(biz.ProviderOwnership, "network resource kind differs")
	}
	if err := p.verifyTenantNamespace(ctx, b, true); err != nil {
		return biz.ProviderObservation{}, err
	}
	obj, err := p.client.Resource(kcResource(b)).Namespace(b.Namespace).Get(ctx, b.ProviderName, metav1.GetOptions{})
	if err != nil {
		return biz.ProviderObservation{}, readFailure(err)
	}
	observation, err := inspectResource(obj, b, t, parentRef)
	if err != nil {
		return observation, err
	}
	if !observation.Ready {
		return observation, biz.Fail(biz.ParentNotReady, "network has not confirmed the current configuration")
	}
	return observation, nil
}
