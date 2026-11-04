package biz

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
)

type lbRepositoryTest struct {
	LoadBalancerRepository
	accepted []LoadBalancerIntent
	rows     []LoadBalancer
	op       Operation
}

func (r *lbRepositoryTest) AcceptLoadBalancer(_ context.Context, i LoadBalancerIntent, _ Attribution) (LoadBalancerResult, error) {
	r.accepted = append(r.accepted, i)
	return LoadBalancerResult{}, nil
}
func (r *lbRepositoryTest) GetLoadBalancer(_ context.Context, _, _ string) (LoadBalancer, error) {
	return r.rows[0], nil
}
func (r *lbRepositoryTest) ListLoadBalancers(_ context.Context, _ string, _ LoadBalancerFilter) ([]LoadBalancer, int64, error) {
	return slices.Clone(r.rows), int64(len(r.rows)), nil
}
func (r *lbRepositoryTest) GetLoadBalancerOperation(_ context.Context, _, _ string) (Operation, error) {
	return r.op, nil
}
func lbTestSetup(t *testing.T) (*LoadBalancers, *lbRepositoryTest, context.Context) {
	t.Helper()
	r := &lbRepositoryTest{}
	l, err := NewLoadBalancers(r, ContextEgressAuthorization{}, []byte(strings.Repeat("c", 32)))
	if err != nil {
		t.Fatal(err)
	}
	return l, r, WithEgressCaller(context.Background(), EgressCaller{TenantID: "11111111-1111-4111-8111-111111111111"})
}
func lbTestRequest() CreateLoadBalancer {
	port := uint32(80)
	return CreateLoadBalancer{VPCID: "vpc_" + strings.Repeat("1", 32), SubnetID: "subnet_" + strings.Repeat("2", 32), Exposure: "private", PrivateIP: "10.2.1.100", IdempotencyKey: "create-lb", LoadBalancerMutableInput: LoadBalancerMutableInput{Health: LoadBalancerHealthInput{Port: &port}, Name: " lb ", Backends: []LoadBalancerBackendInput{{SubnetID: "subnet_" + strings.Repeat("2", 32), Address: "10.2.1.2", Port: 80}}}}
}
func TestLBTrustedContextDefaultsAndExplicitZero(t *testing.T) {
	l, r, ctx := lbTestSetup(t)
	req := lbTestRequest()
	if _, err := l.Create(context.Background(), req); ReasonOf(err) != PermissionDenied {
		t.Fatalf("default authorization: %v", err)
	}
	req.TenantID = "22222222-2222-4222-8222-222222222222"
	if _, err := l.Create(ctx, req); ReasonOf(err) != PermissionDenied {
		t.Fatalf("target is not authority: %v", err)
	}
	if len(r.accepted) != 0 {
		t.Fatal("unauthorized input reached persistence")
	}
	req.TenantID = ""
	if _, err := l.Create(ctx, req); err != nil {
		t.Fatal(err)
	}
	got := r.accepted[0]
	if got.Name != "lb" || got.ListenerPort != 8080 || got.Flavor != "small" || got.Backends[0].Weight != 1 || got.Health != (LoadBalancerHealth{IntervalSeconds: 5, TimeoutSeconds: 3, UnhealthyThreshold: 3, HealthyThreshold: 1, Port: 80}) {
		t.Fatalf("defaults: %+v", got)
	}
	zero := uint32(0)
	req.Backends[0].Weight = &zero
	if _, err := l.Create(ctx, req); err != nil {
		t.Fatal(err)
	}
	if len(r.accepted[1].Backends) != 1 || r.accepted[1].Backends[0].Weight != 0 {
		t.Fatal("zero-weight member was lost or defaulted")
	}
	if got.Fingerprint() == r.accepted[1].Fingerprint() {
		t.Fatal("weight change must change accepted intent")
	}
}
func TestLBNormalizationRejectsAmbiguousOrUnsupportedInputs(t *testing.T) {
	zero := uint32(0)
	for name, change := range map[string]func(*CreateLoadBalancer){
		"private_with_eip":         func(r *CreateLoadBalancer) { r.PublicEIPID = "eip_" + strings.Repeat("3", 32) },
		"public_with_vip":          func(r *CreateLoadBalancer) { r.Exposure = "public" },
		"dual_without_eip":         func(r *CreateLoadBalancer) { r.Exposure = "public_private" },
		"duplicate":                func(r *CreateLoadBalancer) { r.Backends = append(r.Backends, r.Backends[0]) },
		"unsupported_flavor":       func(r *CreateLoadBalancer) { r.Flavor = "dynamic" },
		"unsupported_listener":     func(r *CreateLoadBalancer) { r.ListenerProtocol = "TCP" },
		"zero_listener_port":       func(r *CreateLoadBalancer) { r.ListenerPort = &zero },
		"invalid_backend_port":     func(r *CreateLoadBalancer) { r.Backends[0].Port = 65536 },
		"invalid_backend_ip":       func(r *CreateLoadBalancer) { r.Backends[0].Address = "127.0.0.1" },
		"injected_member_identity": func(r *CreateLoadBalancer) { r.Backends[0].ID = "11111111-1111-4111-8111-111111111111" },
		"zero_health_threshold":    func(r *CreateLoadBalancer) { r.Health.HealthyThreshold = &zero },
		"zero_unhealthy_threshold": func(r *CreateLoadBalancer) { r.Health.UnhealthyThreshold = &zero },
		"zero_health_interval":     func(r *CreateLoadBalancer) { r.Health.IntervalSeconds = &zero },
		"zero_health_timeout":      func(r *CreateLoadBalancer) { r.Health.TimeoutSeconds = &zero },
	} {
		t.Run(name, func(t *testing.T) {
			l, repo, ctx := lbTestSetup(t)
			r := lbTestRequest()
			change(&r)
			if _, err := l.Create(ctx, r); err == nil {
				t.Fatal("accepted invalid input")
			}
			if len(repo.accepted) != 0 {
				t.Fatal("invalid input reached repository")
			}
		})
	}
}
func TestLBHealthTimingParametersAreIndependent(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout uint32
	}{
		{"timeout-equals-interval", 3},
		{"timeout-exceeds-interval", 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, repo, ctx := lbTestSetup(t)
			r := lbTestRequest()
			interval := uint32(3)
			r.Health.IntervalSeconds, r.Health.TimeoutSeconds = &interval, &tc.timeout
			if _, err := l.Create(ctx, r); err != nil {
				t.Fatal("independent positive health timing rejected on create", err)
			}
			if _, err := l.Update(ctx, UpdateLoadBalancer{ID: "lb_11111111111111111111111111111111", ExpectedVersion: 1, IdempotencyKey: "update-timing", LoadBalancerMutableInput: r.LoadBalancerMutableInput}); err != nil {
				t.Fatal("independent positive health timing rejected on update", err)
			}
			if len(repo.accepted) != 2 {
				t.Fatal("health timing create or update did not reach persistence")
			}
			for _, intent := range repo.accepted {
				if intent.Health.IntervalSeconds != interval || intent.Health.TimeoutSeconds != tc.timeout {
					t.Fatalf("positive health timing was changed in persisted intent: %+v", intent.Health)
				}
			}
		})
	}
}
func TestLBBackendSetFingerprintIgnoresOrderButPreservesMemberIdentity(t *testing.T) {
	l, repo, ctx := lbTestSetup(t)
	r := lbTestRequest()
	r.Backends = append(r.Backends, LoadBalancerBackendInput{SubnetID: r.SubnetID, Address: "10.2.1.3", Port: 80})
	if _, err := l.Create(ctx, r); err != nil {
		t.Fatal(err)
	}
	slices.Reverse(r.Backends)
	if _, err := l.Create(ctx, r); err != nil {
		t.Fatal(err)
	}
	if repo.accepted[0].Fingerprint() != repo.accepted[1].Fingerprint() {
		t.Fatal("set reorder changed intent")
	}
	u := UpdateLoadBalancer{ID: "lb_" + strings.Repeat("4", 32), ExpectedVersion: 7, IdempotencyKey: "update", LoadBalancerMutableInput: r.LoadBalancerMutableInput}
	u.Backends[0].ID = "33333333-3333-4333-8333-333333333333"
	if _, err := l.Update(ctx, u); err != nil {
		t.Fatal(err)
	}
	if repo.accepted[2].ExpectedVersion != 7 || repo.accepted[2].Kind != "update_load_balancer" {
		t.Fatal("version or operation kind lost")
	}
}
func TestLBQueryPreservesReportedStateAndCursorScope(t *testing.T) {
	l, repo, ctx := lbTestSetup(t)
	old := time.Unix(900, 0)
	fresh := time.Unix(990, 0)
	repo.rows = []LoadBalancer{{EgressMetadata: EgressMetadata{ID: "lb_" + strings.Repeat("4", 32), ObservedAt: &fresh, CreatedAt: fresh}, ConfigurationState: "configured", DesiredVersion: 2, AppliedVersion: 1, Backends: []LoadBalancerBackend{{State: "available", ObservedAt: &old}}}, {EgressMetadata: EgressMetadata{ID: "lb_" + strings.Repeat("5", 32), CreatedAt: old}}}
	v, err := l.Get(ctx, "", repo.rows[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if v.ObservationStale || v.AppliedVersion != 1 || v.DesiredVersion != 2 || v.Backends[0].State != "available" {
		t.Fatalf("query mixed evidence/version: %+v", v)
	}
	q := ListLoadBalancers{ListVPCs: ListVPCs{Limit: 1}, Exposure: "private"}
	rows, cursor, _, err := l.List(ctx, q)
	if err != nil || len(rows) != 1 || cursor == "" {
		t.Fatalf("page: %v %q %v", rows, cursor, err)
	}
	q.Cursor = cursor
	q.Exposure = "public"
	if _, _, _, err = l.List(ctx, q); ReasonOf(err) != InvalidCursor {
		t.Fatalf("cross-exposure cursor: %v", err)
	}
	q.Exposure = "private"
	q.SubnetID = "subnet_" + strings.Repeat("2", 32)
	if _, _, _, err = l.List(ctx, q); ReasonOf(err) != InvalidCursor {
		t.Fatalf("cross-subnet cursor: %v", err)
	}
	if repo.rows[0].Backends[0].State != "available" {
		t.Fatal("GET mutated repository snapshot")
	}
}
func TestLBOperationQueryDoesNotExposeOtherKindsOrTenants(t *testing.T) {
	l, repo, ctx := lbTestSetup(t)
	id := "33333333-3333-4333-8333-333333333333"
	for _, op := range []Operation{{TenantID: "11111111-1111-4111-8111-111111111111", ResourceType: "vpc"}, {TenantID: "22222222-2222-4222-8222-222222222222", ResourceType: "load_balancer"}} {
		repo.op = op
		if _, err := l.GetOperation(ctx, "", id); ReasonOf(err) != ResourceNotFound {
			t.Fatalf("operation leaked: %v", err)
		}
	}
}
