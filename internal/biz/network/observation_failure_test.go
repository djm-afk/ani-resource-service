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
		{"wrong owner", &ProviderError{Kind: ProviderConflict}, Available},
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

func TestLBObservationErrorIsProjectedWithoutAnAgeThreshold(t *testing.T) {
	old := time.Now().Add(-365 * 24 * time.Hour)
	l := &LoadBalancers{}
	v := l.observation(LoadBalancer{EgressMetadata: EgressMetadata{State: Available, ObservedAt: &old, ObservationStale: true}})
	if v.State != Available || !v.ObservationStale || v.ObservedAt == nil || !v.ObservedAt.Equal(old) {
		t.Fatal(v)
	}
}
