package data

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func switchFixture(t *testing.T) (biz.VirtualSwitch, []unstructured.Unstructured, []unstructured.Unstructured, []unstructured.Unstructured, *unstructured.Unstructured, time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 21, 1, 0, 0, 0, time.UTC)
	node := unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": "n1", "uid": "uid1", "resourceVersion": "1", "labels": map[string]any{allocatedNodeLabel: "true"}, "annotations": map[string]any{"networking.kubercloud.com/chassis": "chassis1"}}, "status": map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}}}
	daemon := unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": "cni1", "namespace": kcSystemNamespace, "labels": map[string]any{"networking.kubercloud.com/app": "cni-ds"}}, "spec": map[string]any{"nodeName": "n1", "containers": []any{map[string]any{"name": "cni"}}}, "status": map[string]any{"phase": "Running", "conditions": []any{map[string]any{"type": "Ready", "status": "True"}}, "containerStatuses": []any{map[string]any{"name": "cni", "ready": true}}}}}
	config := &unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": "kcn-config", "namespace": kcSystemNamespace, "uid": "config1", "resourceVersion": "3"}}}
	return biz.VirtualSwitch{GroupName: "test-group"}, []unstructured.Unstructured{node}, nil, []unstructured.Unstructured{daemon}, config, now
}

func TestVirtualSwitchAdmissionFreezesLogicalIntentWithoutNodeProbe(t *testing.T) {
	for _, failure := range []string{"", "node_unready", "missing_daemon", "external_same_value", "extra_allocated", "missing_uid", "legacy_owner", "foreign_vlan"} {
		t.Run(failure, func(t *testing.T) {
			sw, nodes, maps, daemons, config, now := switchFixture(t)
			var vlans []unstructured.Unstructured
			switch failure {
			case "node_unready":
				_ = unstructured.SetNestedSlice(nodes[0].Object, []any{map[string]any{"type": "Ready", "status": "False"}}, "status", "conditions")
			case "missing_daemon":
				daemons = nil
			case "external_same_value":
				labels := nodes[0].GetLabels()
				labels[virtualSwitchGroupPrefix+sw.GroupName] = "100"
				nodes[0].SetLabels(labels)
			case "extra_allocated":
				extra := nodes[0].DeepCopy()
				extra.SetName("n2")
				extra.SetUID("uid2")
				nodes = append(nodes, *extra)
			case "missing_uid":
				nodes[0].SetUID("")
			case "legacy_owner":
				config.SetAnnotations(map[string]string{adoptionMarker("prepared0"): "other-binding"})
			case "foreign_vlan":
				vlans = []unstructured.Unstructured{{Object: map[string]any{"spec": map[string]any{"devName": "prepared0", "vClusterName": "another-group"}}}}
			}
			v, err := virtualSwitchInventory(sw, "prepared0", nodes, maps, vlans, daemons, config, now, now)
			if failure != "" {
				if err == nil {
					t.Fatal("incomplete or foreign logical binding admitted")
				}
				return
			}
			if err != nil || len(v.Members) != 1 || len(v.Inventory.Items) != 1 || len(v.Fingerprint) != 64 || !v.Members[0].Allocated || v.Members[0].Priority != 100 {
				t.Fatal(v, err)
			}
			device := v.Inventory.Items[0]
			if device.Name != "prepared0" || device.NodeUID != "uid1" {
				t.Fatal("administrator intent fabricated host probe evidence", v)
			}
		})
	}
}

func TestVirtualSwitchAdmissionAcceptsKCNManagedDeviceWithoutLegacyOwner(t *testing.T) {
	sw, nodes, _, daemons, config, now := switchFixture(t)
	_ = unstructured.SetNestedField(config.Object, "ens35", "data", "managedDevices")
	if _, err := virtualSwitchInventory(sw, "ens35", nodes, nil, nil, daemons, config, now, now); err != nil {
		t.Fatal("KCN's prepared device configuration blocked administrator intent", err)
	}
	config.SetAnnotations(map[string]string{adoptionMarker("ens35"): "legacy-binding"})
	if _, err := virtualSwitchInventory(sw, "ens35", nodes, nil, nil, daemons, config, now, now); err == nil {
		t.Fatal("legacy adoption ownership was ignored")
	}
}

