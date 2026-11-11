package instance

import (
	"strconv"
	"strings"
	"testing"

	gp "github.com/zhangzhe-ctrl/ani-accelerator-service/pkg/gpuprotocol"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/instance"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Freeze a complete NVIDIA contract in controller-boundary fixtures. Production
// receives these business fields from ACC instead of parsing resource maps.
func freezeNvidiaFixture(t *testing.T, i *biz.Instance) {
	t.Helper()
	g := &i.Spec.GPU
	g.NodeLabels = map[string]string{gp.GroupLabel: i.ID, gp.ModelLabel: "t4", gp.BaselineLabel: i.ID}
	if g.Annotations == nil {
		g.Annotations = map[string]string{}
	}
	g.Annotations["volcano.sh/vgpu-mode"] = "hami-core"
	fields := &gp.ExecutionFields{SchedulerName: g.Scheduler, QueueName: g.Queue, RuntimeClassName: g.RuntimeClass, NodeSelector: g.NodeLabels, PodAnnotations: g.Annotations, Resources: g.Limits}
	g.Digest, _ = gp.HashExecutionFields(fields)
	number, err := strconv.ParseUint(g.Limits[gp.NvidiaNumberResource], 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	core, err := strconv.ParseUint(g.Limits[gp.NvidiaCoresResource], 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	memory, err := strconv.ParseInt(g.Limits[gp.NvidiaMemoryResource], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	if g.Annotations["accelerator.ani.io/memory-block-mib"] == "10" {
		memory *= 10
	}
	mode, allocation := "SHARED_FIXED", "DYNAMIC_SHARED"
	if core == 100 {
		mode = "WHOLE_EXCLUSIVE"
		allocation = "PHYSICAL"
	}
	i.Spec.GPUSummary = &biz.GPUSummary{ProfileID: i.ID, ProfileVersion: 1, ProfileName: "T4", ModelKey: "t4", Mode: mode, AllocationType: allocation, DevicesPerReplica: uint32(number), MemoryMiBPerDevice: memory, CoreLimitPercent: uint32(core), ResourceKey: gp.NvidiaNumberResource, AdapterID: gp.NvidiaAdapterID, SpecDigest: strings.Repeat("a", 64)}
}

func TestPodQuantityNormalizationKeepsFrozenContract(t *testing.T) {
	_, i, _, _ := diagnosticFixture(t, true)
	i.Spec.GPU.Limits[gp.NvidiaMemoryResource] = "1000"
	freezeNvidiaFixture(t, &i)
	notebook, err := RenderNotebook(i)
	if err != nil {
		t.Fatal(err)
	}
	template, _, _ := unstructured.NestedMap(notebook.Object, "spec", "template")
	pod := &unstructured.Unstructured{Object: template}
	pod.SetLabels(notebook.GetLabels())
	pod.SetAnnotations(notebook.GetAnnotations())
	containers, _, _ := unstructured.NestedSlice(pod.Object, "spec", "containers")
	container := containers[0].(map[string]any)
	for _, field := range []string{"requests", "limits"} {
		values, _, _ := unstructured.NestedStringMap(container, "resources", field)
		values[gp.NvidiaMemoryResource] = "1k"
		_ = unstructured.SetNestedStringMap(container, values, "resources", field)
	}
	_ = unstructured.SetNestedSlice(pod.Object, containers, "spec", "containers")
	if err = verifyPod(pod, i); err != nil {
		t.Fatal("Kubernetes integer quantity normalization changed execution", err)
	}
}
