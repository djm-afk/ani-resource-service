package instance

import (
	"strings"
	"testing"

	gp "github.com/zhangzhe-ctrl/ani-accelerator-service/pkg/gpuprotocol"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/instance"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestHygonFrozenResourcesAndAdmissionProjection(t *testing.T) {
	// Reuse the real CPU notebook fixture, then freeze the Hygon contract.
	_, base, _, _ := diagnosticFixture(t, false)
	for _, fixed := range []bool{false, true} {
		i := base
		i.Spec.ComputeType = biz.TypeGPU
		key, allocation, mode, devices, memory, cu := "hygon.com/hcu", "PHYSICAL", "WHOLE_EXCLUSIVE", uint32(2), int64(0), uint32(0)
		if fixed {
			key = "hygon.com/hcu-share-4c-16g"
			allocation = "FIXED_PARTITION"
			mode = "SHARED_FIXED"
			devices = 1
			memory = 16385
			cu = 4
		}
		i.Spec.GPUSummary = &biz.GPUSummary{ProfileID: i.ID, ProfileVersion: 1, ModelKey: "hcu", Mode: mode, AllocationType: allocation, DevicesPerReplica: devices, MemoryMiBPerDevice: memory, ComputeUnitsPerDevice: cu, ResourceKey: key, AdapterID: gp.HygonAdapterID, SpecDigest: strings.Repeat("a", 64)}
		i.Spec.GPU = biz.GPU{Scheduler: "volcano", Queue: "test", NodeLabels: map[string]string{gp.GroupLabel: i.ID, gp.ModelLabel: "hcu", gp.BaselineLabel: i.ID}, Limits: map[string]string{key: "2"}}
		if fixed {
			i.Spec.GPU.Limits[key] = "1"
		}
		i.Spec.GPU.Digest, _ = gp.HashExecutionFields(&gp.ExecutionFields{SchedulerName: i.Spec.GPU.Scheduler, QueueName: i.Spec.GPU.Queue, NodeSelector: i.Spec.GPU.NodeLabels, Resources: i.Spec.GPU.Limits})
		if err := biz.ValidateFrozenGPU(i.Spec.ComputeType, i.Spec.GPU, i.Spec.GPUSummary); err != nil {
			t.Fatal(err)
		}
		notebook, err := RenderNotebook(i)
		if err != nil {
			t.Fatal(err)
		}
		template, _, _ := unstructured.NestedMap(notebook.Object, "spec", "template")
		pod := &unstructured.Unstructured{Object: template}
		pod.SetLabels(notebook.GetLabels())
		pod.SetAnnotations(notebook.GetAnnotations())
		containers, _, _ := unstructured.NestedSlice(pod.Object, "spec", "containers")
		c := containers[0].(map[string]any)
		for _, field := range []string{"requests", "limits"} {
			values, _, _ := unstructured.NestedStringMap(c, "resources", field)
			if values[key] != i.Spec.GPU.Limits[key] || len(values) != 3 {
				t.Fatal("Hygon request/limit keys changed", values)
			}
		}
		if _, present, _ := unstructured.NestedString(pod.Object, "spec", "runtimeClassName"); present {
			t.Fatal("Hygon inherited NVIDIA runtime")
		}
		if err = verifyPod(pod, i); err != nil {
			t.Fatal("valid Hygon projection rejected", err)
		}
		// Kubernetes admission may add unrelated valid scheduling fields.
		nodes, _, _ := unstructured.NestedStringMap(pod.Object, "spec", "nodeSelector")
		nodes["kubernetes.io/hostname"] = "admission-node"
		_ = unstructured.SetNestedStringMap(pod.Object, nodes, "spec", "nodeSelector")
		if err = verifyPod(pod, i); err != nil {
			t.Fatal("admission selector rejected", err)
		}
		_ = unstructured.SetNestedSlice(pod.Object, []any{map[string]any{"name": "steal-device", "resources": map[string]any{"requests": map[string]any{key: "1"}}}}, "spec", "initContainers")
		if err = verifyPod(pod, i); err == nil {
			t.Fatal("init container requested frozen HCU device")
		}
	}
}
