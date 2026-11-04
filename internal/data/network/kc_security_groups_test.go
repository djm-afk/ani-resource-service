package data

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/tenantnamespace"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
)

func sgProviderFixture() (biz.SecurityGroup, *unstructured.Unstructured) {
	g := biz.SecurityGroup{EgressMetadata: biz.EgressMetadata{ID: "sg_example", Version: 1, State: biz.Provisioning}, TenantID: "11111111-1111-4111-8111-111111111111", Namespace: "tenant-11111111-1111-4111-8111-111111111111", ProviderName: "ani-sg-example", ProviderUID: "uid-sg", BindingID: "durable-create-token", Rules: []biz.SecurityGroupRule{
		{ID: "rule-http", Direction: "ingress", Protocol: "tcp", Action: "allow", Priority: 3000, RemoteAddresses: []string{"192.0.2.0/24"}, PortRanges: []biz.SecurityGroupPortRange{{Min: 80, Max: 80}}},
		biz.DefaultSecurityGroupRule(),
	}}
	o := renderSecurityGroup(g)
	o.SetUID(types.UID(g.ProviderUID))
	o.SetResourceVersion("7")
	o.SetGeneration(2)
	o.Object["status"] = map[string]any{"observedGeneration": int64(2), "allowSameGroupTraffic": false, "conditions": []any{
		map[string]any{"type": "Valid", "status": "True", "observedGeneration": int64(2)},
		map[string]any{"type": "Ready", "status": "True", "observedGeneration": int64(2)},
	}}
	return g, o
}

func sgFakeProvider(objects ...runtime.Object) (*KCProvider, *fake.FakeDynamicClient) {
	c := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{kcSecurityGroups: "SecurityGroupList", kcSecurityGroupVNics: "VNicList", pods: "PodList"}, objects...)
	const tenant = "11111111-1111-4111-8111-111111111111"
	ns := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "tenant-" + tenant, "uid": "namespace-uid", "labels": map[string]any{tenantLabel: tenant, ownerLabel: "ani-network-service"}}, "status": map[string]any{"phase": "Active"}}}
	_ = c.Tracker().Add(ns)
	return &KCProvider{client: c, namespaceIdentityReader: func(context.Context, string) (tenantnamespace.Identity, error) {
		return tenantnamespace.Identity{TenantID: tenant, Name: "tenant-" + tenant, UID: "namespace-uid"}, nil
	}}, c
}

func sgResourceWork(group biz.SecurityGroup, operation string) biz.SecurityGroupWork {
	work := biz.SecurityGroupWork{Group: group}
	if operation != "" {
		work.Operation = biz.Operation{ID: "operation", Kind: operation}
	}
	return work
}

func TestSecurityGroupUsesReadyWithoutSecondaryHealthChecks(t *testing.T) {
	g, object := sgProviderFixture()
	object.Object["status"] = map[string]any{"observedGeneration": int64(1), "allowSameGroupTraffic": true, "conditions": []any{
		map[string]any{"type": "Valid", "status": "False"},
		map[string]any{"type": "Ready", "status": "True", "reason": "NativeReady"},
	}}
	g.State = biz.Failed
	provider, client := sgFakeProvider(object)
	result, err := provider.applySecurityGroupResource(context.Background(), sgResourceWork(g, ""))
	if err != nil || result.State != "available" || result.Reason != "" || !result.Observed {
		t.Fatal(result, err)
	}
	if len(client.Actions()) != 1 || client.Actions()[0].GetVerb() != "get" {
		t.Fatal("status observation performed unrelated calls", client.Actions())
	}
	result, err = provider.applySecurityGroupResource(context.Background(), sgResourceWork(g, "update_security_group"))
	if err != nil || result.State != "available" || result.Complete {
		t.Fatal("old SG generation completed current update or changed Ready", result, err)
	}
}

func TestSecurityGroupReadyUnknownRetainsPriorState(t *testing.T) {
	for _, old := range []biz.ResourceState{biz.Provisioning, biz.Available, biz.Degraded, biz.Failed} {
		g, object := sgProviderFixture()
		g.State = old
		object.Object["status"] = map[string]any{}
		provider, _ := sgFakeProvider(object)
		result, err := provider.applySecurityGroupResource(context.Background(), sgResourceWork(g, ""))
		if err != nil || result.State != string(old) || result.Complete || !result.Observed || result.Reason != string(biz.ProviderStatusUnreported) {
			t.Fatal(old, result, err)
		}
	}
}

func TestSecurityGroupOriginalUIDCannotBeRecreatedOrAdopted(t *testing.T) {
	g, object := sgProviderFixture()
	provider, client := sgFakeProvider()
	result, err := provider.applySecurityGroupResource(context.Background(), sgResourceWork(g, "create_security_group"))
	if err != nil || result.Reason != string(biz.ProviderMissing) || !result.Observed || len(client.Actions()) != 1 {
		t.Fatal("known original was recreated", result, err, client.Actions())
	}
	g.ProviderUID = "another-original"
	provider, client = sgFakeProvider(object)
	_, err = provider.applySecurityGroupResource(context.Background(), sgResourceWork(g, ""))
	var conflict *biz.ProviderError
	if !errors.As(err, &conflict) || conflict.Kind != biz.ProviderConflict || len(client.Actions()) != 1 {
		t.Fatal("replacement was adopted", err, client.Actions())
	}
	g.State = biz.Deleting
	result, err = provider.applySecurityGroupResource(context.Background(), sgResourceWork(g, "delete_security_group"))
	if err != nil || result.State != "deleted" || !result.Complete || len(client.Actions()) != 2 {
		t.Fatal("replacement was mutated during original deletion", result, err, client.Actions())
	}
}

