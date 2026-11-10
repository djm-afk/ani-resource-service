package instance

import (
	"context"
	"encoding/json"
	"testing"

	pb "github.com/zhangzhe-ctrl/ani-resource-service/api/instance/v1"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/instance"
)

func TestGetInstanceFrozenGpuMemoryFactorDisplay(t *testing.T) {
	for _, factor := range []string{"", "10"} {
		t.Run("factor="+factor, func(t *testing.T) {
			execution := testGPU()
			memory := int64(8192)
			if factor != "" {
				execution.Resources["volcano.sh/vgpu-memory"] = "384"
				execution.PodAnnotations["accelerator.ani.io/memory-block-mib"] = factor
				memory = 3840
			}
			execution.ExecutionDigest, _ = pb.ExecutionDigest(execution)
			item := biz.Instance{TenantID: "11111111-1111-4111-8111-111111111111", ID: "22222222-2222-4222-8222-222222222222", Observed: biz.Stopped, Spec: biz.Spec{ClusterID: "33333333-3333-4333-8333-333333333333", ComputeType: biz.TypeGPU, GPUSummary: &biz.GPUSummary{ProfileID: "44444444-4444-4444-8444-444444444444", ProfileVersion: 1, Mode: "SHARED_FIXED", ModelKey: "t4", AllocationType: "DYNAMIC_SHARED", ResourceKey: "volcano.sh/vgpu-number", AdapterID: "volcano-hami-mib-v1", SpecDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", DevicesPerReplica: 1, MemoryMiBPerDevice: memory, CoreLimitPercent: 25}, GPU: biz.GPU{Scheduler: execution.SchedulerName, Queue: execution.QueueName, RuntimeClass: execution.RuntimeClassName, Digest: execution.ExecutionDigest, NodeLabels: execution.NodeSelector, Annotations: execution.PodAnnotations, Limits: execution.Resources}}}
			repo := &metricScopeRepository{item: item}
			images, environment := &cpuImageBoundary{}, &cpuEnvironmentBoundary{}
			service, err := NewService(repo, newTestComputeResolver(t, repo, item.Spec.ClusterID), images, environment, Config{ClusterID: item.Spec.ClusterID, ServiceAccount: "workload", StorageClass: "ani-block", WorkspaceSize: "1Gi", Shell: "/bin/sh"})
			if err != nil {
				t.Fatal(err)
			}
			before, _ := json.Marshal(repo.item.Spec.GPU)
			ctx := biz.WithCaller(context.Background(), biz.Caller{TenantID: item.TenantID, Actor: "governance:user:42"})
			result, err := service.GetInstance(ctx, &pb.GetInstanceRequest{TenantId: item.TenantID, InstanceId: item.ID})
			if err != nil || result.GetGpuSummary().GetMemoryMibPerDevice() != memory || result.GetGpuSummary().GetMemoryMibTotal() != memory || result.GetGpuSummary().GetDevicesPerInstance() != 1 {
				t.Fatal("GPU summary did not use frozen business specification", result, err)
			}
			after, _ := json.Marshal(repo.item.Spec.GPU)
			if string(before) != string(after) || images.calls != 0 || environment.calls != 0 || repo.reads != 1 {
				t.Fatal("display changed frozen execution or re-resolved current configuration")
			}
		})
	}
}
