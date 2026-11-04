package biz

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"
)

type SecurityGroupPortRange struct{ Min, Max int32 }
type SecurityGroupRule struct {
	ID, Direction, Protocol, Action, Description string
	Priority                                     int32
	IsDefault                                    bool
	RemoteAddresses                              []string
	PortRanges                                   []SecurityGroupPortRange
}
type SecurityGroup struct {
	EgressMetadata
	TenantID, ClusterID, Namespace, ProviderName, ProviderUID, BindingID string
	Rules                                                                []SecurityGroupRule
	BoundResourceCount                                                   int64
}
type SecurityGroupBinding struct {
	TenantID, TargetType, TargetID, VPCID, Namespace, State, Reason, LastOperationID string
	SecurityGroupIDs, AppliedSecurityGroupIDs                                        []string
	Version                                                                          int64
	ObservedAt                                                                       *time.Time
}
type SecurityGroupIntent struct {
	TenantID, ID, Action, Name, Description, IdempotencyKey, RuleID string
	ExpectedVersion                                                 int64
	Rules                                                           []SecurityGroupRule
	Rule                                                            SecurityGroupRule
	SecurityGroupIDs                                                []string
}
type SecurityGroupMutation struct {
	SecurityGroup SecurityGroup
	Rule          SecurityGroupRule
	Binding       SecurityGroupBinding
	Operation     Operation
}
type SecurityGroupFilter struct {
	TenantID, Name, State, ID, TargetType string
	Page, PageSize                        uint32
}
type SecurityGroupRepository interface {
	MutateSecurityGroup(context.Context, SecurityGroupIntent, Attribution) (SecurityGroupMutation, error)
	GetSecurityGroup(context.Context, string, string) (SecurityGroup, error)
	ListSecurityGroups(context.Context, SecurityGroupFilter) ([]SecurityGroup, int64, error)
	ListSecurityGroupBindings(context.Context, SecurityGroupFilter) ([]SecurityGroupBinding, int64, error)
	SetEIPSecurityGroups(context.Context, SecurityGroupIntent, Attribution) (SecurityGroupMutation, error)
	GetEIPSecurityGroups(context.Context, string, string) (SecurityGroupBinding, error)
	GetSecurityGroupOperation(context.Context, string, string) (Operation, error)
}
type SecurityGroups struct{ repository SecurityGroupRepository }

