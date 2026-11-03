package data

import (
	"context"
	"testing"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
)

type unavailableProviderReplica struct {
	name   string
	mutate func(*unstructured.Unstructured)
}

func unavailableProviderReplicas() []unavailableProviderReplica {
	return []unavailableProviderReplica{
		{"pending", func(p *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(p.Object, "Pending", "status", "phase")
		}},
		{"deleting", func(p *unstructured.Unstructured) {
			now := metav1.Now()
			p.SetDeletionTimestamp(&now)
		}},
		{"pod_not_ready", func(p *unstructured.Unstructured) {
			_ = unstructured.SetNestedSlice(p.Object, []any{map[string]any{"type": "Ready", "status": "False"}}, "status", "conditions")
		}},
		{"pod_readiness_unknown", func(p *unstructured.Unstructured) {
			_ = unstructured.SetNestedSlice(p.Object, []any{map[string]any{"type": "Ready", "status": "Unknown"}}, "status", "conditions")
		}},
		{"missing_pod_readiness", func(p *unstructured.Unstructured) {
			unstructured.RemoveNestedField(p.Object, "status", "conditions")
		}},
		{"container_not_ready", func(p *unstructured.Unstructured) {
			statuses, _, _ := unstructured.NestedSlice(p.Object, "status", "containerStatuses")
			statuses[0].(map[string]any)["ready"] = false
			_ = unstructured.SetNestedSlice(p.Object, statuses, "status", "containerStatuses")
		}},
		{"missing_container_status", func(p *unstructured.Unstructured) {
			unstructured.RemoveNestedField(p.Object, "status", "containerStatuses")
		}},
		{"missing_sidecar_status", func(p *unstructured.Unstructured) {
			containers, _, _ := unstructured.NestedSlice(p.Object, "spec", "containers")
			containers = append(containers, map[string]any{"name": "sidecar"})
			_ = unstructured.SetNestedSlice(p.Object, containers, "spec", "containers")
		}},
		{"invalid_container_digest", func(p *unstructured.Unstructured) {
			statuses, _, _ := unstructured.NestedSlice(p.Object, "status", "containerStatuses")
			statuses[0].(map[string]any)["imageID"] = "containerd://sha256:not-a-digest"
			_ = unstructured.SetNestedSlice(p.Object, statuses, "status", "containerStatuses")
		}},
		{"tag_only_image", func(p *unstructured.Unstructured) {
			statuses, _, _ := unstructured.NestedSlice(p.Object, "status", "containerStatuses")
			statuses[0].(map[string]any)["imageID"] = "provider:latest"
			_ = unstructured.SetNestedSlice(p.Object, statuses, "status", "containerStatuses")
		}},
	}
}

// Only the external Kubernetes API is replaced. Defaults selection and all
// identity, route, role and running-image checks execute production code.
func providerDefaultsFixture(providerPods []unstructured.Unstructured) *KCProvider {
	_, set, _ := intranetFixtures()
	objects := []runtime.Object{set.objects["default_vpc"], set.objects["intranet_config"]}
	for i := range set.all[kcServiceCIDRs] {
		objects = append(objects, set.all[kcServiceCIDRs][i].DeepCopy())
	}
	for i := range providerPods {
		objects = append(objects, providerPods[i].DeepCopy())
	}
	kinds := map[schema.GroupVersionResource]string{kcVPCs: "VPCList", kcConfigMaps: "ConfigMapList", kcServiceCIDRs: "ServiceCIDRList", pods: "PodList"}
	return &KCProvider{client: fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), kinds, objects...)}
}

func TestIntranetPoolDefaultsRetainsReadyProviderEvidenceDuringReplicaScheduling(t *testing.T) {
	_, set, _ := intranetFixtures()
	providerPods := set.all[pods]
	pending := providerPods[0].DeepCopy()
	pending.SetName("controller-pending")
	_ = unstructured.SetNestedField(pending.Object, "Pending", "status", "phase")
	providerPods = append(providerPods, *pending)
	got, err := providerDefaultsFixture(providerPods).GetIntranetPoolDefaults(context.Background())
	if err != nil || got.DefaultVPCName != "kcn-cluster" || got.DefaultVPCUID != "uid-kcn-cluster" || len(got.IntranetNetworks) != 1 || got.IntranetNetworks[0] != "10.0.0.0/8" {
		t.Fatal("pending replica erased actual ready provider evidence", got, err)
	}
}

func TestIntranetPoolDefaultsRequiresReadyEvidenceForEveryProviderRole(t *testing.T) {
	for _, role := range []string{"controller", "cni-ds", "ovs-ds", "ovn-central"} {
		t.Run(role, func(t *testing.T) {
			for _, state := range append([]unavailableProviderReplica{{name: "missing_role"}}, unavailableProviderReplicas()...) {
				t.Run(state.name, func(t *testing.T) {
					_, set, _ := intranetFixtures()
					providerPods := []unstructured.Unstructured{}
					for _, p := range set.all[pods] {
						if p.GetLabels()["networking.kubercloud.com/app"] == role {
							if state.mutate == nil {
								continue
							}
							state.mutate(&p)
						}
						providerPods = append(providerPods, p)
					}
					if _, err := providerDefaultsFixture(providerPods).GetIntranetPoolDefaults(context.Background()); biz.ReasonOf(err) != biz.BaseConnectivityNotReady {
						t.Fatal("defaults accepted a role without current ready image evidence", err)
					}
				})
			}
		})
	}
}

func TestIntranetPoolDefaultsKeepsHealthyProviderRoleWhenReplicaUnavailable(t *testing.T) {
	for _, role := range []string{"controller", "cni-ds", "ovs-ds", "ovn-central"} {
		t.Run(role, func(t *testing.T) {
			for _, state := range unavailableProviderReplicas() {
				t.Run(state.name, func(t *testing.T) {
					_, set, _ := intranetFixtures()
					providerPods := set.all[pods]
					for _, p := range providerPods {
						if p.GetLabels()["networking.kubercloud.com/app"] == role {
							replica := p.DeepCopy()
							replica.SetName(role + "-unavailable-replica")
							state.mutate(replica)
							// The unavailable instance comes first so admission cannot
							// depend on Kubernetes LIST ordering.
							providerPods = append([]unstructured.Unstructured{*replica}, providerPods...)
							break
						}
					}
					got, err := providerDefaultsFixture(providerPods).GetIntranetPoolDefaults(context.Background())
					if err != nil || got.DefaultVPCUID != "uid-kcn-cluster" {
						t.Fatal("unavailable replica erased another instance's ready provider evidence", got, err)
					}
				})
			}
		})
	}
}
