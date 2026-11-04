package biz

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

type vmPreparationProvider struct {
	ResourceProvider
	repository *earlyCompletionRepository
	calls      int
	ready      *bool
}

type vmPreparationRepository struct {
	*earlyCompletionRepository
	retries int
}

func (r *vmPreparationRepository) RetryVMNetworkPreparation(_ context.Context, _ Work, delay time.Duration) error {
	if r.finishes != 1 || delay <= 0 {
		panic("preparation retry bypassed durable completion or backoff")
	}
	r.retries++
	return nil
}

func (p *vmPreparationProvider) Observe(context.Context, ProviderTarget) (ProviderObservation, error) {
	return ProviderObservation{Exists: true, Ready: p.ready, Identity: "subnet-uid"}, nil
}

func (p *vmPreparationProvider) PrepareVMNetwork(context.Context, ProviderTarget) error {
	if p.repository.finishes != 1 {
		panic("VM preparation ran before durable Network completion")
	}
	p.calls++
	return errors.New("NAD API unavailable")
}

func TestVMNetworkPreparationIsAncillaryToExistingDurableSubnetWorker(t *testing.T) {
	for _, ready := range []*bool{reportedReady(true), reportedReady(false), nil} {
		for _, kind := range []string{"subnet", "vpc"} {
			for _, finishErr := range []error{nil, ErrLeaseLost} {
				r := &vmPreparationRepository{earlyCompletionRepository: &earlyCompletionRepository{work: Work{Resource: ResourceWork{Kind: kind, State: Available}, KnownIdentity: "subnet-uid"}, err: finishErr}}
				p := &vmPreparationProvider{repository: r.earlyCompletionRepository, ready: ready}
				w, err := NewWorker(r, p, uuid.NewString(), DefaultWorkerPolicy())
				if err != nil {
					t.Fatal(err)
				}
				worked, err := w.Step(context.Background())
				if !worked || err != nil {
					t.Fatal("VM NAD failure changed ordinary Network completion", err)
				}
				expected := 0
				if kind == "subnet" && finishErr == nil {
					expected = 1
				}
				if p.calls != expected || r.retries != expected {
					t.Fatal("preparation bypassed successful Subnet completion", kind, finishErr, p.calls)
				}
			}
		}
	}
}
