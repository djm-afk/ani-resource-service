package data

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"

	"github.com/jackc/pgx/v5"
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

// ApplySecurityGroup reads only the current declaration or current bound target.
// Database identity and user configuration remain owned by Res.
func (p *KCProvider) ApplySecurityGroup(ctx context.Context, work biz.SecurityGroupWork) (biz.SecurityGroupResult, error) {
	if work.TenantID == "" || work.TargetID == "" || p.repository == nil {
		return biz.SecurityGroupResult{}, sgProviderConflict("missing durable work identity")
	}
	if work.TargetType == "security_group" {
		if work.Group.TenantID != work.TenantID || work.Group.ID != work.TargetID || work.Group.Version != work.Version || work.Group.ClusterID != p.repository.placement.ClusterID {
			return biz.SecurityGroupResult{}, sgProviderConflict("security group work identity changed")
		}
		return p.applySecurityGroupResource(ctx, work)
	}
	binding := work.Binding
	if binding.TenantID != work.TenantID || binding.TargetID != work.TargetID || binding.TargetType != work.TargetType || binding.Version != work.Version {
		return biz.SecurityGroupResult{}, sgProviderConflict("security group target identity changed")
	}
	if binding.State == "released" || binding.State == "deleted" {
		return biz.SecurityGroupResult{State: binding.State, Complete: true}, nil
	}
	switch work.TargetType {
	case "eip":
		return p.applyEIPSecurityGroups(ctx, work)
	case "container", "vm", "vnic":
		return p.observeTargetSecurityGroups(ctx, binding)
	default:
		return biz.SecurityGroupResult{}, &biz.ProviderError{Kind: biz.ProviderReject}
	}
}

func sgProviderConflict(message string) error {
	return &biz.ProviderError{Kind: biz.ProviderConflict, Cause: fmt.Errorf("%s", message)}
}

