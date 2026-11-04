package main

import (
	"context"
	"strings"

	networkbiz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	networkdata "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network"
	data "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/vm"
)

// VM owns the accepted intent. Network owns directory selection and its own
// namespace/NAD/SA declarations; the composition root connects the two ports.
type vmNetworkAccess struct {
	repository *networkdata.Postgres
	provider   *networkdata.KCProvider
}

func (a vmNetworkAccess) resolve(ctx context.Context, tenant, cluster, namespace string, requested biz.NetworkSelection) (biz.NetworkSelection, error) {
	selected, err := a.repository.ResolveNewNetworkSelection(ctx, networkbiz.NetworkSelection{TenantID: tenant, VPCID: requested.VPCID, SubnetID: requested.SubnetID, ClusterID: cluster, Namespace: namespace})
	if err != nil {
		return biz.NetworkSelection{}, data.MapNetworkError(err)
	}
	intent, err := a.provider.LookupVMNetworkIntent(ctx, selected, nil, "")
	if err != nil {
		return biz.NetworkSelection{}, data.MapNetworkError(err)
	}
	return biz.NetworkSelection{VPCID: selected.VPCID, SubnetID: selected.SubnetID, NADName: intent.NADName, NADUID: intent.NADUID, NADDigest: intent.NADConfigDigest, Reference: selected.Reference, NADConfig: intent.NADConfig, ServiceAccount: intent.ServiceAccount}, nil
}

func (a vmNetworkAccess) accept(ctx context.Context, tenant string, spec biz.Spec) (biz.NetworkSelection, error) {
	selected, err := a.repository.ResolveNewNetworkSelection(ctx, networkbiz.NetworkSelection{TenantID: tenant, VPCID: spec.Network.VPCID, SubnetID: spec.Network.SubnetID, ClusterID: spec.ClusterID, Namespace: spec.Namespace, NamespaceUID: spec.NamespaceUID})
	if err != nil {
		return biz.NetworkSelection{}, data.MapNetworkError(err)
	}
	expected := &networkbiz.VMNetworkExpectation{NADName: spec.Network.NADName, NADUID: spec.Network.NADUID, NADConfigDigest: spec.Network.NADDigest}
	intent, err := a.provider.LookupVMNetworkIntent(ctx, selected, expected, spec.ServiceAccount)
	if err != nil {
		return biz.NetworkSelection{}, data.MapNetworkError(err)
	}
	out := spec.Network
	out.Reference, out.NADConfig = selected.Reference, intent.NADConfig
	return out, nil
}

func vmAcceptedNetwork(v biz.VM) networkbiz.NetworkSelection {
	return networkbiz.NetworkSelection{TenantID: v.TenantID, VPCID: v.Spec.Network.VPCID, SubnetID: v.Spec.Network.SubnetID, ClusterID: v.Spec.ClusterID, Namespace: v.Spec.Namespace, NamespaceUID: v.Spec.NamespaceUID, Reference: v.Spec.Network.Reference}
}

func vmFrozenNetworkIntent(v biz.VM, selected networkbiz.ResolvedNetworkSelection, nadUID string) networkdata.VMNetworkIntent {
	return networkdata.VMNetworkIntent{ManagedVMNAD: networkdata.ManagedVMNAD{TenantID: v.TenantID, VPCID: v.Spec.Network.VPCID, SubnetID: v.Spec.Network.SubnetID, ClusterID: v.Spec.ClusterID, Namespace: v.Spec.Namespace, NamespaceUID: v.Spec.NamespaceUID, NADName: v.Spec.Network.NADName, NADUID: nadUID, NADConfigDigest: v.Spec.Network.NADDigest, KCNSubnetRef: selected.NativeNamespace + "/" + selected.NativeName}, NADConfig: v.Spec.Network.NADConfig, ServiceAccount: v.Spec.ServiceAccount}
}

// frozenIntent only consults the original record/static UID for legacy accepted
// snapshots. Newly accepted owners always carry raw NAD configuration already.
func (a vmNetworkAccess) frozenIntent(ctx context.Context, v biz.VM, selected networkbiz.ResolvedNetworkSelection, nadUID string) (networkdata.VMNetworkIntent, error) {
	intent := vmFrozenNetworkIntent(v, selected, nadUID)
	if intent.NADConfig != "" {
		return intent, nil
	}
	if nadUID == "" {
		return intent, biz.ErrChanged
	}
	resolved, err := a.provider.LookupVMNetworkIntent(ctx, selected, &networkbiz.VMNetworkExpectation{NADName: intent.NADName, NADUID: nadUID, NADConfigDigest: intent.NADConfigDigest}, intent.ServiceAccount)
	if err != nil {
		return intent, data.MapNetworkError(err)
	}
	intent.NADConfig = resolved.NADConfig
	return intent, nil
}

func (a vmNetworkAccess) prepare(ctx context.Context, v biz.VM) (biz.NetworkPlan, error) {
	selected, err := a.repository.PrepareAcceptedTenantNetwork(ctx, vmAcceptedNetwork(v))
	if err != nil {
		return biz.NetworkPlan{}, data.MapNetworkError(err)
	}
	intent, err := a.frozenIntent(ctx, v, selected, v.Spec.Network.NADUID)
	if err != nil {
		return biz.NetworkPlan{}, err
	}
	actual, err := a.provider.PrepareTenantVMNetwork(ctx, selected, intent)
	if err != nil {
		return biz.NetworkPlan{}, data.MapNetworkError(err)
	}
	// No Attachment or consumer snapshot exists in this first saved round.
	return biz.NetworkPlan{Namespace: actual.Namespace, NADName: actual.NADName, NADUID: actual.NADUID, NADDigest: actual.NADConfigDigest, KCNSubnetRef: actual.KCNSubnetRef, BindingRevision: selected.BindingID, BasicIsolationName: selected.BasicIsolationName}, nil
}

func (a vmNetworkAccess) validateExisting(ctx context.Context, v biz.VM) error {
	if v.Network.AttachmentID == "" || v.Network.NADUID == "" {
		return biz.ErrDependency
	}
	selected, err := a.repository.ResolveAcceptedNetworkSelection(ctx, vmAcceptedNetwork(v))
	if err != nil {
		return data.MapNetworkError(err)
	}
	actual := v.Network
	bindingID, _, _ := strings.Cut(actual.BindingRevision, ":")
	if actual.Namespace != v.Spec.Namespace || actual.NADName != v.Spec.Network.NADName || actual.NADDigest != v.Spec.Network.NADDigest || bindingID != selected.BindingID || actual.KCNSubnetRef != selected.NativeNamespace+"/"+selected.NativeName || (v.Spec.Network.NADUID != "" && actual.NADUID != v.Spec.Network.NADUID) {
		return biz.ErrChanged
	}
	intent, err := a.frozenIntent(ctx, v, selected, actual.NADUID)
	if err != nil {
		return err
	}
	return data.MapNetworkError(a.provider.ValidateTenantVMNetwork(ctx, selected, intent))
}
