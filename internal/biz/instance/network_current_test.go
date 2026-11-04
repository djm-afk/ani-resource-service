package instance

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNetworkRuntimeUpgradeKeepsManagementReleaseAndDropsHistory(t *testing.T) {
	var n NetworkRuntime
	if err := json.Unmarshal([]byte(`{"State":"closed","FinalizationID":"same-close","Cleaned":true,"ReleaseStarted":true,"ReleaseAcknowledged":true,"RootController":{"UID":"root"},"Controllers":[{}],"RetiredPods":[{}],"CurrentPods":[],"ConfirmedSnapshotRevision":4}`), &n); err != nil {
		t.Fatal(err)
	}
	if !n.Cleaned || n.FinalizationID != "same-close" || n.ConfirmedSnapshotRevision != 4 {
		t.Fatal(n)
	}
	raw, err := json.Marshal(n)
	if err != nil || strings.Contains(string(raw), "ReleaseStarted") || strings.Contains(string(raw), "RetiredPods") {
		t.Fatal(string(raw), err)
	}
}
