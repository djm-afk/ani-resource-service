package data

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/telemetry"
)

// acceptedEvent records the post-commit outcome of an admission. It is the only
// place a replay can be distinguished from a new acceptance, because both the
// service and business layers see the same returned value.
//
// operation.rejected is not recorded here: a business rejection happens before
// the admission transaction commits, so it is recorded once from the inbound
// RPC completion instead.
type acceptedEvent struct {
	action       string
	resourceType string
	resourceID   string
	tenantID     string
	operationID  string
	// originTraceID is the trace captured when this operation was first
	// accepted, read back from the operation row. A replay must report that
	// stored trace, never the replaying request's own trace.
	originTraceID string
	replayed      bool
}

// logAcceptance writes operation.accepted or operation.replayed at INFO. A nil
// logger (or a struct with no logger bound) records nothing and never blocks the
// business path.
func (p *Postgres) logAcceptance(ctx context.Context, e acceptedEvent) {
	logger := p.logger
	// operation.accepted/operation.replayed both require an operation_id; an
	// admission without one belongs to the attachment mutation vocabulary.
	if logger == nil || e.operationID == "" {
		return
	}
	event, message := "operation.accepted", "network operation accepted"
	if e.replayed {
		event, message = "operation.replayed", "network operation replayed"
	}
	attrs := []any{"event", event, "action", e.action,
		"resource_type", e.resourceType, "resource_id", e.resourceID,
		"tenant_id", e.tenantID, "operation_id", e.operationID}
	if e.replayed {
		attrs = append(attrs, "result", "replayed")
		// Only a stored trace is a real origin; the current request's trace is
		// already carried by trace_id and must not be duplicated here.
		if e.originTraceID != "" {
			attrs = append(attrs, "origin_trace_id", e.originTraceID)
		}
	}
	logger.InfoContext(ctx, message, attrs...)
}

// logResourceMutation records a synchronous attachment mutation that has no
// business operation, so no operation_id is fabricated.
func (p *Postgres) logResourceMutation(ctx context.Context, replayed bool, action, resourceID, tenantID string) {
	logger := p.logger
	if logger == nil {
		return
	}
	event, result, message := "resource.mutated", "succeeded", "network attachment mutated"
	if replayed {
		event, result, message = "resource.replayed", "replayed", "network attachment replayed"
	}
	logger.InfoContext(ctx, message, "event", event, "action", action,
		"resource_type", "attachment", "resource_id", resourceID,
		"attachment_id", resourceID, "tenant_id", tenantID, "result", result)
}

// SetLogger binds the logger used for acceptance events. It is optional: the
// lane runs identically without telemetry.
func (p *Postgres) SetLogger(logger *slog.Logger) { p.logger = logger }

// storedOriginTrace reads through the pool. Callers that already hold a
// transaction connection must use traceReaderOn so the lookup reuses that same
// connection: a second Acquire while the first is held can deadlock the 8-conn
// pool when several replays run concurrently.
func (p *Postgres) storedOriginTrace(ctx context.Context, tenantID, operationID string) string {
	return originTraceOn(ctx, p.queries, tenantID, operationID)
}

func originTraceOn(ctx context.Context, q *sqlcgen.Queries, tenantID, operationID string) string {
	if operationID == "" {
		return ""
	}
	row, err := q.GetOperationTelemetry(ctx, sqlcgen.GetOperationTelemetryParams{
		TenantID: tenantID, OperationID: operationID,
	})
	if err != nil {
		return ""
	}
	tc, err := telemetry.Decode(row)
	if err != nil {
		return ""
	}
	return tc.OriginTraceID
}

// decodeOriginTrace turns one stored telemetry column into its origin trace.
func decodeOriginTrace(raw []byte) string {
	tc, err := telemetry.Decode(raw)
	if err != nil {
		return ""
	}
	return tc.OriginTraceID
}

// Each lane reads through the caller's queries handle so a replay that already
// holds a transaction connection never borrows a second one.
func platformOriginTraceOn(ctx context.Context, q *sqlcgen.Queries, operationID string) string {
	if operationID == "" {
		return ""
	}
	raw, err := q.GetPlatformOperationTelemetry(ctx, sqlcgen.GetPlatformOperationTelemetryParams{OperationID: operationID})
	if err != nil {
		return ""
	}
	return decodeOriginTrace(raw)
}

func switchOriginTraceOn(ctx context.Context, q *sqlcgen.Queries, operationID string) string {
	if operationID == "" {
		return ""
	}
	raw, err := q.GetVirtualSwitchOperationTelemetry(ctx, sqlcgen.GetVirtualSwitchOperationTelemetryParams{OperationID: operationID})
	if err != nil {
		return ""
	}
	return decodeOriginTrace(raw)
}

func securityGroupOriginTraceOn(ctx context.Context, q *sqlcgen.Queries, tenantID, operationID string) string {
	if operationID == "" {
		return ""
	}
	raw, err := q.GetSecurityGroupOperationTelemetry(ctx, sqlcgen.GetSecurityGroupOperationTelemetryParams{
		TenantID: tenantID, OperationID: operationID,
	})
	if err != nil {
		return ""
	}
	return decodeOriginTrace(raw)
}

func defaultEgressOriginTraceOn(ctx context.Context, q *sqlcgen.Queries, operationID string) string {
	if operationID == "" {
		return ""
	}
	raw, err := q.GetDefaultEgressOperationTelemetry(ctx, sqlcgen.GetDefaultEgressOperationTelemetryParams{OperationID: operationID})
	if err != nil {
		return ""
	}
	return decodeOriginTrace(raw)
}

// StoredOriginTraceForTest exposes the replay lookup to integration tests.
func (p *Postgres) StoredOriginTraceForTest(ctx context.Context, tenantID, operationID string) string {
	return originTraceOn(ctx, p.queries, tenantID, operationID)
}

// NewPostgresOnPoolForTest builds a repository over a caller-supplied pool so an
// integration test can constrain its size. Production wiring still uses
// OpenPostgres.
func NewPostgresOnPoolForTest(pool *pgxpool.Pool, clusterID string) *Postgres {
	if pool == nil {
		return nil
	}
	return &Postgres{pool: pool, queries: sqlcgen.New(pool), placement: Placement{ClusterID: clusterID, NamespacePrefix: "tenant-"}}
}

// replayOperationID returns the operation id stored on the platform idempotency
// receipt for this intent. It is the operation the original acceptance created,
// which for a pool verification differs from the resource's last operation.
func (p *Postgres) replayOperationID(ctx context.Context, i biz.PlatformIntent) string {
	return replayOperationIDOn(ctx, p.queries, p.placement.ClusterID, i)
}

// replayOperationIDOn reads through the caller's queries handle so a replay that
// already holds a transaction connection never borrows a second one.
func replayOperationIDOn(ctx context.Context, q *sqlcgen.Queries, cluster string, i biz.PlatformIntent) string {
	prior, err := q.GetPlatformIdempotency(ctx, sqlcgen.GetPlatformIdempotencyParams{
		ClusterID: cluster, OperationKind: i.Kind, IdempotencyKey: i.IdempotencyKey,
	})
	if err != nil {
		return ""
	}
	return prior.OperationID
}
