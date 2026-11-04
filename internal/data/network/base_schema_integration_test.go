package data_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/migrations"
	"github.com/zhangzhe-ctrl/ani-resource-service/tests/testenv"
)

func schemaResourceID(kind string) string {
	return kind + "_" + strings.ReplaceAll(uuid.NewString(), "-", "")
}
func schemaSQLState(err error) string {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Code
	}
	return ""
}

func TestBaseSchemaEmptyMigrationAndRuntimeRole(t *testing.T) {
	f := testenv.NewDatabase(t)
	ctx := context.Background()
	files, err := fs.Glob(migrations.Files, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	var versions int
	if err = f.Owner.QueryRow(ctx, `SELECT count(*) FROM network_schema_version`).Scan(&versions); err != nil || versions != len(files) {
		t.Fatal("incomplete empty migration", versions, err)
	}
	runtime, err := pgxpool.New(ctx, f.RuntimeDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	for _, table := range []string{"network_default_intranet_pools", "network_eip_claims", "network_load_balancers", "network_lb_vip_intents", "network_vpc_base_connectivity", "network_base_backfill_candidates"} {
		var allowed, unsafe bool
		err = runtime.QueryRow(ctx, `SELECT has_table_privilege(current_user,$1::text,'SELECT') AND has_table_privilege(current_user,$1::text,'INSERT') AND has_table_privilege(current_user,$1::text,'UPDATE') AND NOT has_table_privilege(current_user,$1::text,'DELETE'),c.relrowsecurity OR c.relforcerowsecurity OR pg_has_role(current_user,c.relowner,'SET') FROM pg_class c WHERE c.oid=$1::regclass`, table).Scan(&allowed, &unsafe)
		if err != nil || !allowed || unsafe {
			t.Fatal("new table role contract", table, allowed, unsafe, err)
		}
	}
	if err = f.Repository.CheckReady(ctx); err != nil {
		t.Fatal(err)
	}
}

type legacyPublicFixture struct {
	Tenant, VPC, EIP, Snat string
	EIPIntent, SnatIntent  biz.EgressIntent
}

// This is a schema-0005 fixture, not product admission through schema-0006.
func seedSchema5Public(t *testing.T, owner *pgxpool.Pool) legacyPublicFixture {
	t.Helper()
	ctx := context.Background()
	v := legacyPublicFixture{Tenant: uuid.NewString(), VPC: schemaResourceID("vpc"), EIP: schemaResourceID("eip"), Snat: schemaResourceID("snat")}
	v.EIPIntent = biz.EgressIntent{TenantID: v.Tenant, Kind: "create_eip", Name: "legacy-eip", Description: "preserved", IdempotencyKey: "legacy-eip"}
	v.SnatIntent = biz.EgressIntent{TenantID: v.Tenant, Kind: "bind_snat", VPCID: v.VPC, EIPID: v.EIP, Enabled: true, IdempotencyKey: "legacy-snat"}
	tx, err := owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := tx.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`CREATE TABLE network_schema_version(version integer PRIMARY KEY,checksum text NOT NULL,applied_at timestamptz NOT NULL DEFAULT clock_timestamp())`)
	files, err := fs.Glob(migrations.Files, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	for i, name := range files[:5] {
		body, err := migrations.Files.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		exec(string(body))
		sum := sha256.Sum256(body)
		exec(`INSERT INTO network_schema_version(version,checksum) VALUES($1,$2)`, i+1, hex.EncodeToString(sum[:]))
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	vo, eo, so, goID, po := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	gw, poolID := schemaResourceID("egw"), schemaResourceID("pool")
	exec(`INSERT INTO network_tenant_namespaces(tenant_id,cluster_id,namespace) VALUES($1,'test-cluster',$2)`, v.Tenant, "tenant-"+v.Tenant)
	exec(`INSERT INTO network_vpcs(tenant_id,vpc_id,name,description,cidr,state,version,created_at,updated_at,observed_at,last_operation_id) VALUES($1,$2,'legacy','preserved','10.42.0.0/16','available',7,$3,$3,$3,$4)`, v.Tenant, v.VPC, now, vo)
	exec(`INSERT INTO network_operations(tenant_id,vpc_id,operation_id,kind,state,created_at,updated_at,completed_at) VALUES($1,$2,$3,'create_vpc','succeeded',$4,$4,$4)`, v.Tenant, v.VPC, vo, now)
	exec(`INSERT INTO network_provider_bindings(tenant_id,vpc_id,resource_kind,binding_id,cluster_id,namespace,provider_name,provider_uid) VALUES($1,$2,'vpc',gen_random_uuid(),'test-cluster',$3,$2,'legacy-vpc-uid')`, v.Tenant, v.VPC, "tenant-"+v.Tenant)
	for _, r := range []struct{ id, kind, op, opKind string }{{gw, "egress_gateway", goID, "create_egress_gateway"}, {poolID, "public_pool", po, "create_public_pool"}} {
		exec(`INSERT INTO network_platform_resources(resource_id,kind,cluster_id,name,state,created_at,updated_at,observed_at,last_operation_id,provider_name,provider_uid,binding_id) VALUES($1,$2,'test-cluster','legacy','available',$3,$3,$3,$4,$1,$5,gen_random_uuid())`, r.id, r.kind, now, r.op, "legacy-"+r.kind+"-uid")
		exec(`INSERT INTO network_platform_operations(operation_id,resource_id,kind,state,created_at,updated_at,completed_at) VALUES($1,$2,$3,'succeeded',$4,$4,$4)`, r.op, r.id, r.opKind, now)
	}
	exec(`INSERT INTO network_egress_gateways(resource_id,cluster_id) VALUES($1,'test-cluster')`, gw)
	exec(`INSERT INTO network_public_pools(resource_id,cluster_id,mode,gateway_id,cidr,ovn_gateway_ip,excluded_ips) VALUES($1,'test-cluster','overlay',$2,'192.0.2.0/24','192.0.2.1',ARRAY['192.0.2.1'])`, poolID, gw)
	exec(`INSERT INTO network_eips(tenant_id,eip_id,cluster_id,namespace,name,description,pool_id,pool_revision,address,state,version,created_at,updated_at,observed_at,last_operation_id) VALUES($1,$2,'test-cluster',$3,'legacy-eip','preserved',$4,1,'192.0.2.10','available',9,$5,$5,$5,$6)`, v.Tenant, v.EIP, "tenant-"+v.Tenant, poolID, now, eo)
	exec(`INSERT INTO network_snat_bindings(tenant_id,snat_id,cluster_id,namespace,name,vpc_id,eip_id,desired_enabled,applied_enabled,target_generation,state,version,created_at,updated_at,observed_at,last_operation_id) VALUES($1,$2,'test-cluster',$3,'legacy-snat',$4,$5,true,true,3,'available',11,$6,$6,$6,$7)`, v.Tenant, v.Snat, "tenant-"+v.Tenant, v.VPC, v.EIP, now, so)
	eipSnapshot, _ := json.Marshal(biz.EIP{EgressMetadata: biz.EgressMetadata{ID: v.EIP, Name: "legacy-eip", Description: "preserved", State: biz.Provisioning, Version: 1, CreatedAt: now, UpdatedAt: now, LastOperationID: eo, ObservationStale: true}, TenantID: v.Tenant, BindingState: "unbound"})
	snatSnapshot, _ := json.Marshal(biz.VPCSnatBinding{EgressMetadata: biz.EgressMetadata{ID: v.Snat, State: biz.Provisioning, Version: 1, CreatedAt: now, UpdatedAt: now, LastOperationID: so, ObservationStale: true}, TenantID: v.Tenant, VPCID: v.VPC, EIPID: v.EIP, DesiredEnabled: true})
	for _, r := range []struct {
		id, kind, op, opKind, key, fingerprint string
		snapshot                               []byte
	}{{v.EIP, "eip", eo, "create_eip", "legacy-eip", v.EIPIntent.Fingerprint(), eipSnapshot}, {v.Snat, "snat", so, "bind_snat", "legacy-snat", v.SnatIntent.Fingerprint(), snatSnapshot}} {
		exec(`INSERT INTO network_operations(tenant_id,`+r.kind+`_id,operation_id,kind,state,attempt,execution_epoch,created_at,updated_at,completed_at) VALUES($1,$2,$3,$4,'succeeded',3,5,$5,$5,$5)`, v.Tenant, r.id, r.op, r.opKind, now)
		exec(`INSERT INTO network_provider_bindings(tenant_id,`+r.kind+`_id,resource_kind,binding_id,cluster_id,namespace,provider_name,provider_uid) VALUES($1,$2,$3,gen_random_uuid(),'test-cluster',$4,$2,$5)`, v.Tenant, r.id, r.kind, "tenant-"+v.Tenant, "legacy-"+r.kind+"-uid")
		exec(`INSERT INTO network_idempotency(tenant_id,operation_kind,idempotency_key,fingerprint,fingerprint_version,`+r.kind+`_id,operation_id,response,created_at) VALUES($1,$2,$3,$4,1,$5,$6,$7,$8)`, v.Tenant, r.opKind, r.key, r.fingerprint, r.id, r.op, r.snapshot, now)
		exec(`INSERT INTO network_reconciliations(tenant_id,`+r.kind+`_id,next_run_at,lease_owner,lease_until,lease_epoch) VALUES($1,$2,$3::timestamptz,gen_random_uuid(),$3::timestamptz+interval '1 hour',13)`, v.Tenant, r.id, now)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestBaseSchemaLegacyPublicUpgradePreservesDurableFactsAndReplay(t *testing.T) {
	ctx := context.Background()
	before := map[string]string{}
	var legacy legacyPublicFixture
	tables := []string{"network_vpcs", "network_eips", "network_snat_bindings", "network_provider_bindings", "network_operations", "network_idempotency", "network_reconciliations"}
	snapshot := func(owner *pgxpool.Pool, table string) string {
		t.Helper()
		var s string
		err := owner.QueryRow(ctx, `SELECT coalesce(jsonb_agg(to_jsonb(r)-'evidence_hash'-'evidence_applied_at'-'vnic_id'-'observation_error'-'target_generation'-'lease_owner'-'lease_until'-'lease_epoch'-'next_run_at'-'base_connectivity_required'-'scope'-'managed_by'-'system_owner_vpc'-'purpose'-'create_dispatched'-'retired'-'lb_id'-'provider_generation'-'provider_ready'-'provider_observed_at'-'provider_observed_uid'-'provider_observed_generation' ORDER BY to_jsonb(r)::text),'[]')::text FROM `+table+` r WHERE tenant_id=$1`, legacy.Tenant).Scan(&s)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	f := testenv.NewDatabase(t, func(owner *pgxpool.Pool, _ string) {
		legacy = seedSchema5Public(t, owner)
		for _, table := range tables {
			before[table] = snapshot(owner, table)
		}
	})
	for _, table := range tables {
		if after := snapshot(f.Owner, table); after != before[table] {
			t.Fatalf("migration changed legacy %s identity/history/snapshot/lease", table)
		}
	}
	// New LB target columns must remain NULL for every legacy target, while
	// the old column projection above remains byte-for-byte identical.
	for _, table := range []string{"network_provider_bindings", "network_operations", "network_idempotency", "network_reconciliations"} {
		var count int
		if err := f.Owner.QueryRow(ctx, "SELECT count(*) FROM "+table+" WHERE tenant_id=$1 AND lb_id IS NOT NULL", legacy.Tenant).Scan(&count); err != nil || count != 0 {
			t.Fatal("legacy row acquired an LB target", table, count, err)
		}
	}
	// New migration facts are checked independently from the byte-preserved
	// legacy columns: pre-upgrade dispatch history remains conservative, and
	// existing reconciliation work is never silently retired.
	var conservativeDispatch, activeReconciliation bool
	if err := f.Owner.QueryRow(ctx, `SELECT bool_and(create_dispatched) FROM network_provider_bindings WHERE tenant_id=$1`, legacy.Tenant).Scan(&conservativeDispatch); err != nil || !conservativeDispatch {
		t.Fatal("legacy provider dispatch history was treated as never sent", conservativeDispatch, err)
	}
	if err := f.Owner.QueryRow(ctx, `SELECT bool_and(NOT retired) FROM network_reconciliations WHERE tenant_id=$1`, legacy.Tenant).Scan(&activeReconciliation); err != nil || !activeReconciliation {
		t.Fatal("migration retired existing durable work", activeReconciliation, err)
	}
	var obsolete int
	if err := f.Owner.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_name='network_provider_bindings' AND column_name IN ('provider_ready','provider_observed_at','provider_observed_uid','provider_generation','provider_observed_generation')`).Scan(&obsolete); err != nil || obsolete != 0 {
		t.Fatal("obsolete provider proof columns remain", obsolete, err)
	}
	got, err := f.Repository.GetEIP(ctx, legacy.Tenant, legacy.EIP)
	if err != nil || got.Scope != "public" || got.ManagedBy != "tenant" || got.BindingID != legacy.Snat || got.BindingTarget == nil || got.BindingTarget.Kind != "vpc_snat" || got.BindingState != "bound" {
		t.Fatal("legacy claim not converted", got, err)
	}
	snat, err := f.Repository.GetSnat(ctx, legacy.Tenant, legacy.VPC, true)
	if err != nil || snat.ID != legacy.Snat || snat.Purpose != "public" {
		t.Fatal(snat, err)
	}
	replay, err := f.Repository.AcceptEIP(ctx, legacy.EIPIntent, biz.Attribution{})
	if err != nil || replay.ID != legacy.EIP || replay.State != biz.Provisioning || replay.Version != 1 {
		t.Fatal("permanent EIP receipt drifted", replay, err)
	}
	sr, err := f.Repository.AcceptSnat(ctx, legacy.SnatIntent, biz.Attribution{})
	if err != nil || sr.ID != legacy.Snat || sr.State != biz.Provisioning || sr.Version != 1 {
		t.Fatal("permanent SNAT receipt consulted new readiness", sr, err)
	}
	for _, table := range tables {
		if after := snapshot(f.Owner, table); after != before[table] {
			t.Fatalf("legacy replay mutated %s", table)
		}
	}
}

func TestBaseSchemaUnknownPublicProvenanceStopsWholeUpgrade(t *testing.T) {
	ctx := context.Background()
	testenv.NewDatabase(t, func(owner *pgxpool.Pool, role string) {
		legacy := seedSchema5Public(t, owner)
		var name, definition string
		if err := owner.QueryRow(ctx, `SELECT conname,pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid='network_eips'::regclass AND confrelid='network_public_pools'::regclass AND contype='f'`).Scan(&name, &definition); err != nil {
			t.Fatal(err)
		}
		if _, err := owner.Exec(ctx, `ALTER TABLE network_eips DROP CONSTRAINT `+pgx.Identifier{name}.Sanitize()); err != nil {
			t.Fatal(err)
		}
		if _, err := owner.Exec(ctx, `UPDATE network_eips SET pool_revision=987654 WHERE tenant_id=$1 AND eip_id=$2`, legacy.Tenant, legacy.EIP); err != nil {
			t.Fatal(err)
		}
		err := data.Migrate(ctx, owner, role)
		if err == nil || !strings.Contains(err.Error(), "NET_U01_PUBLIC_PROVENANCE_CONFLICT") || !strings.Contains(err.Error(), legacy.EIP) {
			t.Fatal("unknown provenance was accepted or lacked conflict identity", err)
		}
		var count int
		var newColumn bool
		if err = owner.QueryRow(ctx, `SELECT count(*) FROM network_schema_version`).Scan(&count); err != nil || count != 5 {
			t.Fatal("failed migration advanced ledger", count, err)
		}
		if err = owner.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_name='network_eips' AND column_name='scope')`).Scan(&newColumn); err != nil || newColumn {
			t.Fatal("failed migration partially assigned purpose", newColumn, err)
		}
		// Restore only this injected fixture corruption; NewDatabase subsequently
		// exercises the successful exact same migration and role readiness.
		if _, err = owner.Exec(ctx, `UPDATE network_eips SET pool_revision=1 WHERE tenant_id=$1 AND eip_id=$2`, legacy.Tenant, legacy.EIP); err != nil {
			t.Fatal(err)
		}
		if _, err = owner.Exec(ctx, `ALTER TABLE network_eips ADD CONSTRAINT `+pgx.Identifier{name}.Sanitize()+` `+definition); err != nil {
			t.Fatal(err)
		}
	})
}

func schemaInsertSnat(ctx context.Context, tx pgx.Tx, tenant, vpcID, eipID, snatID, purpose string) error {
	op := uuid.NewString()
	tag, err := tx.Exec(ctx, `INSERT INTO network_snat_bindings(tenant_id,snat_id,cluster_id,namespace,name,vpc_id,eip_id,purpose,desired_enabled,state,created_at,updated_at,last_operation_id)
 SELECT tenant_id,$2,cluster_id,namespace,'schema fixture',$3,eip_id,$4,true,'provisioning',clock_timestamp(),clock_timestamp(),$5 FROM network_eips WHERE tenant_id=$1 AND eip_id=$6`, tenant, snatID, vpcID, purpose, op, eipID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("missing fixture EIP")
	}
	_, err = tx.Exec(ctx, `INSERT INTO network_operations(tenant_id,operation_id,snat_id,kind,state,created_at,updated_at) VALUES($1,$2,$3,'bind_snat','queued',clock_timestamp(),clock_timestamp())`, tenant, op, snatID)
	return err
}

func schemaInsertLB(t *testing.T, f *egressFixture, vpcID, subnetID, eipID, address string) string {
	t.Helper()
	id := schemaResourceID("lb")
	exposure := "private"
	if eipID != "" {
		exposure = "public"
		if address != "" {
			exposure = "public_private"
		}
	}
	_, err := f.owner.Exec(f.ctx, `INSERT INTO network_load_balancers(tenant_id,lb_id,cluster_id,namespace,vpc_id,subnet_id,exposure,public_eip_id,private_ip,state)
 SELECT tenant_id,$2,cluster_id,namespace,vpc_id,$3,$4,NULLIF($5,''),NULLIF($6,''),'provisioning' FROM network_provider_bindings WHERE tenant_id=$1 AND vpc_id=$7`, f.tenant, id, subnetID, exposure, eipID, address, vpcID)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// Both writers are genuine open PostgreSQL transactions. The first remains
// uncommitted until pg_stat_activity proves the other is blocked on its lock.
// Running each target order tests either target winning without a scheduler
// lucky race or an in-process mutex standing in for database exclusion.
func schemaCompete(t *testing.T, pool *pgxpool.Pool, first, second func(context.Context, pgx.Tx) error) (error, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	a, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Rollback(context.Background())
	b, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Rollback(context.Background())
	var apid, bpid int
	if err = a.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&apid); err != nil {
		t.Fatal(err)
	}
	if err = b.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&bpid); err != nil || apid == bpid {
		t.Fatal("competition did not use two database sessions", apid, bpid, err)
	}
	if err = first(ctx, a); err != nil {
		t.Fatal("first candidate invalid", err)
	}
	done := make(chan error, 1)
	go func() {
		err := second(ctx, b)
		if err == nil {
			err = b.Commit(ctx)
		}
		done <- err
	}()
	locked := false
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var wait *string
		if err = pool.QueryRow(ctx, `SELECT wait_event_type FROM pg_stat_activity WHERE pid=$1`, bpid).Scan(&wait); err != nil {
			t.Fatal(err)
		}
		if wait != nil && *wait == "Lock" {
			locked = true
			break
		}
		select {
		case err := <-done:
			t.Fatalf("second transaction did not contend: %v", err)
		default:
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !locked {
		t.Fatal("second transaction never blocked on uncommitted conflicting claim")
	}
	firstErr := a.Commit(ctx)
	secondErr := <-done
	return firstErr, secondErr
}

func TestBaseSchemaOneIntranetOnePublicAndSamePurposeContention(t *testing.T) {
	f := newEgressFixture(t)
	v := availableVPC(t, f.n, f.w, f.tenant, "purpose-db")
	base := seedEgressBaseConnectivity(t, f, v)
	e1, e2 := f.eip(t, "purpose-public-1"), f.eip(t, "purpose-public-2")
	firstID, secondID := schemaResourceID("snat"), schemaResourceID("snat")
	a, b := schemaCompete(t, f.owner, func(ctx context.Context, tx pgx.Tx) error {
		return schemaInsertSnat(ctx, tx, f.tenant, v.ID, e1.ID, firstID, "public")
	}, func(ctx context.Context, tx pgx.Tx) error {
		return schemaInsertSnat(ctx, tx, f.tenant, v.ID, e2.ID, secondID, "public")
	})
	if a != nil || schemaSQLState(b) != "23505" {
		t.Fatal("same-purpose exclusion failed", a, b)
	}
	var intranet, public int
	if err := f.owner.QueryRow(f.ctx, `SELECT count(*) FILTER(WHERE purpose='intranet'),count(*) FILTER(WHERE purpose='public') FROM network_snat_bindings WHERE tenant_id=$1 AND vpc_id=$2 AND state<>'deleted'`, f.tenant, v.ID).Scan(&intranet, &public); err != nil || intranet != 1 || public != 1 {
		t.Fatal("one-per-purpose invariant", intranet, public, err)
	}
	for _, state := range []string{"degraded", "deleting", "failed"} {
		if _, err := f.owner.Exec(f.ctx, `UPDATE network_snat_bindings SET state=$3 WHERE tenant_id=$1 AND snat_id=$2`, f.tenant, firstID, state); err != nil {
			t.Fatal(err)
		}
		tx, err := f.owner.Begin(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = schemaInsertSnat(f.ctx, tx, f.tenant, v.ID, e2.ID, schemaResourceID("snat"), "public")
		tx.Rollback(f.ctx)
		if schemaSQLState(err) != "23505" {
			t.Fatal("nonterminal purpose slot released", state, err)
		}
	}
	var active int
	if err := f.owner.QueryRow(f.ctx, `SELECT count(*) FROM network_eip_claims WHERE tenant_id=$1 AND eip_id=$2 AND released_at IS NULL`, f.tenant, base.EIPID).Scan(&active); err != nil || active != 1 {
		t.Fatal("public contention changed intranet claim", active, err)
	}
}

func TestBaseSchemaSnatAndLBClaimContentionBothWinnerOrders(t *testing.T) {
	for _, snatFirst := range []bool{true, false} {
		t.Run(fmt.Sprint("snat-first-", snatFirst), func(t *testing.T) {
			f := newEgressFixture(t)
			v := availableVPC(t, f.n, f.w, f.tenant, "claim-db")
			e := f.eip(t, "claim-eip")
			subnet, err := f.n.CreateSubnet(f.ctx, biz.CreateSubnet{TenantID: f.tenant, VPCID: v.ID, Name: "claim-parent", CIDR: "10.42.1.0/24", IdempotencyKey: "claim-parent"})
			if err != nil {
				t.Fatal(err)
			}
			snatID := schemaResourceID("snat")
			tx, err := f.owner.Begin(f.ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err = schemaInsertSnat(f.ctx, tx, f.tenant, v.ID, e.ID, snatID, "public"); err != nil {
				tx.Rollback(f.ctx)
				t.Fatal(err)
			}
			if err = tx.Commit(f.ctx); err != nil {
				t.Fatal(err)
			}
			lbID := schemaInsertLB(t, f, v.ID, subnet.ID, e.ID, "")
			insert := func(kind, id string) func(context.Context, pgx.Tx) error {
				return func(ctx context.Context, tx pgx.Tx) error {
					column := "snat_id"
					if kind == "load_balancer" {
						column = "lb_id"
					}
					_, err := tx.Exec(ctx, `INSERT INTO network_eip_claims(tenant_id,eip_id,cluster_id,namespace,target_kind,`+column+`) SELECT tenant_id,eip_id,cluster_id,namespace,$3,$4 FROM network_eips WHERE tenant_id=$1 AND eip_id=$2`, f.tenant, e.ID, kind, id)
					return err
				}
			}
			first, second := insert("vpc_snat", snatID), insert("load_balancer", lbID)
			winner := "vpc_snat"
			if !snatFirst {
				first, second = second, first
				winner = "load_balancer"
			}
			a, b := schemaCompete(t, f.owner, first, second)
			if a != nil || schemaSQLState(b) != "23505" {
				t.Fatal("SNAT/LB did not share an atomic claim", a, b)
			}
			var count int
			var kind string
			if err = f.owner.QueryRow(f.ctx, `SELECT count(*),min(target_kind) FROM network_eip_claims WHERE tenant_id=$1 AND eip_id=$2 AND released_at IS NULL`, f.tenant, e.ID).Scan(&count, &kind); err != nil || count != 1 || kind != winner {
				t.Fatal("claim winner mismatch", count, kind, err)
			}
			for _, state := range []string{"reserved", "bound"} {
				if _, err = f.owner.Exec(f.ctx, `UPDATE network_eip_claims SET state=$3 WHERE tenant_id=$1 AND eip_id=$2`, f.tenant, e.ID, state); err != nil {
					t.Fatal(err)
				}
				if _, err = f.e.DeleteEIP(f.ctx, "", e.ID); biz.ReasonOf(err) != biz.EIPInUse {
					t.Fatal("address release ignored active unified claim", state, err)
				}
			}
		})
	}
}

func TestBaseSchemaTypedTenantPlacementAndVIPConstraints(t *testing.T) {
	f := newEgressFixture(t)
	v := availableVPC(t, f.n, f.w, f.tenant, "typed-db")
	base := seedEgressBaseConnectivity(t, f, v)
	e := f.eip(t, "typed-eip")
	s, err := f.n.CreateSubnet(f.ctx, biz.CreateSubnet{TenantID: f.tenant, VPCID: v.ID, Name: "typed-parent", CIDR: "10.42.1.0/24", IdempotencyKey: "typed-parent"})
	if err != nil {
		t.Fatal(err)
	}
	lb1 := schemaInsertLB(t, f, v.ID, s.ID, e.ID, "10.42.1.10")
	lb2 := schemaInsertLB(t, f, v.ID, s.ID, "", "10.42.1.10")
	if _, err = f.owner.Exec(f.ctx, `INSERT INTO network_eip_claims(tenant_id,cluster_id,namespace,eip_id,target_kind,lb_id) SELECT tenant_id,cluster_id,namespace,eip_id,'load_balancer',$3 FROM network_eips WHERE tenant_id=$1 AND eip_id=$2`, f.tenant, e.ID, lb1); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, statement, want string
		args                  []any
	}{
		{"snat-purpose", `UPDATE network_snat_bindings SET purpose='public' WHERE tenant_id=$1 AND snat_id=$2`, "23503", []any{f.tenant, base.SnatID}},
		{"snat-tenant", `UPDATE network_snat_bindings SET tenant_id=$3 WHERE tenant_id=$1 AND snat_id=$2`, "23503", []any{f.tenant, base.SnatID, uuid.NewString()}},
		{"snat-cluster", `UPDATE network_snat_bindings SET cluster_id=$3 WHERE tenant_id=$1 AND snat_id=$2`, "23503", []any{f.tenant, base.SnatID, "foreign-cluster"}},
		{"snat-namespace", `UPDATE network_snat_bindings SET namespace=$3 WHERE tenant_id=$1 AND snat_id=$2`, "23503", []any{f.tenant, base.SnatID, "foreign-namespace"}},
		{"snat-parent", `UPDATE network_snat_bindings SET vpc_id=$3 WHERE tenant_id=$1 AND snat_id=$2`, "23503", []any{f.tenant, base.SnatID, schemaResourceID("vpc")}},
		{"system-eip-management", `UPDATE network_eips SET managed_by='tenant' WHERE tenant_id=$1 AND eip_id=$2`, "23514", []any{f.tenant, base.EIPID}},
		{"system-eip-owner", `UPDATE network_eips SET system_owner_vpc=$3 WHERE tenant_id=$1 AND eip_id=$2`, "23503", []any{f.tenant, base.EIPID, schemaResourceID("vpc")}},
		{"claim-tenant", `UPDATE network_eip_claims SET tenant_id=$3 WHERE tenant_id=$1 AND eip_id=$2`, "23503", []any{f.tenant, e.ID, uuid.NewString()}},
		{"claim-cluster", `UPDATE network_eip_claims SET cluster_id=$3 WHERE tenant_id=$1 AND eip_id=$2`, "23503", []any{f.tenant, e.ID, "foreign-cluster"}},
		{"claim-namespace", `UPDATE network_eip_claims SET namespace=$3 WHERE tenant_id=$1 AND eip_id=$2`, "23503", []any{f.tenant, e.ID, "foreign-namespace"}},
		{"wrong-typed-target", `UPDATE network_eip_claims SET lb_id=$3 WHERE tenant_id=$1 AND eip_id=$2`, "23503", []any{f.tenant, e.ID, lb2}},
		{"discriminator", `UPDATE network_eip_claims SET target_kind='vpc_snat' WHERE tenant_id=$1 AND eip_id=$2`, "23514", []any{f.tenant, e.ID}},
		{"no-typed-target", `UPDATE network_eip_claims SET lb_id=NULL WHERE tenant_id=$1 AND eip_id=$2`, "23514", []any{f.tenant, e.ID}},
		{"lb-parent-tenant", `UPDATE network_load_balancers SET tenant_id=$3 WHERE tenant_id=$1 AND lb_id=$2`, "23503", []any{f.tenant, lb2, uuid.NewString()}},
		{"lb-parent-cluster", `UPDATE network_load_balancers SET cluster_id=$3 WHERE tenant_id=$1 AND lb_id=$2`, "23503", []any{f.tenant, lb2, "foreign-cluster"}},
		{"lb-parent-namespace", `UPDATE network_load_balancers SET namespace=$3 WHERE tenant_id=$1 AND lb_id=$2`, "23503", []any{f.tenant, lb2, "foreign-namespace"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := f.owner.Begin(f.ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(f.ctx)
			_, err = tx.Exec(f.ctx, tc.statement, tc.args...)
			if schemaSQLState(err) != tc.want {
				t.Fatal("tenant/typed FK accepted invalid relation", err)
			}
		})
	}
	vip := func(lbID string) func(context.Context, pgx.Tx) error {
		return func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO network_lb_vip_intents(tenant_id,lb_id,cluster_id,namespace,vpc_id,subnet_id,address) SELECT tenant_id,lb_id,cluster_id,namespace,vpc_id,subnet_id,private_ip FROM network_load_balancers WHERE tenant_id=$1 AND lb_id=$2`, f.tenant, lbID)
			return err
		}
	}
	a, b := schemaCompete(t, f.owner, vip(lb1), vip(lb2))
	if a != nil || schemaSQLState(b) != "23505" {
		t.Fatal("VIP contention accepted both intents", a, b)
	}
	for _, tc := range []struct{ column, value string }{{"tenant_id", uuid.NewString()}, {"cluster_id", "foreign-cluster"}, {"namespace", "foreign-namespace"}, {"address", "10.42.1.11"}, {"subnet_id", schemaResourceID("subnet")}} {
		tx, err := f.owner.Begin(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, err = tx.Exec(f.ctx, `UPDATE network_lb_vip_intents SET `+tc.column+`=$3 WHERE tenant_id=$1 AND lb_id=$2`, f.tenant, lb1, tc.value)
		tx.Rollback(f.ctx)
		if schemaSQLState(err) != "23503" {
			t.Fatal("VIP typed identity was not protected", tc.column, err)
		}
	}
	// Product admission consumes the same persisted LB identity; Provider LB
	// runtime is intentionally absent from this schema-only fixture.
	if _, err = f.n.DeleteSubnet(f.ctx, f.tenant, s.ID); biz.ReasonOf(err) != biz.ResourceInUse {
		t.Fatal("LB identity did not block parent Subnet deletion", err)
	}
	if _, err = f.owner.Exec(f.ctx, `UPDATE network_subnets SET state='deleted' WHERE tenant_id=$1 AND subnet_id=$2`, f.tenant, s.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = f.n.DeleteVPC(f.ctx, f.tenant, v.ID); biz.ReasonOf(err) != biz.ResourceInUse {
		t.Fatal("LB identity did not block parent VPC deletion", err)
	}
}

func TestBaseSchemaIntranetPurposeContention(t *testing.T) {
	f := newEgressFixture(t)
	v := availableVPC(t, f.n, f.w, f.tenant, "intranet-purpose-db")
	base := seedEgressBaseConnectivity(t, f, v)
	// The completed previous fixture generation frees its purpose slot. Both new
	// candidates are then inserted in independent transactions while the winner
	// remains uncommitted. No worker or Provider success is inferred from this.
	if _, err := f.owner.Exec(f.ctx, `UPDATE network_snat_bindings SET state='deleted' WHERE tenant_id=$1 AND snat_id=$2`, f.tenant, base.SnatID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.owner.Exec(f.ctx, `UPDATE network_eip_claims SET released_at=clock_timestamp() WHERE tenant_id=$1 AND eip_id=$2 AND released_at IS NULL`, f.tenant, base.EIPID); err != nil {
		t.Fatal(err)
	}
	secondEIP, op := schemaResourceID("eip"), uuid.NewString()
	tx, err := f.owner.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(f.ctx)
	if _, err = tx.Exec(f.ctx, `INSERT INTO network_eips(tenant_id,eip_id,cluster_id,namespace,name,pool_id,pool_revision,scope,managed_by,system_owner_vpc,state,created_at,updated_at,last_operation_id)
 SELECT tenant_id,$2,cluster_id,namespace,'second candidate',pool_id,pool_revision,scope,managed_by,system_owner_vpc,'provisioning',clock_timestamp(),clock_timestamp(),$3 FROM network_eips WHERE tenant_id=$1 AND eip_id=$4`, f.tenant, secondEIP, op, base.EIPID); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(f.ctx, `INSERT INTO network_operations(tenant_id,eip_id,operation_id,kind,state,created_at,updated_at) VALUES($1,$2,$3,'create_eip','queued',clock_timestamp(),clock_timestamp())`, f.tenant, secondEIP, op); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	a, b := schemaCompete(t, f.owner, func(ctx context.Context, tx pgx.Tx) error {
		return schemaInsertSnat(ctx, tx, f.tenant, v.ID, base.EIPID, schemaResourceID("snat"), "intranet")
	}, func(ctx context.Context, tx pgx.Tx) error {
		return schemaInsertSnat(ctx, tx, f.tenant, v.ID, secondEIP, schemaResourceID("snat"), "intranet")
	})
	if a != nil || schemaSQLState(b) != "23505" {
		t.Fatal("two Intranet candidates took the same VPC purpose", a, b)
	}
	var count int
	if err = f.owner.QueryRow(f.ctx, `SELECT count(*) FROM network_snat_bindings WHERE tenant_id=$1 AND vpc_id=$2 AND purpose='intranet' AND state<>'deleted'`, f.tenant, v.ID).Scan(&count); err != nil || count != 1 {
		t.Fatal("intranet purpose admitted more than one candidate", count, err)
	}
}
