package clusterops

import (
	"context"
	"encoding/json"
	"github.com/google/uuid"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/clusterops"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
	"os"
	"strings"
	"testing"
	"time"
)

type fixtureRegistrations struct{ names []string }

func (s *fixtureRegistrations) ListRegisteredNamespaceNames(context.Context, string) ([]string, error) {
	return append([]string(nil), s.names...), nil
}
func TestClusterOpsLiveTemporaryResources(t *testing.T) {
	path := os.Getenv("CLUSTEROPS_TEST_KUBECONFIG")
	if path == "" {
		t.Skip("requires explicitly authorized 66 test cluster and private temporary kubeconfig")
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		t.Fatal("live config unavailable")
	}
	k, err := NewKubernetes(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	node, err := k.typed.CoreV1().Nodes().Get(ctx, "node1", metav1.GetOptions{})
	if err != nil {
		t.Fatal("cannot verify 66 identity")
	}
	found := false
	for _, a := range node.Status.Addresses {
		found = found || a.Address == "10.10.1.66"
	}
	if !found {
		t.Fatal("refusing writes to a different cluster")
	}
	namespace := "clusterops-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	cluster := "clusterops-live-fixture"
	t.Log("fixture Namespace:", namespace)
	group := namespace + ".ani-test.io"
	crdName := "widgets." + group
	source := new(fixtureRegistrations)
	uc, err := biz.NewUseCase(cluster, k, biz.NewNamespacePolicy(source, &fixtureRegistrations{}))
	if err != nil {
		t.Fatal(err)
	}
	admin := biz.WithCaller(ctx, biz.Caller{Actor: "governance:user:7", DirectCaller: "ani-governance", RequestID: uuid.NewString(), Platform: true})
	ref := func(group, resource, name string, namespaced bool) biz.ResourceRef {
		ns := ""
		if namespaced {
			ns = namespace
		}
		return biz.ResourceRef{ClusterID: cluster, Type: biz.ResourceTypeRef{Group: group, Version: "v1", Resource: resource}, Namespace: ns, Name: name}
	}
	create := func(r biz.ResourceRef, kind string, extra map[string]any) biz.ResourceDocument {
		t.Helper()
		v := map[string]any{"apiVersion": r.Type.Version, "kind": kind, "metadata": map[string]any{"name": r.Name}}
		if r.Type.Group != "" {
			v["apiVersion"] = r.Type.Group + "/" + r.Type.Version
		}
		for key, value := range extra {
			v[key] = value
		}
		body, _ := json.Marshal(v)
		out, e := uc.CreateResource(admin, biz.CreateCommand{Ref: r, ManifestYAML: string(body)})
		if e != nil {
			t.Fatalf("create %s: %s", kind, errorReason(e))
		}
		return out.Resource
	}
	get := func(r biz.ResourceRef) biz.ResourceDetail {
		t.Helper()
		d, e := uc.GetResource(admin, biz.GetQuery{Ref: r})
		if e != nil {
			t.Fatal(errorReason(e))
		}
		return d
	}
	update := func(d biz.ResourceDetail, manifest string) biz.ResourceDocument {
		t.Helper()
		out, e := uc.UpdateResource(admin, biz.UpdateCommand{Ref: d.Resource.Ref, ManifestYAML: manifest, ExpectedUID: d.Resource.UID, ExpectedResourceVersion: d.Resource.ResourceVersion})
		if e != nil {
			t.Fatal(errorReason(e))
		}
		return out.Resource
	}
	remove := func(d biz.ResourceDocument) {
		t.Helper()
		_, e := uc.DeleteResource(admin, biz.DeleteCommand{Ref: d.Ref, ExpectedUID: d.UID})
		if e != nil {
			t.Fatal(errorReason(e))
		}
	}
	ns := create(ref("", "namespaces", namespace, false), "Namespace", nil)
	clusterObjects := []biz.ResourceDocument{}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		for _, d := range clusterObjects {
			uid := types.UID(d.UID)
			e := k.resource(d.Ref).Delete(cleanup, d.Ref.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
			if e != nil && !apierrors.IsNotFound(e) {
				t.Error("task cluster-object cleanup failed", d.Ref.Name)
			}
		}
		uid := types.UID(ns.UID)
		e := k.resource(ns.Ref).Delete(cleanup, namespace, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
		if e != nil && !apierrors.IsNotFound(e) {
			t.Error("task Namespace cleanup request failed")
		}
		for cleanup.Err() == nil {
			_, e := k.get(cleanup, ns.Ref)
			if isNotFound(e) {
				t.Log("cleanup pass:", namespace, crdName)
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
		t.Error("task Namespace disappearance not verified")
	})
	overview, e := uc.GetClusterOverview(admin, cluster)
	if e != nil || overview.NodesTotal < 3 {
		t.Fatal("overview", errorReason(e))
	}
	cm := create(ref("", "configmaps", "config", true), "ConfigMap", map[string]any{"data": map[string]any{"key": "one"}})
	original := get(cm.Ref)
	updated := update(original, strings.ReplaceAll(original.ManifestYAML, "one", "two"))
	if _, e = uc.UpdateResource(admin, biz.UpdateCommand{Ref: cm.Ref, ExpectedUID: cm.UID, ExpectedResourceVersion: original.Resource.ResourceVersion, ManifestYAML: original.ManifestYAML}); errorReason(e) != "RESOURCE_VERSION_CONFLICT" {
		t.Fatal("stale RV accepted", errorReason(e))
	}
	// Verify the API server itself rejects a stale RV, beyond the adapter GET check.
	stale := decode(t, original.ManifestYAML)
	stale.SetNamespace(namespace)
	stale.SetUID(types.UID(cm.UID))
	stale.SetResourceVersion(original.Resource.ResourceVersion)
	if _, e = k.resource(cm.Ref).Update(admin, stale, metav1.UpdateOptions{}); !apierrors.IsConflict(e) {
		t.Fatal("API server stale-RV conflict missing")
	}
	remove(updated)
	replacement := create(cm.Ref, "ConfigMap", map[string]any{"data": map[string]any{"key": "replacement"}})
	if _, e = uc.DeleteResource(admin, biz.DeleteCommand{Ref: cm.Ref, ExpectedUID: cm.UID}); errorReason(e) != "RESOURCE_UID_CHANGED" {
		t.Fatal("old UID deleted replacement", errorReason(e))
	}
	if get(cm.Ref).Resource.UID != replacement.UID {
		t.Fatal("replacement identity changed")
	}
	secret := create(ref("", "secrets", "secret", true), "Secret", map[string]any{"stringData": map[string]any{"key": "clusterops-secret-fixture"}})
	if !secret.SecretDataRedacted || strings.Contains(string(secret.ObjectJSON), "clusterops-secret-fixture") {
		t.Fatal("save response leaked Secret")
	}
	if d := get(secret.Ref); d.ManifestYAML != "" || !d.Resource.SecretDataRedacted {
		t.Fatal("default Secret detail editable")
	}
	full, e := uc.GetResource(admin, biz.GetQuery{Ref: secret.Ref, IncludeSecretData: true})
	if e != nil || full.ManifestYAML == "" || full.Resource.SecretDataRedacted {
		t.Fatal("explicit Secret detail failed")
	}
	if _, e = uc.UpdateResource(admin, biz.UpdateCommand{Ref: secret.Ref, ExpectedUID: full.Resource.UID, ExpectedResourceVersion: full.Resource.ResourceVersion, ManifestYAML: "apiVersion: v1\nkind: Secret\nmetadata: {name: secret}"}); errorReason(e) != "INVALID_ARGUMENT" {
		t.Fatal("redacted Secret accepted for edit", errorReason(e))
	}
	update(full, "apiVersion: v1\nkind: Secret\nmetadata: {name: secret}\ndata: {}")
	deployment := create(ref("apps", "deployments", "workload", true), "Deployment", map[string]any{"spec": map[string]any{"replicas": 0, "selector": map[string]any{"matchLabels": map[string]any{"app": namespace}}, "template": map[string]any{"metadata": map[string]any{"labels": map[string]any{"app": namespace}}, "spec": map[string]any{"containers": []any{map[string]any{"name": "main", "image": "registry.k8s.io/pause:3.10"}}}}}})
	// Wait for this zero-replica fixture's initial controller status write,
	// then explicitly refresh the version used by the next client request.
	for {
		current, e := k.typed.AppsV1().Deployments(namespace).Get(admin, "workload", metav1.GetOptions{})
		if e != nil {
			t.Fatal("fixture Deployment read failed")
		}
		if current.Status.ObservedGeneration >= current.Generation {
			break
		}
		select {
		case <-admin.Done():
			t.Fatal("fixture controller did not observe Deployment")
		case <-time.After(100 * time.Millisecond):
		}
	}
	deployment = get(deployment.Ref).Resource
	zero := int32(0)
	scaled, e := uc.ScaleWorkload(admin, biz.ScaleCommand{Ref: deployment.Ref, ExpectedUID: deployment.UID, ExpectedResourceVersion: deployment.ResourceVersion, Replicas: &zero})
	if e != nil || scaled.DesiredReplicas != 0 {
		t.Fatal("zero scale", errorReason(e))
	}
	deployment = get(deployment.Ref).Resource
	restarted, e := uc.RestartWorkload(admin, biz.RestartCommand{Ref: deployment.Ref, ExpectedUID: deployment.UID, ExpectedResourceVersion: deployment.ResourceVersion})
	if e != nil {
		t.Fatal("restart", errorReason(e))
	}
	if !strings.Contains(string(restarted.Resource.ObjectJSON), "kubectl.kubernetes.io/restartedAt") {
		t.Fatal("restart annotation absent")
	}
	sa := create(ref("", "serviceaccounts", "account", true), "ServiceAccount", nil)
	rule := []any{map[string]any{"apiGroups": []any{""}, "resources": []any{"pods"}, "verbs": []any{"get"}}}
	role := create(ref("rbac.authorization.k8s.io", "roles", "reader", true), "Role", map[string]any{"rules": rule})
	binding := create(ref("rbac.authorization.k8s.io", "rolebindings", "reader", true), "RoleBinding", map[string]any{"subjects": []any{map[string]any{"kind": "ServiceAccount", "name": sa.Ref.Name, "namespace": namespace}}, "roleRef": map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": role.Ref.Name}})
	d := get(binding.Ref)
	bad := strings.ReplaceAll(d.ManifestYAML, "name: reader", "name: changed")
	// Change only roleRef.name; preserve the outer target name.
	o := decode(t, d.ManifestYAML)
	_ = unstructured.SetNestedField(o.Object, "different-role", "roleRef", "name")
	raw, _ := o.MarshalJSON()
	bad = string(raw)
	if _, e = uc.UpdateResource(admin, biz.UpdateCommand{Ref: binding.Ref, ExpectedUID: binding.UID, ExpectedResourceVersion: binding.ResourceVersion, ManifestYAML: bad}); errorReason(e) != "KUBERNETES_INVALID" {
		t.Fatal("roleRef immutable behavior", errorReason(e))
	}
	if get(binding.Ref).Resource.UID != binding.UID {
		t.Fatal("role binding recreated")
	}
	clusterRole := create(ref("rbac.authorization.k8s.io", "clusterroles", namespace, false), "ClusterRole", map[string]any{"rules": rule})
	clusterObjects = append(clusterObjects, clusterRole)
	clusterBinding := create(ref("rbac.authorization.k8s.io", "clusterrolebindings", namespace, false), "ClusterRoleBinding", map[string]any{"subjects": []any{map[string]any{"kind": "ServiceAccount", "name": sa.Ref.Name, "namespace": namespace}}, "roleRef": map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": namespace}})
	clusterObjects = append(clusterObjects, clusterBinding)
	for _, obj := range []biz.ResourceDocument{sa, role, binding, clusterRole, clusterBinding} {
		d := get(obj.Ref)
		o := decode(t, d.ManifestYAML)
		o.SetLabels(map[string]string{"clusterops-test": "true"})
		body, _ := o.MarshalJSON()
		updated := update(d, string(body))
		if updated.UID != obj.UID {
			t.Fatal("RBAC object recreated")
		}
	}
	crdRef := ref("apiextensions.k8s.io", "customresourcedefinitions", crdName, false)
	// CRD creation is a controlled native fixture, since the product intentionally
	// has no CRD Create action. There is deliberately no /status subresource.
	schema := map[string]any{"type": "object", "properties": map[string]any{"spec": map[string]any{"type": "object", "x-kubernetes-preserve-unknown-fields": true}, "status": map[string]any{"type": "object", "x-kubernetes-preserve-unknown-fields": true}}}
	crd := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition", "metadata": map[string]any{"name": crdName}, "spec": map[string]any{"group": group, "scope": "Namespaced", "names": map[string]any{"plural": "widgets", "singular": "widget", "kind": "Widget"}, "versions": []any{map[string]any{"name": "v1", "served": true, "storage": true, "schema": map[string]any{"openAPIV3Schema": schema}}}}}}
	created, e := k.resource(crdRef).Create(admin, crd, metav1.CreateOptions{})
	if e != nil {
		t.Fatal("CRD fixture creation failed")
	}
	crdDoc := biz.ResourceDocument{Ref: crdRef, UID: string(created.GetUID())}
	clusterObjects = append(clusterObjects, crdDoc)
	for {
		current, e := k.get(admin, crdRef)
		if e != nil {
			t.Fatal(errorReason(e))
		}
		conditions, _, _ := unstructured.NestedSlice(current.Object, "status", "conditions")
		ready := false
		for _, c := range conditions {
			m, _ := c.(map[string]any)
			ready = ready || (m["type"] == "Established" && m["status"] == "True")
		}
		if ready {
			break
		}
		select {
		case <-admin.Done():
			t.Fatal("CRD not established")
		case <-time.After(100 * time.Millisecond):
		}
	}
	descriptors, e := uc.ListResourceTypes(admin, cluster)
	if e != nil {
		t.Fatal(errorReason(e))
	}
	found = false
	for _, d := range descriptors {
		found = found || (d.Type.Group == group && d.CustomResource && d.CRDName == crdName)
	}
	if !found {
		t.Fatal("live CRD not discovered")
	}
	crRef := ref(group, "widgets", "sample", true)
	cr := &unstructured.Unstructured{Object: map[string]any{"apiVersion": group + "/v1", "kind": "Widget", "metadata": map[string]any{"name": "sample", "namespace": namespace}, "spec": map[string]any{"count": int64(9007199254740993)}, "status": map[string]any{"state": "native"}}}
	if _, e = k.resource(crRef).Create(admin, cr, metav1.CreateOptions{}); e != nil {
		t.Fatal("native CR fixture failed")
	}
	detail := get(crRef)
	update(detail, strings.ReplaceAll(detail.ManifestYAML, "9007199254740993", "9007199254740995"))
	rawCR, e := k.get(admin, crRef)
	if e != nil {
		t.Fatal(errorReason(e))
	}
	n, _, _ := unstructured.NestedInt64(rawCR.Object, "spec", "count")
	state, _, _ := unstructured.NestedString(rawCR.Object, "status", "state")
	if n != 9007199254740995 || state != "native" {
		t.Fatal("CR integer/status roundtrip failed")
	}
	// Protected registration is test-owned and never changes a real tenant row.
	source.names = []string{namespace}
	if _, e = uc.DeleteResource(admin, biz.DeleteCommand{Ref: crdRef, ExpectedUID: crdDoc.UID}); errorReason(e) != "CRD_HAS_PROTECTED_NAMESPACE_INSTANCES" {
		t.Fatal("CRD tenant preflight", errorReason(e))
	}
	if _, e = uc.CreateResource(admin, biz.CreateCommand{Ref: ref("", "configmaps", "denied", true), ManifestYAML: "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: denied}", DryRun: true}); errorReason(e) != "TENANT_NAMESPACE_READ_ONLY" {
		t.Fatal("registered dry-run write allowed", errorReason(e))
	}
	if d := get(crRef); !d.Resource.ReadOnly {
		t.Fatal("protected CR not marked read-only")
	}
	if _, e = uc.ListResources(admin, biz.ListQuery{ClusterID: cluster, Type: biz.ResourceTypeRef{Version: "v1", Resource: "events"}, AllNamespaces: true, FieldSelector: "involvedObject.uid=" + ns.UID}); e != nil {
		t.Fatal("native Events query failed", errorReason(e))
	}
	source.names = nil
	// CRD definition editing is immediately visible to live discovery (no cache).
	definition := get(crdRef)
	o = decode(t, definition.ManifestYAML)
	o.SetAnnotations(map[string]string{"clusterops-test": "updated"})
	body, _ := o.MarshalJSON()
	update(definition, string(body))
	remove(get(crRef).Resource)
	remove(get(crdRef).Resource)
	for _, obj := range []biz.ResourceDocument{clusterBinding, clusterRole, binding, role, sa} {
		remove(get(obj.Ref).Resource)
	}
	t.Log("live pass: overview, native CRUD, RV/UID conflicts, Secret outlets, zero scale, restart, five RBAC types, CRD discovery/edit/protected delete, CR status/int64, Events")
}
