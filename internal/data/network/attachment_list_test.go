package data

import (
	"encoding/json"
	"testing"
	"time"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
)

func observedAttachment(state biz.AttachmentState, reason, podUID string) biz.Attachment {
	observed := time.Date(2026, 11, 3, 7, 20, 11, 0, time.UTC)
	return biz.Attachment{State: state, Reason: biz.Reason(reason), PodUID: podUID, ObservedAt: &observed}
}

func containerRelations(podUID, nicUID, ipUID string) []attachmentRelation {
	return []attachmentRelation{
		{Kind: "Pod", Namespace: "tenant-1", Name: "instance-0", UID: podUID, OwnerUID: "notebook-uid", Current: true},
		{Kind: "VNic", Namespace: "tenant-1", Name: "vnic-a", UID: nicUID, OwnerUID: podUID, Current: true},
		{Kind: "VNicIP", Namespace: "tenant-1", Name: "vnicip-a", UID: ipUID, OwnerUID: nicUID, Address: "10.42.1.15", Current: true, AddressEligible: true},
	}
}

func TestContainerAddressRelationsRequireTheFullIdentityChain(t *testing.T) {
	raw := mustJSON(t, containerRelations("pod-current", "nic-current", "ip-current"))
	a := observedAttachment(biz.Attached, "", "pod-current")
	got := containerAddressRelations(a, raw)
	if len(got) != 1 || got[0].NICName != "default" || got[0].Address != "10.42.1.15" {
		t.Fatalf("addresses = %+v", got)
	}
	for name, mutate := range map[string]func(*biz.Attachment, *[]attachmentRelation){
		"pod replaced": func(v *biz.Attachment, r *[]attachmentRelation) { v.PodUID = "pod-new" },
		"pod retired":  func(_ *biz.Attachment, r *[]attachmentRelation) { (*r)[0].Current = false },
		"nic recycled": func(_ *biz.Attachment, r *[]attachmentRelation) { (*r)[1].OwnerUID = "other-pod" },
		"nic deleted":  func(_ *biz.Attachment, r *[]attachmentRelation) { (*r)[1].Current = false },
		"ip not ready": func(_ *biz.Attachment, r *[]attachmentRelation) { (*r)[2].AddressEligible = false },
		"ip deleted":   func(_ *biz.Attachment, r *[]attachmentRelation) { (*r)[2].Current = false },
		"ip not valid": func(_ *biz.Attachment, r *[]attachmentRelation) { (*r)[2].Address = "not-an-address" },
		"reserved":     func(v *biz.Attachment, _ *[]attachmentRelation) { v.State = biz.Reserved },
		"blocked":      func(v *biz.Attachment, _ *[]attachmentRelation) { v.Reason = biz.AttachmentProtocol },
		"never seen":   func(v *biz.Attachment, _ *[]attachmentRelation) { v.ObservedAt = nil },
	} {
		t.Run(name, func(t *testing.T) {
			value, relations := a, containerRelations("pod-current", "nic-current", "ip-current")
			mutate(&value, &relations)
			if out := containerAddressRelations(value, mustJSON(t, relations)); len(out) != 0 {
				t.Fatalf("expected no address, got %+v", out)
			}
		})
	}
}

func TestContainerAddressRelationsIgnoreMalformedEvidence(t *testing.T) {
	if out := containerAddressRelations(observedAttachment(biz.Attached, "", "pod-current"), []byte(`"not-an-array"`)); out != nil {
		t.Fatalf("relations = %+v", out)
	}
	if out := containerAddressRelations(observedAttachment(biz.Attached, "", "pod-current"), nil); out != nil {
		t.Fatalf("relations = %+v", out)
	}
}

func TestVMAddressRelationsKeepLauncherIdentityGate(t *testing.T) {
	observed := time.Date(2026, 11, 3, 7, 20, 11, 0, time.UTC)
	a := biz.Attachment{
		State: biz.Attached, PodUID: "launcher-pod", ObservedAt: &observed,
		ConsumerSnapshot: &biz.ConsumerSubmission{
			State: "open", DesiredRunState: "running",
			CurrentPods: []biz.PodReference{{
				Reference: biz.WorkloadReference{Kind: "Pod", UID: "launcher-pod"},
				Owner:     biz.WorkloadReference{Kind: "VirtualMachineInstance", UID: "vmi-uid"},
			}},
		},
	}
	raw := mustJSON(t, []attachmentRelation{{Kind: "VNicIP", UID: "ip-uid", OwnerUID: "nic-uid", Address: "10.42.2.20", Current: true, AddressEligible: true}})
	got := vmCurrentAddressRelations(a, raw)
	if len(got) != 1 || got[0].Address != "10.42.2.20" || got[0].LauncherPodUID != "launcher-pod" || got[0].VMIUID != "vmi-uid" {
		t.Fatalf("addresses = %+v", got)
	}
	// A Notebook-owned Pod is not a virtual machine, even with the same UID.
	a.ConsumerSnapshot.CurrentPods[0].Owner.Kind = "Notebook"
	if out := vmCurrentAddressRelations(a, raw); out != nil {
		t.Fatalf("non-KubeVirt owner projected an address: %+v", out)
	}
	a.ConsumerSnapshot.CurrentPods[0].Owner.Kind = "VirtualMachineInstance"
	a.ConsumerSnapshot.DesiredRunState = "stopped"
	if out := vmCurrentAddressRelations(a, raw); out != nil {
		t.Fatalf("stopped VM projected an address: %+v", out)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
