package vm

import (
	"context"
	"encoding/json"
	"regexp"
	"time"
)

const VNicAnnotation = "networking.kubercloud.com/vnic"

var vnicIDPattern = regexp.MustCompile(`^vnic_[0-9a-f]{32}$`)

type NetworkInterfaceChoice struct {
	Name         string   `json:"vnic_name,omitempty"`
	Mode         string   `json:"mode"`
	VNicID       string   `json:"vnic_id,omitempty"`
	ProviderName string   `json:"provider_name,omitempty"`
	ProviderUID  string   `json:"provider_uid,omitempty"`
	MACAddress   string   `json:"mac_address,omitempty"`
	IPs          []string `json:"ips,omitempty"`
}
type NetworkInterface struct {
	Slot              string                 `json:"slot"`
	Desired           NetworkInterfaceChoice `json:"desired"`
	Applied           NetworkInterfaceChoice `json:"applied"`
	RequestedRevision int64                  `json:"requested_revision"`
	AppliedRevision   int64                  `json:"applied_revision"`
	VerifiedPodUID    string                 `json:"verified_pod_uid,omitempty"`
}
type NetworkInterfaceIntent struct {
	Slot   string `json:"slot"`
	Mode   string `json:"mode"`
	VNicID string `json:"vnic_id"`
}

func (i NetworkInterfaceIntent) Validate() error {
	if i.Slot != "primary" || (i.Mode != "automatic" && i.Mode != "manual") || (i.Mode == "automatic" && i.VNicID != "") || (i.Mode == "manual" && !vnicIDPattern.MatchString(i.VNicID)) {
		return ErrInvalid
	}
	return nil
}
func PrimaryNetworkInterface(v VM) NetworkInterface {
	for _, x := range v.NetworkInterfaces {
		if x.Slot == "primary" {
			return x
		}
	}
	return NetworkInterface{Slot: "primary", Desired: NetworkInterfaceChoice{Mode: "automatic"}, Applied: NetworkInterfaceChoice{Mode: "automatic"}}
}
func SetPrimaryNetworkInterface(v *VM, c NetworkInterface) {
	old := PrimaryNetworkInterface(*v)
	if v.Network.AttachmentID != "" && (old.Desired.VNicID != c.Desired.VNicID || old.Applied.VNicID != c.Applied.VNicID || old.Desired.ProviderUID != c.Desired.ProviderUID || old.Applied.ProviderUID != c.Applied.ProviderUID) {
		v.Network.SnapshotRevision++
	}
	v.NetworkInterfaces = []NetworkInterface{c}
}
func (c NetworkInterface) PendingRestart() bool {
	return c.Desired.Mode != c.Applied.Mode || c.Desired.VNicID != c.Applied.VNicID || c.RequestedRevision != c.AppliedRevision
}
func (c NetworkInterface) KeepVNicIDs() []string {
	out := []string{}
	for _, x := range []NetworkInterfaceChoice{c.Desired, c.Applied} {
		if x.Mode == "manual" && x.VNicID != "" && (len(out) == 0 || out[0] != x.VNicID) {
			out = append(out, x.VNicID)
		}
	}
	return out
}

type ManualNetworkPort interface {
	ReserveNetworkInterface(context.Context, VM, Operation, NetworkInterfaceIntent) (NetworkInterfaceChoice, error)
	SyncNetworkInterfaces(context.Context, VM, []string) error
	ReleaseNetworkInterfaces(context.Context, VM, []string) error
}
type NetworkInterfaceNativePort interface {
	SetNetworkInterface(context.Context, VM, NetworkInterfaceChoice, int64) error
}

