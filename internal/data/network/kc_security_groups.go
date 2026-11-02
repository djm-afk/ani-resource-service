package data

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	db "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

var kcSecurityGroups = schema.GroupVersionResource{Group: kcVPCs.Group, Version: "v1", Resource: "securitygroups"}
var kcSecurityGroupVNics = schema.GroupVersionResource{Group: kcVPCs.Group, Version: "v1", Resource: "vnics"}

// ApplySecurityGroup writes only the KCN v1 contract. Desired state and the
// create token have already been committed before the worker calls this port.
func (p *KCProvider) ApplySecurityGroup(ctx context.Context, w biz.SecurityGroupWork) (biz.SecurityGroupResult, error) {
	if w.TenantID == "" || w.TargetID == "" {
		return biz.SecurityGroupResult{}, sgProviderConflict("missing durable work identity")
	}
	switch w.TargetType {
	case "security_group":
		if w.Group.TenantID != w.TenantID || w.Group.ID != w.TargetID || w.Group.Version != w.Version || p.repository == nil || w.Group.ClusterID != p.repository.placement.ClusterID {
			return biz.SecurityGroupResult{}, sgProviderConflict("security group work identity changed")
		}
		return p.applySecurityGroupResource(ctx, w.Group)
	case "eip":
		if w.Binding.TenantID != w.TenantID || w.Binding.TargetID != w.TargetID || w.Binding.TargetType != "eip" || w.Binding.Version != w.Version {
			return biz.SecurityGroupResult{}, sgProviderConflict("EIP security group work identity changed")
		}
		return p.applyEIPSecurityGroups(ctx, w.Binding)
	default:
		return biz.SecurityGroupResult{}, &biz.ProviderError{Kind: biz.ProviderReject}
	}
}

func sgProviderConflict(message string) error {
	return &biz.ProviderError{Kind: biz.ProviderConflict, Cause: fmt.Errorf("%s", message)}
}

func renderSecurityGroup(g biz.SecurityGroup) *unstructured.Unstructured {
	ingress, egress := []any{}, []any{}
	for _, r := range g.Rules {
		rule := map[string]any{"ipVersion": "IPv4", "protocol": r.Protocol, "action": r.Action, "priority": int64(r.Priority)}
		if len(r.RemoteAddresses) > 0 {
			addresses := make([]any, 0, len(r.RemoteAddresses))
			for _, address := range r.RemoteAddresses {
				addresses = append(addresses, address)
			}
			rule["remoteAddresses"] = addresses
		}
		if len(r.PortRanges) > 0 {
			ports := make([]any, 0, len(r.PortRanges))
			for _, port := range r.PortRanges {
				ports = append(ports, map[string]any{"min": int64(port.Min), "max": int64(port.Max)})
			}
			rule["portRanges"] = ports
		}
		if r.Direction == "ingress" {
			ingress = append(ingress, rule)
		} else {
			egress = append(egress, rule)
		}
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "networking.kubercloud.com/v1", "kind": "SecurityGroup",
		"metadata": map[string]any{"name": g.ProviderName, "namespace": g.Namespace, "labels": map[string]any{
			ownerLabel: "ani-network-service", tenantLabel: g.TenantID, resourceLabel: g.ID, bindingLabel: g.BindingID,
		}},
		"spec": map[string]any{"allowSameGroupTraffic": false, "ingressRules": ingress, "egressRules": egress},
	}}
}

func inspectSecurityGroupIdentity(o *unstructured.Unstructured, g biz.SecurityGroup) error {
	if o == nil || g.TenantID == "" || g.Namespace == "" || g.ProviderName == "" || g.BindingID == "" {
		return sgProviderConflict("incomplete security group identity")
	}
	l := o.GetLabels()
	if o.GetAPIVersion() != "networking.kubercloud.com/v1" || o.GetKind() != "SecurityGroup" ||
		o.GetName() != g.ProviderName || o.GetNamespace() != g.Namespace || o.GetUID() == "" || o.GetResourceVersion() == "" ||
		l[ownerLabel] != "ani-network-service" || l[tenantLabel] != g.TenantID || l[resourceLabel] != g.ID || l[bindingLabel] != g.BindingID ||
		(g.ProviderUID != "" && g.ProviderUID != string(o.GetUID())) {
		return sgProviderConflict("security group UID or ownership differs from persisted intent")
	}
	return nil
}

