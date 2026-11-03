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
}

func (p *deferredLBWorkerProvider) ObserveLoadBalancer(context.Context, Work) (LoadBalancerObservation, error) {
	p.observations++
	return p.observation, nil
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

func TestLBWorkerClearingLastBackendWaitsForGeneratedAndAddressRelease(t *testing.T) {
	work, repository, provider, worker := deferredLBWorkerFixture()
	work.Resource.LoadBalancer.Components = append(work.Resource.LoadBalancer.Components, LoadBalancerComponent{ID: "proxy", Kind: "envoy_proxy", Identity: "uid-proxy", CreateDispatched: true})
	provider.observation.Components = append(provider.observation.Components, LoadBalancerComponentObservation{ID: "proxy", Identity: "uid-proxy", Exists: true})
	if err := worker.stepLoadBalancer(context.Background(), work); err != nil {
		t.Fatal(err)
	}
	if len(provider.mutations) != 1 || provider.mutations[0] != "route:delete" {
		t.Fatal("cleanup did not withdraw forwarding first", provider.mutations)
	}
	provider.observation.Components[1].Exists = false
	if err := worker.stepLoadBalancer(context.Background(), work); err != nil {
		t.Fatal(err)
	}
	if len(provider.mutations) != 2 || provider.mutations[1] != "gateway:delete" {
		t.Fatal("shared gateway was recreated instead of removed", provider.mutations)
	}
	provider.observation.Components[0].Exists = false
	if err := worker.stepLoadBalancer(context.Background(), work); err != nil {
		t.Fatal(err)
	}
	if len(provider.mutations) != 2 || repository.progress.OperationState == Succeeded || repository.progress.Reason != CleanupPending {
		t.Fatal("backend cleanup bypassed generated/address release", provider.mutations, repository.progress)
	}
	provider.observation.GeneratedReleased = true
	if err := worker.stepLoadBalancer(context.Background(), work); err != nil {
		t.Fatal(err)
	}
	if len(provider.mutations) != 2 {
		t.Fatal("generated absence was substituted for actual address release", provider.mutations)
	}
	provider.observation.AddressReleased = true
	if err := worker.stepLoadBalancer(context.Background(), work); err != nil {
		t.Fatal(err)
	}
	if len(provider.mutations) != 3 || provider.mutations[2] != "backend:delete" {
		t.Fatal("backend cleanup did not resume after release", provider.mutations)
	}
	provider.observation.Components[2].Exists = false
	if err := worker.stepLoadBalancer(context.Background(), work); err != nil {
		t.Fatal(err)
	}
	if len(provider.mutations) != 4 || provider.mutations[3] != "envoy_proxy:delete" {
		t.Fatal("dedicated Proxy did not retire after Gateway and address cleanup", provider.mutations)
	}
	provider.observation.Components[3].Exists = false
	if err := worker.stepLoadBalancer(context.Background(), work); err != nil {
		t.Fatal(err)
	}
	p := repository.progress
	if p.State != Provisioning || p.OperationState != Succeeded || p.Reason != "" || p.LoadBalancer.ConfigurationState != "waiting_for_backends" || p.LoadBalancer.AppliedVersion != 4 {
		t.Fatal("confirmed cleanup did not complete at provider target", p)
	}
}

func TestLBWorkerEmptyIntentDoesNotEraseUnknownCreate(t *testing.T) {
	for _, kind := range []string{"gateway", "envoy_proxy"} {
		t.Run(kind, func(t *testing.T) {
			work, repository, provider, worker := deferredLBWorkerFixture()
			work.Resource.LoadBalancer.Components = []LoadBalancerComponent{{ID: kind, Kind: kind, PendingAction: "create", CreateDispatched: true}}
			work.KnownIdentity = ""
			provider.observation.Components = []LoadBalancerComponentObservation{{ID: kind}}
			provider.observation.GeneratedReleased, provider.observation.AddressReleased = true, true
			if err := worker.stepLoadBalancer(context.Background(), work); err != nil {
				t.Fatal(err)
			}
			if len(provider.mutations) != 0 || repository.progress.Reason != ProviderUnknown || repository.progress.OperationState != Blocked || repository.progress.LoadBalancer.ConfigurationState == "waiting_for_backends" {
				t.Fatal("empty desired intent bypassed unknown CREATE", repository.progress, provider.mutations)
			}
		})
	}
}

func TestLBWorkerPureDBVersionDoesNotAdvanceAppliedTarget(t *testing.T) {
	work, repository, provider, worker := deferredLBWorkerFixture()
	work.Resource.LoadBalancer.LoadBalancer = LoadBalancer{DesiredVersion: 3, ProviderTargetVersion: 2, AppliedVersion: 2, Listeners: []LoadBalancerListener{{ID: "runtime", Name: "runtime", Protocol: "HTTP", Backends: []LoadBalancerBackend{{ID: "member", Weight: 0}}}, {ID: "waiting", Name: "waiting", Protocol: "TCP"}}}
	work.Resource.LoadBalancer.Components = []LoadBalancerComponent{{ID: "envoy-proxy", Kind: "envoy_proxy", Identity: "uid-proxy"}, {ID: "gateway", Kind: "gateway", Identity: "uid-gateway"}, {ID: "route", Kind: "route", ListenerID: "runtime", Identity: "uid-route"}, {ID: "policy", Kind: "policy", ListenerID: "runtime", Identity: "uid-policy"}, {ID: "backend", Kind: "backend", MemberID: "member", Identity: "uid-backend"}}
	provider.observation = LoadBalancerObservation{Components: []LoadBalancerComponentObservation{{ID: "envoy-proxy", Identity: "uid-proxy", Exists: true, Matches: true, Ready: true}, {ID: "gateway", Identity: "uid-gateway", Exists: true, Matches: true, Ready: true}, {ID: "route", Identity: "uid-route", Exists: true, Matches: true, Ready: true}, {ID: "policy", Identity: "uid-policy", Exists: true, Matches: true, Ready: true}, {ID: "backend", Identity: "uid-backend", Exists: true, Matches: true, Ready: true}}, Members: []LoadBalancerMemberObservation{{ID: "member", Eligible: true}}, DependenciesReady: true, GeneratedReady: true}
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

func TestLBWorkerRepositoryUnsentProofAndGateAvoidProvider(t *testing.T) {
	for _, work := range []Work{{CancelUnsent: true}, {GateReason: LoadBalancerNotReady}} {
		work.Resource = ResourceWork{Kind: "load_balancer", State: Deleting, LoadBalancer: &LoadBalancerWork{}}
		repository := &deferredLBWorkerRepository{finishError: ErrLeaseLost}
		provider := &deferredLBWorkerProvider{}
		worker := &Worker{repository: repository, provider: provider, policy: DefaultWorkerPolicy()}
		if err := worker.stepLoadBalancer(context.Background(), work); err != nil {
			t.Fatal("lost lease terminated worker", err)
		}
		if provider.observations != 0 || len(provider.mutations) != 0 {
			t.Fatal("repository-proven shortcut or admission gate called provider")
		}
		if work.CancelUnsent && repository.progress.State != Deleted {
			t.Fatal("never-dispatched delete was not completed")
		}
		if work.GateReason != "" && repository.progress.Reason != LoadBalancerNotReady {
			t.Fatal("activation gate was lost")
		}
		repository.finishError = errors.New("database unavailable")
		if err := worker.stepLoadBalancer(context.Background(), work); !errors.Is(err, repository.finishError) {
			t.Fatal("persistence failure was hidden", err)
		}
	}
}

func TestLBWorkerRuntimeRequiresEveryProjectedComponent(t *testing.T) {
	work, repository, provider, worker := deferredLBWorkerFixture()
	work.Resource.LoadBalancer.LoadBalancer.Listeners[0].Backends = []LoadBalancerBackend{{ID: "member", Weight: 1}}
	work.Resource.LoadBalancer.Components = []LoadBalancerComponent{{ID: "gateway", Kind: "gateway", Identity: "uid-gateway"}}
	provider.observation = LoadBalancerObservation{Components: []LoadBalancerComponentObservation{{ID: "gateway", Identity: "uid-gateway", Exists: true, Matches: true, Ready: true}}, Members: []LoadBalancerMemberObservation{{ID: "member", Eligible: true}}, DependenciesReady: true, GeneratedReady: true}
	if err := worker.stepLoadBalancer(context.Background(), work); err != nil {
		t.Fatal(err)
	}
	if repository.progress.OperationState == Succeeded || repository.progress.LoadBalancer.ConfigurationState == "configured" {
		t.Fatal("Gateway readiness replaced missing Route/Policy/Backend facts", repository.progress)
	}
}
