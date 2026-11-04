package data_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network"
)

func (p *memoryProvider) EnsureSubnet(ctx context.Context, target biz.ProviderTarget) (biz.ProviderObservation, error) {
	return p.EnsureVPC(ctx, target)
}
func subnetWorker(t *testing.T, p *data.Postgres) *biz.Worker {
	t.Helper()
	policy := biz.DefaultWorkerPolicy()
	policy.ObserveEvery = 100 * time.Millisecond
	policy.RetryMin = time.Millisecond
	w, err := biz.NewWorker(p, &memoryProvider{}, uuid.NewString(), policy)
	if err != nil {
		t.Fatal(err)
	}
	return w
}
func availableVPC(t *testing.T, n *biz.Network, w *biz.Worker, tenant, key string) biz.VPC {
	t.Helper()
	v, err := n.CreateVPC(context.Background(), biz.CreateVPC{TenantID: tenant, Name: key, CIDR: "10.42.0.0/16", IdempotencyKey: key})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		if _, err := w.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
		current, err := n.GetVPC(context.Background(), tenant, v.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.State == biz.Available {
			return current
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("VPC did not converge")
	return v
}
func driveSubnet(t *testing.T, n *biz.Network, w *biz.Worker, s biz.Subnet, state biz.ResourceState) biz.Subnet {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < 200; i++ {
		if _, err := w.Step(ctx); err != nil {
			t.Fatal(err)
		}
		value, err := n.GetSubnet(ctx, s.TenantID, s.ID)
		if err != nil {
			t.Fatal(err)
		}
		if value.State == state {
			return value
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("subnet %s did not reach %s", s.ID, state)
	return s
}
func TestSubnetConcurrentPermanentIdempotencyAndDeletion(t *testing.T) {
	p, owner := database(t)
	n := newNetwork(t, p, time.Minute)
	w := subnetWorker(t, p)
	ctx := context.Background()
	tenant := uuid.NewString()
	v := availableVPC(t, n, w, tenant, "parent")
	input := biz.CreateSubnet{TenantID: tenant, VPCID: v.ID, Name: "subnet", CIDR: "10.42.1.0/24", Description: "persistent", IdempotencyKey: "same"}
	const count = 12
	values := make(chan biz.Subnet, count)
	errs := make(chan error, count)
	var group sync.WaitGroup
	for i := 0; i < count; i++ {
		group.Add(1)
		go func() { defer group.Done(); v, e := n.CreateSubnet(ctx, input); values <- v; errs <- e }()
	}
	group.Wait()
	close(values)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var accepted biz.Subnet
	for value := range values {
		if accepted.ID != "" && value.ID != accepted.ID {
			t.Fatal("duplicate subnet identity")
		}
		accepted = value
	}
	if accepted.Gateway != "10.42.1.1" || accepted.State != biz.Provisioning {
		t.Fatalf("acceptance: %+v", accepted)
	}
	got, err := n.GetVPC(ctx, tenant, v.ID)
	if err != nil || got.SubnetCount != 1 {
		t.Fatalf("count: %+v %v", got, err)
	}
	page, err := n.ListVPCs(ctx, biz.ListVPCs{TenantID: tenant})
	if err != nil || len(page.Items) != 1 || page.Items[0].SubnetCount != 1 {
		t.Fatalf("list snapshot count: %+v %v", page, err)
	}
	if _, err := n.DeleteVPC(ctx, tenant, v.ID); biz.ReasonOf(err) != biz.ResourceInUse {
		t.Fatal("parent deletion ignored child", err)
	}
	changed := input
	gateway := "10.42.1.2"
	changed.Gateway = &gateway
	if _, err := n.CreateSubnet(ctx, changed); biz.ReasonOf(err) != biz.IdempotencyConflict {
		t.Fatal("changed intent", err)
	}
	other := uuid.NewString()
	if _, err := n.GetSubnet(ctx, other, accepted.ID); biz.ReasonOf(err) != biz.ResourceNotFound {
		t.Fatal("cross-tenant read", err)
	}
	if _, err := n.DeleteSubnet(ctx, other, accepted.ID); biz.ReasonOf(err) != biz.ResourceNotFound {
		t.Fatal("cross-tenant delete", err)
	}
	if _, err := n.GetOperation(ctx, other, accepted.LastOperationID); biz.ReasonOf(err) != biz.ResourceNotFound {
		t.Fatal("cross-tenant operation", err)
	}
	foreign := input
	foreign.TenantID = other
	if _, err := n.CreateSubnet(ctx, foreign); biz.ReasonOf(err) != biz.ResourceNotFound {
		t.Fatal("cross-tenant parent", err)
	}
	op, err := n.GetOperation(ctx, tenant, accepted.LastOperationID)
	if err != nil || op.Kind != "create_subnet" || op.ResourceType != "subnet" || op.ResourceID != accepted.ID {
		t.Fatalf("operation: %+v %v", op, err)
	}
	driveSubnet(t, n, w, accepted, biz.Available)
	deletion, err := n.DeleteSubnet(ctx, tenant, accepted.ID)
	if err != nil {
		t.Fatal(err)
	}
	again, err := n.DeleteSubnet(ctx, tenant, accepted.ID)
	if err != nil || again.LastOperationID != deletion.LastOperationID {
		t.Fatal("deletion changed identity", err)
	}
	driveSubnet(t, n, w, accepted, biz.Deleted)
	if _, err := n.DeleteVPC(ctx, tenant, v.ID); err != nil {
		t.Fatal(err)
	}
	replay, err := n.CreateSubnet(ctx, input)
	if err != nil || replay.ID != accepted.ID || replay.State != biz.Provisioning || replay.LastOperationID != accepted.LastOperationID {
		t.Fatalf("deleted replay: %+v %v", replay, err)
	}
	current, err := n.GetSubnet(ctx, tenant, accepted.ID)
	if err != nil || current.State != biz.Deleted {
		t.Fatal("replay resurrected", err)
	}
	var records int
	if err := owner.QueryRow(ctx, "SELECT count(*) FROM network_idempotency WHERE tenant_id=$1 AND operation_kind='create_subnet'", tenant).Scan(&records); err != nil || records != 1 {
		t.Fatalf("receipts: %d %v", records, err)
	}
}

func TestSubnetAddressRaceAndParentDeletionRace(t *testing.T) {
	p, _ := database(t)
	n := newNetwork(t, p, time.Minute)
	w := subnetWorker(t, p)
	ctx := context.Background()
	tenant := uuid.NewString()
	v := availableVPC(t, n, w, tenant, "overlap")
	const count = 10
	results := make(chan error, count)
	for i := 0; i < count; i++ {
		go func(i int) {
			_, err := n.CreateSubnet(ctx, biz.CreateSubnet{TenantID: tenant, VPCID: v.ID, Name: "race", CIDR: "10.42.1.0/24", IdempotencyKey: fmt.Sprint(i)})
			results <- err
		}(i)
	}
	accepted := 0
	for i := 0; i < count; i++ {
		err := <-results
		if err == nil {
			accepted++
		} else if biz.ReasonOf(err) != biz.CIDROverlap {
			t.Fatal(err)
		}
	}
	if accepted != 1 {
		t.Fatalf("accepted %d overlapping subnets", accepted)
	}
	second := availableVPC(t, n, w, tenant, "separate")
	if _, err := n.CreateSubnet(ctx, biz.CreateSubnet{TenantID: tenant, VPCID: second.ID, Name: "independent", CIDR: "10.42.1.0/24", IdempotencyKey: "cross-vpc"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		parent := availableVPC(t, n, w, tenant, fmt.Sprintf("delete-race-%d", i))
		start := make(chan struct{})
		createResult := make(chan error, 1)
		deleteResult := make(chan error, 1)
		go func() {
			<-start
			_, err := n.CreateSubnet(ctx, biz.CreateSubnet{TenantID: tenant, VPCID: parent.ID, Name: "child", CIDR: "10.42.2.0/24", IdempotencyKey: parent.ID})
			createResult <- err
		}()
		go func() { <-start; _, err := n.DeleteVPC(ctx, tenant, parent.ID); deleteResult <- err }()
		close(start)
		ce, de := <-createResult, <-deleteResult
		if ce == nil && de == nil {
			t.Fatal("parent delete and child create both accepted")
		}
		if ce == nil {
			if biz.ReasonOf(de) != biz.ResourceInUse {
				t.Fatal(de)
			}
		} else if de == nil {
			if biz.ReasonOf(ce) != biz.ParentNotReady {
				t.Fatal(ce)
			}
		} else {
			t.Fatalf("neither accepted: %v %v", ce, de)
		}
	}
}

func TestSubnetTargetConstraintsAndPagination(t *testing.T) {
	p, owner := database(t)
	n := newNetwork(t, p, time.Minute)
	w := subnetWorker(t, p)
	ctx := context.Background()
	tenant := uuid.NewString()
	v := availableVPC(t, n, w, tenant, "parent")
	var subnets []biz.Subnet
	for i := 0; i < 3; i++ {
		s, err := n.CreateSubnet(ctx, biz.CreateSubnet{TenantID: tenant, VPCID: v.ID, Name: "name", CIDR: fmt.Sprintf("10.42.%d.0/24", i), IdempotencyKey: fmt.Sprint(i)})
		if err != nil {
			t.Fatal(err)
		}
		subnets = append(subnets, s)
	}
	first, err := n.ListSubnets(ctx, biz.ListSubnets{TenantID: tenant, VPCID: v.ID, Limit: 2})
	if err != nil || len(first.Items) != 2 || first.NextCursor == "" {
		t.Fatalf("first: %+v %v", first, err)
	}
	last, err := n.ListSubnets(ctx, biz.ListSubnets{TenantID: tenant, VPCID: v.ID, Limit: 2, Cursor: first.NextCursor})
	if err != nil || len(last.Items) != 1 || last.Items[0].ID != subnets[0].ID {
		t.Fatalf("last: %+v %v", last, err)
	}
	for _, r := range []biz.ListSubnets{{TenantID: uuid.NewString(), VPCID: v.ID}, {TenantID: tenant}, {TenantID: tenant, VPCID: v.ID, Name: "other"}} {
		r.Cursor = first.NextCursor
		if _, err := n.ListSubnets(ctx, r); biz.ReasonOf(err) != biz.InvalidCursor {
			t.Fatal("cursor scope accepted", err)
		}
	}
	empty, err := n.ListSubnets(ctx, biz.ListSubnets{TenantID: uuid.NewString()})
	if err != nil || len(empty.Items) != 0 {
		t.Fatal("tenant list leaked", err)
	}
	for _, statement := range []string{
		`INSERT INTO network_reconciliations(tenant_id,subnet_id,next_run_at) VALUES($1,$2,now())`,
		`INSERT INTO network_provider_bindings(tenant_id,subnet_id,binding_id,cluster_id,namespace,provider_name,resource_kind) VALUES($1,$2,gen_random_uuid(),'other','other','other','subnet')`,
		`INSERT INTO network_resource_history(tenant_id,history_id,subnet_id,event,resource_state,created_at) VALUES($1,gen_random_uuid(),$2,'bad','provisioning',now())`,
	} {
		_, err := owner.Exec(ctx, statement, uuid.NewString(), subnets[0].ID)
		var pgerr *pgconn.PgError
		if !errors.As(err, &pgerr) || pgerr.Code != "23503" {
			t.Fatalf("tenant FK not rejected: %v", err)
		}
	}
	for _, statement := range []string{
		`UPDATE network_operations SET vpc_id=$1 WHERE tenant_id=$2 AND subnet_id=$3`,
		`UPDATE network_provider_bindings SET vpc_id=$1 WHERE tenant_id=$2 AND subnet_id=$3`,
		`UPDATE network_reconciliations SET vpc_id=$1 WHERE tenant_id=$2 AND subnet_id=$3`,
	} {
		_, err := owner.Exec(ctx, statement, v.ID, tenant, subnets[0].ID)
		var pgerr *pgconn.PgError
		if !errors.As(err, &pgerr) || pgerr.Code != "23514" {
			t.Fatalf("two resource targets accepted: %v", err)
		}
	}
	_, err = owner.Exec(ctx, `UPDATE network_subnets SET last_operation_id=$1 WHERE tenant_id=$2 AND subnet_id=$3`, subnets[1].LastOperationID, tenant, subnets[0].ID)
	var pgerr *pgconn.PgError
	if !errors.As(err, &pgerr) || pgerr.Code != "23503" {
		t.Fatal("mismatched operation accepted", err)
	}
}

func TestSubnetUnknownCreateRetainsCIDRAndRejectsOldLeaseWrites(t *testing.T) {
	p, _ := database(t)
	n := newNetwork(t, p, time.Minute)
	w := subnetWorker(t, p)
	ctx := context.Background()
	tenant := uuid.NewString()
	v := availableVPC(t, n, w, tenant, "parent")
	input := biz.CreateSubnet{TenantID: tenant, VPCID: v.ID, Name: "unknown", CIDR: "10.42.1.0/24", IdempotencyKey: "unknown"}
	s, err := n.CreateSubnet(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	first, found, err := p.Claim(ctx, uuid.NewString(), 15*time.Millisecond)
	if err != nil || !found || first.Resource.ID != s.ID {
		t.Fatalf("claim: %+v %v %v", first, found, err)
	}
	if err := p.BeginMutation(ctx, first, "create", ""); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	second, found, err := p.Claim(ctx, uuid.NewString(), time.Second)
	if err != nil || !found || second.Epoch <= first.Epoch || second.PendingAction != "create" {
		t.Fatalf("lease recovery: %+v %v %v", second, found, err)
	}
	if err := p.BeginMutation(ctx, first, "create", ""); !errors.Is(err, biz.ErrLeaseLost) {
		t.Fatal("expired mutation admitted", err)
	}
	if err := p.Finish(ctx, first, biz.Progress{State: biz.Available, OperationState: biz.Succeeded, Observed: true, Identity: "late", NextDelay: time.Second}); !errors.Is(err, biz.ErrLeaseLost) {
		t.Fatal("expired T4 admitted", err)
	}
	if err := p.Finish(ctx, second, biz.Progress{State: biz.Provisioning, OperationState: biz.Blocked, Reason: biz.ProviderUnknown, NextDelay: time.Second}); err != nil {
		t.Fatal(err)
	}
	if _, err := n.DeleteSubnet(ctx, tenant, s.ID); biz.ReasonOf(err) != biz.ResourceBusy {
		t.Fatal("unknown creation released by delete", err)
	}
	other := input
	other.IdempotencyKey = "overlap"
	if _, err := n.CreateSubnet(ctx, other); biz.ReasonOf(err) != biz.CIDROverlap {
		t.Fatal("unknown create released CIDR", err)
	}
	before, err := n.GetSubnet(ctx, tenant, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if _, err := n.GetSubnet(ctx, tenant, s.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := n.ListSubnets(ctx, biz.ListSubnets{TenantID: tenant}); err != nil {
			t.Fatal(err)
		}
		if _, err := n.GetOperation(ctx, tenant, s.LastOperationID); err != nil {
			t.Fatal(err)
		}
	}
	after, err := n.GetSubnet(ctx, tenant, s.ID)
	if err != nil || before.Version != after.Version {
		t.Fatal("query advanced resource", err)
	}
}

func TestSubnetParentProjectionAgeDoesNotGateNewIntentOrReplay(t *testing.T) {
	p, owner := database(t)
	n := newNetwork(t, p, time.Minute)
	w := subnetWorker(t, p)
	tenant := uuid.NewString()
	ctx := context.Background()
	v := availableVPC(t, n, w, tenant, "parent")
	input := biz.CreateSubnet{TenantID: tenant, VPCID: v.ID, Name: "original", CIDR: "10.42.1.0/24", IdempotencyKey: "original"}
	accepted, err := n.CreateSubnet(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = owner.Exec(ctx, `UPDATE network_vpcs SET state='degraded',reason='PROVIDER_NOT_READY',observed_at=clock_timestamp()-interval '1 hour' WHERE tenant_id=$1 AND vpc_id=$2`, tenant, v.ID); err != nil {
		t.Fatal(err)
	}
	current, err := n.GetVPC(ctx, tenant, v.ID)
	if err != nil || current.ObservationStale || current.State != biz.Degraded {
		t.Fatalf("projection age changed its diagnostic: %+v %v", current, err)
	}
	replay, err := n.CreateSubnet(ctx, input)
	if err != nil || replay.ID != accepted.ID {
		t.Fatalf("replay reapplied dynamic parent check: %+v %v", replay, err)
	}
	input.IdempotencyKey = "new"
	input.Name, input.CIDR = "second", "10.42.2.0/24"
	if next, err := n.CreateSubnet(ctx, input); err != nil || next.State != biz.Provisioning {
		t.Fatal("provider projection gated new management intent", next, err)
	}
}
