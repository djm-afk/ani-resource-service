package instance

import (
	"context"
	"errors"
	"testing"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/instance"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	ktesting "k8s.io/client-go/testing"
)

func TestSecurityGroupsNotebookAndRegisteredCurrentPod(t *testing.T) {
	n, i, client, pod := diagnosticFixture(t, false)
	i.PodName, i.PodUID = pod.GetName(), string(pod.GetUID())
	ctx := context.Background()
	root, _ := client.Resource(notebookGVR).Namespace(i.Spec.Namespace).Get(ctx, i.Spec.NotebookName, metav1.GetOptions{})
	root.SetResourceVersion("1")
	_, _ = client.Resource(notebookGVR).Namespace(i.Spec.Namespace).Update(ctx, root, metav1.UpdateOptions{})
	pod.SetResourceVersion("2")
	_, _ = client.Resource(podGVR).Namespace(i.Spec.Namespace).Update(ctx, pod, metav1.UpdateOptions{})
	names := []string{"sg-first", "sg-second"}
	client.ClearActions()
	if err := n.SetSecurityGroups(ctx, i, names); err != nil {
		t.Fatal("own declaration waited for generated template convergence", err)
	}
	for _, action := range client.Actions() {
		if action.GetResource().Resource == "statefulsets" || action.GetVerb() == "list" {
			t.Fatal("security group submission scanned generated workloads", action)
		}
	}
	current, _ := client.Resource(podGVR).Namespace(i.Spec.Namespace).Get(ctx, pod.GetName(), metav1.GetOptions{})
	if current.GetAnnotations()[biz.SecurityGroupsAnnotation] != "sg-first,sg-second" {
		t.Fatal("live Pod not updated")
	}
	if err := n.SetSecurityGroups(ctx, i, names); err != nil {
		t.Fatal(err)
	}
	// Explicit empty annotation must remain present in both paths.
	if err := n.SetSecurityGroups(ctx, i, nil); err != nil {
		t.Fatal(err)
	}
	for _, resource := range []struct {
		kind string
		name string
	}{{"root", i.Spec.NotebookName}, {"pod", pod.GetName()}} {
		r := notebookGVR
		if resource.kind == "pod" {
			r = podGVR
		}
		got, _ := client.Resource(r).Namespace(i.Spec.Namespace).Get(ctx, resource.name, metav1.GetOptions{})
		if value, present := got.GetAnnotations()[biz.SecurityGroupsAnnotation]; !present || value != "" {
			t.Fatal("empty detach lost", got.GetAnnotations())
		}
	}
}
func TestSecurityGroupsRejectChangedNotebookUIDBeforeMutation(t *testing.T) {
	n, i, client, _ := diagnosticFixture(t, false)
	client.PrependReactor("get", "notebooks", func(a ktesting.Action) (bool, runtime.Object, error) {
		return true, &unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": i.Spec.NotebookName, "namespace": i.Spec.Namespace, "uid": "foreign"}}}, nil
	})
	if err := n.SetSecurityGroups(context.Background(), i, []string{"sg-web"}); !errors.Is(err, biz.ErrOwnership) {
		t.Fatal(err)
	}
	for _, a := range client.Actions() {
		if a.GetVerb() == "patch" {
			t.Fatal("foreign root was mutated")
		}
	}
}
