package instance

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

type sgWorkerRepository struct {
	Repository
	instance      Instance
	mutationCount int
}

func (r *sgWorkerRepository) Claim(context.Context, string, time.Duration) (*Work, error) {
	return &Work{Instance: r.instance, Operation: r.instance.ActiveOperation}, nil
}
func (r *sgWorkerRepository) BeginMutation(_ context.Context, _ Work, m Mutation) error {
	r.mutationCount++
	r.instance.Pending = &m
	return nil
}
func (r *sgWorkerRepository) Finish(_ context.Context, _ Work, p Progress) error {
	r.instance = p.Instance
	if p.Operation != nil {
		r.instance.ActiveOperation = p.Operation
	}
	return nil
}

type sgWorkerProvider struct {
	Provider
	err   error
	calls int
	names []string
}

func (p *sgWorkerProvider) SetSecurityGroups(_ context.Context, _ Instance, names []string) error {
	p.calls++
	p.names = append([]string{}, names...)
	return p.err
}

type sgWorkerNetwork struct {
	syncErr                       error
	confirms, releases            int
	syncVersions, confirmVersions []int64
}

func (*sgWorkerNetwork) ResolveSecurityGroups(context.Context, string, string, string, []string) ([]string, error) {
	return []string{"sg-new"}, nil
}
func (n *sgWorkerNetwork) SyncSecurityGroupBinding(_ context.Context, _, _, _, _, _ string, version int64, _ []string) ([]string, error) {
	n.syncVersions = append(n.syncVersions, version)
	return []string{"sg-new"}, n.syncErr
}
func (n *sgWorkerNetwork) ConfirmSecurityGroupBinding(_ context.Context, _, _, _ string, version int64, _ []string) error {
	n.confirmVersions = append(n.confirmVersions, version)
	n.confirms++
	return nil
}
func (n *sgWorkerNetwork) ReleaseSecurityGroupBinding(context.Context, string, string, string, int64) error {
	n.releases++
	return nil
}
func TestSecurityGroupsWorkerRetainsCurrentAndReferencesUntilRuntimeConfirmed(t *testing.T) {
	old := "sg_11111111111111111111111111111111"
	selected := "sg_22222222222222222222222222222222"
	i := Instance{TenantID: "tenant", ID: "instance", Version: 5, Desired: Running, RuntimeDesired: Running, Observed: Running, SecurityGroupsInitialized: true, SecurityGroupIDs: []string{old}, SecurityGroupNames: []string{"sg-old"}, Spec: Spec{SecurityGroupIDs: []string{old}, Network: &NetworkSelection{VPCID: "vpc"}, Namespace: "tenant-ns"}}
	i.ActiveOperation = &Operation{ID: "op", Action: "set-security-groups", Version: 5, State: "accepted", Execution: &Execution{SchemaVersion: 1, Phase: "applying_security_groups", PhaseSequence: 1, SecurityGroupIDs: []string{selected}}}
	repo := &sgWorkerRepository{instance: i}
	provider := &sgWorkerProvider{err: ErrVersion}
	network := &sgWorkerNetwork{}
	worker, err := NewWorker(repo, provider, "sg-test", discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	_ = worker.UseSecurityGroups(network)
	if _, err = worker.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if network.confirms != 0 || repo.instance.Pending == nil || !reflect.DeepEqual(repo.instance.SecurityGroupIDs, []string{old}) || repo.instance.ActiveOperation.State != "blocked" {
		t.Fatal("partial runtime mutation released old reference or committed target", repo.instance)
	}
	provider.err = nil
	if _, err = worker.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if network.confirms != 1 || repo.instance.Pending != nil || !reflect.DeepEqual(repo.instance.SecurityGroupIDs, []string{selected}) || repo.instance.ActiveOperation.State != "completed" || repo.mutationCount != 1 {
		t.Fatal("retry failed to converge using durable mutation", repo.instance)
	}
	if !reflect.DeepEqual(network.syncVersions, []int64{5, 5}) || !reflect.DeepEqual(network.confirmVersions, []int64{5}) || repo.instance.SecurityGroupRevision != 5 {
		t.Fatal("reference fence did not use the persisted worker version", network)
	}
	if !reflect.DeepEqual(repo.instance.Spec.SecurityGroupIDs, []string{old}) || repo.instance.Version != 5 || repo.instance.RuntimeDesired != Running {
		t.Fatal("security group update rewrote immutable create or run state")
	}
}
func TestSecurityGroupsWorkerDependencyFailureDoesNotMutateRuntime(t *testing.T) {
	repo := &sgWorkerRepository{instance: Instance{Spec: Spec{Network: &NetworkSelection{}}, ActiveOperation: &Operation{ID: "op", Action: "set-security-groups", Execution: &Execution{SchemaVersion: 1}}}}
	provider := &sgWorkerProvider{}
	network := &sgWorkerNetwork{syncErr: ErrNetworkNotFound}
	worker, _ := NewWorker(repo, provider, "sg-test", discardLogger())
	_ = worker.UseSecurityGroups(network)
	if _, err := worker.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if provider.calls != 0 || network.confirms != 0 || !errors.Is(network.syncErr, ErrNetworkNotFound) {
		t.Fatal("failed selection reached runtime")
	}
}
