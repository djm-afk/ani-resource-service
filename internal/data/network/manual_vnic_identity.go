package data

import (
	"context"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// manualVNicIdentity validates the retained product and consumer relationship.
// A historical released relation is sufficient only for cleanup observation;
// accepting a live endpoint always requires an active reservation.
func (p *KCProvider) manualVNicIdentity(ctx context.Context, tenant, consumer, namespace, subnetID string, o unstructured.Unstructured, live bool) (bool, error) {
	if o.GetLabels()["network.ani.io/vnic-id"] == "" {
		return false, nil
	}
	var matches bool
	err := p.repository.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM network_vnics v JOIN network_provider_bindings p ON p.tenant_id=v.tenant_id AND p.vnic_id=v.vnic_id JOIN network_vnic_bindings b ON b.tenant_id=v.tenant_id AND b.vnic_id=v.vnic_id WHERE v.tenant_id=$1 AND b.consumer_id=$2 AND p.namespace=$3 AND ($4='' OR v.subnet_id=$4) AND p.provider_name=$5 AND p.provider_uid=$6 AND v.vnic_id=$7 AND (NOT $8 OR b.released_at IS NULL))`, tenant, consumer, namespace, subnetID, o.GetName(), string(o.GetUID()), o.GetLabels()["network.ani.io/vnic-id"], live).Scan(&matches)
	if err != nil {
		return false, readFailure(err)
	}
	if !matches || len(o.GetOwnerReferences()) != 0 || o.GetLabels()[tenantLabel] != tenant || o.GetLabels()[ownerLabel] != "ani-network-service" {
		return false, &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	return true, nil
}
