package sandbox

import (
	"context"
	"fmt"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/tenantnamespace"
)

type NamespaceRegistrationReader struct {
	pool      *pgxpool.Pool
	clusterID string
}

// OpenNamespaceRegistrationReader validates static configuration but does not
// Ping or migrate. A failed SELECT can recover naturally on the next request.
func OpenNamespaceRegistrationReader(ctx context.Context, dsn, clusterID string) (*NamespaceRegistrationReader, error) {
	if clusterID == "" {
		return nil, fmt.Errorf("namespace registration cluster required")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("invalid namespace registration configuration")
	}
	cfg.MaxConns = 2
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("namespace registration pool unavailable")
	}
	return &NamespaceRegistrationReader{pool: pool, clusterID: clusterID}, nil
}
func (r *NamespaceRegistrationReader) Close() {
	if r != nil && r.pool != nil {
		r.pool.Close()
	}
}
func (r *NamespaceRegistrationReader) ListRegisteredNamespaceNames(ctx context.Context, clusterID string) ([]string, error) {
	if r == nil || r.pool == nil || r.clusterID != clusterID {
		return nil, fmt.Errorf("namespace registration unavailable")
	}
	rows, err := r.pool.Query(ctx, "SELECT tenant_id::text FROM sandbox.tenant_spaces ORDER BY tenant_id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	names := []string{}
	for rows.Next() {
		var tenant string
		if err = rows.Scan(&tenant); err != nil {
			return nil, err
		}
		name, err := tenantnamespace.Name(tenant)
		if err != nil {
			return nil, fmt.Errorf("invalid namespace registration")
		}
		names = append(names, name)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return names, nil
}
