package sandbox

import (
	"context"
	"errors"
	"fmt"
	"testing"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/sandbox"
	storage "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/storage"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
)

const namespaceTenant = "11111111-1111-4111-8111-111111111111"

func ownedNamespace(uid string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Namespace",
		"metadata": map[string]any{"name": "tenant-" + namespaceTenant, "uid": uid, "labels": map[string]any{
			storage.NamespaceManagedBy: storage.NamespaceOwner, storage.NamespaceTenantID: namespaceTenant,
		}},
		"status": map[string]any{"phase": "Active"},
	}}
}

func TestResolveSandboxNamespaceRejectsUnsafeExistingWithoutMutation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		alter func(*unstructured.Unstructured)
	}{
		{"foreign tenant", func(ns *unstructured.Unstructured) {
			ns.SetLabels(map[string]string{storage.NamespaceManagedBy: storage.NamespaceOwner, storage.NamespaceTenantID: "other"})
		}},
		{"deleting", func(ns *unstructured.Unstructured) { now := metav1.Now(); ns.SetDeletionTimestamp(&now) }},
		{"missing uid", func(ns *unstructured.Unstructured) { ns.SetUID("") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ns := ownedNamespace("owned-uid")
			tc.alter(ns)
			client := fake.NewSimpleDynamicClient(runtime.NewScheme(), ns)
			_, err := (TenantNamespace{Client: client, NamespacePrefix: "tenant-"}).ResolveSandboxNamespaceUID(context.Background(), namespaceTenant)
			if !errors.Is(err, biz.ErrOwnership) {
				t.Fatalf("unsafe namespace accepted: %v", err)
			}
			for _, a := range client.Actions() {
				if a.GetVerb() != "get" {
					t.Fatalf("unexpected mutation: %s", a.GetVerb())
				}
			}
		})
	}
}

func TestResolveSandboxNamespaceNeverCreates(t *testing.T) {
	client := fake.NewSimpleDynamicClient(runtime.NewScheme())
	_, err := (TenantNamespace{Client: client, NamespacePrefix: "tenant-"}).ResolveSandboxNamespaceUID(context.Background(), namespaceTenant)
	if !errors.Is(err, biz.ErrDependencyUnavailable) {
		t.Fatalf("missing namespace: %v", err)
	}
	for _, a := range client.Actions() {
		if a.GetVerb() != "get" {
			t.Fatal(fmt.Sprintf("unexpected mutation: %s", a.GetVerb()))
		}
	}
}

func TestNamespaceReadPreservesCancellationWithoutProviderDetails(t *testing.T) {
	for _, want := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(want.Error(), func(t *testing.T) {
			client := fake.NewSimpleDynamicClient(runtime.NewScheme())
			client.PrependReactor("get", "namespaces", func(ktesting.Action) (bool, runtime.Object, error) {
				return true, nil, fmt.Errorf("private provider detail: %w", want)
			})
			_, err := (TenantNamespace{Client: client, NamespacePrefix: "tenant-"}).ResolveSandboxNamespaceUID(context.Background(), namespaceTenant)
			if err != want {
				t.Fatalf("want cancellation without provider detail, got %v", err)
			}
		})
	}
}
