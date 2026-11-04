package data

import (
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"testing"
)

func TestSwitchDeletionUsesOnlyOwnMetadata(t *testing.T) {
	sw, nodes, _, _, _, _ := switchFixture(t)
	sw.BindingID, sw.State = "binding", biz.Deleting
	member := biz.VirtualSwitchNode{NodeName: "n1", NodeUID: "uid1", Priority: 100, Allocated: true, Prepared: true, DeleteFenced: true}
	nodes[0].SetAnnotations(map[string]string{virtualSwitchGroupPrefix + sw.GroupName: "ens35"})
	got, err := inspectSwitchNodeObject(sw, member, &nodes[0])
	if err != nil || got.State != "cleaned" {
		t.Fatal("KCN annotation blocked own deletion", got, err)
	}
}
