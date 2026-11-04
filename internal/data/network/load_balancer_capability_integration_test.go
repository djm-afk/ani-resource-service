package data_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/google/uuid"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network"
	controlled "github.com/zhangzhe-ctrl/ani-resource-service/tests/net05a/provider"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
	"net/http"
	"strings"
	"testing"
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
func TestLBCapabilityIsAnExplicitQueryAndDoesNotGateAdmission(t *testing.T) {
	controller := &lbControllerFixture{}
	f := newLBAdmissionFixture(t, controller.http)
	seedLBInstallation(f.api)
	config, err := clientcmd.BuildConfigFromFlags("", f.kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := data.NewKCProvider(f.f.p, config)
	if err != nil {
		t.Fatal(err)
	}
	f.f.p.UseLoadBalancerCapabilityReader(provider)
	capability, err := f.f.e.GetPlatformCapabilities(f.f.ctx)
	if err != nil || !capability.LoadBalancer.Ready || capability.LoadBalancer.ObservationStale {
		t.Fatal(capability, err)
	}
	class := f.api.Object("gatewayclasses", "", "lb-common")
	class["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Accepted", "status": "False", "reason": "NotAccepted"}}}
	f.api.Change("gatewayclasses", class, false)
	capability, err = f.f.e.GetPlatformCapabilities(f.f.ctx)
	if err != nil || capability.LoadBalancer.Ready || capability.LoadBalancer.Reason != "NotAccepted" {
		t.Fatal(capability, err)
	}
	if _, err = f.lbs.Create(f.f.ctx, f.request); err != nil {
		t.Fatal("installation diagnostic gated a product declaration", err)
	}
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
			f.f.p.UseLoadBalancerCapabilityReader(provider)
			capability, err := f.f.e.GetPlatformCapabilities(f.f.ctx)
			if err != nil || !capability.LoadBalancer.Ready {
				t.Fatal(capability, err)
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
