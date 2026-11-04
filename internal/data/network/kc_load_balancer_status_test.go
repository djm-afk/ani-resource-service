package data

import (
	"context"
	"testing"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func lbNativeConditions(kinds ...string) []any {
	out := []any{}
	for _, kind := range kinds {
		out = append(out, map[string]any{"type": kind, "status": "True"})
	}
	return out
}

func TestLBStatusUsesControllerAcknowledgements(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"generation": int64(4)}, "status": map[string]any{"conditions": lbNativeConditions("Accepted", "Programmed"), "listeners": []any{map[string]any{"name": "web", "conditions": lbNativeConditions("Accepted", "Programmed", "ResolvedRefs")}}}}}
	desired := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"listeners": []any{map[string]any{"name": "web"}}}}}
	ready, reason := lbObjectReady(obj, desired, biz.LoadBalancerComponent{Kind: "gateway"}, "lb", "tenant-a")
	if ready == nil || !*ready || reason != "" {
		t.Fatal(ready, reason)
	}
	obj.Object["status"].(map[string]any)["conditions"] = []any{map[string]any{"type": "Accepted", "status": "False", "reason": "UnsupportedProtocol"}}
	ready, reason = lbObjectReady(obj, desired, biz.LoadBalancerComponent{Kind: "gateway"}, "lb", "tenant-a")
	if ready == nil || *ready || reason != "UnsupportedProtocol" {
		t.Fatal(ready, reason)
	}
	obj.Object["status"] = map[string]any{}
	ready, _ = lbObjectReady(obj, desired, biz.LoadBalancerComponent{Kind: "gateway"}, "lb", "tenant-a")
	if ready != nil {
		t.Fatal("missing acknowledgement became failure")
	}
}

func TestLBRouteSelectsItsControllerAndParent(t *testing.T) {
	entry := map[string]any{"controllerName": lbController, "parentRef": map[string]any{"name": "lb", "sectionName": "web"}, "conditions": lbNativeConditions("Accepted", "ResolvedRefs")}
	obj := &unstructured.Unstructured{Object: map[string]any{"status": map[string]any{"parents": []any{entry}}}}
	component := biz.LoadBalancerComponent{Kind: "route", ListenerName: "web"}
	ready, _ := lbObjectReady(obj, obj, component, "lb", "tenant-a")
	if ready == nil || !*ready {
		t.Fatal("own parent acknowledgement lost")
	}
	entry["controllerName"] = "another-controller"
	ready, _ = lbObjectReady(obj, obj, component, "lb", "tenant-a")
	if ready != nil {
		t.Fatal("another controller accepted the route")
	}
}

func TestLBCapabilityPerformsOnlyFixedGatewayClassGet(t *testing.T) {
	gc := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "gateway.networking.k8s.io/v1", "kind": "GatewayClass", "metadata": map[string]any{"name": biz.LoadBalancerGatewayClass}, "status": map[string]any{"conditions": lbNativeConditions("Accepted")}}}
	client := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), gc)
	provider := &KCProvider{client: client}
	ready, _, err := provider.ReadLoadBalancerCapability(context.Background())
	if err != nil || ready == nil || !*ready {
		t.Fatal(ready, err)
	}
	actions := client.Actions()
	if len(actions) != 1 || actions[0].GetVerb() != "get" || actions[0].GetResource() != lbGatewayClasses {
		t.Fatal(actions)
	}
}

func TestLBManagedComparisonWithdrawsRemovedBackends(t *testing.T) {
	actual := map[string]any{"rules": []any{map[string]any{"name": "default-rule", "backendRefs": []any{map[string]any{"name": "old-backend"}}}}, "providerDefault": "accepted"}
	desired := map[string]any{"rules": []any{map[string]any{"name": "default-rule"}}}
	if lbManagedSpecMatches(actual, desired, "route") {
		t.Fatal("removed backend remained in the owned Route")
	}
	delete(actual["rules"].([]any)[0].(map[string]any), "backendRefs")
	if !lbManagedSpecMatches(actual, desired, "route") {
		t.Fatal("unowned provider default caused a rewrite")
	}
}
