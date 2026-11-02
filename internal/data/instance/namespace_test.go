package instance

import (
	"context"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/fake"
	"testing"
)

func TestMissingInstanceNamespaceNeverCreatedOrPatched(t *testing.T) {
	client := fake.NewSimpleDynamicClient(runtime.NewScheme())
	n, _ := NewNotebook(client)
	if err := n.ValidateEnvironment(context.Background(), "11111111-1111-4111-8111-111111111111", "tenant-11111111-1111-4111-8111-111111111111", "workload", "disk"); err == nil {
		t.Fatal("missing Namespace accepted")
	}
	for _, a := range client.Actions() {
		if a.GetVerb() == "create" || a.GetVerb() == "patch" {
			t.Fatalf("forbidden action: %s", a.GetVerb())
		}
	}
}
