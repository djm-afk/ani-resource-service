package sandbox

import (
	"context"
	"errors"
	"testing"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/sandbox"
	storage "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/storage"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestTenantAccessProvisioningIsIdempotentAndRejectsForeignBindings(t *testing.T) {
	const tenant = "11111111-1111-4111-8111-111111111111"
	const ns = "tenant-" + tenant
	ctx := context.Background()
	namespace := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Namespace", "status": map[string]any{"phase": "Active"}, "metadata": map[string]any{"name": ns, "uid": "uid-1", "labels": map[string]any{storage.NamespaceManagedBy: storage.NamespaceOwner, storage.NamespaceTenantID: tenant}}}}
	client := fake.NewSimpleDynamicClient(runtime.NewScheme(), namespace)
	access := TenantAccess{Client: client, NamespacePrefix: "tenant-", ServiceAccountNamespace: "ani-system", ServiceAccountName: "ani-resource-service"}
	if err := access.ensure(ctx, tenant, "uid-1"); err != nil {
		t.Fatal(err)
	}
	if err := access.ensure(ctx, tenant, "uid-1"); err != nil {
		t.Fatalf("replay: %v", err)
	}
	role, err := client.Resource(tenantRoleGVR).Namespace(ns).Get(ctx, "ani-sandbox-pull-secret", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	rules, _, _ := unstructured.NestedSlice(role.Object, "rules")
	if len(rules) != 1 || rules[0].(map[string]any)["resourceNames"].([]any)[0] != "ani-sandbox-pull-"+tenant {
		t.Fatalf("unexpected secret scope: %v", rules)
	}
	binding, err := client.Resource(tenantBindingGVR).Namespace(ns).Get(ctx, "ani-sandbox-tenant", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	binding.Object["subjects"] = []any{map[string]any{"kind": "ServiceAccount", "name": "foreign", "namespace": "ani-system"}}
	if _, err := client.Resource(tenantBindingGVR).Namespace(ns).Update(ctx, binding, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := access.ensure(ctx, tenant, "uid-1"); !errors.Is(err, biz.ErrOwnership) {
		t.Fatalf("foreign subject accepted: %v", err)
	}
	stillForeign, err := client.Resource(tenantBindingGVR).Namespace(ns).Get(ctx, "ani-sandbox-tenant", metav1.GetOptions{})
	if err != nil || !reflectSubjectIsForeign(stillForeign) {
		t.Fatalf("foreign binding was changed: %v", err)
	}
	if err := access.ensure(ctx, tenant, "uid-replacement"); !errors.Is(err, biz.ErrOwnership) {
		t.Fatalf("replacement namespace accepted: %v", err)
	}
}

func reflectSubjectIsForeign(binding *unstructured.Unstructured) bool {
	subjects, _, _ := unstructured.NestedSlice(binding.Object, "subjects")
	return len(subjects) == 1 && subjects[0].(map[string]any)["name"] == "foreign"
}

func TestTenantAccessLostCreateResponseReadsReceipt(t *testing.T) {
	const tenant = "11111111-1111-4111-8111-111111111111"
	ns := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Namespace", "status": map[string]any{"phase": "Active"}, "metadata": map[string]any{"name": "tenant-" + tenant, "uid": "uid-1", "labels": map[string]any{storage.NamespaceManagedBy: storage.NamespaceOwner, storage.NamespaceTenantID: tenant}}}}
	client := fake.NewSimpleDynamicClient(runtime.NewScheme(), ns)
	client.PrependReactor("create", "roles", func(action ktesting.Action) (bool, runtime.Object, error) {
		obj := action.(ktesting.CreateAction).GetObject()
		if err := client.Tracker().Create(tenantRoleGVR, obj, "tenant-"+tenant); err != nil {
			t.Fatal(err)
		}
		return true, nil, errors.New("response lost")
	})
	access := TenantAccess{Client: client, NamespacePrefix: "tenant-", ServiceAccountNamespace: "ani-system", ServiceAccountName: "ani-resource-service"}
	if err := access.ensure(context.Background(), tenant, "uid-1"); err != nil {
		t.Fatal(err)
	}
	creates := 0
	for _, action := range client.Actions() {
		if action.GetVerb() == "create" && action.GetResource() == tenantRoleGVR {
			creates++
		}
	}
	if creates != 1 {
		t.Fatalf("role create attempts = %d", creates)
	}
}

func TestTenantAccessDoesNotBindAfterUnconfirmedRoleCreate(t *testing.T) {
	const tenant = "11111111-1111-4111-8111-111111111111"
	ns := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Namespace", "status": map[string]any{"phase": "Active"}, "metadata": map[string]any{"name": "tenant-" + tenant, "uid": "uid-1", "labels": map[string]any{storage.NamespaceManagedBy: storage.NamespaceOwner, storage.NamespaceTenantID: tenant}}}}
	client := fake.NewSimpleDynamicClient(runtime.NewScheme(), ns)
	client.PrependReactor("create", "roles", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("response lost")
	})
	access := TenantAccess{Client: client, NamespacePrefix: "tenant-", ServiceAccountNamespace: "ani-system", ServiceAccountName: "ani-resource-service"}
	if err := access.ensure(context.Background(), tenant, "uid-1"); err == nil {
		t.Fatal("unconfirmed role create was reported as successful")
	}
	for _, action := range client.Actions() {
		if action.GetVerb() == "create" && action.GetResource() == tenantBindingGVR {
			t.Fatal("binding created after unconfirmed role")
		}
	}
}

func TestTenantAccessDoesNotOverwriteNamespaceMarker(t *testing.T) {
	const tenant = "11111111-1111-4111-8111-111111111111"
	ns := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Namespace", "status": map[string]any{"phase": "Active"}, "metadata": map[string]any{"name": "tenant-" + tenant, "uid": "uid-1", "labels": map[string]any{storage.NamespaceManagedBy: storage.NamespaceOwner, storage.NamespaceTenantID: tenant}, "annotations": map[string]any{tenantAccessNamespaceUID: "old-uid"}}}}
	client := fake.NewSimpleDynamicClient(runtime.NewScheme(), ns)
	access := TenantAccess{Client: client, NamespacePrefix: "tenant-", ServiceAccountNamespace: "ani-system", ServiceAccountName: "ani-resource-service"}
	if err := access.ensure(context.Background(), tenant, "uid-1"); !errors.Is(err, biz.ErrOwnership) {
		t.Fatalf("stale namespace marker accepted: %v", err)
	}
	for _, action := range client.Actions() {
		if action.GetVerb() == "update" || action.GetVerb() == "create" {
			t.Fatalf("modified object after marker mismatch: %v", action)
		}
	}
}

func TestTenantAccessChecksNamespaceAgainBeforeBinding(t *testing.T) {
	const tenant = "11111111-1111-4111-8111-111111111111"
	nsName := "tenant-" + tenant
	ns := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Namespace", "status": map[string]any{"phase": "Active"}, "metadata": map[string]any{"name": nsName, "uid": "uid-1", "labels": map[string]any{storage.NamespaceManagedBy: storage.NamespaceOwner, storage.NamespaceTenantID: tenant}}}}
	client := fake.NewSimpleDynamicClient(runtime.NewScheme(), ns)
	client.PrependReactor("create", "roles", func(action ktesting.Action) (bool, runtime.Object, error) {
		obj := action.(ktesting.CreateAction).GetObject()
		if err := client.Tracker().Create(tenantRoleGVR, obj, nsName); err != nil {
			t.Fatal(err)
		}
		replacement := ns.DeepCopy()
		replacement.SetUID("uid-2")
		if err := client.Tracker().Update(namespaceGVR, replacement, ""); err != nil {
			t.Fatal(err)
		}
		return true, obj, nil
	})
	access := TenantAccess{Client: client, NamespacePrefix: "tenant-", ServiceAccountNamespace: "ani-system", ServiceAccountName: "ani-resource-service"}
	if err := access.ensure(context.Background(), tenant, "uid-1"); !errors.Is(err, biz.ErrOwnership) {
		t.Fatalf("replacement namespace accepted: %v", err)
	}
	for _, action := range client.Actions() {
		if action.GetVerb() == "create" && action.GetResource() == tenantBindingGVR {
			t.Fatal("binding created in a replacement namespace")
		}
	}
}

func TestTenantAccessRejectsMissingOrEscalatedClusterRole(t *testing.T) {
	role := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRole",
		"metadata": map[string]any{"name": "ani-sandbox-tenant", "uid": "role-uid"},
		"rules": []any{
			map[string]any{"apiGroups": []any{""}, "resources": []any{"pods", "persistentvolumeclaims"}, "verbs": []any{"get", "create", "delete"}},
			map[string]any{"apiGroups": []any{""}, "resources": []any{"pods/exec"}, "verbs": []any{"create"}},
			map[string]any{"apiGroups": []any{""}, "resources": []any{"secrets"}, "verbs": []any{"create"}},
		},
	}}
	client := fake.NewSimpleDynamicClient(runtime.NewScheme(), role)
	access := TenantAccess{Client: client}
	if err := access.verifyTenantClusterRole(context.Background()); err != nil {
		t.Fatalf("expected exact role: %v", err)
	}
	role.Object["rules"].([]any)[2].(map[string]any)["verbs"] = []any{"create", "get"}
	if _, err := client.Resource(tenantClusterRoleGVR).Update(context.Background(), role, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := access.verifyTenantClusterRole(context.Background()); !errors.Is(err, biz.ErrOwnership) {
		t.Fatalf("escalated role accepted: %v", err)
	}
	if err := client.Resource(tenantClusterRoleGVR).Delete(context.Background(), role.GetName(), metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := access.verifyTenantClusterRole(context.Background()); !errors.Is(err, biz.ErrDependencyUnavailable) {
		t.Fatalf("missing role accepted: %v", err)
	}
}
