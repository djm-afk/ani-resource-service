package sandbox

import (
	"context"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/fake"
	"testing"
)

func TestMissingSandboxNamespaceNeverCreatedOrPatched(t *testing.T) {
	client := fake.NewSimpleDynamicClient(runtime.NewScheme())
	_, err := (TenantNamespace{Client: client, NamespacePrefix: "tenant-"}).ResolveSandboxNamespaceUID(context.Background(), "11111111-1111-4111-8111-111111111111")
	if err == nil {
		t.Fatal("missing Namespace accepted")
	}
	for _, a := range client.Actions() {
		if a.GetVerb() == "create" || a.GetVerb() == "patch" {
			t.Fatalf("forbidden Namespace action %s", a.GetVerb())
		}
	}
}
