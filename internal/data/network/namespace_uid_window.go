package data

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/tenantnamespace"
	"github.com/zhangzhe-ctrl/ani-resource-service/migrations"
)

// NamespaceUIDWindowConstraint exists only during the reviewed 0025 cutover.
// It is deliberately absent from migrations and normal runtime composition.
const NamespaceUIDWindowConstraint = "network_tenant_namespaces_uid_cutover"

type NamespaceUIDWindowRow struct{ TenantID, Namespace, UID string }
type NamespaceUIDWindowBackend struct {
	PID       int32
	StartedAt time.Time
}
type NamespaceUIDWindowInput struct {
	WindowID, Stage, ClusterID, Database, RuntimeRole string
	Rows                                              []NamespaceUIDWindowRow
	WriterAddresses                                   []string
	OldBackends                                       []NamespaceUIDWindowBackend
	WriterProofSHA256                                 string
}
type NamespaceUIDWindowStoredRow struct {
	TenantID, Namespace string
	UID                 *string
}
type NamespaceUIDWindowSnapshot struct {
	Database, User, ServerAddress, TableOwner string
	DatabaseOID, TableOID                     uint32
	ServerPort, SchemaVersion                 int
	UIDColumn                                 bool
	Constraint, ConstraintComment             string
	ConstraintValidated                       bool
	Rows                                      []NamespaceUIDWindowStoredRow
	OldBackends                               []NamespaceUIDWindowBackend
}
type NamespaceUIDWindowPlan struct {
	Input  NamespaceUIDWindowInput
	Before NamespaceUIDWindowSnapshot
	SQL    []string
	SHA256 string
}

func namespaceUIDWindowInput(in NamespaceUIDWindowInput) error {
	for _, value := range []string{in.WindowID, in.ClusterID} {
		parsed, err := uuid.Parse(value)
		if err != nil || parsed == uuid.Nil || parsed.String() != value {
			return fmt.Errorf("window and cluster must be canonical nonzero UUIDs")
		}
	}
	if in.Database == "" || in.RuntimeRole == "" || len(in.Rows) == 0 || len(in.Rows) > 32 {
		return fmt.Errorf("explicit database, runtime role and bounded exact rows required")
	}
	if in.Stage != "freeze" && in.Stage != "require-uid" && in.Stage != "release" && in.Stage != "inspect" {
		return fmt.Errorf("stage must be freeze, require-uid, release or inspect")
	}
	previous := ""
	for _, row := range in.Rows {
		name, err := tenantnamespace.Name(row.TenantID)
		if err != nil || name != row.Namespace || row.UID == "" || row.TenantID <= previous {
			return fmt.Errorf("rows must have ordered unique canonical tenants and exact standard Namespace/UID")
		}
		previous = row.TenantID
	}
	if (in.Stage == "freeze" || in.Stage == "release") && len(in.WriterAddresses) == 0 {
		return fmt.Errorf("freeze/release requires the original Resource pod database client addresses")
	}
	for _, address := range in.WriterAddresses {
		if net.ParseIP(address) == nil {
			return fmt.Errorf("writer addresses must be explicit IP addresses")
		}
	}
	if in.Stage == "release" {
		decoded, err := hex.DecodeString(in.WriterProofSHA256)
		if err != nil || len(decoded) != 32 || hex.EncodeToString(decoded) != in.WriterProofSHA256 {
			return fmt.Errorf("release requires the freshly verified writer proof SHA256")
		}
	}
	return nil
}

func namespaceUIDWindowExpression(in NamespaceUIDWindowInput, uid bool) string {
	// ClusterID has already passed canonical UUID validation. No identifier or
	// user SQL is interpolated; the table and constraint names are fixed.
	expression := "cluster_id <> '" + in.ClusterID + "'"
	if uid {
		expression = "(" + expression + ") OR namespace_uid IS NOT NULL"
	}
	return expression
}
func namespaceUIDWindowExpressionMatches(actual string, in NamespaceUIDWindowInput, uid bool) bool {
	normalize := func(s string) string {
		s = strings.ReplaceAll(s, "::text", "")
		return strings.Map(func(r rune) rune {
			if r == '(' || r == ')' || r == ' ' || r == '\t' || r == '\n' {
				return -1
			}
			return r
		}, s)
	}
	return normalize(actual) == normalize(namespaceUIDWindowExpression(in, uid))
}

