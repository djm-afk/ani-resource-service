package biz

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/netip"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// VNic is a user-owned network interface. Runtime consumer deletion never deletes it.
type VNic struct {
	ID, TenantID, Name, Description, VPCID, SubnetID, VPCName, SubnetName string
	State                                                                 ResourceState
	Reason                                                                Reason
	Version                                                               int64
	MACAddress, PrimaryIP                                                 string
	RequestedIP, RequestedMAC                                             string
	IPs, SecurityGroupIDs, SecurityGroupNames                             []string
	Bindings                                                              []VNicBinding
	ObservedAt                                                            *time.Time
	ObservationStale                                                      bool
	CreatedAt, UpdatedAt                                                  time.Time
	LastOperationID                                                       string
	// Trusted provider identity is an internal port, never accepted from callers.
	ProviderName, ProviderUID, ClusterID, Namespace, NamespaceUID string
}
type VNicBinding struct {
	ID, ConsumerKind, ConsumerID, Slot, ConsumerName, ComputeType string
	RequestedRevision, AppliedRevision                            int64
	Current, Target                                               bool
}
type CreateVNic struct {
	TenantID, Name, Description, SubnetID, IdempotencyKey string
	RequestedIP, RequestedMAC                             string
	Attribution                                           Attribution
}

func (v CreateVNic) Fingerprint() string {
	fields := []string{v.TenantID, v.Name, v.Description, v.SubnetID}
	// Preserve fingerprints of already accepted automatic-allocation requests.
	if v.RequestedIP != "" || v.RequestedMAC != "" {
		fields = append(fields, v.RequestedIP, v.RequestedMAC)
	}
	b, _ := json.Marshal(fields)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

type VNicFilter struct {
	PageWindow
	Name, SubnetID, State, BindingState, BindableToKind, BindableToID string
}
type VNicReservation struct {
	TenantID, VNicID, ConsumerKind, ConsumerID, Slot, OperationID, SubnetID string
	Revision                                                                int64
}
type VNicRepository interface {
	AcceptVNic(context.Context, CreateVNic) (VNic, error)
	GetVNic(context.Context, string, string) (VNic, error)
	ListVNics(context.Context, string, VNicFilter) ([]VNic, int64, error)
	DeleteVNic(context.Context, string, string) (VNic, error)
	ReserveVNic(context.Context, VNicReservation) (VNic, error)
	SyncVNicSecurityGroups(context.Context, string, string, string, string, []string) (VNic, error)
	ReleaseVNicBindings(context.Context, string, string, string, []string) error
	ApplyVNicBinding(context.Context, string, string, string, string, int64) error
}

func validateVNicName(name string) error {
	if !utf8.ValidString(name) || utf8.RuneCountInString(name) < 1 || utf8.RuneCountInString(name) > 128 {
		return Fail(InvalidArgument, "name must contain 1..128 Unicode characters")
	}
	for _, c := range name {
		if unicode.IsControl(c) {
			return Fail(InvalidArgument, "name must not contain control characters")
		}
	}
	return nil
}
func validVNicID(id string) bool {
	if len(id) != 37 || !strings.HasPrefix(id, "vnic_") {
		return false
	}
	_, e := hex.DecodeString(id[5:])
	return e == nil && strings.ToLower(id) == id
}
func (n *Network) vnicRepository() (VNicRepository, error) {
	r, ok := n.repository.(VNicRepository)
	if !ok {
		return nil, Fail(ProviderUnavailable, "manual VNIC management unavailable")
	}
	return r, nil
}
func (n *Network) CreateVNic(ctx context.Context, r CreateVNic) (VNic, error) {
	var err error
	r.TenantID, err = ParseTenant(r.TenantID)
	if err != nil {
		return VNic{}, err
	}
	r.Name = strings.TrimSpace(r.Name)
	r.Description = strings.TrimSpace(r.Description)
	r.RequestedIP, r.RequestedMAC, err = normalizeVNicAddresses(r.RequestedIP, r.RequestedMAC)
	if err != nil {
		return VNic{}, err
	}
	if err = validateVNicName(r.Name); err != nil {
		return VNic{}, err
	}
	if !validSubnetID(r.SubnetID) || len(r.Description) > 1024 || len(r.IdempotencyKey) < 1 || len(r.IdempotencyKey) > 128 {
		return VNic{}, Fail(InvalidArgument, "subnet, description or idempotency key invalid")
	}
	if err = validateAttribution(r.Attribution); err != nil {
		return VNic{}, err
	}
	repo, err := n.vnicRepository()
	if err != nil {
		return VNic{}, err
	}
	return repo.AcceptVNic(ctx, r)
}

func normalizeVNicAddresses(ip, mac string) (string, string, error) {
	ip, mac = strings.TrimSpace(ip), strings.TrimSpace(mac)
	if ip != "" {
		address, err := netip.ParseAddr(ip)
		if err != nil || !address.Is4() || address.IsUnspecified() || address.IsMulticast() {
			return "", "", Fail(InvalidArgument, "requested_ip must be an IPv4 host address")
		}
		ip = address.String()
	}
	if mac != "" {
		address, err := net.ParseMAC(mac)
		if err != nil || len(address) != 6 || address[0]&1 != 0 || address.String() == "00:00:00:00:00:00" {
			return "", "", Fail(InvalidArgument, "requested_mac must be a nonzero unicast MAC address with six octets")
		}
		mac = address.String()
	}
	return ip, mac, nil
}

// ValidateVNicRequestedIP checks the product Subnet boundary; KCN owns the
// allocation pool and concurrent IP reservation.
func ValidateVNicRequestedIP(ip, cidr, gateway string) error {
	if ip == "" {
		return nil
	}
	address, err := netip.ParseAddr(ip)
	prefix, prefixErr := netip.ParsePrefix(cidr)
	if err != nil || prefixErr != nil || !address.Is4() || !prefix.Addr().Is4() || !prefix.Contains(address) {
		return Fail(InvalidArgument, "requested_ip must belong to the selected Subnet")
	}
	bytes := prefix.Masked().Addr().As4()
	for bit := prefix.Bits(); bit < 32; bit++ {
		bytes[bit/8] |= byte(1 << (7 - bit%8))
	}
	if address == prefix.Masked().Addr() || address == netip.AddrFrom4(bytes) || ip == gateway {
		return Fail(InvalidArgument, "requested_ip must not be the network, broadcast or gateway address")
	}
	return nil
}
func (n *Network) GetVNic(ctx context.Context, tenant, id string) (VNic, error) {
	tenant, e := ParseTenant(tenant)
	if e != nil {
		return VNic{}, e
	}
	if !validVNicID(id) {
		return VNic{}, Fail(ResourceNotFound, "VNIC not found")
	}
	r, e := n.vnicRepository()
	if e != nil {
		return VNic{}, e
	}
	v, e := r.GetVNic(ctx, tenant, id)
	return v, e
}
func (n *Network) ListVNics(ctx context.Context, tenant string, f VNicFilter) ([]VNic, int64, error) {
	switch ResourceState(f.State) {
	case "", Provisioning, Available, Degraded, Failed, Deleting, Deleted:
	default:
		return nil, 0, Fail(InvalidArgument, "invalid VNIC state")
	}
	tenant, e := ParseTenant(tenant)
	if e != nil {
		return nil, 0, e
	}
	f.Name = strings.TrimSpace(f.Name)
	if f.Limit < 1 || f.Limit > 100 || f.Offset < 0 || (f.SubnetID != "" && !validSubnetID(f.SubnetID)) || (f.BindingState != "" && f.BindingState != "free" && f.BindingState != "bound" && f.BindingState != "reserved") || ((f.BindableToKind == "") != (f.BindableToID == "")) {
		return nil, 0, Fail(InvalidArgument, "invalid VNIC filter")
	}
	if f.BindableToKind != "" && f.BindableToKind != "vm" && f.BindableToKind != "container" {
		return nil, 0, Fail(InvalidArgument, "unsupported consumer kind")
	}
	r, e := n.vnicRepository()
	if e != nil {
		return nil, 0, e
	}
	vs, total, e := r.ListVNics(ctx, tenant, f)
	return vs, total, e
}
func (n *Network) DeleteVNic(ctx context.Context, tenant, id string) (VNic, error) {
	tenant, e := ParseTenant(tenant)
	if e != nil {
		return VNic{}, e
	}
	if !validVNicID(id) {
		return VNic{}, Fail(ResourceNotFound, "VNIC not found")
	}
	r, e := n.vnicRepository()
	if e != nil {
		return VNic{}, e
	}
	return r.DeleteVNic(ctx, tenant, id)
}
func (n *Network) ReserveVNic(ctx context.Context, v VNicReservation) (VNic, error) {
	if v.Slot != "primary" || v.Revision < 1 || v.OperationID == "" || (v.ConsumerKind != "vm" && v.ConsumerKind != "container") || v.ConsumerID == "" || !validVNicID(v.VNicID) || !validSubnetID(v.SubnetID) {
		return VNic{}, Fail(InvalidArgument, "invalid primary VNIC reservation")
	}
	var e error
	v.TenantID, e = ParseTenant(v.TenantID)
	if e != nil {
		return VNic{}, e
	}
	r, e := n.vnicRepository()
	if e != nil {
		return VNic{}, e
	}
	return r.ReserveVNic(ctx, v)
}
func (n *Network) SyncVNicSecurityGroups(ctx context.Context, tenant, id, kind, consumer string, groups []string) (VNic, error) {
	if _, e := n.GetVNic(ctx, tenant, id); e != nil {
		return VNic{}, e
	}
	r, e := n.vnicRepository()
	if e != nil {
		return VNic{}, e
	}
	return r.SyncVNicSecurityGroups(ctx, tenant, id, kind, consumer, groups)
}
func (n *Network) ReleaseVNicBindings(ctx context.Context, tenant, kind, consumer string, keep []string) error {
	tenant, e := ParseTenant(tenant)
	if e != nil {
		return e
	}
	if consumer == "" || (kind != "vm" && kind != "container") {
		return Fail(InvalidArgument, "invalid VNIC consumer")
	}
	r, e := n.vnicRepository()
	if e != nil {
		return e
	}
	return r.ReleaseVNicBindings(ctx, tenant, kind, consumer, keep)
}

type VNicFacts struct {
	MACAddress, PrimaryIP string
	IPs                   []string
}
type VNicProvider interface {
	EnsureVNic(context.Context, ProviderTarget) (ProviderObservation, error)
}

func (n *Network) ApplyVNicBinding(ctx context.Context, tenant, kind, consumer, id string, revision int64) error {
	tenant, e := ParseTenant(tenant)
	if e != nil {
		return e
	}
	if revision < 1 || (id != "" && !validVNicID(id)) {
		return Fail(InvalidArgument, "invalid applied VNIC revision")
	}
	r, e := n.vnicRepository()
	if e != nil {
		return e
	}
	return r.ApplyVNicBinding(ctx, tenant, kind, consumer, id, revision)
}

// VNicConsumerScope is supplied by the owning instance domain, never by public callers.
type VNicConsumerScope struct {
	TenantID, ConsumerKind, ConsumerID, SubnetID, ClusterID, Namespace, NamespaceUID string
	Version                                                                          int64
	Name, State, ComputeType                                                         string
	SecurityGroupIDs                                                                 []string
}
type VNicConsumerResolver func(context.Context, string, string, string) (VNicConsumerScope, error)
