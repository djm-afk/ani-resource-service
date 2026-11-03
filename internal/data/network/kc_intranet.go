package data

import (
	"context"
	"net/netip"
	"strings"

	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"
)

var kcServiceCIDRs = schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "servicecidrs"}

func renderIntranetPool(c sqlcgen.NetworkPublicPool) map[string]any {
	excluded := make([]any, len(c.ExcludedIps))
	for i, v := range c.ExcludedIps {
		excluded[i] = v
	}
	return map[string]any{"type": "Intranet", "ipVersion": "IPv4", "cidrBlock": c.Cidr, "gatewayIP": c.OvnGatewayIp, "gateway": kcSystemNamespace + "/" + c.DefaultVpcName, "excludeIPs": excluded, "allowedNamespaces": map[string]any{"from": "All"}, "enableDHCP": false, "natOutgoing": false}
}
func intranetGatewayIdentity(c sqlcgen.NetworkPublicPool, vpc *unstructured.Unstructured) bool {
	return vpc != nil && vpc.GetAPIVersion() == "networking.kubercloud.com/v1" && vpc.GetKind() == "VPC" && vpc.GetNamespace() == kcSystemNamespace && vpc.GetName() == c.DefaultVpcName && c.DefaultVpcUid != "" && string(vpc.GetUID()) == c.DefaultVpcUid && vpc.GetResourceVersion() != "" && !hasField(vpc, "spec", "type")
}
func networksCover(networks []string, raw string) bool {
	target, err := netip.ParsePrefix(raw)
	if err != nil || !target.Addr().Is4() || target != target.Masked() {
		return false
	}
	for _, raw := range networks {
		network, err := netip.ParsePrefix(raw)
		if err == nil && network.Addr().Is4() && network == network.Masked() && network.Bits() > 0 && network.Bits() <= target.Bits() && network.Contains(target.Addr()) {
			return true
		}
	}
	return false
}

// The admin supplies required destination ranges; the adapter reads the actual
// controller configuration and never writes the cluster's routing range list.
func intranetRoutesConfigured(c sqlcgen.NetworkPublicPool, config *unstructured.Unstructured, services []unstructured.Unstructured) bool {
	if config == nil || config.GetNamespace() != kcSystemNamespace || config.GetName() != "kcn-config" || config.GetUID() == "" || config.GetResourceVersion() == "" || config.GetDeletionTimestamp() != nil {
		return false
	}
	var networks []string
	if yaml.UnmarshalStrict([]byte(crString(config, "data", "intranetNetworks")), &networks) != nil || len(networks) == 0 || len(c.IntranetNetworks) == 0 {
		return false
	}
	for _, raw := range networks {
		network, err := netip.ParsePrefix(raw)
		if err != nil || !network.Addr().Is4() || network != network.Masked() || network.Bits() == 0 {
			return false
		}
	}
	for _, required := range c.IntranetNetworks {
		if !networksCover(networks, required) {
			return false
		}
	}
	if len(services) == 0 {
		return false
	}
	for _, service := range services {
		cidrs, _, _ := unstructured.NestedStringSlice(service.Object, "spec", "cidrs")
		if len(cidrs) == 0 {
			return false
		}
		for _, cidr := range cidrs {
			if !networksCover(c.IntranetNetworks, cidr) || !networksCover(networks, cidr) {
				return false
			}
		}
	}
	return true
}

