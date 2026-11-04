package data

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/cache"
	"regexp"
	"sigs.k8s.io/yaml"
	"sort"
	"strings"
)

// Optional input for the explicit installation diagnostic only. Runtime
// resource reconciliation does not consume installation proofs.
type LoadBalancerInstallation struct {
	Fingerprint, ControllerImageID, EnvoyImageID, ShutdownImageID, KCImageID string
}

var lbDeployments = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
var lbReplicaSets = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "replicasets"}
var lbEndpointSlices = schema.GroupVersionResource{Group: "discovery.k8s.io", Version: "v1", Resource: "endpointslices"}
var lbCRDs = schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}
var lbReviews = schema.GroupVersionResource{Group: "authorization.k8s.io", Version: "v1", Resource: "subjectaccessreviews"}
var lbInstallationResources = []schema.GroupVersionResource{lbGateways, lbRoutes, lbBackends, lbPolicies, lbDeployments, lbReplicaSets, lbEndpointSlices, lbGatewayClasses, lbEnvoyProxies}

func validateLoadBalancerInstallation(expected LoadBalancerInstallation) error {
	if !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(expected.Fingerprint) {
		return fmt.Errorf("LB installation fingerprint must be sha256")
	}
	for _, id := range []string{expected.ControllerImageID, expected.EnvoyImageID, expected.ShutdownImageID, expected.KCImageID} {
		if !regexp.MustCompile(`^(?:[^\s@]+@)?sha256:[a-f0-9]{64}$`).MatchString(id) {
			return fmt.Errorf("LB expected image IDs must have fixed sha256 digests")
		}
	}
	return nil
}
func lbViewObject(v *lbInstallationView, gvr schema.GroupVersionResource, ns, name string) *unstructured.Unstructured {
	index := v.indices[gvr]
	if index == nil {
		return nil
	}
	key := name
	if ns != "" {
		key = ns + "/" + name
	}
	obj, exists, err := index.GetByKey(key)
	if err != nil || !exists {
		return nil
	}
	return obj.(*unstructured.Unstructured)
}
func lbInstallationBundle(v *lbInstallationView) map[string]any {
	bundle := map[string]any{}
	add := func(gvr schema.GroupVersionResource, ns, name string) {
		obj := lbViewObject(v, gvr, ns, name)
		if obj == nil {
			bundle[gvr.Resource+"/"+ns+"/"+name] = nil
			return
		}
		content := obj.Object["spec"]
		if gvr == kcConfigMaps {
			content = obj.Object["data"]
		}
		bundle[gvr.Resource+"/"+ns+"/"+name] = map[string]any{"uid": string(obj.GetUID()), "content": content}
	}
	add(lbGatewayClasses, "", biz.LoadBalancerGatewayClass)
	add(lbEnvoyProxies, "envoy-gateway-system", biz.LoadBalancerGatewayClass)
	add(kcConfigMaps, "envoy-gateway-system", "envoy-gateway-config")
	for _, name := range []string{"gateways.gateway.networking.k8s.io", "httproutes.gateway.networking.k8s.io", "backends.gateway.envoyproxy.io", "backendtrafficpolicies.gateway.envoyproxy.io", "envoyproxies.gateway.envoyproxy.io"} {
		add(lbCRDs, "", name)
	}
	return bundle
}
func (p *KCProvider) inspectLBCapability(ctx context.Context, v *lbInstallationView, expected *LoadBalancerInstallation) (bool, string, []string) {
	fingerprint := contentHash(lbInstallationBundle(v))
	if expected == nil || fingerprint != expected.Fingerprint {
		return false, fingerprint, nil
	}
	config := lbViewObject(v, kcConfigMaps, "envoy-gateway-system", "envoy-gateway-config")
	if config == nil {
		return false, fingerprint, nil
	}
	raw, _, _ := unstructured.NestedString(config.Object, "data", "envoy-gateway.yaml")
	body, err := yaml.YAMLToJSONStrict([]byte(raw))
	if err != nil {
		return false, fingerprint, nil
	}
	var parsed map[string]any
	if json.Unmarshal(body, &parsed) != nil {
		return false, fingerprint, nil
	}
	s := &unstructured.Unstructured{Object: parsed}
	if !crBool(s, "extensionApis", "enableBackend") || !crBool(s, "extensionApis", "enableEnvoyPatchPolicy") || crString(s, "provider", "type") != "Kubernetes" || crString(s, "provider", "kubernetes", "deploy", "type") != "GatewayNamespace" || crString(s, "gateway", "controllerName") != lbController {
		return false, fingerprint, nil
	}
	class := lbViewObject(v, lbGatewayClasses, "", biz.LoadBalancerGatewayClass)
	proxy := lbViewObject(v, lbEnvoyProxies, "envoy-gateway-system", biz.LoadBalancerGatewayClass)
	if class == nil || proxy == nil || class.GetUID() == "" || proxy.GetUID() == "" || class.GetDeletionTimestamp() != nil || proxy.GetDeletionTimestamp() != nil {
		return false, fingerprint, nil
	}
	conditions, _, _ := unstructured.NestedSlice(class.Object, "status", "conditions")
	if !lbInstallationAccepted(conditions, class.GetGeneration()) || crString(class, "spec", "controllerName") != lbController || crString(class, "spec", "parametersRef", "group") != lbEnvoyProxies.Group || crString(class, "spec", "parametersRef", "kind") != "EnvoyProxy" || crString(class, "spec", "parametersRef", "namespace") != "envoy-gateway-system" || crString(class, "spec", "parametersRef", "name") != proxy.GetName() {
		return false, fingerprint, nil
	}
	// Capacity and Service type belong to a dedicated tenant Proxy; the common
	// object supplies only platform defaults that are inherited by every LB.
	if !crBool(proxy, "spec", "preserveRouteOrder") || crBool(proxy, "spec", "mergeGateways") || crString(proxy, "spec", "provider", "type") != "Kubernetes" || hasField(proxy, "spec", "provider", "kubernetes", "envoyService", "type") || hasField(proxy, "spec", "provider", "kubernetes", "envoyDeployment", "replicas") || hasField(proxy, "spec", "provider", "kubernetes", "envoyDeployment", "container", "resources") {
		return false, fingerprint, nil
	}
	for _, name := range []string{"gateways.gateway.networking.k8s.io", "httproutes.gateway.networking.k8s.io", "backends.gateway.envoyproxy.io", "backendtrafficpolicies.gateway.envoyproxy.io", "envoyproxies.gateway.envoyproxy.io"} {
		crd := lbViewObject(v, lbCRDs, "", name)
		if crd == nil || crd.GetDeletionTimestamp() != nil {
			return false, fingerprint, nil
		}
		established := false
		conditions, _, _ := unstructured.NestedSlice(crd.Object, "status", "conditions")
		for _, raw := range conditions {
			c, _ := raw.(map[string]any)
			if c["type"] == "Established" && c["status"] == "True" {
				established = true
			}
		}
		served := false
		version := "v1"
		if strings.Contains(name, "envoyproxy") {
			version = "v1alpha1"
		}
		versions, _, _ := unstructured.NestedSlice(crd.Object, "spec", "versions")
		for _, raw := range versions {
			v, _ := raw.(map[string]any)
			served = served || v["name"] == version && v["served"] == true
		}
		if !established || !served {
			return false, fingerprint, nil
		}
	}
	seen := map[string]bool{}
	// A digest-pinned installation can bootstrap its first Envoy instance.
	// Mutable image references require an existing running owner chain below.
	image := crString(proxy, "spec", "provider", "kubernetes", "envoyDeployment", "container", "image")
	containers, _, _ := unstructured.NestedSlice(proxy.Object, "spec", "provider", "kubernetes", "envoyDeployment", "patch", "value", "spec", "template", "spec", "containers")
	shutdown := ""
	for _, raw := range containers {
		c, _ := raw.(map[string]any)
		if c["name"] == "shutdown-manager" {
			shutdown, _ = c["image"].(string)
		}
	}
	// A bare runtime config digest is not proof of a pullable manifest. Tags
	// also require actual running images through the verified Gateway chain.
	envoyDigest, shutdownDigest := lbImageDigest(expected.EnvoyImageID), lbImageDigest(expected.ShutdownImageID)
	pinnedProxyImages := envoyDigest != "" && shutdownDigest != "" && strings.HasSuffix(image, "@sha256:"+envoyDigest) && strings.HasSuffix(shutdown, "@sha256:"+shutdownDigest)
	if pinnedProxyImages {
		seen[expected.EnvoyImageID] = true
		seen[expected.ShutdownImageID] = true
	}
	validImages := true
	index := v.indices[pods]
	if index == nil {
		return false, fingerprint, nil
	}
	for _, raw := range index.List() {
		pod := raw.(*unstructured.Unstructured)
		if pod.GetDeletionTimestamp() != nil || crString(pod, "status", "phase") != "Running" {
			continue
		}
		statuses, _, _ := unstructured.NestedSlice(pod.Object, "status", "containerStatuses")
		for _, raw := range statuses {
			c, _ := raw.(map[string]any)
			name, _ := c["name"].(string)
			id, _ := c["imageID"].(string)
			want := ""
			if pod.GetNamespace() == "envoy-gateway-system" && name == "envoy-gateway" {
				want = expected.ControllerImageID
			}
			if pod.GetNamespace() == kcSystemNamespace && (name == "kcn-controller" || name == "kcn-cni-server") {
				want = expected.KCImageID
			}
			if lbInstalledProxyPod(v, pod) {
				if name == "envoy" {
					want = expected.EnvoyImageID
				}
				if name == "shutdown-manager" {
					want = expected.ShutdownImageID
				}
			}
			if want != "" {
				validImages = validImages && id == want && c["ready"] == true
				seen[id] = true
			}
		}
	}
	images := []string{}
	for id := range seen {
		images = append(images, id)
	}
	sort.Strings(images)
	for _, id := range []string{expected.ControllerImageID, expected.EnvoyImageID, expected.ShutdownImageID, expected.KCImageID} {
		if !seen[id] {
			validImages = false
		}
	}
	if !validImages {
		return false, fingerprint, images
	}
	// The installed controller's permissions are checked through Kubernetes'
	// authorization API; no owner-supplied flag substitutes for that result.
	for _, permission := range []struct{ group, resource, namespace, verb string }{{"authentication.k8s.io", "tokenreviews", "", "create"}, {"apps", "deployments", p.repository.placement.NamespacePrefix + "capability", "create"}, {"", "services", p.repository.placement.NamespacePrefix + "capability", "create"}, {lbBackends.Group, "backends", p.repository.placement.NamespacePrefix + "capability", "get"}} {
		request := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "authorization.k8s.io/v1", "kind": "SubjectAccessReview", "spec": map[string]any{"user": "system:serviceaccount:envoy-gateway-system:envoy-gateway", "groups": []any{"system:serviceaccounts", "system:serviceaccounts:envoy-gateway-system", "system:authenticated"}, "resourceAttributes": map[string]any{"group": permission.group, "resource": permission.resource, "namespace": permission.namespace, "verb": permission.verb}}}}
		response, err := p.client.Resource(lbReviews).Create(ctx, request, metav1.CreateOptions{})
		if err != nil || !crBool(response, "status", "allowed") || crBool(response, "status", "denied") {
			return false, fingerprint, images
		}
	}
	return true, fingerprint, images
}
func lbInstalledProxyPod(v *lbInstallationView, pod *unstructured.Unstructured) bool {
	ns := pod.GetNamespace()
	gwName := pod.GetLabels()["gateway.envoyproxy.io/owning-gateway-name"]
	if gwName == "" || pod.GetLabels()["gateway.envoyproxy.io/owning-gateway-namespace"] != ns {
		return false
	}
	gateway := lbViewObject(v, lbGateways, ns, gwName)
	if gateway == nil || crString(gateway, "spec", "gatewayClassName") != biz.LoadBalancerGatewayClass {
		return false
	}
	deployment := lbViewObject(v, lbDeployments, ns, gwName)
	if !lbOwnedBy(deployment, "Gateway", gwName, string(gateway.GetUID())) {
		return false
	}
	for _, ref := range pod.GetOwnerReferences() {
		if ref.Kind != "ReplicaSet" {
			continue
		}
		rs := lbViewObject(v, lbReplicaSets, ns, ref.Name)
		if rs != nil && rs.GetUID() == ref.UID && lbOwnedBy(rs, "Deployment", deployment.GetName(), string(deployment.GetUID())) {
			return true
		}
	}
	return false
}

