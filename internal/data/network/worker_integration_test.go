package data_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
)

// memoryProvider represents the external dependency, never a runtime backend.
type memoryProvider struct {
	mu      sync.Mutex
	objects map[string]biz.ProviderObservation
}

func (p *memoryProvider) Observe(_ context.Context, target biz.ProviderTarget) (biz.ProviderObservation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.objects[target.ResourceID], nil
}

func (p *memoryProvider) EnsureVPC(_ context.Context, target biz.ProviderTarget) (biz.ProviderObservation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.objects == nil {
		p.objects = make(map[string]biz.ProviderObservation)
	}
	value, exists := p.objects[target.ResourceID]
	if !exists {
		value = biz.ProviderObservation{Exists: true, Ready: egressReportedReady(true), Identity: uuid.NewString()}
		p.objects[target.ResourceID] = value
	}
	return value, nil
}

func (p *memoryProvider) Delete(_ context.Context, target biz.ProviderTarget) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	value := p.objects[target.ResourceID]
	if value.Exists && value.Identity != target.KnownIdentity {
		return &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	delete(p.objects, target.ResourceID)
	return nil
}

func TestNetworkWorkerOwnsVPCCreationAndConfirmedDeletion(t *testing.T) {
	repository, _ := database(t)
	network, err := biz.NewNetwork(repository, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	policy := biz.DefaultWorkerPolicy()
	policy.ObserveEvery = 20 * time.Millisecond
	policy.RetryMin = 10 * time.Millisecond
	policy.RetryMax = 30 * time.Millisecond
	worker, err := biz.NewWorker(repository, &memoryProvider{}, uuid.NewString(), policy)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	input := biz.CreateVPC{TenantID: "7a7750cf-73b0-49c5-a3b1-4dba42689401", Name: "worker", CIDR: "10.0.0.0/16", IdempotencyKey: "worker"}
	accepted, err := network.CreateVPC(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	drive := func(expected biz.ResourceState) biz.VPC {
		t.Helper()
		deadline := time.Now().Add(4 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := worker.Step(ctx); err != nil {
				t.Fatal(err)
			}
			value, err := network.GetVPC(ctx, input.TenantID, accepted.ID)
			if err != nil {
				t.Fatal(err)
			}
			if value.State == expected {
				return value
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("worker never reached %s", expected)
		return biz.VPC{}
	}
	available := drive(biz.Available)
	if available.ObservationStale || available.ObservedAt == nil {
		t.Fatal("successful read did not record its timestamp and clear read errors")
	}
	deleting, err := network.DeleteVPC(ctx, input.TenantID, accepted.ID)
	if err != nil || deleting.State != biz.Deleting || deleting.LastOperationID == accepted.LastOperationID {
		t.Fatalf("delete not durably accepted: %+v %v", deleting, err)
	}
	deleted := drive(biz.Deleted)
	again, err := network.DeleteVPC(ctx, input.TenantID, accepted.ID)
	if err != nil || again.LastOperationID != deleted.LastOperationID {
		t.Fatalf("delete replay changed operation: %+v %v", again, err)
	}
	replay, err := network.CreateVPC(ctx, input)
	if err != nil || replay.ID != accepted.ID || replay.State != biz.Provisioning || replay.LastOperationID != accepted.LastOperationID {
		t.Fatalf("create replay resurrected resource or changed receipt: %+v %v", replay, err)
	}
	creation, err := network.GetOperation(ctx, input.TenantID, accepted.LastOperationID)
	if err != nil || creation.State != biz.Succeeded {
		t.Fatalf("historic successful creation changed: %+v %v", creation, err)
	}
}
