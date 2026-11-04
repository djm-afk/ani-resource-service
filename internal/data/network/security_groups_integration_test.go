package data_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	data "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
)

// Only the external KCN API is replaced. The use case, SQL transactions,
// reference fences, worker leases and operation replay below are real code.
type securityGroupProviderBoundary struct{}

func (securityGroupProviderBoundary) ApplySecurityGroup(_ context.Context, w biz.SecurityGroupWork) (biz.SecurityGroupResult, error) {
	if w.TargetType != "security_group" {
		return biz.SecurityGroupResult{State: "applied", AppliedIDs: slices.Clone(w.Binding.SecurityGroupIDs), Complete: true, Observed: true}, nil
	}
	state := string(biz.Available)
	if w.Group.State == biz.Deleting {
		state = string(biz.Deleted)
	}
	return biz.SecurityGroupResult{ProviderUID: "provider-" + w.TargetID, State: state, Complete: true, Observed: true}, nil
}

type securityGroupContractFixture struct {
	p      *data.Postgres
	owner  *pgxpool.Pool
	groups *biz.SecurityGroups
	worker *biz.SecurityGroupWorker
	ctx    context.Context
	tenant string
}

func newSecurityGroupContractFixture(t *testing.T) *securityGroupContractFixture {
	t.Helper()
	p, owner := database(t)
	tenant := uuid.NewString()
	ctx := biz.WithEgressCaller(context.Background(), biz.EgressCaller{TenantID: tenant, Attribution: biz.Attribution{Actor: "security-group-test", DirectCaller: "controlled-boundary"}})
	return &securityGroupContractFixture{p: p, owner: owner, groups: biz.NewSecurityGroups(p), worker: biz.NewSecurityGroupWorker(p, securityGroupProviderBoundary{}, uuid.NewString(), biz.DefaultWorkerPolicy()), ctx: ctx, tenant: tenant}
}
func (f *securityGroupContractFixture) available(t *testing.T, id string) biz.SecurityGroup {
	t.Helper()
	for range 12 {
		current, err := f.groups.Get(f.ctx, f.tenant, id)
		if err != nil {
			t.Fatal(err)
		}
		if current.State == biz.Available {
			return current
		}
		if worked, err := f.worker.Step(f.ctx); err != nil || !worked {
			t.Fatalf("SG worker did not advance: worked=%v error=%v", worked, err)
		}
	}
	t.Fatal("SG did not become available")
	return biz.SecurityGroup{}
}
func (f *securityGroupContractFixture) create(t *testing.T, key string) biz.SecurityGroup {
	t.Helper()
	m, err := f.groups.Mutate(f.ctx, biz.SecurityGroupIntent{Action: "create", Name: key, IdempotencyKey: key})
	if err != nil {
		t.Fatal(err)
	}
	return f.available(t, m.SecurityGroup.ID)
}
func requireSecurityGroupReason(t *testing.T, err error, want biz.Reason) {
	t.Helper()
	if biz.ReasonOf(err) != want {
		t.Fatalf("expected %s, got %v", want, err)
	}
}

