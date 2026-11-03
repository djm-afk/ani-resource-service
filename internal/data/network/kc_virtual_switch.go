package data

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

const virtualSwitchGroupPrefix = "vcluster.networking.kubercloud.com/"
const allocatedNodeLabel = "networking.kubercloud.com/allocated"

// The hash keeps the annotation name within Kubernetes' 63 character limit,
// including when a legal group name itself is 63 characters long.
func VirtualSwitchOwnerKey(group string) string {
	return "network.ani.io/virtual-switch-" + contentHash(group)[:32]
}

func virtualSwitchDeleteFenceKey(group string) string {
	return "network.ani.io/virtual-switch-fence-" + contentHash(group)[:32]
}

func virtualSwitchDeleteFenceValue(sw biz.VirtualSwitch) string {
	return sw.BindingID + ":" + strconv.FormatInt(sw.Version, 10)
}

func switchConflict(causes ...error) error {
	return &biz.ProviderError{Kind: biz.ProviderConflict, Cause: errors.Join(causes...)}
}
func switchUnavailable(causes ...error) error {
	return &biz.ProviderError{Kind: biz.ProviderTemporary, Cause: errors.Join(causes...)}
}

func (p *KCProvider) VirtualSwitchInventory(ctx context.Context, cluster string, sw biz.VirtualSwitch, device string) (biz.VirtualSwitchInventory, error) {
	if cluster == "" || cluster != p.repository.placement.ClusterID || (sw.ClusterID != "" && sw.ClusterID != cluster) || device == "" {
		return biz.VirtualSwitchInventory{}, switchConflict()
	}
	nodes, maps, vlans, collected, err := p.nodeNetworkObjects(ctx)
	if err != nil {
		return biz.VirtualSwitchInventory{}, err
	}
	daemons, err := p.listAll(ctx, pods)
	if err != nil {
		return biz.VirtualSwitchInventory{}, err
	}
	cm, err := p.client.Resource(kcConfigMaps).Namespace(kcSystemNamespace).Get(ctx, "kcn-config", metav1.GetOptions{})
	if err != nil {
		return biz.VirtualSwitchInventory{}, readFailure(err)
	}
	now, err := p.repository.readQueries(ctx).DatabaseTime(ctx)
	if err != nil {
		return biz.VirtualSwitchInventory{}, readFailure(err)
	}
	return virtualSwitchInventory(sw, device, nodes, maps, vlans, daemons, cm, now, collected)
}

// Freeze every allocated node and the administrator's device intent. KCN owns
// physical device validation and takeover; Resource only validates this binding.
func virtualSwitchInventory(sw biz.VirtualSwitch, device string, nodes, maps, vlans, daemons []unstructured.Unstructured, config *unstructured.Unstructured, now, collected time.Time) (biz.VirtualSwitchInventory, error) {
	return virtualSwitchInventoryMode(sw, device, nodes, maps, vlans, daemons, config, now, collected, true)
}

