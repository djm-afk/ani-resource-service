package vm

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	network "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

func (n *Network) UseManualVNics(port *network.Network) error {
	if port == nil {
		return biz.ErrInvalid
	}
	n.vnics = port
	return nil
}
func (n *Network) ReserveNetworkInterface(ctx context.Context, v biz.VM, o biz.Operation, intent biz.NetworkInterfaceIntent) (biz.NetworkInterfaceChoice, error) {
	if err := intent.Validate(); err != nil {
		return biz.NetworkInterfaceChoice{}, err
	}
	if intent.Mode == "automatic" {
		return biz.NetworkInterfaceChoice{Mode: "automatic"}, nil
	}
	if n.vnics == nil {
		return biz.NetworkInterfaceChoice{}, biz.ErrDependency
	}
	x, err := n.vnics.ReserveVNic(ctx, network.VNicReservation{TenantID: v.TenantID, VNicID: intent.VNicID, ConsumerKind: "vm", ConsumerID: v.ID, Slot: intent.Slot, OperationID: o.ID, SubnetID: v.Spec.Network.SubnetID, Revision: o.IntentVersion})
	if err != nil {
		return biz.NetworkInterfaceChoice{}, err
	}
	if x.ClusterID != v.Spec.ClusterID || x.Namespace != v.Spec.Namespace || x.NamespaceUID != v.Spec.NamespaceUID || x.ProviderUID == "" {
		return biz.NetworkInterfaceChoice{}, biz.ErrChanged
	}
	return biz.NetworkInterfaceChoice{Mode: "manual", Name: x.Name, VNicID: x.ID, ProviderName: x.ProviderName, ProviderUID: x.ProviderUID, MACAddress: x.MACAddress, IPs: append([]string{}, x.IPs...)}, nil
}
func (n *Network) SyncNetworkInterfaces(ctx context.Context, v biz.VM, ids []string) error {
	for _, id := range biz.PrimaryNetworkInterface(v).KeepVNicIDs() {
		if n.vnics == nil {
			return biz.ErrDependency
		}
		if _, err := n.vnics.SyncVNicSecurityGroups(ctx, v.TenantID, id, "vm", v.ID, ids); err != nil {
			return err
		}
	}
	return nil
}
func (n *Network) ReleaseNetworkInterfaces(ctx context.Context, v biz.VM, keep []string) error {
	if n.vnics == nil {
		if len(v.NetworkInterfaces) > 0 {
			return biz.ErrDependency
		}
		return nil
	}
	// An ordinary automatic VM has no saved primary selection or applied
	// revision. Only acknowledge a persisted selection; always release its
	// obsolete consumer bindings below.
	for _, cfg := range v.NetworkInterfaces {
		if cfg.Slot != "primary" {
			continue
		}
		if !cfg.PendingRestart() {
			if err := n.vnics.ApplyVNicBinding(ctx, v.TenantID, "vm", v.ID, cfg.Applied.VNicID, cfg.AppliedRevision); err != nil {
				return err
			}
		}
		break
	}
	return n.vnics.ReleaseVNicBindings(ctx, v.TenantID, "vm", v.ID, keep)
}

// SetNetworkInterface is a UID/RV-conditional template mutation while stopped.
// It never patches a VMI or launcher Pod.
func (n *Native) SetNetworkInterface(ctx context.Context, v biz.VM, c biz.NetworkInterfaceChoice, revision int64) error {
	before, err := n.observe(ctx, v, false)
	if err != nil {
		return err
	}
	if !before.Stopped || before.VMI != nil || before.Launcher != nil || len(before.StateChangeRequests) > 0 {
		return biz.ErrDependency
	}
	root, err := n.client.Resource(vmResource).Namespace(v.Spec.Namespace).Get(ctx, v.Native.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if !matches(root, v) || string(root.GetUID()) != v.Native.UID || root.GetDeletionTimestamp() != nil {
		return biz.ErrOwnership
	}
	ann, _, err := unstructured.NestedStringMap(root.Object, "spec", "template", "metadata", "annotations")
	if err != nil {
		return err
	}
	if ann == nil {
		ann = map[string]string{}
	}
	delete(ann, biz.VNicAnnotation)
	if c.Mode == "manual" {
		if c.ProviderName == "" || c.ProviderUID == "" {
			return biz.ErrChanged
		}
		ann[biz.VNicAnnotation] = c.ProviderName
	}
	ann["network.ani.io/interface-revision"] = strconv.FormatInt(revision, 10)
	raw, err := json.Marshal([]map[string]any{{"op": "test", "path": "/metadata/uid", "value": string(root.GetUID())}, {"op": "test", "path": "/metadata/resourceVersion", "value": root.GetResourceVersion()}, {"op": "add", "path": "/spec/template/metadata/annotations", "value": ann}})
	if err != nil {
		return err
	}
	_, err = n.client.Resource(vmResource).Namespace(v.Spec.Namespace).Patch(ctx, v.Native.Name, types.JSONPatchType, raw, metav1.PatchOptions{})
	return err
}

func (n *Network) ResolveNetworkInterfaceChoice(ctx context.Context, v biz.VM, c biz.NetworkInterfaceChoice) (biz.NetworkInterfaceChoice, error) {
	if c.Mode != "manual" || c.Name != "" {
		return c, nil
	}
	if n.vnics == nil {
		return c, biz.ErrDependency
	}
	x, err := n.vnics.GetVNic(ctx, v.TenantID, c.VNicID)
	if err != nil {
		return c, err
	}
	if x.ProviderUID != c.ProviderUID || x.ProviderName != c.ProviderName {
		return c, biz.ErrChanged
	}
	c.Name = x.Name
	return c, nil
}
func (n *Network) CurrentNetworkInterface(ctx context.Context, v biz.VM) (biz.NetworkInterfaceRuntime, error) {
	out := biz.NetworkInterfaceRuntime{}
	if v.Closing || v.Observed == "Stopped" || v.Observed == "Deleted" {
		return out, nil
	}
	a, err := n.attachment(ctx, v)
	if err != nil {
		return out, err
	}
	out.ProviderName, out.ProviderUID, out.MACAddress, out.PodUID, out.Reason = a.CurrentVNicName, a.CurrentVNicUID, a.CurrentMACAddress, a.PodUID, string(a.Reason)
	if a.ObservedAt != nil {
		out.ObservedAt = *a.ObservedAt
	}
	out.IPs = append([]string{}, a.CurrentIPs...)
	if strings.HasPrefix(a.CurrentVNicName, "auto-") {
		out.Mode = "automatic"
	} else if a.CurrentVNicName != "" {
		out.Mode = "manual"
	}
	out.Confirmed = a.State == network.Attached && a.Reason == "" && a.ConfirmedPodUID == a.PodUID && len(a.VMCurrentAddresses) > 0
	return out, nil
}
