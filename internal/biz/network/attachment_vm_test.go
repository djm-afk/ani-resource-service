package biz

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func vmSnapshotFixture() (Attachment, ConsumerSubmission) {
	a, c := managedSnapshotFixture()
	a.WorkloadMode, c.ProtocolVersion = "vm_managed", 3
	c.CurrentPods[0].Owner = WorkloadReference{APIVersion: "kubevirt.io/v1", Kind: "VirtualMachineInstance", Namespace: a.Namespace, Name: "vm", UID: "vmi-uid"}
	return a, c
}

func TestVMConsumerUsesCurrentRegistration(t *testing.T) {
	a, c := vmSnapshotFixture()
	if !validConsumer(a, c) {
		t.Fatal("current VM launcher rejected")
	}
	c.CurrentPods[0].Owner = WorkloadReference{}
	if !validConsumer(a, c) {
		t.Fatal("VM required a Kubernetes ancestry proof")
	}
	c.ProtocolVersion = 2
	if validConsumer(a, c) {
		t.Fatal("wrong management protocol accepted")
	}
	c.ProtocolVersion, c.TenantID = 3, "other"
	if validConsumer(a, c) {
		t.Fatal("tenant mismatch accepted")
	}
}

func TestVMClosedCurrentConsumerReleasesWithoutProviderReads(t *testing.T) {
	a, c := vmSnapshotFixture()
	a.State = Attached
	a.CurrentIPs = []string{"10.42.1.42"}
	c.State, c.DesiredRunState = "closed", "deleted"
	c.FinalizationID = "33333333-3333-4333-8333-333333333333"
	c.CurrentPods = nil
	reader := &managedAddressReader{}
	got := runCurrentAttachment(t, a, &managedConsumer{value: c}, reader)
	if got.State != Released || got.FinalizationID != c.FinalizationID || got.Addresses == nil || len(got.Addresses.CurrentIPs) != 0 || reader.calls != 0 {
		t.Fatal("closed consumer waited for a provider closure", got)
	}
}

func TestVMExpectationDoesNotChangeLegacyPrepareFingerprint(t *testing.T) {
	r := PrepareAttachment{TenantID: "tenant", InstanceID: "instance", SubnetID: "subnet", VPCID: "vpc", Slot: "primary", RequestKey: "ignored", SubmissionID: "submission", Generation: 1, ClusterID: "cluster", Namespace: "namespace"}
	const intent = `"TenantID":"tenant","InstanceID":"instance","SubnetID":"subnet","VPCID":"vpc","Slot":"primary","RequestKey":"","SubmissionID":"submission","Generation":1,"ClusterID":"cluster","Namespace":"namespace"`
	for _, mode := range []string{"", "controller_managed"} {
		r.WorkloadMode = mode
		fields := intent
		if mode != "" {
			fields = `"WorkloadMode":"controller_managed",` + fields
		}
		sum := sha256.Sum256([]byte(`{"Version":1,"Intent":{` + fields + `}}`))
		if r.Fingerprint() != hex.EncodeToString(sum[:]) {
			t.Fatal("old canonical bytes changed", mode)
		}
	}
}
