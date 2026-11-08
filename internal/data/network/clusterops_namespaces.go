package data

import (
	"context"
	"fmt"
)

// ListRegisteredNamespaceNames reads registration only, including NULL UID.
// It never resolves, adopts, backfills, or writes a live Namespace.
func (p *Postgres) ListRegisteredNamespaceNames(ctx context.Context, clusterID string) ([]string, error) {
	if p == nil || p.queries == nil || p.placement.ClusterID != clusterID {
		return nil, fmt.Errorf("namespace registration unavailable")
	}
	rows, err := p.queries.ListTenantNamespaceIdentities(ctx)
	if err != nil {
		return nil, err
	}
	names := []string{}
	seen := map[string]bool{}
	for _, row := range rows {
		if row.ClusterID == clusterID && !seen[row.Namespace] {
			names = append(names, row.Namespace)
			seen[row.Namespace] = true
		}
	}
	return names, nil
}