func virtualSwitchInventoryMode(sw biz.VirtualSwitch, device string, nodes, _, vlans, daemons []unstructured.Unstructured, config *unstructured.Unstructured, _, collected time.Time, requireHealthyMembers bool) (biz.VirtualSwitchInventory, error) {
	result := biz.VirtualSwitchInventory{Inventory: biz.PreparedDeviceInventory{CollectedAt: collected}}
	if device == "" || config == nil || config.GetUID() == "" || config.GetResourceVersion() == "" || config.GetDeletionTimestamp() != nil || collected.IsZero() {
		return result, switchUnavailable()
	}
	if config.GetAnnotations()[adoptionMarker(device)] != "" {
		return result, switchConflict()
	}
	if err := validateSwitchMembers(sw, nodes); err != nil {
		return result, err
	}
	for _, vlan := range vlans {
		if crString(&vlan, "spec", "vClusterName") == sw.GroupName && (sw.BindingID == "" || vlan.GetLabels()[ownerLabel] != "ani-network-service" || vlan.GetLabels()[resourceLabel] == "") {
			return result, switchConflict()
		}
	}
	for _, n := range nodes {
		if n.GetLabels()[allocatedNodeLabel] != "true" {
			continue
		}
		if n.GetDeletionTimestamp() != nil {
			return result, switchConflict()
		}
		if requireHealthyMembers && (!nodeReady(&n) || n.GetAnnotations()["networking.kubercloud.com/chassis"] == "" || !switchDaemonReady(n.GetName(), daemons)) {
			return result, switchUnavailable()
		}
		uid := string(n.GetUID())
		if uid == "" || n.GetResourceVersion() == "" {
			return result, switchConflict()
		}
		v := biz.PreparedDeviceNode{NodeName: n.GetName(), NodeUID: uid, Name: device}
		for _, vlan := range vlans {
			if crString(&vlan, "spec", "devName") != device {
				continue
			}
			if sw.BindingID == "" || crString(&vlan, "spec", "vClusterName") != sw.GroupName || vlan.GetLabels()[ownerLabel] != "ani-network-service" || vlan.GetLabels()[resourceLabel] == "" {
				return result, switchConflict()
			}
		}
		result.Inventory.Items = append(result.Inventory.Items, v)
		result.Members = append(result.Members, biz.VirtualSwitchNode{NodeName: n.GetName(), NodeUID: uid, Priority: 100, Allocated: true, ResourceVersion: n.GetResourceVersion()})
	}
	if len(result.Members) == 0 {
		return result, switchUnavailable()
	}
	slices.SortFunc(result.Members, func(a, b biz.VirtualSwitchNode) int { return strings.Compare(a.NodeUID, b.NodeUID) })
	slices.SortFunc(result.Inventory.Items, func(a, b biz.PreparedDeviceNode) int { return strings.Compare(a.NodeUID, b.NodeUID) })
	stable := slices.Clone(result.Members)
	for i := range stable {
		stable[i].ResourceVersion = ""
	}
	result.Fingerprint = contentHash(stable)
	result.Inventory.Fingerprint = contentHash(result.Inventory.Items)
	return result, nil
}

func (p *KCProvider) virtualSwitchDeviceState(ctx context.Context, sw biz.VirtualSwitch, device sqlcgen.NetworkDeviceAdoption, view *auditView, preparePhysical bool, chassisPool *unstructured.Unstructured) (bool, bool, biz.ObservationProof, error) {
	var proof biz.ObservationProof
	var nodes, maps, vlans, daemons []unstructured.Unstructured
	var collected time.Time
	var err error
	if view == nil {
		nodes, maps, vlans, collected, err = p.nodeNetworkObjects(ctx)
		if err == nil {
			daemons, err = p.listAll(ctx, pods)
		}
	} else {
		// A dependent check must consume the caller's complete collection.
		// Starting a second audit can invalidate the parent and starve its proof.
		if p.observation == nil || !p.observation.valid(view, []string{"node-network"}) || view.indices[pods] == nil {
			return false, false, proof, errEgressAuditInvalid
		}
		nodes, maps, vlans, collected, err = nodeNetworkFromView(view)
		for _, raw := range view.indices[pods].List() {
			o := raw.(*unstructured.Unstructured)
			if o.GetNamespace() == kcSystemNamespace && o.GetLabels()["networking.kubercloud.com/app"] == "cni-ds" {
				daemons = append(daemons, *o.DeepCopy())
			}
		}
	}
	if err != nil {
		return false, false, proof, err
	}
	cm, err := p.client.Resource(kcConfigMaps).Namespace(kcSystemNamespace).Get(ctx, "kcn-config", metav1.GetOptions{})
	if err != nil {
		return false, false, proof, readFailure(err)
	}
	now, err := p.repository.readQueries(ctx).DatabaseTime(ctx)
	if err != nil {
		return false, false, proof, readFailure(err)
	}
	current, err := virtualSwitchInventoryMode(sw, device.DeviceName, nodes, maps, vlans, daemons, cm, now, collected, preparePhysical)
	if err != nil {
		return false, false, proof, err
	}
	configured, wired, err := inspectVirtualSwitchDeviceState(sw, &current, device, nodes)
	if err != nil {
		return false, false, proof, err
	}
	// Existing configuration keeps the complete frozen group and its KCN
	// acknowledgements. Runtime health belongs to the actual selected exit,
	// rather than every configured member of a redundant group.
	if !preparePhysical && chassisPool != nil {
		wired = wired && publicPoolChassisReady(chassisPool, nodes, daemons)
	}
	proof = virtualSwitchObservationProof(sw, current.Inventory, collected)
	// Include KCN's Node acknowledgement and binding ownership in this proof.
	proof.Hash = contentHash([]string{proof.Hash, contentHash(nodes)})
	return configured, wired, proof, nil
}

