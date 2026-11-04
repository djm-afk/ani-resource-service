package data

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"strconv"
)

var lbGateways = schema.GroupVersionResource{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "gateways"}
var lbRoutes = schema.GroupVersionResource{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "httproutes"}
var lbBackends = schema.GroupVersionResource{Group: "gateway.envoyproxy.io", Version: "v1alpha1", Resource: "backends"}
var lbTCPRoutes = schema.GroupVersionResource{Group: lbRoutes.Group, Version: "v1alpha2", Resource: "tcproutes"}
var lbUDPRoutes = schema.GroupVersionResource{Group: lbRoutes.Group, Version: "v1alpha2", Resource: "udproutes"}
var lbClientPolicies = schema.GroupVersionResource{Group: lbBackends.Group, Version: "v1alpha1", Resource: "clienttrafficpolicies"}
var lbPatchPolicies = schema.GroupVersionResource{Group: lbBackends.Group, Version: "v1alpha1", Resource: "envoypatchpolicies"}
var lbPolicies = schema.GroupVersionResource{Group: "gateway.envoyproxy.io", Version: "v1alpha1", Resource: "backendtrafficpolicies"}
var lbGatewayClasses = schema.GroupVersionResource{Group: lbGateways.Group, Version: "v1", Resource: "gatewayclasses"}
var lbEnvoyProxies = schema.GroupVersionResource{Group: lbBackends.Group, Version: "v1alpha1", Resource: "envoyproxies"}

const lbVersionAnnotation = "network.ani.io/configuration-version"
const lbController = "gateway.envoyproxy.io/gatewayclass-controller"

func lbGVR(kind string) schema.GroupVersionResource {
	switch kind {
	case "envoy_proxy":
		return lbEnvoyProxies
	case "gateway":
		return lbGateways
	case "route":
		return lbRoutes
	case "tcp_route":
		return lbTCPRoutes
	case "udp_route":
		return lbUDPRoutes
	case "client_policy":
		return lbClientPolicies
	case "patch_policy":
		return lbPatchPolicies
	case "policy":
		return lbPolicies
	case "backend":
		return lbBackends
	}
	return schema.GroupVersionResource{}
}
func lbKind(kind string) string {
	switch kind {
	case "envoy_proxy":
		return "EnvoyProxy"
	case "gateway":
		return "Gateway"
	case "route":
		return "HTTPRoute"
	case "tcp_route":
		return "TCPRoute"
	case "udp_route":
		return "UDPRoute"
	case "client_policy":
		return "ClientTrafficPolicy"
	case "patch_policy":
		return "EnvoyPatchPolicy"
	case "policy":
		return "BackendTrafficPolicy"
	case "backend":
		return "Backend"
	}
	return ""
}

type lbResolved struct {
	work                      biz.Work
	binding, vpc, subnet, eip sqlcgen.NetworkProviderBinding
}

