package data

import (
	"testing"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestCompletedSNATTombstoneMissingEIPKeepsOrphanFences(t *testing.T) {
	for _, name := range []string{"completed", "unmarked", "direct", "unknown-eip-reference", "orphan-nat", "orphan-service"} {
		t.Run(name, func(t *testing.T) {
			r := egressResolved{
				binding: sqlcgen.NetworkProviderBinding{TenantID: "tenant-a", ResourceKind: "snat", Namespace: "tenant-a", ProviderName: "retired-snat", ProviderUid: "snat-uid"},
				target:  biz.ProviderTarget{Kind: "snat", ResourceID: "retired", KnownIdentity: "snat-uid", Deleting: true, Tombstone: true},
				refs:    map[string]egressReference{"eip": {binding: sqlcgen.NetworkProviderBinding{ResourceKind: "eip", Namespace: "tenant-a", ProviderName: "retired-eip"}, id: "eip-id"}},
			}
			// This is the inspection seam after a complete read: the retained
			// original SNAT and EIP are both absent, including all relationship GVRs.
			set := egressReadSet{objects: map[string]*unstructured.Unstructured{}, all: map[schema.GroupVersionResource][]unstructured.Unstructured{}}
			switch name {
			case "unmarked":
				r.target.Tombstone = false
			case "direct":
				r.target.Direct = true
			case "unknown-eip-reference":
				delete(r.refs, "eip")
			case "orphan-nat":
				orphan := crFixture("Nat", "external-owner", "unlabelled-nat")
				orphan.Object["spec"] = map[string]any{"eip": "tenant-a/retired-eip"}
				set.all[kcNats] = []unstructured.Unstructured{*orphan}
			case "orphan-service":
				orphan := crFixture("Service", "external-owner", "unlabelled-service")
				orphan.SetAPIVersion("v1")
				orphan.SetAnnotations(map[string]string{"networking.kubercloud.com/lb_eips": "tenant-a/retired-eip"})
				orphan.Object["spec"] = map[string]any{"type": "LoadBalancer"}
				set.all[kcServices] = []unstructured.Unstructured{*orphan}
			}
			observation, err := inspectEgressSet(r, set)
			if err != nil || observation.Exists {
				t.Fatalf("completed SNAT/EIP absence was not preserved: %+v %v", observation, err)
			}
			wantDependencies := name != "completed"
			if observation.HasDependencies != wantDependencies {
				t.Fatalf("SNAT tombstone absence/orphan fence differs: dependencies=%v want=%v", observation.HasDependencies, wantDependencies)
			}
		})
	}
}