// The default gateway is identified by the controller's actual cluster-router
// argument (or its pinned default), not merely by a same-named VPC object.
func controllerDefaultGateway(c sqlcgen.NetworkPublicPool, objects []unstructured.Unstructured) bool {
	found := false
	for _, pod := range objects {
		if pod.GetNamespace() != kcSystemNamespace || pod.GetLabels()["networking.kubercloud.com/app"] != "controller" {
			continue
		}
		containers, _, _ := unstructured.NestedSlice(pod.Object, "spec", "containers")
		if len(containers) == 0 {
			return false
		}
		matched := false
		for _, raw := range containers {
			container, ok := raw.(map[string]any)
			if !ok {
				return false
			}
			command, _, _ := unstructured.NestedStringSlice(container, "command")
			args, _, _ := unstructured.NestedStringSlice(container, "args")
			tokens := append(command, args...)
			// Controller pods can contain sidecars. Only the actual controller binary
			// supplies platform identity; missing executable evidence stays unknown.
			isController := false
			for _, token := range command {
				if strings.Contains(token, "controller") {
					isController = true
				}
			}
			if !isController {
				for _, token := range args {
					if token == "controller" {
						isController = true
					}
				}
			}
			if !isController {
				continue
			}
			router, namespace := "kcn-cluster", kcSystemNamespace
			for i, token := range tokens {
				if strings.HasPrefix(token, "--cluster-router=") {
					router = strings.TrimPrefix(token, "--cluster-router=")
				}
				if token == "--cluster-router" {
					if i+1 == len(tokens) {
						return false
					}
					router = tokens[i+1]
				}
				if strings.HasPrefix(token, "--system-namespace=") {
					namespace = strings.TrimPrefix(token, "--system-namespace=")
				}
				if token == "--system-namespace" {
					if i+1 == len(tokens) {
						return false
					}
					namespace = tokens[i+1]
				}
			}
			if router != c.DefaultVpcName || namespace != kcSystemNamespace {
				return false
			}
			matched = true
		}
		if !matched {
			return false
		}
		found = true
	}
	return found
}
func intranetInfrastructureReady(c sqlcgen.NetworkPublicPool, gateway, config *unstructured.Unstructured, services, providerPods []unstructured.Unstructured) bool {
	return c.Scope == "intranet" && c.GatewayID == nil && c.Mode == "overlay" && intranetGatewayIdentity(c, gateway) && goodConditions(gateway, "Valid", "Initialized", "Ready") && crInt(gateway, "status", "observedGeneration") == gateway.GetGeneration() && intranetRoutesConfigured(c, config, services) && controllerDefaultGateway(c, providerPods)
}
func addressPoolReady(r egressResolved, pool *unstructured.Unstructured, set egressReadSet) bool {
	if r.pool.Scope != "intranet" {
		return publicPoolReady(r, pool, set.objects["gateway"], set.objects["vlan"])
	}
	return goodConditions(pool, "Valid", "Initialized", "Ready") && crInt(pool, "status", "observedGeneration") == pool.GetGeneration() && matchesEgressSpec(pool, renderIntranetPool(r.pool), "public_pool") && !hasField(pool, "spec", "underlayConfig") && !hasNonNullField(pool, "status", "underlayState") && intranetInfrastructureReady(r.pool, set.objects["default_vpc"], set.objects["intranet_config"], set.all[kcServiceCIDRs], set.all[pods])
}
func allocatedEIP(r egressResolved, eip *unstructured.Unstructured, set egressReadSet) (bool, string) {
	if r.pool.Scope != "intranet" {
		return eipAllocated(r, eip, set.objects["pool"], set.objects["gateway"], set.objects["vlan"])
	}
	address := crString(eip, "spec", "ipAddress")
	ip, err := netip.ParseAddr(address)
	cidr, cidrErr := netip.ParsePrefix(r.pool.Cidr)
	if err != nil || cidrErr != nil || !ip.Is4() || !cidr.Contains(ip) || ip == cidr.Masked().Addr() || !cidr.Contains(ip.Next()) {
		return false, ""
	}
	for _, raw := range r.pool.ExcludedIps {
		lo, hi, ok := strings.Cut(raw, "..")
		if !ok {
			hi = lo
		}
		a, ea := netip.ParseAddr(lo)
		b, eb := netip.ParseAddr(hi)
		if ea != nil || eb != nil || (ip.Compare(a) >= 0 && ip.Compare(b) <= 0) {
			return false, ""
		}
	}
	pool := set.objects["pool"]
	ready := goodConditions(eip, "Valid", "Initialized") && crString(eip, "spec", "subnet") == kcSystemNamespace+"/"+crName(pool) && crString(eip, "status", "gateway") == kcSystemNamespace+"/"+r.pool.DefaultVpcName && addressPoolReady(r, pool, set)
	return ready, address
}
func (p *KCProvider) ValidateIntranetPool(ctx context.Context, c biz.PublicPoolConfig) error {
	if _, err := biz.NormalizeIntranetPool(c); err != nil {
		return err
	}
	if err := p.ValidatePublicPool(ctx, c); err != nil {
		return err
	}
	gateway, err := p.client.Resource(kcVPCs).Namespace(kcSystemNamespace).Get(ctx, c.DefaultVPCName, metav1.GetOptions{})
	if err != nil {
		return biz.Fail(biz.Reason("BASE_CONNECTIVITY_NOT_READY"), "default VPC facts are unavailable")
	}
	config, err := p.client.Resource(kcConfigMaps).Namespace(kcSystemNamespace).Get(ctx, "kcn-config", metav1.GetOptions{})
	if err != nil {
		return biz.Fail(biz.Reason("BASE_CONNECTIVITY_NOT_READY"), "intranet destination configuration is unavailable")
	}
	services, err := p.listAll(ctx, kcServiceCIDRs)
	if err != nil {
		return err
	}
	providerPods, err := p.listAll(ctx, pods)
	if err != nil {
		return err
	}
	pool := sqlcgen.NetworkPublicPool{Scope: "intranet", Mode: "overlay", DefaultVpcName: c.DefaultVPCName, DefaultVpcUid: c.DefaultVPCUID, IntranetNetworks: c.IntranetNetworks}
	if !intranetInfrastructureReady(pool, gateway, config, services, providerPods) || len(providerImages(providerPods)) == 0 {
		return biz.Fail(biz.Reason("BASE_CONNECTIVITY_NOT_READY"), "default VPC identity, route coverage or running provider facts are not ready")
	}
	return nil
}

