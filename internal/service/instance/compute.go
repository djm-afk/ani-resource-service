package instance

import (
	acc "github.com/zhangzhe-ctrl/ani-accelerator-service/api/gen/go/accelerator/v1"
	pb "github.com/zhangzhe-ctrl/ani-resource-service/api/instance/v1"
	compute "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/compute"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/instance"
	"math"
)

func wireReference(g *compute.GPUReference) *pb.GpuPresetReference {
	if g == nil {
		return nil
	}
	return &pb.GpuPresetReference{ProfileId: g.ProfileID, ProfileVersion: g.ProfileVersion, SpecDigest: g.SpecDigest, DevicesPerInstance: g.DevicesPerInstance, ClusterId: g.ClusterID, PoolId: g.PoolID, Mode: acc.SupplyMode(acc.SupplyMode_value[g.Mode]), ModelKey: g.ModelKey, MemoryMibPerDevice: g.MemoryMiBPerDevice, CoreLimitPercent: g.CoreLimitPercent, AllocationType: acc.AllocationType(acc.AllocationType_value[g.AllocationType]), ResourceKey: g.ResourceKey, ComputeUnitsPerDevice: g.ComputeUnitsPerDevice, AdapterId: g.AdapterID}
}
func wireSummary(g *biz.GPUSummary) *pb.GpuSummary {
	if g == nil {
		return nil
	}
	if g.DevicesPerReplica == 0 || (g.MemoryMiBPerDevice > 0 && g.MemoryMiBPerDevice > math.MaxInt64/int64(g.DevicesPerReplica)) {
		return nil
	}
	return &pb.GpuSummary{ProfileId: g.ProfileID, ProfileVersion: g.ProfileVersion, ProfileName: g.ProfileName, Mode: acc.SupplyMode(acc.SupplyMode_value[g.Mode]), ModelKey: g.ModelKey, DevicesPerInstance: g.DevicesPerReplica, MemoryMibPerDevice: g.MemoryMiBPerDevice, MemoryMibTotal: g.MemoryMiBPerDevice * int64(g.DevicesPerReplica), CoreLimitPercent: g.CoreLimitPercent, AllocationType: acc.AllocationType(acc.AllocationType_value[g.AllocationType]), ResourceKey: g.ResourceKey, ComputeUnitsPerDevice: g.ComputeUnitsPerDevice, AdapterId: g.AdapterID, SpecDigest: g.SpecDigest}
}
func domainScheduling(s *pb.SchedulingOptions) biz.Scheduling {
	return biz.Scheduling{PodPlacement: s.GetPodPlacement().String(), PriorityClassName: s.PriorityClassName}
}
func wireScheduling(s biz.Scheduling) *pb.SchedulingOptions {
	return &pb.SchedulingOptions{PodPlacement: pb.SchedulingOptions_PodPlacement(pb.SchedulingOptions_PodPlacement_value[s.PodPlacement]).Enum(), PriorityClassName: s.PriorityClassName}
}
