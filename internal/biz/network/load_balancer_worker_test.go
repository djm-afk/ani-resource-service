package biz

import (
	"context"
	"errors"
	"testing"
)

type deferredLBWorkerRepository struct {
	WorkRepository
	progress    Progress
	begins      []string
	finishError error
}

func (r *deferredLBWorkerRepository) BeginLoadBalancerMutation(_ context.Context, _ Work, c LoadBalancerComponent, action, _ string) error {
	r.begins = append(r.begins, c.Kind+":"+action)
	return nil
}

func (r *deferredLBWorkerRepository) Finish(_ context.Context, _ Work, p Progress) error {
	r.progress = p
	return r.finishError
}

type deferredLBWorkerProvider struct {
	ResourceProvider
	observation  LoadBalancerObservation
	mutations    []string
	observations int
	readErr      error
}

func (p *deferredLBWorkerProvider) ObserveLoadBalancer(context.Context, Work) (LoadBalancerObservation, error) {
	p.observations++
	return p.observation, p.readErr
}

func (p *deferredLBWorkerProvider) MutateLoadBalancer(_ context.Context, _ Work, c LoadBalancerComponent, action string, _ LoadBalancerObservation) (LoadBalancerComponentObservation, error) {
	p.mutations = append(p.mutations, c.Kind+":"+action)
	return LoadBalancerComponentObservation{ID: c.ID, Identity: c.Identity, Exists: true, Deleting: action == "delete"}, nil
}

func deferredLBWorkerFixture() (Work, *deferredLBWorkerRepository, *deferredLBWorkerProvider, *Worker) {
	work := Work{Resource: ResourceWork{Kind: "load_balancer", State: Available, LoadBalancer: &LoadBalancerWork{
		LoadBalancer: LoadBalancer{DesiredVersion: 4, ProviderTargetVersion: 4, AppliedVersion: 2, Listeners: []LoadBalancerListener{{ID: "listener", Name: "waiting", Protocol: "HTTP", Port: 80}}},
		Components:   []LoadBalancerComponent{{ID: "gateway", Kind: "gateway", Identity: "uid-gateway", CreateDispatched: true}, {ID: "route", Kind: "route", ListenerID: "listener", Identity: "uid-route", CreateDispatched: true}, {ID: "backend", Kind: "backend", MemberID: "old-member", Identity: "uid-backend", CreateDispatched: true}},
	}}, BindingID: "gateway", KnownIdentity: "uid-gateway", ActiveOperation: true}
	repository := &deferredLBWorkerRepository{}
	provider := &deferredLBWorkerProvider{observation: LoadBalancerObservation{Components: []LoadBalancerComponentObservation{{ID: "gateway", Identity: "uid-gateway", Exists: true}, {ID: "route", Identity: "uid-route", Exists: true}, {ID: "backend", Identity: "uid-backend", Exists: true}}}}
	worker := &Worker{repository: repository, provider: provider, policy: DefaultWorkerPolicy()}
	return work, repository, provider, worker
}

func TestLBWorkerClearingLastBackendDeletesOwnDeclarationsInOrder(t *testing.T) {
	work, repository, provider, worker := deferredLBWorkerFixture()
	work.Resource.LoadBalancer.Components = append(work.Resource.LoadBalancer.Components, LoadBalancerComponent{ID: "proxy", Kind: "envoy_proxy", Identity: "uid-proxy"})
	provider.observation.Components = append(provider.observation.Components, LoadBalancerComponentObservation{ID: "proxy", Identity: "uid-proxy", Exists: true})
	for step, index := range []int{1, 0, 2, 3} {
		if err := worker.stepLoadBalancer(context.Background(), work); err != nil {
			t.Fatal(err)
		}
		want := []string{"route:delete", "gateway:delete", "backend:delete", "envoy_proxy:delete"}[step]
		if provider.mutations[len(provider.mutations)-1] != want {
			t.Fatal(provider.mutations)
		}
		provider.observation.Components[index].Exists = false
	}
	if err := worker.stepLoadBalancer(context.Background(), work); err != nil {
		t.Fatal(err)
	}
	if repository.progress.OperationState != Succeeded || repository.progress.LoadBalancer.ConfigurationState != "waiting_for_backends" {
		t.Fatal(repository.progress)
	}
}

func TestLBWorkerAuthoritativeAbsenceFinishesPendingCreateDeletion(t *testing.T) {
	work, repository, provider, worker := deferredLBWorkerFixture()
	work.Resource.State = Deleting
	work.Resource.LoadBalancer.Components = []LoadBalancerComponent{{ID: "gateway", Kind: "gateway", PendingAction: "create", CreateDispatched: true}}
	work.KnownIdentity = ""
	provider.observation.Components = []LoadBalancerComponentObservation{{ID: "gateway"}}
	if err := worker.stepLoadBalancer(context.Background(), work); err != nil {
		t.Fatal(err)
	}
	if repository.progress.State != Deleted || repository.progress.OperationState != Succeeded || len(provider.mutations) != 0 {
		t.Fatal(repository.progress, provider.mutations)
	}
}

