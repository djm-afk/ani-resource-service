package vm

import (
	"context"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/fake"
	"testing"
)

func TestMissingVMNamespaceNeverCreatedOrPatched(t *testing.T) {
	client := fake.NewSimpleDynamicClient(runtime.NewScheme())
	e, err := NewEnvironment(&Native{client: client}, "tenant-", func(context.Context, string, string, string, biz.NetworkSelection) (biz.NetworkSelection, error) {
		return biz.NetworkSelection{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.ResolveNamespace(context.Background(), "11111111-1111-4111-8111-111111111111", ""); err == nil {
		t.Fatal("missing Namespace accepted")
	}
	for _, a := range client.Actions() {
		if a.GetVerb() == "create" || a.GetVerb() == "patch" {
			t.Fatalf("forbidden action: %s", a.GetVerb())
		}
	}
}
