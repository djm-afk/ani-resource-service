package data

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/tenantnamespace"
	"testing"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

func TestSecurityGroupKCNDefaultsAndGeneration(t *testing.T) {
	g, o := sgProviderFixture()
	ports, _, _ := unstructured.NestedSlice(o.Object, "spec", "ingressRules")
	if len(ports) != 2 || ports[0].(map[string]any)["priority"] != int64(3000) || ports[1].(map[string]any)["action"] != "drop" {
		t.Fatal("ingress allow and low priority default drop must retain KCN priority semantics")
	}
	egress, _, _ := unstructured.NestedSlice(o.Object, "spec", "egressRules")
	if len(egress) != 0 || crBool(o, "spec", "allowSameGroupTraffic") {
		t.Fatal("default rules must leave outbound traffic unrestricted and disable intra-group bypass")
	}
	p, _ := sgFakeProvider(o)
	r, err := p.applySecurityGroupResource(context.Background(), g)
	if err != nil || !r.Complete || r.State != "available" {
		t.Fatalf("current generation did not become available: %+v %v", r, err)
	}
	for _, mutate := range []func(*unstructured.Unstructured){
		func(o *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(o.Object, int64(1), "status", "observedGeneration")
		},
		func(o *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(o.Object, true, "status", "allowSameGroupTraffic")
		},
		func(o *unstructured.Unstructured) { o.SetGeneration(3) },
	} {
		copy := o.DeepCopy()
		mutate(copy)
		p, _ = sgFakeProvider(copy)
		r, err = p.applySecurityGroupResource(context.Background(), g)
		if err != nil || r.Complete {
			t.Fatalf("stale or incomplete KCN application was accepted: %+v %v", r, err)
		}
	}
}

func TestSecurityGroupKCNRecoversOnlyItsDurableCreateToken(t *testing.T) {
	g, o := sgProviderFixture()
	g.ProviderUID = "" // the create response was lost before UID persistence
	p, c := sgFakeProvider(o)
	r, err := p.applySecurityGroupResource(context.Background(), g)
	if err != nil || r.ProviderUID != "uid-sg" || !r.Complete {
		t.Fatalf("durable create token did not recover its object: %+v %v", r, err)
	}
	for _, action := range c.Actions() {
		if action.GetVerb() != "get" {
			t.Fatal("recovery wrote a duplicate object", action)
		}
	}
	for _, field := range []string{ownerLabel, tenantLabel, resourceLabel, bindingLabel} {
		copy := o.DeepCopy()
		labels := copy.GetLabels()
		labels[field] = "unrelated"
		copy.SetLabels(labels)
		p, c = sgFakeProvider(copy)
		_, err = p.applySecurityGroupResource(context.Background(), g)
		var provider *biz.ProviderError
		if !errors.As(err, &provider) || provider.Kind != biz.ProviderConflict || len(c.Actions()) != 2 {
			t.Fatal("foreign create token was adopted or mutated", field, err, c.Actions())
		}
	}
	g.ProviderUID = "original-uid"
	p, _ = sgFakeProvider(o)
	if _, err = p.applySecurityGroupResource(context.Background(), g); err == nil {
		t.Fatal("replacement object with copied labels was adopted")
	}
	p, c = sgFakeProvider()
	if _, err = p.applySecurityGroupResource(context.Background(), g); err == nil || len(c.Actions()) != 2 {
		t.Fatal("missing known-UID object was recreated", err, c.Actions())
	}
}