func (w *Worker) setNetworkInterface(ctx context.Context, o *Operation, v *VM) error {
	var intent NetworkInterfaceIntent
	if json.Unmarshal(o.Intent, &intent) != nil || intent.Validate() != nil {
		return ErrInvalid
	}
	port, ok := w.network.(ManualNetworkPort)
	if !ok {
		return ErrDependency
	}
	if err := w.checkpoint(ctx, o, *v, "reserve-network-interface", v.Native); err != nil {
		return err
	}
	choice, err := port.ReserveNetworkInterface(ctx, *v, *o, intent)
	if err != nil {
		return err
	}
	cfg := PrimaryNetworkInterface(*v)
	cfg.Desired = choice
	cfg.RequestedRevision = o.IntentVersion
	if cfg.Desired.Mode == cfg.Applied.Mode && cfg.Desired.VNicID == cfg.Applied.VNicID {
		cfg.AppliedRevision = cfg.RequestedRevision
	}
	SetPrimaryNetworkInterface(v, cfg)
	if err = port.SyncNetworkInterfaces(ctx, *v, v.SecurityGroupIDs); err != nil {
		return err
	}
	// Only references absent from both current and target configurations retire.
	if err = port.ReleaseNetworkInterfaces(ctx, *v, cfg.KeepVNicIDs()); err != nil {
		return err
	}
	o.Pending = nil
	o.State = "succeeded"
	return nil
}

// applyNetworkTemplate is called only after the owner has proved the old
// runtime absent. A saved selection never calls this method.
func (w *Worker) applyNetworkTemplate(ctx context.Context, o *Operation, v *VM) error {
	cfg := PrimaryNetworkInterface(*v)
	if !cfg.PendingRestart() {
		return nil
	}
	port, ok := w.network.(ManualNetworkPort)
	if !ok {
		return ErrDependency
	}
	reservationOp := *o
	reservationOp.IntentVersion = cfg.RequestedRevision
	choice, err := port.ReserveNetworkInterface(ctx, *v, reservationOp, NetworkInterfaceIntent{Slot: "primary", Mode: cfg.Desired.Mode, VNicID: cfg.Desired.VNicID})
	if err != nil {
		return err
	}
	cfg.Desired = choice
	SetPrimaryNetworkInterface(v, cfg)
	if err = port.SyncNetworkInterfaces(ctx, *v, v.SecurityGroupIDs); err != nil {
		return err
	}
	native, ok := w.native.(NetworkInterfaceNativePort)
	if !ok {
		return ErrDependency
	}
	if err = w.checkpoint(ctx, o, *v, "apply-network-template", v.Native); err != nil {
		return err
	}
	if err = native.SetNetworkInterface(ctx, *v, choice, cfg.RequestedRevision); err != nil {
		return err
	}
	cfg.Applied = choice
	cfg.AppliedRevision = cfg.RequestedRevision
	cfg.VerifiedPodUID = ""
	SetPrimaryNetworkInterface(v, cfg)
	if err = port.ReleaseNetworkInterfaces(ctx, *v, cfg.KeepVNicIDs()); err != nil {
		return err
	}
	o.Pending = nil
	return nil
}
func (w *Worker) confirmNetworkInterface(ctx context.Context, v *VM, x Observation) error {
	cfg := PrimaryNetworkInterface(*v)
	if cfg.RequestedRevision == 0 {
		return nil
	}
	if cfg.PendingRestart() || x.Launcher == nil || x.VMI == nil || !x.Running {
		return ErrDependency
	}
	cfg.VerifiedPodUID = x.Launcher.UID
	SetPrimaryNetworkInterface(v, cfg)
	return nil
}

// NetworkInterfaceRuntime is an observed endpoint, separate from either choice.
type NetworkInterfaceRuntime struct {
	Mode, ProviderName, ProviderUID, MACAddress, PodUID, Reason string
	IPs                                                         []string
	ObservedAt                                                  time.Time
	Confirmed                                                   bool
}
type NetworkInterfaceReadPort interface {
	CurrentNetworkInterface(context.Context, VM) (NetworkInterfaceRuntime, error)
	ResolveNetworkInterfaceChoice(context.Context, VM, NetworkInterfaceChoice) (NetworkInterfaceChoice, error)
}