func TestVirtualSwitchWiringUsesExactKCNDeviceAcknowledgement(t *testing.T) {
	sw, nodes, _, _, _, _ := switchFixture(t)
	v := biz.PreparedDeviceNode{NodeName: "n1", NodeUID: "uid1", Name: "ens35"}
	for _, tc := range []struct {
		feedback string
		ready    bool
	}{{"", false}, {"ens350", false}, {"ens36", false}, {"ens36, ens35", true}, {"ens35", true}} {
		nodes[0].SetAnnotations(map[string]string{virtualSwitchGroupPrefix + sw.GroupName: tc.feedback})
		if got := switchWiring(&nodes[0], v, sw.GroupName); got != tc.ready {
			t.Fatalf("feedback=%q ready=%t want=%t", tc.feedback, got, tc.ready)
		}
	}
	nodes[0].SetAnnotations(map[string]string{virtualSwitchGroupPrefix + "foreign": "ens35"})
	if switchWiring(&nodes[0], v, sw.GroupName) {
		t.Fatal("another group's acknowledgement proved this binding")
	}
}

func TestInspectSwitchUnknownRequiresBindingOwner(t *testing.T) {
	sw, nodes, _, _, _, _ := switchFixture(t)
	sw.BindingID = "binding1"
	member := biz.VirtualSwitchNode{NodeName: "n1", NodeUID: "uid1", Priority: 100, Allocated: true, Prepared: true, Unknown: true}
	labels := nodes[0].GetLabels()
	labels[virtualSwitchGroupPrefix+sw.GroupName] = "100"
	nodes[0].SetLabels(labels)
	if _, err := inspectSwitchNodeObject(sw, member, &nodes[0]); err == nil {
		t.Fatal("same label value adopted without owner")
	}
	annotations := nodes[0].GetAnnotations()
	annotations[VirtualSwitchOwnerKey(sw.GroupName)] = sw.BindingID
	nodes[0].SetAnnotations(annotations)
	unprepared := member
	unprepared.Prepared = false
	if _, err := inspectSwitchNodeObject(sw, unprepared, &nodes[0]); err == nil {
		t.Fatal("unrecorded dispatch adopted")
	}
	got, err := inspectSwitchNodeObject(sw, member, &nodes[0])
	if err != nil || !got.Applied || got.Unknown || got.ResourceVersion != "1" {
		t.Fatal(got, err)
	}
	annotations[VirtualSwitchOwnerKey(sw.GroupName)] = "foreign"
	nodes[0].SetAnnotations(annotations)
	if _, err := inspectSwitchNodeObject(sw, member, &nodes[0]); err == nil {
		t.Fatal("foreign binding owner admitted")
	}
	if len(strings.SplitN(VirtualSwitchOwnerKey(strings.Repeat("a", 63)), "/", 2)[1]) > 63 {
		t.Fatal("illegal annotation key")
	}
}

func TestV09VlanCreationPrecedesWiringButReadinessDoesNot(t *testing.T) {
	r := egressResolved{target: biz.ProviderTarget{Kind: "vlan", ResourceID: "vlan-id"}, binding: sqlcgen.NetworkProviderBinding{ResourceKind: "vlan", ProviderName: "vlan-provider", BindingID: "binding"}, spec: map[string]any{"devName": "ens35", "vClusterName": "test-group", "vlanID": int64(0)}, virtualSwitch: &biz.VirtualSwitch{GroupName: "test-group"}}
	set := egressReadSet{deviceRequired: true, deviceReady: true, switchConfigured: true, objects: map[string]*unstructured.Unstructured{}}
	if !egressPrerequisites(r, set) {
		t.Fatal("creation waited for the physical wiring it must initiate")
	}
	o := egressObject(r)
	o.SetUID("vlan-uid")
	o.SetResourceVersion("1")
	o.SetGeneration(1)
	_ = unstructured.SetNestedSlice(o.Object, []any{map[string]any{"type": "Valid", "status": "True"}, map[string]any{"type": "Initialized", "status": "True"}}, "status", "conditions")
	set.objects["self"] = o
	value, err := inspectEgressSet(r, set)
	if err != nil || value.Ready {
		t.Fatal("unwired VLAN reported ready", value, err)
	}
	set.switchWired = true
	value, err = inspectEgressSet(r, set)
	if err != nil || !value.Ready {
		t.Fatal("v0.9 VLAN incorrectly required unpublished Ready condition", value, err)
	}
	set.switchConfigured = false
	if egressPrerequisites(r, set) {
		t.Fatal("missing owned group admitted")
	}
}