// PlanNamespaceUIDWindow is read-only even though it uses the separate owner
// connection needed later for explicit ALTER TABLE. It works before 0025.
func PlanNamespaceUIDWindow(ctx context.Context, owner *pgxpool.Pool, in NamespaceUIDWindowInput) (NamespaceUIDWindowPlan, error) {
	if err := namespaceUIDWindowInput(in); err != nil {
		return NamespaceUIDWindowPlan{}, err
	}
	tx, err := owner.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return NamespaceUIDWindowPlan{}, err
	}
	defer tx.Rollback(ctx)
	return namespaceUIDWindowPlan(ctx, tx, in)
}
func namespaceUIDWindowPlan(ctx context.Context, tx pgx.Tx, in NamespaceUIDWindowInput) (NamespaceUIDWindowPlan, error) {
	plan := NamespaceUIDWindowPlan{Input: in, SQL: []string{}}
	snapshot := &plan.Before
	if err := tx.QueryRow(ctx, `SELECT current_database(),current_user,coalesce(inet_server_addr()::text,''),coalesce(inet_server_port(),0),(SELECT oid FROM pg_database WHERE datname=current_database()),c.oid,pg_get_userbyid(c.relowner) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relname='network_tenant_namespaces' AND c.relkind='r'`).Scan(&snapshot.Database, &snapshot.User, &snapshot.ServerAddress, &snapshot.ServerPort, &snapshot.DatabaseOID, &snapshot.TableOID, &snapshot.TableOwner); err != nil {
		return plan, err
	}
	var ownerAuthorized, statisticsVisible bool
	if err := tx.QueryRow(ctx, `SELECT pg_has_role(current_user,$1,'USAGE'),rolsuper OR pg_has_role(current_user,'pg_read_all_stats','USAGE') FROM pg_roles WHERE rolname=current_user`, snapshot.TableOwner).Scan(&ownerAuthorized, &statisticsVisible); err != nil {
		return plan, err
	}
	if snapshot.Database != in.Database || !ownerAuthorized || snapshot.User == in.RuntimeRole {
		return plan, fmt.Errorf("database/table owner/runtime identities differ")
	}
	if (in.Stage == "freeze" || in.Stage == "release") && !statisticsVisible {
		return plan, fmt.Errorf("existing maintenance owner cannot inspect original backend identities; no privileges were changed")
	}
	var runtimeExists bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname=$1)", in.RuntimeRole).Scan(&runtimeExists); err != nil || !runtimeExists {
		return plan, fmt.Errorf("explicit runtime role not found")
	}
	var unsafeRuntime bool
	if err := tx.QueryRow(ctx, `SELECT rolsuper OR rolbypassrls OR rolcreatedb OR rolcreaterole OR pg_has_role($1,$2,'SET') OR has_table_privilege($1,$3::oid,'DELETE,TRUNCATE,TRIGGER') FROM pg_roles WHERE rolname=$1`, in.RuntimeRole, snapshot.TableOwner, snapshot.TableOID).Scan(&unsafeRuntime); err != nil {
		return plan, err
	}
	if unsafeRuntime {
		return plan, fmt.Errorf("runtime can bypass the bounded INSERT/UPDATE barrier; no ACL changes performed")
	}

	files, err := fs.Glob(migrations.Files, "*.sql")
	if err != nil {
		return plan, err
	}
	sort.Strings(files)
	if len(files) != 25 {
		return plan, fmt.Errorf("this one-time tool requires exactly 25 compiled Network migrations")
	}
	versions, err := tx.Query(ctx, "SELECT version,checksum FROM network_schema_version ORDER BY version")
	if err != nil {
		return plan, err
	}
	for versions.Next() {
		var version int
		var checksum string
		if err = versions.Scan(&version, &checksum); err != nil {
			versions.Close()
			return plan, err
		}
		snapshot.SchemaVersion++
		if version != snapshot.SchemaVersion || version > 25 {
			versions.Close()
			return plan, fmt.Errorf("unexpected Network schema history")
		}
		body, _ := migrations.Files.ReadFile(files[version-1])
		sum := sha256.Sum256(body)
		if checksum != hex.EncodeToString(sum[:]) {
			versions.Close()
			return plan, fmt.Errorf("Network migration checksum differs")
		}
	}
	err = versions.Err()
	versions.Close()
	if err != nil {
		return plan, err
	}
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_attribute WHERE attrelid=$1 AND attname='namespace_uid' AND NOT attisdropped)`, snapshot.TableOID).Scan(&snapshot.UIDColumn); err != nil {
		return plan, err
	}
	var guardName string
	err = tx.QueryRow(ctx, `SELECT conname,pg_get_expr(conbin,conrelid),convalidated,coalesce(obj_description(oid,'pg_constraint'),'') FROM pg_constraint WHERE conrelid=$1 AND conname=$2 AND contype='c'`, snapshot.TableOID, NamespaceUIDWindowConstraint).Scan(&guardName, &snapshot.Constraint, &snapshot.ConstraintValidated, &snapshot.ConstraintComment)
	if err != nil && err != pgx.ErrNoRows {
		return plan, err
	}
	query := "SELECT tenant_id::text,namespace,NULL::text FROM network_tenant_namespaces WHERE cluster_id=$1 ORDER BY tenant_id::text"
	if snapshot.UIDColumn {
		query = "SELECT tenant_id::text,namespace,namespace_uid FROM network_tenant_namespaces WHERE cluster_id=$1 ORDER BY tenant_id::text"
	}
	rows, err := tx.Query(ctx, query, in.ClusterID)
	if err != nil {
		return plan, err
	}
	snapshot.Rows = []NamespaceUIDWindowStoredRow{}
	for rows.Next() {
		var row NamespaceUIDWindowStoredRow
		if err = rows.Scan(&row.TenantID, &row.Namespace, &row.UID); err != nil {
			rows.Close()
			return plan, err
		}
		snapshot.Rows = append(snapshot.Rows, row)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return plan, err
	}
	if len(snapshot.Rows) != len(in.Rows) {
		return plan, fmt.Errorf("exact registered Namespace row count differs")
	}
	for index, row := range snapshot.Rows {
		want := in.Rows[index]
		if row.TenantID != want.TenantID || row.Namespace != want.Namespace || row.UID != nil && *row.UID != want.UID {
			return plan, fmt.Errorf("registered Namespace original identity differs")
		}
	}
	snapshot.OldBackends = []NamespaceUIDWindowBackend{}
	if in.Stage == "freeze" {
		// Do not silently omit a pooler, NAT or other writer sharing the role.
		// Source addresses must be the exact freshly verified original Pods.
		var unknown bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND usename=$1 AND (client_addr IS NULL OR NOT (host(client_addr)=ANY($2::text[]))))`, in.RuntimeRole, in.WriterAddresses).Scan(&unknown); err != nil {
			return plan, err
		}
		if unknown {
			return plan, fmt.Errorf("runtime database writer source is outside the exact original Pod addresses")
		}
		backends, err := tx.Query(ctx, `SELECT pid,backend_start FROM pg_stat_activity WHERE datname=current_database() AND usename=$1 AND host(client_addr)=ANY($2::text[]) ORDER BY pid,backend_start`, in.RuntimeRole, in.WriterAddresses)
		if err != nil {
			return plan, err
		}
		for backends.Next() {
			var b NamespaceUIDWindowBackend
			if err = backends.Scan(&b.PID, &b.StartedAt); err != nil {
				backends.Close()
				return plan, err
			}
			snapshot.OldBackends = append(snapshot.OldBackends, b)
		}
		err = backends.Err()
		backends.Close()
		if err != nil {
			return plan, err
		}
	} else if in.Stage == "release" {
		for _, b := range in.OldBackends {
			var active bool
			if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND pid=$1 AND backend_start=$2)`, b.PID, b.StartedAt).Scan(&active); err != nil {
				return plan, err
			}
			if active {
				snapshot.OldBackends = append(snapshot.OldBackends, b)
			}
		}
		if len(snapshot.OldBackends) != 0 {
			return plan, fmt.Errorf("original database writer connection is still present")
		}
		// Include original Pods' connections opened after the freeze snapshot.
		// Reused Pod IPs fail closed until their connections are absent.
		var originalAddressActive bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND usename=$1 AND host(client_addr)=ANY($2::text[]))`, in.RuntimeRole, in.WriterAddresses).Scan(&originalAddressActive); err != nil {
			return plan, err
		}
		if originalAddressActive {
			return plan, fmt.Errorf("original Pod database client address is still active")
		}
	}
	table := `"public"."network_tenant_namespaces"`
	constraint := pgx.Identifier{NamespaceUIDWindowConstraint}.Sanitize()
	comment := "ani.resource.namespace-uid-window.v1:" + in.WindowID
	switch in.Stage {
	case "freeze":
		if snapshot.SchemaVersion != 24 || snapshot.UIDColumn || guardName != "" {
			return plan, fmt.Errorf("freeze requires exact schema24, absent UID column and absent window constraint")
		}
		plan.SQL = append(plan.SQL, "ALTER TABLE "+table+" ADD CONSTRAINT "+constraint+" CHECK ("+namespaceUIDWindowExpression(in, false)+") NOT VALID", "COMMENT ON CONSTRAINT "+constraint+" ON "+table+" IS '"+comment+"'")
	case "require-uid", "release":
		if snapshot.SchemaVersion != 25 || !snapshot.UIDColumn || guardName == "" || snapshot.ConstraintValidated || snapshot.ConstraintComment != comment || !namespaceUIDWindowExpressionMatches(snapshot.Constraint, in, in.Stage == "release") {
			return plan, fmt.Errorf("expected original unvalidated maintenance constraint or schema25 differs")
		}
		if in.Stage == "release" {
			for _, row := range snapshot.Rows {
				if row.UID == nil {
					return plan, fmt.Errorf("target cluster still has NULL Namespace UID")
				}
			}
		}
		plan.SQL = append(plan.SQL, "ALTER TABLE "+table+" DROP CONSTRAINT "+constraint)
		if in.Stage == "require-uid" {
			plan.SQL = append(plan.SQL, "ALTER TABLE "+table+" ADD CONSTRAINT "+constraint+" CHECK ("+namespaceUIDWindowExpression(in, true)+") NOT VALID", "COMMENT ON CONSTRAINT "+constraint+" ON "+table+" IS '"+comment+"'")
		}
	}
	encoded, err := json.Marshal(plan)
	if err != nil {
		return plan, err
	}
	sum := sha256.Sum256(encoded)
	plan.SHA256 = hex.EncodeToString(sum[:])
	return plan, nil
}

