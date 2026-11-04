package data

import (
	"context"
	"encoding/json"
	"errors"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

var kcBasicNetworkIsolations = schema.GroupVersionResource{Group: "networking.kubercloud.com", Version: "v1", Resource: "basicnetworkisolations"}

func (p *Postgres) EnsureTenantBasicIsolation(ctx context.Context, tenant string) (string, error) {
	if p.networkProvider == nil {
		return "", biz.Fail(biz.DependencyUnavailable, "network provider unavailable")
	}
	name, err := p.networkProvider.EnsureTenantBasicIsolation(ctx, tenant)
	var provider *biz.ProviderError
	if errors.As(err, &provider) {
		reason := biz.DependencyUnavailable
		if provider.Kind == biz.ProviderConflict {
			reason = biz.ProviderOwnership
		}
		return name, &biz.Error{Reason: reason, Message: "tenant basic isolation declaration failed", Cause: err}
	}
	return name, err
}

// EnsureTenantBasicIsolation owns only this single declaration and original
// UID. KCN is responsible for realizing it; no Ready or member-list gate exists.
func (p *KCProvider) EnsureTenantBasicIsolation(ctx context.Context, tenant string) (string, error) {
	identity, err := p.repository.tenantNamespaceIdentity(ctx, tenant)
	if err != nil {
		return "", err
	}
	name := "ani-tenant-" + tenant
	q := p.repository.queries
	changed, err := q.ReserveTenantBasicIsolation(ctx, sqlcgen.ReserveTenantBasicIsolationParams{TenantID: tenant, ClusterID: identity.ClusterID, NamespaceUid: identity.UID, Name: name})
	if err != nil {
		return "", databaseFailure(err)
	}
	if changed != 1 {
		return "", &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	saved, err := q.GetTenantBasicIsolation(ctx, sqlcgen.GetTenantBasicIsolationParams{TenantID: tenant, ClusterID: identity.ClusterID})
	if err != nil {
		return "", databaseFailure(err)
	}
	r := p.client.Resource(kcBasicNetworkIsolations)
	object, err := r.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if saved.BasicIsolationUid != "" {
			return "", &biz.ProviderError{Kind: biz.ProviderConflict, Cause: err}
		}
		object = &unstructured.Unstructured{Object: map[string]any{"apiVersion": "networking.kubercloud.com/v1", "kind": "BasicNetworkIsolation", "metadata": map[string]any{"name": name, "labels": map[string]any{ownerLabel: "ani-network-service", tenantLabel: tenant}}, "spec": map[string]any{"enabled": true}}}
		object, err = r.Create(ctx, object, metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			object, err = r.Get(ctx, name, metav1.GetOptions{})
		}
		if err != nil {
			return "", mutationFailure(err)
		}
	} else if err != nil {
		return "", readFailure(err)
	}
	labels := object.GetLabels()
	if object.GetUID() == "" || object.GetName() != name || object.GetNamespace() != "" || object.GetKind() != "BasicNetworkIsolation" || object.GetAPIVersion() != "networking.kubercloud.com/v1" || object.GetDeletionTimestamp() != nil || labels[tenantLabel] != tenant || labels[ownerLabel] != "ani-network-service" || saved.BasicIsolationUid != "" && saved.BasicIsolationUid != string(object.GetUID()) {
		return "", &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	changed, err = q.SaveTenantBasicIsolationUID(ctx, sqlcgen.SaveTenantBasicIsolationUIDParams{TenantID: tenant, ClusterID: identity.ClusterID, Name: name, Uid: string(object.GetUID())})
	if err != nil {
		return "", databaseFailure(err)
	}
	if changed != 1 {
		return "", &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	enabled, _, _ := unstructured.NestedBool(object.Object, "spec", "enabled")
	if !enabled {
		patch, _ := json.Marshal([]map[string]any{{"op": "test", "path": "/metadata/uid", "value": string(object.GetUID())}, {"op": "test", "path": "/metadata/resourceVersion", "value": object.GetResourceVersion()}, {"op": "add", "path": "/spec/enabled", "value": true}})
		if _, err = r.Patch(ctx, name, types.JSONPatchType, patch, metav1.PatchOptions{}); err != nil {
			return "", mutationFailure(err)
		}
	}
	return name, nil
}