// API collection time proves logical state; no physical-source freshness is
// renewed or required after removing the host collector.
func TestVirtualSwitchObservationProofUsesCompleteKCNCollection(t *testing.T) {
	sw, nodes, maps, daemons, config, now := switchFixture(t)
	collected := now.Add(2 * time.Minute)
	current, err := virtualSwitchInventoryMode(sw, "ens35", nodes, maps, nil, daemons, config, collected, collected, false)
	if err != nil {
		t.Fatal(err)
	}
	proof := virtualSwitchObservationProof(sw, current.Inventory, collected)
	proof.CoveredGeneration = 2
	requirement := biz.ObservationRequirement{RequestedGeneration: 2, AppliedAt: now.Add(10 * time.Second), Hash: "prior-applied-fact"}
	if !proof.Covers(requirement) || !proof.CollectedAt.Equal(collected) {
		t.Fatalf("complete KCN collection did not cover committed evidence: proof=%+v requirement=%+v", proof, requirement)
	}
	requirement.PreviousCollectedAt = collected.Add(time.Second)
	if proof.Covers(requirement) {
		t.Fatal("logical evidence regressed behind committed observation")
	}
	requirement.PreviousCollectedAt = time.Time{}
	proof.CollectionStartedAt = now.Add(5 * time.Second)
	if proof.Covers(requirement) {
		t.Fatal("older API collection bypassed commit fence")
	}
}

func TestVirtualSwitchDeviceStateWaitsForEveryNodeAndIgnoresStoredMAC(t *testing.T) {
	sw, nodes, maps, daemons, config, now := switchFixture(t)
	extra := nodes[0].DeepCopy()
	extra.SetName("n2")
	extra.SetUID("uid2")
	nodes = append(nodes, *extra)
	cni := daemons[0].DeepCopy()
	cni.SetName("cni2")
	_ = unstructured.SetNestedField(cni.Object, "n2", "spec", "nodeName")
	daemons = append(daemons, *cni)
	accepted, err := virtualSwitchInventory(sw, "ens35", nodes, maps, nil, daemons, config, now, now)
	if err != nil {
		t.Fatal(err)
	}
	sw.Nodes, sw.BindingID, sw.BindingState = accepted.Members, "binding1", "configured"
	for i := range nodes {
		labels := nodes[i].GetLabels()
		labels[virtualSwitchGroupPrefix+sw.GroupName] = "100"
		nodes[i].SetLabels(labels)
		nodes[i].SetAnnotations(map[string]string{"networking.kubercloud.com/chassis": "chassis", VirtualSwitchOwnerKey(sw.GroupName): sw.BindingID, virtualSwitchGroupPrefix + sw.GroupName: "ens35"})
	}
	body, _ := json.Marshal(accepted.Inventory.Items)
	var historical []map[string]any
	if err := json.Unmarshal(body, &historical); err != nil {
		t.Fatal(err)
	}
	for _, node := range historical {
		node["MAC"], node["Management"], node["OVSManaged"] = "historical-mac", true, true
	}
	body, _ = json.Marshal(historical)
	device := sqlcgen.NetworkDeviceAdoption{DeviceName: "ens35", NodeInventory: body}
	// Existing switch-owned devices can remain in KCN's prepared device list.
	_ = unstructured.SetNestedField(config.Object, "ens35", "data", "managedDevices")
	for _, feedback := range []string{"", "ens350", "ens35"} {
		annotations := nodes[1].GetAnnotations()
		annotations[virtualSwitchGroupPrefix+sw.GroupName] = feedback
		nodes[1].SetAnnotations(annotations)
		current, err := virtualSwitchInventory(sw, "ens35", nodes, nil, nil, daemons, config, now, now)
		if err != nil {
			t.Fatal(err)
		}
		configured, ready, err := inspectVirtualSwitchDeviceState(sw, &current, device, nodes)
		if err != nil || !configured || ready != (feedback == "ens35") {
			t.Fatal(feedback, configured, ready, err)
		}
	}
	changed := append([]biz.PreparedDeviceNode(nil), accepted.Inventory.Items...)
	changed[1].NodeUID = "replacement-uid"
	body, _ = json.Marshal(changed)
	device.NodeInventory = body
	if _, _, err := inspectVirtualSwitchDeviceState(sw, &accepted, device, nodes); err == nil {
		t.Fatal("replacement node adopted under frozen device registration")
	}
}

func TestVirtualSwitchDependentReadRejectsInvalidParentViewForBoundedRetry(t *testing.T) {
	p := &KCProvider{observation: &KCObservation{epoch: 2}}
	_, _, proof, err := p.virtualSwitchDeviceState(context.Background(), biz.VirtualSwitch{}, sqlcgen.NetworkDeviceAdoption{}, &auditView{epoch: 1}, false, nil)
	if err != errEgressAuditInvalid || !proof.CollectedAt.IsZero() {
		t.Fatal("dependent read escaped parent audit fence", proof, err)
	}
}
