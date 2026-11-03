package biz

import (
	"context"
	"testing"
	"time"
)

type manualVNicBoundary struct {
	ResourceProvider
	ensured int
}

func (p *manualVNicBoundary) Observe(context.Context, ProviderTarget) (ProviderObservation, error) {
	return ProviderObservation{}, nil
}
func (p *manualVNicBoundary) EnsureVNic(context.Context, ProviderTarget) (ProviderObservation, error) {
	p.ensured++
	return ProviderObservation{Exists: true, Identity: "manual-original-uid", Ready: true, VNic: &VNicFacts{MACAddress: "02:00:00:00:00:01", PrimaryIP: "10.42.1.42", IPs: []string{"10.42.1.42"}}}, nil
}
func TestManualVNicWorkerUsesSpecificReadinessAndPreservesAllocation(t *testing.T) {
	r := &tombstoneBudgetRepository{work: Work{Resource: ResourceWork{Kind: "vnic", State: Provisioning}, Operation: Operation{Kind: "create_vnic"}, ActiveOperation: true, Now: time.Now()}}
	p := &manualVNicBoundary{}
	w, e := NewWorker(r, p, "44444444-4444-4444-8444-444444444444", DefaultWorkerPolicy())
	if e != nil {
		t.Fatal(e)
	}
	if _, e = w.Step(context.Background()); e != nil {
		t.Fatal(e)
	}
	if p.ensured != 1 || r.progress.State != Available || r.progress.OperationState != Succeeded || r.progress.VNic == nil || r.progress.VNic.PrimaryIP != "10.42.1.42" {
		t.Fatal("VNIC-specific allocation was lost", r.progress, p.ensured)
	}
}
