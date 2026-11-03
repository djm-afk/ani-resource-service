package biz

import (
	"context"
	"errors"
	"testing"
	"time"
)

type unavailableLBProvider struct {
	ResourceProvider
	err error
}

func (p unavailableLBProvider) ObserveLoadBalancer(context.Context, Work) (LoadBalancerObservation, error) {
	return LoadBalancerObservation{}, p.err
}
func (p unavailableLBProvider) MutateLoadBalancer(context.Context, Work, LoadBalancerComponent, string, LoadBalancerObservation) (LoadBalancerComponentObservation, error) {
	panic("a failed observation cannot authorize a mutation")
}

func TestLBObservationFailurePreservesLastConfirmedState(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		state ResourceState
	}{
		{"timeout", context.DeadlineExceeded, Available},
		{"read failure", &ProviderError{Kind: ProviderTemporary, Cause: errors.New("API unavailable")}, Available},
		{"wrong owner", &ProviderError{Kind: ProviderConflict}, Degraded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			work, repository, _, worker := deferredLBWorkerFixture()
			work.ActiveOperation = false
			old := time.Now().Add(-5 * time.Minute)
			work.Resource.ObservedAt = &old
			worker.provider = unavailableLBProvider{err: tc.err}
			if err := worker.stepLoadBalancer(context.Background(), work); err != nil {
				t.Fatal(err)
			}
			p := repository.progress
			if p.State != tc.state || p.Observed || p.LoadBalancer != nil || p.ProviderError == nil || !p.Backoff || len(repository.begins) != 0 {
				t.Fatalf("observation failure changed confirmed facts or authorized work: %+v", p)
			}
		})
	}
}

func TestLBObservationErrorIsVisibleBeforeFreshnessExpires(t *testing.T) {
	now := time.Now()
	l := &LoadBalancers{now: func() time.Time { return now }, freshness: time.Minute}
	v := l.observation(LoadBalancer{EgressMetadata: EgressMetadata{State: Available, Reason: ProviderUnavailable, ObservedAt: &now}, DataPlaneState: "ready", DataPlaneObservedAt: &now})
	if v.State != Available || !v.ObservationStale || v.DataPlaneState != "unknown" || v.ObservedAt == nil || !v.ObservedAt.Equal(now) {
		t.Fatalf("observation failure hidden behind fresh green state: %+v", v)
	}
}