func publicPoolChassisReady(pool *unstructured.Unstructured, nodes, daemons []unstructured.Unstructured) bool {
	selected := strings.Split(crString(pool, "status", "underlayState", "chassisNode"), ",")
	if len(selected) == 0 || len(selected) > 2 {
		return false
	}
	seen := map[string]bool{}
	for _, raw := range selected {
		name := strings.TrimSpace(raw)
		if name == "" || seen[name] {
			return false
		}
		seen[name] = true
		found := false
		for i := range nodes {
			n := &nodes[i]
			if n.GetName() != name {
				continue
			}
			// validateSwitchMembers and inspectVirtualSwitchDeviceState have
			// already checked this exact UID, owner, priority and device ack.
			if n.GetLabels()[allocatedNodeLabel] != "true" || !nodeReady(n) || n.GetAnnotations()["networking.kubercloud.com/chassis"] == "" || !switchDaemonReady(name, daemons) {
				return false
			}
			found = true
		}
		if !found {
			return false
		}
	}
	return true
}

func inspectVirtualSwitchDeviceState(sw biz.VirtualSwitch, current *biz.VirtualSwitchInventory, device sqlcgen.NetworkDeviceAdoption, nodes []unstructured.Unstructured) (bool, bool, error) {
	var frozen []biz.PreparedDeviceNode
	if sw.BindingID == "" || json.Unmarshal(device.NodeInventory, &frozen) != nil || len(frozen) == 0 || len(frozen) != len(current.Inventory.Items) {
		return false, false, switchConflict()
	}
	identities := map[string]biz.PreparedDeviceNode{}
	for _, v := range frozen {
		if v.NodeUID == "" || v.NodeName == "" || v.Name != device.DeviceName || identities[v.NodeUID].NodeUID != "" {
			return false, false, switchConflict()
		}
		identities[v.NodeUID] = v
	}
	byUID := map[string]*unstructured.Unstructured{}
	for i := range nodes {
		byUID[string(nodes[i].GetUID())] = &nodes[i]
	}
	configured, wired := sw.BindingState == "configured", true
	for _, v := range current.Inventory.Items {
		expected, ok := identities[v.NodeUID]
		if !ok || expected.NodeName != v.NodeName || expected.Name != v.Name {
			return false, false, switchConflict()
		}
		n := byUID[v.NodeUID]
		if n == nil || n.GetName() != v.NodeName {
			return false, false, switchConflict()
		}
		owned := n.GetAnnotations()[VirtualSwitchOwnerKey(sw.GroupName)] == sw.BindingID && n.GetLabels()[virtualSwitchGroupPrefix+sw.GroupName] == "100"
		configured = configured && owned
		wired = wired && owned && switchWiring(n, v, sw.GroupName)
	}
	return configured, wired && configured, nil
}

func virtualSwitchObservationProof(sw biz.VirtualSwitch, inventory biz.PreparedDeviceInventory, collected time.Time) biz.ObservationProof {
	// This proves the complete KCN/Node collection, not physical NIC readiness.
	return biz.ObservationProof{CollectedAt: inventory.CollectedAt, CollectionStartedAt: collected, Hash: contentHash(struct {
		SwitchID, BindingID, Fingerprint string
		Version                          int64
		Inventory                        biz.PreparedDeviceInventory
	}{sw.ID, sw.BindingID, sw.MembershipFingerprint, sw.BindingVersion, inventory})}
}

func switchWiring(n *unstructured.Unstructured, v biz.PreparedDeviceNode, group string) bool {
	return n != nil && v.Name != "" && slices.Contains(strings.FieldsFunc(n.GetAnnotations()[virtualSwitchGroupPrefix+group], func(r rune) bool { return r == ',' || r == ' ' }), v.Name)
}

func nodeReady(n *unstructured.Unstructured) bool {
	if n == nil || n.GetDeletionTimestamp() != nil {
		return false
	}
	conditions, _, _ := unstructured.NestedSlice(n.Object, "status", "conditions")
	for _, raw := range conditions {
		c, ok := raw.(map[string]any)
		if ok && c["type"] == "Ready" {
			return c["status"] == "True"
		}
	}
	return false
}

