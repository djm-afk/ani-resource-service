package data

import (
	"encoding/json"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// The same identities seed candidate lookup and Watch-to-Attachment routing.
// They only locate evidence: observeAttachment retains all ownership checks.
func attachmentObservationKeys(w biz.AttachmentWork) ([]string, error) {
	a := w.Attachment
	keys := []string{"attachment:" + a.ID, "uid:" + a.PodUID, "object:Pod/" + a.Namespace + "/" + a.PodName, "podname:" + a.Namespace + "/" + a.PodName, "eip-subnet:" + w.Plan.PrimaryNetworkRef}
	for _, uid := range w.ConsumerPodUIDs {
		keys = append(keys, "uid:"+uid)
	}
	var prior []attachmentRelation
	if err := json.Unmarshal(w.Relations, &prior); err != nil {
		return nil, err
	}
	for _, r := range prior {
		keys = append(keys, "uid:"+r.UID, "uid:"+r.OwnerUID, "object:"+r.Kind+"/"+r.Namespace+"/"+r.Name, "vNic:"+r.Namespace+"/"+r.Name)
	}
	if w.ConsumerSnapshot != nil && (a.WorkloadMode == "controller_managed" || a.WorkloadMode == "vm_managed") {
		// A retained manual VNIC has no Pod owner. Initial/recreated Pods must
		// locate its original identity before any current relation is persisted.
		// These are candidate hints; Provider still verifies the DB binding/UID.
		for _, manual := range w.ConsumerSnapshot.ManualVNics {
			if manual.ProviderUID != "" {
				keys = append(keys, "uid:"+manual.ProviderUID)
			}
			if manual.ProviderName != "" {
				keys = append(keys, "object:VNic/"+a.Namespace+"/"+manual.ProviderName, "vNic:"+a.Namespace+"/"+manual.ProviderName)
			}
		}
	}
	if a.WorkloadMode != "vm_managed" || w.ConsumerSnapshot == nil {
		return keys, nil
	}
	add := func(ref biz.WorkloadReference) {
		if ref.UID != "" {
			keys = append(keys, "uid:"+ref.UID)
		}
		if ref.Kind != "" && ref.Name != "" {
			keys = append(keys, "object:"+ref.Kind+"/"+ref.Namespace+"/"+ref.Name)
			if ref.Kind == "Pod" {
				keys = append(keys, "podname:"+ref.Namespace+"/"+ref.Name)
			}
		}
	}
	c := w.ConsumerSnapshot
	add(c.RootController)
	for _, controller := range c.Controllers {
		add(controller.Reference)
		add(controller.Owner)
	}
	for _, group := range [][]biz.PodReference{c.CurrentPods, c.RetiredPods} {
		for _, pod := range group {
			add(pod.Reference)
			add(pod.Owner)
		}
	}
	return keys, nil
}

// A same-name replacement or a launcher whose labels disappeared still needs
// inspection. A controller UID is a candidate hint, never proof of ownership.
func attachmentConsumerPodCandidate(w biz.AttachmentWork, pod unstructured.Unstructured) bool {
	if w.Attachment.WorkloadMode != "vm_managed" || w.ConsumerSnapshot == nil || pod.GetNamespace() != w.Attachment.Namespace {
		return false
	}
	c := w.ConsumerSnapshot
	for _, group := range [][]biz.PodReference{c.CurrentPods, c.RetiredPods} {
		for _, known := range group {
			if known.Reference.Namespace == pod.GetNamespace() && known.Reference.Name == pod.GetName() {
				return true
			}
		}
	}
	for _, owner := range pod.GetOwnerReferences() {
		if c.RootController.UID != "" && string(owner.UID) == c.RootController.UID {
			return true
		}
		for _, controller := range c.Controllers {
			if controller.Reference.UID != "" && string(owner.UID) == controller.Reference.UID {
				return true
			}
		}
	}
	return false
}

// VM/VMI are independently GET-verified by observeAttachment. Their resulting
// relations and owner snapshot belong to the proof even when the shared Pod,
// VNic and VNicIP set did not change. The caller retains its oldest source time.
func attachmentObservationHash(w biz.AttachmentWork, candidates map[schema.GroupVersionResource][]unstructured.Unstructured, result biz.AttachmentObservation) string {
	objects := candidatesForHash(candidates)
	if w.Attachment.WorkloadMode != "vm_managed" {
		return contentHash(objects)
	}
	snapshot := ""
	if w.ConsumerSnapshot != nil {
		snapshot = biz.ConsumerSnapshotHash(*w.ConsumerSnapshot)
	}
	return contentHash(struct {
		Objects   map[string][]unstructured.Unstructured
		Relations json.RawMessage
		Consumer  string
	}{objects, result.Relations, snapshot})
}
