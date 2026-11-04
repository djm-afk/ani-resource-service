package data

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"reflect"
)

var kcEIPs = schema.GroupVersionResource{Group: kcVPCs.Group, Version: "v1", Resource: "eips"}
var kcSnats = schema.GroupVersionResource{Group: kcVPCs.Group, Version: "v1", Resource: "snats"}
var kcEgressGateways = schema.GroupVersionResource{Group: kcVPCs.Group, Version: "v1", Resource: "eipgateways"}
var kcVlans = schema.GroupVersionResource{Group: kcVPCs.Group, Version: "v1", Resource: "vlannetworks"}
var kcNodes = schema.GroupVersionResource{Version: "v1", Resource: "nodes"}
var kcConfigMaps = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}

const kcSystemNamespace = "kcn-system"

func egressKind(kind string) bool {
	switch kind {
	case "eip", "snat", "vlan", "egress_gateway", "public_pool":
		return true
	}
	return false
}
func egressGVR(kind string) schema.GroupVersionResource {
	switch kind {
	case "eip":
		return kcEIPs
	case "snat":
		return kcSnats
	case "vlan":
		return kcVlans
	case "egress_gateway":
		return kcEgressGateways
	case "public_pool":
		return kcSubnets
	}
	return schema.GroupVersionResource{}
}
func egressCRKind(kind string) string {
	switch kind {
	case "eip":
		return "EIP"
	case "snat":
		return "Snat"
	case "vlan":
		return "VlanNetwork"
	case "egress_gateway":
		return "EIPGateway"
	case "public_pool":
		return "Subnet"
	case "vpc":
		return "VPC"
	}
	return ""
}
func resourceEndpoint(c dynamic.Interface, b sqlcgen.NetworkProviderBinding) dynamic.ResourceInterface {
	gvr := egressGVR(b.ResourceKind)
	if b.ResourceKind == "vpc" {
		gvr = kcVPCs
	}
	if b.Namespace == "" {
		return c.Resource(gvr)
	}
	return c.Resource(gvr).Namespace(b.Namespace)
}
func (p *KCProvider) platformBinding(ctx context.Context, kind, id string) (sqlcgen.NetworkProviderBinding, error) {
	row, err := p.repository.readQueries(ctx).GetPlatform(ctx, sqlcgen.GetPlatformParams{ClusterID: p.repository.placement.ClusterID, Kind: kind, ResourceID: id})
	if err != nil {
		return sqlcgen.NetworkProviderBinding{}, readFailure(err)
	}
	r := row.NetworkPlatformResource
	namespace := ""
	if kind == "public_pool" {
		namespace = kcSystemNamespace
	}
	return sqlcgen.NetworkProviderBinding{ResourceKind: kind, ClusterID: r.ClusterID, Namespace: namespace, ProviderName: r.ProviderName, ProviderUid: r.ProviderUid, BindingID: r.BindingID}, nil
}
func (p *KCProvider) egressBinding(ctx context.Context, t biz.ProviderTarget) (sqlcgen.NetworkProviderBinding, error) {
	if t.TenantID != "" {
		return p.binding(ctx, t)
	}
	b, err := p.platformBinding(ctx, t.Kind, t.ResourceID)
	if err != nil {
		return b, err
	}
	if b.BindingID != t.BindingID || (t.KnownIdentity != "" && b.ProviderUid != "" && t.KnownIdentity != b.ProviderUid) {
		return b, &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	return b, nil
}

type egressResolved struct {
	binding sqlcgen.NetworkProviderBinding
	target  biz.ProviderTarget
	spec    map[string]any
}

// resolveEgress renders only the declaration owned by Res. Status reads never
// traverse these references to validate another controller's implementation.
func (p *KCProvider) resolveEgress(ctx context.Context, t biz.ProviderTarget) (egressResolved, error) {
	r := egressResolved{target: t}
	var err error
	r.binding, err = p.egressBinding(ctx, t)
	if err != nil {
		return r, err
	}
	q := p.repository.readQueries(ctx)
	tenantRef := func(kind, id string) (string, error) {
		b, err := q.GetBinding(ctx, sqlcgen.GetBindingParams{TenantID: t.TenantID, ResourceID: id})
		if err != nil {
			return "", readFailure(err)
		}
		if b.ResourceKind != kind || b.ClusterID != r.binding.ClusterID || b.Namespace != r.binding.Namespace {
			return "", &biz.ProviderError{Kind: biz.ProviderConflict}
		}
		return b.ProviderName, nil
	}
	switch t.Kind {
	case "eip":
		eip, err := q.GetEIPInternal(ctx, sqlcgen.GetEIPInternalParams{TenantID: t.TenantID, EipID: t.ResourceID})
		if err != nil {
			return r, readFailure(err)
		}
		pool, err := p.platformBinding(ctx, "public_pool", eip.PoolID)
		if err != nil {
			return r, err
		}
		r.spec = map[string]any{"subnet": kcSystemNamespace + "/" + pool.ProviderName, "ipVersion": "IPv4"}
		selection, err := q.GetSecurityGroupTarget(ctx, sqlcgen.GetSecurityGroupTargetParams{TenantID: t.TenantID, TargetType: "eip", TargetID: t.ResourceID})
		if err == nil {
			binding, err := sgTarget(selection)
			if err != nil {
				return r, err
			}
			names := []any{}
			for _, id := range binding.SecurityGroupIDs {
				group, err := p.repository.GetSecurityGroup(ctx, t.TenantID, id)
				if err != nil {
					return r, err
				}
				if group.Namespace != r.binding.Namespace {
					return r, &biz.ProviderError{Kind: biz.ProviderConflict}
				}
				names = append(names, group.ProviderName)
			}
			r.spec["securityGroups"] = names
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return r, readFailure(err)
		}
	case "snat":
		snat, err := q.GetSnatInternal(ctx, sqlcgen.GetSnatInternalParams{TenantID: t.TenantID, SnatID: t.ResourceID})
		if err != nil {
			return r, readFailure(err)
		}
		if t.Egress == nil || t.Egress.EIPID != snat.EipID || t.VPCID != snat.VpcID || t.Egress.DesiredEnabled != snat.DesiredEnabled {
			return r, &biz.ProviderError{Kind: biz.ProviderConflict}
		}
		eip, err := tenantRef("eip", snat.EipID)
		if err != nil {
			return r, err
		}
		vpc, err := tenantRef("vpc", snat.VpcID)
		if err != nil {
			return r, err
		}
		r.spec = map[string]any{"eip": eip, "vpc": vpc, "disable": !snat.DesiredEnabled}
	case "egress_gateway":
		r.spec = map[string]any{"scope": "Public", "egressType": "Host"}
	case "vlan":
		vlan, err := q.GetVlanNetwork(ctx, sqlcgen.GetVlanNetworkParams{ClusterID: r.binding.ClusterID, ResourceID: t.ResourceID})
		if err != nil {
			return r, readFailure(err)
		}
		device, err := q.GetDeviceAdoption(ctx, sqlcgen.GetDeviceAdoptionParams{ClusterID: r.binding.ClusterID, ResourceID: vlan.DeviceID})
		if err != nil {
			return r, readFailure(err)
		}
		r.spec = map[string]any{"devName": device.DeviceName, "vlanID": int64(vlan.VlanID)}
		if device.VirtualSwitchID != nil {
			sw, err := p.repository.GetVirtualSwitch(ctx, *device.VirtualSwitchID)
			if err != nil {
				return r, err
			}
			if device.SwitchBindingID == nil || sw.BindingID != *device.SwitchBindingID || sw.ClusterID != r.binding.ClusterID {
				return r, &biz.ProviderError{Kind: biz.ProviderConflict}
			}
			r.spec["vClusterName"] = sw.GroupName
		}
	case "public_pool":
		pool, err := q.GetPublicPool(ctx, sqlcgen.GetPublicPoolParams{ClusterID: r.binding.ClusterID, ResourceID: t.ResourceID})
		if err != nil {
			return r, readFailure(err)
		}
		if pool.Scope == "intranet" {
			r.spec = renderIntranetPool(pool)
			break
		}
		gateway, err := p.platformBinding(ctx, "egress_gateway", textValue(pool.GatewayID))
		if err != nil {
			return r, err
		}
		vlanName := ""
		if pool.VlanNetworkID != nil {
			vlan, err := p.platformBinding(ctx, "vlan", *pool.VlanNetworkID)
			if err != nil {
				return r, err
			}
			vlanName = vlan.ProviderName
		}
		r.spec = renderPublicPool(pool, gateway.ProviderName, vlanName)
	default:
		return r, &biz.ProviderError{Kind: biz.ProviderReject}
	}
	return r, nil
}

func renderPublicPool(c sqlcgen.NetworkPublicPool, gateway, vlan string) map[string]any {
	excluded := make([]any, len(c.ExcludedIps))
	for i, x := range c.ExcludedIps {
		excluded[i] = x
	}
	spec := map[string]any{"type": "Public", "ipVersion": "IPv4", "cidrBlock": c.Cidr, "gatewayIP": c.OvnGatewayIp, "gateway": gateway, "excludeIPs": excluded, "allowedNamespaces": map[string]any{"from": "All"}, "enableDHCP": false, "natOutgoing": false}
	if c.Mode == "underlay" {
		spec["underlayConfig"] = map[string]any{"vlanNetwork": vlan, "gatewayIP": textValue(c.UpstreamGatewayIp)}
	}
	return spec
}

func egressObject(r egressResolved) *unstructured.Unstructured {
	labels := map[string]any{ownerLabel: "ani-network-service", resourceLabel: r.target.ResourceID, bindingLabel: r.binding.BindingID}
	if r.target.TenantID != "" {
		labels[tenantLabel] = r.target.TenantID
	}
	metadata := map[string]any{"name": r.binding.ProviderName, "labels": labels}
	if r.binding.Namespace != "" {
		metadata["namespace"] = r.binding.Namespace
	}
	return &unstructured.Unstructured{Object: map[string]any{"apiVersion": "networking.kubercloud.com/v1", "kind": egressCRKind(r.target.Kind), "metadata": metadata, "spec": r.spec}}
}

func inspectEgressIdentity(o *unstructured.Unstructured, b sqlcgen.NetworkProviderBinding, id, uid string) error {
	if o == nil {
		return &biz.ProviderError{Kind: biz.ProviderTemporary}
	}
	labels := o.GetLabels()
	if o.GetAPIVersion() != "networking.kubercloud.com/v1" || o.GetKind() != egressCRKind(b.ResourceKind) || o.GetName() != b.ProviderName || o.GetNamespace() != b.Namespace || o.GetUID() == "" || labels[ownerLabel] != "ani-network-service" || labels[resourceLabel] != id || labels[bindingLabel] != b.BindingID || labels[tenantLabel] != b.TenantID || (uid != "" && string(o.GetUID()) != uid) || (b.ProviderUid != "" && string(o.GetUID()) != b.ProviderUid) {
		return &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	return nil
}

func crString(o *unstructured.Unstructured, fields ...string) string {
	if o == nil {
		return ""
	}
	v, _, _ := unstructured.NestedString(o.Object, fields...)
	return v
}

func crBool(o *unstructured.Unstructured, fields ...string) bool {
	if o == nil {
		return false
	}
	v, _, _ := unstructured.NestedBool(o.Object, fields...)
	return v
}

func crInt(o *unstructured.Unstructured, fields ...string) int64 {
	if o == nil {
		return 0
	}
	v, _, _ := unstructured.NestedInt64(o.Object, fields...)
	return v
}

func hasField(o *unstructured.Unstructured, fields ...string) bool {
	if o == nil {
		return false
	}
	_, ok, _ := unstructured.NestedFieldNoCopy(o.Object, fields...)
	return ok
}

// Only fields supplied by Res are compared for mutation decisions. Defaults
// and controller-owned fields are not a second readiness contract.
func managedFieldsMatch(actual, desired any) bool {
	if wanted, ok := desired.(map[string]any); ok {
		got, _ := actual.(map[string]any)
		for key, value := range wanted {
			if !managedFieldsMatch(got[key], value) {
				return false
			}
		}
		return true
	}
	if wanted, ok := desired.([]any); ok {
		got, _ := actual.([]any)
		if len(got) != len(wanted) {
			return false
		}
		for i := range wanted {
			if !managedFieldsMatch(got[i], wanted[i]) {
				return false
			}
		}
		return true
	}
	if actual == nil {
		switch desired {
		case false, "", int64(0):
			return true
		}
	}
	return reflect.DeepEqual(actual, desired)
}

func snatPhase(o *unstructured.Unstructured, desired bool) (*bool, *bool, biz.Reason) {
	var applied *bool
	switch crString(o, "status", "phase") {
	case "Bound":
		value := true
		applied = &value
	case "Disabled":
		value := false
		applied = &value
	case "Pending":
		return nil, nil, biz.ProviderNotReady
	default:
		return nil, nil, biz.Reason("PROVIDER_STATUS_UNREPORTED")
	}
	if *applied != desired {
		return nil, applied, biz.ProviderNotReady
	}
	ready := true
	return &ready, applied, ""
}

func inspectEgressObject(r egressResolved, object *unstructured.Unstructured) (biz.ProviderObservation, error) {
	out := biz.ProviderObservation{Identity: r.target.KnownIdentity}
	if object == nil {
		return out, nil
	}
	known := r.target.KnownIdentity
	if known == "" {
		known = r.binding.ProviderUid
	}
	if r.target.Deleting && known != "" && string(object.GetUID()) != known {
		out.Identity = known
		return out, nil // The original object is gone. Never delete its replacement.
	}
	if err := inspectEgressIdentity(object, r.binding, r.target.ResourceID, known); err != nil {
		return out, err
	}
	out.Exists, out.Identity = true, string(object.GetUID())
	switch r.target.Kind {
	case "snat":
		if r.target.Egress == nil {
			return out, &biz.ProviderError{Kind: biz.ProviderConflict}
		}
		var applied *bool
		out.Ready, applied, out.StatusReason = snatPhase(object, r.target.Egress.DesiredEnabled)
		out.NeedsUpdate = crBool(object, "spec", "disable") == r.target.Egress.DesiredEnabled
		out.Egress = &biz.EgressAppliedFacts{Address: crString(object, "status", "eipAddress"), AppliedEnabled: applied}
	case "eip":
		out.Ready, out.StatusReason = primaryCondition(object, "Ready")
		out.Egress = &biz.EgressAppliedFacts{Address: crString(object, "spec", "ipAddress")}
	case "vlan":
		out.Ready, out.StatusReason = primaryCondition(object, "Initialized")
	default:
		out.Ready, out.StatusReason = primaryCondition(object, "Ready")
	}
	return out, nil
}

func (p *KCProvider) observeEgress(ctx context.Context, t biz.ProviderTarget) (biz.ProviderObservation, error) {
	binding, err := p.egressBinding(ctx, t)
	if err != nil {
		return biz.ProviderObservation{}, err
	}
	r := egressResolved{binding: binding, target: t}
	object, err := resourceEndpoint(p.client, binding).Get(ctx, binding.ProviderName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return inspectEgressObject(r, nil)
	}
	if err != nil {
		return biz.ProviderObservation{}, readFailure(err)
	}
	return inspectEgressObject(r, object)
}

func (p *KCProvider) EnsureEgress(ctx context.Context, t biz.ProviderTarget) (biz.ProviderObservation, error) {
	r, err := p.resolveEgress(ctx, t)
	if err != nil {
		return biz.ProviderObservation{}, err
	}
	if t.KnownIdentity != "" || r.binding.ProviderUid != "" {
		return biz.ProviderObservation{}, &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	if t.TenantID != "" {
		if err = p.verifyTenantNamespace(ctx, r.binding, true); err != nil {
			return biz.ProviderObservation{}, err
		}
	}
	endpoint := resourceEndpoint(p.client, r.binding)
	object, err := endpoint.Get(ctx, r.binding.ProviderName, metav1.GetOptions{})
	if err == nil {
		return inspectEgressObject(r, object)
	}
	if !apierrors.IsNotFound(err) {
		return biz.ProviderObservation{}, readFailure(err)
	}
	object, err = endpoint.Create(ctx, egressObject(r), metav1.CreateOptions{FieldManager: "ani-network-service", FieldValidation: "Strict"})
	if apierrors.IsAlreadyExists(err) {
		object, err = endpoint.Get(ctx, r.binding.ProviderName, metav1.GetOptions{})
		if err != nil {
			return biz.ProviderObservation{}, readFailure(err)
		}
	} else if err != nil {
		return biz.ProviderObservation{}, mutationFailure(err)
	}
	observed, err := inspectEgressObject(r, object)
	if err != nil {
		return observed, &biz.ProviderError{Kind: biz.ProviderUncertain, Cause: err}
	}
	return observed, nil
}

func (p *KCProvider) UpdateEgress(ctx context.Context, t biz.ProviderTarget) (biz.ProviderObservation, error) {
	if t.Kind != "snat" || t.Egress == nil || t.KnownIdentity == "" {
		return biz.ProviderObservation{}, &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	binding, err := p.egressBinding(ctx, t)
	if err != nil {
		return biz.ProviderObservation{}, err
	}
	r := egressResolved{binding: binding, target: t}
	endpoint := resourceEndpoint(p.client, binding)
	object, err := endpoint.Get(ctx, binding.ProviderName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return inspectEgressObject(r, nil)
	}
	if err != nil {
		return biz.ProviderObservation{}, readFailure(err)
	}
	if err = inspectEgressIdentity(object, binding, t.ResourceID, t.KnownIdentity); err != nil {
		return biz.ProviderObservation{}, err
	}
	if crBool(object, "spec", "disable") == t.Egress.DesiredEnabled {
		patch, _ := json.Marshal([]map[string]any{
			{"op": "test", "path": "/metadata/uid", "value": t.KnownIdentity},
			{"op": "test", "path": "/metadata/resourceVersion", "value": object.GetResourceVersion()},
			{"op": "add", "path": "/spec/disable", "value": !t.Egress.DesiredEnabled},
		})
		object, err = endpoint.Patch(ctx, binding.ProviderName, types.JSONPatchType, patch, metav1.PatchOptions{FieldManager: "ani-network-service", FieldValidation: "Strict"})
		if err != nil {
			return biz.ProviderObservation{}, mutationFailure(err)
		}
	}
	return inspectEgressObject(r, object)
}

func (p *KCProvider) deleteEgress(ctx context.Context, t biz.ProviderTarget) error {
	binding, err := p.egressBinding(ctx, t)
	if err != nil {
		return err
	}
	endpoint := resourceEndpoint(p.client, binding)
	object, err := endpoint.Get(ctx, binding.ProviderName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return readFailure(err)
	}
	known := t.KnownIdentity
	if known == "" {
		known = binding.ProviderUid
	}
	if known != "" && string(object.GetUID()) != known {
		return nil
	}
	if err = inspectEgressIdentity(object, binding, t.ResourceID, known); err != nil {
		return err
	}
	uid, rv := object.GetUID(), object.GetResourceVersion()
	err = endpoint.Delete(ctx, binding.ProviderName, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv}})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return mutationFailure(err)
	}
	return nil
}