func lbImageDigest(id string) string { _, digest, _ := strings.Cut(id, "@sha256:"); return digest }

// VerifyLoadBalancerInstallation is invoked explicitly by the deployment driver;
// it is never called by resource reconciliation or startup task recovery.
func VerifyLoadBalancerInstallation(ctx context.Context, client dynamic.Interface, expected LoadBalancerInstallation) (bool, error) {
	p := &KCProvider{client: client, repository: &Postgres{placement: Placement{NamespacePrefix: "tenant-"}}}
	if err := validateLoadBalancerInstallation(expected); err != nil {
		return false, err
	}
	v := &lbInstallationView{indices: map[schema.GroupVersionResource]cache.Indexer{}}
	for _, gvr := range append(append([]schema.GroupVersionResource{}, lbInstallationResources...), lbCRDs, kcConfigMaps, pods) {
		objects, err := p.listAll(ctx, gvr)
		if err != nil {
			return false, err
		}
		index := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
		for _, obj := range objects {
			if err := index.Add(obj.DeepCopy()); err != nil {
				return false, err
			}
		}
		v.indices[gvr] = index
	}
	ready, _, _ := p.inspectLBCapability(ctx, v, &expected)
	return ready, nil
}

// The explicit diagnostic owns this temporary collection. It is never stored
// in the runtime observer or used as a resource admission requirement.
type lbInstallationView struct {
	indices map[schema.GroupVersionResource]cache.Indexer
}