func securityGroupSpecEqual(o, desired *unstructured.Unstructured) bool {
	actual, _, _ := unstructured.NestedMap(o.Object, "spec")
	want, _, _ := unstructured.NestedMap(desired.Object, "spec")
	if actual["allowSameGroupTraffic"] != false {
		return false
	}
	for _, field := range []string{"ingressRules", "egressRules"} {
		a, _, e := unstructured.NestedSlice(actual, field)
		b, _, _ := unstructured.NestedSlice(want, field)
		if e != nil || (len(a) != 0 || len(b) != 0) && !reflect.DeepEqual(a, b) {
			return false
		}
	}
	return true
}

func securityGroupReady(o *unstructured.Unstructured) bool {
	return goodConditions(o, "Valid", "Ready") && crInt(o, "status", "observedGeneration") == o.GetGeneration() &&
		hasField(o, "status", "allowSameGroupTraffic") && !crBool(o, "status", "allowSameGroupTraffic")
}

func (p *KCProvider) applySecurityGroupResource(ctx context.Context, g biz.SecurityGroup) (biz.SecurityGroupResult, error) {
	r := biz.SecurityGroupResult{ProviderUID: g.ProviderUID, State: "provisioning"}
	if g.Namespace == "" || g.ProviderName == "" || g.BindingID == "" {
		return r, sgProviderConflict("incomplete durable security group mapping")
	}
	if err := biz.ValidateSecurityGroupRules(g.Rules); err != nil {
		return r, &biz.ProviderError{Kind: biz.ProviderReject, Cause: err}
	}
	if err := p.verifyTenantNamespace(ctx, db.NetworkProviderBinding{TenantID: g.TenantID, ClusterID: g.ClusterID, Namespace: g.Namespace}); err != nil {
		return r, err
	}
	endpoint := p.client.Resource(kcSecurityGroups).Namespace(g.Namespace)
	o, err := endpoint.Get(ctx, g.ProviderName, metav1.GetOptions{})
	deleting := g.State == biz.Deleting || g.State == biz.Deleted
	if apierrors.IsNotFound(err) {
		if deleting {
			r.State, r.Complete = "deleted", true
			return r, nil
		}
		if g.ProviderUID != "" {
			return r, sgProviderConflict("persisted security group provider object disappeared")
		}
		if err = p.verifyTenantNamespace(ctx, db.NetworkProviderBinding{TenantID: g.TenantID, Namespace: g.Namespace}); err != nil {
			return r, err
		}
		o, err = endpoint.Create(ctx, renderSecurityGroup(g), metav1.CreateOptions{FieldManager: "ani-network-service", FieldValidation: "Strict"})
		if apierrors.IsAlreadyExists(err) {
			o, err = endpoint.Get(ctx, g.ProviderName, metav1.GetOptions{})
		}
		if err != nil {
			return r, mutationFailure(err)
		}
	} else if err != nil {
		return r, readFailure(err)
	}
	if err = inspectSecurityGroupIdentity(o, g); err != nil {
		return r, err
	}
	r.ProviderUID = string(o.GetUID())
	if deleting {
		r.State = "deleting"
		vnics, _, ve := unstructured.NestedStringSlice(o.Object, "status", "appliedVNics")
		eips, _, ee := unstructured.NestedStringMap(o.Object, "status", "appliedEIPs")
		if ve != nil || ee != nil {
			return r, sgProviderConflict("malformed security group applied membership")
		}
		if len(vnics) > 0 || len(eips) > 0 {
			return r, &biz.ProviderError{Kind: biz.ProviderInUse}
		}
		if o.GetDeletionTimestamp() == nil {
			uid, rv := o.GetUID(), o.GetResourceVersion()
			err = endpoint.Delete(ctx, g.ProviderName, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv}})
			if err != nil && !apierrors.IsNotFound(err) {
				return r, mutationFailure(err)
			}
		}
		// Normal finalizers remain authoritative. Absence, rather than DELETE
		// acceptance, completes the durable deletion operation.
		_, err = endpoint.Get(ctx, g.ProviderName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			r.State, r.Complete = "deleted", true
			return r, nil
		}
		if err != nil {
			return r, readFailure(err)
		}
		return r, nil
	}
	if o.GetDeletionTimestamp() != nil {
		return r, sgProviderConflict("security group is unexpectedly deleting")
	}
	desired := renderSecurityGroup(g)
	if !securityGroupSpecEqual(o, desired) {
		spec, _, _ := unstructured.NestedMap(desired.Object, "spec")
		patch, _ := json.Marshal([]map[string]any{
			{"op": "test", "path": "/metadata/uid", "value": string(o.GetUID())},
			{"op": "test", "path": "/metadata/resourceVersion", "value": o.GetResourceVersion()},
			{"op": "add", "path": "/spec", "value": spec},
		})
		o, err = endpoint.Patch(ctx, g.ProviderName, types.JSONPatchType, patch, metav1.PatchOptions{FieldManager: "ani-network-service", FieldValidation: "Strict"})
		if err != nil {
			return r, mutationFailure(err)
		}
		if err = inspectSecurityGroupIdentity(o, g); err != nil {
			return r, &biz.ProviderError{Kind: biz.ProviderUncertain, Cause: err}
		}
	}
	if securityGroupReady(o) && securityGroupSpecEqual(o, desired) {
		r.State, r.Complete = "available", true
	}
	return r, nil
}

