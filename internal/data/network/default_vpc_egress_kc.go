package data

import (
	"context"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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

func defaultEgressBinding(x biz.DefaultEgressWork, kind string) sqlcgen.NetworkProviderBinding {
	b := sqlcgen.NetworkProviderBinding{ResourceKind: kind, ClusterID: x.Egress.Identity.ClusterID, Namespace: x.Egress.Identity.Namespace}
	if kind == "eip" {
		b.ProviderName, b.ProviderUid, b.BindingID = x.Egress.EIPProviderName, x.Egress.EIPProviderUID, x.EIPBindingID
	} else {
		b.ProviderName, b.ProviderUid, b.BindingID = x.Egress.SnatProviderName, x.Egress.SnatProviderUID, x.SnatBindingID
	}
	return b
}
func (p *DefaultEgressKCProvider) readOwned(ctx context.Context, x biz.DefaultEgressWork, kind string, deleting bool) (*unstructured.Unstructured, error) {
	b := defaultEgressBinding(x, kind)
	object, err := resourceEndpoint(p.provider.client, b).Get(ctx, b.ProviderName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, readFailure(err)
	}
	if deleting && b.ProviderUid != "" && string(object.GetUID()) != b.ProviderUid {
		return nil, nil
	}
	if err = inspectEgressIdentity(object, b, x.Egress.ID, b.ProviderUid); err != nil {
		return nil, err
	}
	return object, nil
}
func defaultEgressEIPSpec(pool string) map[string]any {
	return map[string]any{"subnet": kcSystemNamespace + "/" + pool, "ipVersion": "IPv4"}
}
func defaultEgressSnatSpec(e biz.DefaultVPCEgress) map[string]any {
	return map[string]any{"eip": e.EIPProviderName, "vpc": e.Identity.Name, "disable": false}
}
func (p *DefaultEgressKCProvider) observe(ctx context.Context, x biz.DefaultEgressWork, deleting bool) (biz.DefaultEgressObservation, *unstructured.Unstructured, *unstructured.Unstructured, error) {
	o := biz.DefaultEgressObservation{EIPUID: x.Egress.EIPProviderUID, SnatUID: x.Egress.SnatProviderUID}
	if x.Egress.Identity != p.identity {
		return o, nil, nil, &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	eip, err := p.readOwned(ctx, x, "eip", deleting)
	if err != nil {
		return o, nil, nil, err
	}
	snat, err := p.readOwned(ctx, x, "snat", deleting)
	if err != nil {
		return o, nil, nil, err
	}
	o.EIPExists, o.SnatExists = eip != nil, snat != nil
	if eip != nil {
		o.EIPUID, o.Address = string(eip.GetUID()), crString(eip, "spec", "ipAddress")
	}
	if snat != nil {
		o.SnatUID = string(snat.GetUID())
		o.Ready, o.AppliedEnabled, o.StatusReason = snatPhase(snat, x.Egress.DesiredEnabled)
		if address := crString(snat, "status", "eipAddress"); address != "" {
			o.Address = address
		}
	}
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
	if x.Egress.Identity != p.identity {
		return &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	kind := "eip"
	if action == "create_snat" || action == "delete_snat" {
		kind = "snat"
	}
	deleting := action == "delete_eip" || action == "delete_snat"
	if !deleting && action != "create_eip" && action != "create_snat" {
		return &biz.ProviderError{Kind: biz.ProviderReject}
	}
	b := defaultEgressBinding(x, kind)
	endpoint := resourceEndpoint(p.provider.client, b)
	object, err := p.readOwned(ctx, x, kind, deleting)
	if err != nil {
		return err
	}
	if deleting {
		if object == nil {
			return nil
		}
		uid, rv := object.GetUID(), object.GetResourceVersion()
		if err = p.assertMutationLease(ctx, x); err != nil {
			return err
		}
		err = endpoint.Delete(ctx, b.ProviderName, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv}})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return mutationFailure(err)
		}
		return nil
	}
	if object != nil {
		return nil
	}
	if b.ProviderUid != "" {
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
		spec = defaultEgressSnatSpec(x.Egress)
	}
	if err = p.assertMutationLease(ctx, x); err != nil {
		return err
	}
	created, err := endpoint.Create(ctx, egressObject(egressResolved{binding: b, target: biz.ProviderTarget{Kind: kind, ResourceID: x.Egress.ID}, spec: spec}), metav1.CreateOptions{FieldManager: "ani-network-service", FieldValidation: "Strict"})
	if apierrors.IsAlreadyExists(err) {
		_, err = p.readOwned(ctx, x, kind, false)
		return err
	}
	if err != nil {
		return mutationFailure(err)
	}
	if err = inspectEgressIdentity(created, b, x.Egress.ID, ""); err != nil {
		return &biz.ProviderError{Kind: biz.ProviderUncertain, Cause: err}
	}
	return nil
}
