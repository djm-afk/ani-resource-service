package data

import (
	"context"
	"errors"
	"testing"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/fake"
)

func TestPlatformObservationReadsOriginalObjectWithoutOwningIt(t *testing.T) {
	object := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "networking.kubercloud.com/v1", "kind": "Subnet", "metadata": map[string]any{"namespace": "kcn-system", "name": "workload", "uid": "original"}, "spec": map[string]any{"cidrBlock": "10.9.0.0/24", "gatewayIP": "10.9.0.1", "enableDHCP": true}, "status": map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "False", "reason": "NativeReason"}}, "v4availableIPrange": "10.9.0.2..10.9.0.254"}}}
	client := fake.NewSimpleDynamicClient(runtime.NewScheme(), object)
	provider := &KCProvider{repository: &Postgres{placement: Placement{ClusterID: "cluster"}}, client: client}
	target := biz.ProviderTarget{PlatformNetwork: &biz.PlatformNetworkWorkSpec{Kind: "subnet", ClusterID: "cluster", Namespace: "kcn-system", Name: "workload", UID: "original"}}
	observed, err := provider.Observe(context.Background(), target)
	if err != nil || !observed.Exists || observed.Ready == nil || *observed.Ready || observed.StatusReason != "NativeReason" {
		t.Fatal(observed, err)
	}
	if observed.PlatformNetwork == nil || observed.PlatformNetwork.CIDR != "10.9.0.0/24" || observed.PlatformNetwork.Gateway != "10.9.0.1" || observed.PlatformNetwork.Available == nil {
		t.Fatal(observed.PlatformNetwork)
	}
	if len(client.Actions()) != 1 || client.Actions()[0].GetVerb() != "get" {
		t.Fatal("directory observation mutated or enumerated infrastructure", client.Actions())
	}
	// Original identity, not labels or Ready, determines which object is read.
	target.PlatformNetwork.UID = "replaced"
	_, err = provider.Observe(context.Background(), target)
	var conflict *biz.ProviderError
	if !errors.As(err, &conflict) || conflict.Kind != biz.ProviderConflict {
		t.Fatal("same-name replacement was accepted", err)
	}
}

func TestSharedParentReferencesKeepActualTenantConsumerIdentity(t *testing.T) {
	privateVPC, privateSubnet := "vpc_private", "subnet_private"
	platformVPC, platformSubnet := "vpc_platform", "subnet_platform"
	private := normalizeNetworkReference(&privateVPC, &privateSubnet, nil, nil, "private-binding")
	platform := normalizeNetworkReference(nil, nil, &platformVPC, &platformSubnet, "platform-binding")
	if private.OwnerScope != biz.OwnerScopeTenant || private.VPCID != privateVPC || private.SubnetBindingID != "private-binding" {
		t.Fatal(private)
	}
	if platform.OwnerScope != biz.OwnerScopePlatform || platform.VPCID != platformVPC || platform.SubnetID != platformSubnet || platform.SubnetBindingID != "platform-binding" {
		t.Fatal(platform)
	}
}