func (p *KCProvider) readMappedSecurityGroup(ctx context.Context, tenant, namespace, id string) (biz.SecurityGroup, *unstructured.Unstructured, error) {
	row, err := p.repository.queries.GetSecurityGroupRow(ctx, db.GetSecurityGroupRowParams{TenantID: tenant, SecurityGroupID: id})
	if err != nil {
		return biz.SecurityGroup{}, nil, readFailure(err)
	}
	g, err := sgRead(row)
	if err != nil {
		return g, nil, readFailure(err)
	}
	if g.Namespace != namespace || g.ProviderUID == "" {
		return g, nil, sgProviderConflict("security group mapping differs from target namespace")
	}
	if err := p.verifyTenantNamespace(ctx, db.NetworkProviderBinding{TenantID: tenant, ClusterID: g.ClusterID, Namespace: namespace}); err != nil {
		return g, nil, err
	}
	o, err := p.client.Resource(kcSecurityGroups).Namespace(namespace).Get(ctx, g.ProviderName, metav1.GetOptions{})
	if err != nil {
		return g, nil, readFailure(err)
	}
	return g, o, inspectSecurityGroupIdentity(o, g)
}

func (p *KCProvider) applyEIPSecurityGroups(ctx context.Context, binding biz.SecurityGroupBinding) (biz.SecurityGroupResult, error) {
	r := biz.SecurityGroupResult{State: "applying"}
	eip, err := p.repository.queries.GetEIPInternal(ctx, db.GetEIPInternalParams{TenantID: binding.TenantID, EipID: binding.TargetID})
	if err != nil {
		return r, readFailure(err)
	}
	b, err := p.repository.queries.GetBinding(ctx, db.GetBindingParams{TenantID: binding.TenantID, ResourceID: binding.TargetID})
	if err != nil {
		return r, readFailure(err)
	}
	if err := p.verifyTenantNamespace(ctx, b); err != nil {
		return r, err
	}

	if b.ResourceKind != "eip" || b.TenantID != binding.TenantID || b.ClusterID != p.repository.placement.ClusterID || b.Namespace != binding.Namespace ||
		b.Namespace != eip.Namespace || b.BindingID == "" || b.ProviderName == "" || eip.Scope != "public" || eip.ManagedBy != "tenant" {
		return r, sgProviderConflict("EIP binding differs from persisted security group target")
	}
	endpoint := p.client.Resource(kcEIPs).Namespace(b.Namespace)
	o, err := endpoint.Get(ctx, b.ProviderName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) && (eip.State == "deleted" || eip.State == "deleting") {
		clean, e := p.eipSecurityGroupMembershipsRemoved(ctx, binding.TenantID, binding.Namespace, b.ProviderName)
		if e != nil {
			return r, e
		}
		if !clean {
			r.Reason = "waiting_for_security_group_cleanup"
			return r, nil
		}
		return biz.SecurityGroupResult{State: "released", Complete: true, AppliedIDs: []string{}}, nil
	}
	if err != nil {
		return r, readFailure(err)
	}
	if b.ProviderUid == "" {
		r.Reason = "waiting_for_eip_identity"
		return r, nil
	}
	if err = inspectEgressIdentity(o, b, binding.TargetID, b.ProviderUid); err != nil {
		return r, err
	}
	if o.GetDeletionTimestamp() != nil || eip.State == "deleted" || eip.State == "deleting" {
		r.Reason = "waiting_for_eip_deletion"
		return r, nil
	}
	groups := map[string]*unstructured.Unstructured{}
	names := make([]string, 0, len(binding.SecurityGroupIDs))
	allIDs := slices.Clone(binding.SecurityGroupIDs)
	for _, id := range binding.AppliedSecurityGroupIDs {
		if !slices.Contains(allIDs, id) {
			allIDs = append(allIDs, id)
		}
	}
	for _, id := range allIDs {
		g, object, e := p.readMappedSecurityGroup(ctx, binding.TenantID, binding.Namespace, id)
		if e != nil {
			return r, e
		}
		groups[id] = object
		if slices.Contains(binding.SecurityGroupIDs, id) {
			if g.State != biz.Available || !securityGroupReady(object) || !securityGroupSpecEqual(object, renderSecurityGroup(g)) {
				r.Reason = "waiting_for_security_group"
				return r, nil
			}
			names = append(names, g.ProviderName)
		}
	}
	slices.Sort(names)
	actual, found, readErr := unstructured.NestedStringSlice(o.Object, "spec", "securityGroups")
	if readErr != nil {
		return r, sgProviderConflict("invalid EIP securityGroups field")
	}
	if !sameStrings(actual, names) || !found && len(names) > 0 {
		values := make([]any, 0, len(names))
		for _, name := range names {
			values = append(values, name)
		}
		patch, _ := json.Marshal([]map[string]any{
			{"op": "test", "path": "/metadata/uid", "value": string(o.GetUID())},
			{"op": "test", "path": "/metadata/resourceVersion", "value": o.GetResourceVersion()},
			{"op": "add", "path": "/spec/securityGroups", "value": values},
		})
		o, err = endpoint.Patch(ctx, b.ProviderName, types.JSONPatchType, patch, metav1.PatchOptions{FieldManager: "ani-network-service", FieldValidation: "Strict"})
		if err != nil {
			return r, mutationFailure(err)
		}
		if err = inspectEgressIdentity(o, b, binding.TargetID, b.ProviderUid); err != nil {
			return r, &biz.ProviderError{Kind: biz.ProviderUncertain, Cause: err}
		}
		// Membership read before a mutation cannot prove that mutation applied.
		r.Reason = "waiting_for_security_group_application"
		return r, nil
	}
	if len(names) > 0 && !eipReadyForSecurityGroups(o) {
		r.State, r.Reason = "waiting_for_binding", "waiting_for_binding"
		return r, nil
	}
	for id, group := range groups {
		applied, _, e := unstructured.NestedStringMap(group.Object, "status", "appliedEIPs")
		if e != nil {
			return r, readFailure(e)
		}
		_, present := applied[b.ProviderName]
		want := slices.Contains(binding.SecurityGroupIDs, id)
		if !securityGroupReady(group) || (want && applied[b.ProviderName] == "") || (!want && present) {
			r.Reason = "waiting_for_security_group_application"
			return r, nil
		}
	}
	// An earlier accepted configuration may have reached KCN before its worker
	// response was persisted. Check all our SGs so that replacing that intent
	// cannot release references while an intermediate membership still exists.
	list, err := p.client.Resource(kcSecurityGroups).Namespace(binding.Namespace).List(ctx, metav1.ListOptions{LabelSelector: ownerLabel + "=ani-network-service," + tenantLabel + "=" + binding.TenantID})
	if err != nil {
		return r, readFailure(err)
	}
	for _, group := range list.Items {
		if slices.Contains(names, group.GetName()) {
			continue
		}
		applied, _, e := unstructured.NestedStringMap(group.Object, "status", "appliedEIPs")
		if e != nil {
			return r, readFailure(e)
		}
		if _, present := applied[b.ProviderName]; present {
			r.Reason = "waiting_for_security_group_application"
			return r, nil
		}
	}
	r.State, r.Complete, r.AppliedIDs = "applied", true, slices.Clone(binding.SecurityGroupIDs)
	return r, nil
}