func (p *KCProvider) resolveLB(ctx context.Context, w biz.Work) (lbResolved, error) {
	r := lbResolved{work: w}
	if w.Resource.LoadBalancer == nil {
		return r, &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	if _, ok := biz.FindLoadBalancerFlavor(w.Resource.LoadBalancer.LoadBalancer.Flavor); !ok {
		return r, &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	var err error
	r.binding, err = p.binding(ctx, biz.ProviderTarget{Kind: "load_balancer", TenantID: w.Resource.TenantID, ResourceID: w.Resource.ID, BindingID: w.BindingID, KnownIdentity: w.KnownIdentity})
	if err != nil {
		return r, err
	}
	for id, out := range map[string]*sqlcgen.NetworkProviderBinding{w.Resource.VPCID: &r.vpc, w.Resource.LoadBalancer.LoadBalancer.SubnetID: &r.subnet} {
		*out, err = p.repository.readQueries(ctx).GetBinding(ctx, sqlcgen.GetBindingParams{TenantID: w.Resource.TenantID, ResourceID: id})
		if err != nil {
			return r, readFailure(err)
		}
		if out.ClusterID != r.binding.ClusterID || out.Namespace != r.binding.Namespace {
			return r, &biz.ProviderError{Kind: biz.ProviderConflict}
		}
	}
	if id := w.Resource.LoadBalancer.LoadBalancer.PublicEIPID; id != "" {
		r.eip, err = p.repository.readQueries(ctx).GetBinding(ctx, sqlcgen.GetBindingParams{TenantID: w.Resource.TenantID, ResourceID: id})
		if err != nil {
			return r, readFailure(err)
		}
		if r.eip.Namespace != r.binding.Namespace || r.eip.ClusterID != r.binding.ClusterID {
			return r, &biz.ProviderError{Kind: biz.ProviderConflict}
		}
	}
	return r, nil
}
func lbComponentName(w biz.Work, kind string) string {
	for _, c := range w.Resource.LoadBalancer.Components {
		if c.Kind == kind && !c.Deleted {
			return c.Name
		}
	}
	return ""
}
func lbMemberEligible(o biz.LoadBalancerObservation, id string) bool {
	for _, m := range o.Members {
		if m.ID == id {
			return m.Eligible
		}
	}
	return false
}
func lbDesiredObject(r lbResolved, c biz.LoadBalancerComponent, o biz.LoadBalancerObservation) *unstructured.Unstructured {
	l := r.work.Resource.LoadBalancer.LoadBalancer
	spec := map[string]any{}
	version := l.ProviderTargetVersion
	switch c.Kind {
	case "envoy_proxy":
		flavor, ok := biz.FindLoadBalancerFlavor(l.Flavor)
		if !ok {
			return nil
		}
		serviceType := "LoadBalancer"
		if l.Exposure == "private" {
			serviceType = "ClusterIP"
		}
		spec = map[string]any{"mergeType": "StrategicMerge", "provider": map[string]any{"type": "Kubernetes", "kubernetes": map[string]any{
			"envoyDeployment": map[string]any{"replicas": flavor.Replicas, "container": map[string]any{"resources": map[string]any{
				"requests": map[string]any{"cpu": flavor.RequestCPU, "memory": flavor.RequestMemory},
				"limits":   map[string]any{"cpu": flavor.LimitCPU, "memory": flavor.LimitMemory},
			}}},
			"envoyService": map[string]any{"type": serviceType},
		}}}
	case "gateway":
		vip := l.PrivateIP
		if vip == "" {
			vip = "disable"
		}
		a := map[string]any{"networking.kubercloud.com/lb_vpc": r.vpc.Namespace + "/" + r.vpc.ProviderName, "networking.kubercloud.com/subnet": r.subnet.Namespace + "/" + r.subnet.ProviderName, "networking.kubercloud.com/lb_vip_address": vip}
		if l.PublicEIPID != "" {
			a["networking.kubercloud.com/lb_eips"] = r.eip.ProviderName
		}
		listeners := []any{}
		for _, listener := range biz.ProjectLoadBalancerListeners(biz.EffectiveLoadBalancerListeners(l)) {
			listeners = append(listeners, lbGatewayListener(listener))
		}
		spec = map[string]any{"gatewayClassName": biz.LoadBalancerGatewayClass, "infrastructure": map[string]any{"annotations": a, "parametersRef": map[string]any{"group": lbEnvoyProxies.Group, "kind": "EnvoyProxy", "name": lbComponentName(r.work, "envoy_proxy")}}, "listeners": listeners}
	case "backend":
		version = 1
		for _, m := range r.work.Resource.LoadBalancer.Members {
			if m.ID == c.MemberID {
				spec = map[string]any{"type": "Endpoints", "endpoints": []any{map[string]any{"ip": map[string]any{"address": m.Address, "port": int64(m.Port)}}}}
			}
		}
	case "route", "tcp_route", "udp_route":
		listener := lbComponentListener(r.work, c)
		refs := []any{}
		for _, m := range listener.Backends {
			if lbMemberEligible(o, m.ID) {
				for _, backend := range r.work.Resource.LoadBalancer.Components {
					if backend.Kind == "backend" && backend.MemberID == m.ID {
						refs = append(refs, map[string]any{"group": lbBackends.Group, "kind": "Backend", "name": backend.Name, "port": int64(m.Port), "weight": int64(m.Weight)})
					}
				}
			}
		}
		rule := lbRouteRule(listener, refs)
		if listener.Parameters.HTTPSRedirectStatusCode != 0 {
			target, _ := biz.ResolveLoadBalancerRedirectTarget(biz.EffectiveLoadBalancerListeners(l))
			rule["filters"].([]any)[0].(map[string]any)["requestRedirect"].(map[string]any)["port"] = int64(target.Port)
		}

		spec = map[string]any{"parentRefs": []any{map[string]any{"group": lbGateways.Group, "kind": "Gateway", "name": r.binding.ProviderName, "sectionName": listener.Name}}, "rules": []any{rule}}
	case "policy":
		spec = lbBackendPolicy(r, c)
	case "client_policy":
		listener := lbComponentListener(r.work, c)
		spec = map[string]any{"targetRefs": []any{map[string]any{"group": lbGateways.Group, "kind": "Gateway", "name": r.binding.ProviderName, "sectionName": listener.Name}}, "connection": map[string]any{"connectionLimit": map[string]any{"value": int64(listener.Parameters.MaxConnections)}}}
	case "patch_policy":
		spec = lbUDPHealthPolicy(r, c)

	}
	gvr := lbGVR(c.Kind)
	return &unstructured.Unstructured{Object: map[string]any{"apiVersion": gvr.Group + "/" + gvr.Version, "kind": lbKind(c.Kind), "metadata": map[string]any{"name": c.Name, "namespace": r.binding.Namespace, "labels": map[string]any{ownerLabel: "ani-network-service", tenantLabel: r.work.Resource.TenantID, resourceLabel: r.work.Resource.ID, bindingLabel: c.ID}, "annotations": map[string]any{lbVersionAnnotation: strconv.FormatInt(version, 10)}}, "spec": spec}}
}
func lbConditions(raw []any, generation int64, kinds ...string) (*bool, biz.Reason) {
	var missing bool
	for _, kind := range kinds {
		var found bool
		for _, item := range raw {
			c, ok := item.(map[string]any)
			if !ok || c["type"] != kind {
				continue
			}
			found = true
			reason, _ := c["reason"].(string)
			// Gateway API publishes the generation acknowledged for this declaration.
			// Missing generations remain consumable; an explicitly old result is pending.
			if g, present, _ := unstructured.NestedInt64(c, "observedGeneration"); present && g < generation {
				missing = true
				break
			}
			switch c["status"] {
			case "False":
				value := false
				return &value, biz.Reason(reason)
			case "True":
			default:
				missing = true
			}
			break
		}
		if !found {
			missing = true
		}
	}
	if missing {
		return nil, ""
	}
	value := true
	return &value, ""
}
func lbObjectReady(obj, desired *unstructured.Unstructured, c biz.LoadBalancerComponent, gateway, namespace string) (*bool, biz.Reason) {
	if obj.GetDeletionTimestamp() != nil {
		return nil, ""
	}
	if c.Kind == "envoy_proxy" {
		value := true
		return &value, ""
	}
	conditions, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	switch c.Kind {
	case "backend":
		return lbConditions(conditions, obj.GetGeneration(), "Accepted")
	case "patch_policy":
		return lbConditions(conditions, obj.GetGeneration(), "Accepted", "Programmed")
	case "gateway":
		ready, reason := lbConditions(conditions, obj.GetGeneration(), "Accepted", "Programmed")
		if ready != nil && !*ready {
			return ready, reason
		}
		missing := ready == nil
		listeners, _, _ := unstructured.NestedSlice(obj.Object, "status", "listeners")
		wanted, _, _ := unstructured.NestedSlice(desired.Object, "spec", "listeners")
		for _, raw := range wanted {
			want, _ := raw.(map[string]any)
			found := false
			for _, item := range listeners {
				listener, _ := item.(map[string]any)
				if listener["name"] != want["name"] {
					continue
				}
				found = true
				lc, _, _ := unstructured.NestedSlice(listener, "conditions")
				state, why := lbConditions(lc, obj.GetGeneration(), "Accepted", "Programmed", "ResolvedRefs")
				if state != nil && !*state {
					return state, why
				}
				missing = missing || state == nil
				break
			}
			missing = missing || !found
		}
		if missing {
			return nil, ""
		}
		value := true
		return &value, ""
	}
	field, refField := "parents", "parentRef"
	if c.Kind == "policy" || c.Kind == "client_policy" {
		field, refField = "ancestors", "ancestorRef"
	}
	entries, _, _ := unstructured.NestedSlice(obj.Object, "status", field)
	for _, raw := range entries {
		e, ok := raw.(map[string]any)
		if !ok || e["controllerName"] != lbController {
			continue
		}
		ref, _, _ := unstructured.NestedMap(e, refField)
		ns, _ := ref["namespace"].(string)
		if ns == "" {
			ns = namespace
		}
		kind, _ := ref["kind"].(string)
		if kind == "" {
			kind = "Gateway"
		}
		group, _ := ref["group"].(string)
		if group == "" {
			group = lbGateways.Group
		}
		if ref["name"] != gateway || ns != namespace || kind != "Gateway" || group != lbGateways.Group {
			continue
		}
		if biz.LoadBalancerIsRoute(c.Kind) && c.ListenerName != "" && ref["sectionName"] != c.ListenerName {
			continue
		}
		conditions, _, _ := unstructured.NestedSlice(e, "conditions")
		if c.Kind == "policy" || c.Kind == "client_policy" {
			return lbConditions(conditions, obj.GetGeneration(), "Accepted")
		}
		return lbConditions(conditions, obj.GetGeneration(), "Accepted", "ResolvedRefs")
	}
	return nil, ""
}
func lbInspect(obj, desired *unstructured.Unstructured, c biz.LoadBalancerComponent, r lbResolved) (biz.LoadBalancerComponentObservation, error) {
	v := biz.LoadBalancerComponentObservation{ID: c.ID}
	if obj == nil {
		return v, nil
	}
	v.Exists, v.Identity, v.Deleting = true, string(obj.GetUID()), obj.GetDeletionTimestamp() != nil
	labels := obj.GetLabels()
	if desired == nil || obj.GetName() != c.Name || obj.GetNamespace() != r.binding.Namespace || obj.GetUID() == "" || obj.GetAPIVersion() != desired.GetAPIVersion() || obj.GetKind() != desired.GetKind() || labels[ownerLabel] != "ani-network-service" || labels[tenantLabel] != r.work.Resource.TenantID || labels[resourceLabel] != r.work.Resource.ID || labels[bindingLabel] != c.ID || (c.Identity != "" && c.Identity != v.Identity) {
		v.Conflict = true
		return v, &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	v.Matches = lbManagedSpecMatches(obj.Object["spec"], desired.Object["spec"], c.Kind) && obj.GetAnnotations()[lbVersionAnnotation] == desired.GetAnnotations()[lbVersionAnnotation]
	v.Ready, v.StatusReason = lbObjectReady(obj, desired, c, r.binding.ProviderName, r.binding.Namespace)
	return v, nil
}

func (p *KCProvider) ObserveLoadBalancer(ctx context.Context, w biz.Work) (biz.LoadBalancerObservation, error) {
	o := biz.LoadBalancerObservation{}
	r, err := p.resolveLB(ctx, w)
	if err != nil {
		return o, err
	}
	for _, m := range w.Resource.LoadBalancer.Members {
		desired := false
		for _, b := range w.Resource.LoadBalancer.LoadBalancer.Backends {
			desired = desired || b.ID == m.ID
		}
		if !desired {
			continue
		}
		eligible, err := p.lbMemberIdentity(ctx, r, m)
		if err != nil {
			return biz.LoadBalancerObservation{}, err
		}
		reason := biz.Reason("")
		if !eligible {
			reason = biz.BackendIdentityMismatch
		}
		o.Members = append(o.Members, biz.LoadBalancerMemberObservation{ID: m.ID, Eligible: eligible, Reason: reason})
	}
	for _, c := range w.Resource.LoadBalancer.Components {
		obj, err := p.client.Resource(lbGVR(c.Kind)).Namespace(r.binding.Namespace).Get(ctx, c.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			obj, err = nil, nil
		}
		if err != nil {
			return biz.LoadBalancerObservation{}, readFailure(err)
		}
		v, err := lbInspect(obj, lbDesiredObject(r, c, o), c, r)
		if err != nil {
			v.Conflict = true
		}
		o.Components = append(o.Components, v)
	}
	return o, nil
}

func (p *KCProvider) lbMemberIdentity(ctx context.Context, r lbResolved, m biz.LoadBalancerMemberIdentity) (bool, error) {
	a, err := p.repository.readQueries(ctx).GetAttachment(ctx, sqlcgen.GetAttachmentParams{TenantID: r.work.Resource.TenantID, AttachmentID: m.AttachmentID})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, readFailure(err)
	}
	if a.State != "attached" || a.PodUid != m.PodUID || a.SubnetID != m.SubnetID || a.VpcID != r.work.Resource.VPCID || a.Namespace != r.binding.Namespace || a.ClusterID != r.binding.ClusterID {
		return false, nil
	}
	for _, address := range a.CurrentIps {
		if address == m.Address {
			return true, nil
		}
	}
	return false, nil
}

func (p *KCProvider) MutateLoadBalancer(ctx context.Context, w biz.Work, c biz.LoadBalancerComponent, action string, observed biz.LoadBalancerObservation) (biz.LoadBalancerComponentObservation, error) {
	v := biz.LoadBalancerComponentObservation{ID: c.ID}
	r, err := p.resolveLB(ctx, w)
	if err != nil {
		return v, err
	}
	desired := lbDesiredObject(r, c, observed)
	endpoint := p.client.Resource(lbGVR(c.Kind)).Namespace(r.binding.Namespace)
	obj, err := endpoint.Get(ctx, c.Name, metav1.GetOptions{})
	if action == "create" {
		if err == nil {
			return lbInspect(obj, desired, c, r)
		}
		if !apierrors.IsNotFound(err) {
			return v, readFailure(err)
		}
		if c.Identity != "" {
			return v, &biz.ProviderError{Kind: biz.ProviderConflict}
		}
		obj, err = endpoint.Create(ctx, desired, metav1.CreateOptions{FieldManager: "ani-network-service", FieldValidation: "Strict"})
		if apierrors.IsAlreadyExists(err) {
			obj, err = endpoint.Get(ctx, c.Name, metav1.GetOptions{})
			if err != nil {
				return v, &biz.ProviderError{Kind: biz.ProviderUncertain, Cause: err}
			}
		}
	} else {
		if apierrors.IsNotFound(err) && action == "delete" {
			return v, nil
		}
		if err != nil {
			return v, readFailure(err)
		}
		if action == "delete" && c.Identity != "" && c.Identity != string(obj.GetUID()) {
			return v, nil
		}
		v, err = lbInspect(obj, desired, c, r)
		if err != nil {
			return v, err
		}
		if v.Identity == "" {
			return v, &biz.ProviderError{Kind: biz.ProviderConflict}
		}
		if action == "delete" {
			uid := obj.GetUID()
			rv := obj.GetResourceVersion()
			err = endpoint.Delete(ctx, c.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv}})
			if apierrors.IsNotFound(err) {
				err = nil
			}
			if err != nil {
				return v, mutationFailure(err)
			}
			v.Deleting = true
			return v, nil
		}
		if action != "update" || (!biz.LoadBalancerIsRoute(c.Kind) && c.Kind != "policy" && c.Kind != "client_policy" && c.Kind != "patch_policy" && c.Kind != "gateway" && c.Kind != "envoy_proxy") {
			return v, &biz.ProviderError{Kind: biz.ProviderConflict}
		}
		annotations := obj.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[lbVersionAnnotation] = desired.GetAnnotations()[lbVersionAnnotation]
		patch, _ := json.Marshal([]map[string]any{{"op": "test", "path": "/metadata/uid", "value": string(obj.GetUID())}, {"op": "test", "path": "/metadata/resourceVersion", "value": obj.GetResourceVersion()}, {"op": "replace", "path": "/spec", "value": desired.Object["spec"]}, {"op": "add", "path": "/metadata/annotations", "value": annotations}})
		obj, err = endpoint.Patch(ctx, c.Name, types.JSONPatchType, patch, metav1.PatchOptions{FieldManager: "ani-network-service", FieldValidation: "Strict"})
	}
	if err != nil {
		return v, mutationFailure(err)
	}
	v, err = lbInspect(obj, desired, c, r)
	if err != nil {
		return v, &biz.ProviderError{Kind: biz.ProviderUncertain, Cause: err}
	}
	return v, nil
}