func switchDaemonReady(node string, daemons []unstructured.Unstructured) bool {
	count := 0
	for _, p := range daemons {
		if p.GetNamespace() != kcSystemNamespace || p.GetLabels()["networking.kubercloud.com/app"] != "cni-ds" || crString(&p, "spec", "nodeName") != node {
			continue
		}
		count++
		if !nodeReady(&p) || crString(&p, "status", "phase") != "Running" {
			return false
		}
		statuses, _, _ := unstructured.NestedSlice(p.Object, "status", "containerStatuses")
		containers, _, _ := unstructured.NestedSlice(p.Object, "spec", "containers")
		if len(statuses) == 0 || len(statuses) != len(containers) {
			return false
		}
		for _, raw := range statuses {
			s, ok := raw.(map[string]any)
			if !ok || s["ready"] != true {
				return false
			}
		}
	}
	return count == 1
}

func validateSwitchMembers(sw biz.VirtualSwitch, nodes []unstructured.Unstructured) error {
	frozen := map[string]biz.VirtualSwitchNode{}
	for _, m := range sw.Nodes {
		if m.NodeUID == "" || frozen[m.NodeUID].NodeUID != "" {
			return switchConflict()
		}
		frozen[m.NodeUID] = m
	}
	seen := map[string]bool{}
	for _, n := range nodes {
		uid := string(n.GetUID())
		allocated := n.GetLabels()[allocatedNodeLabel] == "true"
		_, hasLabel := n.GetLabels()[virtualSwitchGroupPrefix+sw.GroupName]
		_, hasOwner := n.GetAnnotations()[VirtualSwitchOwnerKey(sw.GroupName)]
		hasFeedback := strings.TrimSpace(n.GetAnnotations()[virtualSwitchGroupPrefix+sw.GroupName]) != ""
		if hasLabel || hasOwner || hasFeedback {
			m, ok := frozen[uid]
			if !ok || !allocated || m.NodeName != n.GetName() || sw.BindingID == "" || n.GetAnnotations()[VirtualSwitchOwnerKey(sw.GroupName)] != sw.BindingID || n.GetLabels()[virtualSwitchGroupPrefix+sw.GroupName] != "100" {
				return switchConflict()
			}
		}
		if !allocated {
			continue
		}
		if uid == "" || seen[uid] {
			return switchConflict()
		}
		if len(frozen) > 0 {
			m, ok := frozen[uid]
			if !ok || m.NodeName != n.GetName() || !m.Allocated || m.Priority != 100 {
				return switchConflict()
			}
		}
		seen[uid] = true
	}
	if len(frozen) > 0 && len(seen) != len(frozen) {
		return switchConflict()
	}
	return nil
}

func (p *KCProvider) InspectVirtualSwitchNode(ctx context.Context, sw biz.VirtualSwitch, member biz.VirtualSwitchNode) (biz.VirtualSwitchNode, error) {
	if sw.ClusterID != p.repository.placement.ClusterID || sw.BindingID == "" {
		return member, switchConflict(fmt.Errorf("virtual switch %s cluster or binding identity invalid", sw.ID))
	}
	var nodes []unstructured.Unstructured
	var collected time.Time
	var err error
	if p.observation != nil && sw.State == biz.Available && sw.BindingState == "configured" && member.Prepared && member.Applied && !member.Unknown {
		// Stable binding observation needs the frozen node identities, not a
		// new cluster-wide collection after every node-probe ConfigMap update.
		// Node changes and source boundaries still invalidate this proof;
		// unresolved mutations and deletion retain current collection below.
		var now time.Time
		now, err = p.repository.readQueries(ctx).DatabaseTime(ctx)
		if err != nil {
			return member, readFailure(err)
		}
		view, readErr := p.observation.cachedSnapshot(now)
		if readErr != nil {
			return member, readFailure(readErr)
		}
		if !p.observation.valid(view, []string{"node-membership"}) {
			return member, switchUnavailable(fmt.Errorf("switch node observation awaits shared audit"))
		}
		nodes, _, _, collected, err = nodeNetworkFromView(view)
	} else {
		nodes, _, _, collected, err = p.nodeNetworkObjects(ctx)
	}
	if err != nil {
		return member, err
	}
	if sw.State != biz.Deleting {
		if err = validateSwitchMembers(sw, nodes); err != nil {
			return member, switchConflict(fmt.Errorf("virtual switch %s frozen membership changed: %w", sw.ID, err))
		}
		now, err := p.repository.readQueries(ctx).DatabaseTime(ctx)
		if err != nil {
			return member, readFailure(err)
		}
		if collected.IsZero() || collected.After(now) {
			return member, switchUnavailable(fmt.Errorf("virtual switch %s has invalid observation time %s", sw.ID, collected))
		}
		if !member.LastObservedAt.IsZero() && collected.Before(member.LastObservedAt) {
			return member, switchUnavailable(fmt.Errorf("virtual switch %s node %s observation moved backwards from %s to %s", sw.ID, member.NodeName, member.LastObservedAt, collected))
		}
	}
	for _, n := range nodes {
		if n.GetName() == member.NodeName {
			current, err := inspectSwitchNodeObject(sw, member, &n)
			if err != nil {
				return current, switchConflict(fmt.Errorf("virtual switch %s node %s identity, owner or group configuration mismatch: %w", sw.ID, member.NodeName, err))
			}
			if err == nil && sw.State != biz.Deleting && current.Applied {
				current.LastObservedAt = collected
			}
			if err != nil || sw.State != biz.Deleting || !current.DeleteFenced || current.Applied {
				return current, err
			}
			// Confirm native VLAN absence from a fresh complete API read. No
			// physical NIC or host OVS observation participates in product deletion.
			freshVlans, err := p.listAll(ctx, kcVlans)
			if err != nil {
				return current, err
			}
			freshNode, err := p.client.Resource(kcNodes).Get(ctx, member.NodeName, metav1.GetOptions{})
			if err != nil {
				return current, readFailure(err)
			}
			return inspectSwitchNodeCleanup(sw, current, freshNode, freshVlans)
		}
	}
	return member, switchConflict(fmt.Errorf("virtual switch %s member node %s missing", sw.ID, member.NodeName))
}

