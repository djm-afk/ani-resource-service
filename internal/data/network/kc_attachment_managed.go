package data

import (
	"context"
	"encoding/json"
	"fmt"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func workloadController(o *unstructured.Unstructured, api, kind string) (biz.WorkloadReference, bool) {
	var found biz.WorkloadReference
	for _, owner := range o.GetOwnerReferences() {
		if owner.Controller == nil || !*owner.Controller {
			continue
		}
		versionMatches := owner.APIVersion == api
		// Notebook 1.10 writes the served v1beta1 owner even when the same
		// object is read through v1. Canonicalize only this known alias; the
		// independent GET below must still match group/kind/name/namespace/UID.
		if api == "kubeflow.org/v1" && kind == "Notebook" && owner.APIVersion == "kubeflow.org/v1beta1" {
			versionMatches = true
		}
		if found.UID != "" || !versionMatches || owner.Kind != kind || owner.Name == "" || owner.UID == "" {
			return biz.WorkloadReference{}, false
		}
		found = biz.WorkloadReference{APIVersion: api, Kind: owner.Kind, Namespace: o.GetNamespace(), Name: owner.Name, UID: string(owner.UID)}
	}
	return found, found.UID != ""
}
func workloadIdentity(o *unstructured.Unstructured) biz.WorkloadReference {
	return biz.WorkloadReference{APIVersion: o.GetAPIVersion(), Kind: o.GetKind(), Namespace: o.GetNamespace(), Name: o.GetName(), UID: string(o.GetUID())}
}
func (p *KCProvider) getWorkload(ctx context.Context, r biz.WorkloadReference) (*unstructured.Unstructured, error) {
	gvr := schema.GroupVersionResource{}
	switch r.Kind {
	case "VirtualMachine":
		gvr = schema.GroupVersionResource{Group: "kubevirt.io", Version: "v1", Resource: "virtualmachines"}
	case "VirtualMachineInstance":
		gvr = schema.GroupVersionResource{Group: "kubevirt.io", Version: "v1", Resource: "virtualmachineinstances"}
	case "Notebook":
		gvr = schema.GroupVersionResource{Group: "kubeflow.org", Version: "v1", Resource: "notebooks"}
	case "StatefulSet":
		gvr = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"}
	default:
		return nil, fmt.Errorf("unsupported workload controller")
	}
	return p.client.Resource(gvr).Namespace(r.Namespace).Get(ctx, r.Name, metav1.GetOptions{})
}
func (p *KCProvider) verifyManagedPod(ctx context.Context, w biz.AttachmentWork, pod *unstructured.Unstructured, track func(attachmentRelation)) (bool, bool, error) {
	if w.Attachment.WorkloadMode == "vm_managed" {
		return p.verifyVMPod(ctx, w, pod, track)
	}
	conflict := func() (bool, bool, error) { return false, false, &biz.ProviderError{Kind: biz.ProviderConflict} }
	setRef, ok := workloadController(pod, "apps/v1", "StatefulSet")
	if !ok {
		return conflict()
	}
	set, err := p.getWorkload(ctx, setRef)
	if apierrors.IsNotFound(err) && w.ConsumerSnapshot != nil && w.ConsumerSnapshot.State != "open" {
		return false, false, nil
	}
	if err != nil {
		return false, false, readFailure(err)
	}
	if workloadIdentity(set) != setRef {
		return conflict()
	}
	rootRef, ok := workloadController(set, "kubeflow.org/v1", "Notebook")
	if !ok {
		return conflict()
	}
	root, err := p.getWorkload(ctx, rootRef)
	if apierrors.IsNotFound(err) && w.ConsumerSnapshot != nil && w.ConsumerSnapshot.State != "open" {
		return false, false, nil
	}
	if err != nil {
		return false, false, readFailure(err)
	}
	if workloadIdentity(root) != rootRef {
		return conflict()
	}
	for key, value := range map[string]string{tenantLabel: w.Attachment.TenantID, instanceLabel: w.Attachment.InstanceID, attachmentLabel: w.Attachment.ID, submissionLabel: w.Attachment.SubmissionID, generationLabel: fmt.Sprint(w.Attachment.Generation)} {
		if root.GetLabels()[key] != value {
			return conflict()
		}
	}
	if root.GetAnnotations()[subnetAnnotation] != w.Plan.PrimaryNetworkRef {
		return conflict()
	}
	track(attachmentRelation{Kind: "Notebook", Namespace: rootRef.Namespace, Name: rootRef.Name, UID: rootRef.UID, Current: true})
	track(attachmentRelation{Kind: "StatefulSet", Namespace: setRef.Namespace, Name: setRef.Name, UID: setRef.UID, OwnerUID: rootRef.UID, Current: true})
	c := w.ConsumerSnapshot
	if c == nil || c.RootController.UID == "" {
		return false, true, nil
	}
	if c.RootController != rootRef {
		return conflict()
	}
	identity := workloadIdentity(pod)
	for _, current := range c.CurrentPods {
		if current.Reference.UID == identity.UID {
			if current.Reference != identity || current.Owner != setRef {
				return conflict()
			}
			return c.State == "open" && c.DesiredRunState == "running" && pod.GetDeletionTimestamp() == nil, false, nil
		}
	}
	for _, retired := range c.RetiredPods {
		if retired.Reference.UID == identity.UID {
			if retired.Reference != identity || retired.Owner != setRef {
				return conflict()
			}
			return false, false, nil
		}
	}
	return false, true, nil
}
func (p *KCProvider) managedControllersExist(ctx context.Context, w biz.AttachmentWork) (bool, error) {
	c := w.ConsumerSnapshot
	exists := false
	refs := []biz.WorkloadReference{}
	if c != nil && c.RootController.UID != "" {
		refs = append(refs, c.RootController)
		for _, set := range c.Controllers {
			refs = append(refs, set.Reference)
		}
	}
	var prior []attachmentRelation
	if err := json.Unmarshal(w.Relations, &prior); err != nil {
		return false, readFailure(err)
	}
	for _, r := range prior {
		if r.Kind == "Notebook" || r.Kind == "StatefulSet" || (w.Attachment.WorkloadMode == "vm_managed" && (r.Kind == "VirtualMachine" || r.Kind == "VirtualMachineInstance")) {
			api := "kubeflow.org/v1"
			if r.Kind == "VirtualMachine" || r.Kind == "VirtualMachineInstance" {
				api = "kubevirt.io/v1"
			}
			if r.Kind == "StatefulSet" {
				api = "apps/v1"
			}
			refs = append(refs, biz.WorkloadReference{APIVersion: api, Kind: r.Kind, Namespace: r.Namespace, Name: r.Name, UID: r.UID})
		}
	}
	seen := map[biz.WorkloadReference]bool{}
	for _, ref := range refs {
		if seen[ref] {
			continue
		}
		seen[ref] = true
		obj, err := p.getWorkload(ctx, ref)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return false, readFailure(err)
		}
		if workloadIdentity(obj) != ref && !acceptedManagedControllerReplacement(w, ref, obj) {
			return false, &biz.ProviderError{Kind: biz.ProviderConflict}
		}
		exists = true
	}
	return exists, nil
}

