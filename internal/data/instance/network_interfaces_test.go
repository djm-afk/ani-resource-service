package instance

import (
	"context"
	"errors"
	"testing"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/instance"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ktesting "k8s.io/client-go/testing"
)

func TestNetworkInterfaceRebuildsOnlyStoppedOwnedNotebookTemplate(t *testing.T) {
	choice := biz.NetworkInterfaceChoice{Mode: "manual", ProviderName: "retained-vnic", ProviderUID: "retained-vnic-uid"}
	t.Run("old template waits for controller replacement", func(t *testing.T) {
		n, i, client, notebook, set := stoppedLifecycleFixture(t)
		ctx := context.Background()
		ann := notebook.GetAnnotations()
		ann[biz.VNicAnnotation], ann["network.ani.io/interface-revision"], ann["team.example/note"] = choice.ProviderName, "4", "preserved"
		notebook.SetAnnotations(ann)
		if _, err := client.Resource(notebookGVR).Namespace(i.Spec.Namespace).Update(ctx, notebook, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		client.ClearActions()
		if err := n.SetNetworkInterface(ctx, i, choice, 4); !errors.Is(err, biz.ErrNetworkUnavailable) {
			t.Fatal("old template must not start", err)
		}
		deletes := 0
		for _, action := range client.Actions() {
			if action.GetVerb() != "delete" {
				continue
			}
			deletes++
			deletion := action.(ktesting.DeleteAction)
			pre := deletion.GetDeleteOptions().Preconditions
			if action.GetResource() != stsGVR || deletion.GetName() != i.Spec.NotebookName || pre == nil || pre.UID == nil || string(*pre.UID) != string(set.GetUID()) || pre.ResourceVersion == nil || *pre.ResourceVersion != set.GetResourceVersion() {
				t.Fatal("replacement did not bind the exact controlled StatefulSet UID/RV", action)
			}
		}
		if deletes != 1 {
			t.Fatalf("old stopped template was not retired once: deletes=%d", deletes)
		}
		if _, err := client.Resource(stsGVR).Namespace(i.Spec.Namespace).Get(ctx, set.GetName(), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			t.Fatal("old StatefulSet still exists", err)
		}
		if err := n.SetNetworkInterface(ctx, i, choice, 4); !errors.Is(err, biz.ErrNetworkUnavailable) {
			t.Fatal("missing replacement template was treated as applied", err)
		}
		// Only the external Notebook controller creates the replacement. Its
		// v1.10 create path copies root annotations; the update path does not.
		set.SetUID("replacement-set-uid")
		set.SetResourceVersion("30")
		if err := unstructured.SetNestedStringMap(set.Object, ann, "spec", "template", "metadata", "annotations"); err != nil {
			t.Fatal(err)
		}
		if _, err := client.Resource(stsGVR).Namespace(i.Spec.Namespace).Create(ctx, set, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		client.ClearActions()
		if err := n.SetNetworkInterface(ctx, i, choice, 4); err != nil {
			t.Fatal("confirmed controller replacement was not accepted", err)
		}
		for _, action := range client.Actions() {
			if action.GetVerb() != "get" && action.GetVerb() != "list" {
				t.Fatal("matching replacement triggered another native mutation", action)
			}
		}
		root, err := client.Resource(notebookGVR).Namespace(i.Spec.Namespace).Get(ctx, i.Spec.NotebookName, metav1.GetOptions{})
		if err != nil || root.GetUID() != notebook.GetUID() || root.GetAnnotations()[stoppedAnnotation] != "true" || root.GetAnnotations()["team.example/note"] != "preserved" {
			t.Fatal("rebuild changed Notebook ownership, stop state or unrelated annotations", root, err)
		}
		if _, exists, _ := unstructured.NestedMap(root.Object, "spec", "template", "metadata"); exists {
			t.Fatal("rebuild wrote metadata pruned by the Notebook CRD")
		}
	})
	for _, mode := range []string{"online", "foreign controller"} {
		t.Run(mode+" cannot delete", func(t *testing.T) {
			n, i, client, notebook, set := stoppedLifecycleFixture(t)
			ctx := context.Background()
			if mode == "online" {
				ann := notebook.GetAnnotations()
				delete(ann, stoppedAnnotation)
				notebook.SetAnnotations(ann)
				_, _ = client.Resource(notebookGVR).Namespace(i.Spec.Namespace).Update(ctx, notebook, metav1.UpdateOptions{})
			} else {
				owners := set.GetOwnerReferences()
				owners[0].UID = "foreign-notebook-uid"
				set.SetOwnerReferences(owners)
				_, _ = client.Resource(stsGVR).Namespace(i.Spec.Namespace).Update(ctx, set, metav1.UpdateOptions{})
			}
			client.ClearActions()
			if err := n.SetNetworkInterface(ctx, i, choice, 4); err == nil {
				t.Fatal("unsafe template change accepted")
			}
			for _, action := range client.Actions() {
				if action.GetVerb() != "get" && action.GetVerb() != "list" {
					t.Fatal("unsafe template triggered a native mutation", action)
				}
			}
		})
	}
}
