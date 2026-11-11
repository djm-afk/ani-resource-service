package data

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

var kcManualVNics = schema.GroupVersionResource{Group: "networking.kubercloud.com", Version: "v1", Resource: "vnics"}
var kcManualVNicIPs = schema.GroupVersionResource{Group: "networking.kubercloud.com", Version: "v1", Resource: "vnicips"}

const requestedVNicIPUIDAnnotation = "network.ani.io/requested-ip-uid"

func (p *KCProvider) inspectVNic(ctx context.Context, object *unstructured.Unstructured, binding sqlcgen.NetworkProviderBinding, target biz.ProviderTarget) (biz.ProviderObservation, error) {
	exists, err := managedObjectExists(object, binding, target, "VNic")
	if err != nil || !exists {
		return biz.ProviderObservation{}, err
	}
	ready, reason := vnicAddressConditions(object)
	return biz.ProviderObservation{
		Exists: true, Identity: string(object.GetUID()), Ready: ready, StatusReason: reason,
		VNic:        vnicFacts(object),
		NeedsUpdate: !target.Deleting && target.RequestedIP != "" && object.GetAnnotations()[requestedVNicIPUIDAnnotation] == "",
	}, nil
}

func vnicAddressConditions(object *unstructured.Unstructured) (*bool, biz.Reason) {
	return primaryCondition(object, "Ready")
}

func vnicFacts(object *unstructured.Unstructured) *biz.VNicFacts {
	mac, _, _ := unstructured.NestedString(object.Object, "spec", "macAddress")
	primary, _, _ := unstructured.NestedString(object.Object, "status", "primaryIP")
	values, _, _ := unstructured.NestedStringMap(object.Object, "status", "ips")
	ips := make([]string, 0, len(values)+1)
	for _, address := range values {
		if address != "" {
			ips = append(ips, address)
		}
	}
	if primary != "" {
		ips = append(ips, primary)
	}
	sort.Strings(ips)
	return &biz.VNicFacts{MACAddress: mac, PrimaryIP: primary, IPs: slices.Compact(ips)}
}