func securityGroupOperationActive(work biz.SecurityGroupWork) bool {
	return work.Operation.ID != "" && work.Operation.CompletedAt == nil
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

func securityGroupReady(object *unstructured.Unstructured) (*bool, biz.Reason) {
	return primaryCondition(object, "Ready")
}

func securityGroupProjection(result biz.SecurityGroupResult, previous biz.ResourceState, object *unstructured.Unstructured) biz.SecurityGroupResult {
	ready, reason := securityGroupReady(object)
	result.Observed = true
	result.ProviderUID = string(object.GetUID())
	result.Reason = string(reason)
	if ready == nil {
		if result.Reason == "" {
			result.Reason = string(biz.ProviderStatusUnreported)
		}
		return result
	}
	if *ready {
		result.State, result.Reason, result.Complete = "available", "", true
		return result
	}
	if previous == biz.Available || previous == biz.Degraded {
		result.State = "degraded"
	} else {
		result.State = "provisioning"
	}
	if result.Reason == "" {
		result.Reason = string(biz.ProviderNotReady)
	}
	return result
}

func (p *KCProvider) applySecurityGroupResource(ctx context.Context, work biz.SecurityGroupWork) (biz.SecurityGroupResult, error) {
	g := work.Group
	result := biz.SecurityGroupResult{ProviderUID: g.ProviderUID, State: string(g.State), Reason: string(g.Reason)}
	if g.State == biz.Deleted {
		result.Complete = true
		return result, nil
	}
	if g.Namespace == "" || g.ProviderName == "" || g.BindingID == "" {
		return result, sgProviderConflict("incomplete durable security group mapping")
	}
	endpoint := p.client.Resource(kcSecurityGroups).Namespace(g.Namespace)
	object, err := endpoint.Get(ctx, g.ProviderName, metav1.GetOptions{})
	deleting := g.State == biz.Deleting
	if apierrors.IsNotFound(err) {
		result.Observed = true
		if deleting {
			result.State, result.Reason, result.Complete = "deleted", "", true
			return result, nil
		}
		if g.ProviderUID != "" || !securityGroupOperationActive(work) || work.Operation.Kind != "create_security_group" {
			if g.State == biz.Available || g.State == biz.Degraded {
				result.State = "degraded"
			}
			result.Reason, result.OperationReason = string(biz.ProviderMissing), biz.ProviderMissing
			return result, nil
		}
		if err = p.verifyTenantNamespace(ctx, db.NetworkProviderBinding{TenantID: g.TenantID, ClusterID: g.ClusterID, Namespace: g.Namespace}); err != nil {
			return result, err
		}
		object, err = endpoint.Create(ctx, renderSecurityGroup(g), metav1.CreateOptions{FieldManager: "ani-network-service", FieldValidation: "Strict"})
		if apierrors.IsAlreadyExists(err) {
			object, err = endpoint.Get(ctx, g.ProviderName, metav1.GetOptions{})
			if err != nil {
				result.Observed, result.ObservationError = false, biz.ProviderUnavailable
				return result, readFailure(err)
			}
		} else if err != nil {
			failure := mutationFailure(err)
			var provider *biz.ProviderError
			if errors.As(failure, &provider) && provider.Kind == biz.ProviderReject {
				result.State, result.Reason = "failed", string(biz.ProviderRejected)
			}
			return result, failure
		}
	} else if err != nil {
		result.ObservationError = biz.ProviderUnavailable
		return result, readFailure(err)
	}
	// A same-name replacement ends the original deletion, without touching it.
	if deleting && g.ProviderUID != "" && string(object.GetUID()) != g.ProviderUID {
		result.State, result.Reason, result.Complete, result.Observed = "deleted", "", true, true
		return result, nil
	}
	if err = inspectSecurityGroupIdentity(object, g); err != nil {
		result.Observed = false
		return result, err
	}
	result.Observed, result.ProviderUID = true, string(object.GetUID())
	if deleting {
		result.State = "deleting"
		if object.GetDeletionTimestamp() == nil {
			uid, rv := object.GetUID(), object.GetResourceVersion()
			err = endpoint.Delete(ctx, g.ProviderName, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv}})
			if apierrors.IsNotFound(err) {
				result.State, result.Reason, result.Complete = "deleted", "", true
			} else if err != nil {
				return result, mutationFailure(err)
			}
		}
		return result, nil
	}
	result = securityGroupProjection(result, g.State, object)
	desired := renderSecurityGroup(g)
	if securityGroupOperationActive(work) && object.GetDeletionTimestamp() == nil && !securityGroupSpecEqual(object, desired) {
		spec, _, _ := unstructured.NestedMap(desired.Object, "spec")
		patch, _ := json.Marshal([]map[string]any{
			{"op": "test", "path": "/metadata/uid", "value": string(object.GetUID())},
			{"op": "test", "path": "/metadata/resourceVersion", "value": object.GetResourceVersion()},
			{"op": "add", "path": "/spec", "value": spec},
		})
		result.Complete = false
		object, err = endpoint.Patch(ctx, g.ProviderName, types.JSONPatchType, patch, metav1.PatchOptions{FieldManager: "ani-network-service", FieldValidation: "Strict"})
		if err != nil {
			return result, mutationFailure(err)
		}
		if err = inspectSecurityGroupIdentity(object, g); err != nil {
			return result, &biz.ProviderError{Kind: biz.ProviderUncertain, Cause: err}
		}
		result = securityGroupProjection(result, g.State, object)
	}
	if securityGroupOperationActive(work) && work.Operation.Kind == "update_security_group" {
		// This only confirms Res's current submitted SG version. It does not
		// change the resource projection from its primary Ready condition.
		result.Complete = result.Complete && crInt(object, "status", "observedGeneration") == object.GetGeneration()
	}
	return result, nil
}

// Only Res's persisted name mapping is needed to submit or display membership.
func (p *KCProvider) securityGroupNames(ctx context.Context, binding biz.SecurityGroupBinding) ([]string, error) {
	names := make([]string, 0, len(binding.SecurityGroupIDs))
	for _, id := range binding.SecurityGroupIDs {
		row, err := p.repository.queries.GetSecurityGroupRow(ctx, db.GetSecurityGroupRowParams{TenantID: binding.TenantID, SecurityGroupID: id})
		if err != nil {
			return nil, databaseFailure(err)
		}
		group, err := sgRead(row)
		if err != nil {
			return nil, err
		}
		if group.Namespace != binding.Namespace || group.ClusterID != p.repository.placement.ClusterID || group.ProviderName == "" {
			return nil, sgProviderConflict("security group name mapping differs from target")
		}
		names = append(names, group.ProviderName)
	}
	slices.Sort(names)
	return names, nil
}

func (p *KCProvider) securityGroupAppliedIDs(ctx context.Context, binding biz.SecurityGroupBinding, names []string) ([]string, error) {
	ids, err := p.repository.queries.MapSecurityGroupProviderNames(ctx, db.MapSecurityGroupProviderNamesParams{
		TenantID: binding.TenantID, ClusterID: p.repository.placement.ClusterID, Namespace: binding.Namespace, ProviderNames: names,
	})
	return ids, databaseFailure(err)
}

