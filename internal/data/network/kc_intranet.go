package data

import (
	"context"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
)

func renderIntranetPool(c sqlcgen.NetworkPublicPool) map[string]any {
	excluded := make([]any, len(c.ExcludedIps))
	for i, v := range c.ExcludedIps {
		excluded[i] = v
	}
	return map[string]any{"type": "Intranet", "ipVersion": "IPv4", "cidrBlock": c.Cidr, "gatewayIP": c.OvnGatewayIp, "gateway": kcSystemNamespace + "/" + c.DefaultVpcName, "excludeIPs": excluded, "allowedNamespaces": map[string]any{"from": "All"}, "enableDHCP": false, "natOutgoing": false}
}

// Defaults are an explicit administrator query, not a resource admission gate.
func (p *KCProvider) GetIntranetPoolDefaults(ctx context.Context) (biz.PublicPoolConfig, error) {
	objects, err := p.listScoped(ctx, kcVPCs, kcSystemNamespace, "")
	if err != nil {
		return biz.PublicPoolConfig{}, readFailure(err)
	}
	result := biz.PublicPoolConfig{IntranetNetworks: []string{}}
	for _, object := range objects {
		if !crBool(&object, "status", "default") {
			continue
		}
		if result.DefaultVPCName != "" {
			return result, biz.Fail(biz.ResourceBusy, "multiple default VPC references are reported")
		}
		result.DefaultVPCName = object.GetName()
		result.DefaultVPCUID = string(object.GetUID())
	}
	if result.DefaultVPCName == "" {
		return result, biz.Fail(biz.ResourceNotFound, "no default VPC reference is reported")
	}
	return result, nil
}
