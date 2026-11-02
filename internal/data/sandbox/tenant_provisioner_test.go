package sandbox

import (
	"context"
	"errors"
	"testing"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/sandbox"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestTenantProvisionerUsesActualServiceAccount(t *testing.T) {
	for _, tc := range []struct {
		name, username, namespace, serviceAccount string
		fail                                      bool
	}{
		{"service account", "system:serviceaccount:ani-system:ani-resource-service", "ani-system", "ani-resource-service", false},
		{"administrator", "kubernetes-admin", "", "", true},
		{"malformed", "system:serviceaccount::service", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := fake.NewSimpleDynamicClient(runtime.NewScheme())
			client.PrependReactor("create", "selfsubjectreviews", func(ktesting.Action) (bool, runtime.Object, error) {
				return true, &unstructured.Unstructured{Object: map[string]any{"status": map[string]any{"userInfo": map[string]any{"username": tc.username}}}}, nil
			})
			namespace, name, err := currentServiceAccount(context.Background(), client)
			if (err != nil) != tc.fail || namespace != tc.namespace || name != tc.serviceAccount {
				t.Fatalf("namespace=%q name=%q err=%v", namespace, name, err)
			}
			if tc.fail && !errors.Is(err, biz.ErrDependencyUnavailable) {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}