func TestSecurityGroupKCNUpdateTestsUIDAndResourceVersion(t *testing.T) {
	g, o := sgProviderFixture()
	g.Rules[0].PortRanges[0] = biz.SecurityGroupPortRange{Min: 443, Max: 443}
	p, c := sgFakeProvider(o)
	patched := false
	c.PrependReactor("patch", "securitygroups", func(a ktesting.Action) (bool, runtime.Object, error) {
		pa := a.(ktesting.PatchAction)
		var patch []map[string]any
		if err := json.Unmarshal(pa.GetPatch(), &patch); err != nil {
			t.Fatal(err)
		}
		if pa.GetPatchType() != types.JSONPatchType || len(patch) != 3 || patch[0]["op"] != "test" || patch[0]["path"] != "/metadata/uid" || patch[0]["value"] != "uid-sg" || patch[1]["op"] != "test" || patch[1]["path"] != "/metadata/resourceVersion" || patch[1]["value"] != "7" {
			t.Fatal("update omitted conditional identity protection", patch)
		}
		patched = true
		result := renderSecurityGroup(g)
		result.SetUID("uid-sg")
		result.SetResourceVersion("8")
		result.SetGeneration(3)
		return true, result, nil
	})
	r, err := p.applySecurityGroupResource(context.Background(), g)
	if err != nil || !patched || r.Complete {
		t.Fatalf("update was skipped or claimed success before controller application: %+v %v", r, err)
	}
}

func TestSecurityGroupKCNDeleteHonorsMembershipUIDAndFinalizers(t *testing.T) {
	g, o := sgProviderFixture()
	g.State = biz.Deleting
	_ = unstructured.SetNestedStringSlice(o.Object, []string{"vnic-a"}, "status", "appliedVNics")
	p, c := sgFakeProvider(o)
	r, err := p.applySecurityGroupResource(context.Background(), g)
	var provider *biz.ProviderError
	if !errors.As(err, &provider) || provider.Kind != biz.ProviderInUse || r.Complete || len(c.Actions()) != 2 {
		t.Fatal("SG with applied members was deleted", r, err, c.Actions())
	}
	unstructured.RemoveNestedField(o.Object, "status", "appliedVNics")
	o.SetFinalizers([]string{"networking.kubercloud.com/security-group"})
	p, c = sgFakeProvider(o)
	deleted := false
	c.PrependReactor("delete", "securitygroups", func(a ktesting.Action) (bool, runtime.Object, error) {
		opts := a.(ktesting.DeleteAction).GetDeleteOptions()
		if opts.Preconditions == nil || opts.Preconditions.UID == nil || *opts.Preconditions.UID != "uid-sg" || opts.Preconditions.ResourceVersion == nil || *opts.Preconditions.ResourceVersion != "7" {
			t.Fatal("delete omitted exact UID/RV preconditions", opts)
		}
		deleted = true
		// API accepted deletion but KCN still owns its finalizer.
		return true, nil, nil
	})
	r, err = p.applySecurityGroupResource(context.Background(), g)
	if err != nil || !deleted || r.Complete || r.State != "deleting" {
		t.Fatal("DELETE acceptance was confused with finalizer completion", r, err)
	}
	for _, action := range c.Actions() {
		if action.GetVerb() == "patch" || action.GetVerb() == "update" {
			t.Fatal("provider removed KCN finalizers", action)
		}
	}
	p, c = sgFakeProvider()
	r, err = p.applySecurityGroupResource(context.Background(), g)
	if err != nil || !r.Complete || r.State != "deleted" {
		t.Fatal("absent deleted object did not finish", r, err, c.Actions())
	}
}

func TestEIPSecurityGroupApplicationRequiresActualBoundGateway(t *testing.T) {
	eip := crFixture("EIP", "tenant-11111111-1111-4111-8111-111111111111", "eip-a")
	eip.Object["status"] = map[string]any{
		"phase": "Available", "appliedSecurityGroups": []any{"sg-a"},
		"conditions": []any{map[string]any{"type": "Valid", "status": "True"}, map[string]any{"type": "Ready", "status": "True"}},
	}
	if eipReadyForSecurityGroups(eip) {
		t.Fatal("EIP status group existence was accepted as applied network policy")
	}
	_ = unstructured.SetNestedField(eip.Object, "Bound", "status", "phase")
	_ = unstructured.SetNestedField(eip.Object, "gateway-a", "status", "gateway")
	_ = unstructured.SetNestedMap(eip.Object, map[string]any{"vpc": "tenant-a/vpc-a", "nodeName": "node-a", "disabled": false}, "status", "boundResource")
	if !eipReadyForSecurityGroups(eip) {
		t.Fatal("active bound EIP was not eligible for application check")
	}
	_ = unstructured.SetNestedField(eip.Object, true, "status", "boundResource", "disabled")
	if eipReadyForSecurityGroups(eip) {
		t.Fatal("disabled EIP was accepted as protected")
	}
}

