package data

import (
	"fmt"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
)

func lbGatewayListener(l biz.LoadBalancerListener) map[string]any {
	protocol := l.Protocol
	if protocol == "" {
		protocol = "HTTP"
	}
	if protocol == "SSL" {
		protocol = "TLS"
	}
	v := map[string]any{"name": l.Name, "port": int64(l.Port), "protocol": protocol, "allowedRoutes": map[string]any{"namespaces": map[string]any{"from": "Same"}, "kinds": []any{map[string]any{"group": lbRoutes.Group, "kind": lbKind(biz.LoadBalancerRouteKind(l.Protocol))}}}}
	if l.Parameters.Certificate != nil {
		v["tls"] = map[string]any{"mode": "Terminate", "certificateRefs": []any{map[string]any{"group": "", "kind": "Secret", "name": l.Parameters.Certificate.Name}}}
	}
	return v
}

func lbRouteRule(l biz.LoadBalancerListener, refs []any) map[string]any {
	v := map[string]any{}
	if l.Protocol == "" || l.Protocol == "HTTP" || l.Protocol == "HTTPS" {
		v["name"] = "default-rule"
		v["matches"] = []any{map[string]any{"path": map[string]any{"type": "PathPrefix", "value": "/"}}}
	}
	if code := l.Parameters.HTTPSRedirectStatusCode; code != 0 {
		v["filters"] = []any{map[string]any{"type": "RequestRedirect", "requestRedirect": map[string]any{"scheme": "https", "statusCode": int64(code)}}}
	} else if len(refs) > 0 {
		v["backendRefs"] = refs
	}
	if mode := l.Parameters.SessionMode; mode != "" {
		cookie := map[string]any{"lifetimeType": "Session"}
		persistence := map[string]any{"sessionName": "KUBERCLOUD_SESSIONID", "type": "Cookie", "cookieConfig": cookie}
		if mode == "CookieTimeout" {
			cookie["lifetimeType"] = "Permanent"
			persistence["absoluteTimeout"] = fmt.Sprintf("%ds", l.Parameters.CookieExpirySeconds)
		}
		v["sessionPersistence"] = persistence
	}
	return v
}

func lbBackendPolicy(r lbResolved, c biz.LoadBalancerComponent) map[string]any {
	l := lbComponentListener(r.work, c)
	algorithm := l.Parameters.Algorithm
	if algorithm == "" {
		algorithm = "RoundRobin"
	}
	balancer := map[string]any{"type": algorithm}
	if algorithm == "SourceIP" {
		balancer["type"] = "ConsistentHash"
		balancer["consistentHash"] = map[string]any{"type": "SourceIP"}
	}
	target := map[string]any{"group": lbRoutes.Group, "kind": lbKind(biz.LoadBalancerRouteKind(l.Protocol)), "name": lbListenerRouteName(r.work, c.ListenerID)}
	if l.Protocol == "" || l.Protocol == "HTTP" || l.Protocol == "HTTPS" {
		target["sectionName"] = "default-rule"
	}
	v := map[string]any{"targetRefs": []any{target}, "loadBalancer": balancer}
	h := l.Health
	if l.Protocol != "UDP" {
		protocol := h.Protocol
		if protocol == "" {
			protocol = "TCP"
		}
		active := map[string]any{"type": protocol, "interval": fmt.Sprintf("%ds", h.IntervalSeconds), "timeout": fmt.Sprintf("%ds", h.TimeoutSeconds), "unhealthyThreshold": int64(h.UnhealthyThreshold), "healthyThreshold": int64(h.HealthyThreshold)}
		if protocol == "HTTP" {
			http := map[string]any{"path": h.Path}
			if h.Hostname != "" {
				http["hostname"] = h.Hostname
			}
			active["http"] = http
		} else {
			active["tcp"] = map[string]any{}
		}
		if h.Port != 0 {
			active["overrides"] = map[string]any{"port": int64(h.Port)}
		}
		v["healthCheck"] = map[string]any{"panicThreshold": int64(0), "active": active}
	}
	if l.Parameters.GzipEnabled {
		v["compressor"] = []any{map[string]any{"type": "Gzip", "gzip": map[string]any{}}}
	}
	return v
}

func lbUDPHealthPolicy(r lbResolved, c biz.LoadBalancerComponent) map[string]any {
	h := lbComponentListener(r.work, c).Health
	cluster := "udproute/" + r.binding.Namespace + "/" + lbListenerRouteName(r.work, c.ListenerID) + "/rule/-1"
	patch := func(typ, path string, value any) map[string]any {
		return map[string]any{"type": "type.googleapis.com/envoy.config." + typ, "name": cluster, "operation": map[string]any{"op": "add", "path": path, "value": value}}
	}
	patches := []any{patch("cluster.v3.Cluster", "/health_checks", []any{map[string]any{"timeout": fmt.Sprintf("%ds", h.TimeoutSeconds), "interval": fmt.Sprintf("%ds", h.IntervalSeconds), "unhealthy_threshold": int64(h.UnhealthyThreshold), "healthy_threshold": int64(h.HealthyThreshold), "tcp_health_check": map[string]any{}}})}
	if h.Port != 0 {
		endpoint := patch("endpoint.v3.ClusterLoadAssignment", "/health_check_config", map[string]any{"port_value": int64(h.Port)})
		endpoint["operation"].(map[string]any)["jsonPath"] = "$.endpoints[*].lb_endpoints[*].endpoint"
		patches = append(patches, endpoint)
	}
	patches = append(patches, patch("cluster.v3.Cluster", "/common_lb_config/healthy_panic_threshold", map[string]any{"value": int64(0)}))
	return map[string]any{"targetRef": map[string]any{"group": lbGateways.Group, "kind": "Gateway", "name": r.binding.ProviderName}, "type": "JSONPatch", "jsonPatches": patches}
}
