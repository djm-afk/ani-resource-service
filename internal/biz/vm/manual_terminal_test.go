package vm

import (
	"context"
	"errors"
	"testing"
)

type terminalPowerNative struct {
	NativePort
	x             Observation
	stops, starts int
}

func (n *terminalPowerNative) Observe(context.Context, VM) (Observation, error) { return n.x, nil }
func (n *terminalPowerNative) Stop(context.Context, VM, bool) error             { n.stops++; return nil }
func (n *terminalPowerNative) Start(context.Context, VM) error                  { n.starts++; return nil }

type terminalPowerNetwork struct {
	NetworkPort
	addresses []Address
	err       error
}

func (n terminalPowerNetwork) CurrentAddresses(context.Context, VM) ([]Address, error) {
	return n.addresses, n.err
}

func (n terminalPowerNetwork) Confirm(context.Context, VM, Observation) error { return nil }

func TestStopConvergesWithRetainedManualTerminalVMI(t *testing.T) {
	v := mediaFixtureVM()
	x := Observation{VM: v.Native, VMI: &ObjectRef{Kind: "VirtualMachineInstance", UID: "ended"}, State: "Stopped", Terminal: true}
	n := &terminalPowerNative{x: x}
	w := &Worker{repo: &mediaCheckpointRepo{}, native: n, network: terminalPowerNetwork{}}
	o := Operation{Action: "stop", State: "unknown", Pending: &Mutation{Action: "stop", Submitted: true, Target: v.Native}}
	if err := w.power(context.Background(), &o, &v); err != nil || o.State != "succeeded" || o.Pending != nil || n.stops != 0 {
		t.Fatalf("Stop must finish after confirmed native exit and launcher absence: op=%+v stops=%d err=%v", o, n.stops, err)
	}
}

func TestTerminalStopRequiresOnlyOwnNativeClosure(t *testing.T) {
	for _, mode := range []string{"live-launcher", "native-request", "current-address", "network-unknown"} {
		t.Run(mode, func(t *testing.T) {
			v := mediaFixtureVM()
			x := Observation{VM: v.Native, VMI: &ObjectRef{UID: "ended"}, Terminal: true, State: "Stopped"}
			network := terminalPowerNetwork{}
			switch mode {
			case "live-launcher":
				x.Launcher = &ObjectRef{UID: "still-running"}
			case "native-request":
				x.StateChangeRequests = []string{"pending"}
			case "current-address":
				network.addresses = []Address{{Address: "10.0.0.2"}}
			case "network-unknown":
				network.err = ErrDependency
			}
			n := &terminalPowerNative{x: x}
			w := &Worker{repo: &mediaCheckpointRepo{}, native: n, network: network}
			o := Operation{Action: "stop", State: "unknown", Pending: &Mutation{Action: "stop", Submitted: true, Target: v.Native}}
			err := w.power(context.Background(), &o, &v)
			if mode == "current-address" || mode == "network-unknown" {
				if err != nil || o.State != "succeeded" || n.stops != 0 {
					t.Fatal("network observation blocked native Stop completion", mode, o, err)
				}
			} else if err == nil || o.State == "succeeded" || n.stops != 0 {
				t.Fatal("Stop completed before its native workload closed", mode, o, err)
			}
		})
	}
}

func TestUnknownStartWithRetainedTerminalVMIIsNeverBlindlyRepeated(t *testing.T) {
	v := mediaFixtureVM()
	x := Observation{VM: v.Native, VMI: &ObjectRef{UID: "ended"}, Terminal: true, State: "Stopped"}
	n := &terminalPowerNative{x: x}
	w := &Worker{repo: &mediaCheckpointRepo{}, native: n, network: terminalPowerNetwork{}}
	before := "ended"
	o := Operation{Action: "start", State: "unknown", Pending: &Mutation{Action: "start", Submitted: true, Target: v.Native, BeforeVMIUID: &before}}
	if err := w.power(context.Background(), &o, &v); !errors.Is(err, ErrUnknown) || n.starts != 0 {
		t.Fatal("retained terminal VMI became proof to repeat unknown Start", o, n.starts, err)
	}
}