func eipReadyForSecurityGroups(o *unstructured.Unstructured) bool {
	return goodConditions(o, "Valid", "Ready") && crString(o, "status", "phase") == "Bound" &&
		crString(o, "status", "gateway") != "" && crString(o, "status", "boundResource", "vpc") != "" &&
		crString(o, "status", "boundResource", "nodeName") != "" && !crBool(o, "status", "boundResource", "disabled")
}

func (p *KCProvider) eipSecurityGroupMembershipsRemoved(ctx context.Context, tenant, namespace, name string) (bool, error) {
	groups, err := p.listScoped(ctx, kcSecurityGroups, namespace, ownerLabel+"=ani-network-service,"+tenantLabel+"="+tenant)
	if err != nil {
		return false, err
	}
	for _, group := range groups {
		applied, _, e := unstructured.NestedStringMap(group.Object, "status", "appliedEIPs")
		if e != nil {
			return false, readFailure(e)
		}
		if _, present := applied[name]; present {
			return false, nil
		}
	}
	return true, nil
}

// ObserveSecurityGroupConsumer proves application on the auto VNic belonging
// to the selected Pod UID. A stopped workload has a configuration, but cannot
// be reported as having a live data-plane application.
func (p *KCProvider) ObserveSecurityGroupConsumer(ctx context.Context, binding biz.SecurityGroupBinding) (bool, error) {
	if binding.TenantID == "" || binding.Namespace == "" || binding.TargetID == "" || (binding.TargetType != "container" && binding.TargetType != "vm") {
		return false, sgProviderConflict("incomplete security group consumer identity")
	}
	names := make([]string, 0, len(binding.SecurityGroupIDs))
	for _, id := range binding.SecurityGroupIDs {
		g, _, err := p.readMappedSecurityGroup(ctx, binding.TenantID, binding.Namespace, id)
		if err != nil {
			return false, err
		}
		names = append(names, g.ProviderName)
	}
	return p.observeSecurityGroupConsumerPods(ctx, binding, names)
}

