package data

import (
	"context"
	"fmt"
	"net/netip"
	"time"

	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func adoptionMarker(device string) string {
	return "network.ani.io/device-adoption-" + contentHash(device)[:16]
}

// Infrastructure collision checks are read-only and happen outside admission
// transactions. The full ServiceCIDR source is required, not just allocated IPs.
func (p *KCProvider) ValidatePublicPool(ctx context.Context, c biz.PublicPoolConfig) error {
	cidr, err := netip.ParsePrefix(c.CIDR)
	if err != nil {
		return biz.Fail(biz.InvalidArgument, "invalid pool CIDR")
	}
	overlaps := func(raw string) bool {
		p, err := netip.ParsePrefix(raw)
		return err == nil && p.Addr().Is4() && p.Overlaps(cidr)
	}
	subnets, err := p.listAll(ctx, kcSubnets)
	if err != nil {
		return err
	}
	for _, s := range subnets {
		if crString(&s, "spec", "type") != "VPC" && overlaps(crString(&s, "spec", "cidrBlock")) {
			return biz.Fail(biz.ResourceInUse, "pool overlaps provider infrastructure")
		}
	}
	nodes, err := p.listAll(ctx, kcNodes)
	if err != nil {
		return err
	}
	for _, n := range nodes {
		addresses, _, _ := unstructured.NestedSlice(n.Object, "status", "addresses")
		for _, raw := range addresses {
			a, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			s, _ := a["address"].(string)
			ip, err := netip.ParseAddr(s)
			if err == nil && cidr.Contains(ip) {
				return biz.Fail(biz.ResourceInUse, "pool overlaps node infrastructure")
			}
		}
		for _, field := range []string{"podCIDR"} {
			if overlaps(crString(&n, "spec", field)) {
				return biz.Fail(biz.ResourceInUse, "pool overlaps cluster Pod network")
			}
		}
	}
	services, err := p.listAll(ctx, schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "servicecidrs"})
	if err != nil {
		return err
	}
	if len(services) == 0 {
		return biz.Fail(biz.DependencyUnavailable, "Service CIDR facts are unavailable")
	}
	for _, s := range services {
		cidrs, _, _ := unstructured.NestedStringSlice(s.Object, "spec", "cidrs")
		for _, other := range cidrs {
			if overlaps(other) {
				return biz.Fail(biz.ResourceInUse, "pool overlaps cluster Service network")
			}
		}
	}
	return nil
}

func (p *KCProvider) nodeNetworkObjects(ctx context.Context) ([]unstructured.Unstructured, []unstructured.Unstructured, []unstructured.Unstructured, time.Time, error) {
	if p.observation != nil {
		var after time.Time
		refreshed := false
		for {
			if err := ctx.Err(); err != nil {
				return nil, nil, nil, time.Time{}, readFailure(fmt.Errorf("node network audit stopped: %w", err))
			}
			view, err := p.observation.snapshot(ctx, after)
			if err != nil {
				return nil, nil, nil, time.Time{}, readFailure(fmt.Errorf("node network audit collection failed: %w", err))
			}
			nodes, maps, vlans, collected, err := nodeNetworkFromView(view)
			if err != nil {
				return nil, nil, nil, time.Time{}, err
			}
			if err = ctx.Err(); err != nil {
				return nil, nil, nil, time.Time{}, readFailure(fmt.Errorf("node network audit stopped: %w", err))
			}
			if p.observation.valid(view, []string{"node-network"}) {
				return nodes, maps, vlans, collected, nil
			}
			// A related event or Watch boundary can invalidate more than one
			// collection. Recollect within the original caller budget, always
			// after a new DB-clock boundary. Real read failures are not retries.
			if _, bounded := ctx.Deadline(); !bounded && refreshed {
				return nil, nil, nil, time.Time{}, readFailure(fmt.Errorf("node network audit invalidated after one fresh collection"))
			}
			after, err = p.repository.readQueries(ctx).DatabaseTime(ctx)
			if err != nil {
				return nil, nil, nil, time.Time{}, readFailure(fmt.Errorf("node network audit boundary failed: %w", err))
			}
			refreshed = true
		}
	}
	collected, err := p.repository.readQueries(ctx).DatabaseTime(ctx)
	if err != nil {
		return nil, nil, nil, time.Time{}, readFailure(err)
	}
	nodes, err := p.listAll(ctx, kcNodes)
	if err != nil {
		return nil, nil, nil, time.Time{}, err
	}
	maps, err := p.client.Resource(kcConfigMaps).Namespace(kcSystemNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, nil, time.Time{}, readFailure(err)
	}
	vlans, err := p.listAll(ctx, kcVlans)
	if err != nil {
		return nil, nil, nil, time.Time{}, err
	}
	return nodes, maps.Items, vlans, collected, nil
}

func nodeNetworkFromView(view *auditView) ([]unstructured.Unstructured, []unstructured.Unstructured, []unstructured.Unstructured, time.Time, error) {
	for _, gvr := range []schema.GroupVersionResource{kcNodes, kcConfigMaps, kcVlans} {
		if view.indices[gvr] == nil {
			return nil, nil, nil, time.Time{}, readFailure(fmt.Errorf("node network audit is missing %s", gvr.Resource))
		}
	}
	items := func(gvr schema.GroupVersionResource) []unstructured.Unstructured {
		out := []unstructured.Unstructured{}
		for _, raw := range view.indices[gvr].List() {
			out = append(out, *raw.(*unstructured.Unstructured).DeepCopy())
		}
		return out
	}
	return items(kcNodes), items(kcConfigMaps), items(kcVlans), view.collected, nil
}
