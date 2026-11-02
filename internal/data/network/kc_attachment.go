package data

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sort"
	"strconv"
	"strings"
)

const attachmentLabel = "network.ani.io/attachment-id"
const instanceLabel = "network.ani.io/instance-id"
const submissionLabel = "network.ani.io/submission-id"
const generationLabel = "network.ani.io/generation"
const subnetAnnotation = "networking.kubercloud.com/subnet"

var pods = schema.GroupVersionResource{Version: "v1", Resource: "pods"}

type attachmentRelation struct {
	Kind, Namespace, Name, UID, OwnerUID string
	// Address eligibility is a fresh identity fact, never traffic health. Old
	// snapshots lack these fields and cannot authorize an LB until re-observed.
	Address         string `json:",omitempty"`
	Current         bool   `json:",omitempty"`
	AddressEligible bool   `json:",omitempty"`
}

func (p *KCProvider) listAll(ctx context.Context, resource schema.GroupVersionResource) ([]unstructured.Unstructured, error) {
	return p.listScoped(ctx, resource, "", "")
}

// Namespace and selector come only from fixed platform policy or a binding
// already verified by the caller; pagination retains the original safeguards.
func (p *KCProvider) listScoped(ctx context.Context, resource schema.GroupVersionResource, namespace, selector string) ([]unstructured.Unstructured, error) {
	var values []unstructured.Unstructured
	next := ""
	revision := ""
	seen := map[string]bool{}
	for {
		page, e := p.client.Resource(resource).Namespace(namespace).List(ctx, metav1.ListOptions{Limit: 500, Continue: next, LabelSelector: selector})
		if e != nil {
			return nil, readFailure(e)
		}
		if p.observation != nil && page.GetResourceVersion() == "" {
			return nil, readFailure(fmt.Errorf("audit LIST has no collection revision"))
		}
		if revision != "" && page.GetResourceVersion() != revision {
			return nil, readFailure(fmt.Errorf("audit pagination revision changed"))
		}
		revision = page.GetResourceVersion()
		values = append(values, page.Items...)
		next = page.GetContinue()
		if next != "" && seen[next] {
			return nil, readFailure(fmt.Errorf("audit pagination repeated token"))
		}
		seen[next] = true
		if next == "" {
			return values, nil
		}
	}
}
func kcRef(value, namespace string) string {
	if value != "" && !strings.Contains(value, "/") {
		return namespace + "/" + value
	}
	return value
}
func relationKey(kind, ns, name string) string { return kind + "/" + ns + "/" + name }
func ownerUID(o unstructured.Unstructured, kind string, ids map[string]bool) (string, bool) {
	for _, r := range o.GetOwnerReferences() {
		if r.Kind == kind && ids[string(r.UID)] && r.APIVersion == map[string]string{"Pod": "v1", "VNic": "networking.kubercloud.com/v1"}[kind] {
			return string(r.UID), true
		}
	}
	return "", false
}
func (p *KCProvider) observeAttachment(ctx context.Context, w biz.AttachmentWork, list func(context.Context, schema.GroupVersionResource) ([]unstructured.Unstructured, error)) (biz.AttachmentObservation, error) {
	a, plan := w.Attachment, w.Plan
	vmManaged := a.WorkloadMode == "vm_managed"
	managed := a.WorkloadMode == "controller_managed" || vmManaged
	identityKey := func(kind, namespace, name, uid string) string {
		key := relationKey(kind, namespace, name)
		if managed {
			key += "/" + uid
		}
		return key
	}
	conflict := func() (biz.AttachmentObservation, error) {
		return biz.AttachmentObservation{}, &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	b, e := p.repository.queries.GetBinding(ctx, sqlcgen.GetBindingParams{TenantID: a.TenantID, ResourceID: a.SubnetID})
	if e != nil {
		return biz.AttachmentObservation{}, readFailure(e)
	}
	if err := p.verifyTenantNamespace(ctx, b); err != nil {
		return biz.AttachmentObservation{}, err
	}
	if b.ClusterID != a.ClusterID || b.ClusterID != p.repository.placement.ClusterID || b.Namespace != a.Namespace || b.BindingID+":"+b.ProviderUid != a.BindingRevision || plan.PrimaryNetworkRef != b.Namespace+"/"+b.ProviderName {
		return conflict()
	}
	if vmManaged && w.ConsumerSnapshot != nil {
		vp := plan.VMPrimary
		if vp == nil {
			return conflict()
		}
		// Closure still proves the exact frozen NAD identity and bytes. A
		// historical config which cannot run must not prevent its cleanup.
		runnable := w.ConsumerSnapshot.State == "open" && w.ConsumerSnapshot.DesiredRunState == "running"
		_, err := p.resolveVMNetwork(ctx, biz.VMNetworkSelection{TenantID: a.TenantID, VPCID: a.VPCID, SubnetID: a.SubnetID, ClusterID: a.ClusterID, Namespace: a.Namespace, BindingRevision: a.BindingRevision, KCNSubnetRef: plan.PrimaryNetworkRef}, biz.VMNetworkExpectation{NADName: vp.NADName, NADUID: vp.NADUID, NADConfigDigest: vp.NADConfigDigest}, runnable)
		if err != nil {
			return biz.AttachmentObservation{}, readFailure(err)
		}
	}
	tracked := map[string]attachmentRelation{}
	var prior []attachmentRelation
	if e = json.Unmarshal(w.Relations, &prior); e != nil {
		return biz.AttachmentObservation{}, readFailure(e)
	}
	podUIDs := map[string]bool{}
	if a.PodUID != "" {
		podUIDs[a.PodUID] = true
	}
	for _, id := range w.ConsumerPodUIDs {
		podUIDs[id] = true
	}
	vnicUIDs := map[string]bool{}
	vnicNames := map[string]string{}
	for _, r := range prior {
		r.Current, r.AddressEligible = false, false
		tracked[identityKey(r.Kind, r.Namespace, r.Name, r.UID)] = r
		if r.Kind == "Pod" {
			podUIDs[r.UID] = true
		}
		if r.Kind == "VNic" {
			vnicUIDs[r.UID] = true
			vnicNames[r.Namespace+"/"+r.Name] = r.UID
		}
	}
	allPods, e := list(ctx, pods)
	if e != nil {
		return biz.AttachmentObservation{}, e
	}
	result := biz.AttachmentObservation{}
	var pod *unstructured.Unstructured
	livePods := 0
	for _, o := range allPods {
		labels := o.GetLabels()
		candidate := labels[attachmentLabel] == a.ID || podUIDs[string(o.GetUID())] || (a.PodName != "" && o.GetNamespace() == a.Namespace && o.GetName() == a.PodName)
		if !candidate {
			continue
		}
		if vmManaged && labels["kubevirt.io"] == "hotplug-disk" {
			continue
		}
		if (!managed && pod != nil) || o.GetKind() != "Pod" || o.GetAPIVersion() != "v1" || o.GetNamespace() != a.Namespace || o.GetUID() == "" || o.GetResourceVersion() == "" || labels[tenantLabel] != a.TenantID || labels[instanceLabel] != a.InstanceID || labels[attachmentLabel] != a.ID || labels[submissionLabel] != a.SubmissionID || labels[generationLabel] != strconv.FormatInt(a.Generation, 10) || o.GetAnnotations()[subnetAnnotation] != plan.PrimaryNetworkRef || (!managed && a.PodUID != "" && a.PodUID != string(o.GetUID())) {
			return conflict()
		}
		for key, value := range o.GetAnnotations() {
			if value != "" && (key == "k8s.v1.cni.cncf.io/networks" || strings.HasPrefix(key, "ovn.kubernetes.io/") || strings.HasSuffix(key, ".networking.kubercloud.com/subnet") || strings.HasSuffix(key, ".networking.kubercloud.com/vnic") || strings.HasSuffix(key, ".networking.kubercloud.com/vnicip")) {
				return conflict()
			}
		}
		copy := o
		result.Exists = true
		podUIDs[string(o.GetUID())] = true
		if managed {
			verified, pending, err := p.verifyManagedPod(ctx, w, &copy, func(r attachmentRelation) { tracked[identityKey(r.Kind, r.Namespace, r.Name, r.UID)] = r })
			if err != nil {
				return result, err
			}
			result.PendingCandidate = result.PendingCandidate || pending
			if o.GetDeletionTimestamp() == nil {
				livePods++
				if livePods > 1 {
					return conflict()
				}
			}
			owner, _ := workloadController(&copy, "apps/v1", "StatefulSet")
			if vmManaged {
				owner, _ = workloadController(&copy, "kubevirt.io/v1", "VirtualMachineInstance")
			}
			tracked[identityKey("Pod", o.GetNamespace(), o.GetName(), string(o.GetUID()))] = attachmentRelation{Kind: "Pod", Namespace: o.GetNamespace(), Name: o.GetName(), UID: string(o.GetUID()), OwnerUID: owner.UID, Current: verified}
			if !verified {
				continue
			}
			result.VerifiedCurrent = true
		}
		pod = &copy
		result.PodName = o.GetName()
		result.PodUID = string(o.GetUID())
	}
	vnics, e := list(ctx, schema.GroupVersionResource{Group: kcVPCs.Group, Version: kcVPCs.Version, Resource: "vnics"})
	if e != nil {
		return result, e
	}
	liveVNics := map[string]string{}
	currentVNics := map[string]bool{}
	for _, o := range vnics {
		key := identityKey("VNic", o.GetNamespace(), o.GetName(), string(o.GetUID()))
		old, seen := tracked[key]
		owner, owned := ownerUID(o, "Pod", podUIDs)
		podAnnotation := false
		if result.PodName != "" {
			_, podAnnotation = o.GetAnnotations()["pod.networking.kubercloud.com/"+result.PodName]
		}
		if a.PodName != "" {
			_, known := o.GetAnnotations()["pod.networking.kubercloud.com/"+a.PodName]
			podAnnotation = podAnnotation || known
		}
		if !owned && !seen && !podAnnotation {
			continue
		}
		subnet, _, _ := unstructured.NestedString(o.Object, "spec", "subnet")
		kind, _, _ := unstructured.NestedString(o.Object, "spec", "type")
		if !owned || o.GetKind() != "VNic" || o.GetAPIVersion() != "networking.kubercloud.com/v1" || o.GetNamespace() != a.Namespace || o.GetUID() == "" || kind != "VETH" || kcRef(subnet, o.GetNamespace()) != plan.PrimaryNetworkRef || (seen && old.UID != string(o.GetUID())) {
			return conflict()
		}
		uid := string(o.GetUID())
		tracked[key] = attachmentRelation{Kind: "VNic", Namespace: o.GetNamespace(), Name: o.GetName(), UID: uid, OwnerUID: owner, Current: o.GetDeletionTimestamp() == nil && (!managed || (pod != nil && owner == string(pod.GetUID())))}
		vnicUIDs[uid] = true
		vnicNames[o.GetNamespace()+"/"+o.GetName()] = uid
		liveVNics[o.GetNamespace()+"/"+o.GetName()] = uid
		currentVNics[uid] = !managed || (pod != nil && owner == string(pod.GetUID()))
		result.HasDependencies = true
	}
	ips, e := list(ctx, schema.GroupVersionResource{Group: kcVPCs.Group, Version: kcVPCs.Version, Resource: "vnicips"})
	if e != nil {
		return result, e
	}
	liveIPs := map[string]string{}
	for _, o := range ips {
		key := identityKey("VNicIP", o.GetNamespace(), o.GetName(), string(o.GetUID()))
		old, seen := tracked[key]
		owner, owned := ownerUID(o, "VNic", vnicUIDs)
		ref, _, _ := unstructured.NestedString(o.Object, "spec", "vNic")
		statusRef, _, _ := unstructured.NestedString(o.Object, "status", "vNic")
		related := vnicNames[kcRef(ref, o.GetNamespace())] != "" || vnicNames[kcRef(statusRef, o.GetNamespace())] != ""
		if !owned && !seen && !related {
			continue
		}
		subnet, _, _ := unstructured.NestedString(o.Object, "spec", "subnet")
		if !owned || o.GetKind() != "VNicIP" || o.GetAPIVersion() != "networking.kubercloud.com/v1" || o.GetNamespace() != a.Namespace || o.GetUID() == "" || kcRef(subnet, o.GetNamespace()) != plan.PrimaryNetworkRef || (seen && old.UID != string(o.GetUID())) || ((!managed || currentVNics[owner]) && ((ref != "" && vnicNames[kcRef(ref, o.GetNamespace())] != owner) || (statusRef != "" && vnicNames[kcRef(statusRef, o.GetNamespace())] != owner))) {
			return conflict()
		}
		address, _, _ := unstructured.NestedString(o.Object, "spec", "ipAddress")
		eligible := currentVNics[owner] && pod != nil && pod.GetDeletionTimestamp() == nil && o.GetDeletionTimestamp() == nil && liveVNics[kcRef(ref, o.GetNamespace())] == owner && kcRef(statusRef, o.GetNamespace()) == kcRef(ref, o.GetNamespace())
		podIP := ""
		if pod != nil {
			podIP, _, _ = unstructured.NestedString(pod.Object, "status", "podIP")
		}
		if vmManaged {
			podIP = ""
			for _, relation := range tracked {
				if relation.Kind == "VirtualMachineInstance" && relation.Current {
					podIP = relation.Address
				}
			}
		}
		eligible = eligible && address != "" && podIP == address
		tracked[key] = attachmentRelation{Kind: "VNicIP", Namespace: o.GetNamespace(), Name: o.GetName(), UID: string(o.GetUID()), OwnerUID: owner, Address: address, Current: o.GetDeletionTimestamp() == nil, AddressEligible: eligible}
		liveIPs[o.GetNamespace()+"/"+o.GetName()] = string(o.GetUID())
		result.HasDependencies = true
	}
	if pod != nil {
		annotations := pod.GetAnnotations()
		if value := annotations["networking.kubercloud.com/vnic"]; value != "" && liveVNics[kcRef(value, a.Namespace)] == "" {
			return conflict()
		}
		if value := annotations["networking.kubercloud.com/vnicip"]; value != "" && liveIPs[kcRef(value, a.Namespace)] == "" {
			return conflict()
		}
	}
	if managed && result.VerifiedCurrent {
		addressReady := false
		for _, relation := range tracked {
			if relation.Kind == "VNicIP" && relation.AddressEligible {
				addressReady = true
			}
		}
		result.VerifiedCurrent = addressReady
	}
	if managed && w.ConsumerSnapshot != nil && w.ConsumerSnapshot.State != "open" {
		controllers, err := p.managedControllersExist(ctx, w)
		if err != nil {
			return result, err
		}
		result.HasDependencies = result.HasDependencies || controllers
		nics, err := p.managedNics(ctx, w, vnicNames)
		if err != nil {
			return result, err
		}
		for _, nic := range nics {
			tracked[identityKey(nic.Kind, nic.Namespace, nic.Name, nic.UID)] = nic
			result.HasDependencies = true
		}
	}
	// Keep every witnessed UID after absence: a later same-name replacement or
	// an orphaned IP referring to a deleted VNic must still be attributable.
	keys := make([]string, 0, len(tracked))
	for k := range tracked {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	relations := make([]attachmentRelation, 0, len(keys))
	for _, k := range keys {
		relations = append(relations, tracked[k])
	}
	result.Relations, _ = json.Marshal(relations)
	return result, nil
}