func TestSecurityGroupConditionalPatchUsesOwnUIDAndResourceVersion(t *testing.T) {
	g, object := sgProviderFixture()
	g.Rules[0].PortRanges[0] = biz.SecurityGroupPortRange{Min: 443, Max: 443}
	provider, client := sgFakeProvider(object)
	patched := false
	client.PrependReactor("patch", "securitygroups", func(action ktesting.Action) (bool, runtime.Object, error) {
		var patch []map[string]any
		if err := json.Unmarshal(action.(ktesting.PatchAction).GetPatch(), &patch); err != nil {
			t.Fatal(err)
		}
		if len(patch) != 3 || patch[0]["path"] != "/metadata/uid" || patch[0]["value"] != "uid-sg" || patch[1]["path"] != "/metadata/resourceVersion" || patch[1]["value"] != "7" {
			t.Fatal(patch)
		}
		patched = true
		return false, nil, nil
	})
	if _, err := provider.applySecurityGroupResource(context.Background(), sgResourceWork(g, "update_security_group")); err != nil || !patched {
		t.Fatal(patched, err)
	}
}

func TestSecurityGroupDeleteUsesOriginalObjectWithoutMembershipAudit(t *testing.T) {
	g, object := sgProviderFixture()
	g.State = biz.Deleting
	_ = unstructured.SetNestedStringSlice(object.Object, []string{"old-vnic"}, "status", "appliedVNics")
	_ = unstructured.SetNestedStringMap(object.Object, map[string]string{"old-eip": "node"}, "status", "appliedEIPs")
	object.SetFinalizers([]string{"networking.kubercloud.com/security-group"})
	provider, client := sgFakeProvider(object)
	client.PrependReactor("delete", "securitygroups", func(action ktesting.Action) (bool, runtime.Object, error) {
		options := action.(ktesting.DeleteAction).GetDeleteOptions()
		if options.Preconditions == nil || options.Preconditions.UID == nil || *options.Preconditions.UID != "uid-sg" || options.Preconditions.ResourceVersion == nil || *options.Preconditions.ResourceVersion != "7" {
			t.Fatal(options)
		}
		return true, nil, nil
	})
	result, err := provider.applySecurityGroupResource(context.Background(), sgResourceWork(g, "delete_security_group"))
	if err != nil || result.Complete || result.State != "deleting" || len(client.Actions()) != 2 {
		t.Fatal(result, err, client.Actions())
	}
	for _, action := range client.Actions() {
		if action.GetVerb() != "get" && action.GetVerb() != "delete" {
			t.Fatal("membership or finalizer bypass", action)
		}
	}
	provider, _ = sgFakeProvider()
	result, err = provider.applySecurityGroupResource(context.Background(), sgResourceWork(g, "delete_security_group"))
	if err != nil || result.State != "deleted" || !result.Complete || !result.Observed {
		t.Fatal(result, err)
	}
}

func TestSecurityGroupReadErrorKeepsProjection(t *testing.T) {
	g, object := sgProviderFixture()
	g.State, g.Reason = biz.Available, "last-native-reason"
	provider, client := sgFakeProvider(object)
	client.PrependReactor("get", "securitygroups", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("unavailable")
	})
	result, err := provider.applySecurityGroupResource(context.Background(), sgResourceWork(g, ""))
	if err == nil || result.Observed || result.State != "available" || result.Reason != "last-native-reason" || result.ObservationError != biz.ProviderUnavailable {
		t.Fatal(result, err)
	}
}

func TestSecurityGroupUnknownCreateRetriesSameNameOnlyWhileActive(t *testing.T) {
	g, _ := sgProviderFixture()
	g.ProviderUID = ""
	provider, client := sgFakeProvider()
	result, err := provider.applySecurityGroupResource(context.Background(), sgResourceWork(g, ""))
	if err != nil || result.Reason != string(biz.ProviderMissing) || len(client.Actions()) != 1 {
		t.Fatal("idle missing object recreated", result, err, client.Actions())
	}
	client.PrependReactor("create", "securitygroups", func(action ktesting.Action) (bool, runtime.Object, error) {
		object := action.(ktesting.CreateAction).GetObject().(*unstructured.Unstructured).DeepCopy()
		object.SetUID("new-original")
		object.SetResourceVersion("1")
		return true, object, nil
	})
	result, err = provider.applySecurityGroupResource(context.Background(), sgResourceWork(g, "create_security_group"))
	if err != nil || result.ProviderUID != "new-original" || !result.Observed {
		t.Fatal(result, err)
	}
}
