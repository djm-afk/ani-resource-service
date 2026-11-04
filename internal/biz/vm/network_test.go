package vm

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestCurrentNetworkSnapshotTracksOnlyOwnerCurrentFacts(t *testing.T) {
	v := VM{ID: "vm-id", Desired: "Running", Network: NetworkPlan{AttachmentID: "attachment", SnapshotRevision: 1, Consumer: &NetworkConsumerState{State: "open", DesiredRunState: "running"}}}
	x := Observation{VMI: &ObjectRef{Kind: "VirtualMachineInstance", Namespace: "tenant", Name: "vm", UID: "vmi-one"}, Launcher: &ObjectRef{Kind: "Pod", Namespace: "tenant", Name: "launcher", UID: "pod-one"}, Running: true}
	if !ReconcileNetworkSubmission(&v, x) || v.Network.SnapshotRevision != 2 || v.Network.Consumer.CurrentLauncher.UID != "pod-one" {
		t.Fatal(v.Network)
	}
	if ReconcileNetworkSubmission(&v, x) || v.Network.SnapshotRevision != 2 {
		t.Fatal("unchanged current state advanced revision", v.Network)
	}
	x.Launcher.UID = "pod-two"
	if !ReconcileNetworkSubmission(&v, x) || v.Network.SnapshotRevision != 3 || v.Network.Consumer.CurrentLauncher.UID != "pod-two" {
		t.Fatal(v.Network)
	}
	v.Closing = true
	if !ReconcileNetworkSubmission(&v, Observation{}) || v.Network.Consumer.State != "closing" {
		t.Fatal("absence inferred closure", v.Network)
	}
	x = Observation{Deleted: true, ObservedAt: time.Now().UTC()}
	if !ReconcileNetworkSubmission(&v, x) || v.Network.Consumer.State != "closed" || v.Network.Consumer.CurrentLauncher != nil || v.Network.Consumer.CurrentVMI != nil || v.Network.Consumer.ClosedAt == nil {
		t.Fatal(v.Network)
	}
}

func TestCurrentNetworkSnapshotReadsLegacyCurrentPodAndIgnoresHistory(t *testing.T) {
	raw := []byte(`{"State":"open","DesiredRunState":"running","RootController":{"UID":"old-vm"},"Controllers":[{"Reference":{"UID":"old-vmi"}}],"RetiredPods":[{"Reference":{"UID":"old-pod"}}],"CurrentPods":[{"Reference":{"APIVersion":"v1","Kind":"Pod","Namespace":"tenant","Name":"launcher","UID":"current-pod"},"Owner":{"Kind":"VirtualMachineInstance","Namespace":"tenant","Name":"vm","UID":"current-vmi"}}]}`)
	var c NetworkConsumerState
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	if c.CurrentLauncher == nil || c.CurrentLauncher.UID != "current-pod" || c.CurrentVMI == nil || c.CurrentVMI.UID != "current-vmi" {
		t.Fatal(c)
	}
	encoded, err := json.Marshal(c)
	if err != nil || strings.Contains(string(encoded), "RetiredPods") || strings.Contains(string(encoded), "Controllers") || strings.Contains(string(encoded), "old-pod") {
		t.Fatal(string(encoded), err)
	}
}