func NewSecurityGroups(r SecurityGroupRepository) *SecurityGroups {
	return &SecurityGroups{repository: r}
}
func ValidateSecurityGroupIDs(ids []string) error {
	if len(ids) > 16 {
		return Fail(InvalidArgument, "at most 16 security groups are allowed")
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if !validEgressID(id, "sg") || seen[id] {
			return Fail(InvalidArgument, "invalid or duplicate security group ID")
		}
		seen[id] = true
	}
	return nil
}
func ValidateSecurityGroupRules(rules []SecurityGroupRule) error {
	counts := map[string]int{}
	priorities := map[string]string{}
	for _, r := range rules {
		if r.Direction != "ingress" && r.Direction != "egress" {
			return Fail(InvalidArgument, "rule direction must be ingress or egress")
		}
		if r.IsDefault {
			if r.ID != "sgr_default_ingress_drop" || r.Direction != "ingress" || r.Action != "drop" || r.Protocol != "all" || r.Priority != 1 || len(r.RemoteAddresses) != 0 || len(r.PortRanges) != 0 {
				return Fail(InvalidArgument, "invalid default rule")
			}
		} else if r.Priority < 2 {
			return Fail(InvalidArgument, "priority 1 is reserved for the default ingress drop rule")
		}
		counts[r.Direction]++
		key := fmt.Sprintf("%s/%d", r.Direction, r.Priority)
		if counts[r.Direction] > 100 || r.Priority < 1 || r.Priority > 4000 {
			return Fail(InvalidArgument, "rule limit exceeded or invalid priority")
		}
		if old, exists := priorities[key]; exists && old != r.Action {
			return Fail(InvalidArgument, "opposite actions share one direction and priority")
		}
		priorities[key] = r.Action
		if r.Action != "allow" && r.Action != "drop" {
			return Fail(InvalidArgument, "rule action must be allow or drop")
		}
		if !slices.Contains([]string{"tcp", "udp", "icmp", "ipip", "gre", "all"}, r.Protocol) {
			return Fail(InvalidArgument, "unsupported rule protocol")
		}
		if len(r.RemoteAddresses) > 10 || len(r.PortRanges) > 10 || len(r.Description) > 1024 {
			return Fail(InvalidArgument, "rule address, port or description limit exceeded")
		}
		seenAddresses := map[string]bool{}
		for _, a := range r.RemoteAddresses {
			if seenAddresses[a] {
				return Fail(InvalidArgument, "duplicate remote address")
			}
			seenAddresses[a] = true
			p, e := netip.ParsePrefix(a)
			if e != nil || !p.Addr().Is4() || p != p.Masked() {
				return Fail(InvalidArgument, "remote address must be canonical IPv4 CIDR")
			}
		}
		for index, p := range r.PortRanges {
			for _, other := range r.PortRanges[:index] {
				if p.Min <= other.Max && other.Min <= p.Max {
					return Fail(InvalidArgument, "port ranges must not overlap")
				}
			}
			if (r.Protocol != "tcp" && r.Protocol != "udp") || p.Min < 1 || p.Max > 65535 || p.Min > p.Max {
				return Fail(InvalidArgument, "invalid destination port range")
			}
		}
	}
	return nil
}
func (s *SecurityGroups) Mutate(ctx context.Context, i SecurityGroupIntent) (SecurityGroupMutation, error) {
	tenant, a, e := (ContextEgressAuthorization{}).Tenant(ctx, i.TenantID)
	if e != nil {
		return SecurityGroupMutation{}, e
	}
	i.TenantID = tenant
	if i.Action != "create" && !validEgressID(i.ID, "sg") {
		return SecurityGroupMutation{}, Fail(ResourceNotFound, "security group not found")
	}
	if i.Action == "create" || i.Action == "update" {
		i.Name = strings.TrimSpace(i.Name)
		if _, e = normalizeEgressName(i.Name, i.Description, i.IdempotencyKey); e != nil {
			return SecurityGroupMutation{}, e
		}
	}
	if i.Action != "delete" && i.Action != "delete_rule" {
		if e = validateSecurityGroupKey(i.IdempotencyKey); e != nil {
			return SecurityGroupMutation{}, e
		}
	}
	if i.Action == "update" || i.Action == "create_rule" || i.Action == "update_rule" {
		if i.ExpectedVersion < 1 {
			return SecurityGroupMutation{}, Fail(InvalidArgument, "expected_version is required")
		}
	}
	for _, r := range append(slices.Clone(i.Rules), i.Rule) {
		if r.IsDefault || r.ID == "sgr_default_ingress_drop" {
			return SecurityGroupMutation{}, Fail(InvalidArgument, "default ingress rule is managed by Resource")
		}
	}
	if i.Action == "create" {
		e = ValidateSecurityGroupRules(i.Rules)
	} else if i.Action == "create_rule" || i.Action == "update_rule" {
		e = ValidateSecurityGroupRules([]SecurityGroupRule{i.Rule})
	}
	if e != nil {
		return SecurityGroupMutation{}, e
	}
	return s.repository.MutateSecurityGroup(ctx, i, a)
}
func (s *SecurityGroups) Get(ctx context.Context, tenant, id string) (SecurityGroup, error) {
	tenant, _, e := (ContextEgressAuthorization{}).Tenant(ctx, tenant)
	if e != nil {
		return SecurityGroup{}, e
	}
	if !validEgressID(id, "sg") {
		return SecurityGroup{}, Fail(ResourceNotFound, "security group not found")
	}
	return s.repository.GetSecurityGroup(ctx, tenant, id)
}
func (s *SecurityGroups) List(ctx context.Context, f SecurityGroupFilter) ([]SecurityGroup, int64, error) {
	tenant, _, e := (ContextEgressAuthorization{}).Tenant(ctx, f.TenantID)
	if e != nil {
		return nil, 0, e
	}
	f.TenantID = tenant
	if e = securityGroupPage(&f); e != nil {
		return nil, 0, e
	}
	return s.repository.ListSecurityGroups(ctx, f)
}
func (s *SecurityGroups) Bindings(ctx context.Context, f SecurityGroupFilter) ([]SecurityGroupBinding, int64, error) {
	tenant, _, e := (ContextEgressAuthorization{}).Tenant(ctx, f.TenantID)
	if e != nil {
		return nil, 0, e
	}
	f.TenantID = tenant
	if e = securityGroupPage(&f); e != nil {
		return nil, 0, e
	}
	if _, e = s.Get(ctx, tenant, f.ID); e != nil {
		return nil, 0, e
	}
	return s.repository.ListSecurityGroupBindings(ctx, f)
}
func securityGroupPage(f *SecurityGroupFilter) error {
	if f.Page == 0 {
		f.Page = 1
	}
	if f.PageSize == 0 {
		f.PageSize = 20
	}
	if f.PageSize > 100 || f.Page > 1000000 {
		return Fail(InvalidArgument, "invalid page or page_size")
	}
	return nil
}
func (s *SecurityGroups) SetEIP(ctx context.Context, i SecurityGroupIntent) (SecurityGroupMutation, error) {
	tenant, a, e := (ContextEgressAuthorization{}).Tenant(ctx, i.TenantID)
	if e != nil {
		return SecurityGroupMutation{}, e
	}
	i.TenantID = tenant
	i.Action = "set_eip_security_groups"
	if !validEgressID(i.ID, "eip") {
		return SecurityGroupMutation{}, Fail(ResourceNotFound, "EIP not found")
	}
	if e = ValidateSecurityGroupIDs(i.SecurityGroupIDs); e != nil {
		return SecurityGroupMutation{}, e
	}
	if e = validateSecurityGroupKey(i.IdempotencyKey); e != nil {
		return SecurityGroupMutation{}, e
	}
	if i.ExpectedVersion < 1 {
		return SecurityGroupMutation{}, Fail(InvalidArgument, "expected_version is required")
	}
	return s.repository.SetEIPSecurityGroups(ctx, i, a)
}
func (s *SecurityGroups) GetEIP(ctx context.Context, tenant, id string) (SecurityGroupBinding, error) {
	tenant, _, e := (ContextEgressAuthorization{}).Tenant(ctx, tenant)
	if e != nil {
		return SecurityGroupBinding{}, e
	}
	return s.repository.GetEIPSecurityGroups(ctx, tenant, id)
}
func (s *SecurityGroups) Operation(ctx context.Context, tenant, id string) (Operation, error) {
	if _, e := ParseTenant(id); e != nil {
		return Operation{}, Fail(ResourceNotFound, "security group operation not found")
	}
	tenant, _, e := (ContextEgressAuthorization{}).Tenant(ctx, tenant)
	if e != nil {
		return Operation{}, e
	}
	return s.repository.GetSecurityGroupOperation(ctx, tenant, id)
}

