package biz

import (
	"context"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"time"
)

const IntranetVerificationScope = "base_intranet"

type IntranetPoolDefaultsInfrastructure interface {
	GetIntranetPoolDefaults(context.Context) (PublicPoolConfig, error)
}

func (e *Egress) GetIntranetPoolDefaults(ctx context.Context) (PublicPoolConfig, error) {
	if _, err := e.authorization.Platform(ctx); err != nil {
		return PublicPoolConfig{}, err
	}
	return e.defaults.GetIntranetPoolDefaults(ctx)
}

func NormalizeIntranetPool(p PublicPoolConfig) (PublicPoolConfig, error) {
	bad := func() (PublicPoolConfig, error) {
		return p, Fail(InvalidArgument, "invalid intranet pool, default VPC identity or destination networks")
	}
	if (p.Scope != "" && p.Scope != "intranet") || (p.Mode != "" && p.Mode != "overlay") || p.GatewayID != "" || p.VlanNetworkID != "" || p.UpstreamGatewayIP != "" {
		return bad()
	}
	if !regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`).MatchString(p.DefaultVPCName) || strings.TrimSpace(p.DefaultVPCUID) != p.DefaultVPCUID || p.DefaultVPCUID == "" || len(p.DefaultVPCUID) > 128 || strings.ContainsAny(p.DefaultVPCUID, "/\\\t\r\n ") {
		return bad()
	}
	cidr, err := netip.ParsePrefix(p.CIDR)
	if err != nil || !cidr.Addr().Is4() || cidr != cidr.Masked() || cidr.Bits() > 30 || cidr.Bits() == 0 {
		return bad()
	}
	gw, err := netip.ParseAddr(p.OVNGatewayIP)
	if err != nil || !gw.Is4() || gw.String() != p.OVNGatewayIP || !cidr.Contains(gw) || gw == cidr.Addr() || gw == lastPoolAddress(cidr) {
		return bad()
	}
	if len(p.ExcludedIPs) > 1024 || len(p.IntranetNetworks) > 1024 {
		return bad()
	}
	p.ExcludedIPs = append(slices.Clone(p.ExcludedIPs), p.OVNGatewayIP)
	for _, raw := range p.ExcludedIPs {
		lo, hi, ok := strings.Cut(raw, "..")
		if !ok {
			hi = lo
		}
		a, ea := netip.ParseAddr(lo)
		b, eb := netip.ParseAddr(hi)
		if ea != nil || eb != nil || a.String() != lo || b.String() != hi || !cidr.Contains(a) || !cidr.Contains(b) || a.Compare(b) > 0 {
			return bad()
		}
	}
	p.IntranetNetworks = slices.Clone(p.IntranetNetworks)
	for _, raw := range p.IntranetNetworks {
		network, err := netip.ParsePrefix(raw)
		if err != nil || !network.Addr().Is4() || network != network.Masked() || network.String() != raw || network.Bits() == 0 {
			return bad()
		}
	}
	slices.Sort(p.ExcludedIPs)
	p.ExcludedIPs = slices.Compact(p.ExcludedIPs)
	slices.Sort(p.IntranetNetworks)
	p.IntranetNetworks = slices.Compact(p.IntranetNetworks)
	p.Scope, p.Mode = "intranet", "overlay"
	return p, nil
}

func ValidatePoolVerificationForScope(v PublicPoolVerification, scope string, now time.Time) error {
	if err := ValidatePoolVerification(v, now); err != nil {
		return err
	}
	if (scope == "intranet" && v.Scope != IntranetVerificationScope) || (scope != "intranet" && v.Scope == IntranetVerificationScope) {
		return Fail(InvalidArgument, "verification capability does not match pool scope")
	}
	return nil
}

func (e *Egress) GetPlatformCapabilities(ctx context.Context) (PlatformNetworkCapabilities, error) {
	if _, err := e.authorization.Platform(ctx); err != nil {
		return PlatformNetworkCapabilities{}, err
	}
	repository, ok := e.repository.(PlatformCapabilitiesRepository)
	if !ok {
		return PlatformNetworkCapabilities{}, Fail(DependencyUnavailable, "platform capability repository is unavailable")
	}
	return repository.GetPlatformCapabilities(ctx)
}
