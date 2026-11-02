package data

import (
	"context"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/tenantnamespace"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// ConfigureTenantNamespaceResolver supplies only the external Kubernetes read
// boundary. Acceptance and the identity transaction remain production code.
func (p *Postgres) ConfigureTenantNamespaceResolver(resolve func(context.Context, string) (tenantnamespace.Identity, error)) {
	p.namespaceResolver = resolve
}
func (p *Postgres) ResolveTenantNamespace(ctx context.Context, tenant string) (tenantnamespace.Identity, error) {
	if p.namespaceResolver == nil {
		return tenantnamespace.Identity{}, biz.Fail(biz.DependencyUnavailable, "tenant namespace reader unavailable")
	}
	identity, err := p.namespaceResolver(ctx, tenant)
	if err != nil {
		return tenantnamespace.Identity{}, err
	}
	name, err := tenantnamespace.Name(tenant)
	if err != nil || identity.TenantID != tenant || identity.ClusterID != p.placement.ClusterID || identity.Name != name || identity.UID == "" {
		return tenantnamespace.Identity{}, biz.Fail(biz.PlacementMismatch, "invalid tenant namespace identity")
	}
	return identity, nil
}
func (p *Postgres) acceptTenantNamespace(ctx context.Context, q *sqlcgen.Queries, identity tenantnamespace.Identity) error {
	name, err := tenantnamespace.Name(identity.TenantID)
	if err != nil || identity.ClusterID != p.placement.ClusterID || identity.Name != name || identity.UID == "" {
		return biz.Fail(biz.PlacementMismatch, "tenant namespace identity required")
	}
	if err := q.EnsureTenantNamespace(ctx, sqlcgen.EnsureTenantNamespaceParams{TenantID: identity.TenantID, ClusterID: identity.ClusterID, Namespace: identity.Name, NamespaceUid: &identity.UID}); err != nil {
		return databaseFailure(err)
	}
	row, err := q.GetTenantNamespaceIdentity(ctx, sqlcgen.GetTenantNamespaceIdentityParams{TenantID: identity.TenantID, ClusterID: identity.ClusterID})
	if err != nil {
		return databaseFailure(err)
	}
	if row.Namespace != identity.Name || row.NamespaceUid == nil || *row.NamespaceUid != identity.UID {
		return biz.Fail(biz.PlacementMismatch, "persisted tenant namespace identity differs")
	}
	return nil
}
func namespaceObject(value *unstructured.Unstructured) tenantnamespace.Object {
	if value == nil {
		return tenantnamespace.Object{}
	}
	phase, _, _ := unstructured.NestedString(value.Object, "status", "phase")
	return tenantnamespace.Object{Name: value.GetName(), UID: string(value.GetUID()), Phase: phase, Deleting: value.GetDeletionTimestamp() != nil, Labels: value.GetLabels()}
}
func (p *KCProvider) resolveTenantNamespace(ctx context.Context, tenant string) (tenantnamespace.Identity, error) {
	name, err := tenantnamespace.Name(tenant)
	if err != nil {
		return tenantnamespace.Identity{}, biz.Fail(biz.TenantRequired, err.Error())
	}
	value, err := p.client.Resource(namespaces).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return tenantnamespace.Identity{}, readFailure(err)
	}
	if err := tenantnamespace.Validate(tenant, namespaceObject(value), "", true); err != nil {
		return tenantnamespace.Identity{}, biz.Fail(biz.PlacementMismatch, err.Error())
	}
	return tenantnamespace.Identity{TenantID: tenant, ClusterID: p.repository.placement.ClusterID, Name: name, UID: string(value.GetUID())}, nil
}

// The persisted original UID is authoritative for every provider operation.
// Missing Namespace is an identity error, never provider resource absence.
func (p *KCProvider) verifyTenantNamespace(ctx context.Context, binding sqlcgen.NetworkProviderBinding, requireActive ...bool) error {
	if p.namespaceIdentityReader == nil {
		return &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	identity, err := p.namespaceIdentityReader(ctx, binding.TenantID)
	if err != nil {
		return &biz.ProviderError{Kind: biz.ProviderConflict, Cause: err}
	}
	if identity.UID == "" || identity.Name != binding.Namespace || identity.TenantID != binding.TenantID || (binding.ClusterID != "" && identity.ClusterID != binding.ClusterID) {
		return &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	value, err := p.client.Resource(namespaces).Get(ctx, binding.Namespace, metav1.GetOptions{})
	if err != nil {
		return &biz.ProviderError{Kind: biz.ProviderConflict, Cause: err}
	}
	active := len(requireActive) > 0 && requireActive[0]
	if err := tenantnamespace.Validate(binding.TenantID, namespaceObject(value), identity.UID, active); err != nil {
		return &biz.ProviderError{Kind: biz.ProviderConflict, Cause: err}
	}
	return nil
}

func (p *Postgres) tenantNamespaceIdentity(ctx context.Context, tenant string) (tenantnamespace.Identity, error) {
	row, err := p.queries.GetTenantNamespaceIdentity(ctx, sqlcgen.GetTenantNamespaceIdentityParams{TenantID: tenant, ClusterID: p.placement.ClusterID})
	if err != nil {
		return tenantnamespace.Identity{}, err
	}
	if row.NamespaceUid == nil || *row.NamespaceUid == "" {
		return tenantnamespace.Identity{}, biz.Fail(biz.PlacementMismatch, "tenant namespace UID is NULL")
	}
	name, err := tenantnamespace.Name(tenant)
	if err != nil || name != row.Namespace {
		return tenantnamespace.Identity{}, biz.Fail(biz.PlacementMismatch, "tenant namespace name differs")
	}
	return tenantnamespace.Identity{TenantID: tenant, ClusterID: row.ClusterID, Name: row.Namespace, UID: *row.NamespaceUid}, nil
}
