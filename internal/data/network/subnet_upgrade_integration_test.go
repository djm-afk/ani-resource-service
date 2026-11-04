package data_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/migrations"
	"github.com/zhangzhe-ctrl/ani-resource-service/tests/testenv"
)

func TestSubnetMigrationUpgradesNET01WithoutChangingDurableFacts(t *testing.T) {
	tenant, opID, bindingID, historyID := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	vpcID := "vpc_12345678901234567890123456789012"
	now := time.Now().UTC().Truncate(time.Microsecond)
	intent, err := biz.NewVPCIntent(tenant, "legacy", "10.42.0.0/16", "preserved", "legacy-key")
	if err != nil {
		t.Fatal(err)
	}
	accepted := biz.VPC{ID: vpcID, TenantID: tenant, Name: intent.Name, CIDR: intent.CIDR, Description: intent.Description, State: biz.Provisioning, Version: 1, CreatedAt: now, UpdatedAt: now, ObservationStale: true, LastOperationID: opID}
	snapshot, _ := json.Marshal(accepted)
	ctx := context.Background()
	before := map[string]string{}
	tables := []string{"network_vpcs", "network_operations", "network_reconciliations", "network_idempotency", "network_provider_bindings", "network_resource_history"}
	// Compare the durable NET-01 contract on both schemas. Task notification
	// columns may be added or retired, and old execution leases are invalidated.
	columns := map[string]string{
		"network_vpcs":              "tenant_id,vpc_id,name,description,cidr,state,reason,version,created_at,updated_at,observed_at,last_operation_id",
		"network_operations":        "tenant_id,operation_id,vpc_id,kind,state,reason,attempt,execution_epoch,created_at,updated_at,completed_at,next_attempt_at",
		"network_reconciliations":   "tenant_id,vpc_id",
		"network_idempotency":       "tenant_id,operation_kind,idempotency_key,fingerprint,fingerprint_version,vpc_id,operation_id,response,created_at",
		"network_provider_bindings": "tenant_id,vpc_id,binding_id,cluster_id,namespace,provider_name,provider_uid,pending_action,pending_since",
		"network_resource_history":  "tenant_id,history_id,vpc_id,operation_id,event,resource_state,operation_state,reason,actor_ref,caller_ref,correlation_id,identity_verified,created_at",
	}
	stableRow := func(table string) string {
		return "SELECT to_jsonb(t)::text FROM (SELECT " + columns[table] + " FROM " + table + " WHERE tenant_id=$1) t"
	}
	fixture := testenv.NewDatabase(t, func(owner *pgxpool.Pool, _ string) {
		body, err := migrations.Files.ReadFile("0001_vpc.sql")
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(body)
		tx, err := owner.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `CREATE TABLE network_schema_version(version integer PRIMARY KEY,checksum text NOT NULL,applied_at timestamptz NOT NULL DEFAULT clock_timestamp())`); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO network_schema_version(version,checksum) VALUES(1,$1)`, hex.EncodeToString(digest[:])); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO network_vpcs(tenant_id,vpc_id,name,description,cidr,state,created_at,updated_at,last_operation_id) VALUES($1,$2,'legacy','preserved','10.42.0.0/16','provisioning',$3,$3,$4)`, tenant, vpcID, now, opID); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO network_operations(tenant_id,vpc_id,operation_id,kind,state,created_at,updated_at,next_attempt_at) VALUES($1,$2,$3,'create_vpc','queued',$4,$4,$4)`, tenant, vpcID, opID, now); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO network_reconciliations(tenant_id,vpc_id,next_run_at,lease_owner,lease_until,lease_epoch) VALUES($1,$2,$3,$4,$5,7)`, tenant, vpcID, now, uuid.NewString(), now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO network_idempotency(tenant_id,operation_kind,idempotency_key,fingerprint,fingerprint_version,vpc_id,operation_id,response,created_at) VALUES($1,'create_vpc','legacy-key',$2,1,$3,$4,$5,$6)`, tenant, intent.Fingerprint(), vpcID, opID, snapshot, now); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO network_provider_bindings(tenant_id,vpc_id,binding_id,cluster_id,namespace,provider_name,provider_uid) VALUES($1,$2,$3,'test-cluster','legacy-namespace','legacy-vpc','legacy-provider-uid')`, tenant, vpcID, bindingID); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO network_resource_history(tenant_id,history_id,vpc_id,operation_id,event,resource_state,created_at) VALUES($1,$2,$3,$4,'create_accepted','provisioning',$5)`, tenant, historyID, vpcID, opID, now); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		for _, table := range tables {
			var row string
			if err := owner.QueryRow(ctx, stableRow(table), tenant).Scan(&row); err != nil {
				t.Fatal(err)
			}
			before[table] = row
		}
	})
	for _, table := range tables {
		var row string
		if err := fixture.Owner.QueryRow(ctx, stableRow(table), tenant).Scan(&row); err != nil {
			t.Fatal(err)
		}
		if row != before[table] {
			t.Fatalf("upgrade changed original %s row", table)
		}
		if table != "network_vpcs" {
			var lbReferences int
			if err := fixture.Owner.QueryRow(ctx, "SELECT count(*) FROM "+table+" WHERE tenant_id=$1 AND lb_id IS NOT NULL", tenant).Scan(&lbReferences); err != nil || lbReferences != 0 {
				t.Fatalf("upgrade invented LB references in %s: %d %v", table, lbReferences, err)
			}
		}
	}
	var retiredLease bool
	if err := fixture.Owner.QueryRow(ctx, `SELECT lease_owner IS NULL AND lease_until IS NULL AND lease_epoch=8 AND next_run_at<=clock_timestamp() AND NOT retired FROM network_reconciliations WHERE tenant_id=$1 AND vpc_id=$2`, tenant, vpcID).Scan(&retiredLease); err != nil || !retiredLease {
		t.Fatalf("old execution lease was not retired and requeued: %v %v", retiredLease, err)
	}
	n := newNetwork(t, fixture.Repository, time.Minute)
	replay, err := n.CreateVPC(ctx, biz.CreateVPC{TenantID: tenant, Name: intent.Name, CIDR: intent.CIDR, Description: intent.Description, IdempotencyKey: intent.IdempotencyKey})
	if err != nil || replay.ID != accepted.ID || replay.LastOperationID != opID {
		t.Fatalf("legacy replay: %+v %v", replay, err)
	}
	if err := fixture.Repository.CheckReady(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestSubnetIPRangesMigrationPreservesUnknownOldRows(t *testing.T) {
	ctx := context.Background()
	tenant := uuid.NewString()
	f := testenv.NewDatabase(t, func(owner *pgxpool.Pool, _ string) {
		tx, e := owner.Begin(ctx)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback(ctx)
		if _, e = tx.Exec(ctx, `CREATE TABLE network_schema_version(version integer PRIMARY KEY,checksum text NOT NULL,applied_at timestamptz NOT NULL DEFAULT clock_timestamp())`); e != nil {
			t.Fatal(e)
		}
		files, e := fs.Glob(migrations.Files, "*.sql")
		if e != nil {
			t.Fatal(e)
		}
		sort.Strings(files)
		for i, name := range files {
			if name >= "0004_observation.sql" {
				break
			}
			body, e := migrations.Files.ReadFile(name)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = tx.Exec(ctx, string(body)); e != nil {
				t.Fatal(e)
			}
			hash := sha256.Sum256(body)
			if _, e = tx.Exec(ctx, `INSERT INTO network_schema_version(version,checksum)VALUES($1,$2)`, i+1, hex.EncodeToString(hash[:])); e != nil {
				t.Fatal(e)
			}
		}
		seedNET05AUpgrade(t, tx, tenant, uuid.NewString())
		if e = tx.Commit(ctx); e != nil {
			t.Fatal(e)
		}
	})
	var available, used *string
	var version int64
	if e := f.Owner.QueryRow(ctx, `SELECT available_ip_range,used_ip_range,version FROM network_subnets WHERE tenant_id=$1`, tenant).Scan(&available, &used, &version); e != nil || available != nil || used != nil || version != 9 {
		t.Fatal("migration invented ranges or changed old row", available, used, version, e)
	}
	if _, e := f.Owner.Exec(ctx, `UPDATE network_subnets SET available_ip_range='',used_ip_range='' WHERE tenant_id=$1`, tenant); e != nil {
		t.Fatal(e)
	}
	if e := f.Owner.QueryRow(ctx, `SELECT available_ip_range,used_ip_range FROM network_subnets WHERE tenant_id=$1`, tenant).Scan(&available, &used); e != nil || available == nil || used == nil || *available != "" || *used != "" {
		t.Fatal("migration lost known-empty range", available, used, e)
	}
	for _, field := range []string{"available_ip_count", "used_ip_count"} {
		if _, e := f.Owner.Exec(ctx, "UPDATE network_subnets SET "+field+"=-1 WHERE tenant_id=$1", tenant); e == nil {
			t.Fatal("negative count accepted", field)
		}
	}
}