func acceptedManagedControllerReplacement(w biz.AttachmentWork, previous biz.WorkloadReference, obj *unstructured.Unstructured) bool {
	c, a := w.ConsumerSnapshot, w.Attachment
	if c == nil || a.WorkloadMode != "controller_managed" || c.TenantID != a.TenantID || c.InstanceID != a.InstanceID || c.AttachmentID != a.ID || c.SubmissionID != a.SubmissionID || c.Generation != a.Generation || c.ClusterID != a.ClusterID || c.Namespace != a.Namespace {
		return false
	}
	root := c.RootController
	if root != a.RootController || root.APIVersion != "kubeflow.org/v1" || root.Kind != "Notebook" || root.Namespace != a.Namespace || root.Name == "" || root.UID == "" {
		return false
	}
	current := workloadIdentity(obj)
	if previous.APIVersion != "apps/v1" || previous.Kind != "StatefulSet" || previous.Namespace != root.Namespace || previous.UID == "" || current.UID == "" || current.UID == previous.UID {
		return false
	}
	sameName := current
	sameName.UID = previous.UID
	if sameName != previous {
		return false
	}
	owner, ok := workloadController(obj, root.APIVersion, root.Kind)
	if !ok || owner != root {
		return false
	}
	// The old UID has exited, but a specifically accepted successor still
	// exists. It remains a dependency until it exits normally.
	for _, accepted := range c.Controllers {
		if accepted.Reference == current && accepted.Owner == root {
			return true
		}
	}
	return false
}

