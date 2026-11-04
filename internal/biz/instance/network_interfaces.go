package instance

import (
	"context"
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
func PrimaryNetworkInterface(v Instance) NetworkInterface {
	for _, x := range v.NetworkInterfaces {
		if x.Slot == "primary" {
			return x
		}
	}
	return NetworkInterface{Slot: "primary", Desired: NetworkInterfaceChoice{Mode: "automatic"}, Applied: NetworkInterfaceChoice{Mode: "automatic"}}
}
func SetPrimaryNetworkInterface(v *Instance, c NetworkInterface) {
	old := PrimaryNetworkInterface(*v)
	if v.Network != nil && (old.Desired.VNicID != c.Desired.VNicID || old.Applied.VNicID != c.Applied.VNicID || old.Desired.ProviderUID != c.Desired.ProviderUID || old.Applied.ProviderUID != c.Applied.ProviderUID) {
		v.Network = CloneNetwork(v.Network)
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
	ReserveNetworkInterface(context.Context, Instance, Operation, NetworkInterfaceIntent) (NetworkInterfaceChoice, error)
	SyncNetworkInterfaces(context.Context, Instance, []string) error
	ReleaseNetworkInterfaces(context.Context, Instance, []string) error
}
type NetworkInterfaceNativePort interface {
	SetNetworkInterface(context.Context, Instance, NetworkInterfaceChoice, int64) error
}

func (w *Worker) stepNetworkInterface(ctx context.Context, work Work) (bool, error) {
	p := Progress{Instance: work.Instance, Operation: cloneOperation(work.Operation), Next: 5 * time.Second}
	o := p.Operation
	if o == nil || o.Execution == nil || o.Execution.NetworkInterfaceTarget == nil {
		return true, ErrInvalid
	}
	intent := *o.Execution.NetworkInterfaceTarget
	if intent.Validate() != nil || p.Instance.Spec.Network == nil {
		return true, ErrInvalid
	}
	port, ok := w.network.(ManualNetworkPort)
	if !ok {
		block(&p, ErrNetworkUnavailable.Error())
		return true, w.repo.Finish(ctx, work, p)
	}
	choice, err := port.ReserveNetworkInterface(ctx, p.Instance, *o, intent)
	if err == nil {
		cfg := PrimaryNetworkInterface(p.Instance)
		cfg.Desired = choice
		cfg.RequestedRevision = o.Version
		if cfg.Desired.Mode == cfg.Applied.Mode && cfg.Desired.VNicID == cfg.Applied.VNicID {
			cfg.AppliedRevision = cfg.RequestedRevision
		}
		SetPrimaryNetworkInterface(&p.Instance, cfg)
		err = port.SyncNetworkInterfaces(ctx, p.Instance, p.Instance.SecurityGroupIDs)
		if err == nil {
			err = port.ReleaseNetworkInterfaces(ctx, p.Instance, cfg.KeepVNicIDs())
		}
	}
	if err != nil {
		block(&p, err.Error())
	} else {
		o.State = "completed"
		o.Reason = ""
		o.Execution.Phase = "done"
		p.Instance.Reason = ""
	}
	return true, w.repo.Finish(ctx, work, p)
}

// applyNetworkTemplate is restricted to a freshly proven stopped controller.
func (w *Worker) applyNetworkTemplate(ctx context.Context, p *Progress, obs Observation) error {
	cfg := PrimaryNetworkInterface(p.Instance)
	if !cfg.PendingRestart() {
		return nil
	}
	if !obs.SafeStopped || obs.PodExists {
		return ErrNetworkUnavailable
	}
	port, ok := w.network.(ManualNetworkPort)
	if !ok {
		return ErrNetworkUnavailable
	}
	native, ok := w.provider.(NetworkInterfaceNativePort)
	if !ok {
		return ErrNetworkUnavailable
	}
	if p.Operation == nil {
		return ErrOperationInProgress
	}
	reservationOp := *p.Operation
	reservationOp.Version = cfg.RequestedRevision
	choice, err := port.ReserveNetworkInterface(ctx, p.Instance, reservationOp, NetworkInterfaceIntent{Slot: "primary", Mode: cfg.Desired.Mode, VNicID: cfg.Desired.VNicID})
	if err != nil {
		return err
	}
	cfg.Desired = choice
	SetPrimaryNetworkInterface(&p.Instance, cfg)
	if err = port.SyncNetworkInterfaces(ctx, p.Instance, p.Instance.SecurityGroupIDs); err != nil {
		return err
	}
	if err = native.SetNetworkInterface(ctx, p.Instance, choice, cfg.RequestedRevision); err != nil {
		return err
	}
	cfg.Applied = choice
	cfg.AppliedRevision = cfg.RequestedRevision
	cfg.VerifiedPodUID = ""
	SetPrimaryNetworkInterface(&p.Instance, cfg)
	return port.ReleaseNetworkInterfaces(ctx, p.Instance, cfg.KeepVNicIDs())
}
func confirmNetworkInterface(i *Instance, obs Observation) error {
	cfg := PrimaryNetworkInterface(*i)
	if cfg.RequestedRevision == 0 {
		return nil
	}
	if cfg.PendingRestart() || obs.PodUID == "" || !obs.PodReady || i.Network == nil || i.Network.NetworkState != "attached" || i.Network.ConfirmedSnapshotRevision != i.Network.SnapshotRevision {
		return ErrNetworkUnavailable
	}
	cfg.VerifiedPodUID = obs.PodUID
	SetPrimaryNetworkInterface(i, cfg)
	return nil
}
