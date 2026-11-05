package instance

import (
	"context"
	pb "github.com/zhangzhe-ctrl/ani-resource-service/api/instance/v1"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/instance"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// markMountablePVCs 统一为候选标记不可选原因，创建范围与实例范围共用同一规则：
// 删除 guard 优先（PVC_DELETION_IN_PROGRESS），其次按引用台账标记 PVC_IN_USE。
// 只覆盖当前可选的项，保留 Block/Lost/Terminating 等既有原因。候选是观测值，
// 受理期的权威校验在受理事务内执行。
func (s *Service) markMountablePVCs(ctx context.Context, tenant, cluster, namespace string, items []biz.MountablePVC) ([]biz.MountablePVC, error) {
	guard, ok := s.repo.(biz.PVCGuardReader)
	if !ok {
		return nil, biz.ErrNetworkUnavailable
	}
	refs := map[string]bool{}
	if usage, ok := s.repo.(biz.VolumeUsageReader); ok {
		list, err := usage.VolumeReferences(ctx, tenant, cluster, namespace)
		if err != nil {
			return nil, err
		}
		for _, r := range list {
			refs[r.PVCName+"\x00"+r.PVCUID] = true
		}
	}
	for i := range items {
		deleting, err := guard.PVCDeleting(ctx, tenant, cluster, namespace, items[i].PVCName)
		if err != nil {
			return nil, err
		}
		switch {
		case deleting:
			items[i].Selectable = false
			items[i].UnavailableReason = "PVC_DELETION_IN_PROGRESS"
		case items[i].Selectable && refs[items[i].PVCName+"\x00"+items[i].PVCUID]:
			items[i].Selectable = false
			items[i].UnavailableReason = "PVC_IN_USE"
		}
	}
	return items, nil
}

func (s *Service) ListMountablePVCs(ctx context.Context, r *pb.ListMountablePVCsRequest) (*pb.ListMountablePVCsResponse, error) {
	if r == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	if e := biz.RequireTenant(ctx, r.TenantId); e != nil {
		return nil, rpcError(e)
	}
	if s.volumes == nil {
		return nil, status.Error(codes.Unavailable, "INSTANCE_DEPENDENCY_UNAVAILABLE")
	}
	ns, e := biz.ResolveNamespace(r.TenantId, "", s.config.NamespacePrefix)
	if e != nil {
		return nil, rpcError(e)
	}
	items, total, e := s.volumes.ListMountablePVCs(ctx, r.TenantId, s.config.ClusterID, ns, r.Page, r.PageSize, r.Name)
	if e != nil {
		return nil, rpcError(e)
	}
	if items, e = s.markMountablePVCs(ctx, r.TenantId, s.config.ClusterID, ns, items); e != nil {
		return nil, rpcError(e)
	}
	out := &pb.ListMountablePVCsResponse{Items: []*pb.MountablePVC{}, Total: total}
	for _, v := range items {
		out.Items = append(out.Items, &pb.MountablePVC{PvcName: v.PVCName, Phase: v.Phase, RequestedCapacity: v.RequestedCapacity, AccessModes: v.AccessModes, VolumeMode: v.VolumeMode, Selectable: v.Selectable, UnavailableReason: v.UnavailableReason})
	}
	return out, nil
}
func (s *Service) GetInstanceConfiguration(ctx context.Context, r *pb.GetInstanceRequest) (*pb.InstanceConfiguration, error) {
	if r == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	if e := biz.RequireTenant(ctx, r.TenantId); e != nil {
		return nil, rpcError(e)
	}
	i, e := s.repo.Get(ctx, r.TenantId, r.InstanceId)
	if e != nil {
		return nil, rpcError(e)
	}
	if e := biz.ValidateConfiguration(i.Spec); e != nil {
		return nil, rpcError(e)
	}
	out := &pb.InstanceConfiguration{InstanceId: i.ID, WorkspaceMountPath: i.Spec.WorkspaceMountPath, Labels: i.Spec.Labels, Annotations: i.Spec.Annotations}
	out.SecurityGroupIds = append([]string(nil), i.SecurityGroupIDs...)
	if !i.SecurityGroupsInitialized {
		out.SecurityGroupIds = append([]string(nil), i.Spec.SecurityGroupIDs...)
	}
	out.Version = i.Version
	out.AppliedMountVersion = i.AppliedMountVersion
	if o := i.ActiveOperation; o != nil && o.Execution != nil && (o.Action == "attach-pvc" || o.Action == "detach-pvc") {
		out.PendingChange = &pb.PendingMountChange{OperationId: o.ID, IntentVersion: o.Version, Phase: o.Execution.Phase, TargetConfiguration: wireMountTarget(o.Execution.TargetConfiguration)}
	}
	for _, m := range i.CurrentMounts {
		out.VolumeMounts = append(out.VolumeMounts, &pb.VolumeMount{PvcName: m.PVCName, MountPath: m.MountPath, ReadOnly: m.ReadOnly})
	}
	for _, v := range i.Spec.Env {
		out.Env = append(out.Env, &pb.InstanceEnvVar{Name: v.Name, Value: v.Value})
	}
	if i.Spec.Network != nil {
		out.Network = &pb.NetworkSelection{VpcId: i.Spec.Network.VPCID, SubnetId: i.Spec.Network.SubnetID}
	}
	return out, nil
}
