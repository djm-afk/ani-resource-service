package instance

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/instance"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// patchSecurityGroupAnnotation is an absolute replacement protected by the exact
// UID and resource version. An interrupted request can safely be observed/retried.
func (n *Notebook) patchSecurityGroupAnnotation(ctx context.Context, resource schema.GroupVersionResource, obj *unstructured.Unstructured, value string, intentVersion int64) error {
	if obj.GetUID() == "" || obj.GetResourceVersion() == "" || obj.GetDeletionTimestamp() != nil {
		return biz.ErrVersion
	}
	ann := obj.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	version := strconv.FormatInt(intentVersion, 10)
	if current, present := ann[biz.SecurityGroupsAnnotation]; present && current == value && ann[versionAnnotation] == version {
		return nil
	}
	ann[biz.SecurityGroupsAnnotation] = value
	ann[versionAnnotation] = version
	raw, err := json.Marshal([]map[string]any{{"op": "test", "path": "/metadata/uid", "value": string(obj.GetUID())}, {"op": "test", "path": "/metadata/resourceVersion", "value": obj.GetResourceVersion()}, {"op": "add", "path": "/metadata/annotations", "value": ann}})
	if err != nil {
		return err
	}
	_, err = n.client.Resource(resource).Namespace(obj.GetNamespace()).Patch(ctx, obj.GetName(), types.JSONPatchType, raw, metav1.PatchOptions{})
	return err
}

func (n *Notebook) SetSecurityGroups(ctx context.Context, i biz.Instance, names []string) error {
	value := strings.Join(names, ",")
	root, err := n.lifecycleGet(ctx, notebookGVR, i.Spec.Namespace, i.Spec.NotebookName)
	if err != nil {
		return err
	}
	if !owned(root, i, i.NotebookUID) {
		return biz.ErrOwnership
	}
	if err = n.patchSecurityGroupAnnotation(ctx, notebookGVR, root, value, i.Version); err != nil {
		return err
	}
	if i.PodName == "" || i.PodUID == "" {
		return nil
	}
	pod, err := n.lifecycleGet(ctx, podGVR, i.Spec.Namespace, i.PodName)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if string(pod.GetUID()) != i.PodUID {
		return biz.ErrVersion
	}
	return n.patchSecurityGroupAnnotation(ctx, podGVR, pod, value, i.Version)
}
