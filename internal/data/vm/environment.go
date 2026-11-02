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
	native  *Native
	prefix  string
	network NetworkSelectionValidator
}

func NewEnvironment(n *Native, prefix string, network NetworkSelectionValidator) (*Environment, error) {
	if n == nil || prefix != "tenant-" || network == nil {
		return nil, biz.ErrInvalid
	}
	return &Environment{n, prefix, network}, nil
}
func (e *Environment) ResolveNamespace(ctx context.Context, tenant, requested string) (biz.ObjectRef, error) {
	if !biz.UUID(tenant) {
		return biz.ObjectRef{}, biz.ErrInvalid
	}
	name := e.prefix + tenant
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
