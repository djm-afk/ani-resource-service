package clusterops

import (
	"context"
	"encoding/json"
	"errors"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/clusterops"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/version"
	"time"
)

func builtin(group, version, name, kind string, namespaced bool, extra ...string) biz.ResourceDescriptor {
	actions := []string{"list", "get"}
	if len(extra) == 0 {
		actions = append(actions, "create", "update", "delete")
	} else if extra[0] != "read" {
		actions = append(actions, extra...)
	}
	return biz.ResourceDescriptor{Type: biz.ResourceTypeRef{Group: group, Version: version, Resource: name}, Kind: kind, Namespaced: namespaced, Actions: actions}
}

var builtins = []biz.ResourceDescriptor{
	builtin("", "v1", "nodes", "Node", false, "read"),
	builtin("", "v1", "namespaces", "Namespace", false),
	builtin("", "v1", "pods", "Pod", true, "create", "update", "delete", "logs"),
	builtin("apps", "v1", "deployments", "Deployment", true, "create", "update", "delete", "scale", "restart"),
	builtin("apps", "v1", "statefulsets", "StatefulSet", true, "create", "update", "delete", "scale", "restart"),
	builtin("apps", "v1", "daemonsets", "DaemonSet", true, "create", "update", "delete", "restart"),
	builtin("apps", "v1", "replicasets", "ReplicaSet", true, "read"),
	builtin("apps", "v1", "controllerrevisions", "ControllerRevision", true, "read"),
	builtin("batch", "v1", "jobs", "Job", true),
	builtin("batch", "v1", "cronjobs", "CronJob", true),
	builtin("", "v1", "configmaps", "ConfigMap", true),
	builtin("", "v1", "secrets", "Secret", true),
	builtin("", "v1", "services", "Service", true),
	builtin("networking.k8s.io", "v1", "ingresses", "Ingress", true),
	builtin("discovery.k8s.io", "v1", "endpointslices", "EndpointSlice", true, "read"),
	builtin("", "v1", "persistentvolumeclaims", "PersistentVolumeClaim", true),
	builtin("", "v1", "persistentvolumes", "PersistentVolume", false),
	builtin("storage.k8s.io", "v1", "storageclasses", "StorageClass", false),
	builtin("", "v1", "serviceaccounts", "ServiceAccount", true),
	builtin("rbac.authorization.k8s.io", "v1", "roles", "Role", true),
	builtin("rbac.authorization.k8s.io", "v1", "rolebindings", "RoleBinding", true),
	builtin("rbac.authorization.k8s.io", "v1", "clusterroles", "ClusterRole", false),
	builtin("rbac.authorization.k8s.io", "v1", "clusterrolebindings", "ClusterRoleBinding", false),
	builtin("apiextensions.k8s.io", "v1", "customresourcedefinitions", "CustomResourceDefinition", false, "update", "delete"),
	builtin("", "v1", "events", "Event", true, "read"),
}
var crdType = biz.ResourceTypeRef{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}

