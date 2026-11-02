package data

import (
	"context"
	imagebiz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/image"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/fake"
	"testing"
)

func TestModelDevDynamicNamespaceAndSecretWriterBoundaries(t *testing.T) {
	const tenant = "11111111-1111-4111-8111-111111111111"
	for _, name := range []string{"valid", "missing", "wrong-label", "wrong-uid", "terminating", "cluster-mismatch"} {
		t.Run(name, func(t *testing.T) {
			ns := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "tenant-" + tenant, "uid": "original-uid", "labels": map[string]any{"network.ani.io/tenant-id": tenant, "network.ani.io/managed-by": "ani-network-service"}}, "status": map[string]any{"phase": "Active"}}}
			if name == "wrong-label" {
				ns.SetLabels(map[string]string{"ani.io/resource-tenant-id": tenant, "ani.io/managed-by": "ani-modeldev-service"})
			}
			if name == "wrong-uid" {
				ns.SetUID("replacement")
			}
			if name == "terminating" {
				ns.Object["status"] = map[string]any{"phase": "Terminating"}
			}
			client := fake.NewSimpleDynamicClient(runtime.NewScheme())
			if name != "missing" {
				_ = client.Tracker().Add(ns)
			}
			p := &PullSecrets{client: client, clusterID: "cluster-a"}
			target := imagebiz.PullSecretTarget{TenantID: tenant, ClusterID: "cluster-a", Namespace: "tenant-" + tenant, NamespaceUID: "original-uid", StandardTenantNamespace: true, SecretName: "ani-pull-example", SpaceID: "space-a", RegistryAuthority: "registry.test", Generation: 1, Username: "robot-pull"}
			if name == "cluster-mismatch" {
				target.ClusterID = "cluster-b"
			}
			_, err := p.EnsurePullSecret(context.Background(), target, imagebiz.Secret("test-only-password"))
			if (err == nil) != (name == "valid") {
				t.Fatalf("write result: %v", err)
			}
			for _, a := range client.Actions() {
				if a.GetVerb() == "create" || a.GetVerb() == "patch" || a.GetVerb() == "update" {
					if name != "valid" || a.GetResource().Resource != "secrets" {
						t.Fatalf("forbidden writer action: %s %s", a.GetVerb(), a.GetResource().Resource)
					}
				}
			}
		})
	}
}
