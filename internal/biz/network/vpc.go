package biz

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/netip"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/tenantnamespace"
)

// Reason is a stable product failure, independent of transport or provider.
type Reason string

const (
	InvalidArgument       Reason = "INVALID_ARGUMENT"
	TenantRequired        Reason = "TENANT_REQUIRED"
	InvalidCursor         Reason = "INVALID_CURSOR"
	ResourceNotFound      Reason = "RESOURCE_NOT_FOUND"
	IdempotencyConflict   Reason = "IDEMPOTENCY_CONFLICT"
	ResourceInUse         Reason = "RESOURCE_IN_USE"
	ResourceBusy          Reason = "RESOURCE_BUSY"
	DependencyUnavailable Reason = "DEPENDENCY_UNAVAILABLE"
)

type Error struct {
	Reason  Reason
	Message string
	Cause   error
}

func (e *Error) Error() string { return string(e.Reason) + ": " + e.Message }
func (e *Error) Unwrap() error { return e.Cause }

func ReasonOf(err error) Reason {
	var failure *Error
	if errors.As(err, &failure) {
		return failure.Reason
	}
	return ""
}

func Fail(reason Reason, message string) error {
	return &Error{Reason: reason, Message: message}
}

// VPCIntent is a normalized create request. Attribution is deliberately absent:
// the actor/direct caller must not change the identity of an idempotent request.
type VPCIntent struct {
	NamespaceIdentity tenantnamespace.Identity
	TenantID          string
	Name              string
	CIDR              string
	Description       string
	IdempotencyKey    string
}

func ParseTenant(value string) (string, error) {
	id, err := uuid.Parse(value)
	if len(value) != 36 || err != nil || id == uuid.Nil || id.String() != value {
		return "", Fail(TenantRequired, "a nonzero UUID tenant_id is required")
	}
	return id.String(), nil
}

func NewVPCIntent(tenant, name, cidr, description, key string) (VPCIntent, error) {
	tenant, err := ParseTenant(tenant)
	if err != nil {
		return VPCIntent{}, err
	}
	name = strings.TrimSpace(name)
	if !utf8.ValidString(name) || utf8.RuneCountInString(name) < 1 || utf8.RuneCountInString(name) > 128 {
		return VPCIntent{}, Fail(InvalidArgument, "name must contain 1..128 Unicode characters")
	}
	for _, character := range name {
		if unicode.IsControl(character) {
			return VPCIntent{}, Fail(InvalidArgument, "name must not contain control characters")
		}
	}
	if !utf8.ValidString(description) || utf8.RuneCountInString(description) > 1024 {
		return VPCIntent{}, Fail(InvalidArgument, "description must contain at most 1024 Unicode characters")
	}
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil || !prefix.Addr().Is4() || prefix != prefix.Masked() || prefix.Bits() > 30 {
		return VPCIntent{}, Fail(InvalidArgument, "cidr must be a canonical IPv4 network with prefix at most /30")
	}
	private := false
	for _, network := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"} {
		parent := netip.MustParsePrefix(network)
		if prefix.Bits() >= parent.Bits() && parent.Contains(prefix.Addr()) {
			private = true
		}
	}
	if !private {
		return VPCIntent{}, Fail(InvalidArgument, "cidr must be contained in one RFC1918 private network")
	}
	if len(key) < 1 || len(key) > 128 {
		return VPCIntent{}, Fail(InvalidArgument, "idempotency_key must contain 1..128 printable non-whitespace ASCII characters")
	}
	for _, character := range []byte(key) {
		if character < 0x21 || character > 0x7e {
			return VPCIntent{}, Fail(InvalidArgument, "idempotency_key contains an invalid character")
		}
	}
	return VPCIntent{
		TenantID: tenant, Name: name, CIDR: prefix.String(),
		Description: description, IdempotencyKey: key,
	}, nil
}

// Fingerprint uses a versioned canonical record; trace and attribution never
// participate. It does not depend on current resource/provider state.
func (v VPCIntent) Fingerprint() string {
	value, _ := json.Marshal(struct {
		Version     int
		Name        string
		CIDR        string
		Description string
	}{1, v.Name, v.CIDR, v.Description})
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

// Message is the public explanation paired with a stable reason code. It never
// includes infrastructure errors, identifiers or caller-provided data.
func (r Reason) Message() string {
	switch r {
	case "":
		return ""
	case ProviderUnavailable:
		return "The network provider is temporarily unavailable."
	case ProviderNotReady:
		return "The requested configuration is not ready yet."
	case ProviderRejected:
		return "The network provider rejected the creation request."
	case ProviderOwnership:
		return "The provider object identity or configuration conflicts with this resource."
	case ProviderUnknown:
		return "The external request result is unknown; Network is observing the original request."
	case ProviderMissing:
		return "The previously recorded provider object is missing."
	case CleanupPending:
		return "Provider cleanup has been requested and awaits confirmation."
	case BaseConnectivityNotReady:
		return "VPC base connectivity is not ready"
	case LoadBalancerNotReady:
		return "The load balancer capability is not ready."
	case BackendIdentityMismatch:
		return "The backend no longer matches its accepted network identity."
	case WaitingForBackends:
		return "The listener configuration is saved and awaits backends."
	case WaitingForRedirectTarget:
		return "The redirect configuration awaits backends on its HTTPS target."
	case VIPInUse:
		return "The private address is reserved by another network resource."
	case Reason("CREATE_TERMINATED"):
		return "VPC creation was terminated; cleanup is tracked by its delete operation"
	case PublicEgressNotReady:
		return "The public egress configuration or acceptance evidence is not ready."
	case ProviderStateMismatch:
		return "The observed provider configuration differs from the requested state."
	case EIPInUse:
		return "The EIP is reserved by a binding."
	case VPCSnatExists:
		return "The VPC has a binding that must be released first."
	case PermissionDenied:
		return "The trusted caller does not have permission for this operation."
	case ResourceInUse:
		return "Dependent network resources must be released before deletion."
	default:
		return "The network resource requires attention."
	}
}