func TestLBWorkerPureDBVersionDoesNotAdvanceAppliedTarget(t *testing.T) {
	work, repository, provider, worker := deferredLBWorkerFixture()
	work.Resource.LoadBalancer.LoadBalancer = LoadBalancer{DesiredVersion: 3, ProviderTargetVersion: 2, AppliedVersion: 2, Listeners: []LoadBalancerListener{{ID: "runtime", Name: "runtime", Protocol: "HTTP", Backends: []LoadBalancerBackend{{ID: "member", Weight: 0}}}, {ID: "waiting", Name: "waiting", Protocol: "TCP"}}}
	work.Resource.LoadBalancer.Components = []LoadBalancerComponent{{ID: "envoy-proxy", Kind: "envoy_proxy", Identity: "uid-proxy"}, {ID: "gateway", Kind: "gateway", Identity: "uid-gateway"}, {ID: "route", Kind: "route", ListenerID: "runtime", Identity: "uid-route"}, {ID: "policy", Kind: "policy", ListenerID: "runtime", Identity: "uid-policy"}, {ID: "backend", Kind: "backend", MemberID: "member", Identity: "uid-backend"}}
	provider.observation = LoadBalancerObservation{Components: []LoadBalancerComponentObservation{{ID: "envoy-proxy", Identity: "uid-proxy", Exists: true, Matches: true, Ready: lbReportedReady(true)}, {ID: "gateway", Identity: "uid-gateway", Exists: true, Matches: true, Ready: lbReportedReady(true)}, {ID: "route", Identity: "uid-route", Exists: true, Matches: true, Ready: lbReportedReady(true)}, {ID: "policy", Identity: "uid-policy", Exists: true, Matches: true, Ready: lbReportedReady(true)}, {ID: "backend", Identity: "uid-backend", Exists: true, Matches: true, Ready: lbReportedReady(true)}}, Members: []LoadBalancerMemberObservation{{ID: "member", Eligible: true}}}
	if err := worker.stepLoadBalancer(context.Background(), work); err != nil {
		t.Fatal(err)
	}
	if len(provider.mutations) != 0 || repository.progress.State != Available || repository.progress.LoadBalancer.AppliedVersion != 2 {
		t.Fatal("DB-only listener edit mutated runtime or advanced applied to desired", repository.progress, provider.mutations)
	}
	for _, c := range repository.progress.LoadBalancer.Components {
		if c.AppliedVersion != 2 {
			t.Fatal("component observation used desired instead of provider target", c)
		}
	}
	provider.observation.Members[0].Eligible = false
	provider.observation.Components[2].Matches = false
	if err := worker.stepLoadBalancer(context.Background(), work); err != nil {
		t.Fatal(err)
	}
	if repository.progress.Reason != BackendIdentityMismatch || repository.progress.LoadBalancer.ConfigurationState == "waiting_for_backends" {
		t.Fatal("invalid zero-weight member became normal waiting configuration", repository.progress)
	}
	if len(provider.mutations) != 1 || provider.mutations[0] != "route:update" {
		t.Fatal("invalid member was not withdrawn before other writes", provider.mutations)
	}
}

func TestLBWorkerRepositoryUnsentCancellationAvoidsProvider(t *testing.T) {
	work := Work{CancelUnsent: true, Resource: ResourceWork{Kind: "load_balancer", State: Deleting, LoadBalancer: &LoadBalancerWork{}}}
	repository := &deferredLBWorkerRepository{finishError: ErrLeaseLost}
	provider := &deferredLBWorkerProvider{}
	worker := &Worker{repository: repository, provider: provider, policy: DefaultWorkerPolicy()}
	if err := worker.stepLoadBalancer(context.Background(), work); err != nil {
		t.Fatal(err)
	}
	if provider.observations != 0 || repository.progress.State != Deleted {
		t.Fatal(repository.progress)
	}
	repository.finishError = errors.New("database unavailable")
	if err := worker.stepLoadBalancer(context.Background(), work); !errors.Is(err, repository.finishError) {
		t.Fatal(err)
	}
}

func TestLBWorkerRuntimeRequiresEveryProjectedComponent(t *testing.T) {
	work, repository, provider, worker := deferredLBWorkerFixture()
	work.Resource.LoadBalancer.LoadBalancer.Listeners[0].Backends = []LoadBalancerBackend{{ID: "member", Weight: 1}}
	work.Resource.LoadBalancer.Components = []LoadBalancerComponent{{ID: "gateway", Kind: "gateway", Identity: "uid-gateway"}}
	provider.observation = LoadBalancerObservation{Components: []LoadBalancerComponentObservation{{ID: "gateway", Identity: "uid-gateway", Exists: true, Matches: true, Ready: lbReportedReady(true)}}, Members: []LoadBalancerMemberObservation{{ID: "member", Eligible: true}}}
	if err := worker.stepLoadBalancer(context.Background(), work); err != nil {
		t.Fatal(err)
	}
	if repository.progress.OperationState == Succeeded || repository.progress.LoadBalancer.ConfigurationState == "configured" {
		t.Fatal("Gateway readiness replaced missing Route/Policy/Backend facts", repository.progress)
	}
}

func lbReportedReady(value bool) *bool { return &value }

func TestLBReadErrorPreservesLastProjection(t *testing.T) {
	work, repository, provider, worker := deferredLBWorkerFixture()
	work.Resource.Reason = Reason("LAST_REPORTED_REASON")
	provider.readErr = &ProviderError{Kind: ProviderTemporary}
	if err := worker.stepLoadBalancer(context.Background(), work); err != nil {
		t.Fatal(err)
	}
	p := repository.progress
	if p.State != Available || p.Reason != work.Resource.Reason || p.Observed || p.LoadBalancer != nil || p.ObservationError != ProviderUnavailable {
		t.Fatal(p)
	}
}
