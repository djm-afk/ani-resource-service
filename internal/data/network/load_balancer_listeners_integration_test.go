package data_test

import (
	"github.com/google/uuid"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"reflect"
	"testing"
)

// Reuses the actual admission, PostgreSQL, worker and KC adapter. Only the
// instance owner and Kubernetes/controller protocol boundary are controlled.
func TestLBMultipleListenersMainFlow(t *testing.T) {
	c := &lbControllerFixture{}
	f := newLBAdmissionFixture(t, c.http)
	second := addLBBackend(t, f, f.backendSubnet, "lb-second", "10.42.2.3")
	second.Port = 9090 // Independent backend and health port on this listener.
	port := func(v uint32) *uint32 { return &v }
	listener := func(name string, p uint32, backend biz.LoadBalancerBackendInput) biz.LoadBalancerListenerInput {
		return biz.LoadBalancerListenerInput{Name: name, Protocol: "HTTP", Port: port(p), Backends: []biz.LoadBalancerBackendInput{backend}, Health: biz.LoadBalancerHealthInput{Port: port(backend.Port)}}
	}
	r := f.request
	r.LoadBalancerMutableInput = biz.LoadBalancerMutableInput{Name: "two-listeners", Listeners: []biz.LoadBalancerListenerInput{listener("first", 80, f.request.Backends[0]), listener("second", 8081, second)}}
	created, err := f.lbs.Create(f.f.ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	lb := lbState(t, f, created.LoadBalancer.ID, biz.Available)
	if lb.AppliedVersion != 1 || len(lb.Listeners) != 2 || lb.Listeners[0].Backends[0].Address == lb.Listeners[1].Backends[0].Address {
		t.Fatal("independent listeners did not configure", lb)
	}
	var namespace, gateway, gatewayUID string
	if err = f.f.owner.QueryRow(f.f.ctx, `SELECT namespace,provider_name,provider_uid FROM network_provider_bindings WHERE tenant_id=$1 AND lb_id=$2 AND resource_kind='load_balancer'`, f.f.tenant, lb.ID).Scan(&namespace, &gateway, &gatewayUID); err != nil {
		t.Fatal(err)
	}
	firstID, secondID := lb.Listeners[0].ID, lb.Listeners[1].ID
	checkRoutes := func(want int) {
		t.Helper()
		rows, err := f.f.owner.Query(f.f.ctx, `SELECT listener_id,provider_name FROM network_lb_components WHERE tenant_id=$1 AND lb_id=$2 AND kind='route' AND deleted_at IS NULL`, f.f.tenant, lb.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		count := 0
		for rows.Next() {
			var id, name string
			if err = rows.Scan(&id, &name); err != nil {
				t.Fatal(err)
			}
			obj := f.api.Object("httproutes", namespace, name)
			if obj == nil {
				t.Fatal("missing listener route", id)
			}
			spec := obj["spec"].(map[string]any)
			section := spec["parentRefs"].([]any)[0].(map[string]any)["sectionName"]
			found := false
			for _, l := range lb.Listeners {
				if l.ID == id {
					found = section == l.Name
				}
			}
			if !found {
				t.Fatal("route lost listener binding", id, section)
			}
			count++
		}
		if rows.Err() != nil || count != want {
			t.Fatal("incomplete route set", count, want, rows.Err())
		}
	}
	checkRoutes(2)
	// Withdrawing one current consumer address affects only its listener route.
	var firstRoute, secondRoute string
	if err = f.f.owner.QueryRow(f.f.ctx, `SELECT provider_name FROM network_lb_components WHERE tenant_id=$1 AND lb_id=$2 AND kind='route' AND listener_id=$3`, f.f.tenant, lb.ID, firstID).Scan(&firstRoute); err != nil {
		t.Fatal(err)
	}
	if err = f.f.owner.QueryRow(f.f.ctx, `SELECT provider_name FROM network_lb_components WHERE tenant_id=$1 AND lb_id=$2 AND kind='route' AND listener_id=$3`, f.f.tenant, lb.ID, secondID).Scan(&secondRoute); err != nil {
		t.Fatal(err)
	}
	firstBackend := lb.Listeners[0].Backends[0]
	setLBCurrentBackendAddresses(t, f, lb.ID, firstBackend.AttachmentID, []string{})
	f.f.drive(t, func() bool {
		lb, err = f.lbs.Get(f.f.ctx, "", lb.ID)
		firstObj, secondObj := f.api.Object("httproutes", namespace, firstRoute), f.api.Object("httproutes", namespace, secondRoute)
		if err != nil || firstObj == nil || secondObj == nil {
			return false
		}
		firstRule := firstObj["spec"].(map[string]any)["rules"].([]any)[0].(map[string]any)
		secondRule := secondObj["spec"].(map[string]any)["rules"].([]any)[0].(map[string]any)
		return lb.State == biz.Degraded && firstRule["backendRefs"] == nil && secondRule["backendRefs"] != nil
	})
	if lb.AppliedVersion != 1 {
		t.Fatal("backend identity loss changed applied version")
	}
	setLBCurrentBackendAddresses(t, f, lb.ID, firstBackend.AttachmentID, []string{firstBackend.Address})
	lb = lbState(t, f, lb.ID, biz.Available)
	// A masked metadata update omits the listener set and preserves its identity.
	metadata, err := f.lbs.Update(f.f.ctx, biz.UpdateLoadBalancer{ID: lb.ID, ExpectedVersion: lb.Version, IdempotencyKey: "rename-only", LoadBalancerMutableInput: biz.LoadBalancerMutableInput{Name: "renamed", UpdateMask: []string{"name"}}})
	if err != nil {
		t.Fatal(err)
	}
	f.f.drive(t, func() bool {
		lb, err = f.lbs.Get(f.f.ctx, "", lb.ID)
		return err == nil && lb.AppliedVersion == metadata.LoadBalancer.ProviderTargetVersion && lb.ConfigurationState == "configured"
	})
	if metadata.LoadBalancer.ProviderTargetVersion != 1 || lb.DesiredVersion != 2 {
		t.Fatal("metadata edit changed the Provider target or lost the complete configuration version", lb)
	}
	if len(lb.Listeners) != 2 || lb.Listeners[0].ID != firstID || lb.Listeners[1].ID != secondID || lb.Listeners[0].Port != 80 || lb.Listeners[1].Port != 8081 {
		t.Fatal("omitted listener set changed", lb.Listeners)
	}
	update := func(key string, listeners []biz.LoadBalancerListenerInput) {
		t.Helper()
		accepted, err := f.lbs.Update(f.f.ctx, biz.UpdateLoadBalancer{ID: lb.ID, ExpectedVersion: lb.Version, IdempotencyKey: key, LoadBalancerMutableInput: biz.LoadBalancerMutableInput{Listeners: listeners, UpdateMask: []string{"listeners"}}})
		if err != nil {
			t.Fatal(err)
		}
		f.f.drive(t, func() bool {
			lb, err = f.lbs.Get(f.f.ctx, "", lb.ID)
			return err == nil && lb.AppliedVersion == accepted.LoadBalancer.DesiredVersion && lb.ConfigurationState == "configured"
		})
		obj := f.api.Object("gateways", namespace, gateway)
		if obj == nil || obj["metadata"].(map[string]any)["uid"] != gatewayUID {
			t.Fatal("listener update replaced the Gateway")
		}
	}
	first := listener("first", 80, f.request.Backends[0])
	first.ID = firstID
	first.Backends[0].ID = lb.Listeners[0].Backends[0].ID
	changed := listener("second", 8082, second)
	changed.ID = secondID
	changed.Health.IntervalSeconds = port(7)
	third := listener("third", 8083, f.request.Backends[0])
	update("add-and-change", []biz.LoadBalancerListenerInput{first, changed, third})
	if len(lb.Listeners) != 3 || lb.Listeners[1].ID != secondID || lb.Listeners[1].Port != 8082 || lb.Listeners[1].Health.IntervalSeconds != 7 {
		t.Fatal("listener update lost identity or health", lb.Listeners)
	}
	checkRoutes(3)
	third.ID = lb.Listeners[2].ID
	update("remove-second", []biz.LoadBalancerListenerInput{first, third})
	if len(lb.Listeners) != 2 || lb.Listeners[0].ID != firstID {
		t.Fatal("removing one listener changed another", lb.Listeners)
	}
	checkRoutes(2)
	if _, err = f.lbs.Delete(f.f.ctx, "", lb.ID); err != nil {
		t.Fatal(err)
	}
	lbState(t, f, lb.ID, biz.Deleted)
	var active int
	if err = f.f.owner.QueryRow(f.f.ctx, `SELECT (SELECT count(*) FROM network_lb_subnet_refs WHERE tenant_id=$1 AND lb_id=$2 AND released_at IS NULL)+(SELECT count(*) FROM network_lb_vip_intents WHERE tenant_id=$1 AND lb_id=$2 AND released_at IS NULL)+(SELECT count(*) FROM network_lb_components WHERE tenant_id=$1 AND lb_id=$2 AND deleted_at IS NULL)`, f.f.tenant, lb.ID).Scan(&active); err != nil || active != 0 {
		t.Fatal("LB cleanup leaked occupancy or components", active, err)
	}
}

// Parameters traverse the production admission/PG/worker path. CRD/controller
// status and the tenant TLS Secret are the only controlled external facts.
func TestLBListenerParametersMainFlow(t *testing.T) {
	c := &lbControllerFixture{}
	f := newLBAdmissionFixture(t, c.http)
	port := func(v uint32) *uint32 { return &v }
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	equal := func(got, want any) {
		t.Helper()
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %#v; want %#v", got, want)
		}
	}
	var ns string
	check(f.f.owner.QueryRow(f.f.ctx, `SELECT namespace FROM network_provider_bindings WHERE tenant_id=$1 AND subnet_id=$2 AND resource_kind='subnet'`, f.f.tenant, f.subnet.ID).Scan(&ns))
	f.api.Change("secrets", map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": "tenant-tls", "namespace": ns, "uid": uuid.NewString(), "labels": map[string]any{"network.ani.io/tenant-id": f.f.tenant}}, "type": "kubernetes.io/tls", "data": map[string]any{"tls.crt": "external-cert", "tls.key": "external-key"}}, false)
	listener := func(name, protocol string, p uint32) biz.LoadBalancerListenerInput {
		return biz.LoadBalancerListenerInput{Name: name, Protocol: protocol, Port: port(p), Backends: f.request.Backends, Health: biz.LoadBalancerHealthInput{Port: port(0)}}
	}
	a := listener("a-http", "HTTP", 80)
	a.Parameters = biz.LoadBalancerListenerParameters{Algorithm: "LeastRequest", SessionMode: "CookieTimeout", CookieExpirySeconds: 600, MaxConnections: 100, GzipEnabled: true}
	a.Health = biz.LoadBalancerHealthInput{Protocol: "HTTP", Path: "/ready", Port: port(9090), IntervalSeconds: port(2), TimeoutSeconds: port(5), HealthyThreshold: port(2), UnhealthyThreshold: port(4)}
	b := listener("b-https", "HTTPS", 443)
	b.Parameters = biz.LoadBalancerListenerParameters{Algorithm: "SourceIP", Certificate: &biz.LoadBalancerCertificateRef{Name: "tenant-tls"}, SessionMode: "CookieSession", MaxConnections: 200, GzipEnabled: true}
	tcp := listener("c-tcp", "TCP", 3001)
	tcp.Parameters = biz.LoadBalancerListenerParameters{Algorithm: "LeastRequest", MaxConnections: 300}
	udp := listener("d-udp", "UDP", 3002)
	udp.Health.Port = port(9091)
	udp.Parameters.Algorithm = "SourceIP"
	ssl := listener("e-ssl", "SSL", 3003)
	ssl.Parameters = biz.LoadBalancerListenerParameters{Certificate: &biz.LoadBalancerCertificateRef{Name: "tenant-tls", Namespace: ns}, MaxConnections: 400}
	r := f.request
	r.LoadBalancerMutableInput = biz.LoadBalancerMutableInput{Name: "parameters", Listeners: []biz.LoadBalancerListenerInput{a, b, tcp, udp, ssl}}
	bad := r
	bad.IdempotencyKey = "foreign-cert"
	bad.Listeners = []biz.LoadBalancerListenerInput{b}
	bad.Listeners[0].Parameters.Certificate = &biz.LoadBalancerCertificateRef{Name: "tenant-tls", Namespace: "foreign"}
	if _, err := f.lbs.Create(f.f.ctx, bad); err == nil {
		t.Fatal("cross-namespace certificate accepted")
	}
	created, err := f.lbs.Create(f.f.ctx, r)
	check(err)
	lb := lbState(t, f, created.LoadBalancer.ID, biz.Available)
	equal(len(lb.Listeners), 5)
	equal(lb.AppliedVersion, int64(1))
	equal(lb.Listeners[0].Parameters, a.Parameters)
	equal(lb.Listeners[0].Health.Port, uint32(9090))
	equal(lb.Listeners[0].Health.TimeoutSeconds, uint32(5))
	equal(lb.Listeners[1].Parameters.Certificate.Namespace, ns)
	replay, err := f.lbs.Create(f.f.ctx, r)
	check(err)
	equal(replay.LoadBalancer.ID, lb.ID)
	obj := func(listenerID, kind string) map[string]any {
		t.Helper()
		var name string
		check(f.f.owner.QueryRow(f.f.ctx, `SELECT provider_name FROM network_lb_components WHERE tenant_id=$1 AND lb_id=$2 AND listener_id=$3 AND kind=$4 AND deleted_at IS NULL`, f.f.tenant, lb.ID, listenerID, kind).Scan(&name))
		resource := map[string]string{"route": "httproutes", "tcp_route": "tcproutes", "udp_route": "udproutes", "policy": "backendtrafficpolicies", "client_policy": "clienttrafficpolicies", "patch_policy": "envoypatchpolicies"}[kind]
		value := f.api.Object(resource, ns, name)
		if value == nil {
			t.Fatalf("missing %s", kind)
		}
		return value["spec"].(map[string]any)
	}
	rule := func(id, kind string) map[string]any { return obj(id, kind)["rules"].([]any)[0].(map[string]any) }
	first := lb.Listeners[0].ID
	httpRule := rule(first, "route")
	equal(httpRule["name"], "default-rule")
	persistence := httpRule["sessionPersistence"].(map[string]any)
	equal(persistence["absoluteTimeout"], "600s")
	equal(persistence["cookieConfig"].(map[string]any)["lifetimeType"], "Permanent")
	policy := obj(first, "policy")
	target := policy["targetRefs"].([]any)[0].(map[string]any)
	equal(target["sectionName"], "default-rule")
	equal(target["kind"], "HTTPRoute")
	equal(policy["loadBalancer"].(map[string]any)["type"], "LeastRequest")
	equal(policy["compressor"].([]any)[0].(map[string]any)["type"], "Gzip")
	active := policy["healthCheck"].(map[string]any)["active"].(map[string]any)
	equal(active["overrides"].(map[string]any)["port"], float64(9090))
	equal(active["interval"], "2s")
	equal(active["timeout"], "5s")
	equal(active["http"].(map[string]any)["path"], "/ready")
	equal(active["http"].(map[string]any)["hostname"], nil)
	connection := obj(first, "client_policy")
	equal(connection["targetRefs"].([]any)[0].(map[string]any)["sectionName"], "a-http")
	equal(connection["connection"].(map[string]any)["connectionLimit"].(map[string]any)["value"], float64(100))
	equal(rule(lb.Listeners[1].ID, "route")["sessionPersistence"].(map[string]any)["cookieConfig"].(map[string]any)["lifetimeType"], "Session")
	equal(obj(lb.Listeners[1].ID, "policy")["loadBalancer"].(map[string]any)["consistentHash"].(map[string]any)["type"], "SourceIP")
	equal(rule(lb.Listeners[2].ID, "tcp_route")["matches"], nil)
	equal(obj(lb.Listeners[2].ID, "policy")["healthCheck"].(map[string]any)["active"].(map[string]any)["overrides"], nil)
	equal(obj(lb.Listeners[3].ID, "policy")["healthCheck"], nil)
	patches := obj(lb.Listeners[3].ID, "patch_policy")["jsonPatches"].([]any)
	equal(len(patches), 3)
	equal(patches[1].(map[string]any)["operation"].(map[string]any)["value"].(map[string]any)["port_value"], float64(9091))
	equal(rule(lb.Listeners[4].ID, "tcp_route")["matches"], nil)
	var gateway string
	check(f.f.owner.QueryRow(f.f.ctx, `SELECT provider_name FROM network_provider_bindings WHERE tenant_id=$1 AND lb_id=$2 AND resource_kind='load_balancer'`, f.f.tenant, lb.ID).Scan(&gateway))
	gl := f.api.Object("gateways", ns, gateway)["spec"].(map[string]any)["listeners"].([]any)
	equal(gl[4].(map[string]any)["protocol"], "TLS")
	equal(gl[4].(map[string]any)["tls"].(map[string]any)["mode"], "Terminate")
	equal(gl[1].(map[string]any)["tls"].(map[string]any)["certificateRefs"].([]any)[0].(map[string]any)["name"], "tenant-tls")
	update := func(key string, items []biz.LoadBalancerListenerInput) {
		t.Helper()
		accepted, err := f.lbs.Update(f.f.ctx, biz.UpdateLoadBalancer{ID: lb.ID, ExpectedVersion: lb.Version, IdempotencyKey: key, LoadBalancerMutableInput: biz.LoadBalancerMutableInput{Listeners: items, UpdateMask: []string{"listeners"}}})
		check(err)
		f.f.drive(t, func() bool {
			lb, err = f.lbs.Get(f.f.ctx, "", lb.ID)
			return err == nil && lb.ConfigurationState == "configured" && lb.AppliedVersion == accepted.LoadBalancer.DesiredVersion
		})
	}
	a.ID = first
	b.ID = lb.Listeners[1].ID
	tcp.ID = lb.Listeners[2].ID
	udp.ID = lb.Listeners[3].ID
	ssl.ID = lb.Listeners[4].ID
	a.Parameters = biz.LoadBalancerListenerParameters{}
	a.Health = biz.LoadBalancerHealthInput{Port: port(0)}
	// Clearing parameters removes their fields and the independently owned CTP.
	tcp.Protocol = "HTTP"
	udp.Protocol = "TCP"
	udp.Health.Port = port(0)
	update("clear-and-switch", []biz.LoadBalancerListenerInput{a, b, tcp, udp, ssl})
	equal(lb.Listeners[0].ID, first)
	equal(rule(first, "route")["sessionPersistence"], nil)
	equal(obj(first, "policy")["compressor"], nil)
	equal(obj(first, "policy")["loadBalancer"].(map[string]any)["type"], "RoundRobin")
	var obsolete int
	check(f.f.owner.QueryRow(f.f.ctx, `SELECT count(*) FROM network_lb_components WHERE tenant_id=$1 AND lb_id=$2 AND deleted_at IS NULL AND ((listener_id=$3 AND kind='client_policy') OR (listener_id=$4 AND kind='tcp_route') OR (listener_id=$5 AND kind IN ('udp_route','patch_policy')))`, f.f.tenant, lb.ID, a.ID, tcp.ID, udp.ID).Scan(&obsolete))
	equal(obsolete, 0)
	a.Parameters.HTTPSRedirectStatusCode = 301
	if _, err := f.lbs.Update(f.f.ctx, biz.UpdateLoadBalancer{ID: lb.ID, ExpectedVersion: lb.Version, IdempotencyKey: "redirect-backend-conflict", LoadBalancerMutableInput: biz.LoadBalancerMutableInput{Listeners: []biz.LoadBalancerListenerInput{a, b}, UpdateMask: []string{"listeners"}}}); err == nil {
		t.Fatal("redirect with backend accepted")
	}
	a.Backends = nil
	update("redirect", []biz.LoadBalancerListenerInput{a, b})
	equal(rule(first, "route")["backendRefs"], nil)
	equal(rule(first, "route")["filters"].([]any)[0].(map[string]any)["requestRedirect"].(map[string]any)["statusCode"], float64(301))
	a.Parameters.HTTPSRedirectStatusCode = 302
	update("redirect-302", []biz.LoadBalancerListenerInput{a, b})
	equal(rule(first, "route")["filters"].([]any)[0].(map[string]any)["requestRedirect"].(map[string]any)["statusCode"], float64(302))
	equal(rule(first, "route")["filters"].([]any)[0].(map[string]any)["requestRedirect"].(map[string]any)["port"], float64(443))
	_, err = f.lbs.Delete(f.f.ctx, "", lb.ID)
	check(err)
	lbState(t, f, lb.ID, biz.Deleted)
	check(f.f.owner.QueryRow(f.f.ctx, `SELECT count(*) FROM network_lb_components WHERE tenant_id=$1 AND lb_id=$2 AND deleted_at IS NULL`, f.f.tenant, lb.ID).Scan(&obsolete))
	equal(obsolete, 0)
}
