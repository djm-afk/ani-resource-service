package biz

import (
	"context"
	"fmt"
	"github.com/google/uuid"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	LoadBalancerNotReady     Reason = "LOAD_BALANCER_NOT_READY"
	BackendIdentityMismatch  Reason = "BACKEND_IDENTITY_MISMATCH"
	VIPInUse                 Reason = "VIP_IN_USE"
	WaitingForBackends       Reason = "WAITING_FOR_BACKENDS"
	WaitingForRedirectTarget Reason = "WAITING_FOR_REDIRECT_TARGET"
)

type LoadBalancerHealth struct {
	Protocol, Path, Hostname                                              string `json:",omitempty"`
	IntervalSeconds, TimeoutSeconds, UnhealthyThreshold, HealthyThreshold uint32
	// Zero is reserved for persisted legacy configurations using endpoint ports.
	Port uint32 `json:"Port,omitempty"`
}
type LoadBalancerHealthInput struct {
	Path, Hostname                                                        string
	Protocol                                                              string
	IntervalSeconds, TimeoutSeconds, UnhealthyThreshold, HealthyThreshold *uint32
	Port                                                                  *uint32
}
type LoadBalancerListener struct {
	Parameters       LoadBalancerListenerParameters `json:"Parameters,omitzero"`
	ID               string
	Port             uint32
	Name             string                `json:"Name,omitempty"`
	Protocol         string                `json:"Protocol,omitempty"`
	Backends         []LoadBalancerBackend `json:"Backends,omitempty"`
	Health           LoadBalancerHealth    `json:"Health,omitzero"`
	DeploymentState  string                `json:",omitempty"`
	DeploymentReason Reason                `json:",omitempty"`
}
type LoadBalancerBackendInput struct {
	ID, SubnetID, Address string
	Port                  uint32
	Weight                *uint32
}
type LoadBalancerBackend struct {
	ID, SubnetID, Address string
	Port, Weight          uint32
	AttachmentID, State   string
	Reason                Reason
	ObservedAt            *time.Time
	ObservationStale      bool
}
type LoadBalancer struct {
	EgressMetadata
	TenantID, VPCID, SubnetID, Exposure, Flavor, PublicEIPID, PrivateIP, PublicAddress string
	Listener                                                                           LoadBalancerListener
	Listeners                                                                          []LoadBalancerListener `json:"Listeners,omitempty"`
	Backends                                                                           []LoadBalancerBackend
	Health                                                                             LoadBalancerHealth
	DesiredVersion, AppliedVersion                                                     int64
	ProviderTargetVersion                                                              int64 `json:",omitempty"`
	ConfigurationState                                                                 string
}
type LoadBalancerResult struct {
	LoadBalancer LoadBalancer
	Operation    Operation
}
type LoadBalancerMutableInput struct {
	Name, Description string
	Backends          []LoadBalancerBackendInput
	Health            LoadBalancerHealthInput
	Listeners         []LoadBalancerListenerInput
	UpdateMask        []string
}
type CreateLoadBalancer struct {
	LoadBalancerMutableInput
	TenantID, VPCID, SubnetID, Exposure, Flavor, PublicEIPID, PrivateIP, IdempotencyKey string
	ListenerProtocol                                                                    string
	ListenerPort                                                                        *uint32
}
type UpdateLoadBalancer struct {
	LoadBalancerMutableInput
	TenantID, ID, IdempotencyKey string
	ExpectedVersion              int64
}

// LoadBalancerIntent contains only normalized product input. Provider identities
// and configuration/member IDs are allocated once by transactional acceptance.
type LoadBalancerIntent struct {
	TenantID, ID, Kind, IdempotencyKey                                           string
	ExpectedVersion                                                              int64
	Name, Description, VPCID, SubnetID, Exposure, Flavor, PublicEIPID, PrivateIP string
	ListenerPort                                                                 uint32
	Backends                                                                     []LoadBalancerBackend
	Health                                                                       LoadBalancerHealth
	Listeners                                                                    []LoadBalancerListener `json:"Listeners,omitempty"`
	UpdateMask                                                                   []string               `json:"UpdateMask,omitempty"`
}

func (i LoadBalancerIntent) Fingerprint() string {
	i.IdempotencyKey = ""
	return egressFingerprint(i)
}