// ApplyNamespaceUIDWindow locks out and drains in-flight identity-table
// transactions, rechecks the complete plan under that lock, then applies only
// the reviewed DDL atomically. No error path drops a previously committed guard.
// verify is the CLI's bounded fresh cluster/Namespace/writer verification.
func ApplyNamespaceUIDWindow(ctx context.Context, owner *pgxpool.Pool, in NamespaceUIDWindowInput, reviewedSHA256 string, verify func(context.Context) error) (NamespaceUIDWindowPlan, error) {
	if err := namespaceUIDWindowInput(in); err != nil {
		return NamespaceUIDWindowPlan{}, err
	}
	if in.Stage == "inspect" {
		return NamespaceUIDWindowPlan{}, fmt.Errorf("inspect is always read-only")
	}
	if reviewedSHA256 == "" || verify == nil {
		return NamespaceUIDWindowPlan{}, fmt.Errorf("reviewed plan SHA and live identity verifier required")
	}
	if err := verify(ctx); err != nil {
		return NamespaceUIDWindowPlan{}, err
	}
	tx, err := owner.Begin(ctx)
	if err != nil {
		return NamespaceUIDWindowPlan{}, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SET LOCAL lock_timeout='5s';SET LOCAL statement_timeout='15s'`); err != nil {
		return NamespaceUIDWindowPlan{}, err
	}
	if _, err = tx.Exec(ctx, `LOCK TABLE public.network_tenant_namespaces IN ACCESS EXCLUSIVE MODE`); err != nil {
		return NamespaceUIDWindowPlan{}, err
	}
	plan, err := namespaceUIDWindowPlan(ctx, tx, in)
	if err != nil {
		return plan, err
	}
	if reviewedSHA256 != plan.SHA256 {
		return plan, fmt.Errorf("reviewed maintenance plan changed; no DDL applied")
	}
	if err = verify(ctx); err != nil {
		return plan, err
	}
	for _, sql := range plan.SQL {
		if _, err = tx.Exec(ctx, sql); err != nil {
			return plan, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return plan, err
	}
	return plan, nil
}