func isNotFound(err error) bool {
	var e *biz.Error
	return errors.As(err, &e) && e.Reason == "RESOURCE_NOT_FOUND"
}
func (k *Kubernetes) apiResources(ctx context.Context, t biz.ResourceTypeRef) (*metav1.APIResourceList, error) {
	path := "/api/" + t.Version
	if t.Group != "" {
		path = "/apis/" + t.Group + "/" + t.Version
	}
	return apiCall(ctx, func(c context.Context) (*metav1.APIResourceList, error) {
		list := new(metav1.APIResourceList)
		err := k.discovery.RESTClient().Get().AbsPath(path).Do(c).Into(list)
		return list, err
	})
}
func available(d biz.ResourceDescriptor, list *metav1.APIResourceList) bool {
	for _, r := range list.APIResources {
		if r.Name == d.Type.Resource && r.Kind == d.Kind && r.Namespaced == d.Namespaced {
			return true
		}
	}
	return false
}
func crdInfo(obj *unstructured.Unstructured) (biz.CRDReadInfo, error) {
	info := biz.CRDReadInfo{UID: string(obj.GetUID())}
	info.Scope, _, _ = unstructured.NestedString(obj.Object, "spec", "scope")
	info.Group, _, _ = unstructured.NestedString(obj.Object, "spec", "group")
	info.Plural, _, _ = unstructured.NestedString(obj.Object, "spec", "names", "plural")
	versions, _, err := unstructured.NestedSlice(obj.Object, "spec", "versions")
	if err != nil {
		return info, biz.NewError("CRD_INSPECTION_FAILED")
	}
	for _, v := range versions {
		m, ok := v.(map[string]any)
		if !ok {
			return info, biz.NewError("CRD_INSPECTION_FAILED")
		}
		name, _, _ := unstructured.NestedString(m, "name")
		served, _, _ := unstructured.NestedBool(m, "served")
		storage, _, _ := unstructured.NestedBool(m, "storage")
		info.Versions = append(info.Versions, biz.CRDVersion{Name: name, Served: served, Storage: storage})
	}
	if info.Group == "" || info.Plural == "" || (info.Scope != "Cluster" && info.Scope != "Namespaced") {
		return info, biz.NewError("CRD_INSPECTION_FAILED")
	}
	return info, nil
}
func (k *Kubernetes) customDescriptors(ctx context.Context) ([]biz.ResourceDescriptor, error) {
	var out []biz.ResourceDescriptor
	token := ""
	for {
		page, err := apiCall(ctx, func(c context.Context) (*unstructured.UnstructuredList, error) {
			return k.dynamic.Resource(gvr(crdType)).List(c, metav1.ListOptions{Limit: 200, Continue: token})
		})
		if err != nil {
			return nil, err
		}
		for i := range page.Items {
			obj := &page.Items[i]
			info, err := crdInfo(obj)
			if err != nil {
				return nil, err
			}
			kind, _, _ := unstructured.NestedString(obj.Object, "spec", "names", "kind")
			if kind == "" {
				return nil, biz.NewError("CRD_INSPECTION_FAILED")
			}
			for _, v := range info.Versions {
				if v.Served {
					d := builtin(info.Group, v.Name, info.Plural, kind, info.Scope == "Namespaced")
					d.CustomResource = true
					d.CRDName = obj.GetName()
					out = append(out, d)
				}
			}
		}
		token = page.GetContinue()
		if token == "" {
			return out, nil
		}
	}
}
func (k *Kubernetes) ListResourceTypes(ctx context.Context, cluster string) ([]biz.ResourceDescriptor, error) {
	groups := map[string]*metav1.APIResourceList{}
	out := []biz.ResourceDescriptor{}
	for _, d := range builtins {
		key := d.Type.Group + "/" + d.Type.Version
		list, ok := groups[key]
		if !ok {
			var err error
			list, err = k.apiResources(ctx, d.Type)
			if err != nil && !isNotFound(err) {
				return nil, err
			}
			groups[key] = list
		}
		if list != nil && available(d, list) {
			out = append(out, d)
		}
	}
	custom, err := k.customDescriptors(ctx)
	if err != nil {
		return nil, err
	}
	return append(out, custom...), nil
}
func (k *Kubernetes) ResolveResourceType(ctx context.Context, cluster string, t biz.ResourceTypeRef) (biz.ResourceDescriptor, error) {
	for _, d := range builtins {
		if d.Type == t {
			list, err := k.apiResources(ctx, t)
			if isNotFound(err) {
				return biz.ResourceDescriptor{}, biz.NewError("RESOURCE_TYPE_UNAVAILABLE")
			}
			if err != nil {
				return biz.ResourceDescriptor{}, err
			}
			if available(d, list) {
				return d, nil
			}
			return biz.ResourceDescriptor{}, biz.NewError("RESOURCE_TYPE_UNAVAILABLE")
		}
	}
	custom, err := k.customDescriptors(ctx)
	if err != nil {
		return biz.ResourceDescriptor{}, err
	}
	for _, d := range custom {
		if d.Type == t {
			return d, nil
		}
	}
	return biz.ResourceDescriptor{}, biz.NewError("RESOURCE_TYPE_UNAVAILABLE")
}
func (k *Kubernetes) GetCRDReadInfo(ctx context.Context, ref biz.ResourceRef) (biz.CRDReadInfo, error) {
	obj, err := k.get(ctx, ref)
	if err != nil {
		return biz.CRDReadInfo{}, err
	}
	return crdInfo(obj)
}
func (k *Kubernetes) GetClusterOverview(ctx context.Context, cluster string) (biz.ClusterOverview, error) {
	out := biz.ClusterOverview{ClusterID: cluster, PodPhaseCounts: map[string]int64{}, Capacity: map[string]string{}, Allocatable: map[string]string{}}
	raw, err := apiCall(ctx, func(c context.Context) ([]byte, error) {
		return k.discovery.RESTClient().Get().AbsPath("/version").Do(c).Raw()
	})
	if err != nil {
		return out, err
	}
	var ver version.Info
	if json.Unmarshal(raw, &ver) != nil {
		return out, biz.NewError("INTERNAL_ERROR")
	}
	out.KubernetesVersion = ver.GitVersion
	capacity := map[corev1.ResourceName]resource.Quantity{}
	allocatable := map[corev1.ResourceName]resource.Quantity{}
	token := ""
	for {
		page, err := apiCall(ctx, func(c context.Context) (*corev1.NodeList, error) {
			return k.typed.CoreV1().Nodes().List(c, metav1.ListOptions{Limit: 200, Continue: token})
		})
		if err != nil {
			return out, err
		}
		out.NodesTotal += int64(len(page.Items))
		for _, node := range page.Items {
			for _, c := range node.Status.Conditions {
				if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
					out.NodesReady++
					break
				}
			}
			for key, v := range node.Status.Capacity {
				q := capacity[key]
				q.Add(v)
				capacity[key] = q
			}
			for key, v := range node.Status.Allocatable {
				q := allocatable[key]
				q.Add(v)
				allocatable[key] = q
			}
		}
		token = page.Continue
		if token == "" {
			break
		}
	}
	for key, v := range capacity {
		out.Capacity[string(key)] = v.String()
	}
	for key, v := range allocatable {
		out.Allocatable[string(key)] = v.String()
	}
	token = ""
	for {
		page, err := apiCall(ctx, func(c context.Context) (*corev1.NamespaceList, error) {
			return k.typed.CoreV1().Namespaces().List(c, metav1.ListOptions{Limit: 200, Continue: token})
		})
		if err != nil {
			return out, err
		}
		out.NamespacesTotal += int64(len(page.Items))
		token = page.Continue
		if token == "" {
			break
		}
	}
	token = ""
	for {
		page, err := apiCall(ctx, func(c context.Context) (*corev1.PodList, error) {
			return k.typed.CoreV1().Pods("").List(c, metav1.ListOptions{Limit: 200, Continue: token})
		})
		if err != nil {
			return out, err
		}
		out.PodsTotal += int64(len(page.Items))
		for _, pod := range page.Items {
			phase := string(pod.Status.Phase)
			if phase == "" {
				phase = "Unknown"
			}
			out.PodPhaseCounts[phase]++
		}
		token = page.Continue
		if token == "" {
			break
		}
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	out.SampledAt = time.Now().UTC()
	return out, nil
}
