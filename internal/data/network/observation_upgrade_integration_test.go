package data_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/zhangzhe-ctrl/ani-resource-service/migrations"
	"github.com/zhangzhe-ctrl/ani-resource-service/tests/testenv"
	"io/fs"
	"slices"
	"testing"
)

func TestNET05AMigrationUpgradesAllThreeHistoricalChecksums(t *testing.T) {
	before := map[int]string{}
	var historicalSchema string
	facts := map[string]string{}
	tenant, leaseOwner := uuid.NewString(), uuid.NewString()
	columns := map[string]string{
		"network_vpcs":               "tenant_id,vpc_id,name,description,cidr,state,reason,version,created_at,updated_at,observed_at,last_operation_id",
		"network_subnets":            "tenant_id,subnet_id,vpc_id,name,description,cidr,gateway,state,reason,version,created_at,updated_at,observed_at,last_operation_id",
		"network_operations":         "tenant_id,operation_id,vpc_id,subnet_id,kind,state,reason,attempt,execution_epoch,created_at,updated_at,completed_at,next_attempt_at",
		"network_reconciliations":    "tenant_id,vpc_id,subnet_id",
		"network_provider_bindings":  "tenant_id,vpc_id,subnet_id,binding_id,resource_kind,cluster_id,namespace,provider_name,provider_uid,pending_action,pending_since",
		"network_attachments":        "tenant_id,attachment_id,vpc_id,subnet_id,binding_id,instance_id,slot,request_key,submission_id,generation,fingerprint,cluster_id,namespace,binding_revision,plan,state,reason,version,pod_name,pod_uid,confirm_uid,finalization_id,created_at,updated_at,observed_at,released_at",
		"network_attachment_history": "tenant_id,history_id,attachment_id,version,event,state,reason,pod_uid,finalization_id,created_at",
	}
	tables := []string{"network_vpcs", "network_subnets", "network_operations", "network_reconciliations", "network_provider_bindings", "network_attachments", "network_attachment_history"}
	projection := func(table string) string {
		return "SELECT coalesce(jsonb_agg(to_jsonb(t) ORDER BY to_jsonb(t)::text),'[]')::text FROM (SELECT " + columns[table] + " FROM " + table + ") t"
	}
	ctx := context.Background()
	f := testenv.NewDatabase(t, func(owner *pgxpool.Pool, _ string) {
		tx, e := owner.Begin(ctx)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback(ctx)
		if _, e = tx.Exec(ctx, `CREATE TABLE network_schema_version(version integer PRIMARY KEY,checksum text NOT NULL,applied_at timestamptz NOT NULL DEFAULT clock_timestamp())`); e != nil {
			t.Fatal(e)
		}
		for i, name := range []string{"0001_vpc.sql", "0002_subnet.sql", "0003_attachment.sql"} {
			body, e := migrations.Files.ReadFile(name)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = tx.Exec(ctx, string(body)); e != nil {
				t.Fatal(e)
			}
			hash := sha256.Sum256(body)
			before[i+1] = hex.EncodeToString(hash[:])
			if _, e = tx.Exec(ctx, `INSERT INTO network_schema_version(version,checksum)VALUES($1,$2)`, i+1, before[i+1]); e != nil {
				t.Fatal(e)
			}
		}
		seedNET05AUpgrade(t, tx, tenant, leaseOwner)
		if e = tx.Commit(ctx); e != nil {
			t.Fatal(e)
		}
		if e = owner.QueryRow(ctx, "SELECT jsonb_agg(to_jsonb(v) ORDER BY version)::text FROM network_schema_version v").Scan(&historicalSchema); e != nil {
			t.Fatal(e)
		}
		for _, table := range tables {
			var body string
			if e = owner.QueryRow(ctx, projection(table)).Scan(&body); e != nil {
				t.Fatal(e)
			}
			facts[table] = body
		}
	})
	for _, table := range tables {
		var body string
		if e := f.Owner.QueryRow(ctx, projection(table)).Scan(&body); e != nil {
			t.Fatal(e)
		}
		if body != facts[table] {
			t.Fatalf("upgrade changed existing product facts: %s", table)
		}
	}
	var leasesRetired bool
	if err := f.Owner.QueryRow(ctx, `SELECT bool_and(lease_owner IS NULL AND lease_until IS NULL AND lease_epoch=CASE WHEN vpc_id IS NOT NULL THEN 14 ELSE 18 END) FROM network_reconciliations WHERE tenant_id=$1`, tenant).Scan(&leasesRetired); err != nil || !leasesRetired {
		t.Fatal("coordinated upgrade did not fence pre-upgrade workers", leasesRetired, err)
	}
	var nonnullIPCounts int
	if e := f.Owner.QueryRow(ctx, "SELECT count(*) FROM network_subnets WHERE available_ip_count IS NOT NULL OR used_ip_count IS NOT NULL").Scan(&nonnullIPCounts); e != nil || nonnullIPCounts != 0 {
		t.Fatalf("upgrade invented subnet IP counts: %d %v", nonnullIPCounts, e)
	}
	var nonnullIPRanges int
	if e := f.Owner.QueryRow(ctx, "SELECT count(*) FROM network_subnets WHERE available_ip_range IS NOT NULL OR used_ip_range IS NOT NULL").Scan(&nonnullIPRanges); e != nil || nonnullIPRanges != 0 {
		t.Fatalf("upgrade invented subnet IP ranges: %d %v", nonnullIPRanges, e)
	}
	for _, table := range []string{"network_operations", "network_reconciliations", "network_provider_bindings"} {
		var lbReferences int
		if e := f.Owner.QueryRow(ctx, "SELECT count(*) FROM "+table+" WHERE lb_id IS NOT NULL").Scan(&lbReferences); e != nil || lbReferences != 0 {
			t.Fatalf("upgrade invented LB references in %s: %d %v", table, lbReferences, e)
		}
	}
	for _, table := range []string{"network_reconciliations", "network_attachments"} {
		var valid bool
		if e := f.Owner.QueryRow(ctx, "SELECT bool_and(requested_generation=1 AND processed_generation=0 AND observation_error='') FROM "+table).Scan(&valid); e != nil || !valid {
			t.Fatalf("unsafe upgrade defaults %s %v", table, e)
		}
	}
	for version, expected := range before {
		var got string
		if e := f.Owner.QueryRow(ctx, `SELECT checksum FROM network_schema_version WHERE version=$1`, version).Scan(&got); e != nil || got != expected {
			t.Fatalf("historical checksum changed %d %v", version, e)
		}
	}
	var currentHistoricalSchema string
	if e := f.Owner.QueryRow(ctx, "SELECT jsonb_agg(to_jsonb(v) ORDER BY version)::text FROM network_schema_version v WHERE version <= $1", len(before)).Scan(&currentHistoricalSchema); e != nil || currentHistoricalSchema != historicalSchema {
		t.Fatalf("historical migration facts or timestamps changed: %v", e)
	}
	files, e := fs.Glob(migrations.Files, "*.sql")
	if e != nil {
		t.Fatal(e)
	}
	slices.Sort(files)
	for index, name := range files {
		body, e := migrations.Files.ReadFile(name)
		if e != nil {
			t.Fatal(e)
		}
		hash := sha256.Sum256(body)
		var got string
		if e = f.Owner.QueryRow(ctx, "SELECT checksum FROM network_schema_version WHERE version=$1", index+1).Scan(&got); e != nil || got != hex.EncodeToString(hash[:]) {
			t.Fatalf("migration %s missing or changed: %v", name, e)
		}
	}
	var count int
	if e = f.Owner.QueryRow(ctx, `SELECT count(*) FROM network_schema_version`).Scan(&count); e != nil || count != len(files) {
		t.Fatalf("upgrade migrations: got=%d want=%d %v", count, len(files), e)
	}
}