func TestSecurityGroupsPostgresLifecycleRulesAndPermanentReplay(t *testing.T) {
	f := newSecurityGroupContractFixture(t)
	intent := biz.SecurityGroupIntent{Action: "create", Name: "web", IdempotencyKey: "web", Rules: []biz.SecurityGroupRule{{Direction: "ingress", Protocol: "tcp", Action: "allow", Priority: 3000, RemoteAddresses: []string{"192.0.2.0/24"}, PortRanges: []biz.SecurityGroupPortRange{{Min: 443, Max: 443}}}}}
	first, err := f.groups.Mutate(f.ctx, intent)
	if err != nil {
		t.Fatal(err)
	}
	if errors := validation.IsDNS1123Subdomain(first.SecurityGroup.ProviderName); len(errors) > 0 {
		t.Fatal("provider name is not a Kubernetes resource name", first.SecurityGroup.ProviderName, errors)
	}
	defaults := 0
	for _, rule := range first.SecurityGroup.Rules {
		if rule.IsDefault {
			defaults++
			if rule.Direction != "ingress" || rule.Protocol != "all" || rule.Action != "drop" || rule.Priority != 1 {
				t.Fatal("wrong default policy", rule)
			}
		}
		if rule.Direction == "egress" {
			t.Fatal("creation invented an egress default", rule)
		}
	}
	if defaults != 1 {
		t.Fatal("creation did not append exactly one managed ingress drop", first.SecurityGroup.Rules)
	}
	replay, err := f.groups.Mutate(f.ctx, intent)
	if err != nil || replay.SecurityGroup.ID != first.SecurityGroup.ID || replay.Operation.ID != first.Operation.ID {
		t.Fatal("same intent replay changed durable identity", replay, err)
	}
	changed := intent
	changed.Name = "other"
	_, err = f.groups.Mutate(f.ctx, changed)
	requireSecurityGroupReason(t, err, biz.IdempotencyConflict)
	current := f.available(t, first.SecurityGroup.ID)
	operation, err := f.groups.Operation(f.ctx, f.tenant, first.Operation.ID)
	if err != nil || operation.State != biz.Succeeded {
		t.Fatal("worker did not persist operation success", operation, err)
	}
	foreign := biz.WithEgressCaller(context.Background(), biz.EgressCaller{TenantID: uuid.NewString(), Attribution: biz.Attribution{Actor: "other", DirectCaller: "test"}})
	_, err = f.groups.Get(foreign, "", current.ID)
	requireSecurityGroupReason(t, err, biz.ResourceNotFound)
	_, err = f.groups.Get(foreign, f.tenant, current.ID)
	requireSecurityGroupReason(t, err, biz.PermissionDenied)
	add := biz.SecurityGroupIntent{Action: "create_rule", ID: current.ID, ExpectedVersion: current.Version, IdempotencyKey: "add-dns", Rule: biz.SecurityGroupRule{Direction: "egress", Protocol: "udp", Action: "allow", Priority: 2000, PortRanges: []biz.SecurityGroupPortRange{{Min: 53, Max: 53}}}}
	added, err := f.groups.Mutate(f.ctx, add)
	if err != nil || added.Rule.ID == "" {
		t.Fatal(added, err)
	}
	stale := add
	stale.IdempotencyKey = "stale-version"
	_, err = f.groups.Mutate(f.ctx, stale)
	requireSecurityGroupReason(t, err, biz.VersionConflict)
	_, err = f.groups.Mutate(f.ctx, biz.SecurityGroupIntent{Action: "delete_rule", ID: current.ID, RuleID: "sgr_default_ingress_drop", ExpectedVersion: added.SecurityGroup.Version})
	requireSecurityGroupReason(t, err, biz.InvalidArgument)
	_, err = f.groups.Mutate(f.ctx, biz.SecurityGroupIntent{Action: "update_rule", ID: current.ID, RuleID: "sgr_default_ingress_drop", ExpectedVersion: added.SecurityGroup.Version, IdempotencyKey: "change-default", Rule: biz.SecurityGroupRule{Direction: "ingress", Protocol: "all", Action: "allow", Priority: 2}})
	requireSecurityGroupReason(t, err, biz.InvalidArgument)
	again, err := f.groups.Mutate(f.ctx, intent)
	if err != nil || again.Operation.ID != first.Operation.ID || again.SecurityGroup.Version != 1 {
		t.Fatal("later edits changed initial replay", again, err)
	}
}

