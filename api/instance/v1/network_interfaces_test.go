package instancev1

import (
	"bytes"
	"testing"
)

func TestNetworkInterfaceCanonicalIntentPreservesVersionAndSelection(t *testing.T) {
	id := "11111111-1111-4111-8111-111111111111"
	vnic := "vnic_11111111111111111111111111111111"
	raw, err := CanonicalNetworkInterfaceIntent(id, "primary", "manual", vnic, 9007199254740993)
	if err != nil {
		t.Fatal(err)
	}
	out, err := ParseCommandIntent("set-network-interface", id, raw)
	if err != nil || out.ExpectedVersion != 9007199254740993 || out.VNicID != vnic {
		t.Fatal(out, err)
	}
	if _, err = CanonicalNetworkInterfaceIntent(id, "secondary", "manual", vnic, 1); err == nil {
		t.Fatal("unsupported slot accepted")
	}
	if _, err = CanonicalNetworkInterfaceIntent(id, "primary", "automatic", vnic, 1); err == nil {
		t.Fatal("automatic accepted an arbitrary reference")
	}
	different, _ := CanonicalNetworkInterfaceIntent(id, "primary", "automatic", "", 9007199254740993)
	if bytes.Equal(different, raw) {
		t.Fatal("selection absent from replay identity")
	}
}
