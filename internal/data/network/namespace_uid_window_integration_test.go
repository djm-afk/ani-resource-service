package data_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	data "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/tenantnamespace"
	"github.com/zhangzhe-ctrl/ani-resource-service/tests/testenv"
)

type uidWindowFixture struct {
	f     *testenv.Database
	owner *pgxpool.Pool
	repo  *data.Postgres
	input data.NamespaceUIDWindowInput
	vpc   biz.VPC
}

func newUIDWindowFixture(t *testing.T) *uidWindowFixture {
	t.Helper()
	ctx := context.Background()
	f := testenv.NewDatabase(t)
	config, err := pgxpool.ParseConfig(os.Getenv("NETWORK_TEST_ADMIN_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.Database = f.Owner.Config().ConnConfig.Database
	config.MaxConns = 2
	owner, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
	var clientAddress string
	if err = owner.QueryRow(ctx, "SELECT host(inet_client_addr())").Scan(&clientAddress); err != nil {
		t.Fatal(err)
	}
	input := data.NamespaceUIDWindowInput{WindowID: uuid.NewString(), Stage: "freeze", ClusterID: "f39bf6e2-f3ed-4191-ada3-caeebf86c220", Database: config.ConnConfig.Database, RuntimeRole: f.RuntimeRole, WriterAddresses: []string{clientAddress}, Rows: []data.NamespaceUIDWindowRow{{TenantID: "11111111-1111-4111-8111-111111111111", Namespace: "tenant-11111111-1111-4111-8111-111111111111", UID: "uid-a"}, {TenantID: "22222222-2222-4222-8222-222222222222", Namespace: "tenant-22222222-2222-4222-8222-222222222222", UID: "uid-b"}}}
	repo, err := data.OpenPostgres(ctx, f.RuntimeDSN, data.Placement{ClusterID: input.ClusterID, NamespacePrefix: "tenant-"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(repo.Close)
	repo.ConfigureTenantNamespaceResolver(func(_ context.Context, tenant string) (tenantnamespace.Identity, error) {
		for _, r := range input.Rows {
			if r.TenantID == tenant {
				return tenantnamespace.Identity{TenantID: tenant, ClusterID: input.ClusterID, Name: r.Namespace, UID: r.UID}, nil
			}
		}
		return tenantnamespace.Identity{}, errors.New("unknown fixture tenant")
	})
	if err = repo.ConfigureVPCCIDRPresets([]string{"10.42.0.0/16"}); err != nil {
		t.Fatal(err)
	}
	n, err := biz.NewNetwork(repo, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	var original biz.VPC
	for index, r := range input.Rows {
		v, err := n.CreateVPC(ctx, biz.CreateVPC{TenantID: r.TenantID, Name: "existing", CIDR: "10.42.0.0/16", IdempotencyKey: uuid.NewString()})
		if err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			original = v
		}
	}
	// Prepare two pre-existing accepted resources; the tested operations below
	// still call the real Network repository and its transactions.
	if _, err = f.Owner.Exec(ctx, `UPDATE network_operations SET state='succeeded',completed_at=clock_timestamp();UPDATE network_vpcs SET state='available';ALTER TABLE network_tenant_namespaces DROP CONSTRAINT network_tenant_namespaces_uid_nonempty;ALTER TABLE network_tenant_namespaces DROP COLUMN namespace_uid;DELETE FROM network_schema_version WHERE version=25`); err != nil {
		t.Fatal(err)
	}
	return &uidWindowFixture{f: f, owner: owner, repo: repo, input: input, vpc: original}
}
func (f *uidWindowFixture) plan(t *testing.T) data.NamespaceUIDWindowPlan {
	t.Helper()
	p, err := data.PlanNamespaceUIDWindow(context.Background(), f.owner, f.input)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func (f *uidWindowFixture) apply(t *testing.T, p data.NamespaceUIDWindowPlan) {
	t.Helper()
	_, err := data.ApplyNamespaceUIDWindow(context.Background(), f.owner, f.input, p.SHA256, func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
}
func (f *uidWindowFixture) guard(t *testing.T) string {
	t.Helper()
	var value string
	err := f.owner.QueryRow(context.Background(), `SELECT coalesce((SELECT pg_get_expr(conbin,conrelid) FROM pg_constraint WHERE conrelid='network_tenant_namespaces'::regclass AND conname=$1),'')`, data.NamespaceUIDWindowConstraint).Scan(&value)
	if err != nil {
		t.Fatal(err)
	}
	return value
}
func requireWindowCheck(t *testing.T, err error) {
	t.Helper()
	var pgerr *pgconn.PgError
	if !errors.As(err, &pgerr) || pgerr.Code != "23514" || pgerr.ConstraintName != data.NamespaceUIDWindowConstraint {
		t.Fatalf("want exact maintenance CHECK rejection, got %v", err)
	}
}

func TestNamespaceUIDWindowOldConflictWritesAndPreservedOperations(t *testing.T) {
	f := newUIDWindowFixture(t)
	ctx := context.Background()
	row := f.input.Rows[0]
	old := `INSERT INTO network_tenant_namespaces(tenant_id,cluster_id,namespace) VALUES($1,$2,$3) ON CONFLICT(tenant_id,cluster_id) DO NOTHING`
	runtime, err := pgxpool.New(ctx, f.f.RuntimeDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if _, err = runtime.Exec(ctx, old, row.TenantID, f.input.ClusterID, row.Namespace); err != nil {
		t.Fatal("original existing-row conflict must work before freeze", err)
	}
	unknown := f.input
	unknown.WriterAddresses = []string{"192.0.2.123"}
	if _, err = data.PlanNamespaceUIDWindow(ctx, f.owner, unknown); err == nil || !strings.Contains(err.Error(), "outside the exact original Pod addresses") {
		t.Fatalf("unregistered writer source accepted: %v", err)
	}
	freeze := f.plan(t)
	if f.guard(t) != "" {
		t.Fatal("read-only plan wrote a constraint")
	}
	f.apply(t, freeze)
	_, err = runtime.Exec(ctx, old, row.TenantID, f.input.ClusterID, row.Namespace)
	requireWindowCheck(t, err)
	_, err = runtime.Exec(ctx, `UPDATE network_tenant_namespaces SET namespace=namespace WHERE tenant_id=$1 AND cluster_id=$2`, row.TenantID, f.input.ClusterID)
	requireWindowCheck(t, err)
	if _, err = runtime.Exec(ctx, old, row.TenantID, "unrelated-cluster", row.Namespace); err != nil {
		t.Fatal("unrelated cluster blocked", err)
	}
	if _, err = f.repo.GetVPC(ctx, row.TenantID, f.vpc.ID); err != nil {
		t.Fatal("original read blocked", err)
	}
	deleted, err := f.repo.DeleteVPC(ctx, row.TenantID, f.vpc.ID)
	if err != nil || deleted.State != biz.Deleting {
		t.Fatalf("existing non-identity delete admission blocked: %+v %v", deleted, err)
	}
	if err = data.Migrate(ctx, f.f.Owner, f.f.RuntimeRole); err != nil {
		t.Fatal(err)
	}
	if f.guard(t) == "" {
		t.Fatal("official migration removed freeze")
	}
	f.input.Stage = "require-uid"
	requirePlan := f.plan(t)
	// A failure in the replacement ADD must roll back its preceding DROP.
	_, err = f.owner.Exec(ctx, `CREATE FUNCTION reject_uid_window_add() RETURNS event_trigger LANGUAGE plpgsql AS $$ BEGIN IF EXISTS(SELECT 1 FROM pg_constraint WHERE conname='network_tenant_namespaces_uid_cutover' AND pg_get_expr(conbin,conrelid) LIKE '%IS NOT NULL%') THEN RAISE EXCEPTION 'injected ADD failure';END IF;END $$;CREATE EVENT TRIGGER reject_uid_window_add ON ddl_command_end WHEN TAG IN ('ALTER TABLE') EXECUTE FUNCTION reject_uid_window_add()`)
	if err != nil {
		t.Fatal(err)
	}
	before := f.guard(t)
	_, err = data.ApplyNamespaceUIDWindow(ctx, f.owner, f.input, requirePlan.SHA256, func(context.Context) error { return nil })
	if err == nil || f.guard(t) != before {
		t.Fatalf("replacement error lost original freeze: %v", err)
	}
	if _, err = f.owner.Exec(ctx, `DROP EVENT TRIGGER reject_uid_window_add;DROP FUNCTION reject_uid_window_add()`); err != nil {
		t.Fatal(err)
	}
	f.apply(t, requirePlan)
	_, err = runtime.Exec(ctx, old, row.TenantID, f.input.ClusterID, row.Namespace)
	requireWindowCheck(t, err)
	_, err = runtime.Exec(ctx, `INSERT INTO network_tenant_namespaces(tenant_id,cluster_id,namespace,namespace_uid) VALUES($1,$2,$3,$4) ON CONFLICT(tenant_id,cluster_id) DO NOTHING`, row.TenantID, f.input.ClusterID, row.Namespace, row.UID)
	if err != nil {
		t.Fatal("new non-NULL writer blocked", err)
	}
	plan, err := f.repo.PlanNamespaceUIDBackfill(ctx)
	if err != nil || plan.NullCount != 2 {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	after, err := f.repo.ApplyNamespaceUIDBackfillGuarded(ctx, plan.SHA256, f.input)
	if err != nil || after.NullCount != 0 {
		t.Fatalf("backfill=%+v err=%v", after, err)
	}
	for _, r := range f.input.Rows {
		var uid string
		if err = f.owner.QueryRow(ctx, `SELECT namespace_uid FROM network_tenant_namespaces WHERE tenant_id=$1 AND cluster_id=$2`, r.TenantID, f.input.ClusterID).Scan(&uid); err != nil || uid != r.UID {
			t.Fatalf("original UID differs: %s %v", uid, err)
		}
	}
	var other *string
	if err = f.owner.QueryRow(ctx, `SELECT namespace_uid FROM network_tenant_namespaces WHERE cluster_id='unrelated-cluster'`).Scan(&other); err != nil || other != nil {
		t.Fatalf("unrelated cluster changed: %v %v", other, err)
	}
	f.input.Stage = "release"
	f.input.OldBackends = freeze.Before.OldBackends
	f.input.WriterProofSHA256 = strings.Repeat("a", 64)
	if len(f.input.OldBackends) == 0 {
		t.Fatal("fixture did not capture original runtime backend identities")
	}
	if _, err = data.PlanNamespaceUIDWindow(ctx, f.owner, f.input); err == nil {
		t.Fatal("original backends were not retired")
	}
	withoutCaptured := f.input
	withoutCaptured.OldBackends = nil
	if _, err = data.PlanNamespaceUIDWindow(ctx, f.owner, withoutCaptured); err == nil || !strings.Contains(err.Error(), "original Pod database client address is still active") {
		t.Fatalf("late original Pod writer bypassed capture: %v", err)
	}
	runtime.Close()
	f.repo.Close()
	f.f.Repository.Close()
	var release data.NamespaceUIDWindowPlan
	deadline := time.Now().Add(2 * time.Second)
	for {
		release, err = data.PlanNamespaceUIDWindow(ctx, f.owner, f.input)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	_, err = data.ApplyNamespaceUIDWindow(ctx, f.owner, f.input, release.SHA256, func(context.Context) error { return errors.New("old pod or new image not proven") })
	if err == nil || f.guard(t) == "" {
		t.Fatal("unverified release dropped guard")
	}
	f.apply(t, release)
	if f.guard(t) != "" {
		t.Fatal("final schema retained temporary constraint")
	}
	t.Log("UID_WINDOW_CYCLE PASS: old existing-row ON CONFLICT23514; UPDATE23514; reads/original Delete/unrelated cluster preserve; official25; atomic ADD failure rollback; exact2 backfill; old backend/identity release guard; final temporary constraint absent")
}

func TestNamespaceUIDWindowWaitsForInflightTransactionAndRejectsRowDrift(t *testing.T) {
	f := newUIDWindowFixture(t)
	ctx := context.Background()
	plan := f.plan(t)
	runtime, err := pgxpool.New(ctx, f.f.RuntimeDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	tx, err := runtime.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	const tenant = "33333333-3333-4333-8333-333333333333"
	if _, err = tx.Exec(ctx, `INSERT INTO network_tenant_namespaces(tenant_id,cluster_id,namespace) VALUES($1,$2,$3)`, tenant, f.input.ClusterID, "tenant-"+tenant); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, e := data.ApplyNamespaceUIDWindow(ctx, f.owner, f.input, plan.SHA256, func(context.Context) error { return nil })
		done <- e
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		var waiting bool
		if err = f.f.Owner.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE relation='network_tenant_namespaces'::regclass AND mode='AccessExclusiveLock' AND NOT granted)`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("DDL did not wait for original writer")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err = <-done:
		t.Fatalf("did not wait for in-flight writer: %v", err)
	default:
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err == nil {
		t.Fatal("late identity row was silently adopted")
	}
	if f.guard(t) != "" {
		t.Fatal("failed initial freeze partially installed a guard")
	}
	var version int
	if err = f.owner.QueryRow(ctx, `SELECT max(version) FROM network_schema_version`).Scan(&version); err != nil || version != 24 {
		t.Fatalf("failed freeze migrated schema: %d %v", version, err)
	}
	t.Log("UID_WINDOW_RACE PASS: ACCESS EXCLUSIVE waits original writer; changed exact row set rejects whole initial freeze; migration remains24")
}

func TestNamespaceUIDWindowFailureLeavesOriginalBarrier(t *testing.T) {
	f := newUIDWindowFixture(t)
	ctx := context.Background()
	freeze := f.plan(t)
	f.apply(t, freeze)
	if err := data.Migrate(ctx, f.f.Owner, f.f.RuntimeRole); err != nil {
		t.Fatal(err)
	}
	f.input.Stage = "require-uid"
	p := f.plan(t)
	original := f.guard(t)
	unsafe := f.input
	unsafe.RuntimeRole = f.f.OwnerRole
	if _, err := data.PlanNamespaceUIDWindow(ctx, f.owner, unsafe); err == nil {
		t.Fatal("table owner cannot be accepted as runtime role")
	}
	if _, err := data.ApplyNamespaceUIDWindow(ctx, f.owner, f.input, "wrong", func(context.Context) error { return nil }); err == nil {
		t.Fatal("unreviewed SHA accepted")
	}
	var calls atomic.Int32
	if _, err := data.ApplyNamespaceUIDWindow(ctx, f.owner, f.input, p.SHA256, func(context.Context) error {
		if calls.Add(1) == 2 {
			return errors.New("Namespace changed under lock")
		}
		return nil
	}); err == nil {
		t.Fatal("under-lock identity failure ignored")
	}
	cancelled, stop := context.WithCancel(ctx)
	stop()
	if _, err := data.ApplyNamespaceUIDWindow(cancelled, f.owner, f.input, p.SHA256, func(ctx context.Context) error { return ctx.Err() }); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel ignored: %v", err)
	}
	if f.guard(t) != original {
		t.Fatal("failed transition removed original barrier")
	}
	f.apply(t, p)
	f.input.Stage = "release"
	f.input.WriterProofSHA256 = strings.Repeat("a", 64)
	if _, err := data.PlanNamespaceUIDWindow(ctx, f.owner, f.input); err == nil {
		t.Fatal("release accepted NULL identity rows")
	}
	if !strings.Contains(f.guard(t), "IS NOT NULL") {
		t.Fatal("NULL refusal removed the require-UID barrier")
	}
	t.Log("UID_WINDOW_FAILURE PASS: wrong SHA, under-lock external identity failure, cancellation and NULL release refusal keep original committed guard")
}
