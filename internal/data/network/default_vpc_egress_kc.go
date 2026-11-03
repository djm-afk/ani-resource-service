package data

import (
	"context"

	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// This adapter only uses the installed KCN v0.9 protocol. It never patches the
// default VPC, native resources, node labels, daemon state or natOutgoing.
type DefaultEgressKCProvider struct {
	provider *KCProvider
	identity biz.DefaultVPCIdentity
}

func NewDefaultEgressKCProvider(p *KCProvider, i biz.DefaultVPCIdentity) (*DefaultEgressKCProvider, error) {
	if err := i.Validate(); err != nil {
		return nil, err
	}
	if p == nil || p.repository == nil || p.repository.placement.ClusterID != i.ClusterID {
		return nil, biz.Fail(biz.InvalidArgument, "matching fixed cluster identity and KC provider are required")
	}
	return &DefaultEgressKCProvider{p, i}, nil
}
func (p *DefaultEgressKCProvider) identityObject(ctx context.Context, i biz.DefaultVPCIdentity) (*unstructured.Unstructured, error) {
	if i != p.identity {
		return nil, &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	cluster, err := p.provider.client.Resource(namespaces).Get(ctx, "kube-system", metav1.GetOptions{})
	if err != nil {
		return nil, readFailure(err)
	}
	vpc, err := p.provider.client.Resource(kcVPCs).Namespace(i.Namespace).Get(ctx, i.Name, metav1.GetOptions{})
	if err != nil {
		return nil, readFailure(err)
	}
	if cluster.GetAPIVersion() != "v1" || cluster.GetKind() != "Namespace" || cluster.GetNamespace() != "" || string(cluster.GetUID()) != i.ClusterUID || cluster.GetName() != "kube-system" || cluster.GetResourceVersion() == "" || cluster.GetDeletionTimestamp() != nil || vpc.GetAPIVersion() != "networking.kubercloud.com/v1" || vpc.GetKind() != "VPC" || vpc.GetNamespace() != i.Namespace || vpc.GetName() != i.Name || string(vpc.GetUID()) != i.UID || vpc.GetResourceVersion() == "" || vpc.GetDeletionTimestamp() != nil || hasField(vpc, "spec", "type") {
		return nil, &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	return vpc, nil
}
func (p *DefaultEgressKCProvider) poolSet(ctx context.Context, poolID string, cleanup, critical bool) (egressResolved, egressReadSet, bool, error) {
	b, err := p.provider.platformBinding(ctx, "public_pool", poolID)
	if err != nil {
		return egressResolved{}, egressReadSet{}, false, err
	}
	r, err := p.provider.resolveEgress(ctx, biz.ProviderTarget{Kind: "public_pool", ResourceID: poolID, BindingID: b.BindingID, KnownIdentity: b.ProviderUid, Direct: critical, Deleting: cleanup})
	if err != nil {
		return r, egressReadSet{}, false, err
	}
	if r.pool.Scope != "public" || r.pool.Retired || b.ProviderUid == "" {
		return r, egressReadSet{}, false, &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	if cleanup {
		r.deviceID = ""
		r.virtualSwitch = nil
	} // Cleanup does not require healthy hardware.
	set, err := p.provider.readEgressSet(ctx, r, critical)
	if err != nil {
		return r, set, false, err
	}
	o, err := inspectEgressSet(r, set)
	if err != nil {
		return r, set, false, err
	}
	// Reuse the joint control-plane, complete device wiring and deployment proof.
	ready := o.Exists && o.Ready && (!set.deviceRequired || set.deviceReady)
	return r, set, ready, nil
}
func (p *DefaultEgressKCProvider) ValidateDefaultVPCEgress(ctx context.Context, i biz.DefaultVPCIdentity, poolID string) (biz.DefaultEgressPoolProof, error) {
	vpc, err := p.identityObject(ctx, i)
	if err != nil {
		return biz.DefaultEgressPoolProof{}, err
	}
	r, set, ready, err := p.poolSet(ctx, poolID, false, true)
	if err != nil {
		return biz.DefaultEgressPoolProof{}, err
	}
	c := r.pool
	c.DefaultVpcName = p.identity.Name
	if !ready || len(providerImages(set.all[pods])) == 0 || !controllerDefaultGateway(c, set.all[pods]) || !goodConditions(vpc, "Valid", "Initialized", "Ready") || crInt(vpc, "status", "observedGeneration") != vpc.GetGeneration() {
		return biz.DefaultEgressPoolProof{}, &biz.ProviderError{Kind: biz.ProviderTemporary}
	}
	return biz.DefaultEgressPoolProof{Revision: r.pool.ConfigRevision, ProviderUID: r.binding.ProviderUid, Hash: contentHash([]string{set.proof.Hash, i.ClusterUID, i.UID}), ObservedAt: set.proof.CollectedAt}, nil
}
func defaultEgressBinding(x biz.DefaultEgressWork, kind string) sqlcgen.NetworkProviderBinding {
	b := sqlcgen.NetworkProviderBinding{ResourceKind: kind, ClusterID: x.Egress.Identity.ClusterID, Namespace: x.Egress.Identity.Namespace}
	if kind == "eip" {
		b.ProviderName, b.ProviderUid, b.BindingID = x.Egress.EIPProviderName, x.Egress.EIPProviderUID, x.EIPBindingID
	} else {
		b.ProviderName, b.ProviderUid, b.BindingID = x.Egress.SnatProviderName, x.Egress.SnatProviderUID, x.SnatBindingID
	}
	return b
}
func (p *DefaultEgressKCProvider) readOwned(ctx context.Context, x biz.DefaultEgressWork, kind string, spec map[string]any, cleanup bool) (*unstructured.Unstructured, error) {
	b := defaultEgressBinding(x, kind)
	o, err := resourceEndpoint(p.provider.client, b).Get(ctx, b.ProviderName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, readFailure(err)
	}
	if err = inspectEgressIdentity(o, b, x.Egress.ID, b.ProviderUid); err != nil {
		return nil, err
	}
	if !matchesEgressSpec(o, spec, kind) || (!cleanup && kind == "snat" && crBool(o, "spec", "disable")) {
		return nil, &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	if b.ProviderUid == "" && x.Egress.PendingAction != "create_"+kind {
		return nil, &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	return o, nil
}
func defaultEgressEIPSpec(pool string) map[string]any {
	return map[string]any{"subnet": kcSystemNamespace + "/" + pool, "ipVersion": "IPv4"}
}
func defaultEgressSnatSpec(e biz.DefaultVPCEgress) map[string]any {
	return map[string]any{"eip": e.EIPProviderName, "vpc": e.Identity.Name, "disable": false}
}
func (p *DefaultEgressKCProvider) observe(ctx context.Context, x biz.DefaultEgressWork, cleanup bool) (biz.DefaultEgressObservation, *unstructured.Unstructured, *unstructured.Unstructured, error) {
	var o biz.DefaultEgressObservation
	vpc, err := p.identityObject(ctx, x.Egress.Identity)
	if err != nil {
		return o, nil, nil, err
	}
	r, set, ready, err := p.poolSet(ctx, x.Egress.PoolID, cleanup, cleanup || x.Egress.State == biz.Provisioning || x.Egress.PendingAction != "")
	if err != nil {
		return o, nil, nil, err
	}
	if r.pool.ConfigRevision != x.PoolRevision || r.binding.ProviderUid != x.PoolProviderUID {
		return o, nil, nil, &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	eip, err := p.readOwned(ctx, x, "eip", defaultEgressEIPSpec(r.binding.ProviderName), cleanup)
	if err != nil {
		return o, nil, nil, err
	}
	snat, err := p.readOwned(ctx, x, "snat", defaultEgressSnatSpec(x.Egress), cleanup)
	if err != nil {
		return o, nil, nil, err
	}
	// Aggregate bindings are independent of tenant tables and therefore read the
	// complete external binding relation, including native SNAT/NAT/LB claimants.
	for _, resource := range []schema.GroupVersionResource{kcSnats, kcNats, kcServices} {
		if set.view != nil {
			set.all[resource] = indexedObjects(set.view.indices[resource], []string{"eip:" + x.Egress.Identity.Namespace + "/" + x.Egress.EIPProviderName})
		} else {
			items, err := p.provider.listAll(ctx, resource)
			if err != nil {
				return o, nil, nil, err
			}
			set.all[resource] = items
		}
	}
	conflict := eipBindingConflict(set, x.Egress.Identity.Namespace, x.Egress.EIPProviderName, x.Egress.SnatProviderName)
	if conflict {
		return o, nil, nil, &biz.ProviderError{Kind: biz.ProviderInUse}
	}
	o.ObservedAt = set.proof.CollectedAt
	if o.ObservedAt.IsZero() {
		return o, nil, nil, &biz.ProviderError{Kind: biz.ProviderTemporary}
	}
	o.EIPExists, o.SnatExists = eip != nil, snat != nil
	if eip != nil {
		o.EIPUID = string(eip.GetUID())
		o.EIPReady, o.Address = allocatedEIP(r, eip, set)
		o.EIPReady = o.EIPReady && ready
		if x.Egress.Address != "" && o.Address != x.Egress.Address {
			return o, nil, nil, &biz.ProviderError{Kind: biz.ProviderConflict}
		}
	}
	if snat != nil {
		o.SnatUID = string(snat.GetUID())
		o.TargetGeneration = snat.GetGeneration()
	}
	o.Unbound = snat == nil && (eip == nil || emptyBoundResource(eip))
	o.Ready = o.EIPReady && snat != nil && bindingApplied(eip, snat, vpc, true)
	if o.Ready {
		enabled := true
		o.AppliedEnabled = &enabled
	}
	o.Hash = contentHash(struct {
		Identity  biz.DefaultVPCIdentity
		PoolProof string
		EIP, Snat *unstructured.Unstructured
	}{x.Egress.Identity, set.proof.Hash, eip, snat})
	return o, eip, snat, nil
}
func (p *DefaultEgressKCProvider) ObserveDefaultVPCEgress(ctx context.Context, x biz.DefaultEgressWork) (biz.DefaultEgressObservation, error) {
	o, _, _, err := p.observe(ctx, x, x.Egress.State == biz.Deleting)
	return o, err
}
func (p *DefaultEgressKCProvider) assertMutationLease(ctx context.Context, x biz.DefaultEgressWork) error {
	ok, err := p.provider.repository.queries.AssertDefaultEgressLease(ctx, sqlcgen.AssertDefaultEgressLeaseParams{ClusterID: x.Egress.Identity.ClusterID, ResourceID: x.Egress.ID, Version: x.Egress.Version, OperationID: x.Egress.LastOperationID, Owner: x.Owner, Epoch: x.Epoch})
	if err != nil {
		return readFailure(err)
	}
	if !ok {
		return biz.ErrLeaseLost
	}
	return nil
}
func (p *DefaultEgressKCProvider) MutateDefaultVPCEgress(ctx context.Context, x biz.DefaultEgressWork, action string) error {
	// The repository persisted this marker before dispatch. Reflect it locally
	// for the exact preflight; no owner or provider identity is invented.
	x.Egress.PendingAction = action
	o, eip, snat, err := p.observe(ctx, x, action == "delete_eip" || action == "delete_snat")
	if err != nil {
		return err
	}
	kind := "eip"
	if action == "create_snat" || action == "delete_snat" {
		kind = "snat"
	}
	b := defaultEgressBinding(x, kind)
	endpoint := resourceEndpoint(p.provider.client, b)
	if action == "delete_snat" || action == "delete_eip" {
		object := eip
		if kind == "snat" {
			object = snat
		} else if !o.Unbound {
			return &biz.ProviderError{Kind: biz.ProviderInUse}
		}
		if object == nil {
			return nil
		}
		if b.ProviderUid == "" || string(object.GetUID()) != b.ProviderUid {
			return &biz.ProviderError{Kind: biz.ProviderConflict}
		}
		uid, rv := types.UID(b.ProviderUid), object.GetResourceVersion()
		orphan := metav1.DeletePropagationOrphan
		if err = p.assertMutationLease(ctx, x); err != nil {
			return err
		}
		err = endpoint.Delete(ctx, b.ProviderName, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv}, PropagationPolicy: &orphan})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return mutationFailure(err)
		}
		return nil
	}
	if action != "create_eip" && action != "create_snat" {
		return &biz.ProviderError{Kind: biz.ProviderReject}
	}
	proof, err := p.ValidateDefaultVPCEgress(ctx, x.Egress.Identity, x.Egress.PoolID)
	if err != nil {
		return err
	}
	if proof.Revision != x.PoolRevision {
		return &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	if eip != nil && kind == "eip" || snat != nil && kind == "snat" {
		return &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	var spec map[string]any
	if kind == "eip" {
		pool, err := p.provider.platformBinding(ctx, "public_pool", x.Egress.PoolID)
		if err != nil {
			return err
		}
		spec = defaultEgressEIPSpec(pool.ProviderName)
	} else {
		if !o.EIPReady || eip == nil || crString(eip, "status", "phase") != "Available" || !emptyBoundResource(eip) {
			return &biz.ProviderError{Kind: biz.ProviderTemporary}
		}
		spec = defaultEgressSnatSpec(x.Egress)
	}
	object := egressObject(egressResolved{binding: b, target: biz.ProviderTarget{Kind: kind, ResourceID: x.Egress.ID}, spec: spec})
	if err = p.assertMutationLease(ctx, x); err != nil {
		return err
	}
	created, err := endpoint.Create(ctx, object, metav1.CreateOptions{FieldManager: "ani-network-service", FieldValidation: "Strict"})
	if err != nil {
		return mutationFailure(err)
	}
	if err = inspectEgressIdentity(created, b, x.Egress.ID, ""); err != nil {
		return &biz.ProviderError{Kind: biz.ProviderUncertain, Cause: err}
	}
	return nil
}