func lbInstallationAccepted(conditions []any, generation int64) bool {
	ready, _ := lbConditions(conditions, generation, "Accepted")
	return ready != nil && *ready
}

func (p *KCProvider) ReadLoadBalancerCapability(ctx context.Context) (*bool, biz.Reason, error) {
	class, err := p.client.Resource(lbGatewayClasses).Get(ctx, biz.LoadBalancerGatewayClass, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, biz.ProviderMissing, nil
	}
	if err != nil {
		return nil, "", readFailure(err)
	}
	ready, reason := primaryCondition(class, "Accepted")
	return ready, reason, nil
}

func lbOwnedBy(o *unstructured.Unstructured, kind, name, uid string) bool {
	if o == nil || uid == "" {
		return false
	}
	for _, ref := range o.GetOwnerReferences() {
		if ref.Kind == kind && ref.Name == name && string(ref.UID) == uid && ref.APIVersion == lbOwnerAPIVersion(kind) {
			return true
		}
	}
	return false
}

func lbOwnerAPIVersion(kind string) string {
	switch kind {
	case "Gateway":
		return "gateway.networking.k8s.io/v1"
	case "Pod", "Service":
		return "v1"
	case "Deployment", "ReplicaSet":
		return "apps/v1"
	case "VNic":
		return "networking.kubercloud.com/v1"
	}
	return ""
}
