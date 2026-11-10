//go:build instanceintegration

package instance_test

import (
	"bytes"
	"context"
	"testing"

	pb "github.com/zhangzhe-ctrl/ani-resource-service/api/instance/v1"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/instance"
	data "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/instance"
	service "github.com/zhangzhe-ctrl/ani-resource-service/internal/service/instance"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
)

func TestInstanceDiagnosticDatabaseScopeAndNoWrites(t *testing.T) {
	f := database(t)
	c := cpuCreate()
	if _, e := f.repo.AcceptCreate(context.Background(), c); e != nil {
		t.Fatal(e)
	}
	ctx := biz.WithCaller(context.Background(), biz.Caller{TenantID: c.TenantID, Actor: "governance:user:42"})
	i, e := f.repo.Get(ctx, c.TenantID, c.ID)
	if e != nil {
		t.Fatal(e)
	}
	notebook, e := data.RenderNotebook(i)
	if e != nil {
		t.Fatal(e)
	}
	notebook.SetUID("diagnostic-notebook")
	template, _, _ := unstructured.NestedMap(notebook.Object, "spec", "template")
	pod := &unstructured.Unstructured{Object: template}
	pod.SetAPIVersion("v1")
	pod.SetKind("Pod")
	pod.SetName(i.Spec.NotebookName + "-0")
	pod.SetUID("diagnostic-pod")
	pod.SetNamespace(i.Spec.Namespace)
	pod.SetLabels(notebook.GetLabels())
	pod.SetAnnotations(notebook.GetAnnotations())
	controller := true
	set := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "apps/v1", "kind": "StatefulSet", "metadata": map[string]any{"name": i.Spec.NotebookName, "namespace": i.Spec.Namespace, "uid": "diagnostic-set"}, "spec": map[string]any{"replicas": int64(1)}}}
	set.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "kubeflow.org/v1", Kind: "Notebook", Name: i.Spec.NotebookName, UID: notebook.GetUID(), Controller: &controller}})
	pod.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "StatefulSet", Name: i.Spec.NotebookName, UID: set.GetUID(), Controller: &controller}})
	client := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{{Version: "v1", Resource: "pods"}: "PodList", {Version: "v1", Resource: "events"}: "EventList"}, notebook, set, pod)
	provider, e := data.NewNotebook(client)
	if e != nil {
		t.Fatal(e)
	}
	s, e := service.NewService(f.repo, computeResolver(t, f, i.Spec.ClusterID), fixtureImage{}, provider, service.Config{NamespacePrefix: "tenant-", ClusterID: i.Spec.ClusterID, ServiceAccount: i.Spec.ServiceAccount, StorageClass: i.Spec.StorageClass, WorkspaceSize: i.Spec.WorkspaceSize, Shell: i.Spec.Shell, UID: i.Spec.UID, GID: i.Spec.GID})
	if e != nil {
		t.Fatal(e)
	}
	var before, after []byte
	if e = f.runtime.QueryRow(ctx, "SELECT snapshot FROM instance.instances WHERE tenant_id=$1 AND instance_id=$2", i.TenantID, i.ID).Scan(&before); e != nil {
		t.Fatal(e)
	}
	view, e := s.GetInstanceRuntime(ctx, &pb.GetInstanceRequest{TenantId: i.TenantID, InstanceId: i.ID})
	if e != nil || view.Pod == nil || view.Pod.Uid != "diagnostic-pod" || view.Version != i.Version {
		t.Fatal("saved target runtime", view, e)
	}
	events, e := s.ListInstanceEvents(ctx, &pb.GetInstanceRequest{TenantId: i.TenantID, InstanceId: i.ID})
	if e != nil || events.PodUid == nil || events.Events == nil || len(events.Events) != 0 {
		t.Fatal("empty events versus failure", events, e)
	}
	if e = f.runtime.QueryRow(ctx, "SELECT snapshot FROM instance.instances WHERE tenant_id=$1 AND instance_id=$2", i.TenantID, i.ID).Scan(&after); e != nil || !bytes.Equal(before, after) {
		t.Fatal("diagnostic rewrote instance snapshot", e)
	}
	for _, a := range client.Actions() {
		if a.GetVerb() != "get" && a.GetVerb() != "list" {
			t.Fatal("diagnostic wrote Kubernetes", a.GetVerb())
		}
	}
	foreign := biz.WithCaller(context.Background(), biz.Caller{TenantID: c.OperationID, Actor: "governance:user:42"})
	if _, e = s.GetInstanceRuntime(foreign, &pb.GetInstanceRequest{TenantId: c.OperationID, InstanceId: i.ID}); status.Code(e) != codes.NotFound {
		t.Fatal("foreign resource ownership not isolated", e)
	}
}
