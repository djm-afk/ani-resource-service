package data_test

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5/pgconn"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/tenantnamespace"
	"github.com/zhangzhe-ctrl/ani-resource-service/tests/testenv"
	"strings"
)

func database(t *testing.T) (*data.Postgres, *pgxpool.Pool) {
	t.Helper()
	fixture := testenv.NewDatabase(t)
	// Older repository/worker tests now supply an explicit startup policy.
	// Admission, locking and SQL still run through the production implementation;
	// NewDatabase itself remains closed until the fixture publishes its policy.
	configureRepositoryVPCPresets(t, fixture.Repository)
	fixture.Repository.ConfigureTenantNamespaceResolver(func(_ context.Context, tenant string) (tenantnamespace.Identity, error) {
		return tenantnamespace.Identity{TenantID: tenant, ClusterID: "test-cluster", Name: "tenant-" + tenant, UID: "namespace-" + tenant}, nil
	})
	return fixture.Repository, fixture.Owner
}

func configureRepositoryVPCPresets(t *testing.T, repository *data.Postgres) {
	t.Helper()
	if err := repository.ConfigureVPCCIDRPresets([]string{"10.0.0.0/16", "10.42.0.0/16"}); err != nil {
		t.Fatal(err)
	}
}

func TestVPCAcceptanceIsTenantScopedAndReplaysExactly(t *testing.T) {
	repository, owner := database(t)
	network, err := biz.NewNetwork(repository, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	input := biz.CreateVPC{
		TenantID: "7a7750cf-73b0-49c5-a3b1-4dba42689401",
		Name:     "研发", CIDR: "10.42.0.0/16", Description: "私有网络", IdempotencyKey: "first",
	}
	input.Attribution = biz.Attribution{Actor: "first-actor", DirectCaller: "gateway"}
	accepted, err := network.CreateVPC(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if accepted.State != biz.Provisioning || accepted.LastOperationID == "" || accepted.Version != 1 {
		t.Fatalf("not a durable acceptance: %+v", accepted)
	}
	got, err := network.GetVPC(ctx, input.TenantID, accepted.ID)
	if err != nil || got.ID != accepted.ID || got.Description != "私有网络" {
		t.Fatalf("accepted VPC not retrievable: %+v, %v", got, err)
	}
	input.Attribution.Actor = "different-actor"
	replay, err := network.CreateVPC(ctx, input)
	if err != nil || replay.ID != accepted.ID || replay.LastOperationID != accepted.LastOperationID || !replay.CreatedAt.Equal(accepted.CreatedAt) {
		t.Fatalf("replay changed durable identity: %+v, %v", replay, err)
	}
	var actor string
	var verified bool
	if err := owner.QueryRow(ctx, "SELECT actor_ref,identity_verified FROM network_resource_history WHERE tenant_id=$1 AND vpc_id=$2 AND event='create_accepted'", input.TenantID, accepted.ID).Scan(&actor, &verified); err != nil {
		t.Fatal(err)
	}
	if actor != "first-actor" || verified {
		t.Fatal("replay overwrote original unverified attribution")
	}
	input.CIDR = "10.43.0.0/16"
	if _, err := network.CreateVPC(ctx, input); biz.ReasonOf(err) != biz.IdempotencyConflict {
		t.Fatalf("changed intent with same key: %v", err)
	}
	if _, err := network.GetVPC(ctx, "261c1986-c745-4a51-9faa-95067b798ba4", accepted.ID); biz.ReasonOf(err) != biz.ResourceNotFound {
		t.Fatalf("cross-tenant read: %v", err)
	}
	op, err := network.GetOperation(ctx, input.TenantID, accepted.LastOperationID)
	if err != nil || op.State != biz.Queued || op.ResourceID != accepted.ID {
		t.Fatalf("missing durable operation: %+v, %v", op, err)
	}
}

func TestVPCPagesRemainScopedAndCursorCannotChangeFilters(t *testing.T) {
	repository, _ := database(t)
	network, err := biz.NewNetwork(repository, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tenant := "7a7750cf-73b0-49c5-a3b1-4dba42689401"
	for _, name := range []string{"alpha", "beta", "gamma"} {
		if _, err := network.CreateVPC(ctx, biz.CreateVPC{TenantID: tenant, Name: name, CIDR: "10.42.0.0/16", IdempotencyKey: name}); err != nil {
			t.Fatal(err)
		}
	}
	first, err := network.ListVPCs(ctx, biz.ListVPCs{TenantID: tenant, Limit: 2})
	if err != nil || len(first.Items) != 2 || first.NextCursor == "" {
		t.Fatalf("first page: %+v, %v", first, err)
	}
	second, err := network.ListVPCs(ctx, biz.ListVPCs{TenantID: tenant, Limit: 2, Cursor: first.NextCursor})
	if err != nil || len(second.Items) != 1 || second.NextCursor != "" {
		t.Fatalf("second page: %+v, %v", second, err)
	}
	if first.Items[0].Name != "gamma" || first.Items[1].Name != "beta" || second.Items[0].Name != "alpha" {
		t.Fatalf("wrong ordering: %+v %+v", first, second)
	}
	for _, request := range []biz.ListVPCs{
		{TenantID: tenant, Name: "alpha", Cursor: first.NextCursor},
		{TenantID: "261c1986-c745-4a51-9faa-95067b798ba4", Cursor: first.NextCursor},
		{TenantID: tenant, Cursor: first.NextCursor + "x"},
	} {
		if _, err := network.ListVPCs(ctx, request); biz.ReasonOf(err) != biz.InvalidCursor {
			t.Fatalf("invalid cursor scope was accepted: %v", err)
		}
	}
}

func TestPostgresRejectsPartialAcceptanceAndCrossTenantReferences(t *testing.T) {
	repository, owner := database(t)
	n := newNetwork(t, repository, time.Minute)
	ctx := context.Background()
	// Fault injection runs as migration owner; behavior still crosses the public
	// use case using the restricted runtime role and the real T1 transaction.
	if _, err := owner.Exec(ctx, `CREATE FUNCTION reject_history() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN RAISE EXCEPTION 'injected acceptance failure'; END$$;
 CREATE TRIGGER reject_history BEFORE INSERT ON network_resource_history FOR EACH ROW EXECUTE FUNCTION reject_history()`); err != nil {
		t.Fatal(err)
	}
	input := biz.CreateVPC{TenantID: uuid.NewString(), Name: "atomic", CIDR: "10.42.0.0/16", IdempotencyKey: "atomic"}
	if _, err := n.CreateVPC(ctx, input); biz.ReasonOf(err) != biz.DependencyUnavailable {
		t.Fatalf("T1 failure not returned: %v", err)
	}
	for _, table := range []string{"network_vpcs", "network_operations", "network_reconciliations", "network_provider_bindings", "network_idempotency", "network_resource_history"} {
		var count int
		if err := owner.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("half acceptance in %s", table)
		}
	}
	if _, err := owner.Exec(ctx, "DROP TRIGGER reject_history ON network_resource_history"); err != nil {
		t.Fatal(err)
	}
	accepted, err := n.CreateVPC(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT INTO network_reconciliations(tenant_id,vpc_id,next_run_at) VALUES($1,$2,now())`,
		`INSERT INTO network_provider_bindings(tenant_id,vpc_id,binding_id,cluster_id,namespace,provider_name) VALUES($1,$2,gen_random_uuid(),'other','other','other')`,
		`INSERT INTO network_resource_history(tenant_id,history_id,vpc_id,event,resource_state,created_at) VALUES($1,gen_random_uuid(),$2,'bad','provisioning',now())`,
	} {
		if _, err := owner.Exec(ctx, statement, uuid.NewString(), accepted.ID); err == nil {
			t.Fatalf("cross-tenant FK accepted: %s", statement)
		} else {
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
				t.Fatalf("not a foreign-key rejection: %v", err)
			}
		}
	}
	other := uuid.NewString()
	if _, err := n.GetOperation(ctx, other, accepted.LastOperationID); biz.ReasonOf(err) != biz.ResourceNotFound {
		t.Fatalf("cross-tenant operation exposed: %v", err)
	}
	if _, err := n.DeleteVPC(ctx, other, accepted.ID); biz.ReasonOf(err) != biz.ResourceNotFound {
		t.Fatalf("cross-tenant delete exposed: %v", err)
	}
	page, err := n.ListVPCs(ctx, biz.ListVPCs{TenantID: other})
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("cross-tenant list exposed: %+v %v", page, err)
	}
}

func TestRuntimeRoleCannotOwnOrElevateIntoSchemaOwner(t *testing.T) {
	fixture := testenv.NewDatabase(t)
	repository, owner := fixture.Repository, fixture.Owner
	runtime, err := pgxpool.New(context.Background(), fixture.RuntimeDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	for _, statement := range []string{"CREATE TABLE public.unsafe(id int)", "CREATE TEMP TABLE unsafe(id int)", "ALTER TABLE network_vpcs ADD COLUMN unsafe int"} {
		_, err := runtime.Exec(context.Background(), statement)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Fatalf("runtime DDL was not denied: %v", err)
		}
	}
	ctx := context.Background()
	if err := repository.CheckReady(ctx); err != nil {
		t.Fatal(err)
	}
	// This grant simulates a misconfigured runtime identity, not a new supported
	// privilege mode. CheckReady must reject inherited/SET ROLE owner authority.
	dsn := os.Getenv("NETWORK_TEST_ADMIN_DSN")
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	ownerRole := owner.Config().ConnConfig.User
	runtimeRole := "network_runtime_" + strings.TrimPrefix(ownerRole, "network_owner_")
	if _, err := admin.Exec(ctx, "GRANT "+pgx.Identifier{ownerRole}.Sanitize()+" TO "+pgx.Identifier{runtimeRole}.Sanitize()+" WITH INHERIT FALSE"); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := admin.Exec(ctx, "REVOKE "+pgx.Identifier{ownerRole}.Sanitize()+" FROM "+pgx.Identifier{runtimeRole}.Sanitize()); err != nil {
			t.Error(err)
		}
	}()
	if err := repository.CheckReady(ctx); err == nil {
		t.Fatal("runtime can SET ROLE into migration owner but readiness passed")
	}
	var enabled int
	if err := owner.QueryRow(ctx, `SELECT count(*) FROM pg_class WHERE relname LIKE 'network_%' AND (relrowsecurity OR relforcerowsecurity)`).Scan(&enabled); err != nil || enabled != 0 {
		t.Fatalf("RLS enabled: %d %v", enabled, err)
	}
}