func inspectSwitchNodeCleanup(sw biz.VirtualSwitch, member biz.VirtualSwitchNode, node *unstructured.Unstructured, vlans []unstructured.Unstructured) (biz.VirtualSwitchNode, error) {
	current, err := inspectSwitchNodeObject(sw, member, node)
	if err != nil || current.Applied || !current.DeleteFenced {
		return current, err
	}
	clean, err := switchNodeCleanupConfirmed(sw, current, node, vlans)
	if err != nil || !clean {
		return current, err
	}
	current.State = "cleaned"
	if _, present := node.GetAnnotations()[virtualSwitchDeleteFenceKey(sw.GroupName)]; present {
		current.State = "unfencing"
	}
	return current, nil
}

func inspectSwitchNodeObject(sw biz.VirtualSwitch, member biz.VirtualSwitchNode, n *unstructured.Unstructured) (biz.VirtualSwitchNode, error) {
	if n == nil || n.GetName() != member.NodeName || string(n.GetUID()) != member.NodeUID || n.GetResourceVersion() == "" || n.GetDeletionTimestamp() != nil {
		return member, switchConflict()
	}
	label, present := n.GetLabels()[virtualSwitchGroupPrefix+sw.GroupName]
	owner, ownerPresent := n.GetAnnotations()[VirtualSwitchOwnerKey(sw.GroupName)]
	if sw.State == biz.Deleting {
		fence, fencePresent := n.GetAnnotations()[virtualSwitchDeleteFenceKey(sw.GroupName)]
		if fencePresent && (!member.Prepared || fence != virtualSwitchDeleteFenceValue(sw)) {
			return member, switchConflict()
		}
		if ownerPresent || present {
			if !member.Prepared || owner != sw.BindingID || !present || label != strconv.Itoa(int(member.Priority)) {
				return member, switchConflict()
			}
			member.Applied = true
		} else {
			member.Applied = false
		}
		member.State = "cleaning"
		member.ResourceVersion = n.GetResourceVersion()
		if fencePresent {
			// The exact marker proves a successful RV barrier even when its
			// response was lost. The worker saves this before removing it.
			member.DeleteFenced = true
			member.Unknown = false
		} else if member.DeleteFenced {
			// A previously persisted barrier also covers a lost unfence reply.
			member.Unknown = false
		}
		return member, nil
	}
	if owner == sw.BindingID && present && label == strconv.Itoa(int(member.Priority)) {
		if !member.Prepared {
			return member, switchConflict()
		}
		member.Applied = true
		member.State = "applied"
		member.Unknown = false
		member.ResourceVersion = n.GetResourceVersion()
		return member, nil
	}
	if ownerPresent || present {
		return member, switchConflict()
	}
	if member.Applied && !member.Unknown && sw.State != biz.Deleting {
		return member, switchConflict()
	}
	member.Applied = false
	member.State = "pending"
	member.Unknown = false
	member.ResourceVersion = n.GetResourceVersion()
	if !member.Prepared {
		member.OriginalPresent = false
		member.OriginalValue = ""
		member.OriginalOwner = ""
	}
	return member, nil
}