func TestSecurityGroupsPostgresConsumerReferenceRevisionFence(t *testing.T) {
	f := newSecurityGroupContractFixture(t)
	old := f.create(t, "old")
	next := f.create(t, "next")
	consumer := uuid.NewString()
	if _, err := f.p.SyncSecurityGroupBinding(f.ctx, f.tenant, "container", consumer, "", old.Namespace, 1, []string{old.ID}); err != nil {
		t.Fatal(err)
	}
	if err := f.p.ConfirmSecurityGroupBinding(f.ctx, f.tenant, "container", consumer, 1, []string{old.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.p.SyncSecurityGroupBinding(f.ctx, f.tenant, "container", consumer, "", old.Namespace, 2, []string{next.ID}); err != nil {
		t.Fatal(err)
	}
	// The pending management submission retains its prior accepted references
	// until the consumer confirms the exact new configuration revision.
	for _, id := range []string{old.ID, next.ID} {
		v, err := f.groups.Get(f.ctx, "", id)
		if err != nil || v.BoundResourceCount != 1 {
			t.Fatal("Sync changed references before configuration confirmation", v, err)
		}
	}
	err := f.p.ConfirmSecurityGroupBinding(f.ctx, f.tenant, "container", consumer, 1, []string{old.ID})
	requireSecurityGroupReason(t, err, biz.VersionConflict)
	err = f.p.ConfirmSecurityGroupBinding(f.ctx, f.tenant, "container", consumer, 2, []string{old.ID})
	requireSecurityGroupReason(t, err, biz.VersionConflict)
	_, err = f.p.SyncSecurityGroupBinding(f.ctx, f.tenant, "container", consumer, "", old.Namespace, 1, []string{old.ID})
	requireSecurityGroupReason(t, err, biz.VersionConflict)
	if err = f.p.ConfirmSecurityGroupBinding(f.ctx, f.tenant, "container", consumer, 2, []string{next.ID}); err != nil {
		t.Fatal(err)
	}
	oldCurrent, _ := f.groups.Get(f.ctx, "", old.ID)
	nextCurrent, _ := f.groups.Get(f.ctx, "", next.ID)
	if oldCurrent.BoundResourceCount != 0 || nextCurrent.BoundResourceCount != 1 {
		t.Fatal("Confirm did not retire exactly old references", oldCurrent, nextCurrent)
	}
	if err = f.p.ReleaseSecurityGroupBinding(f.ctx, f.tenant, "container", consumer, 3); err != nil {
		t.Fatal(err)
	}
	for _, version := range []int64{1, 2, 3} {
		_, err = f.p.SyncSecurityGroupBinding(f.ctx, f.tenant, "container", consumer, "", old.Namespace, version, []string{old.ID})
		requireSecurityGroupReason(t, err, biz.VersionConflict)
	}
	bindings, total, err := f.groups.Bindings(f.ctx, biz.SecurityGroupFilter{ID: next.ID})
	if err != nil || total != 0 || len(bindings) != 0 {
		t.Fatal("released tombstone still exposes a binding", bindings, total, err)
	}
	if err = f.p.ReleaseSecurityGroupBinding(f.ctx, f.tenant, "container", consumer, 3); err != nil {
		t.Fatal("release replay changed", err)
	}
}

func TestSecurityGroupsPostgresUnboundEIPBindingVersionsAndDeleteProtection(t *testing.T) {
	e := newEgressFixture(t)
	groups := biz.NewSecurityGroups(e.p)
	worker := biz.NewSecurityGroupWorker(e.p, securityGroupProviderBoundary{}, uuid.NewString(), biz.DefaultWorkerPolicy())
	f := &securityGroupContractFixture{p: e.p, owner: e.owner, groups: groups, worker: worker, ctx: e.ctx, tenant: e.tenant}
	group := f.create(t, "eip-filter")
	address := e.eip(t, "unbound-address")
	var claimed bool
	if err := e.owner.QueryRow(e.ctx, "SELECT EXISTS(SELECT 1 FROM network_eip_claims WHERE tenant_id=$1 AND eip_id=$2 AND state<>'released')", e.tenant, address.ID).Scan(&claimed); err != nil || claimed {
		t.Fatal("fixture EIP was already bound", claimed, err)
	}
	before, err := groups.GetEIP(e.ctx, "", address.ID)
	if err != nil || before.Version != 1 || len(before.SecurityGroupIDs) != 0 {
		t.Fatal("initial binding version is not actionable", before, err)
	}
	intent := biz.SecurityGroupIntent{ID: address.ID, ExpectedVersion: before.Version, IdempotencyKey: "set-unbound-eip", SecurityGroupIDs: []string{group.ID}}
	first, err := groups.SetEIP(e.ctx, intent)
	if err != nil || first.Binding.Version != 2 {
		t.Fatal("first Set did not use binding version", first, err)
	}
	replay, err := groups.SetEIP(e.ctx, intent)
	if err != nil || replay.Operation.ID != first.Operation.ID {
		t.Fatal("accepted EIP operation replay changed", replay, err)
	}
	_, err = groups.Mutate(e.ctx, biz.SecurityGroupIntent{Action: "delete", ID: group.ID})
	requireSecurityGroupReason(t, err, biz.ResourceInUse)
	stale := intent
	stale.IdempotencyKey = "new-key-old-version"
	_, err = groups.SetEIP(e.ctx, stale)
	requireSecurityGroupReason(t, err, biz.VersionConflict)
	if worked, err := worker.Step(e.ctx); err != nil || !worked {
		t.Fatal("EIP SG worker did not advance", worked, err)
	}
	applied, err := groups.GetEIP(e.ctx, "", address.ID)
	if err != nil || applied.Version != 2 || !slices.Equal(applied.AppliedSecurityGroupIDs, []string{group.ID}) {
		t.Fatal(applied, err)
	}
	// EIP metadata version and SG binding version are independent. The current
	// binding version is the precondition for the next complete replacement.
	clear := biz.SecurityGroupIntent{ID: address.ID, ExpectedVersion: applied.Version, IdempotencyKey: "clear-unbound-eip", SecurityGroupIDs: []string{}}
	removed, err := groups.SetEIP(e.ctx, clear)
	if err != nil || removed.Binding.Version != 3 {
		t.Fatal(removed, err)
	}
	if worked, err := worker.Step(e.ctx); err != nil || !worked {
		t.Fatal(worked, err)
	}
	gone, err := groups.Get(e.ctx, "", group.ID)
	if err != nil || gone.BoundResourceCount != 0 {
		t.Fatal("verified empty replacement leaked reference", gone, err)
	}
	original, err := groups.SetEIP(e.ctx, intent)
	if err != nil || original.Operation.ID != first.Operation.ID || original.Binding.Version != 2 {
		t.Fatal("terminal initial replay changed", original, err)
	}
	if _, err = groups.Mutate(e.ctx, biz.SecurityGroupIntent{Action: "delete", ID: group.ID}); err != nil {
		t.Fatal("unbound and cleared group could not delete", err)
	}
	foreign := biz.WithEgressCaller(context.Background(), biz.EgressCaller{TenantID: uuid.NewString(), Attribution: biz.Attribution{Actor: "other", DirectCaller: "test"}})
	_, err = groups.GetEIP(foreign, "", address.ID)
	requireSecurityGroupReason(t, err, biz.ResourceNotFound)
}

func TestSecurityGroupReadFailurePreservesProjectionAndCompletedOperation(t *testing.T) {
	f := newSecurityGroupContractFixture(t)
	group := f.create(t, "read-failure")
	snapshot := func() string {
		t.Helper()
		var value string
		if err := f.owner.QueryRow(f.ctx, `SELECT jsonb_build_array(g.payload,o.payload)::text FROM network_security_groups g JOIN network_security_group_operations o ON o.tenant_id=g.tenant_id AND o.operation_id=$3 WHERE g.tenant_id=$1 AND g.security_group_id=$2`, f.tenant, group.ID, group.LastOperationID).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	before := snapshot()
	if err := f.p.NotifyObservedObject(f.ctx, schema.GroupVersionResource{Group: "networking.kubercloud.com", Version: "v1", Resource: "securitygroups"}, group.Namespace, group.ProviderName); err != nil {
		t.Fatal(err)
	}
	work, found, err := f.p.ClaimSecurityGroup(f.ctx, uuid.NewString(), time.Second)
	if err != nil || !found || work.Operation.CompletedAt == nil {
		t.Fatal(work, found, err)
	}
	failure := &biz.ProviderError{Kind: biz.ProviderTemporary}
	if err = f.p.FinishSecurityGroup(f.ctx, work, biz.SecurityGroupResult{State: "failed", Reason: "must-not-replace-status", ObservationError: biz.ProviderUnavailable, OperationReason: biz.ProviderUnavailable, Backoff: true, NextDelay: time.Second}, failure); err != nil {
		t.Fatal(err)
	}
	if snapshot() != before {
		t.Fatal("read failure overwrote resource projection or immutable operation")
	}
	var diagnostic string
	if err = f.owner.QueryRow(f.ctx, `SELECT observation_error FROM network_security_group_reconciliations WHERE tenant_id=$1 AND target_type='security_group' AND target_id=$2`, f.tenant, group.ID).Scan(&diagnostic); err != nil || diagnostic != string(biz.ProviderUnavailable) {
		t.Fatal(diagnostic, err)
	}
}
