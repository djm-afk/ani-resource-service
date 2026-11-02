package sandbox

import (
	"context"
	"fmt"
	"reflect"

	"github.com/google/uuid"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/sandbox"
	storage "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/storage"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

const tenantAccessNamespaceUID = "sandbox.ani.io/namespace-uid"

var (
	tenantRoleGVR        = schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}
	tenantBindingGVR     = schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"}
	tenantClusterRoleGVR = schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}
)

// TenantAccess provisions only the Sandbox permissions in an already verified
// shared tenant namespace. Its Kubernetes identity must also be constrained by
// an admission policy; RBAC create/bind alone cannot constrain subjects.
type TenantAccess struct {
	Client                  dynamic.Interface
	NamespacePrefix         string
	ServiceAccountNamespace string
	ServiceAccountName      string
	journal                 biz.TenantSpaceJournal
	operation               string
}

// EnsureJournaled records each provider write before dispatching it. A lost
// response remains pending until a trustworthy receipt can resolve it.
func (a TenantAccess) EnsureJournaled(ctx context.Context, tenant, operation, namespaceUID string, journal biz.TenantSpaceJournal) error {
	if journal == nil || !biz.UUID(operation) || namespaceUID == "" {
		return biz.ErrInvalid
	}
	uid, err := (TenantNamespace{Client: a.Client, NamespacePrefix: a.NamespacePrefix}).ResolveSandboxNamespaceUID(ctx, tenant)
	if err != nil {
		return err
	}
	if uid != namespaceUID {
		return fmt.Errorf("%w: tenant namespace changed", biz.ErrOwnership)
	}
	if err = a.verifyTenantClusterRole(ctx); err != nil {
		return err
	}
	if _, err = journal.PinTenantSpaceNamespace(ctx, tenant, operation, uid); err != nil {
		return err
	}
	a.journal, a.operation = journal, operation
	return a.ensure(ctx, tenant, uid)
}

// A RoleBinding may reference a missing or changed ClusterRole. Check the
// actual authority before reporting a tenant ready or creating any binding.
func (a TenantAccess) verifyTenantClusterRole(ctx context.Context) error {
	role, err := a.Client.Resource(tenantClusterRoleGVR).Get(ctx, "ani-sandbox-tenant", metav1.GetOptions{})
	if err != nil || role == nil {
		return fmt.Errorf("%w: sandbox tenant cluster role unavailable", biz.ErrDependencyUnavailable)
	}
	if role.GetName() != "ani-sandbox-tenant" || role.GetUID() == "" || role.GetDeletionTimestamp() != nil {
		return fmt.Errorf("%w: sandbox tenant cluster role invalid", biz.ErrOwnership)
	}
	want := []any{
		map[string]any{"apiGroups": []any{""}, "resources": []any{"pods", "persistentvolumeclaims"}, "verbs": []any{"get", "create", "delete"}},
		map[string]any{"apiGroups": []any{""}, "resources": []any{"pods/exec"}, "verbs": []any{"create"}},
		map[string]any{"apiGroups": []any{""}, "resources": []any{"secrets"}, "verbs": []any{"create"}},
	}
	if !reflect.DeepEqual(role.Object["rules"], want) || role.Object["aggregationRule"] != nil {
		return fmt.Errorf("%w: sandbox tenant cluster role differs", biz.ErrOwnership)
	}
	return nil
}

func (a TenantAccess) beginWrite(ctx context.Context, tenant, kind string) (string, error) {
	if a.journal == nil {
		return "", nil
	}
	id := uuid.NewString()
	m, err := a.journal.BeginTenantSpaceMutation(ctx, tenant, a.operation, id, kind)
	if err != nil {
		return "", err
	}
	if !m.Dispatch {
		return "", biz.ErrConflict
	}
	return id, nil
}

func (a TenantAccess) finishWrite(ctx context.Context, tenant, id, receipt string) error {
	if id == "" {
		return nil
	}
	if receipt == "" {
		return fmt.Errorf("%w: provider returned no UID", biz.ErrDependencyUnavailable)
	}
	_, err := a.journal.FinishTenantSpaceMutation(ctx, tenant, a.operation, id, "applied", receipt)
	return err
}

