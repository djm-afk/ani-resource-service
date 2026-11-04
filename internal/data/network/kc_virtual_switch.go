package data

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"slices"
	"strconv"
	"strings"
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
	if sw.BindingID != "" {
		return virtualSwitchInventory(sw, device, nil)
	}
	nodes, err := p.listAll(ctx, kcNodes)
	if err != nil {
		return biz.VirtualSwitchInventory{}, err
	}
	return virtualSwitchInventory(sw, device, nodes)
}

// Freeze every allocated node and the administrator's device intent. KCN owns
// physical device validation and takeover; Resource only validates this binding.
func virtualSwitchInventory(sw biz.VirtualSwitch, device string, nodes []unstructured.Unstructured) (biz.VirtualSwitchInventory, error) {
	result := biz.VirtualSwitchInventory{}
	if device == "" {
		return result, switchConflict()
	}
	if sw.BindingID != "" {
		result.Members = slices.Clone(sw.Nodes)
	} else {
		for _, node := range nodes {
			if node.GetLabels()[allocatedNodeLabel] != "true" {
				continue
			}
			if node.GetUID() == "" {
				continue
			}
			if _, exists := node.GetLabels()[virtualSwitchGroupPrefix+sw.GroupName]; exists {
				return result, switchConflict()
			}
			if _, exists := node.GetAnnotations()[VirtualSwitchOwnerKey(sw.GroupName)]; exists {
				return result, switchConflict()
			}
			result.Members = append(result.Members, biz.VirtualSwitchNode{NodeName: node.GetName(), NodeUID: string(node.GetUID()), Priority: 100, Allocated: true, ResourceVersion: node.GetResourceVersion()})
		}
	}
	if len(result.Members) == 0 {
		return result, switchUnavailable()
	}
	slices.SortFunc(result.Members, func(a, b biz.VirtualSwitchNode) int { return strings.Compare(a.NodeUID, b.NodeUID) })
	stable := make([]biz.PreparedDeviceNode, 0, len(result.Members))
	for _, member := range result.Members {
		stable = append(stable, biz.PreparedDeviceNode{NodeName: member.NodeName, NodeUID: member.NodeUID, Name: device})
	}
	result.Inventory.Items = stable
	result.Inventory.Fingerprint = contentHash(stable)
	// Membership identity excludes request resourceVersions and device selection.
	members := make([]struct {
		Name, UID string
		Priority  int32
		Allocated bool
	}, 0, len(result.Members))
	for _, member := range result.Members {
		members = append(members, struct {
			Name, UID string
			Priority  int32
			Allocated bool
		}{member.NodeName, member.NodeUID, member.Priority, member.Allocated})
	}
	result.Fingerprint = contentHash(members)
	if sw.BindingID != "" {
		result.Fingerprint = sw.MembershipFingerprint
	}
	return result, nil
}

func (p *KCProvider) InspectVirtualSwitchNode(ctx context.Context, sw biz.VirtualSwitch, member biz.VirtualSwitchNode) (biz.VirtualSwitchNode, error) {
	if sw.ClusterID != p.repository.placement.ClusterID || sw.BindingID == "" {
		return member, switchConflict()
	}
	node, err := p.client.Resource(kcNodes).Get(ctx, member.NodeName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) && sw.State == biz.Deleting {
		member.State, member.Applied, member.Unknown, member.DeleteFenced = "cleaned", false, false, true
		return member, nil
	}
	if err != nil {
		return member, readFailure(err)
	}
	if sw.State == biz.Deleting && string(node.GetUID()) != member.NodeUID {
		member.State, member.Applied, member.Unknown, member.DeleteFenced = "cleaned", false, false, true
		return member, nil
	}
	return inspectSwitchNodeObject(sw, member, node)
}

func inspectSwitchNodeObject(sw biz.VirtualSwitch, member biz.VirtualSwitchNode, n *unstructured.Unstructured) (biz.VirtualSwitchNode, error) {
	if n == nil || n.GetName() != member.NodeName || string(n.GetUID()) != member.NodeUID {
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
		if !member.Applied && member.DeleteFenced {
			member.State = "cleaned"
			if fencePresent {
				member.State = "unfencing"
			}
		} else if !member.Applied && !member.Prepared {
			member.State = "cleaned"
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

	n, err := p.client.Resource(kcNodes).Get(ctx, member.NodeName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) && deleting {
		member.State, member.Applied, member.Unknown, member.DeleteFenced = "cleaned", false, false, true
		return member, nil
	}
	if err != nil {
		return member, readFailure(err)
	}
	if deleting && string(n.GetUID()) != member.NodeUID {
		member.State, member.Applied, member.Unknown, member.DeleteFenced = "cleaned", false, false, true
		return member, nil
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
		// This marker only recovers our own conditional PATCH response.
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
