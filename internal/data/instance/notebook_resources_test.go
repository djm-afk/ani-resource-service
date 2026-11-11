package instance

import (
	"context"
	"errors"
	"testing"

	gp "github.com/zhangzhe-ctrl/ani-accelerator-service/pkg/gpuprotocol"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/instance"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestPodDefaultEphemeralStorageKeepsRuntimeReady(t *testing.T) {
	for _, gpu := range []bool{false, true} {
		kind := "cpu"
		if gpu {
			kind = "gpu"
		}
		for name, fields := range map[string][]string{
			"request": {"requests"}, "limit": {"limits"}, "both": {"requests", "limits"},
		} {
			t.Run(kind+"/"+name, func(t *testing.T) {
				n, i, client, pod := diagnosticFixture(t, gpu)
				for _, field := range fields {
					setPodResource(t, pod, field, "ephemeral-storage", "1Gi")
				}
				if _, err := client.Resource(podGVR).Namespace(i.Spec.Namespace).Update(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
				out, err := n.VerifyRuntime(context.Background(), i, biz.Observation{})
				if err != nil || !out.PodReady {
					t.Fatal("storage default rejected a ready managed Pod", out, err)
				}
			})
		}
	}
}

func TestPodStorageDefaultPreservesComputeResourceGuards(t *testing.T) {
	for _, gpu := range []bool{false, true} {
		kind := "cpu"
		if gpu {
			kind = "gpu"
		}
		mutations := map[string]string{"cpu": "2", "memory": "3Gi", "nvidia.com/gpu": "1", "example.com/fpga": "1"}
		if gpu {
			mutations[gp.NvidiaNumberResource] = "2"
		}
		for _, field := range []string{"requests", "limits"} {
			for key, value := range mutations {
				t.Run(kind+"/"+field+"/"+key, func(t *testing.T) {
					n, i, client, pod := diagnosticFixture(t, gpu)
					setPodResource(t, pod, field, "ephemeral-storage", "1Gi")
					setPodResource(t, pod, field, key, value)
					if _, err := client.Resource(podGVR).Namespace(i.Spec.Namespace).Update(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
						t.Fatal(err)
					}
					out, err := n.VerifyRuntime(context.Background(), i, biz.Observation{})
					if !errors.Is(err, biz.ErrState) || out.PodReady {
						t.Fatal("storage default hid compute resource drift", out, err)
					}
				})
			}
		}
	}
}

func setPodResource(t *testing.T, pod *unstructured.Unstructured, field, key, value string) {
	t.Helper()
	containers, _, _ := unstructured.NestedSlice(pod.Object, "spec", "containers")
	c := containers[0].(map[string]any)
	resources, _, _ := unstructured.NestedStringMap(c, "resources", field)
	resources[key] = value
	if err := unstructured.SetNestedStringMap(c, resources, "resources", field); err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedSlice(pod.Object, containers, "spec", "containers"); err != nil {
		t.Fatal(err)
	}
}