// Nic lives in kcn's system Namespace and has no Pod UID ownerReference. Its
// exact witnessed identity is cleanup evidence only, never address authority.
func (p *KCProvider) managedNics(ctx context.Context, w biz.AttachmentWork, vnicNames map[string]string) ([]attachmentRelation, error) {
	podNames := map[string]bool{}
	if w.ConsumerSnapshot != nil {
		for _, group := range [][]biz.PodReference{w.ConsumerSnapshot.CurrentPods, w.ConsumerSnapshot.RetiredPods} {
			for _, pod := range group {
				podNames[pod.Reference.Namespace+"/"+pod.Reference.Name] = true
			}
		}
	}
	var prior []attachmentRelation
	if err := json.Unmarshal(w.Relations, &prior); err != nil {
		return nil, readFailure(err)
	}
	known := map[string]bool{}
	for _, relation := range prior {
		if relation.Kind == "Nic" {
			known[relation.Namespace+"/"+relation.Name] = true
		}
		if relation.Kind == "Pod" {
			podNames[relation.Namespace+"/"+relation.Name] = true
		}
	}
	var out []attachmentRelation
	gvr := schema.GroupVersionResource{Group: kcVPCs.Group, Version: "v1", Resource: "nics"}
	continuation, revision := "", ""
	count := 0
	for {
		list, err := p.client.Resource(gvr).Namespace(kcSystemNamespace).List(ctx, metav1.ListOptions{Limit: 200, Continue: continuation})
		if err != nil {
			return nil, readFailure(err)
		}
		if continuation != "" && revision != list.GetResourceVersion() {
			return nil, readFailure(fmt.Errorf("Nic cleanup snapshot changed"))
		}
		revision = list.GetResourceVersion()
		count += len(list.Items)
		if count > 2000 || (count == 2000 && list.GetContinue() != "") {
			return nil, readFailure(fmt.Errorf("Nic cleanup capacity exceeded"))
		}
		for _, obj := range list.Items {
			ref, _, _ := unstructured.NestedString(obj.Object, "spec", "vNic")
			if vnicNames[ref] == "" && !podNames[obj.GetAnnotations()["networking.kubercloud.com/pod"]] && !known[obj.GetNamespace()+"/"+obj.GetName()] {
				continue
			}
			if obj.GetAPIVersion() != "networking.kubercloud.com/v1" || obj.GetKind() != "Nic" || obj.GetUID() == "" {
				return nil, &biz.ProviderError{Kind: biz.ProviderConflict}
			}
			out = append(out, attachmentRelation{Kind: "Nic", Namespace: obj.GetNamespace(), Name: obj.GetName(), UID: string(obj.GetUID()), OwnerUID: vnicNames[ref], Current: true})
		}
		next := list.GetContinue()
		if next == "" {
			return out, nil
		}
		if next == continuation {
			return nil, readFailure(fmt.Errorf("Nic cleanup repeated page"))
		}
		continuation = next
	}
}