func (p *KCProvider) observeSecurityGroupConsumerPods(ctx context.Context, binding biz.SecurityGroupBinding, names []string) (bool, error) {
	objects, err := p.listScoped(ctx, pods, binding.Namespace, tenantLabel+"="+binding.TenantID+","+instanceLabel+"="+binding.TargetID)
	if err != nil {
		return false, err
	}
	vnicNames := []string{}
	active := 0
	for _, pod := range objects {
		if binding.TargetType == "vm" && pod.GetLabels()["kubevirt.io"] == "hotplug-disk" {
			continue
		}
		if pod.GetKind() != "Pod" || pod.GetAPIVersion() != "v1" || pod.GetNamespace() != binding.Namespace || pod.GetUID() == "" || pod.GetResourceVersion() == "" ||
			pod.GetLabels()[tenantLabel] != binding.TenantID || pod.GetLabels()[instanceLabel] != binding.TargetID ||
			(binding.TargetType == "vm" && pod.GetLabels()["kubevirt.io"] != "virt-launcher") {
			return false, sgProviderConflict("consumer Pod differs from tenant and instance selection")
		}
		ref := pod.GetAnnotations()["networking.kubercloud.com/vnic"]
		if pod.GetDeletionTimestamp() != nil {
			if ref != "" {
				name := strings.TrimPrefix(ref, binding.Namespace+"/")
				if strings.Contains(name, "/") || name == "" {
					return false, sgProviderConflict("retiring Pod VNic is in another namespace")
				}
				_, e := p.client.Resource(kcSecurityGroupVNics).Namespace(binding.Namespace).Get(ctx, name, metav1.GetOptions{})
				if e == nil {
					return false, &biz.ProviderError{Kind: biz.ProviderTemporary}
				}
				if !apierrors.IsNotFound(e) {
					return false, readFailure(e)
				}
			}
			continue
		}
		active++
		if active > 1 {
			return false, &biz.ProviderError{Kind: biz.ProviderTemporary}
		}
		annotation := pod.GetAnnotations()["networking.kubercloud.com/security_groups"]
		declared := []string{}
		if annotation != "" {
			declared = strings.Split(annotation, ",")
			for i := range declared {
				declared[i] = strings.TrimSpace(declared[i])
			}
		}
		if !sameStrings(declared, names) || ref == "" {
			return false, &biz.ProviderError{Kind: biz.ProviderTemporary}
		}
		name := strings.TrimPrefix(ref, binding.Namespace+"/")
		if name == "" || strings.Contains(name, "/") {
			return false, sgProviderConflict("consumer VNic is in another namespace")
		}
		vnic, e := p.client.Resource(kcSecurityGroupVNics).Namespace(binding.Namespace).Get(ctx, name, metav1.GetOptions{})
		if e != nil {
			return false, readFailure(e)
		}
		owner, owned := ownerUID(*vnic, "Pod", map[string]bool{string(pod.GetUID()): true})
		if !owned || owner != string(pod.GetUID()) || vnic.GetAPIVersion() != "networking.kubercloud.com/v1" || vnic.GetKind() != "VNic" || vnic.GetNamespace() != binding.Namespace ||
			vnic.GetName() != name || vnic.GetUID() == "" || vnic.GetResourceVersion() == "" {
			return false, sgProviderConflict("VNic owner does not match the current Pod UID")
		}
		if vnic.GetDeletionTimestamp() != nil {
			return false, &biz.ProviderError{Kind: biz.ProviderTemporary}
		}
		vnicNames = append(vnicNames, name)
	}
	if active == 0 {
		relations, e := p.repository.queries.ListSecurityGroupConsumerRelations(ctx, db.ListSecurityGroupConsumerRelationsParams{TenantID: binding.TenantID, InstanceID: binding.TargetID})
		if e != nil {
			return false, readFailure(e)
		}
		if e = p.observeRetiredSecurityGroupVNics(ctx, binding, relations); e != nil {
			return false, e
		}
		return false, nil
	}
	if err = p.ObserveConsumerSecurityGroups(ctx, binding.TenantID, binding.Namespace, binding.SecurityGroupIDs, vnicNames); err != nil {
		return false, err
	}
	return true, nil
}

