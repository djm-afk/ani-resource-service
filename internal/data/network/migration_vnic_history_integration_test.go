package data_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	data "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/migrations"
	"github.com/zhangzhe-ctrl/ani-resource-service/tests/testenv"
)

type vnicMigrationReceipt struct {
	Version   int
	Checksum  string
	AppliedAt time.Time
}

func vnicMigrationLedger(t *testing.T, owner *pgxpool.Pool) []vnicMigrationReceipt {
	t.Helper()
	rows, err := owner.Query(context.Background(), `SELECT version,checksum,applied_at FROM network_schema_version ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []vnicMigrationReceipt
	for rows.Next() {
		var row vnicMigrationReceipt
		if err := rows.Scan(&row.Version, &row.Checksum, &row.AppliedAt); err != nil {
			t.Fatal(err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func seedVNicMigrationHistory(t *testing.T, owner *pgxpool.Pool, legacy bool) {
	t.Helper()
	ctx := context.Background()
	tx, err := owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `CREATE TABLE network_schema_version(version integer PRIMARY KEY,checksum text NOT NULL,applied_at timestamptz NOT NULL DEFAULT clock_timestamp())`); err != nil {
		t.Fatal(err)
	}
	files, err := fs.Glob(migrations.Files, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	names := files[:28]
	if legacy {
		names = append(append([]string{}, files[:27]...), files[28:31]...)
	}
	for index, name := range names {
		body, err := migrations.Files.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
		sum := sha256.Sum256(body)
		if _, err := tx.Exec(ctx, `INSERT INTO network_schema_version(version,checksum,applied_at) VALUES($1,$2,'2026-11-03 12:00:00+00'::timestamptz + $1::integer * interval '1 second')`, index+1, hex.EncodeToString(sum[:])); err != nil {
			t.Fatal(err)
		}
	}
	if legacy {
		// Existing product intent and the consumer backfill must survive together.
		_, err := tx.Exec(ctx, `
INSERT INTO network_vpcs(tenant_id,vpc_id,name,description,cidr,state,created_at,updated_at,last_operation_id)
 VALUES('11111111-1111-4111-8111-111111111111','vpc_11111111111111111111111111111111','retained','','10.42.0.0/16','available',now(),now(),'11111111-1111-4111-8111-111111111111');
INSERT INTO network_operations(tenant_id,operation_id,vpc_id,kind,state,created_at,updated_at,completed_at)
 VALUES('11111111-1111-4111-8111-111111111111','11111111-1111-4111-8111-111111111111','vpc_11111111111111111111111111111111','create_vpc','succeeded',now(),now(),now());
INSERT INTO network_subnets(tenant_id,subnet_id,vpc_id,name,description,cidr,gateway,state,created_at,updated_at,last_operation_id,exclude_ips,enable_dhcp,dns_servers)
 VALUES('11111111-1111-4111-8111-111111111111','subnet_11111111111111111111111111111111','vpc_11111111111111111111111111111111','retained','','10.42.1.0/24','10.42.1.1','available',now(),now(),'22222222-2222-4222-8222-222222222222',ARRAY['10.42.1.10'],true,ARRAY['1.1.1.1']);
INSERT INTO network_operations(tenant_id,operation_id,subnet_id,kind,state,created_at,updated_at,completed_at)
 VALUES('11111111-1111-4111-8111-111111111111','22222222-2222-4222-8222-222222222222','subnet_11111111111111111111111111111111','create_subnet','succeeded',now(),now(),now());
INSERT INTO network_tenant_namespaces(tenant_id,cluster_id,namespace,namespace_uid)
 VALUES('11111111-1111-4111-8111-111111111111','test-cluster','tenant-11111111-1111-4111-8111-111111111111','namespace-original');
INSERT INTO network_provider_bindings(tenant_id,subnet_id,resource_kind,binding_id,cluster_id,namespace,provider_name,provider_uid)
 VALUES('11111111-1111-4111-8111-111111111111','subnet_11111111111111111111111111111111','subnet','44444444-4444-4444-8444-444444444444','test-cluster','tenant-11111111-1111-4111-8111-111111111111','retained-subnet','subnet-original');
INSERT INTO network_vnics(tenant_id,vnic_id,vpc_id,subnet_id,name,state,version,created_at,updated_at,last_operation_id,requested_ip,requested_mac)
 VALUES('11111111-1111-4111-8111-111111111111','vnic_11111111111111111111111111111111','vpc_11111111111111111111111111111111','subnet_11111111111111111111111111111111','retained','available',7,now(),now(),'33333333-3333-4333-8333-333333333333','10.42.1.21','02:75:00:00:00:21');
INSERT INTO network_operations(tenant_id,operation_id,vnic_id,kind,state,created_at,updated_at,completed_at)
 VALUES('11111111-1111-4111-8111-111111111111','33333333-3333-4333-8333-333333333333','vnic_11111111111111111111111111111111','create_vnic','succeeded',now(),now(),now());
INSERT INTO network_attachments(tenant_id,attachment_id,vpc_id,subnet_id,binding_id,instance_id,slot,request_key,submission_id,generation,fingerprint,cluster_id,namespace,binding_revision,plan,state)
 VALUES('11111111-1111-4111-8111-111111111111','att_11111111111111111111111111111111','vpc_11111111111111111111111111111111','subnet_11111111111111111111111111111111','44444444-4444-4444-8444-444444444444','retained-instance','primary','retained','55555555-5555-4555-8555-555555555555',3,repeat('a',64),'test-cluster','tenant-11111111-1111-4111-8111-111111111111','original','{"WorkloadMode":"controller_managed"}','reserved');`)
		if err != nil {
			t.Fatal("seed retained intent", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestVNicMigrationMain28Upgrade(t *testing.T) {
	var before []vnicMigrationReceipt
	f := testenv.NewDatabase(t, func(owner *pgxpool.Pool, _ string) {
		seedVNicMigrationHistory(t, owner, false)
		before = vnicMigrationLedger(t, owner)
	})
	after := vnicMigrationLedger(t, f.Owner)
	if len(after) != 31 || !reflect.DeepEqual(before, after[:28]) {
		t.Fatal("main28 receipts changed", before, after)
	}
	var columns int
	if err := f.Owner.QueryRow(context.Background(), `SELECT count(*) FROM information_schema.columns WHERE table_schema='public' AND ((table_name='network_vnics' AND column_name IN ('requested_ip','requested_mac')) OR (table_name='network_subnets' AND column_name IN ('exclude_ips','enable_dhcp','dns_servers')) OR (table_name='network_attachments' AND column_name='consumer_type'))`).Scan(&columns); err != nil || columns != 6 {
		t.Fatal("missing canonical schema", columns, err)
	}
	if err := f.Repository.CheckReady(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestVNicMigrationReleased30Upgrade(t *testing.T) {
	var before []vnicMigrationReceipt
	f := testenv.NewDatabase(t, func(owner *pgxpool.Pool, _ string) {
		seedVNicMigrationHistory(t, owner, true)
		before = vnicMigrationLedger(t, owner)
	})
	after := vnicMigrationLedger(t, f.Owner)
	if len(after) != 31 || !reflect.DeepEqual(before[:27], after[:27]) {
		t.Fatal("shared receipts changed", before, after)
	}
	for i := 27; i < len(before); i++ {
		want := before[i]
		want.Version++
		if after[i+1] != want {
			t.Fatalf("released receipt changed: want %+v, got %+v", want, after[i+1])
		}
	}
	var consumerColumn, activeIndex bool
	if err := f.Owner.QueryRow(context.Background(), `SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_name='network_attachments' AND column_name='consumer_type'),to_regclass('network_attachment_vpc_active') IS NOT NULL`).Scan(&consumerColumn, &activeIndex); err != nil || !consumerColumn || !activeIndex {
		t.Fatal("main28 not applied", consumerColumn, activeIndex, err)
	}
	var retained bool
	if err := f.Owner.QueryRow(context.Background(), `SELECT a.consumer_type='container_instance' AND a.generation=3 AND a.state='reserved' AND v.version=7 AND v.requested_ip='10.42.1.21' AND v.requested_mac='02:75:00:00:00:21' AND s.exclude_ips=ARRAY['10.42.1.10'] AND s.enable_dhcp AND s.dns_servers=ARRAY['1.1.1.1'] FROM network_attachments a JOIN network_subnets s USING(tenant_id,subnet_id) JOIN network_vnics v USING(tenant_id,subnet_id)`).Scan(&retained); err != nil || !retained {
		t.Fatal("retained intent/backfill changed", retained, err)
	}
	if err := data.Migrate(context.Background(), f.Owner, f.RuntimeRole); err != nil {
		t.Fatal("canonical replay", err)
	}
	if replay := vnicMigrationLedger(t, f.Owner); !reflect.DeepEqual(after, replay) {
		t.Fatal("migration replay changed receipts")
	}
	if err := f.Repository.CheckReady(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestVNicMigrationRejectsUnrecognizedHistory(t *testing.T) {
	for _, failure := range []string{"unknown checksum", "mixed history", "missing version"} {
		t.Run(failure, func(t *testing.T) {
			testenv.NewDatabase(t, func(owner *pgxpool.Pool, role string) {
				seedVNicMigrationHistory(t, owner, true)
				ctx := context.Background()
				original := vnicMigrationLedger(t, owner)
				var err error
				switch failure {
				case "unknown checksum":
					_, err = owner.Exec(ctx, `UPDATE network_schema_version SET checksum=repeat('0',64) WHERE version=28`)
				case "mixed history":
					body, e := migrations.Files.ReadFile("0028_attachment_consumer_type.sql")
					if e != nil {
						t.Fatal(e)
					}
					sum := sha256.Sum256(body)
					_, err = owner.Exec(ctx, `UPDATE network_schema_version SET checksum=$1 WHERE version=29`, hex.EncodeToString(sum[:]))
				case "missing version":
					_, err = owner.Exec(ctx, `DELETE FROM network_schema_version WHERE version=29`)
				}
				if err != nil {
					t.Fatal(err)
				}
				before := vnicMigrationLedger(t, owner)
				if err := data.Migrate(ctx, owner, role); err == nil {
					t.Fatal("unrecognized history accepted")
				}
				if after := vnicMigrationLedger(t, owner); !reflect.DeepEqual(before, after) {
					t.Fatal("rejected migration changed ledger")
				}
				var consumerColumn bool
				if err := owner.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_name='network_attachments' AND column_name='consumer_type')`).Scan(&consumerColumn); err != nil || consumerColumn {
					t.Fatal("rejected migration changed schema", consumerColumn, err)
				}
				// Repair only this deliberately corrupted fixture so NewDatabase can
				// finish its standard migration and cleanup lifecycle.
				for _, row := range original[27:] {
					if _, err := owner.Exec(ctx, `INSERT INTO network_schema_version(version,checksum,applied_at) VALUES($1,$2,$3) ON CONFLICT(version) DO UPDATE SET checksum=excluded.checksum,applied_at=excluded.applied_at`, row.Version, row.Checksum, row.AppliedAt); err != nil {
						t.Fatal(err)
					}
				}
			})
		})
	}
}
