package sandbox

import (
	"context"
	"errors"
	"fmt"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/sandbox"
	storage "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/storage"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/tenantnamespace"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
)

// TenantNamespace uses the shared Network namespace ownership convention.
// It only reads existing objects; acceptance and workers freeze the original UID.
type TenantNamespace struct {
	Client          dynamic.Interface
	NamespacePrefix string
}

func (n TenantNamespace) ResolveSandboxNamespaceUID(ctx context.Context, tenant string) (string, error) {
	return n.namespaceUID(ctx, tenant)
}

func (n TenantNamespace) namespaceUID(ctx context.Context, tenant string) (string, error) {
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
