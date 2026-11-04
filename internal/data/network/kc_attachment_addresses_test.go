package data

import (
	"context"
	"errors"
	"reflect"
	"testing"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
)

func currentAddressFixture() (biz.Attachment, biz.ConsumerSubmission, *unstructured.Unstructured) {
	a := biz.Attachment{State: biz.Attached, Namespace: "tenant", PodName: "current", PodUID: "pod-uid", CurrentVNicName: "nic", CurrentVNicUID: "nic-uid", CurrentMACAddress: "02:00:00:00:00:01", CurrentIPs: []string{"10.42.1.2"}}
	c := biz.ConsumerSubmission{State: "open", DesiredRunState: "running"}
	pod := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{"namespace": a.Namespace, "name": a.PodName, "uid": a.PodUID}}}
	return a, c, pod
}

func TestAttachmentAddressReadUsesOnlyCurrentPublishedOutput(t *testing.T) {
	for _, name := range []string{"pod-ip", "no-output", "vnic-output", "vm-default", "vm-manual"} {
		t.Run(name, func(t *testing.T) {
			a, c, pod := currentAddressFixture()
			vnic := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "networking.kubercloud.com/v1", "kind": "VNic", "metadata": map[string]any{"namespace": a.Namespace, "name": "nic", "uid": "nic-uid"}, "spec": map[string]any{"macAddress": "02:00:00:00:00:02"}, "status": map[string]any{"primaryIP": "10.42.1.3", "conditions": []any{map[string]any{"type": "Ready", "status": "False"}}}}}
			want, reads := []string{}, 1
			switch name {
			case "pod-ip":
				pod.Object["status"] = map[string]any{"podIPs": []any{map[string]any{"ip": "10.42.1.4"}}}
				want = []string{"10.42.1.4"}
			case "vnic-output":
				pod.SetAnnotations(map[string]string{primaryVNicAnnotation: "tenant/nic"})
				want, reads = []string{"10.42.1.3"}, 2
			case "vm-default", "vm-manual":
				a.WorkloadMode = "vm_managed"
				pod.SetAnnotations(map[string]string{primaryVNicAnnotation: "nic", "k8s.v1.cni.cncf.io/network-status": `[{"default":true,"ips":["10.42.1.5"],"mac":"02:00:00:00:00:05"}]`})
				want = []string{"10.42.1.5"}
				if name == "vm-manual" {
					c.ManualVNics = []biz.ManualVNicReference{{ProviderName: "nic", ProviderUID: "expected-uid"}}
					reads = 2
				}
			}
			client := fake.NewSimpleDynamicClient(runtime.NewScheme(), pod, vnic)
			got, err := (&KCProvider{client: client}).ObserveAttachmentAddresses(context.Background(), a, c)
			if err != nil || !reflect.DeepEqual(got.CurrentIPs, want) || len(client.Actions()) != reads {
				t.Fatal(got, err, client.Actions())
			}
			if name == "vm-manual" && got.CurrentVNicUID != string(vnic.GetUID()) {
				t.Fatal("manual endpoint identity must come from the current VNIC", got)
			}
			for _, action := range client.Actions() {
				if action.GetVerb() != "get" || (action.GetResource() != pods && action.GetResource() != kcManualVNics) {
					t.Fatal("address projection inspected a provider dependency", action)
				}
			}
		})
	}
}

func TestAttachmentAddressReadFailurePreservesOnlyUnchangedSource(t *testing.T) {
	for _, name := range []string{"pod-error", "same-vnic-error", "changed-vnic-error"} {
		t.Run(name, func(t *testing.T) {
			a, c, pod := currentAddressFixture()
			pod.SetAnnotations(map[string]string{primaryVNicAnnotation: "nic"})
			if name == "changed-vnic-error" {
				pod.SetAnnotations(map[string]string{primaryVNicAnnotation: "new-nic"})
			}
			client := fake.NewSimpleDynamicClient(runtime.NewScheme(), pod)
			resource := "vnics"
			if name == "pod-error" {
				resource = "pods"
			}
			client.PrependReactor("get", resource, func(ktesting.Action) (bool, runtime.Object, error) { return true, nil, errors.New("read unavailable") })
			got, err := (&KCProvider{client: client}).ObserveAttachmentAddresses(context.Background(), a, c)
			if err == nil {
				t.Fatal("read failure hidden")
			}
			if name == "changed-vnic-error" {
				if got.CurrentVNicName != "new-nic" || len(got.CurrentIPs) != 0 || got.CurrentMACAddress != "" {
					t.Fatal("old source output copied to replacement", got)
				}
			} else if !reflect.DeepEqual(got.CurrentIPs, a.CurrentIPs) || got.CurrentMACAddress != a.CurrentMACAddress {
				t.Fatal("unchanged source output erased", got)
			}
		})
	}
}

func TestAttachmentCurrentSourceDisappearanceClearsOnlyAddressProjection(t *testing.T) {
	for _, name := range []string{"not-found", "replaced", "inactive"} {
		t.Run(name, func(t *testing.T) {
			a, c, pod := currentAddressFixture()
			var objects []runtime.Object
			if name == "replaced" {
				pod.SetUID("replacement")
				objects = append(objects, pod)
			}
			if name == "inactive" {
				c.DesiredRunState = "stopped"
			}
			client := fake.NewSimpleDynamicClient(runtime.NewScheme(), objects...)
			got, err := (&KCProvider{client: client}).ObserveAttachmentAddresses(context.Background(), a, c)
			if err != nil || len(got.CurrentIPs) != 0 || got.CurrentVNicName != "" || a.State != biz.Attached {
				t.Fatal(got, err)
			}
			if name == "inactive" && len(client.Actions()) != 0 {
				t.Fatal("inactive consumer inspected provider state")
			}
		})
	}
}
