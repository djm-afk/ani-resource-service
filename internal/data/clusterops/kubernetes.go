package clusterops

import (
	"context"
	"fmt"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/clusterops"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	coreclient "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
)

const fieldManager = "ani-res-clusterops"

type Kubernetes struct {
	dynamic   dynamic.Interface
	typed     kubernetes.Interface
	discovery discovery.DiscoveryInterface
	logs      coreclient.CoreV1Interface
}

var _ biz.KubernetesPort = (*Kubernetes)(nil)

// NewKubernetes only changes configuration copies owned by this module.
func NewKubernetes(config *rest.Config) (*Kubernetes, error) {
	if config == nil {
		return nil, fmt.Errorf("clusterops Kubernetes configuration required")
	}
	ordinary := rest.CopyConfig(config)
	ordinary.Timeout = biz.KubernetesTimeout
	ordinary.QPS = 5
	ordinary.Burst = 10
	ordinary.UserAgent = "ani-resource-service/clusterops"
	ordinary.WarningHandlerWithContext = rest.NoWarnings{}
	dyn, err := dynamic.NewForConfig(ordinary)
	if err != nil {
		return nil, fmt.Errorf("clusterops dynamic client configuration failed")
	}
	typed, err := kubernetes.NewForConfig(ordinary)
	if err != nil {
		return nil, fmt.Errorf("clusterops typed client configuration failed")
	}
	disc, err := discovery.NewDiscoveryClientForConfig(ordinary)
	if err != nil {
		return nil, fmt.Errorf("clusterops discovery configuration failed")
	}
	logConfig := rest.CopyConfig(ordinary)
	logConfig.Timeout = 0
	logs, err := coreclient.NewForConfig(logConfig)
	if err != nil {
		return nil, fmt.Errorf("clusterops log transport configuration failed")
	}
	return &Kubernetes{dynamic: dyn, typed: typed, discovery: disc, logs: logs}, nil
}
func gvr(t biz.ResourceTypeRef) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: t.Group, Version: t.Version, Resource: t.Resource}
}
func (k *Kubernetes) resource(ref biz.ResourceRef) dynamic.ResourceInterface {
	r := k.dynamic.Resource(gvr(ref.Type))
	if ref.Namespace != "" {
		return r.Namespace(ref.Namespace)
	}
	return r
}
func dryRun(dry bool) []string {
	if dry {
		return []string{metav1.DryRunAll}
	}
	return nil
}
func (k *Kubernetes) ListResources(ctx context.Context, q biz.ListQuery) (biz.ResourcePage, error) {
	labelSelector, err := labels.Parse(q.LabelSelector)
	if err != nil {
		return biz.ResourcePage{}, biz.NewError("INVALID_ARGUMENT")
	}
	fieldSelector, err := fields.ParseSelector(q.FieldSelector)
	if err != nil {
		return biz.ResourcePage{}, biz.NewError("INVALID_ARGUMENT")
	}
	if q.Name != "" {
		for _, term := range fieldSelector.Requirements() {
			if term.Field == "metadata.name" && (term.Operator != "=" && term.Operator != "==" || term.Value != q.Name) {
				return biz.ResourcePage{}, biz.NewError("INVALID_ARGUMENT")
			}
		}
		fieldSelector = fields.AndSelectors(fieldSelector, fields.OneTermEqualSelector("metadata.name", q.Name))
	}
	options := metav1.ListOptions{Limit: int64(q.PageSize), Continue: q.ContinueToken, LabelSelector: labelSelector.String(), FieldSelector: fieldSelector.String()}
	list, err := apiCall(ctx, func(c context.Context) (*unstructured.UnstructuredList, error) {
		return k.resource(biz.ResourceRef{Type: q.Type, Namespace: q.Namespace}).List(c, options)
	})
	if err != nil {
		return biz.ResourcePage{}, err
	}
	page := biz.ResourcePage{NextContinueToken: list.GetContinue(), ListResourceVersion: list.GetResourceVersion()}
	for i := range list.Items {
		ref := biz.ResourceRef{ClusterID: q.ClusterID, Type: q.Type, Namespace: list.Items[i].GetNamespace(), Name: list.Items[i].GetName()}
		r, err := project(ref, &list.Items[i])
		if err != nil {
			return biz.ResourcePage{}, err
		}
		page.Raw = append(page.Raw, r)
	}
	return page, nil
}
func (k *Kubernetes) get(ctx context.Context, ref biz.ResourceRef) (*unstructured.Unstructured, error) {
	return apiCall(ctx, func(c context.Context) (*unstructured.Unstructured, error) {
		return k.resource(ref).Get(c, ref.Name, metav1.GetOptions{})
	})
}
func (k *Kubernetes) GetResource(ctx context.Context, ref biz.ResourceRef) (biz.RawResource, error) {
	obj, err := k.get(ctx, ref)
	if err != nil {
		return biz.RawResource{}, err
	}
	return project(ref, obj)
}
func (k *Kubernetes) CreateResource(ctx context.Context, c biz.CreateCommand) (biz.RawResource, error) {
	d, err := k.ResolveResourceType(ctx, c.Ref.ClusterID, c.Ref.Type)
	if err != nil {
		return biz.RawResource{}, err
	}
	obj, err := decodeManifestStrict(c.ManifestYAML)
	if err != nil {
		return biz.RawResource{}, err
	}
	if err = matchManifest(obj, c.Ref, d); err != nil {
		return biz.RawResource{}, err
	}
	if err = checkCreateFields(obj); err != nil {
		return biz.RawResource{}, err
	}
	saved, err := apiCall(ctx, func(ctx context.Context) (*unstructured.Unstructured, error) {
		return k.resource(c.Ref).Create(ctx, obj, metav1.CreateOptions{FieldManager: fieldManager, FieldValidation: "Strict", DryRun: dryRun(c.DryRun)})
	})
	if err != nil {
		return biz.RawResource{}, err
	}
	return project(c.Ref, saved)
}
func (k *Kubernetes) UpdateResource(ctx context.Context, c biz.UpdateCommand) (biz.RawResource, error) {
	d, err := k.ResolveResourceType(ctx, c.Ref.ClusterID, c.Ref.Type)
	if err != nil {
		return biz.RawResource{}, err
	}
	obj, err := decodeManifestStrict(c.ManifestYAML)
	if err != nil {
		return biz.RawResource{}, err
	}
	if err = matchManifest(obj, c.Ref, d); err != nil {
		return biz.RawResource{}, err
	}
	current, err := k.get(ctx, c.Ref)
	if err != nil {
		return biz.RawResource{}, err
	}
	if err = checkIdentity(current.GetUID(), current.GetResourceVersion(), c.ExpectedUID, c.ExpectedResourceVersion); err != nil {
		return biz.RawResource{}, err
	}
	if err = preserveSystemFields(obj, current); err != nil {
		return biz.RawResource{}, err
	}
	if c.Ref.Type.Group == "" && c.Ref.Type.Resource == "secrets" {
		_, data := obj.Object["data"]
		_, stringData := obj.Object["stringData"]
		if !data && !stringData {
			return biz.RawResource{}, biz.NewError("INVALID_ARGUMENT")
		}
	}
	obj.SetUID(types.UID(c.ExpectedUID))
	obj.SetResourceVersion(c.ExpectedResourceVersion)
	saved, err := apiCall(ctx, func(ctx context.Context) (*unstructured.Unstructured, error) {
		return k.resource(c.Ref).Update(ctx, obj, metav1.UpdateOptions{FieldManager: fieldManager, FieldValidation: "Strict", DryRun: dryRun(c.DryRun)})
	})
	if err != nil {
		return biz.RawResource{}, err
	}
	return project(c.Ref, saved)
}
func (k *Kubernetes) DeleteResource(ctx context.Context, ref biz.ResourceRef, uid string) (biz.DeleteResult, error) {
	if err := ctx.Err(); err != nil {
		return biz.DeleteResult{}, err
	}
	sub, cancel := context.WithTimeout(ctx, biz.KubernetesTimeout)
	defer cancel()
	expected := types.UID(uid)
	err := k.resource(ref).Delete(sub, ref.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &expected}})
	if ctx.Err() != nil {
		return biz.DeleteResult{}, ctx.Err()
	}
	if err != nil {
		e := kubeError(err, true)
		if e.(*biz.Error).Reason == "RESOURCE_NOT_FOUND" {
			return biz.DeleteResult{AlreadyAbsent: true}, nil
		}
		return biz.DeleteResult{}, e
	}
	return biz.DeleteResult{DeletionRequested: true}, nil
}
func checkIdentity(uid types.UID, rv, expectedUID, expectedRV string) error {
	if string(uid) != expectedUID {
		return biz.NewError("RESOURCE_UID_CHANGED")
	}
	if rv != expectedRV {
		return biz.NewError("RESOURCE_VERSION_CONFLICT")
	}
	return nil
}
