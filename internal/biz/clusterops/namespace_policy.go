package clusterops

import (
	"context"
	"regexp"
)

var namespaceName = regexp.MustCompile("^[a-z0-9]([-a-z0-9]*[a-z0-9])?$")

func validNamespace(s string) bool { return len(s) > 0 && len(s) <= 63 && namespaceName.MatchString(s) }

type NamespacePolicy struct{ sources []NamespaceSource }

func NewNamespacePolicy(sources ...NamespaceSource) *NamespacePolicy {
	return &NamespacePolicy{sources: append([]NamespaceSource(nil), sources...)}
}
func (p *NamespacePolicy) ProtectedNamespaces(ctx context.Context, cluster string) (map[string]struct{}, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p == nil || len(p.sources) != 2 {
		return nil, NewError("NAMESPACE_POLICY_UNAVAILABLE")
	}
	sub, cancel := context.WithTimeout(ctx, NamespaceTimeout)
	defer cancel()
	result := map[string]struct{}{}
	for _, s := range p.sources {
		if s == nil {
			return nil, NewError("NAMESPACE_POLICY_UNAVAILABLE")
		}
		names, err := s.ListRegisteredNamespaceNames(sub, cluster)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err != nil || sub.Err() != nil {
			return nil, NewError("NAMESPACE_POLICY_UNAVAILABLE")
		}
		for _, n := range names {
			if !validNamespace(n) {
				return nil, NewError("NAMESPACE_POLICY_UNAVAILABLE")
			}
			result[n] = struct{}{}
		}
	}
	return result, nil
}
func needsNamespace(d ResourceDescriptor) bool {
	return d.Namespaced || (d.Type.Group == "" && d.Type.Resource == "namespaces")
}
func RequireNamespaceWritable(protected map[string]struct{}, d ResourceDescriptor, ref ResourceRef) error {
	n := ref.Namespace
	if !d.Namespaced {
		if d.Type.Group != "" || d.Type.Resource != "namespaces" {
			return nil
		}
		n = ref.Name
	}
	if _, ok := protected[n]; ok {
		return NewError("TENANT_NAMESPACE_READ_ONLY")
	}
	return nil
}
