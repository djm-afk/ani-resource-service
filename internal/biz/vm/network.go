package vm

import (
	"encoding/json"
	"reflect"
	"strings"
	"time"

	"github.com/google/uuid"
)

type NetworkConsumerState struct {
	State, DesiredRunState, FinalizationID string
	ClosedAt                               *time.Time
	CurrentVMI, CurrentLauncher            *ObjectRef
}

// UnmarshalJSON accepts the previous current-Pod spelling during the in-place
// JSON upgrade. Historic controllers and retired Pods are intentionally ignored.
func (c *NetworkConsumerState) UnmarshalJSON(raw []byte) error {
	type current NetworkConsumerState
	var value struct {
		*current
		CurrentPods []struct{ Reference, Owner ObjectRef }
	}
	value.current = (*current)(c)
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	if c.CurrentLauncher == nil && len(value.CurrentPods) == 1 {
		pod := value.CurrentPods[0]
		c.CurrentLauncher = &pod.Reference
		if pod.Owner.UID != "" {
			c.CurrentVMI = &pod.Owner
		}
	}
	return nil
}

// ReconcileNetworkSubmission maps the VM owner's successful observation to its
// current consumer record. It never reads Network, Kubernetes or KCN.
func ReconcileNetworkSubmission(v *VM, observed Observation) bool {
	if v.Network.AttachmentID == "" {
		return false
	}
	current := &NetworkConsumerState{State: "open", DesiredRunState: strings.ToLower(v.Desired)}
	if observed.VMI != nil {
		ref := *observed.VMI
		current.CurrentVMI = &ref
	}
	if observed.Launcher != nil {
		ref := *observed.Launcher
		current.CurrentLauncher = &ref
	}
	if v.Closing {
		current.State, current.DesiredRunState = "closing", "deleted"
		current.FinalizationID = uuid.NewSHA1(uuid.NameSpaceOID, []byte("ani-vm-close/"+v.ID)).String()
		if observed.Deleted || (v.Network.Consumer != nil && v.Network.Consumer.State == "closed") {
			current.State = "closed"
			current.CurrentVMI, current.CurrentLauncher = nil, nil
			if v.Network.Consumer != nil && v.Network.Consumer.ClosedAt != nil {
				current.ClosedAt = v.Network.Consumer.ClosedAt
			} else if !observed.ObservedAt.IsZero() {
				closed := observed.ObservedAt.UTC()
				current.ClosedAt = &closed
			} else if !v.ObservedAt.IsZero() {
				closed := v.ObservedAt.UTC()
				current.ClosedAt = &closed
			}
		}
	}
	if reflect.DeepEqual(v.Network.Consumer, current) {
		return false
	}
	v.Network.Consumer = current
	v.Network.SnapshotRevision++
	return true
}
