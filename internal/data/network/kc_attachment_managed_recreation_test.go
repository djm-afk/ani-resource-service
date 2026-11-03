package data

import (
	"context"
	"errors"
	"testing"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
)

func TestManagedControllersWaitForAcceptedStatefulSetRecreation(t *testing.T) {
	for _, name := range []string{"accepted", "unaccepted_uid", "foreign_owner", "foreign_tenant", "foreign_namespace"} {
		t.Run(name, func(t *testing.T) {
			root := biz.WorkloadReference{APIVersion: "kubeflow.org/v1", Kind: "Notebook", Namespace: "tenant-one", Name: "notebook", UID: "original-notebook"}
			old := biz.WorkloadReference{APIVersion: "apps/v1", Kind: "StatefulSet", Namespace: root.Namespace, Name: root.Name, UID: "old-set"}
			next := old
			next.UID = "accepted-new-set"
			a := biz.Attachment{ID: "att_11111111111111111111111111111111", TenantID: "11111111-1111-4111-8111-111111111111", InstanceID: "instance-one", SubmissionID: "submission-one", Generation: 1, ClusterID: "cluster-one", Namespace: root.Namespace, RootController: root, WorkloadMode: "controller_managed"}
			c := biz.ConsumerSubmission{TenantID: a.TenantID, InstanceID: a.InstanceID, AttachmentID: a.ID, SubmissionID: a.SubmissionID, Generation: a.Generation, ClusterID: a.ClusterID, Namespace: a.Namespace, RootController: root, Controllers: []biz.ControllerReference{{Reference: old, Owner: root}, {Reference: next, Owner: root}}}
			uid, ownerUID := next.UID, root.UID
			switch name {
			case "unaccepted_uid":
				uid = "foreign-set"
			case "foreign_owner":
				ownerUID = "other-notebook"
			case "foreign_tenant":
				c.TenantID = "22222222-2222-4222-8222-222222222222"
			case "foreign_namespace":
				c.Controllers[1].Reference.Namespace = "tenant-other"
			}
			notebook := &unstructured.Unstructured{Object: map[string]any{"apiVersion": root.APIVersion, "kind": root.Kind, "metadata": map[string]any{"name": root.Name, "namespace": root.Namespace, "uid": root.UID}}}
			set := &unstructured.Unstructured{Object: map[string]any{"apiVersion": next.APIVersion, "kind": next.Kind, "metadata": map[string]any{"name": next.Name, "namespace": next.Namespace, "uid": uid, "ownerReferences": []any{map[string]any{"apiVersion": "kubeflow.org/v1beta1", "kind": "Notebook", "name": root.Name, "uid": ownerUID, "controller": true}}}}}
			client := fake.NewSimpleDynamicClient(runtime.NewScheme(), notebook, set)
			p := &KCProvider{client: client}
			work := biz.AttachmentWork{Attachment: a, ConsumerSnapshot: &c, Relations: []byte("[]")}
			exists, e := p.managedControllersExist(context.Background(), work)
			if name != "accepted" {
				var conflict *biz.ProviderError
				if !errors.As(e, &conflict) || conflict.Kind != biz.ProviderConflict {
					t.Fatal("unaccepted controller replacement crossed its original scope", exists, e)
				}
				return
			}
			if e != nil || !exists {
				t.Fatal("accepted replacement was rejected instead of retained as a dependency", exists, e)
			}
			if e = client.Resource(schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"}).Namespace(root.Namespace).Delete(context.Background(), next.Name, metav1.DeleteOptions{}); e != nil {
				t.Fatal(e)
			}
			if e = client.Resource(schema.GroupVersionResource{Group: "kubeflow.org", Version: "v1", Resource: "notebooks"}).Namespace(root.Namespace).Delete(context.Background(), root.Name, metav1.DeleteOptions{}); e != nil {
				t.Fatal(e)
			}
			if exists, e = p.managedControllersExist(context.Background(), work); e != nil || exists {
				t.Fatal("normal controller exit did not release the dependency", exists, e)
			}
		})
	}
}