// Seed exact schema-3 records with live leases, unknown create intent and
// durable public history. The coordinated upgrade resets execution leases;
// it preserves the accepted command and original provider identity.
func seedNET05AUpgrade(t *testing.T, tx pgx.Tx, tenant, lease string) {
	t.Helper()
	ctx := context.Background()
	v, s, a := "vpc_12345678901234567890123456789012", "subnet_12345678901234567890123456789012", "att_12345678901234567890123456789012"
	vo, so, vb, sb := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	exec := func(query string, args ...any) {
		if _, e := tx.Exec(ctx, query, args...); e != nil {
			t.Fatal(e)
		}
	}
	exec(`INSERT INTO network_vpcs(tenant_id,vpc_id,name,description,cidr,state,version,created_at,updated_at,last_operation_id) VALUES($1,$2,'upgrade','preserved','10.42.0.0/16','available',7,clock_timestamp(),clock_timestamp(),$3)`, tenant, v, vo)
	exec(`INSERT INTO network_operations(tenant_id,vpc_id,operation_id,kind,state,created_at,updated_at,completed_at) VALUES($1,$2,$3,'create_vpc','succeeded',clock_timestamp(),clock_timestamp(),clock_timestamp())`, tenant, v, vo)
	exec(`INSERT INTO network_subnets(tenant_id,subnet_id,vpc_id,name,description,cidr,gateway,state,version,created_at,updated_at,last_operation_id) VALUES($1,$2,$3,'upgrade','preserved','10.42.1.0/24','10.42.1.1','provisioning',9,clock_timestamp(),clock_timestamp(),$4)`, tenant, s, v, so)
	exec(`INSERT INTO network_operations(tenant_id,subnet_id,operation_id,kind,state,created_at,updated_at) VALUES($1,$2,$3,'create_subnet','blocked',clock_timestamp(),clock_timestamp())`, tenant, s, so)
	exec(`INSERT INTO network_provider_bindings(tenant_id,vpc_id,binding_id,cluster_id,namespace,provider_name,provider_uid) VALUES($1,$2,$3,'test-cluster','tenant-upgrade','vpc-upgrade','vpc-uid')`, tenant, v, vb)
	exec(`INSERT INTO network_provider_bindings(tenant_id,subnet_id,binding_id,resource_kind,cluster_id,namespace,provider_name,pending_action,pending_since) VALUES($1,$2,$3,'subnet','test-cluster','tenant-upgrade','subnet-upgrade','create',clock_timestamp())`, tenant, s, sb)
	exec(`INSERT INTO network_reconciliations(tenant_id,vpc_id,next_run_at,lease_owner,lease_until,lease_epoch) VALUES($1,$2,clock_timestamp(),$3,clock_timestamp()+interval '10 minutes',13)`, tenant, v, lease)
	exec(`INSERT INTO network_reconciliations(tenant_id,subnet_id,next_run_at,lease_owner,lease_until,lease_epoch) VALUES($1,$2,clock_timestamp(),$3,clock_timestamp()+interval '10 minutes',17)`, tenant, s, lease)
	exec(`INSERT INTO network_attachments(tenant_id,attachment_id,vpc_id,subnet_id,binding_id,instance_id,slot,request_key,submission_id,generation,fingerprint,cluster_id,namespace,binding_revision,plan,state,version,provider_relations,lease_owner,lease_until,epoch) VALUES($1,$2,$3,$4,$5,'inst_upgrade','primary','permanent',$6,1,repeat('a',64),'test-cluster','tenant-upgrade','revision','{}','reserved',19,'[{"Kind":"VNicIP","Name":"orphan","UID":"known-ip"}]',$7,clock_timestamp()+interval '10 minutes',23)`, tenant, a, v, s, sb, uuid.NewString(), lease)
	exec(`INSERT INTO network_attachment_history(tenant_id,history_id,attachment_id,version,event,state,reason) VALUES($1,$2,$3,19,'prepared','reserved','')`, tenant, uuid.NewString(), a)
}
