package biz

import (
	"context"
	"github.com/google/uuid"
	"testing"
	"time"
)

type switchObservationRepository struct {
	work     VirtualSwitchWork
	progress VirtualSwitchProgress
	saves    int
}

func (r *switchObservationRepository) ClaimVirtualSwitch(context.Context, string, time.Duration) (VirtualSwitchWork, bool, error) {
	return r.work, true, nil
}
func (r *switchObservationRepository) SaveVirtualSwitchNode(_ context.Context, _ VirtualSwitchWork, index int, node VirtualSwitchNode) error {
	r.work.Switch.Nodes[index] = node
	r.saves++
	return nil
}
func (r *switchObservationRepository) FinishVirtualSwitch(_ context.Context, _ VirtualSwitchWork, p VirtualSwitchProgress, _ time.Duration) error {
	r.progress = p
	return nil
}

type switchObservationProvider struct {
	readErr error
	patches int
}

func (p *switchObservationProvider) InspectVirtualSwitchNode(_ context.Context, _ VirtualSwitch, node VirtualSwitchNode) (VirtualSwitchNode, error) {
	return node, p.readErr
}
func (p *switchObservationProvider) PatchVirtualSwitchNode(_ context.Context, _ VirtualSwitch, node VirtualSwitchNode, _ bool) (VirtualSwitchNode, error) {
	p.patches++
	node.Applied = true
	return node, nil
}

func TestVirtualSwitchReadFailureNeverAgesIntoNetworkFailure(t *testing.T) {
	for _, elapsed := range []time.Duration{time.Second, time.Hour, 100 * 24 * time.Hour} {
		r := &switchObservationRepository{work: VirtualSwitchWork{Now: time.Unix(1, 0).Add(elapsed), Switch: VirtualSwitch{EgressMetadata: EgressMetadata{State: Available, Reason: Reason("LAST_REPORTED")}, BindingState: "configured", Nodes: []VirtualSwitchNode{{NodeName: "node", NodeUID: "uid", Applied: true}}}}}
		p := &switchObservationProvider{readErr: &ProviderError{Kind: ProviderTemporary}}
		w, err := NewVirtualSwitchWorker(r, p, uuid.NewString(), DefaultWorkerPolicy())
		if err != nil {
			t.Fatal(err)
		}
		if _, err = w.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
		if r.progress.BindingState != "configured" || r.progress.Reason != "LAST_REPORTED" || r.progress.Observed || r.progress.ObservationError != ProviderUnavailable || p.patches != 0 {
			t.Fatal(r.progress)
		}
	}
}
func TestVirtualSwitchOwnedMetadataCompletesBinding(t *testing.T) {
	r := &switchObservationRepository{work: VirtualSwitchWork{Switch: VirtualSwitch{EgressMetadata: EgressMetadata{State: Available}, BindingState: "pending", Nodes: []VirtualSwitchNode{{NodeName: "node", NodeUID: "uid", Applied: true}}}}}
	p := &switchObservationProvider{}
	w, err := NewVirtualSwitchWorker(r, p, uuid.NewString(), DefaultWorkerPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.progress.BindingState != "configured" || !r.progress.Observed || p.patches != 0 || r.saves != 1 {
		t.Fatal(r.progress)
	}
}