func (p *KCProvider) applyEIPSecurityGroups(ctx context.Context, work biz.SecurityGroupWork) (biz.SecurityGroupResult, error) {
	binding := work.Binding
	result := biz.SecurityGroupResult{State: binding.State, Reason: binding.Reason, AppliedIDs: slices.Clone(binding.AppliedSecurityGroupIDs)}
	eip, err := p.repository.queries.GetEIPInternal(ctx, db.GetEIPInternalParams{TenantID: binding.TenantID, EipID: binding.TargetID})
	if err != nil {
		result.OperationReason = biz.DependencyUnavailable
		return result, databaseFailure(err)
	}
	b, err := p.repository.queries.GetBinding(ctx, db.GetBindingParams{TenantID: binding.TenantID, ResourceID: binding.TargetID})
	if err != nil {
		result.OperationReason = biz.DependencyUnavailable
		return result, databaseFailure(err)
	}
	if b.ResourceKind != "eip" || b.TenantID != binding.TenantID || b.ClusterID != p.repository.placement.ClusterID || b.Namespace != binding.Namespace || b.Namespace != eip.Namespace || b.BindingID == "" || b.ProviderName == "" || eip.Scope != "public" || eip.ManagedBy != "tenant" {
		return result, sgProviderConflict("EIP binding differs from persisted target")
	}
	endpoint := p.client.Resource(kcEIPs).Namespace(b.Namespace)
	object, err := endpoint.Get(ctx, b.ProviderName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if b.ProviderUid != "" || eip.State == "deleting" || eip.State == "deleted" {
			return biz.SecurityGroupResult{State: "released", Complete: true, Observed: true, AppliedIDs: []string{}}, nil
		}
		return biz.SecurityGroupResult{State: "configured", Reason: string(biz.ProviderMissing), OperationReason: biz.ProviderMissing, Observed: true, AppliedIDs: []string{}}, nil
	}
	if err == nil && b.ProviderUid != "" && string(object.GetUID()) != b.ProviderUid {
		return biz.SecurityGroupResult{State: "released", Complete: true, Observed: true, AppliedIDs: []string{}}, nil
	}
	if err != nil {
		result.ObservationError = biz.ProviderUnavailable
		return result, readFailure(err)
	}
	if err = inspectEgressIdentity(object, b, binding.TargetID, b.ProviderUid); err != nil {
		return result, err
	}
	names, err := p.securityGroupNames(ctx, binding)
	if err != nil {
		result.OperationReason = biz.DependencyUnavailable
		return result, err
	}
	applied, found, err := unstructured.NestedStringSlice(object.Object, "status", "appliedSecurityGroups")
	if err != nil {
		result.ObservationError = biz.ProviderUnavailable
		return result, readFailure(err)
	}
	ids, err := p.securityGroupAppliedIDs(ctx, binding, applied)
	if err != nil {
		result.OperationReason = biz.DependencyUnavailable
		return result, err
	}
	result.Observed, result.AppliedIDs, result.State, result.Reason = true, ids, "configured", ""
	if !found {
		result.Reason = string(biz.ProviderStatusUnreported)
	} else if sameStrings(applied, names) {
		result.State, result.Complete = "applied", true
	}
	actual, _, _ := unstructured.NestedStringSlice(object.Object, "spec", "securityGroups")
	if securityGroupOperationActive(work) && object.GetDeletionTimestamp() == nil && eip.State != "deleting" && eip.State != "deleted" && !sameStrings(actual, names) {
		values := make([]any, 0, len(names))
		for _, name := range names {
			values = append(values, name)
		}
		patch, _ := json.Marshal([]map[string]any{
			{"op": "test", "path": "/metadata/uid", "value": string(object.GetUID())},
			{"op": "test", "path": "/metadata/resourceVersion", "value": object.GetResourceVersion()},
			{"op": "add", "path": "/spec/securityGroups", "value": values},
		})
		_, err = endpoint.Patch(ctx, b.ProviderName, types.JSONPatchType, patch, metav1.PatchOptions{FieldManager: "ani-network-service", FieldValidation: "Strict"})
		result.Complete = false
		if err != nil {
			return result, mutationFailure(err)
		}
	}
	return result, nil
}

