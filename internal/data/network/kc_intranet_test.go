package data

import (
	"context"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"testing"
)

func TestIntranetDefaultsConsumeNativeDefaultFlag(t *testing.T) {
	vpc := crFixture("VPC", kcSystemNamespace, "chosen-default")
	vpc.Object["status"] = map[string]any{"default": true}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{kcVPCs: "VPCList"}, vpc)
	p := &KCProvider{client: client}
	got, err := p.GetIntranetPoolDefaults(context.Background())
	if err != nil || got.DefaultVPCName != vpc.GetName() || got.DefaultVPCUID != string(vpc.GetUID()) || len(got.IntranetNetworks) != 0 {
		t.Fatal(got, err)
	}
	for _, action := range client.Actions() {
		if action.GetVerb() != "list" || action.GetResource() != kcVPCs || action.GetNamespace() != kcSystemNamespace {
			t.Fatal("defaults read lower-layer infrastructure", action)
		}
	}
}

func TestIntranetRenderRetainsManagedPoolInput(t *testing.T) {
	c := sqlcgen.NetworkPublicPool{Cidr: "10.232.254.0/24", OvnGatewayIp: "10.232.254.1", ExcludedIps: []string{"10.232.254.1"}, DefaultVpcName: "chosen-default"}
	o := &unstructured.Unstructured{Object: map[string]any{"spec": renderIntranetPool(c)}}
	if crString(o, "spec", "type") != "Intranet" || crString(o, "spec", "gateway") != "kcn-system/chosen-default" {
		t.Fatal(o)
	}
}