func sgConsumerFixtures() (biz.SecurityGroupBinding, *unstructured.Unstructured, *unstructured.Unstructured) {
	b := biz.SecurityGroupBinding{TenantID: "11111111-1111-4111-8111-111111111111", TargetType: "container", TargetID: "instance-a", Namespace: "tenant-11111111-1111-4111-8111-111111111111", SecurityGroupIDs: []string{}}
	pod := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{"name": "instance-a-0", "namespace": b.Namespace, "uid": "pod-current", "resourceVersion": "10",
			"labels":      map[string]any{tenantLabel: b.TenantID, instanceLabel: b.TargetID},
			"annotations": map[string]any{"networking.kubercloud.com/vnic": "auto-vnic-a", "networking.kubercloud.com/security_groups": ""}},
	}}
	vnic := crFixture("VNic", b.Namespace, "auto-vnic-a")
	vnic.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "v1", Kind: "Pod", Name: pod.GetName(), UID: pod.GetUID()}})
	return b, pod, vnic
}

func TestSecurityGroupConsumerObservesPodVNicOwnershipAndUnbinding(t *testing.T) {
	b, pod, vnic := sgConsumerFixtures()
	p, _ := sgFakeProvider(pod, vnic)
	applied, err := p.ObserveSecurityGroupConsumer(context.Background(), b)
	if err != nil || !applied {
		t.Fatal("unbound live consumer was not observed", applied, err)
	}
	copy := vnic.DeepCopy()
	copy.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "v1", Kind: "Pod", Name: pod.GetName(), UID: "old-pod-uid"}})
	p, _ = sgFakeProvider(pod, copy)
	applied, err = p.ObserveSecurityGroupConsumer(context.Background(), b)
	var failure *biz.ProviderError
	if applied || !errors.As(err, &failure) || failure.Kind != biz.ProviderConflict {
		t.Fatal("same-name Pod with a replacement UID inherited an old VNic", applied, err)
	}
	_, group := sgProviderFixture()
	_ = unstructured.SetNestedStringSlice(group.Object, []string{vnic.GetName()}, "status", "appliedVNics")
	p, _ = sgFakeProvider(pod, vnic, group)
	applied, err = p.ObserveSecurityGroupConsumer(context.Background(), b)
	if applied || !errors.As(err, &failure) || failure.Kind != biz.ProviderTemporary {
		t.Fatal("unbind completed while an old group still applied the VNic", applied, err)
	}
}

func TestSecurityGroupConsumerRetiringVNicBlocksRelease(t *testing.T) {
	b, pod, vnic := sgConsumerFixtures()
	now := metav1.Now()
	pod.SetDeletionTimestamp(&now)
	p, _ := sgFakeProvider(pod, vnic)
	applied, err := p.ObserveSecurityGroupConsumer(context.Background(), b)
	var failure *biz.ProviderError
	if applied || !errors.As(err, &failure) || failure.Kind != biz.ProviderTemporary {
		t.Fatal("retiring Pod retained a VNic while old references were released", applied, err)
	}
}

