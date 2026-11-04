package data_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/zhangzhe-ctrl/ani-resource-service/migrations"
	"github.com/zhangzhe-ctrl/ani-resource-service/tests/testenv"
	"testing"
)

// Migration-only fixtures use actual schema 13 bytes. They do not claim product
// admission or runtime readiness; those are covered by the vertical-flow tests.
func TestLBDeferredUpgradeBackfillsOnlyAcceptedConfigurations(t *testing.T) {
	ctx := context.Background()
	var legacy legacyPublicFixture
	lbID, reservationID, subnetID := schemaResourceID("lb"), schemaResourceID("lb"), schemaResourceID("subnet")
	lbOperation, subOperation, listenerID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	receipt := []byte(`{"historical_receipt": "must remain byte-for-byte"}`)
	db := testenv.NewDatabase(t, func(owner *pgxpool.Pool, _ string) {
		legacy = seedSchema5Public(t, owner)
		tx, err := owner.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		exec := func(query string, args ...any) {
			t.Helper()
			if _, err := tx.Exec(ctx, query, args...); err != nil {
				t.Fatal(err)
			}
		}
		names := []string{"0006_vpc_base_connectivity.sql", "0007_load_balancers.sql", "0008_lb_health_check_port.sql", "0009_lb_fixed_flavors.sql", "0010_lb_listeners.sql", "0011_subnet_ip_counts.sql", "0012_lb_listener_parameters.sql", "0013_subnet_ip_ranges.sql"}
		for index, name := range names {
			body, err := migrations.Files.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			exec(string(body))
			sum := sha256.Sum256(body)
			exec(`INSERT INTO network_schema_version(version,checksum) VALUES($1,$2)`, index+6, hex.EncodeToString(sum[:]))
		}
		ns := "tenant-" + legacy.Tenant
		exec(`INSERT INTO network_subnets(tenant_id,subnet_id,vpc_id,name,description,cidr,gateway,state,last_operation_id,created_at,updated_at) VALUES($1,$2,$3,'deferred-upgrade-entry','','10.42.1.0/24','10.42.1.1','available',$4,clock_timestamp(),clock_timestamp())`, legacy.Tenant, subnetID, legacy.VPC, subOperation)
		exec(`INSERT INTO network_operations(tenant_id,subnet_id,operation_id,kind,state,completed_at,created_at,updated_at) VALUES($1,$2,$3,'create_subnet','succeeded',clock_timestamp(),clock_timestamp(),clock_timestamp())`, legacy.Tenant, subnetID, subOperation)
		exec(`INSERT INTO network_provider_bindings(tenant_id,subnet_id,resource_kind,binding_id,cluster_id,namespace,provider_name,provider_uid) VALUES($1,$2,'subnet',gen_random_uuid(),'test-cluster',$3,'deferred-upgrade-entry','old-subnet-uid')`, legacy.Tenant, subnetID, ns)
		exec(`INSERT INTO network_load_balancers(tenant_id,lb_id,cluster_id,namespace,vpc_id,subnet_id,exposure,private_ip,name,state,last_operation_id,applied_version) VALUES($1,$2,'test-cluster',$3,$4,$5,'private','10.42.1.100','old-runtime','available',$6,1)`, legacy.Tenant, lbID, ns, legacy.VPC, subnetID, lbOperation)
		exec(`INSERT INTO network_operations(tenant_id,lb_id,operation_id,kind,state,completed_at,created_at,updated_at) VALUES($1,$2,$3,'create_load_balancer','succeeded',clock_timestamp(),clock_timestamp(),clock_timestamp())`, legacy.Tenant, lbID, lbOperation)
		exec(`INSERT INTO network_lb_listeners(tenant_id,cluster_id,namespace,lb_id,listener_id,name,port) VALUES($1,'test-cluster',$2,$3,$4,'http',80)`, legacy.Tenant, ns, lbID, listenerID)
		exec(`INSERT INTO network_lb_configurations(tenant_id,cluster_id,namespace,lb_id,config_version,name,description,interval_seconds,timeout_seconds,unhealthy_threshold,healthy_threshold,health_check_port) VALUES($1,'test-cluster',$2,$3,1,'old-runtime','',5,3,3,1,8080)`, legacy.Tenant, ns, lbID)
		exec(`INSERT INTO network_lb_configuration_listeners(tenant_id,cluster_id,namespace,lb_id,config_version,listener_id,protocol,port,interval_seconds,timeout_seconds,unhealthy_threshold,healthy_threshold,health_check_port) VALUES($1,'test-cluster',$2,$3,1,$4,'HTTP',80,5,3,3,1,8080)`, legacy.Tenant, ns, lbID, listenerID)
		exec(`INSERT INTO network_provider_bindings(tenant_id,lb_id,resource_kind,binding_id,cluster_id,namespace,provider_name,provider_uid,create_dispatched) VALUES($1,$2,'load_balancer',gen_random_uuid(),'test-cluster',$3,'old-gateway','old-gateway-uid',true)`, legacy.Tenant, lbID, ns)
		exec(`INSERT INTO network_idempotency(tenant_id,operation_kind,idempotency_key,fingerprint,fingerprint_version,lb_id,operation_id,response,created_at) VALUES($1,'create_load_balancer','old-deferred-receipt',repeat('a',64),1,$2,$3,$4,clock_timestamp())`, legacy.Tenant, lbID, lbOperation, receipt)
		exec(`INSERT INTO network_load_balancers(tenant_id,lb_id,cluster_id,namespace,vpc_id,subnet_id,exposure,private_ip,name,state) VALUES($1,$2,'test-cluster',$3,$4,$5,'private','10.42.1.101','old-reservation','provisioning')`, legacy.Tenant, reservationID, ns, legacy.VPC, subnetID)
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	})
	var acceptedTarget int64
	var reservationTarget pgtype.Int8
	var actualReceipt []byte
	var gatewayUID string
	if err := db.Owner.QueryRow(ctx, `SELECT provider_target_version FROM network_load_balancers WHERE tenant_id=$1 AND lb_id=$2`, legacy.Tenant, lbID).Scan(&acceptedTarget); err != nil || acceptedTarget != 1 {
		t.Fatal("legacy accepted target was not backfilled", acceptedTarget, err)
	}
	if err := db.Owner.QueryRow(ctx, `SELECT provider_target_version FROM network_load_balancers WHERE tenant_id=$1 AND lb_id=$2`, legacy.Tenant, reservationID).Scan(&reservationTarget); err != nil || reservationTarget.Valid {
		t.Fatal("reservation-only row promoted to provider target", reservationTarget, err)
	}
	if err := db.Owner.QueryRow(ctx, `SELECT response FROM network_idempotency WHERE tenant_id=$1 AND lb_id=$2`, legacy.Tenant, lbID).Scan(&actualReceipt); err != nil || string(actualReceipt) != string(receipt) {
		t.Fatal("migration modified historical receipt", fmt.Sprintf("%q", actualReceipt), err)
	}
	if err := db.Owner.QueryRow(ctx, `SELECT provider_uid FROM network_provider_bindings WHERE tenant_id=$1 AND lb_id=$2`, legacy.Tenant, lbID).Scan(&gatewayUID); err != nil || gatewayUID != "old-gateway-uid" {
		t.Fatal("migration replaced live binding identity", gatewayUID, err)
	}
}
