package data

import (
	"context"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/tenantnamespace"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/fake"
	"testing"
)

func TestMissingNetworkNamespaceCannotBeCreatedOrTreatedAsProviderAbsence(t *testing.T) {
	const tenant = "11111111-1111-4111-8111-111111111111"
	client := fake.NewSimpleDynamicClient(runtime.NewScheme())
	p := &KCProvider{client: client, repository: &Postgres{placement: Placement{ClusterID: "cluster-a"}}, namespaceIdentityReader: func(context.Context, string) (tenantnamespace.Identity, error) {
		return tenantnamespace.Identity{TenantID: tenant, ClusterID: "cluster-a", Name: "tenant-" + tenant, UID: "original-uid"}, nil
	}}
	if _, err := p.resolveTenantNamespace(context.Background(), tenant); err == nil {
		t.Fatal("missing namespace resolved")
	}
	if err := p.verifyTenantNamespace(context.Background(), sqlcgen.NetworkProviderBinding{TenantID: tenant, ClusterID: "cluster-a", Namespace: "tenant-" + tenant}); err == nil {
		t.Fatal("missing namespace treated as successful provider absence")
	}
	for _, a := range client.Actions() {
		if a.GetVerb() == "create" || a.GetVerb() == "patch" {
			t.Fatalf("forbidden Namespace action %s", a.GetVerb())
		}
	}
}
