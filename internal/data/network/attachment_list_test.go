package data

import (
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"testing"
	"time"
)

func TestVMCurrentAddressesUsePersistedCurrentOutputWithoutAgeOrReasonGate(t *testing.T) {
	old := time.Now().Add(-24 * time.Hour)
	a := biz.Attachment{State: biz.Attached, Reason: biz.ProviderUnavailable, ObservedAt: &old, CurrentIPs: []string{"10.42.1.3"}, ConsumerSnapshot: &biz.ConsumerSubmission{State: "open", DesiredRunState: "running", CurrentPods: []biz.PodReference{{Reference: biz.WorkloadReference{UID: "launcher"}, Owner: biz.WorkloadReference{UID: "vmi"}}}}}
	got := vmCurrentAddresses(a)
	if len(got) != 1 || got[0].Address != "10.42.1.3" || got[0].LauncherPodUID != "launcher" {
		t.Fatal(got)
	}
	a.ConsumerSnapshot.CurrentPods[0].Owner = biz.WorkloadReference{}
	if got := vmCurrentAddresses(a); len(got) != 1 {
		t.Fatal("optional owner metadata became an address gate", got)
	}
	a.ConsumerSnapshot.DesiredRunState = "stopped"
	if got := vmCurrentAddresses(a); len(got) != 0 {
		t.Fatal("stopped consumer displayed an active address", got)
	}
	a.ConsumerSnapshot.DesiredRunState, a.State = "running", biz.Released
	if got := vmCurrentAddresses(a); len(got) != 0 {
		t.Fatal("released consumer displayed an active address", got)
	}
}