func (p *KCProvider) observeRetiredSecurityGroupVNics(ctx context.Context, binding biz.SecurityGroupBinding, history [][]byte) error {
	seen := map[string]bool{}
	for _, encoded := range history {
		var relations []attachmentRelation
		if err := json.Unmarshal(encoded, &relations); err != nil {
			return readFailure(err)
		}
		for _, relation := range relations {
			if relation.Kind != "VNic" {
				continue
			}
			if relation.Namespace != binding.Namespace || relation.Name == "" || relation.UID == "" {
				return sgProviderConflict("historical consumer VNic has an invalid identity")
			}
			key := relation.Namespace + "/" + relation.Name + "/" + relation.UID
			if seen[key] {
				continue
			}
			seen[key] = true
			o, err := p.client.Resource(kcSecurityGroupVNics).Namespace(relation.Namespace).Get(ctx, relation.Name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				continue
			}
			if err != nil {
				return readFailure(err)
			}
			if o.GetAPIVersion() != "networking.kubercloud.com/v1" || o.GetKind() != "VNic" || o.GetNamespace() != relation.Namespace ||
				o.GetName() != relation.Name || string(o.GetUID()) != relation.UID || o.GetResourceVersion() == "" {
				return sgProviderConflict("historical consumer VNic was replaced")
			}
			// A terminating object still owns its SG membership until KCN removes
			// it. Never release the database reference merely because its Pod left.
			return &biz.ProviderError{Kind: biz.ProviderTemporary}
		}
	}
	return nil
}

