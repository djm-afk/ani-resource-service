package instance

import (
	"context"
	"encoding/json"
	"strconv"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/instance"
	network "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

func (n *InstanceNetwork) ReserveNetworkInterface(ctx context.Context, i biz.Instance, o biz.Operation, intent biz.NetworkInterfaceIntent) (biz.NetworkInterfaceChoice, error) {
	if err := intent.Validate(); err != nil {
		return biz.NetworkInterfaceChoice{}, err
	}
	if intent.Mode == "automatic" {
		return biz.NetworkInterfaceChoice{Mode: "automatic"}, nil
	}
	if i.Spec.Network == nil {
		return biz.NetworkInterfaceChoice{}, biz.ErrNetworkConflict
	}
	x, err := n.network.ReserveVNic(ctx, network.VNicReservation{TenantID: i.TenantID, VNicID: intent.VNicID, ConsumerKind: "container", ConsumerID: i.ID, Slot: intent.Slot, OperationID: o.ID, SubnetID: i.Spec.Network.SubnetID, Revision: o.Version})
	if err != nil {
		return biz.NetworkInterfaceChoice{}, instanceNetworkError(err)
	}
	if x.ClusterID != i.Spec.ClusterID || x.Namespace != i.Spec.Namespace {
		return biz.NetworkInterfaceChoice{}, biz.ErrNetworkConflict
	}
	return biz.NetworkInterfaceChoice{Mode: "manual", Name: x.Name, VNicID: x.ID, ProviderName: x.ProviderName, ProviderUID: x.ProviderUID, MACAddress: x.MACAddress, IPs: append([]string{}, x.IPs...)}, nil
}
func (n *InstanceNetwork) SyncNetworkInterfaces(ctx context.Context, i biz.Instance, ids []string) error {
	for _, id := range biz.PrimaryNetworkInterface(i).KeepVNicIDs() {
		if _, err := n.network.SyncVNicSecurityGroups(ctx, i.TenantID, id, "container", i.ID, ids); err != nil {
			return instanceNetworkError(err)
		}
	}
	return nil
}
func (n *InstanceNetwork) ReleaseNetworkInterfaces(ctx context.Context, i biz.Instance, keep []string) error {
	cfg := biz.PrimaryNetworkInterface(i)
	if !cfg.PendingRestart() && len(keep) > 0 {
		if err := n.network.ApplyVNicBinding(ctx, i.TenantID, "container", i.ID, cfg.Applied.VNicID, cfg.AppliedRevision); err != nil {
			return instanceNetworkError(err)
		}
	}
	return instanceNetworkError(n.network.ReleaseVNicBindings(ctx, i.TenantID, "container", i.ID, keep))
}

func (n *Notebook) SetNetworkInterface(ctx context.Context, i biz.Instance, c biz.NetworkInterfaceChoice, revision int64) error {
	obs, err := n.ObserveIdentity(ctx, i)
	if err != nil {
		return err
	}
	if !obs.SafeStopped || obs.PodExists {
		return biz.ErrNetworkUnavailable
	}
	root, err := n.lifecycleGet(ctx, notebookGVR, i.Spec.Namespace, i.Spec.NotebookName)
	if err != nil {
		return err
	}
	if !owned(root, i, obs.NotebookUID) || string(root.GetUID()) != i.NotebookUID || root.GetDeletionTimestamp() != nil {
		return biz.ErrOwnership
	}
	ann := root.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	value := ""
	if c.Mode == "manual" {
		if c.ProviderName == "" {
			return biz.ErrNetworkConflict
		}
		value = c.ProviderName
	}
	revisionText := strconv.FormatInt(revision, 10)
	if ann[biz.VNicAnnotation] != value || ann["network.ani.io/interface-revision"] != revisionText {
		delete(ann, biz.VNicAnnotation)
		if value != "" {
			ann[biz.VNicAnnotation] = value
		}
		ann["network.ani.io/interface-revision"] = revisionText
		raw, err := json.Marshal([]map[string]any{{"op": "test", "path": "/metadata/uid", "value": string(root.GetUID())}, {"op": "test", "path": "/metadata/resourceVersion", "value": root.GetResourceVersion()}, {"op": "add", "path": "/metadata/annotations", "value": ann}})
		if err != nil {
			return err
		}
		if _, err = n.client.Resource(notebookGVR).Namespace(i.Spec.Namespace).Patch(ctx, i.Spec.NotebookName, types.JSONPatchType, raw, metav1.PatchOptions{}); err != nil {
			return err
		}
		return biz.ErrNetworkUnavailable
	}
	// Notebook owns StatefulSet propagation. Do not start before that template
	// carries this exact revision, including removal when returning to automatic.
	set, err := n.lifecycleGet(ctx, stsGVR, i.Spec.Namespace, i.Spec.NotebookName)
	if apierrors.IsNotFound(err) {
		return biz.ErrNetworkUnavailable // The Notebook controller is rebuilding it.
	}
	if err != nil {
		return err
	}
	if !controlledBy(set, "kubeflow.org", "Notebook", i.Spec.NotebookName, i.NotebookUID) || string(set.GetUID()) != obs.StatefulSetUID {
		return biz.ErrOwnership
	}
	template, _, err := unstructured.NestedStringMap(set.Object, "spec", "template", "metadata", "annotations")
	if err != nil {
		return err
	}
	matches := template[biz.VNicAnnotation] == value && template["network.ani.io/interface-revision"] == revisionText
	after, err := n.ObserveIdentity(ctx, i)
	if err != nil {
		return err
	}
	if !after.SafeStopped || after.PodExists || after.NotebookUID != obs.NotebookUID || after.StatefulSetUID != obs.StatefulSetUID {
		return biz.ErrNetworkUnavailable
	}
	latestRoot, err := n.lifecycleGet(ctx, notebookGVR, i.Spec.Namespace, i.Spec.NotebookName)
	if err != nil {
		return err
	}
	if !owned(latestRoot, i, obs.NotebookUID) || !sameMountTemplateRead(root, latestRoot) {
		return biz.ErrOwnership
	}
	latestSet, err := n.lifecycleGet(ctx, stsGVR, i.Spec.Namespace, i.Spec.NotebookName)
	if apierrors.IsNotFound(err) {
		return biz.ErrNetworkUnavailable
	}
	if err != nil {
		return err
	}
	if !controlledBy(latestSet, "kubeflow.org", "Notebook", i.Spec.NotebookName, i.NotebookUID) || string(latestSet.GetUID()) != after.StatefulSetUID || !sameMountTemplateRead(set, latestSet) {
		return biz.ErrOwnership
	}
	if matches {
		return nil
	}
	// Notebook v1.10 propagates annotations only when creating its StatefulSet.
	// Retire this stopped, empty controller child; the Notebook recreates it
	// from the saved root annotations. UID/RV fencing makes response loss safe.
	if latestSet.GetDeletionTimestamp() != nil || latestSet.GetResourceVersion() == "" {
		return biz.ErrNetworkUnavailable
	}
	uid, rv := latestSet.GetUID(), latestSet.GetResourceVersion()
	policy := metav1.DeletePropagationForeground
	err = n.client.Resource(stsGVR).Namespace(i.Spec.Namespace).Delete(ctx, i.Spec.NotebookName, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv}, PropagationPolicy: &policy})
	if err != nil && !apierrors.IsNotFound(err) {
		return mutationError(err)
	}
	return biz.ErrNetworkUnavailable
}

func (n *InstanceNetwork) ResolveNetworkInterfaceChoice(ctx context.Context, i biz.Instance, c biz.NetworkInterfaceChoice) (biz.NetworkInterfaceChoice, error) {
	if c.Mode != "manual" || c.Name != "" {
		return c, nil
	}
	x, err := n.network.GetVNic(ctx, i.TenantID, c.VNicID)
	if err != nil {
		return c, instanceNetworkError(err)
	}
	if (c.ProviderUID != "" && x.ProviderUID != "" && x.ProviderUID != c.ProviderUID) || x.ProviderName != c.ProviderName {
		return c, biz.ErrNetworkConflict
	}
	c.Name = x.Name
	return c, nil
}
