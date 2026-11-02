package sandbox

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/sandbox"
	storage "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/storage"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/tenantnamespace"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
)

// TenantNamespace uses the shared Network namespace ownership convention.
// Tenant provisioning may create it with a journal; ordinary resolution is read-only.
type TenantNamespace struct {
	Client          dynamic.Interface
	NamespacePrefix string
}

// ProvisionJournaled creates the shared namespace only after recording the
// provider write. An uncertain create is deliberately never sent twice.
func (n TenantNamespace) ProvisionJournaled(ctx context.Context, tenant, operation string, journal biz.TenantSpaceJournal) (string, error) {
	if n.Client == nil || journal == nil || !biz.UUID(tenant) || !biz.UUID(operation) {
		return "", biz.ErrInvalid
	}
	name, err := tenantnamespace.Name(tenant)
	if err != nil || n.NamespacePrefix != tenantnamespace.Prefix {
		return "", biz.ErrInvalid
	}
	resource := n.Client.Resource(namespaceGVR)
	namespace, err := resource.Get(ctx, name, metav1.GetOptions{})
	mutation := ""
	if apierrors.IsNotFound(err) {
		mutation = uuid.NewString()
		started, beginErr := journal.BeginTenantSpaceMutation(ctx, tenant, operation, mutation, "namespace_create")
		if beginErr != nil {
			return "", beginErr
		}
		if !started.Dispatch {
			return "", biz.ErrConflict
		}
		namespace, err = resource.Create(ctx, &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1", "kind": "Namespace",
			"metadata": map[string]any{"name": name, "labels": map[string]any{
				storage.NamespaceManagedBy: storage.NamespaceOwner, storage.NamespaceTenantID: tenant,
			}},
		}}, metav1.CreateOptions{FieldManager: "ani-network-service"})
		if apierrors.IsAlreadyExists(err) {
			if _, finishErr := journal.FinishTenantSpaceMutation(ctx, tenant, operation, mutation, "not_sent", "already_exists"); finishErr != nil {
				return "", finishErr
			}
			mutation = ""
			namespace, err = resource.Get(ctx, name, metav1.GetOptions{})
		}
	}
	if err != nil {
		return "", fmt.Errorf("tenant namespace provision: %w", err)
	}
	if namespace == nil || namespace.GetDeletionTimestamp() != nil || tenantnamespace.Validate(tenant, tenantnamespace.Object{Name: namespace.GetName(), UID: string(namespace.GetUID()), Labels: namespace.GetLabels()}, "", false) != nil {
		return "", fmt.Errorf("%w: tenant namespace is not usable", biz.ErrOwnership)
	}
	uid := string(namespace.GetUID())
	if mutation != "" {
		if _, err = journal.FinishTenantSpaceMutation(ctx, tenant, operation, mutation, "applied", uid); err != nil {
			return "", err
		}
	} else {
		if _, err = journal.PinTenantSpaceNamespace(ctx, tenant, operation, uid); err != nil {
			return "", err
		}
	}
	phase, _, _ := unstructured.NestedString(namespace.Object, "status", "phase")
	if phase != "Active" {
		return "", fmt.Errorf("%w: tenant namespace is not active", biz.ErrDependencyUnavailable)
	}
	return uid, nil
}

func (n TenantNamespace) ResolveSandboxNamespaceUID(ctx context.Context, tenant string) (string, error) {
	if n.Client == nil || !biz.UUID(tenant) {
		return "", biz.ErrDependencyUnavailable
	}
	name, err := storage.NamespaceName(n.NamespacePrefix, tenant)
	if err != nil {
		return "", biz.ErrDependencyUnavailable
	}
	resource := n.Client.Resource(namespaceGVR)
	namespace, err := resource.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if errors.Is(err, context.Canceled) {
			return "", context.Canceled
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return "", context.DeadlineExceeded
		}
		return "", biz.ErrDependencyUnavailable
	}
	if namespace == nil {
		return "", biz.ErrDependencyUnavailable
	}
	phase, _, _ := unstructured.NestedString(namespace.Object, "status", "phase")
	uid := string(namespace.GetUID())
	if err := tenantnamespace.Validate(tenant, tenantnamespace.Object{Name: namespace.GetName(), UID: uid, Phase: phase, Labels: namespace.GetLabels(), Deleting: namespace.GetDeletionTimestamp() != nil}, "", true); err != nil {
		return "", fmt.Errorf("%w: %v", biz.ErrOwnership, err)
	}

	return uid, nil
}

var _ biz.NamespaceResolver = TenantNamespace{}