func TestSecurityGroupConsumerHistoricalVNicUIDGuardsStoppedRelease(t *testing.T) {
	b, _, vnic := sgConsumerFixtures()
	relation := attachmentRelation{Kind: "VNic", Namespace: b.Namespace, Name: vnic.GetName(), UID: string(vnic.GetUID())}
	encoded, err := json.Marshal([]attachmentRelation{relation})
	if err != nil {
		t.Fatal(err)
	}
	history := [][]byte{encoded}
	p, _ := sgFakeProvider(vnic)
	err = p.observeRetiredSecurityGroupVNics(context.Background(), b, history)
	var failure *biz.ProviderError
	if !errors.As(err, &failure) || failure.Kind != biz.ProviderTemporary {
		t.Fatal("missing Pod released a still-live historical VNic", err)
	}
	copy := vnic.DeepCopy()
	copy.SetUID("replacement-vnic-uid")
	p, _ = sgFakeProvider(copy)
	err = p.observeRetiredSecurityGroupVNics(context.Background(), b, history)
	if !errors.As(err, &failure) || failure.Kind != biz.ProviderConflict {
		t.Fatal("same-name replacement VNic was rebound to a historical UID", err)
	}
	p, _ = sgFakeProvider()
	if err = p.observeRetiredSecurityGroupVNics(context.Background(), b, history); err != nil {
		t.Fatal("absent historical VNic prevented stopped configuration", err)
	}
	for _, invalid := range [][][]byte{
		{[]byte("invalid-json")},
		{sgJSON([]attachmentRelation{{Kind: "VNic", Namespace: "another-tenant", Name: relation.Name, UID: relation.UID}})},
		{sgJSON([]attachmentRelation{{Kind: "VNic", Namespace: b.Namespace, Name: relation.Name}})},
	} {
		if err = p.observeRetiredSecurityGroupVNics(context.Background(), b, invalid); err == nil {
			t.Fatal("invalid historical provider identity was ignored", invalid)
		}
	}
}

func TestSecurityGroupConsumerWaitsForPodAnnotationAndAutoVNic(t *testing.T) {
	b, pod, vnic := sgConsumerFixtures()
	for _, mutate := range []func(*unstructured.Unstructured){
		func(o *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(o.Object, "unexpected-sg", "metadata", "annotations", "networking.kubercloud.com/security_groups")
		},
		func(o *unstructured.Unstructured) {
			unstructured.RemoveNestedField(o.Object, "metadata", "annotations", "networking.kubercloud.com/vnic")
		},
	} {
		copy := pod.DeepCopy()
		mutate(copy)
		p, _ := sgFakeProvider(copy, vnic)
		applied, err := p.ObserveSecurityGroupConsumer(context.Background(), b)
		var failure *biz.ProviderError
		if applied || !errors.As(err, &failure) || failure.Kind != biz.ProviderTemporary {
			t.Fatal("pending Pod declaration was marked applied", applied, err)
		}
	}
}

func TestEIPSecurityGroupDeletionWaitsForAllOwnedMemberships(t *testing.T) {
	_, group := sgProviderFixture()
	_ = unstructured.SetNestedStringMap(group.Object, map[string]string{"old-eip": "uplink.tenant-a.vpc-a"}, "status", "appliedEIPs")
	p, _ := sgFakeProvider(group)
	clean, err := p.eipSecurityGroupMembershipsRemoved(context.Background(), "11111111-1111-4111-8111-111111111111", "tenant-11111111-1111-4111-8111-111111111111", "old-eip")
	if err != nil || clean {
		t.Fatal("deleted EIP was released while SG ACL membership remained", clean, err)
	}
	_ = unstructured.SetNestedStringMap(group.Object, map[string]string{}, "status", "appliedEIPs")
	p, _ = sgFakeProvider(group)
	clean, err = p.eipSecurityGroupMembershipsRemoved(context.Background(), "11111111-1111-4111-8111-111111111111", "tenant-11111111-1111-4111-8111-111111111111", "old-eip")
	if err != nil || !clean {
		t.Fatal("cleared deleted-EIP membership did not release", clean, err)
	}
}
