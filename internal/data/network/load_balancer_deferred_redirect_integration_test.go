package data_test

import (
	"github.com/google/uuid"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"reflect"
	"testing"
)

// Real admission, PG, Worker and KC adapter must resolve redirects from the
// complete saved configuration. TLS Secret/controller status are external
// protocol facts; zero backend weight still represents runtime configuration.
func TestLBDeferredRedirectWaitsForPreferred443AndWithdrawsWithTarget(t *testing.T) {
	controller := &lbControllerFixture{}
	f := newLBAdmissionFixture(t, controller.http)
	var namespace string
	if err := f.f.owner.QueryRow(f.f.ctx, `SELECT namespace FROM network_provider_bindings WHERE tenant_id=$1 AND subnet_id=$2 AND resource_kind='subnet'`, f.f.tenant, f.subnet.ID).Scan(&namespace); err != nil {
		t.Fatal(err)
	}
	secret := func(name string) {
		t.Helper()
		f.api.Change("secrets", map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": name, "namespace": namespace, "uid": uuid.NewString(), "labels": map[string]any{"network.ani.io/tenant-id": f.f.tenant}}, "type": "kubernetes.io/tls", "data": map[string]any{"tls.crt": "external-cert", "tls.key": "external-key"}}, false)
	}
	secret("alternate-tls")
	port := func(value uint32) *uint32 { return &value }
	zero := uint32(0)
	backend := f.request.Backends[0]
	backend.Weight = &zero
	redirect := biz.LoadBalancerListenerInput{Name: "a-redirect", Protocol: "HTTP", Port: port(80), Parameters: biz.LoadBalancerListenerParameters{HTTPSRedirectStatusCode: 301}}
	standard := biz.LoadBalancerListenerInput{Name: "b-standard", Protocol: "HTTPS", Port: port(443), Parameters: biz.LoadBalancerListenerParameters{Certificate: &biz.LoadBalancerCertificateRef{Name: "waiting-standard-tls"}}}
	alternate := biz.LoadBalancerListenerInput{Name: "c-alternate", Protocol: "HTTPS", Port: port(8443), Backends: []biz.LoadBalancerBackendInput{backend}, Parameters: biz.LoadBalancerListenerParameters{Certificate: &biz.LoadBalancerCertificateRef{Name: "alternate-tls"}}}
	request := f.request
	request.LoadBalancerMutableInput = biz.LoadBalancerMutableInput{Name: "redirect-deferred", Listeners: []biz.LoadBalancerListenerInput{redirect, standard, alternate}}
	bad := request
	bad.IdempotencyKey = "redirect-cannot-have-backends"
	bad.Listeners = append([]biz.LoadBalancerListenerInput(nil), request.Listeners...)
	bad.Listeners[0].Backends = f.request.Backends
	if _, err := f.lbs.Create(f.f.ctx, bad); biz.ReasonOf(err) != biz.InvalidArgument {
		t.Fatal("deferred redirect bypassed incompatible backend validation", err)
	}
	created, err := f.lbs.Create(f.f.ctx, request)
	if err != nil {
		t.Fatal("waiting HTTPS listener required its unreadable TLS Secret", err)
	}
	lb := deferredConfigured(t, f, created.LoadBalancer.ID, 1)
	deferredVersions(t, lb, 1, 1, 1)
	listener := func(name string) biz.LoadBalancerListener {
		t.Helper()
		for _, value := range lb.Listeners {
			if value.Name == name {
				return value
			}
		}
		t.Fatalf("missing saved listener %s", name)
		return biz.LoadBalancerListener{}
	}
	r := listener("a-redirect")
	s := listener("b-standard")
	a := listener("c-alternate")
	if r.DeploymentState != "waiting_for_backends" || r.DeploymentReason != biz.WaitingForRedirectTarget || s.DeploymentState != "waiting_for_backends" || a.DeploymentState != "configured" {
		t.Fatal("443 waiting configuration redirected to active 8443 or degraded it", lb.Listeners)
	}
	controller.mu.Lock()
	readWaitingSecret := false
	for _, request := range controller.requests {
		readWaitingSecret = readWaitingSecret || request == "GET/secrets/waiting-standard-tls"
	}
	controller.mu.Unlock()
	if readWaitingSecret {
		t.Fatal("empty HTTPS:443 listener triggered runtime certificate reads")
	}
	binding := deferredBinding(t, f, lb.ID)
	checkOnlyAlternatePort := func() {
		t.Helper()
		gateway := f.api.Object("gateways", binding.namespace, binding.name)
		service := f.api.Object("services", binding.namespace, binding.name)
		slice := f.api.Object("endpointslices", binding.namespace, binding.name+"-slice")
		if gateway == nil || service == nil || slice == nil {
			t.Fatal("missing generated runtime for active HTTPS:8443")
		}
		ports := gateway["spec"].(map[string]any)["listeners"].([]any)
		servicePorts := service["spec"].(map[string]any)["ports"].([]any)
		slicePorts := slice["ports"].([]any)
		if len(ports) != 1 || ports[0].(map[string]any)["port"] != float64(8443) || len(servicePorts) != 1 || servicePorts[0].(map[string]any)["port"] != float64(8443) || len(slicePorts) != 1 || slicePorts[0].(map[string]any)["port"] != float64(8443) {
			t.Fatal("waiting listeners appeared in Gateway/Service/EndpointSlice port expectations", ports, servicePorts, slicePorts)
		}
	}
	checkOnlyAlternatePort()
	var activeWaitingComponents int
	if err = f.f.owner.QueryRow(f.f.ctx, `SELECT count(*) FROM network_lb_components WHERE tenant_id=$1 AND lb_id=$2 AND listener_id IN ($3,$4) AND deleted_at IS NULL`, f.f.tenant, lb.ID, r.ID, s.ID).Scan(&activeWaitingComponents); err != nil || activeWaitingComponents != 0 {
		t.Fatal("waiting redirect/443 listener owns runtime components", activeWaitingComponents, err)
	}
	componentName := func(id, kind string) string {
		t.Helper()
		var name string
		if err := f.f.owner.QueryRow(f.f.ctx, `SELECT provider_name FROM network_lb_components WHERE tenant_id=$1 AND lb_id=$2 AND listener_id=$3 AND kind=$4 AND deleted_at IS NULL`, f.f.tenant, lb.ID, id, kind).Scan(&name); err != nil {
			t.Fatal(err)
		}
		return name
	}
	alternateRoute := componentName(a.ID, "route")
	priorAlternate := f.api.Object("httproutes", binding.namespace, alternateRoute)
	refs := priorAlternate["spec"].(map[string]any)["rules"].([]any)[0].(map[string]any)["backendRefs"].([]any)
	if len(refs) != 1 || refs[0].(map[string]any)["weight"] != float64(0) || len(a.Backends) != 1 || a.Backends[0].Weight != 0 {
		t.Fatal("zero weight was mistaken for an unconfigured backend", priorAlternate, a)
	}
	secret("waiting-standard-tls")
	redirect = deferredListenerInput(r, nil)
	standardMembers := deferredRetainMembers(a)
	one := uint32(1)
	standardMembers[0].Weight = &one
	standard = deferredListenerInput(s, standardMembers)
	alternate = deferredListenerInput(a, deferredRetainMembers(a))
	activated, err := f.lbs.Update(f.f.ctx, biz.UpdateLoadBalancer{ID: lb.ID, ExpectedVersion: lb.Version, IdempotencyKey: "activate-preferred-443", LoadBalancerMutableInput: biz.LoadBalancerMutableInput{Listeners: []biz.LoadBalancerListenerInput{redirect, standard, alternate}, UpdateMask: []string{"listeners"}}})
	if err != nil {
		t.Fatal(err)
	}
	lb = deferredConfigured(t, f, lb.ID, 2)
	deferredVersions(t, lb, 2, 2, 2)
	operation, err := f.lbs.GetOperation(f.f.ctx, "", activated.Operation.ID)
	if err != nil || operation.State != biz.Succeeded {
		t.Fatal("preferred HTTPS target did not activate", operation, err)
	}
	r = listener("a-redirect")
	s = listener("b-standard")
	a = listener("c-alternate")
	redirectRoute := componentName(r.ID, "route")
	standardRoute := componentName(s.ID, "route")
	redirectObject := f.api.Object("httproutes", binding.namespace, redirectRoute)
	if redirectObject == nil {
		t.Fatal("443 activation did not create saved redirect")
	}
	rule := redirectObject["spec"].(map[string]any)["rules"].([]any)[0].(map[string]any)
	filter := rule["filters"].([]any)[0].(map[string]any)["requestRedirect"].(map[string]any)
	if rule["backendRefs"] != nil || filter["port"] != float64(443) || filter["statusCode"] != float64(301) || r.DeploymentState != "configured" {
		t.Fatal("redirect activation silently selected a different HTTPS target", rule, r)
	}
	if current := deferredBinding(t, f, lb.ID); current.uid != binding.uid || current.id != binding.id {
		t.Fatal("activating preferred target replaced the shared Gateway", current, binding)
	}
	redirect = deferredListenerInput(r, nil)
	standard = deferredListenerInput(s, nil)
	alternate = deferredListenerInput(a, deferredRetainMembers(a))
	cleared, err := f.lbs.Update(f.f.ctx, biz.UpdateLoadBalancer{ID: lb.ID, ExpectedVersion: lb.Version, IdempotencyKey: "clear-preferred-443", LoadBalancerMutableInput: biz.LoadBalancerMutableInput{Listeners: []biz.LoadBalancerListenerInput{redirect, standard, alternate}, UpdateMask: []string{"listeners"}}})
	if err != nil {
		t.Fatal(err)
	}
	lb = deferredConfigured(t, f, lb.ID, 3)
	deferredVersions(t, lb, 3, 3, 3)
	operation, err = f.lbs.GetOperation(f.f.ctx, "", cleared.Operation.ID)
	if err != nil || operation.State != biz.Succeeded {
		t.Fatal("clearing preferred HTTPS target did not complete", operation, err)
	}
	r = listener("a-redirect")
	s = listener("b-standard")
	a = listener("c-alternate")
	checkOnlyAlternatePort()
	if current := deferredBinding(t, f, lb.ID); current.uid != binding.uid || current.id != binding.id {
		t.Fatal("clearing 443 replaced running 8443 Gateway", current, binding)
	}
	currentAlternate := f.api.Object("httproutes", binding.namespace, alternateRoute)
	if f.api.Object("httproutes", binding.namespace, redirectRoute) != nil || f.api.Object("httproutes", binding.namespace, standardRoute) != nil || currentAlternate == nil || currentAlternate["metadata"].(map[string]any)["uid"] != priorAlternate["metadata"].(map[string]any)["uid"] || !reflect.DeepEqual(currentAlternate["spec"], priorAlternate["spec"]) {
		t.Fatal("clearing preferred target did not withdraw redirect while preserving alternate forwarding")
	}
	if r.DeploymentState != "waiting_for_backends" || r.DeploymentReason != biz.WaitingForRedirectTarget || s.DeploymentState != "waiting_for_backends" || a.DeploymentState != "configured" {
		t.Fatal("redirect failed to return to waiting for saved 443 target", lb.Listeners)
	}
	if err = f.f.owner.QueryRow(f.f.ctx, `SELECT count(*) FROM network_lb_components WHERE tenant_id=$1 AND lb_id=$2 AND listener_id IN ($3,$4) AND deleted_at IS NULL`, f.f.tenant, lb.ID, r.ID, s.ID).Scan(&activeWaitingComponents); err != nil || activeWaitingComponents != 0 {
		t.Fatal("redirect/443 cleanup retained active components", activeWaitingComponents, err)
	}
	deleted, err := f.lbs.Delete(f.f.ctx, "", lb.ID)
	if err != nil {
		t.Fatal(err)
	}
	lbState(t, f, lb.ID, biz.Deleted)
	operation, err = f.lbs.GetOperation(f.f.ctx, "", deleted.Operation.ID)
	if err != nil || operation.State != biz.Succeeded {
		t.Fatal("redirect fixture product cleanup failed", operation, err)
	}
}