// ensure is idempotent, but never adopts or rewrites an existing different
// role or binding. The namespace UID is included in each object as a receipt.
// An admission policy must additionally fence late writes to a replacement
// namespace; API-server CEL does not expose namespaceObject.metadata.uid.
func (a TenantAccess) ensure(ctx context.Context, tenant, namespaceUID string) error {
	if a.Client == nil || !biz.UUID(tenant) || namespaceUID == "" || a.ServiceAccountNamespace == "" || a.ServiceAccountName == "" {
		return biz.ErrInvalid
	}
	name, err := storage.NamespaceName(a.NamespacePrefix, tenant)
	if err != nil {
		return biz.ErrInvalid
	}
	verifyNamespace := func() error {
		uid, err := (TenantNamespace{Client: a.Client, NamespacePrefix: a.NamespacePrefix}).ResolveSandboxNamespaceUID(ctx, tenant)
		if err != nil {
			return err
		}
		if uid != namespaceUID {
			return fmt.Errorf("%w: tenant namespace changed", biz.ErrOwnership)
		}
		namespace, err := a.Client.Resource(namespaceGVR).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if string(namespace.GetUID()) != namespaceUID || namespace.GetDeletionTimestamp() != nil || namespace.GetLabels()[storage.NamespaceManagedBy] != storage.NamespaceOwner || namespace.GetLabels()[storage.NamespaceTenantID] != tenant || namespace.GetAnnotations()[tenantAccessNamespaceUID] != namespaceUID {
			return fmt.Errorf("%w: tenant namespace access marker differs", biz.ErrOwnership)
		}
		return nil
	}
	// The admission policy cannot read namespaceObject.metadata.uid directly.
	// It compares this immutable marker with the annotation on each RBAC object.
	// Stamping requires a separately constrained namespace-update permission.
	if err := a.ensureNamespaceMarker(ctx, tenant, name, namespaceUID); err != nil {
		return err
	}
	role := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "Role",
		"metadata": map[string]any{"name": "ani-sandbox-pull-secret", "namespace": name, "annotations": map[string]any{tenantAccessNamespaceUID: namespaceUID}},
		"rules":    []any{map[string]any{"apiGroups": []any{""}, "resources": []any{"secrets"}, "resourceNames": []any{"ani-sandbox-pull-" + tenant}, "verbs": []any{"get", "update"}}},
	}}
	binding := func(bindingName, kind, roleName string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding",
			"metadata": map[string]any{"name": bindingName, "namespace": name, "annotations": map[string]any{tenantAccessNamespaceUID: namespaceUID}},
			"roleRef":  map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": kind, "name": roleName},
			"subjects": []any{map[string]any{"kind": "ServiceAccount", "name": a.ServiceAccountName, "namespace": a.ServiceAccountNamespace}},
		}}
	}
	objects := []struct {
		gvr    schema.GroupVersionResource
		obj    *unstructured.Unstructured
		fields []string
		kind   string
	}{
		{tenantRoleGVR, role, []string{"rules"}, "pull_secret_role"},
		{tenantBindingGVR, binding("ani-sandbox-pull-secret", "Role", "ani-sandbox-pull-secret"), []string{"roleRef", "subjects"}, "pull_secret_binding"},
		{tenantBindingGVR, binding("ani-sandbox-tenant", "ClusterRole", "ani-sandbox-tenant"), []string{"roleRef", "subjects"}, "tenant_role_binding"},
	}
	for _, item := range objects {
		if err := verifyNamespace(); err != nil {
			return err
		}
		resource := a.Client.Resource(item.gvr).Namespace(name)
		actual, err := resource.Get(ctx, item.obj.GetName(), metav1.GetOptions{})
		var mutation string
		if apierrors.IsNotFound(err) {
			mutation, err = a.beginWrite(ctx, tenant, item.kind)
			if err != nil {
				return err
			}
			actual, err = resource.Create(ctx, item.obj, metav1.CreateOptions{FieldManager: "ani-sandbox-tenant-access"})
			if err != nil && ctx.Err() == nil && a.journal == nil {
				// Create can have committed even when its response was lost.
				actual, err = resource.Get(ctx, item.obj.GetName(), metav1.GetOptions{})
			}
		}
		if err != nil {
			return fmt.Errorf("tenant access %s: %w", item.obj.GetName(), err)
		}
		if actual == nil || actual.GetName() != item.obj.GetName() || actual.GetNamespace() != name || actual.GetDeletionTimestamp() != nil || actual.GetAnnotations()[tenantAccessNamespaceUID] != namespaceUID {
			return fmt.Errorf("%w: tenant access %s", biz.ErrOwnership, item.obj.GetName())
		}
		for _, field := range item.fields {
			if !reflect.DeepEqual(actual.Object[field], item.obj.Object[field]) {
				return fmt.Errorf("%w: tenant access %s differs", biz.ErrOwnership, item.obj.GetName())
			}
		}
		if a.journal != nil && actual.GetUID() == "" {
			return fmt.Errorf("%w: tenant access has no UID", biz.ErrDependencyUnavailable)
		}
		if err = a.finishWrite(ctx, tenant, mutation, string(actual.GetUID())); err != nil {
			return err
		}
	}
	if a.journal != nil {
		if err := a.verifyTenantClusterRole(ctx); err != nil {
			return err
		}
	}
	return verifyNamespace()
}

func (a TenantAccess) ensureNamespaceMarker(ctx context.Context, tenant, name, namespaceUID string) error {
	resource := a.Client.Resource(namespaceGVR)
	namespace, err := resource.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	check := func(ns *unstructured.Unstructured, markerRequired bool) error {
		if ns == nil || ns.GetName() != name || string(ns.GetUID()) != namespaceUID || ns.GetDeletionTimestamp() != nil || ns.GetLabels()[storage.NamespaceManagedBy] != storage.NamespaceOwner || ns.GetLabels()[storage.NamespaceTenantID] != tenant {
			return fmt.Errorf("%w: tenant namespace changed", biz.ErrOwnership)
		}
		marker := ns.GetAnnotations()[tenantAccessNamespaceUID]
		if (markerRequired && marker != namespaceUID) || (marker != "" && marker != namespaceUID) {
			return fmt.Errorf("%w: tenant namespace access marker differs", biz.ErrOwnership)
		}
		return nil
	}
	if err := check(namespace, false); err != nil {
		return err
	}
	if namespace.GetAnnotations()[tenantAccessNamespaceUID] == namespaceUID {
		return nil
	}
	annotations := namespace.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}
	annotations[tenantAccessNamespaceUID] = namespaceUID
	namespace.SetAnnotations(annotations)
	mutation, err := a.beginWrite(ctx, tenant, "namespace_marker")
	if err != nil {
		return err
	}
	actual, err := resource.Update(ctx, namespace, metav1.UpdateOptions{FieldManager: "ani-sandbox-tenant-access"})
	if err != nil && ctx.Err() == nil && a.journal == nil {
		actual, err = resource.Get(ctx, name, metav1.GetOptions{})
	}
	if err != nil {
		return fmt.Errorf("tenant namespace marker: %w", err)
	}
	if err := check(actual, true); err != nil {
		return err
	}
	return a.finishWrite(ctx, tenant, mutation, string(actual.GetUID()))
}
