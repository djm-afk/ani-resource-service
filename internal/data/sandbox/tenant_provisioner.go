package sandbox

import (
	"context"
	"fmt"
	"strings"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/sandbox"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// TenantSpaceProvisioner uses the shared client and existing Resource process.
type TenantSpaceProvisioner struct {
	Namespace TenantNamespace
	Access    TenantAccess
}

func (p TenantSpaceProvisioner) ProvisionTenantSpace(ctx context.Context, tenant, operation string, journal biz.TenantSpaceJournal) (string, error) {
	if p.Access.ServiceAccountNamespace == "" || p.Access.ServiceAccountName == "" {
		namespace, name, err := currentServiceAccount(ctx, p.Namespace.Client)
		if err != nil {
			return "", err
		}
		p.Access.ServiceAccountNamespace, p.Access.ServiceAccountName = namespace, name
	}
	uid, err := p.Namespace.ProvisionJournaled(ctx, tenant, operation, journal)
	if err != nil {
		return "", err
	}
	if err = p.Access.EnsureJournaled(ctx, tenant, operation, uid, journal); err != nil {
		return "", err
	}
	return uid, nil
}

// Query Kubernetes for the identity of this exact client before any write.
// Configured credentials that are not a ServiceAccount cannot be bound.
func currentServiceAccount(ctx context.Context, client dynamic.Interface) (string, string, error) {
	if client == nil {
		return "", "", biz.ErrDependencyUnavailable
	}
	resource := schema.GroupVersionResource{Group: "authentication.k8s.io", Version: "v1", Resource: "selfsubjectreviews"}
	result, err := client.Resource(resource).Create(ctx, &unstructured.Unstructured{Object: map[string]any{"apiVersion": "authentication.k8s.io/v1", "kind": "SelfSubjectReview"}}, metav1.CreateOptions{})
	if err != nil {
		return "", "", fmt.Errorf("%w: tenant provisioner identity unavailable", biz.ErrDependencyUnavailable)
	}
	if result == nil {
		return "", "", biz.ErrDependencyUnavailable
	}
	username, ok, err := unstructured.NestedString(result.Object, "status", "userInfo", "username")
	if err != nil || !ok {
		return "", "", biz.ErrDependencyUnavailable
	}
	parts := strings.Split(username, ":")
	if len(parts) != 4 || parts[0] != "system" || parts[1] != "serviceaccount" || parts[2] == "" || parts[3] == "" {
		return "", "", biz.ErrDependencyUnavailable
	}
	return parts[2], parts[3], nil
}
