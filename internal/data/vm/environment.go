package vm

import (
	"context"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/tenantnamespace"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type NetworkSelectionValidator func(context.Context, string, string, string, biz.NetworkSelection) (biz.NetworkSelection, error)
type Environment struct {
	native          *Native
	network         NetworkSelectionValidator
	existingNetwork func(context.Context, biz.VM) error
}

func NewEnvironment(n *Native, prefix string, network NetworkSelectionValidator) (*Environment, error) {
	if n == nil || prefix != tenantnamespace.Prefix || network == nil {
		return nil, biz.ErrInvalid
	}
	return &Environment{native: n, network: network}, nil
}
func (e *Environment) ResolveNamespace(ctx context.Context, tenant, requested string) (biz.ObjectRef, error) {
	name, err := tenantnamespace.Name(tenant)
	if err != nil {
		return biz.ObjectRef{}, biz.ErrInvalid
	}
	if requested != "" && requested != name {
		return biz.ObjectRef{}, biz.ErrOwnership
	}
	o, x := e.native.client.Resource(nsResource).Get(ctx, name, metav1.GetOptions{})
	if x != nil {
		return biz.ObjectRef{}, x
	}
	phase, _, _ := unstructured.NestedString(o.Object, "status", "phase")
	if tenantnamespace.Validate(tenant, tenantnamespace.Object{Name: o.GetName(), UID: string(o.GetUID()), Phase: phase, Labels: o.GetLabels(), Deleting: o.GetDeletionTimestamp() != nil}, "", true) != nil {
		return biz.ObjectRef{}, biz.ErrOwnership
	}

	return objectRef(o), nil
}
func (e *Environment) ValidateStorage(ctx context.Context, name, uid string) error {
	o, x := e.native.client.Resource(schema.GroupVersionResource{Group: "storage.k8s.io", Version: "v1", Resource: "storageclasses"}).Get(ctx, name, metav1.GetOptions{})
	if x != nil {
		return x
	}
	policy, _, _ := unstructured.NestedString(o.Object, "reclaimPolicy")
	if string(o.GetUID()) != uid || policy != "Delete" {
		return biz.ErrChanged
	}
	return nil
}
func (e *Environment) ValidateNetwork(ctx context.Context, t, c, ns string, s biz.NetworkSelection) (biz.NetworkSelection, error) {
	return e.network(ctx, t, c, ns, s)
}

func (e *Environment) UseExistingNetworkValidator(validate func(context.Context, biz.VM) error) {
	e.existingNetwork = validate
}

func (e *Environment) ValidateExistingNetwork(ctx context.Context, v biz.VM) error {
	if e.existingNetwork == nil {
		return biz.ErrDependency
	}
	return e.existingNetwork(ctx, v)
}