// Read actual cluster facts for the administrator's creation form. Creation
// still validates the submitted frozen identity, so replacement between GET and
// POST cannot silently select a different gateway.
func (p *KCProvider) GetIntranetPoolDefaults(ctx context.Context) (biz.PublicPoolConfig, error) {
	missing := func() (biz.PublicPoolConfig, error) {
		return biz.PublicPoolConfig{}, biz.Fail(biz.BaseConnectivityNotReady, "default VPC identity, route coverage or running provider facts are not ready")
	}
	gateways, err := p.listAll(ctx, kcVPCs)
	if err != nil {
		return missing()
	}
	config, err := p.client.Resource(kcConfigMaps).Namespace(kcSystemNamespace).Get(ctx, "kcn-config", metav1.GetOptions{})
	if err != nil {
		return missing()
	}
	var networks []string
	if yaml.UnmarshalStrict([]byte(crString(config, "data", "intranetNetworks")), &networks) != nil {
		return missing()
	}
	services, err := p.listAll(ctx, kcServiceCIDRs)
	if err != nil {
		return missing()
	}
	providerPods, err := p.listAll(ctx, pods)
	if err != nil || len(providerImages(providerPods)) == 0 {
		return missing()
	}
	var result biz.PublicPoolConfig
	for j := range gateways {
		gateway := &gateways[j]
		c := sqlcgen.NetworkPublicPool{Scope: "intranet", Mode: "overlay", DefaultVpcName: gateway.GetName(), DefaultVpcUid: string(gateway.GetUID()), IntranetNetworks: networks}
		if intranetInfrastructureReady(c, gateway, config, services, providerPods) {
			if result.DefaultVPCUID != "" {
				return missing()
			}
			result = biz.PublicPoolConfig{DefaultVPCName: c.DefaultVpcName, DefaultVPCUID: c.DefaultVpcUid, IntranetNetworks: networks}
		}
	}
	if result.DefaultVPCUID == "" {
		return missing()
	}
	return result, nil
}

func hasNonNullField(o *unstructured.Unstructured, fields ...string) bool {
	value, found, err := unstructured.NestedFieldNoCopy(objectMap(o), fields...)
	return err != nil || (found && value != nil)
}
