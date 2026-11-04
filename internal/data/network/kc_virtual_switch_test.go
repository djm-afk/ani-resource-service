package data

import (
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"testing"
	"time"
)

func switchFixture(t *testing.T) (biz.VirtualSwitch, []unstructured.Unstructured, []unstructured.Unstructured, []unstructured.Unstructured, *unstructured.Unstructured, time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 21, 1, 0, 0, 0, time.UTC)
	node := unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": "n1", "uid": "uid1", "resourceVersion": "1", "labels": map[string]any{allocatedNodeLabel: "true"}, "annotations": map[string]any{"networking.kubercloud.com/chassis": "chassis1"}}, "status": map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}}}
	daemon := unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": "cni1", "namespace": kcSystemNamespace, "labels": map[string]any{"networking.kubercloud.com/app": "cni-ds"}}, "spec": map[string]any{"nodeName": "n1", "containers": []any{map[string]any{"name": "cni"}}}, "status": map[string]any{"phase": "Running", "conditions": []any{map[string]any{"type": "Ready", "status": "True"}}, "containerStatuses": []any{map[string]any{"name": "cni", "ready": true}}}}}
	config := &unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": "kcn-config", "namespace": kcSystemNamespace, "uid": "config1", "resourceVersion": "3"}}}
	return biz.VirtualSwitch{GroupName: "test-group"}, []unstructured.Unstructured{node}, nil, []unstructured.Unstructured{daemon}, config, now
}

func TestVirtualSwitchInventoryUsesOnlyManagementMembership(t *testing.T) {
	sw, nodes, _, _, _, _ := switchFixture(t)
	nodes[0].SetAnnotations(nil)
	nodes[0].Object["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "False"}}}
	got, err := virtualSwitchInventory(sw, "ens35", nodes)
	if err != nil || len(got.Members) != 1 || got.Members[0].NodeUID != "uid1" || got.Inventory.Items[0].Name != "ens35" {
		t.Fatal(got, err)
	}
	sw.BindingID, sw.Nodes, sw.MembershipFingerprint = "binding", got.Members, got.Fingerprint
	bound, err := virtualSwitchInventory(sw, "ens36", nil)
	if err != nil || len(bound.Members) != 1 || bound.Fingerprint != got.Fingerprint {
		t.Fatal("frozen membership was reaudited", bound, err)
	}
}

func TestVirtualSwitchKeepsOwnMetadataIdentityChecks(t *testing.T) {
	sw, nodes, _, _, _, _ := switchFixture(t)
	sw.BindingID = "binding"
	member := biz.VirtualSwitchNode{NodeName: "n1", NodeUID: "uid1", Priority: 100, Allocated: true, Prepared: true}
	nodes[0].SetLabels(map[string]string{virtualSwitchGroupPrefix + sw.GroupName: "100"})
	nodes[0].SetAnnotations(map[string]string{VirtualSwitchOwnerKey(sw.GroupName): sw.BindingID})
	got, err := inspectSwitchNodeObject(sw, member, &nodes[0])
	if err != nil || !got.Applied {
		t.Fatal(got, err)
	}
	nodes[0].SetUID("replacement")
	if _, err = inspectSwitchNodeObject(sw, member, &nodes[0]); err == nil {
		t.Fatal("adopted replacement Node")
	}
}