// ObserveConsumerSecurityGroups receives VNic names whose attachment identity
// has already been checked by the attachment adapter. It checks the declaration
// and controller application separately, including stale membership on unbind.
func (p *KCProvider) ObserveConsumerSecurityGroups(ctx context.Context, tenant, namespace string, ids, vnicNames []string) error {
	if len(ids) > 0 && len(vnicNames) == 0 {
		return &biz.ProviderError{Kind: biz.ProviderTemporary}
	}
	names := make([]string, 0, len(ids))
	expected := map[string]biz.SecurityGroup{}
	for _, id := range ids {
		g, o, err := p.readMappedSecurityGroup(ctx, tenant, namespace, id)
		if err != nil {
			return err
		}
		if g.State != biz.Available || !securityGroupReady(o) || !securityGroupSpecEqual(o, renderSecurityGroup(g)) {
			return &biz.ProviderError{Kind: biz.ProviderTemporary}
		}
		names = append(names, g.ProviderName)
		expected[g.ProviderName] = g
	}
	localNames := make([]string, 0, len(vnicNames))
	for _, ref := range vnicNames {
		name := strings.TrimPrefix(ref, namespace+"/")
		if name == "" || strings.Contains(name, "/") {
			return sgProviderConflict("VNic namespace differs from consumer")
		}
		o, err := p.client.Resource(kcSecurityGroupVNics).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return readFailure(err)
		}
		actual, _, e := unstructured.NestedStringSlice(o.Object, "spec", "securityGroups")
		if e != nil || o.GetDeletionTimestamp() != nil || !sameStrings(actual, names) {
			return &biz.ProviderError{Kind: biz.ProviderTemporary}
		}
		localNames = append(localNames, name)
	}
	list, err := p.client.Resource(kcSecurityGroups).Namespace(namespace).List(ctx, metav1.ListOptions{LabelSelector: ownerLabel + "=ani-network-service," + tenantLabel + "=" + tenant})
	if err != nil {
		return readFailure(err)
	}
	seen := map[string]bool{}
	for _, group := range list.Items {
		applied, _, e := unstructured.NestedStringSlice(group.Object, "status", "appliedVNics")
		if e != nil {
			return readFailure(e)
		}
		want := slices.Contains(names, group.GetName())
		if want {
			if err = inspectSecurityGroupIdentity(&group, expected[group.GetName()]); err != nil {
				return err
			}
			seen[group.GetName()] = true
		}
		for _, name := range localNames {
			if slices.Contains(applied, name) != want || want && !securityGroupReady(&group) {
				return &biz.ProviderError{Kind: biz.ProviderTemporary}
			}
		}
	}
	for _, name := range names {
		if !seen[name] {
			return &biz.ProviderError{Kind: biz.ProviderTemporary}
		}
	}
	return nil
}

var _ biz.SecurityGroupProvider = (*KCProvider)(nil)
