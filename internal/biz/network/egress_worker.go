package biz

import (
	"context"
)

// EgressWorkSpec carries immutable product references and the accepted desired
// state through the existing worker. Provider namespace/name/UID are resolved
// from durable mappings by the infrastructure adapter, never by callers.
type EgressWorkSpec struct {
	EIPID, PoolID  string
	DesiredEnabled bool
	Platform       *PlatformResource
}
type EgressAppliedFacts struct {
	Address        string
	AppliedEnabled *bool
}
type EgressResourceProvider interface {
	EnsureEgress(context.Context, ProviderTarget) (ProviderObservation, error)
	UpdateEgress(context.Context, ProviderTarget) (ProviderObservation, error)
}

func isEgressKind(kind string) bool {
	switch kind {
	case "eip", "snat", "vlan", "egress_gateway", "public_pool":
		return true
	}
	return false
}
func (w *Worker) mutateEgress(ctx context.Context, target ProviderTarget, update bool) (ProviderObservation, error) {
	p, ok := w.provider.(EgressResourceProvider)
	if !ok {
		return ProviderObservation{}, &ProviderError{Kind: ProviderTemporary}
	}
	if update {
		return p.UpdateEgress(ctx, target)
	}
	return p.EnsureEgress(ctx, target)
}
func applyEgressFacts(progress Progress, observed ProviderObservation) Progress {
	if observed.Egress != nil {
		progress.Egress = observed.Egress
	}
	return progress
}
