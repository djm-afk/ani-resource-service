package vm

import (
	"context"
	"encoding/json"
	"strings"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

func (n *Native) patchSecurityGroups(ctx context.Context, resource schema.GroupVersionResource, obj *unstructured.Unstructured, template bool, value string) error {
	if obj.GetUID() == "" || obj.GetResourceVersion() == "" || obj.GetDeletionTimestamp() != nil {
		return biz.ErrChanged
	}
	ann := obj.GetAnnotations()
	path := "/metadata/annotations"
	if template {
		var err error
		ann, _, err = unstructured.NestedStringMap(obj.Object, "spec", "template", "metadata", "annotations")
		if err != nil {
			return err
		}
		path = "/spec/template/metadata/annotations"
	}
	if ann == nil {
		ann = map[string]string{}
	}
	if current, present := ann[biz.SecurityGroupsAnnotation]; present && current == value {
		return nil
	}
	ann[biz.SecurityGroupsAnnotation] = value
	raw, err := json.Marshal([]map[string]any{{"op": "test", "path": "/metadata/uid", "value": string(obj.GetUID())}, {"op": "test", "path": "/metadata/resourceVersion", "value": obj.GetResourceVersion()}, {"op": "add", "path": path, "value": ann}})
	if err != nil {
		return err
	}
	_, err = n.client.Resource(resource).Namespace(obj.GetNamespace()).Patch(ctx, obj.GetName(), types.JSONPatchType, raw, metav1.PatchOptions{})
	return err
}

// SetSecurityGroups submits the VM template and the exact current references
// already published by this VM's owner. KCN reports the subsequent application.
func (n *Native) SetSecurityGroups(ctx context.Context, v biz.VM, names []string) error {
	if v.Native.UID == "" {
		return biz.ErrDependency
	}
	if err := n.namespace(ctx, v); err != nil {
		return err
	}
	value := strings.Join(names, ",")
	root, err := n.client.Resource(vmResource).Namespace(v.Spec.Namespace).Get(ctx, v.Native.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if !matches(root, v) || string(root.GetUID()) != v.Native.UID {
		return biz.ErrOwnership
	}
	if err = n.patchSecurityGroups(ctx, vmResource, root, true, value); err != nil {
		return err
	}
	if v.Network.Consumer == nil {
		return nil
	}
	for _, target := range []struct {
		reference *biz.ObjectRef
		resource  schema.GroupVersionResource
	}{{v.Network.Consumer.CurrentVMI, vmiResource}, {v.Network.Consumer.CurrentLauncher, podResource}} {
		if target.reference == nil || target.reference.Name == "" || target.reference.UID == "" {
			continue
		}
		object, err := n.client.Resource(target.resource).Namespace(target.reference.Namespace).Get(ctx, target.reference.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
		if string(object.GetUID()) != target.reference.UID {
			return biz.ErrChanged
		}
		if err = n.patchSecurityGroups(ctx, target.resource, object, false, value); err != nil {
			return err
		}
	}
	return nil
}