func (p *KCProvider) observeTargetSecurityGroups(ctx context.Context, binding biz.SecurityGroupBinding) (biz.SecurityGroupResult, error) {
	result := biz.SecurityGroupResult{State: binding.State, Reason: binding.Reason, AppliedIDs: slices.Clone(binding.AppliedSecurityGroupIDs)}
	namespace, name, uid := binding.Namespace, "", ""
	var manual db.NetworkProviderBinding
	if binding.TargetType == "vnic" {
		row, err := p.repository.queries.GetVNicRow(ctx, db.GetVNicRowParams{TenantID: binding.TenantID, VnicID: binding.TargetID})
		if err != nil {
			result.OperationReason = biz.DependencyUnavailable
			return result, databaseFailure(err)
		}
		if row.State == "deleted" {
			return biz.SecurityGroupResult{State: "released", Complete: true, AppliedIDs: []string{}}, nil
		}
		manual, err = p.repository.queries.GetBinding(ctx, db.GetBindingParams{TenantID: binding.TenantID, ResourceID: binding.TargetID})
		if err != nil {
			result.OperationReason = biz.DependencyUnavailable
			return result, databaseFailure(err)
		}
		if manual.ResourceKind != "vnic" || manual.ClusterID != p.repository.placement.ClusterID || manual.Namespace != binding.Namespace {
			return result, sgProviderConflict("manual VNic mapping differs from target")
		}
		namespace, name, uid = manual.Namespace, manual.ProviderName, manual.ProviderUid
	} else {
		consumerType := "container_instance"
		if binding.TargetType == "vm" {
			consumerType = "virtual_machine"
		}
		row, err := p.repository.queries.GetSecurityGroupConsumerAttachment(ctx, db.GetSecurityGroupConsumerAttachmentParams{
			TenantID: binding.TenantID, ClusterID: p.repository.placement.ClusterID, Namespace: binding.Namespace,
			InstanceID: binding.TargetID, ConsumerType: consumerType,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return biz.SecurityGroupResult{State: "configured", Complete: true, AppliedIDs: []string{}}, nil
		}
		if err != nil {
			result.OperationReason = biz.DependencyUnavailable
			return result, databaseFailure(err)
		}
		a := biz.Attachment{Namespace: row.Namespace, CurrentVNicName: row.CurrentVnicName}
		namespace, name = currentVNicReference(a)
		uid = row.CurrentVnicUid
	}
	if name == "" {
		return biz.SecurityGroupResult{State: "configured", Complete: true, AppliedIDs: []string{}}, nil
	}
	if namespace != binding.Namespace {
		return result, sgProviderConflict("current VNic belongs to another namespace")
	}
	object, err := p.client.Resource(kcSecurityGroupVNics).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return biz.SecurityGroupResult{State: "configured", Reason: string(biz.ProviderMissing), Observed: true, AppliedIDs: []string{}}, nil
	}
	if err != nil {
		result.ObservationError = biz.ProviderUnavailable
		return result, readFailure(err)
	}
	if uid != "" && string(object.GetUID()) != uid {
		return result, sgProviderConflict("current VNic UID differs from persisted target")
	}
	if binding.TargetType == "vnic" {
		_, err = managedObjectExists(object, manual, biz.ProviderTarget{ResourceID: binding.TargetID, TenantID: binding.TenantID, KnownIdentity: uid}, "VNic")
		if err != nil {
			return result, err
		}
	}
	names, err := p.securityGroupNames(ctx, binding)
	if err != nil {
		result.OperationReason = biz.DependencyUnavailable
		return result, err
	}
	members, found, err := unstructured.NestedMap(object.Object, "status", "appliedSecurityGroups")
	if err != nil {
		result.ObservationError = biz.ProviderUnavailable
		return result, readFailure(err)
	}
	applied := make([]string, 0, len(members))
	for name := range members {
		applied = append(applied, name)
	}
	ids, err := p.securityGroupAppliedIDs(ctx, binding, applied)
	if err != nil {
		result.OperationReason = biz.DependencyUnavailable
		return result, err
	}
	result.Observed, result.AppliedIDs, result.State, result.Reason = true, ids, "configured", ""
	if !found {
		result.Reason = string(biz.ProviderStatusUnreported)
	} else if sameStrings(applied, names) {
		result.State, result.Complete = "applied", true
	}
	return result, nil
}

var _ biz.SecurityGroupProvider = (*KCProvider)(nil)
