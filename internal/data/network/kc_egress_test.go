package data

import (
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"testing"
)

func crFixture(kind, ns, name string) *unstructured.Unstructured {
	o := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "networking.kubercloud.com/v1", "kind": kind, "metadata": map[string]any{"name": name, "namespace": ns, "uid": "uid-" + name, "resourceVersion": "10", "generation": int64(2)}, "spec": map[string]any{}, "status": map[string]any{"conditions": []any{map[string]any{"type": "Valid", "status": "True"}, map[string]any{"type": "Initialized", "status": "True"}, map[string]any{"type": "Ready", "status": "True"}}}}}
	return o
}

func ownedEgressFixture(kind string) (egressResolved, *unstructured.Unstructured) {
	b := sqlcgen.NetworkProviderBinding{ResourceKind: kind, Namespace: "tenant-a", ProviderName: "target", ProviderUid: "uid-target", BindingID: "binding", TenantID: "tenant-a"}
	if kind == "vlan" || kind == "egress_gateway" {
		b.Namespace, b.TenantID = "", ""
	}
	o := crFixture(egressCRKind(kind), b.Namespace, b.ProviderName)
	o.SetLabels(map[string]string{ownerLabel: "ani-network-service", resourceLabel: "resource", bindingLabel: b.BindingID, tenantLabel: b.TenantID})
	r := egressResolved{binding: b, target: biz.ProviderTarget{Kind: kind, ResourceID: "resource", TenantID: b.TenantID, KnownIdentity: b.ProviderUid, Egress: &biz.EgressWorkSpec{DesiredEnabled: true}}}
	return r, o
}

func TestEgressConsumesNativeStatusWithoutSecondaryProofs(t *testing.T) {
	for _, kind := range []string{"eip", "public_pool", "egress_gateway", "vlan"} {
		t.Run(kind, func(t *testing.T) {
			r, object := ownedEgressFixture(kind)
			primary := "Ready"
			if kind == "vlan" {
				primary = "Initialized"
			}
			object.Object["status"] = map[string]any{"conditions": []any{map[string]any{"type": primary, "status": "True"}, map[string]any{"type": "Valid", "status": "False"}}, "observedGeneration": int64(0), "notReadyNodes": "worker-a,worker-b"}
			object.Object["spec"] = map[string]any{"ipAddress": "192.0.2.42"}
			got, err := inspectEgressObject(r, object)
			if err != nil || got.Ready == nil || !*got.Ready {
				t.Fatal(got, err)
			}
			if kind == "eip" && (got.Egress == nil || got.Egress.Address != "192.0.2.42") {
				t.Fatal("lost KCN allocation", got)
			}
			object.Object["status"] = map[string]any{}
			got, err = inspectEgressObject(r, object)
			if err != nil || got.Ready != nil {
				t.Fatal("unreported status became false", got, err)
			}
		})
	}
}

func TestSnatNativePhaseReportsActualAppliedState(t *testing.T) {
	for _, tc := range []struct {
		phase   string
		desired bool
		applied *bool
		ready   bool
	}{
		{"Bound", true, testApplied(true), true}, {"Disabled", false, testApplied(false), true}, {"Disabled", true, testApplied(false), false}, {"Pending", true, nil, false}, {"", true, nil, false},
	} {
		r, o := ownedEgressFixture("snat")
		r.target.Egress.DesiredEnabled = tc.desired
		o.Object["status"] = map[string]any{"phase": tc.phase, "eipAddress": "192.0.2.9"}
		got, err := inspectEgressObject(r, o)
		if err != nil || (got.Ready != nil && *got.Ready) != tc.ready {
			t.Fatal(tc.phase, got, err)
		}
		if (got.Egress.AppliedEnabled == nil) != (tc.applied == nil) || (tc.applied != nil && *got.Egress.AppliedEnabled != *tc.applied) {
			t.Fatal("invented SNAT application", tc.phase, got)
		}
	}
}
func testApplied(value bool) *bool { return &value }

func TestEgressOriginalUIDEndsWithoutAdoptingReplacement(t *testing.T) {
	r, o := ownedEgressFixture("eip")
	o.SetUID("replacement")
	if _, err := inspectEgressObject(r, o); err == nil {
		t.Fatal("replacement adopted")
	}
	r.target.Deleting = true
	got, err := inspectEgressObject(r, o)
	if err != nil || got.Exists || got.Identity != r.target.KnownIdentity {
		t.Fatal(got, err)
	}
	got, err = inspectEgressObject(r, nil)
	if err != nil || got.Exists {
		t.Fatal(got, err)
	}
}
