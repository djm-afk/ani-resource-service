package main

import (
	"context"
	"errors"

	instance "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/instance"
	network "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	vm "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	networkdata "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network"
	networkservice "github.com/zhangzhe-ctrl/ani-resource-service/internal/service/network"
)

// Consumer reads stay within each owner. Network never reads VM/Instance SQL.
func wireVNicConsumers(repo *networkdata.Postgres, handler *networkservice.NetworkService, vms *vmComponents, containers *instanceComponents) {
	resolver := func(ctx context.Context, tenant, kind, id string) (network.VNicConsumerScope, error) {
		out := network.VNicConsumerScope{TenantID: tenant, ConsumerKind: kind, ConsumerID: id}
		switch kind {
		case "vm":
			if vms == nil {
				return out, network.Fail(network.ResourceNotFound, "VM network owner unavailable")
			}
			v, err := vms.repo.Get(ctx, tenant, id)
			if err != nil {
				return out, ownerVNicError(err)
			}
			out.SubnetID, out.ClusterID, out.Namespace, out.NamespaceUID = v.Spec.Network.SubnetID, v.Spec.ClusterID, v.Spec.Namespace, v.Spec.NamespaceUID
			out.Version, out.Name, out.State = v.Version, v.Name, v.Observed
			out.ComputeType = "vm"
			out.SecurityGroupIDs = append([]string{}, v.SecurityGroupIDs...)
			if v.Closing {
				out.State = "deleting"
			}
		case "container":
			if containers == nil {
				return out, network.Fail(network.ResourceNotFound, "container network owner unavailable")
			}
			i, err := containers.repo.Get(instance.WithCaller(ctx, instance.Caller{TenantID: tenant, Actor: "resource:network-owner-query"}), tenant, id)
			if err != nil {
				return out, ownerVNicError(err)
			}
			if i.Spec.Network == nil || i.Spec.InstanceKind != instance.KindContainer {
				return out, network.Fail(network.ResourceNotFound, "instance has no supported network")
			}
			out.SubnetID, out.ClusterID, out.Namespace = i.Spec.Network.SubnetID, i.Spec.ClusterID, i.Spec.Namespace
			out.Version, out.Name, out.State, out.ComputeType = i.Version, i.Spec.Name, i.Observed, i.Spec.ComputeType
			out.SecurityGroupIDs = append([]string{}, i.SecurityGroupIDs...)
			if i.Desired == instance.Deleted {
				out.State = "deleting"
			}
		default:
			return out, network.Fail(network.InvalidArgument, "unsupported VNIC consumer")
		}
		return out, nil
	}
	repo.ConfigureVNicConsumerResolver(resolver)
	handler.UseVNicBindingTargets(func(ctx context.Context, v network.VNic, kind, name string, page, size uint32) ([]network.VNicConsumerScope, int64, error) {
		out := []network.VNicConsumerScope{}
		switch kind {
		case "vm":
			if vms == nil {
				return out, 0, nil
			}
			vs, total, err := vms.repo.List(ctx, v.TenantID, vm.ListFilter{Page: page, PageSize: size, Keyword: name, SubnetID: v.SubnetID, ClusterID: v.ClusterID, Namespace: v.Namespace, BindableOnly: true})
			if err != nil {
				return nil, 0, ownerVNicError(err)
			}
			for _, x := range vs {
				out = append(out, network.VNicConsumerScope{TenantID: v.TenantID, ConsumerKind: kind, ConsumerID: x.ID, Name: x.Name, Version: x.Version, State: x.Observed, ComputeType: "vm", SubnetID: x.Spec.Network.SubnetID, ClusterID: x.Spec.ClusterID, Namespace: x.Spec.Namespace, NamespaceUID: x.Spec.NamespaceUID, SecurityGroupIDs: append([]string{}, x.SecurityGroupIDs...)})
			}
			return out, total, nil
		case "container":
			if containers == nil {
				return out, 0, nil
			}
			is, total, err := containers.repo.List(instance.WithCaller(ctx, instance.Caller{TenantID: v.TenantID, Actor: "resource:network-owner-query"}), v.TenantID, instance.ListFilter{Page: page, PageSize: size, Name: name, SubnetID: v.SubnetID, ClusterID: v.ClusterID, Namespace: v.Namespace, BindableOnly: true})
			if err != nil {
				return nil, 0, ownerVNicError(err)
			}
			for _, x := range is {
				out = append(out, network.VNicConsumerScope{TenantID: v.TenantID, ConsumerKind: kind, ConsumerID: x.ID, Name: x.Spec.Name, Version: x.Version, State: x.Observed, ComputeType: x.Spec.ComputeType, SubnetID: x.Spec.Network.SubnetID, ClusterID: x.Spec.ClusterID, Namespace: x.Spec.Namespace, SecurityGroupIDs: append([]string{}, x.SecurityGroupIDs...)})
			}
			return out, total, nil
		}
		return nil, 0, network.Fail(network.InvalidArgument, "unsupported VNIC consumer")
	})
}
func ownerVNicError(err error) error {
	if errors.Is(err, vm.ErrNotFound) || errors.Is(err, instance.ErrNotFound) {
		return network.Fail(network.ResourceNotFound, "VNIC consumer not found")
	}
	return network.Fail(network.ProviderUnavailable, "VNIC consumer query unavailable")
}