// SecurityGroupWorker uses the same bounded Stepper scheduler as other Network
// slices. PostgreSQL owns pending intent; Get/List never execute provider writes.
type SecurityGroupWork struct {
	Group                                 SecurityGroup
	Binding                               SecurityGroupBinding
	Operation                             Operation
	TenantID, TargetType, TargetID, Owner string
	Version, Epoch                        int64
	ClaimedGeneration                     int64
}
type SecurityGroupResult struct {
	ProviderUID, State, Reason string
	AppliedIDs                 []string
	Complete                   bool
	Observed                   bool
	ObservationError           Reason
	OperationReason            Reason
	ProviderError              error `json:"-"`
	Backoff                    bool
	NextDelay                  time.Duration
}
type SecurityGroupWorkRepository interface {
	ClaimSecurityGroup(context.Context, string, time.Duration) (SecurityGroupWork, bool, error)
	FinishSecurityGroup(context.Context, SecurityGroupWork, SecurityGroupResult, error) error
}
type SecurityGroupProvider interface {
	ApplySecurityGroup(context.Context, SecurityGroupWork) (SecurityGroupResult, error)
}
type SecurityGroupWorker struct {
	repository SecurityGroupWorkRepository
	provider   SecurityGroupProvider
	owner      string
	policy     WorkerPolicy
}

func NewSecurityGroupWorker(r SecurityGroupWorkRepository, p SecurityGroupProvider, owner string, policy WorkerPolicy) *SecurityGroupWorker {
	return &SecurityGroupWorker{r, p, owner, policy}
}
func (w *SecurityGroupWorker) Step(ctx context.Context) (bool, error) {
	step, cancelStep := context.WithTimeout(ctx, w.policy.Lease)
	defer cancelStep()
	work, found, err := w.repository.ClaimSecurityGroup(step, w.owner, w.policy.Lease)
	if err != nil || !found {
		return found, err
	}
	call, cancel := context.WithTimeout(step, w.policy.RequestTimeout)
	result, applyErr := w.provider.ApplySecurityGroup(call, work)
	cancel()
	result.NextDelay = w.policy.ObserveEvery
	if applyErr != nil {
		result.ProviderError = applyErr
		if result.OperationReason == "" {
			result.OperationReason = providerReason(applyErr)
		}
		result.Backoff = true
		result.NextDelay = w.policy.RetryMin
		if providerKind(applyErr) == ProviderReject && result.ObservationError == "" && work.Group.State != Deleting {
			result.Backoff = false
		}
	}
	if applyErr == nil && !result.Complete && result.OperationReason == "" {
		result.OperationReason = Reason(result.Reason)
		if result.OperationReason == "" {
			result.OperationReason = ProviderNotReady
		}
	}
	finish, cancelFinish := context.WithTimeout(context.WithoutCancel(step), w.policy.RequestTimeout)
	defer cancelFinish()
	err = w.repository.FinishSecurityGroup(finish, work, result, applyErr)
	if errors.Is(err, ErrLeaseLost) {
		return true, nil
	}
	return true, err
}

func validateSecurityGroupKey(key string) error {
	_, e := normalizeEgressName("security-group", "", key)
	return e
}

func DefaultSecurityGroupRule() SecurityGroupRule {
	return SecurityGroupRule{ID: "sgr_default_ingress_drop", Direction: "ingress", Protocol: "all", Action: "drop", Priority: 1, IsDefault: true}
}
