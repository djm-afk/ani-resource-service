package instance

import (
	"context"
	"encoding/json"
	"reflect"
	"time"
)

// Network owns Attachment transactions; Instance only publishes its durable
// workload submission and consumes these narrow lifecycle operations.
type WorkloadReference struct{ APIVersion, Kind, Namespace, Name, UID string }
type PodReference struct{ Reference, Owner WorkloadReference }
type NetworkRuntime struct {
	CurrentVNicUID, CurrentMode                      string
	ObservedAt                                       time.Time
	CurrentVNicName, CurrentMACAddress               string
	CurrentIPs                                       []string
	SubmissionID, RequestKey                         string
	Generation, SnapshotRevision                     int64
	NotifiedSnapshotRevision                         int64
	State, DesiredRunState, FinalizationID           string
	ClosedAt                                         *time.Time
	CurrentPods                                      []PodReference
	AttachmentID, BindingRevision, PrimaryNetworkRef string
	BasicIsolationName                               string
	AttachmentVersion, ConfirmedSnapshotRevision     int64
	NetworkState, Reason                             string
	PrepareStarted                                   bool
	Cleaned                                          bool
}

// Old snapshots can retain fields that belonged to Network's former physical
// closure protocol. They are ignored when loading the current management state.
func (n *NetworkRuntime) UnmarshalJSON(raw []byte) error {
	type current NetworkRuntime
	return json.Unmarshal(raw, (*current)(n))
}

type NetworkBinding struct {
	AttachmentID, BindingRevision, PrimaryNetworkRef string
	BasicIsolationName                               string
	Version                                          int64
}
type NetworkObservation struct {
	Confirmed                                                       bool
	CurrentVNicName, CurrentVNicUID, CurrentMACAddress, CurrentMode string
	ObservedAt                                                      time.Time
	CurrentIPs                                                      []string
	State, Reason, PodUID                                           string
	Version, SnapshotRevision                                       int64
}
type NetworkPort interface {
	ValidateSelection(context.Context, string, string, string, NetworkSelection) error
	Prepare(context.Context, Instance) (NetworkBinding, error)
	Observe(context.Context, Instance) (NetworkObservation, error)
	Confirm(context.Context, Instance) error
	Release(context.Context, Instance) (bool, error)
}
type InstanceReader interface {
	Get(context.Context, string, string) (Instance, error)
}

func CloneNetwork(n *NetworkRuntime) *NetworkRuntime {
	if n == nil {
		return nil
	}
	out := *n
	out.CurrentPods = append([]PodReference(nil), n.CurrentPods...)
	out.CurrentIPs = append([]string(nil), n.CurrentIPs...)
	return &out
}

// ReconcileSubmission publishes only the current workload management facts
// after a successful observation by the Instance owner.
func ReconcileSubmission(i *Instance, o Observation) bool {
	if i.Network == nil {
		return false
	}
	old := CloneNetwork(i.Network)
	n := i.Network
	n.DesiredRunState = i.RuntimeDesired
	n.CurrentPods = nil
	if o.PodExists && o.PodUID != "" {
		pod := PodReference{Reference: WorkloadReference{APIVersion: "v1", Kind: "Pod", Namespace: i.Spec.Namespace, Name: o.PodName, UID: o.PodUID}}
		if o.StatefulSetUID != "" {
			pod.Owner = WorkloadReference{APIVersion: "apps/v1", Kind: "StatefulSet", Namespace: i.Spec.Namespace, Name: i.Spec.NotebookName, UID: o.StatefulSetUID}
		}
		n.CurrentPods = []PodReference{pod}
	}
	if n.State == "closing" && !o.NotebookExists && !o.PodExists && o.StatefulSetUID == "" && i.Pending == nil {
		closed := time.Now().UTC()
		n.State, n.ClosedAt = "closed", &closed
	}
	changed := !reflect.DeepEqual(old, n)
	if changed {
		n.SnapshotRevision++
	}
	return changed
}
