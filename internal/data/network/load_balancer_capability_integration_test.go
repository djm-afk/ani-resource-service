package data_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network"
	controlled "github.com/zhangzhe-ctrl/ani-resource-service/tests/net05a/provider"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
)

func seedLBInstallation(api *controlled.Server) data.LoadBalancerInstallation {
	expected := data.LoadBalancerInstallation{ControllerImageID: "fixture@sha256:" + strings.Repeat("b", 64), EnvoyImageID: "fixture@sha256:" + strings.Repeat("c", 64), ShutdownImageID: "fixture@sha256:" + strings.Repeat("d", 64), KCImageID: "fixture@sha256:" + strings.Repeat("a", 64)}
	bundle := map[string]any{}
	put := func(resource, version, kind, ns, name string, spec, status map[string]any) {
		obj := map[string]any{"apiVersion": version, "kind": kind, "metadata": map[string]any{"name": name, "namespace": ns, "uid": uuid.NewString(), "generation": float64(1)}, "spec": spec, "status": status}
		if kind == "ConfigMap" {
			delete(obj, "spec")
			obj["data"] = spec
		}
		api.Change(resource, obj, false)
		bundle[resource+"/"+ns+"/"+name] = map[string]any{"uid": obj["metadata"].(map[string]any)["uid"], "content": spec}
	}
	put("gatewayclasses", "gateway.networking.k8s.io/v1", "GatewayClass", "", "lb-common", map[string]any{"controllerName": "gateway.envoyproxy.io/gatewayclass-controller", "parametersRef": map[string]any{"group": "gateway.envoyproxy.io", "kind": "EnvoyProxy", "name": "lb-common", "namespace": "envoy-gateway-system"}}, map[string]any{"conditions": lbTestConditions(float64(1), "Accepted")})
	put("envoyproxies", "gateway.envoyproxy.io/v1alpha1", "EnvoyProxy", "envoy-gateway-system", "lb-common", map[string]any{"preserveRouteOrder": true, "mergeGateways": false, "provider": map[string]any{"type": "Kubernetes", "kubernetes": map[string]any{"envoyDeployment": map[string]any{"container": map[string]any{"image": expected.EnvoyImageID}, "patch": map[string]any{"type": "StrategicMerge", "value": map[string]any{"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"name": "shutdown-manager", "image": expected.ShutdownImageID}}}}}}}}}}}, map[string]any{})
	put("configmaps", "v1", "ConfigMap", "envoy-gateway-system", "envoy-gateway-config", map[string]any{"envoy-gateway.yaml": "apiVersion: gateway.envoyproxy.io/v1alpha1\nkind: EnvoyGateway\nextensionApis:\n  enableBackend: true\n  enableEnvoyPatchPolicy: true\ngateway:\n  controllerName: gateway.envoyproxy.io/gatewayclass-controller\nprovider:\n  type: Kubernetes\n  kubernetes:\n    deploy:\n      type: GatewayNamespace\n"}, map[string]any{})
	for _, name := range []string{"gateways.gateway.networking.k8s.io", "httproutes.gateway.networking.k8s.io", "backends.gateway.envoyproxy.io", "backendtrafficpolicies.gateway.envoyproxy.io", "envoyproxies.gateway.envoyproxy.io"} {
		version := "v1"
		if strings.Contains(name, "envoyproxy") {
			version = "v1alpha1"
		}
		put("customresourcedefinitions", "apiextensions.k8s.io/v1", "CustomResourceDefinition", "", name, map[string]any{"versions": []any{map[string]any{"name": version, "served": true, "storage": true}}}, map[string]any{"conditions": []any{map[string]any{"type": "Established", "status": "True"}}})
	}
	for _, role := range []struct{ namespace, name, image string }{{"envoy-gateway-system", "envoy-gateway", expected.ControllerImageID}, {"kcn-system", "kcn-controller", expected.KCImageID}} {
		api.Change("pods", map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{"namespace": role.namespace, "name": "lb-capability-" + role.name, "uid": uuid.NewString()}, "spec": map[string]any{"containers": []any{map[string]any{"name": role.name}}}, "status": map[string]any{"phase": "Running", "containerStatuses": []any{map[string]any{"name": role.name, "ready": true, "imageID": role.image}}}}, false)
	}
	body, _ := json.Marshal(bundle)
	sum := sha256.Sum256(body)
	expected.Fingerprint = hex.EncodeToString(sum[:])
	return expected
}
func TestLBCapabilityUsesSharedAuditAndKeepsVPCIndependent(t *testing.T) {
	controller := &lbControllerFixture{}
	intercept := func(w http.ResponseWriter, r *http.Request, api *controlled.Server) bool {
		if strings.HasSuffix(r.URL.Path, "/subjectaccessreviews") && r.Method == "POST" {
			var request map[string]any
			if json.NewDecoder(r.Body).Decode(&request) != nil {
				w.WriteHeader(400)
				return true
			}
			request["status"] = map[string]any{"allowed": true}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(request)
			return true
		}
		return controller.http(w, r, api)
	}
	f := newLBAdmissionFixture(t, intercept)
	expected := seedLBInstallation(f.api)
	config, err := clientcmd.BuildConfigFromFlags("", f.kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	config.QPS, config.Burst = 1000, 1000
	provider, err := data.NewKCProvider(f.f.p, config)
	if err != nil {
		t.Fatal(err)
	}
	if err = provider.ConfigureLoadBalancer(expected); err != nil {
		t.Fatal(err)
	}
	options := data.DefaultObservationOptions()
	options.AuditInterval = 100 * time.Millisecond
	options.AuditJitter = time.Millisecond
	options.FlushInterval = 10 * time.Millisecond
	observer, err := provider.EnableObservation(options)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- observer.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	awaitNET05A(t, 5*time.Second, func() bool {
		var ready bool
		err := f.f.owner.QueryRow(f.f.ctx, `SELECT ready FROM network_lb_capabilities WHERE cluster_id='test-cluster' AND fingerprint=$1`, expected.Fingerprint).Scan(&ready)
		return err == nil && ready
	})
	cap, err := f.f.e.GetPlatformCapabilities(f.f.ctx)
	if err != nil || !cap.LoadBalancer.Ready || cap.LoadBalancer.ObservationStale {
		t.Fatal("capability was not observed", cap, err)
	}
	policy := biz.DefaultWorkerPolicy()
	policy.ObserveEvery, policy.RetryMin, policy.RetryMax = 100*time.Millisecond, 5*time.Millisecond, 20*time.Millisecond
	f.f.w, err = biz.NewWorker(f.f.p, provider, uuid.NewString(), policy)
	if err != nil {
		t.Fatal(err)
	}
	created, err := f.lbs.Create(f.f.ctx, f.request)
	if err != nil {
		t.Fatal(err)
	}
	lb := lbState(t, f, created.LoadBalancer.ID, biz.Available)
	// Even a live Watch/audit cannot renew the resource's unapplied PG fact.
	shortReads, err := biz.NewLoadBalancers(f.f.p, biz.ContextEgressAuthorization{}, []byte(strings.Repeat("c", 32)), 150*time.Millisecond, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	stale, err := shortReads.Get(f.f.ctx, "", lb.ID)
	if err != nil || !stale.ObservationStale || stale.Version != lb.Version || !stale.ObservedAt.Equal(*lb.ObservedAt) {
		t.Fatal("unapplied audit or GET renewed LB freshness", stale, err)
	}
	for _, code := range []int{0, 410} {
		before, _ := f.api.Counts()
		f.api.Disconnect(code)
		awaitNET05A(t, 8*time.Second, func() bool {
			if _, err := f.f.w.Step(f.f.ctx); err != nil {
				t.Fatal(err)
			}
			after, _ := f.api.Counts()
			return after["WATCH/gateways"] > before["WATCH/gateways"]
		})
	}
	lbState(t, f, lb.ID, biz.Available)
	// Platform installation drift is checked explicitly, not projected into
	// each existing LB's runtime health or normal resource admission.
	proxy := f.api.Object("envoyproxies", "envoy-gateway-system", "lb-common")
	proxy["spec"].(map[string]any)["provider"].(map[string]any)["kubernetes"].(map[string]any)["envoyDeployment"].(map[string]any)["replicas"] = float64(1)
	f.api.Change("envoyproxies", proxy, false)
	client, err := dynamic.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	ready, err := data.VerifyLoadBalancerInstallation(f.f.ctx, client, expected)
	if err != nil || ready {
		t.Fatal("explicit installation check missed drift", ready, err)
	}
	lbState(t, f, lb.ID, biz.Available)
	if _, err = f.lbs.Create(f.f.ctx, f.request); err != nil {
		t.Fatal("permanent LB replay rechecked installation", err)
	}
	v, err := f.f.n.CreateVPC(f.f.ctx, biz.CreateVPC{TenantID: f.f.tenant, Name: "lb-independent-vpc", CIDR: "10.61.0.0/16", IdempotencyKey: "lb-independent-vpc"})
	if err != nil {
		t.Fatal("LB capability blocked ordinary VPC", err)
	}
	baseState(t, f.f, v.ID, biz.Available)
	delete(proxy["spec"].(map[string]any)["provider"].(map[string]any)["kubernetes"].(map[string]any)["envoyDeployment"].(map[string]any), "replicas")
	f.api.Change("envoyproxies", proxy, false)
	awaitNET05A(t, 5*time.Second, func() bool {
		cap, err = f.f.e.GetPlatformCapabilities(f.f.ctx)
		return err == nil && cap.LoadBalancer.Ready
	})
	// The restored observation updates the independent pure projection.
	if cap.LoadBalancer.ObservedAt == nil {
		t.Fatal("missing capability observation time")
	}
	lbState(t, f, lb.ID, biz.Available)
	if _, err = f.lbs.Delete(f.f.ctx, "", lb.ID); err != nil {
		t.Fatal(err)
	}
	lbState(t, f, lb.ID, biz.Deleted)
}

// Fresh containerd imports expose config image IDs without a repository prefix.
// Exercise the actual observer against the supplied install's omitted service
// default and numeric resource quantities, with independent negative controls.
func TestLBCapabilityImportedRuntimeImagesAndInstallDefaults(t *testing.T) {
	for _, scenario := range []string{"valid", "wrong-image", "missing-owner-chain", "common-capacity", "common-service-type"} {
		t.Run(scenario, func(t *testing.T) {
			f := newLBAdmissionFixture(t, func(w http.ResponseWriter, r *http.Request, _ *controlled.Server) bool {
				if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/subjectaccessreviews") {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"apiVersion":"authorization.k8s.io/v1","kind":"SubjectAccessReview","status":{"allowed":true}}`))
					return true
				}
				return false
			})
			expected := seedLBInstallation(f.api)
			expected.ControllerImageID = strings.TrimPrefix(expected.ControllerImageID, "fixture@")
			expected.EnvoyImageID = strings.TrimPrefix(expected.EnvoyImageID, "fixture@")
			expected.ShutdownImageID = strings.TrimPrefix(expected.ShutdownImageID, "fixture@")
			expected.KCImageID = strings.TrimPrefix(expected.KCImageID, "fixture@")
			for _, role := range []struct{ ns, name, image string }{{"envoy-gateway-system", "envoy-gateway", expected.ControllerImageID}, {"kcn-system", "kcn-controller", expected.KCImageID}} {
				pod := f.api.Object("pods", role.ns, "lb-capability-"+role.name)
				pod["status"].(map[string]any)["containerStatuses"].([]any)[0].(map[string]any)["imageID"] = role.image
				f.api.Change("pods", pod, false)
			}
			proxy := f.api.Object("envoyproxies", "envoy-gateway-system", "lb-common")
			k := proxy["spec"].(map[string]any)["provider"].(map[string]any)["kubernetes"].(map[string]any)
			if scenario == "common-service-type" {
				k["envoyService"] = map[string]any{"type": "LoadBalancer"}
			}
			dep := k["envoyDeployment"].(map[string]any)
			if scenario == "common-capacity" {
				dep["replicas"] = float64(2)
			}
			// Manifest-looking strings cannot bootstrap a bare config digest;
			// the actual running Gateway owner chain below remains necessary.
			dep["container"].(map[string]any)["image"] = "registry/envoy@" + expected.EnvoyImageID
			dep["patch"].(map[string]any)["value"].(map[string]any)["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["image"] = "registry/shutdown@" + expected.ShutdownImageID
			f.api.Change("envoyproxies", proxy, false)
			ns, gateway, deployment, rs := "installed-lb", uuid.NewString(), uuid.NewString(), uuid.NewString()
			put := func(resource, version, kind, name, uid, parentKind, parentName, parentUID string) map[string]any {
				meta := map[string]any{"namespace": ns, "name": name, "uid": uid}
				if parentKind != "" {
					parentVersion := "apps/v1"
					if parentKind == "Gateway" {
						parentVersion = "gateway.networking.k8s.io/v1"
					}
					meta["ownerReferences"] = []any{map[string]any{"apiVersion": parentVersion, "kind": parentKind, "name": parentName, "uid": parentUID}}
				}
				obj := map[string]any{"apiVersion": version, "kind": kind, "metadata": meta}
				f.api.Change(resource, obj, false)
				return obj
			}
			gw := put("gateways", "gateway.networking.k8s.io/v1", "Gateway", "proxy", gateway, "", "", "")
			gw["spec"] = map[string]any{"gatewayClassName": biz.LoadBalancerGatewayClass}
			f.api.Change("gateways", gw, false)
			put("deployments", "apps/v1", "Deployment", "proxy", deployment, "Gateway", "proxy", gateway)
			put("replicasets", "apps/v1", "ReplicaSet", "proxy-rs", rs, "Deployment", "proxy", deployment)
			pod := put("pods", "v1", "Pod", "proxy-pod", uuid.NewString(), "ReplicaSet", "proxy-rs", rs)
			pod["metadata"].(map[string]any)["labels"] = map[string]any{"gateway.envoyproxy.io/owning-gateway-name": "proxy", "gateway.envoyproxy.io/owning-gateway-namespace": ns}
			liveEnvoy := expected.EnvoyImageID
			if scenario == "wrong-image" {
				liveEnvoy = "sha256:" + strings.Repeat("e", 64)
			}
			pod["status"] = map[string]any{"phase": "Running", "containerStatuses": []any{map[string]any{"name": "envoy", "ready": true, "imageID": liveEnvoy}, map[string]any{"name": "shutdown-manager", "ready": true, "imageID": expected.ShutdownImageID}}}
			if scenario == "missing-owner-chain" {
				delete(pod["metadata"].(map[string]any), "ownerReferences")
			}
			f.api.Change("pods", pod, false)
			bundle := map[string]any{}
			add := func(resource, ns, name string) {
				obj := f.api.Object(resource, ns, name)
				content := obj["spec"]
				if resource == "configmaps" {
					content = obj["data"]
				}
				bundle[resource+"/"+ns+"/"+name] = map[string]any{"uid": obj["metadata"].(map[string]any)["uid"], "content": content}
			}
			add("gatewayclasses", "", "lb-common")
			add("envoyproxies", "envoy-gateway-system", "lb-common")
			add("configmaps", "envoy-gateway-system", "envoy-gateway-config")
			for _, name := range []string{"gateways.gateway.networking.k8s.io", "httproutes.gateway.networking.k8s.io", "backends.gateway.envoyproxy.io", "backendtrafficpolicies.gateway.envoyproxy.io", "envoyproxies.gateway.envoyproxy.io"} {
				add("customresourcedefinitions", "", name)
			}
			body, _ := json.Marshal(bundle)
			sum := sha256.Sum256(body)
			expected.Fingerprint = hex.EncodeToString(sum[:])
			config, err := clientcmd.BuildConfigFromFlags("", f.kubeconfig)
			if err != nil {
				t.Fatal(err)
			}
			config.QPS, config.Burst = 1000, 1000
			provider, err := data.NewKCProvider(f.f.p, config)
			if err != nil {
				t.Fatal(err)
			}
			if err = provider.ConfigureLoadBalancer(expected); err != nil {
				t.Fatal(err)
			}
			options := data.DefaultObservationOptions()
			options.AuditInterval, options.AuditJitter, options.FlushInterval = 100*time.Millisecond, time.Millisecond, 10*time.Millisecond
			observer, err := provider.EnableObservation(options)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- observer.Start(ctx) }()
			defer func() {
				cancel()
				if err := <-done; err != nil {
					t.Error(err)
				}
			}()
			var ready bool
			awaitNET05A(t, 5*time.Second, func() bool {
				return f.f.owner.QueryRow(f.f.ctx, `SELECT ready FROM network_lb_capabilities WHERE cluster_id='test-cluster' AND fingerprint=$1`, expected.Fingerprint).Scan(&ready) == nil
			})
			if !ready {
				t.Fatalf("installation diagnostics leaked into runtime capability: %s", scenario)
			}
			client, err := dynamic.NewForConfig(config)
			if err != nil {
				t.Fatal(err)
			}
			accepted, err := data.VerifyLoadBalancerInstallation(f.f.ctx, client, expected)
			if err != nil || accepted != (scenario == "valid") {
				t.Fatalf("installation acceptance=%v for %s: %v", accepted, scenario, err)
			}
		})
	}
}