func inspectRequestedVNicIP(child, parent *unstructured.Unstructured, b sqlcgen.NetworkProviderBinding, t biz.ProviderTarget) error {
	l := child.GetLabels()
	if child.GetAPIVersion() != "networking.kubercloud.com/v1" || child.GetKind() != "VNicIP" || child.GetNamespace() != b.Namespace || child.GetName() != b.ProviderName || child.GetUID() == "" || child.GetResourceVersion() == "" || l[ownerLabel] != "ani-network-service" || l[tenantLabel] != t.TenantID || l[resourceLabel] != t.ResourceID || l[bindingLabel] != b.BindingID {
		return &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	owners := child.GetOwnerReferences()
	if len(owners) != 1 || owners[0].APIVersion != "networking.kubercloud.com/v1" || owners[0].Kind != "VNic" || owners[0].Name != b.ProviderName || owners[0].UID != parent.GetUID() || (owners[0].Controller != nil && *owners[0].Controller) {
		return &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	known := parent.GetAnnotations()[requestedVNicIPUIDAnnotation]
	if known != "" && known != string(child.GetUID()) {
		return &biz.ProviderError{Kind: biz.ProviderConflict}
	}

	return nil
}

func manualVNicObject(b sqlcgen.NetworkProviderBinding, t biz.ProviderTarget, subnet string) *unstructured.Unstructured {
	annotations := map[string]any{}
	if t.RequestedIP == "" {
		annotations["networking.kubercloud.com/allocate_ip_with_dhcp"] = "false"
	}
	spec := map[string]any{"type": "VETH", "subnet": subnet}
	if t.BasicIsolationName != "" {
		spec["isolationGroup"] = t.BasicIsolationName
	}
	if t.RequestedMAC != "" {
		spec["macAddress"] = t.RequestedMAC
	}
	return &unstructured.Unstructured{Object: map[string]any{"apiVersion": "networking.kubercloud.com/v1", "kind": "VNic", "metadata": map[string]any{"name": b.ProviderName, "namespace": b.Namespace, "labels": map[string]any{ownerLabel: "ani-network-service", tenantLabel: t.TenantID, resourceLabel: t.ResourceID, bindingLabel: b.BindingID, "network.ani.io/vnic-id": t.ResourceID}, "annotations": annotations}, "spec": spec}}
}

func (p *KCProvider) ensureRequestedVNicIP(ctx context.Context, parent *unstructured.Unstructured, b sqlcgen.NetworkProviderBinding, t biz.ProviderTarget) (*unstructured.Unstructured, error) {
	if t.RequestedIP == "" || parent.GetAnnotations()[requestedVNicIPUIDAnnotation] != "" {
		return parent, nil
	}
	if parent.GetUID() == "" || parent.GetResourceVersion() == "" || parent.GetDeletionTimestamp() != nil {
		return nil, &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	r := p.client.Resource(kcManualVNicIPs).Namespace(b.Namespace)
	child, err := r.Get(ctx, b.ProviderName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if parent.GetAnnotations()[requestedVNicIPUIDAnnotation] != "" || parent.GetDeletionTimestamp() != nil {
			return nil, &biz.ProviderError{Kind: biz.ProviderConflict}
		}
		subnet, _, _ := unstructured.NestedString(parent.Object, "spec", "subnet")
		child = &unstructured.Unstructured{Object: map[string]any{"apiVersion": "networking.kubercloud.com/v1", "kind": "VNicIP", "metadata": map[string]any{"name": b.ProviderName, "namespace": b.Namespace, "labels": map[string]any{ownerLabel: "ani-network-service", tenantLabel: t.TenantID, resourceLabel: t.ResourceID, bindingLabel: b.BindingID}, "ownerReferences": []any{map[string]any{"apiVersion": "networking.kubercloud.com/v1", "kind": "VNic", "name": b.ProviderName, "uid": string(parent.GetUID())}}}, "spec": map[string]any{"subnet": subnet, "vNic": b.ProviderName, "ipVersion": "IPv4", "ipAddress": t.RequestedIP, "dhcp": false}}}
		child, err = r.Create(ctx, child, metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			child, err = r.Get(ctx, b.ProviderName, metav1.GetOptions{})
		}
		if err != nil {
			return nil, mutationFailure(err)
		}
	} else if err != nil {
		return nil, readFailure(err)
	}
	if err := inspectRequestedVNicIP(child, parent, b, t); err != nil {
		return nil, err
	}
	if parent.GetAnnotations()[requestedVNicIPUIDAnnotation] != "" {
		return parent, nil
	}
	annotations := parent.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[requestedVNicIPUIDAnnotation] = string(child.GetUID())
	patch, _ := json.Marshal([]map[string]any{{"op": "test", "path": "/metadata/uid", "value": string(parent.GetUID())}, {"op": "test", "path": "/metadata/resourceVersion", "value": parent.GetResourceVersion()}, {"op": "add", "path": "/metadata/annotations", "value": annotations}})
	parentUID, parentVersion := parent.GetUID(), parent.GetResourceVersion()
	updated, err := p.client.Resource(kcManualVNics).Namespace(b.Namespace).Patch(ctx, b.ProviderName, types.JSONPatchType, patch, metav1.PatchOptions{})
	if err != nil {
		// A failed JSON-patch test is returned as 422 by Kubernetes. Confirm
		// whether KCN only advanced the same parent's version before treating
		// it as a permanently invalid address request.
		if apierrors.IsInvalid(err) || apierrors.IsConflict(err) {
			current, readErr := p.client.Resource(kcManualVNics).Namespace(b.Namespace).Get(ctx, b.ProviderName, metav1.GetOptions{})
			if apierrors.IsNotFound(readErr) || (readErr == nil && current.GetUID() != parentUID) {
				return nil, &biz.ProviderError{Kind: biz.ProviderConflict, Cause: err}
			}
			if readErr != nil {
				return nil, readFailure(readErr)
			}
			if current.GetResourceVersion() != parentVersion {
				return nil, &biz.ProviderError{Kind: biz.ProviderTemporary, Cause: err}
			}
		}
		return nil, mutationFailure(err)
	}
	return updated, nil
}
func (p *KCProvider) EnsureVNic(ctx context.Context, t biz.ProviderTarget) (biz.ProviderObservation, error) {
	b, e := p.binding(ctx, t)
	if e != nil {
		return biz.ProviderObservation{}, e
	}
	if e = p.verifyTenantNamespace(ctx, b, true); e != nil {
		return biz.ProviderObservation{}, e
	}
	r := p.client.Resource(kcManualVNics).Namespace(b.Namespace)
	existing, e := r.Get(ctx, b.ProviderName, metav1.GetOptions{})
	if e == nil {
		observed, err := p.inspectVNic(ctx, existing, b, t)
		if err != nil || !observed.NeedsUpdate {
			return observed, err
		}
		existing, err = p.ensureRequestedVNicIP(ctx, existing, b, t)
		if err != nil {
			return observed, err
		}
		return p.inspectVNic(ctx, existing, b, t)
	}
	if !apierrors.IsNotFound(e) {
		return biz.ProviderObservation{}, readFailure(e)
	}
	if b.ProviderUid != "" || t.KnownIdentity != "" {
		return biz.ProviderObservation{}, &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	row, e := p.repository.queries.GetVNicRow(ctx, sqlcgen.GetVNicRowParams{TenantID: t.TenantID, VnicID: t.ResourceID})
	if e != nil {
		return biz.ProviderObservation{}, readFailure(e)
	}
	reference := normalizeNetworkReference(row.VpcID, row.SubnetID, row.PlatformVpcID, row.PlatformSubnetID, "")
	var subnetRef string
	if reference.OwnerScope == biz.OwnerScopePlatform {
		selected, err := p.repository.ResolveAcceptedNetworkSelection(ctx, biz.NetworkSelection{TenantID: t.TenantID, VPCID: reference.VPCID, SubnetID: reference.SubnetID, ClusterID: b.ClusterID, Namespace: b.Namespace, Reference: reference})
		if err != nil {
			return biz.ProviderObservation{}, err
		}
		t.BasicIsolationName, e = p.EnsureTenantBasicIsolation(ctx, t.TenantID)
		if e != nil {
			return biz.ProviderObservation{}, e
		}
		subnetRef = selected.NativeNamespace + "/" + selected.NativeName
	} else {
		sb, err := p.repository.queries.GetBinding(ctx, sqlcgen.GetBindingParams{TenantID: t.TenantID, ResourceID: reference.SubnetID})
		if err != nil {
			return biz.ProviderObservation{}, readFailure(err)
		}
		if sb.Namespace != b.Namespace || sb.ClusterID != b.ClusterID || sb.ProviderName == "" {
			return biz.ProviderObservation{}, &biz.ProviderError{Kind: biz.ProviderConflict}
		}
		subnetRef = sb.Namespace + "/" + sb.ProviderName
	}
	o := manualVNicObject(b, t, subnetRef)
	o, e = r.Create(ctx, o, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(e) {
		o, e = r.Get(ctx, b.ProviderName, metav1.GetOptions{})
	}
	if e != nil {
		return biz.ProviderObservation{}, mutationFailure(e)
	}
	// The worker records this original UID before submitting Res's optional
	// RequestedIP declaration on its following turn.
	return p.inspectVNic(ctx, o, b, t)
}
func (p *KCProvider) deleteVNic(ctx context.Context, t biz.ProviderTarget) error {
	b, e := p.binding(ctx, t)
	if e != nil {
		return e
	}
	r := p.client.Resource(kcManualVNics).Namespace(b.Namespace)
	o, e := r.Get(ctx, b.ProviderName, metav1.GetOptions{})
	if apierrors.IsNotFound(e) {
		return nil
	}
	if e != nil {
		return readFailure(e)
	}
	t.Deleting = true
	observed, err := p.inspectVNic(ctx, o, b, t)
	if err != nil || !observed.Exists || o.GetDeletionTimestamp() != nil {
		return err
	}
	uid, rv := types.UID(observed.Identity), o.GetResourceVersion()
	e = r.Delete(ctx, b.ProviderName, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv}})
	if apierrors.IsNotFound(e) {
		return nil
	}
	if e != nil {
		return mutationFailure(e)
	}
	return nil
}

const vnicConsumerAnnotation = "network.ani.io/vnic-consumer"
const vnicSGVersionAnnotation = "network.ani.io/vnic-security-groups-version"

// Lock only Network's own selection and reservation. The transaction is
// committed before any provider I/O; no owner-domain SQL or provider lock spans.
func (p *KCProvider) lockVNicSGSelection(ctx context.Context, tx pgx.Tx, v biz.VNic, kind, consumer string, ids []string, initialize bool) (biz.SecurityGroupBinding, error) {
	q := p.repository.queries.WithTx(tx)
	if e := sgTenantLock(ctx, q, v.TenantID); e != nil {
		return biz.SecurityGroupBinding{}, e
	}
	if e := q.LockSecurityGroupRequest(ctx, sqlcgen.LockSecurityGroupRequestParams{TenantID: v.TenantID, Action: kind, RequestKey: consumer}); e != nil {
		return biz.SecurityGroupBinding{}, databaseFailure(e)
	}
	row, e := q.LockSecurityGroupTarget(ctx, sqlcgen.LockSecurityGroupTargetParams{TenantID: v.TenantID, TargetType: kind, TargetID: consumer})
	selection := biz.SecurityGroupBinding{}
	if errors.Is(e, pgx.ErrNoRows) && initialize && len(ids) == 0 {
		selection = biz.SecurityGroupBinding{TenantID: v.TenantID, TargetType: kind, TargetID: consumer, VPCID: v.VPCID, Namespace: v.Namespace, Version: 1, State: "configured", SecurityGroupIDs: []string{}}
		if e = sgSaveTarget(ctx, q, selection); e != nil {
			return selection, e
		}
	} else if e != nil {
		return selection, databaseFailure(e)
	} else {
		selection, e = sgTarget(row)
		if e != nil {
			return selection, e
		}
	}
	if selection.State == "released" || selection.VPCID != v.VPCID || selection.Namespace != v.Namespace || !sameStrings(selection.SecurityGroupIDs, ids) {
		return selection, biz.Fail(biz.VersionConflict, "consumer security group selection changed")
	}
	resource, e := q.LockVNicRow(ctx, sqlcgen.LockVNicRowParams{TenantID: v.TenantID, VnicID: v.ID})
	if e != nil {
		return selection, databaseFailure(e)
	}
	binding, e := q.GetBinding(ctx, sqlcgen.GetBindingParams{TenantID: v.TenantID, ResourceID: v.ID})
	if e != nil {
		return selection, databaseFailure(e)
	}
	if resource.State == "deleting" || resource.State == "deleted" || (v.ProviderUID != "" && binding.ProviderUid != "" && binding.ProviderUid != v.ProviderUID) || binding.ProviderName != v.ProviderName || binding.Namespace != v.Namespace {
		return selection, biz.Fail(biz.ResourceBusy, "manual VNIC identity is no longer available")
	}
	var active bool
	e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM network_vnic_bindings WHERE tenant_id=$1 AND vnic_id=$2 AND consumer_kind=$3 AND consumer_id=$4 AND released_at IS NULL)`, v.TenantID, v.ID, kind, consumer).Scan(&active)
	if e != nil {
		return selection, databaseFailure(e)
	}
	if !active {
		return selection, biz.Fail(biz.ResourceBusy, "VNIC reservation does not belong to consumer")
	}
	return selection, nil
}

func (p *KCProvider) syncVNicSecurityGroups(ctx context.Context, tenant, id, kind, consumer string, ids []string) (biz.VNic, error) {
	// No selection and an empty selection are the same VNIC fact. A caller may
	// pass a nil slice (a VM whose stored configuration has no security groups),
	// and the column below is NOT NULL, so an absent selection must never be
	// written as SQL NULL.
	if ids == nil {
		ids = []string{}
	}
	v, e := p.repository.GetVNic(ctx, tenant, id)
	if e != nil {
		return v, e
	}
	tx, e := p.repository.pool.Begin(ctx)
	if e != nil {
		return v, databaseFailure(e)
	}
	defer tx.Rollback(ctx)
	selection, e := p.lockVNicSGSelection(ctx, tx, v, kind, consumer, ids, true)
	if e != nil {
		return v, e
	}
	q := p.repository.queries.WithTx(tx)
	names, e := resolveSecurityGroups(ctx, q, tenant, v.ClusterID, v.Namespace, ids)
	if e != nil {
		return v, e
	}
	target := biz.SecurityGroupBinding{TenantID: tenant, TargetType: "vnic", TargetID: id, VPCID: v.VPCID, Namespace: v.Namespace, State: "applying", SecurityGroupIDs: slices.Clone(ids), Version: selection.Version}
	previous, readErr := q.LockSecurityGroupTarget(ctx, sqlcgen.LockSecurityGroupTargetParams{TenantID: tenant, TargetType: "vnic", TargetID: id})
	if readErr == nil {
		prior, err := sgTarget(previous)
		if err != nil {
			return v, err
		}
		target.AppliedSecurityGroupIDs = slices.Clone(prior.AppliedSecurityGroupIDs)
		target.ObservedAt = prior.ObservedAt
	} else if !errors.Is(readErr, pgx.ErrNoRows) {
		return v, databaseFailure(readErr)
	}
	if e = sgSaveTarget(ctx, q, target); e != nil {
		return v, e
	}
	for _, gid := range ids {
		if e = q.ReserveSecurityGroupReference(ctx, sqlcgen.ReserveSecurityGroupReferenceParams{TenantID: tenant, TargetType: "vnic", TargetID: id, SecurityGroupID: gid}); e != nil {
			return v, databaseFailure(e)
		}
	}
	if e = tx.Commit(ctx); e != nil {
		return v, databaseFailure(e)
	}
	r := p.client.Resource(kcManualVNics).Namespace(v.Namespace)
	o, e := r.Get(ctx, v.ProviderName, metav1.GetOptions{})
	if e != nil {
		return v, readFailure(e)
	}
	if (v.ProviderUID != "" && string(o.GetUID()) != v.ProviderUID) || o.GetLabels()[tenantLabel] != tenant || o.GetLabels()[resourceLabel] != id || o.GetLabels()[ownerLabel] != "ani-network-service" || o.GetDeletionTimestamp() != nil {
		return v, &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	consumerKey := kind + ":" + consumer
	version := strconv.FormatInt(selection.Version, 10)
	annotations := o.GetAnnotations()
	if annotations[vnicConsumerAnnotation] == consumerKey && annotations[vnicSGVersionAnnotation] != "" {
		previous, parseErr := strconv.ParseInt(annotations[vnicSGVersionAnnotation], 10, 64)
		if parseErr != nil || previous > selection.Version {
			return v, biz.Fail(biz.VersionConflict, "VNIC already declares a newer consumer security group selection")
		}
	}
	// A stalled worker must still own the same Network selection and reservation
	// immediately before its conditional provider request.
	check, e := p.repository.pool.Begin(ctx)
	if e != nil {
		return v, databaseFailure(e)
	}
	defer check.Rollback(ctx)
	current, e := p.lockVNicSGSelection(ctx, check, v, kind, consumer, ids, false)
	if e != nil {
		return v, e
	}
	if current.Version != selection.Version {
		return v, biz.Fail(biz.VersionConflict, "consumer security group version changed")
	}
	if e = check.Commit(ctx); e != nil {
		return v, databaseFailure(e)
	}
	actual, _, _ := unstructured.NestedStringSlice(o.Object, "spec", "securityGroups")
	if !sameStrings(actual, names) || annotations[vnicConsumerAnnotation] != consumerKey || annotations[vnicSGVersionAnnotation] != version {
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[vnicConsumerAnnotation] = consumerKey
		annotations[vnicSGVersionAnnotation] = version
		patch, _ := json.Marshal([]map[string]any{{"op": "test", "path": "/metadata/uid", "value": string(o.GetUID())}, {"op": "test", "path": "/metadata/resourceVersion", "value": o.GetResourceVersion()}, {"op": "add", "path": "/metadata/annotations", "value": annotations}, {"op": "add", "path": "/spec/securityGroups", "value": names}})
		if _, e = r.Patch(ctx, v.ProviderName, types.JSONPatchType, patch, metav1.PatchOptions{}); e != nil {
			return v, mutationFailure(e)
		}
	}
	tx, e = p.repository.pool.Begin(ctx)
	if e != nil {
		return v, databaseFailure(e)
	}
	defer tx.Rollback(ctx)
	current, e = p.lockVNicSGSelection(ctx, tx, v, kind, consumer, ids, false)
	if e != nil {
		return v, e
	}
	if current.Version != selection.Version {
		return v, biz.Fail(biz.VersionConflict, "consumer security group version changed before completion")
	}
	q = p.repository.queries.WithTx(tx)
	if _, e = tx.Exec(ctx, `UPDATE network_vnics SET security_group_ids=$3 WHERE tenant_id=$1 AND vnic_id=$2`, tenant, id, ids); e != nil {
		return v, databaseFailure(e)
	}
	target.State = "configured"
	if e = sgSaveTarget(ctx, q, target); e != nil {
		return v, e
	}
	if e = q.ReleaseSecurityGroupReferences(ctx, sqlcgen.ReleaseSecurityGroupReferencesParams{TenantID: tenant, TargetType: "vnic", TargetID: id, KeepIds: ids}); e != nil {
		return v, databaseFailure(e)
	}
	if e = q.ScheduleSecurityGroup(ctx, sqlcgen.ScheduleSecurityGroupParams{TenantID: tenant, TargetType: "vnic", TargetID: id, RequestedVersion: target.Version}); e != nil {
		return v, databaseFailure(e)
	}
	if e = tx.Commit(ctx); e != nil {
		return v, databaseFailure(e)
	}
	return p.repository.GetVNic(ctx, tenant, id)
}