func jsonPointerKey(key string) string {
	return strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1")
}

func (p *KCProvider) PatchVirtualSwitchNode(ctx context.Context, sw biz.VirtualSwitch, member biz.VirtualSwitchNode, deleting bool) (biz.VirtualSwitchNode, error) {
	if !member.Prepared || member.ResourceVersion == "" || sw.BindingID == "" || sw.ClusterID != p.repository.placement.ClusterID || member.Priority != 100 || !member.Allocated || member.OriginalPresent || member.OriginalOwner != "" {
		return member, switchConflict()
	}
	current, err := p.repository.GetVirtualSwitch(ctx, sw.ID)
	if err != nil {
		return member, err
	}
	if current.BindingID != sw.BindingID || current.BindingVersion != sw.BindingVersion || current.Version != sw.Version || (current.State == biz.Deleting) != deleting || current.State == biz.Deleted {
		return member, switchConflict()
	}
	durable := false
	for _, saved := range current.Nodes {
		if saved.NodeUID == member.NodeUID && saved.NodeName == member.NodeName && saved.Prepared && saved.ResourceVersion == member.ResourceVersion && saved.OriginalPresent == member.OriginalPresent && saved.OriginalOwner == member.OriginalOwner && saved.OriginalValue == member.OriginalValue && (!deleting || (saved.DeleteFenced == member.DeleteFenced && saved.State == member.State)) {
			durable = true
			break
		}
	}
	if !durable {
		return member, switchConflict()
	}
	if !deleting {
		nodes, err := p.listAll(ctx, kcNodes)
		if err != nil {
			return member, err
		}
		if err = validateSwitchMembers(sw, nodes); err != nil {
			return member, err
		}
	}
	n, err := p.client.Resource(kcNodes).Get(ctx, member.NodeName, metav1.GetOptions{})
	if err != nil {
		return member, readFailure(err)
	}
	if string(n.GetUID()) != member.NodeUID || n.GetResourceVersion() != member.ResourceVersion {
		return member, switchConflict()
	}
	if deleting {
		return p.patchVirtualSwitchDeletionNode(ctx, sw, member, n)
	}
	labelKey, ownerKey := virtualSwitchGroupPrefix+sw.GroupName, VirtualSwitchOwnerKey(sw.GroupName)
	label, hasLabel := n.GetLabels()[labelKey]
	owner, ownerPresent := n.GetAnnotations()[ownerKey]
	patch := []map[string]any{{"op": "test", "path": "/metadata/uid", "value": member.NodeUID}, {"op": "test", "path": "/metadata/resourceVersion", "value": member.ResourceVersion}}
	if owner == sw.BindingID && hasLabel && label == "100" {
		member.Applied = true
		member.State = "applied"
		member.Unknown = false
		return member, nil
	}
	if ownerPresent || hasLabel {
		return member, switchConflict()
	}
	if n.GetLabels() == nil {
		patch = append(patch, map[string]any{"op": "add", "path": "/metadata/labels", "value": map[string]any{}})
	}
	if n.GetAnnotations() == nil {
		patch = append(patch, map[string]any{"op": "add", "path": "/metadata/annotations", "value": map[string]any{}})
	}
	patch = append(patch, map[string]any{"op": "add", "path": "/metadata/labels/" + jsonPointerKey(labelKey), "value": "100"}, map[string]any{"op": "add", "path": "/metadata/annotations/" + jsonPointerKey(ownerKey), "value": sw.BindingID})
	body, _ := json.Marshal(patch)
	updated, err := p.client.Resource(kcNodes).Patch(ctx, member.NodeName, types.JSONPatchType, body, metav1.PatchOptions{FieldManager: "ani-resource-virtual-switch"})
	if err != nil {
		member.Unknown = true
		return member, mutationFailure(err)
	}
	member.Applied = true
	member.State = "applied"
	member.Unknown = false
	member.ResourceVersion = updated.GetResourceVersion()
	return member, nil
}