type LoadBalancerFilter struct {
	VPCFilter
	VPCID, SubnetID, Exposure string
}
type ListLoadBalancers struct {
	ListVPCs
	VPCID, SubnetID, Exposure string
}
type LoadBalancerRepository interface {
	AcceptLoadBalancer(context.Context, LoadBalancerIntent, Attribution) (LoadBalancerResult, error)
	GetLoadBalancer(context.Context, string, string) (LoadBalancer, error)
	ListLoadBalancers(context.Context, string, LoadBalancerFilter) ([]LoadBalancer, int64, error)
	DeleteLoadBalancer(context.Context, string, string, Attribution) (LoadBalancerResult, error)
	GetLoadBalancerOperation(context.Context, string, string) (Operation, error)
}
type LoadBalancers struct {
	repository    LoadBalancerRepository
	authorization EgressAuthorization
	cursor        *Network
}

func NewLoadBalancers(r LoadBalancerRepository, a EgressAuthorization, key []byte) (*LoadBalancers, error) {
	if r == nil || a == nil || len(key) < 32 {
		return nil, fmt.Errorf("load balancer repository, authorization and cursor key are required")
	}
	return &LoadBalancers{repository: r, authorization: a, cursor: &Network{cursorKey: slices.Clone(key)}}, nil
}
func lbDefault(v *uint32, fallback uint32) uint32 {
	if v == nil {
		return fallback
	}
	return *v
}
func normalizeLBMutable(i *LoadBalancerIntent, r LoadBalancerMutableInput, creating, redirecting bool) error {
	var err error
	i.Name, err = normalizeEgressName(r.Name, r.Description, i.IdempotencyKey)
	if err != nil {
		return err
	}
	i.Description = r.Description
	h := r.Health
	if h.Protocol != "" && h.Protocol != "TCP" && h.Protocol != "HTTP" {
		return Fail(InvalidArgument, "health check protocol must be TCP or HTTP")
	}
	if h.Port != nil && *h.Port > 65535 {
		return Fail(InvalidArgument, "health_check.port must be within 0..65535")
	}
	i.Health = LoadBalancerHealth{IntervalSeconds: lbDefault(h.IntervalSeconds, 5), TimeoutSeconds: lbDefault(h.TimeoutSeconds, 3), UnhealthyThreshold: lbDefault(h.UnhealthyThreshold, 3), HealthyThreshold: lbDefault(h.HealthyThreshold, 1), Port: lbDefault(h.Port, 0)}
	if h.Protocol == "HTTP" {
		if len(h.Path) == 0 || len(h.Path) > 1024 || !strings.HasPrefix(h.Path, "/") || strings.ContainsAny(h.Path, "\r\n") || len(h.Hostname) > 253 || (h.Hostname != "" && !lbListenerName.MatchString(h.Hostname)) {
			return Fail(InvalidArgument, "HTTP health check requires a path and an optional DNS hostname")
		}
		i.Health.Protocol, i.Health.Path, i.Health.Hostname = "HTTP", h.Path, h.Hostname
	} else if h.Path != "" || h.Hostname != "" {
		return Fail(InvalidArgument, "TCP health check does not accept HTTP parameters")
	}
	if i.Health.TimeoutSeconds == 0 || i.Health.IntervalSeconds == 0 || i.Health.UnhealthyThreshold == 0 || i.Health.HealthyThreshold == 0 {
		return Fail(InvalidArgument, "positive health parameters are required")
	}
	ids, endpoints := map[string]bool{}, map[string]bool{}
	for _, b := range r.Backends {
		if !validSubnetID(b.SubnetID) {
			return Fail(ResourceNotFound, "backend subnet not found")
		}
		ip, err := netip.ParseAddr(b.Address)
		if err != nil || !ip.Is4() || !ip.IsPrivate() || ip.String() != b.Address || b.Port < 1 || b.Port > 65535 {
			return Fail(InvalidArgument, "backend requires a canonical private IPv4 address and a valid port")
		}
		if b.ID != "" {
			id, err := uuid.Parse(b.ID)
			if creating || err != nil || id == uuid.Nil || id.String() != b.ID || ids[b.ID] {
				return Fail(InvalidArgument, "invalid or duplicate backend member identity")
			}
			ids[b.ID] = true
		}
		endpoint := b.SubnetID + "/" + b.Address + "/" + strconv.FormatUint(uint64(b.Port), 10)
		if endpoints[endpoint] {
			return Fail(InvalidArgument, "duplicate backend endpoint")
		}
		endpoints[endpoint] = true
		// Gateway API HTTPBackendRef weight is bounded by its installed CRD.
		weight := lbDefault(b.Weight, 1)
		if weight > 1000000 {
			return Fail(InvalidArgument, "backend weight must be within 0..1000000")
		}
		i.Backends = append(i.Backends, LoadBalancerBackend{ID: b.ID, SubnetID: b.SubnetID, Address: b.Address, Port: b.Port, Weight: weight})
	}
	// Backend input is a set. Ordering cannot change the idempotent intent.
	slices.SortFunc(i.Backends, func(a, b LoadBalancerBackend) int {
		return compareLBEndpoint(a, b)
	})
	return nil
}
func compareLBEndpoint(a, b LoadBalancerBackend) int {
	for _, p := range [][2]string{{a.SubnetID, b.SubnetID}, {a.Address, b.Address}, {a.ID, b.ID}} {
		if p[0] < p[1] {
			return -1
		}
		if p[0] > p[1] {
			return 1
		}
	}
	if a.Port < b.Port {
		return -1
	}
	if a.Port > b.Port {
		return 1
	}
	return 0
}
func (l *LoadBalancers) Create(ctx context.Context, r CreateLoadBalancer) (LoadBalancerResult, error) {
	tenant, a, err := l.authorization.Tenant(ctx, r.TenantID)
	if err != nil {
		return LoadBalancerResult{}, err
	}
	i := LoadBalancerIntent{TenantID: tenant, Kind: "create_load_balancer", IdempotencyKey: r.IdempotencyKey, VPCID: r.VPCID, SubnetID: r.SubnetID, Exposure: r.Exposure, Flavor: r.Flavor, PublicEIPID: r.PublicEIPID, PrivateIP: r.PrivateIP, ListenerPort: lbDefault(r.ListenerPort, 8080)}
	if !validVPCID(i.VPCID) || !validSubnetID(i.SubnetID) {
		return LoadBalancerResult{}, Fail(ResourceNotFound, "network parent not found")
	}
	if i.Flavor == "" {
		i.Flavor = "small"
	}
	if _, ok := FindLoadBalancerFlavor(i.Flavor); !ok {
		return LoadBalancerResult{}, Fail(InvalidArgument, "flavor must be small, medium or large")
	}
	if (r.ListenerProtocol != "" && r.ListenerProtocol != "HTTP") || i.ListenerPort == 0 || i.ListenerPort > 65535 {
		return LoadBalancerResult{}, Fail(InvalidArgument, "an HTTP listener on a valid port is required")
	}
	if i.Exposure != "private" && i.Exposure != "public" && i.Exposure != "public_private" {
		return LoadBalancerResult{}, Fail(InvalidArgument, "invalid load balancer exposure")
	}
	if (i.Exposure == "private" && i.PublicEIPID != "") || (i.Exposure == "public" && i.PrivateIP != "") {
		return LoadBalancerResult{}, Fail(InvalidArgument, "entry addresses do not match exposure")
	}
	if i.Exposure != "private" && !validEgressID(i.PublicEIPID, "eip") {
		return LoadBalancerResult{}, Fail(ResourceNotFound, "Public EIP not found")
	}
	if i.Exposure != "public" {
		ip, err := netip.ParseAddr(i.PrivateIP)
		if err != nil || !ip.Is4() || !ip.IsPrivate() || ip.String() != i.PrivateIP {
			return LoadBalancerResult{}, Fail(InvalidArgument, "a canonical private IPv4 VIP is required")
		}
	}
	if r.Listeners != nil && (r.ListenerProtocol != "" || r.ListenerPort != nil) {
		return LoadBalancerResult{}, Fail(InvalidArgument, "listeners cannot be combined with the legacy listener")
	}
	if r.Listeners != nil {
		i.ListenerPort = 0
	}
	if err = normalizeLBInput(&i, r.LoadBalancerMutableInput, true); err != nil {
		return LoadBalancerResult{}, err
	}
	return l.repository.AcceptLoadBalancer(ctx, i, a)
}
func (l *LoadBalancers) Update(ctx context.Context, r UpdateLoadBalancer) (LoadBalancerResult, error) {
	tenant, a, err := l.authorization.Tenant(ctx, r.TenantID)
	if err != nil {
		return LoadBalancerResult{}, err
	}
	if !validEgressID(r.ID, "lb") {
		return LoadBalancerResult{}, Fail(ResourceNotFound, "load balancer not found")
	}
	if r.ExpectedVersion < 1 {
		return LoadBalancerResult{}, Fail(InvalidArgument, "expected_version is required")
	}
	i := LoadBalancerIntent{TenantID: tenant, ID: r.ID, Kind: "update_load_balancer", IdempotencyKey: r.IdempotencyKey, ExpectedVersion: r.ExpectedVersion}
	if err = normalizeLBInput(&i, r.LoadBalancerMutableInput, false); err != nil {
		return LoadBalancerResult{}, err
	}
	return l.repository.AcceptLoadBalancer(ctx, i, a)
}
func (l *LoadBalancers) observation(v LoadBalancer) LoadBalancer { return v }
func (l *LoadBalancers) Get(ctx context.Context, tenant, id string) (LoadBalancer, error) {
	tenant, _, err := l.authorization.Tenant(ctx, tenant)
	if err != nil {
		return LoadBalancer{}, err
	}
	if !validEgressID(id, "lb") {
		return LoadBalancer{}, Fail(ResourceNotFound, "load balancer not found")
	}
	v, err := l.repository.GetLoadBalancer(ctx, tenant, id)
	return l.observation(v), err
}
func (l *LoadBalancers) Delete(ctx context.Context, tenant, id string) (LoadBalancerResult, error) {
	tenant, a, err := l.authorization.Tenant(ctx, tenant)
	if err != nil {
		return LoadBalancerResult{}, err
	}
	if !validEgressID(id, "lb") {
		return LoadBalancerResult{}, Fail(ResourceNotFound, "load balancer not found")
	}
	return l.repository.DeleteLoadBalancer(ctx, tenant, id, a)
}
func (l *LoadBalancers) GetOperation(ctx context.Context, tenant, id string) (Operation, error) {
	tenant, _, err := l.authorization.Tenant(ctx, tenant)
	if err != nil {
		return Operation{}, err
	}
	parsed, err := uuid.Parse(id)
	if err != nil || parsed == uuid.Nil || len(id) != 36 {
		return Operation{}, Fail(ResourceNotFound, "operation not found")
	}
	op, err := l.repository.GetLoadBalancerOperation(ctx, tenant, parsed.String())
	if err == nil && (op.TenantID != tenant || op.ResourceType != "load_balancer") {
		return Operation{}, Fail(ResourceNotFound, "operation not found")
	}
	return op, err
}
func (l *LoadBalancers) List(ctx context.Context, r ListLoadBalancers) ([]LoadBalancer, string, int64, error) {
	tenant, _, err := l.authorization.Tenant(ctx, r.TenantID)
	if err != nil {
		return nil, "", 0, err
	}
	f, err := normalizeList(r.Name, r.State, r.Limit)
	if err != nil {
		return nil, "", 0, err
	}
	if (r.VPCID != "" && !validVPCID(r.VPCID)) || (r.SubnetID != "" && !validSubnetID(r.SubnetID)) || (r.Exposure != "" && r.Exposure != "private" && r.Exposure != "public" && r.Exposure != "public_private") {
		return nil, "", 0, Fail(InvalidArgument, "invalid load balancer filter")
	}
	kind := "load_balancer:" + egressFingerprint([]string{r.VPCID, r.SubnetID, r.Exposure})
	if r.Cursor != "" {
		c, err := l.cursor.decodeCursor(r.Cursor)
		if err != nil || c.Version != 1 || c.Kind != kind || c.TenantID != tenant || c.Name != f.Name || c.State != f.State || !validEgressID(c.ID, "lb") || c.CreatedAt.IsZero() {
			return nil, "", 0, Fail(InvalidCursor, "cursor does not match query")
		}
		f.AfterID, f.AfterCreatedAt = c.ID, c.CreatedAt
	}
	rows, total, err := l.repository.ListLoadBalancers(ctx, tenant, LoadBalancerFilter{VPCFilter: f, VPCID: r.VPCID, SubnetID: r.SubnetID, Exposure: r.Exposure})
	if err != nil {
		return nil, "", 0, err
	}
	next := ""
	if len(rows) == int(f.Limit) {
		rows = rows[:len(rows)-1]
		v := rows[len(rows)-1]
		next = l.cursor.encodeCursor(vpcCursor{Version: 1, Kind: kind, TenantID: tenant, Name: f.Name, State: f.State, ID: v.ID, CreatedAt: v.CreatedAt})
	}
	for j := range rows {
		rows[j] = l.observation(rows[j])
	}
	return rows, next, total, nil
}
