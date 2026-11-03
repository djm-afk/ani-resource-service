package vm

import (
	"context"
	"encoding/json"
	"testing"
)

type manualNetworkFixture struct {
	NetworkPort
	released []string
	sgCalls  int
	reserved int
}

func (n *manualNetworkFixture) ReserveNetworkInterface(_ context.Context, _ VM, _ Operation, i NetworkInterfaceIntent) (NetworkInterfaceChoice, error) {
	n.reserved++
	if i.Mode == "automatic" {
		return NetworkInterfaceChoice{Mode: "automatic"}, nil
	}
	return NetworkInterfaceChoice{Mode: "manual", VNicID: i.VNicID, ProviderName: "ani-vnic-target", ProviderUID: "original-uid", IPs: []string{"10.0.0.9"}}, nil
}
func (n *manualNetworkFixture) SyncNetworkInterfaces(context.Context, VM, []string) error {
	n.sgCalls++
	return nil
}
func (n *manualNetworkFixture) ReleaseNetworkInterfaces(_ context.Context, _ VM, keep []string) error {
	n.released = append([]string{}, keep...)
	return nil
}

type manualNativeFixture struct {
	NativePort
	applies int
}

func (n *manualNativeFixture) SetNetworkInterface(context.Context, VM, NetworkInterfaceChoice, int64) error {
	n.applies++
	return nil
}
func TestManualNetworkSaveAndCancelRetainRuntime(t *testing.T) {
	net := &manualNetworkFixture{}
	native := &manualNativeFixture{}
	repo := &sgRepository{}
	w := &Worker{repo: repo, native: native, network: net}
	v := VM{Observed: "Running", Addresses: []Address{{Address: "10.0.0.2", LauncherUID: "old-pod"}}}
	target := "vnic_11111111111111111111111111111111"
	raw, _ := json.Marshal(NetworkInterfaceIntent{Slot: "primary", Mode: "manual", VNicID: target})
	o := Operation{ID: "original-op", IntentVersion: 2, Intent: raw}
	if err := w.setNetworkInterface(context.Background(), &o, &v); err != nil {
		t.Fatal(err)
	}
	cfg := PrimaryNetworkInterface(v)
	if !cfg.PendingRestart() || cfg.Applied.Mode != "automatic" || o.State != "succeeded" || native.applies != 0 || len(v.Addresses) != 1 || v.Addresses[0].LauncherUID != "old-pod" || len(net.released) != 1 {
		t.Fatal("save changed runtime or lost target reservation", v, cfg, o, net)
	}
	o.Intent = []byte(`{"slot":"primary","mode":"automatic","vnic_id":""}`)
	o.IntentVersion = 3
	if err := w.setNetworkInterface(context.Background(), &o, &v); err != nil {
		t.Fatal(err)
	}
	if PrimaryNetworkInterface(v).PendingRestart() || len(net.released) != 0 || native.applies != 0 || v.Addresses[0].Address != "10.0.0.2" {
		t.Fatal("cancel did not release only target", v, net)
	}
}
func TestManualNetworkConfirmationRequiresNewEndpointAndAllocatedAddress(t *testing.T) {
	c := NetworkInterfaceChoice{Mode: "manual", VNicID: "vnic_11111111111111111111111111111111", IPs: []string{"10.0.0.9"}}
	v := VM{NetworkInterfaces: []NetworkInterface{{Slot: "primary", Desired: c, Applied: c, RequestedRevision: 2, AppliedRevision: 2}}}
	w := &Worker{}
	x := Observation{Running: true, VMI: &ObjectRef{UID: "new-vmi"}, Launcher: &ObjectRef{UID: "new-pod"}}
	for _, addresses := range [][]Address{nil, {{Address: "10.0.0.9", VMIUID: "new-vmi", LauncherUID: "old-pod"}}, {{Address: "10.0.0.2", VMIUID: "new-vmi", LauncherUID: "new-pod"}}} {
		if w.confirmNetworkInterface(context.Background(), &v, x, addresses) == nil {
			t.Fatal("unconfirmed endpoint became effective", addresses)
		}
	}
	if err := w.confirmNetworkInterface(context.Background(), &v, x, []Address{{Address: "10.0.0.9", VMIUID: "new-vmi", LauncherUID: "new-pod"}}); err != nil {
		t.Fatal(err)
	}
	if PrimaryNetworkInterface(v).VerifiedPodUID != "new-pod" {
		t.Fatal(v)
	}
}
func TestManualNetworkStopTemplateRetiresOnlyAfterApply(t *testing.T) {
	net := &manualNetworkFixture{}
	native := &manualNativeFixture{}
	w := &Worker{repo: &sgRepository{}, native: native, network: net}
	a := NetworkInterfaceChoice{Mode: "manual", VNicID: "vnic_22222222222222222222222222222222", ProviderUID: "old-original"}
	b := NetworkInterfaceChoice{Mode: "manual", VNicID: "vnic_11111111111111111111111111111111"}
	v := VM{NetworkInterfaces: []NetworkInterface{{Slot: "primary", Desired: b, Applied: a, RequestedRevision: 5, AppliedRevision: 2}}}
	o := Operation{ID: "restart-original", IntentVersion: 6}
	if err := w.applyNetworkTemplate(context.Background(), &o, &v); err != nil {
		t.Fatal(err)
	}
	cfg := PrimaryNetworkInterface(v)
	if cfg.PendingRestart() || cfg.AppliedRevision != 5 || cfg.VerifiedPodUID != "" || native.applies != 1 || len(net.released) != 1 || net.released[0] != b.VNicID {
		t.Fatal("template application lost revision or retained old reservation", cfg, net)
	}
}
