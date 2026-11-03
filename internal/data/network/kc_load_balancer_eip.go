package data

import (
	"context"

	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (p *KCProvider) readEIPLoadBalancer(ctx context.Context, e egressResolved, set *egressReadSet) error {
	row, err := p.repository.readQueries(ctx).GetLBInternal(ctx, sqlcgen.GetLBInternalParams{TenantID: e.target.TenantID, LbID: e.lbID})
	if err != nil {
		return readFailure(err)
	}
	// U01 reservation-only rows continue to occupy the EIP. They provide no
	// authority to adopt a Gateway or Service until a product operation exists.
	if row.LastOperationID == nil {
		return nil
	}
	if textValue(row.PublicEipID) != e.target.ResourceID {
		return &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	b, err := p.repository.readQueries(ctx).GetBinding(ctx, sqlcgen.GetBindingParams{TenantID: row.TenantID, ResourceID: row.LbID})
	if err != nil {
		return readFailure(err)
	}
	set.keys = append(set.keys, objectKey(b), "uid:"+b.ProviderUid, "load-balancer:"+b.Namespace+"/"+e.lbID)
	rw := lbWork(row)
	if err = hydrateLBWork(ctx, p.repository.readQueries(ctx), &rw, b); err != nil {
		return readFailure(err)
	}
	w := biz.Work{Resource: rw, BindingID: b.BindingID, KnownIdentity: b.ProviderUid}
	r, err := p.resolveLB(ctx, w)
	if err != nil {
		return err
	}
	obj, err := p.client.Resource(lbGateways).Namespace(b.Namespace).Get(ctx, b.ProviderName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return readFailure(err)
	}
	c := rw.LoadBalancer.Components[0]
	v, err := lbInspect(obj, lbDesiredObject(r, c, biz.LoadBalancerObservation{}), c, r)
	if err != nil {
		return err
	}
	if !v.Matches {
		return &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	if lbEIPBindingConflict(set.objects["self"], r) {
		return &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	set.objects["lb_gateway"] = obj
	svc := lbFind(set.all[kcServices], b.Namespace, b.ProviderName)
	if svc == nil {
		return nil
	}
	if !lbOwnedBy(svc, "Gateway", b.ProviderName, v.Identity) || !lbServiceMatches(svc, r) {
		return &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	prior, err := p.repository.readQueries(ctx).ListLBGenerated(ctx, sqlcgen.ListLBGeneratedParams{TenantID: row.TenantID, LbID: row.LbID})
	if err != nil {
		return readFailure(err)
	}
	for _, g := range prior {
		if g.GatewayUid != v.Identity && g.ReleasedAt != nil {
			continue
		}
		if g.Kind == "Service" && (g.ProviderName != svc.GetName() || g.ProviderUid != string(svc.GetUID()) || g.Namespace != svc.GetNamespace() || g.GatewayUid != v.Identity) {
			return &biz.ProviderError{Kind: biz.ProviderConflict}
		}
	}
	set.lbServiceUID = string(svc.GetUID())
	set.lbBound = lbEIPBound(set.objects["self"], svc, r)
	return nil
}