// Even an absent group needs a real CAS write: a timed-out old create can still
// be waiting inside the API server with the last prepared resourceVersion.
func (p *KCProvider) patchVirtualSwitchDeletionNode(ctx context.Context, sw biz.VirtualSwitch, member biz.VirtualSwitchNode, n *unstructured.Unstructured) (biz.VirtualSwitchNode, error) {
	labelKey, ownerKey := virtualSwitchGroupPrefix+sw.GroupName, VirtualSwitchOwnerKey(sw.GroupName)
	fenceKey, fenceValue := virtualSwitchDeleteFenceKey(sw.GroupName), virtualSwitchDeleteFenceValue(sw)
	label, hasLabel := n.GetLabels()[labelKey]
	owner, hasOwner := n.GetAnnotations()[ownerKey]
	fence, hasFence := n.GetAnnotations()[fenceKey]
	if hasFence && fence != fenceValue {
		return member, switchConflict()
	}
	patch := []map[string]any{{"op": "test", "path": "/metadata/uid", "value": member.NodeUID}, {"op": "test", "path": "/metadata/resourceVersion", "value": member.ResourceVersion}}
	if hasLabel || hasOwner {
		if owner != sw.BindingID || !hasLabel || label != "100" {
			return member, switchConflict()
		}
		patch = append(patch, map[string]any{"op": "test", "path": "/metadata/annotations/" + jsonPointerKey(ownerKey), "value": sw.BindingID}, map[string]any{"op": "test", "path": "/metadata/labels/" + jsonPointerKey(labelKey), "value": "100"}, map[string]any{"op": "remove", "path": "/metadata/labels/" + jsonPointerKey(labelKey)}, map[string]any{"op": "remove", "path": "/metadata/annotations/" + jsonPointerKey(ownerKey)})
	} else if member.DeleteFenced {
		if !hasFence || member.State != "unfencing" {
			member.Unknown = false
			member.State = "cleaning"
			return member, nil
		}
		// Only a persisted barrier plus fresh KCN release evidence may
		// request this step. Unknown removal is recoverable from DeleteFenced.
		patch = append(patch, map[string]any{"op": "test", "path": "/metadata/annotations/" + jsonPointerKey(fenceKey), "value": fenceValue}, map[string]any{"op": "remove", "path": "/metadata/annotations/" + jsonPointerKey(fenceKey)})
	}
	removing := !hasLabel && !hasOwner && member.DeleteFenced && hasFence && member.State == "unfencing"
	if !removing {
		if n.GetAnnotations() == nil {
			patch = append(patch, map[string]any{"op": "add", "path": "/metadata/annotations", "value": map[string]any{}})
		}
		if hasFence {
			patch = append(patch, map[string]any{"op": "test", "path": "/metadata/annotations/" + jsonPointerKey(fenceKey), "value": fenceValue})
		}
		patch = append(patch, map[string]any{"op": "add", "path": "/metadata/annotations/" + jsonPointerKey(fenceKey), "value": fenceValue})
	}
	body, _ := json.Marshal(patch)
	updated, err := p.client.Resource(kcNodes).Patch(ctx, member.NodeName, types.JSONPatchType, body, metav1.PatchOptions{FieldManager: "ani-resource-virtual-switch"})
	if err != nil {
		member.Unknown = true
		return member, mutationFailure(err)
	}
	member.DeleteFenced = true
	member.Applied = false
	member.Unknown = false
	member.State = "cleaning"
	member.ResourceVersion = updated.GetResourceVersion()
	return member, nil
}

// Completion releases Resource's logical binding after KCN has removed its
// VLANs and node device acknowledgement. It does not assert physical OVS teardown.
func switchNodeCleanupConfirmed(sw biz.VirtualSwitch, member biz.VirtualSwitchNode, node *unstructured.Unstructured, vlans []unstructured.Unstructured) (bool, error) {
	if node == nil || node.GetName() != member.NodeName || string(node.GetUID()) != member.NodeUID || node.GetResourceVersion() == "" || node.GetDeletionTimestamp() != nil {
		return false, switchUnavailable()
	}
	if _, present := node.GetLabels()[virtualSwitchGroupPrefix+sw.GroupName]; present {
		return false, nil
	}
	if _, present := node.GetAnnotations()[VirtualSwitchOwnerKey(sw.GroupName)]; present {
		return false, nil
	}
	for _, vlan := range vlans {
		if crString(&vlan, "spec", "vClusterName") == sw.GroupName {
			return false, nil
		}
	}
	if strings.TrimSpace(node.GetAnnotations()[virtualSwitchGroupPrefix+sw.GroupName]) != "" {
		return false, nil
	}
	return true, nil
}