func lbComponentListener(w biz.Work, c biz.LoadBalancerComponent) biz.LoadBalancerListener {
	for _, l := range biz.EffectiveLoadBalancerListeners(w.Resource.LoadBalancer.LoadBalancer) {
		if l.ID == c.ListenerID || c.ListenerID == "" {
			return l
		}
	}
	return biz.LoadBalancerListener{ID: c.ListenerID, Name: c.ListenerName, Protocol: "HTTP"}
}
func lbListenerRouteName(w biz.Work, id string) string {
	for _, c := range w.Resource.LoadBalancer.Components {
		if biz.LoadBalancerIsRoute(c.Kind) && c.ListenerID == id && !c.Deleted && c.Kind == biz.LoadBalancerRouteKind(lbComponentListener(w, c).Protocol) {
			return c.Name
		}
	}
	return ""
}

// Optional fields written by Res must also disappear when removed from the
// desired configuration. Provider defaults outside these fields are ignored.
func lbManagedSpecMatches(actual, desired any, kind string) bool {
	if !managedFieldsMatch(actual, desired) {
		return false
	}
	got, _ := actual.(map[string]any)
	want, _ := desired.(map[string]any)
	optional := func(a, b map[string]any, fields ...string) bool {
		_, wanted, _ := unstructured.NestedFieldNoCopy(b, fields...)
		if wanted {
			return true
		}
		value, present, _ := unstructured.NestedFieldNoCopy(a, fields...)
		if !present || value == nil {
			return true
		}
		switch v := value.(type) {
		case []any:
			return len(v) == 0
		case map[string]any:
			return len(v) == 0
		case string:
			return v == ""
		case bool:
			return !v
		}
		return false
	}
	if biz.LoadBalancerIsRoute(kind) {
		ar, _, _ := unstructured.NestedSlice(got, "rules")
		br, _, _ := unstructured.NestedSlice(want, "rules")
		for i := range br {
			a, _ := ar[i].(map[string]any)
			b, _ := br[i].(map[string]any)
			for _, field := range []string{"backendRefs", "filters", "sessionPersistence"} {
				if !optional(a, b, field) {
					return false
				}
			}
			if !optional(a, b, "sessionPersistence", "absoluteTimeout") {
				return false
			}
		}
	}
	if kind == "gateway" {
		al, _, _ := unstructured.NestedSlice(got, "listeners")
		bl, _, _ := unstructured.NestedSlice(want, "listeners")
		for i := range bl {
			a, _ := al[i].(map[string]any)
			b, _ := bl[i].(map[string]any)
			if !optional(a, b, "tls") {
				return false
			}
		}
	}
	if kind == "policy" {
		for _, fields := range [][]string{{"compressor"}, {"healthCheck", "active", "overrides"}, {"healthCheck", "active", "http", "hostname"}} {
			if !optional(got, want, fields...) {
				return false
			}
		}
	}
	return true
}
