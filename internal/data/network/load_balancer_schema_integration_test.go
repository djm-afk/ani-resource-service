package data_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/migrations"
	"github.com/zhangzhe-ctrl/ani-resource-service/tests/testenv"
	"strings"
	"testing"
)

func TestLBSchemaFrom0006PreservesAllOldColumnsAndDormantReservations(t *testing.T) {
	ctx := context.Background()
	columns, before := map[string]string{}, map[string]string{}
	var legacy legacyPublicFixture
	lbID, subnetID := schemaResourceID("lb"), schemaResourceID("subnet")
	tables := []string{"network_vpcs", "network_subnets", "network_eips", "network_snat_bindings", "network_eip_claims", "network_load_balancers", "network_lb_vip_intents", "network_provider_bindings", "network_operations", "network_reconciliations", "network_idempotency", "network_resource_history"}
	snapshot := func(owner *pgxpool.Pool, table string) string {
		t.Helper()
		var value string
		query := fmt.Sprintf(`SELECT coalesce(jsonb_agg(to_jsonb(r) ORDER BY to_jsonb(r)::text),'[]')::text FROM (SELECT %s FROM %s) r`, columns[table], pgx.Identifier{table}.Sanitize())
		if err := owner.QueryRow(ctx, query).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
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
		body, err := migrations.Files.ReadFile("0006_vpc_base_connectivity.sql")
		if err != nil {
			t.Fatal(err)
		}
		exec(string(body))
		sum := sha256.Sum256(body)
		exec(`INSERT INTO network_schema_version(version,checksum) VALUES(6,$1)`, hex.EncodeToString(sum[:]))
		op := uuid.NewString()
		ns := "tenant-" + legacy.Tenant
		exec(`INSERT INTO network_subnets(tenant_id,subnet_id,vpc_id,name,description,cidr,gateway,state,created_at,updated_at,observed_at,last_operation_id) VALUES($1,$2,$3,'old-entry','','10.42.1.0/24','10.42.1.1','available',clock_timestamp(),clock_timestamp(),clock_timestamp(),$4)`, legacy.Tenant, subnetID, legacy.VPC, op)
		exec(`INSERT INTO network_operations(tenant_id,subnet_id,operation_id,kind,state,created_at,updated_at,completed_at) VALUES($1,$2,$3,'create_subnet','succeeded',clock_timestamp(),clock_timestamp(),clock_timestamp())`, legacy.Tenant, subnetID, op)
		exec(`INSERT INTO network_provider_bindings(tenant_id,subnet_id,resource_kind,binding_id,cluster_id,namespace,provider_name,provider_uid) VALUES($1,$2,'subnet',gen_random_uuid(),'test-cluster',$3,'old-entry','old-subnet-uid')`, legacy.Tenant, subnetID, ns)
		exec(`INSERT INTO network_load_balancers(tenant_id,lb_id,cluster_id,namespace,vpc_id,subnet_id,exposure,private_ip,state) VALUES($1,$2,'test-cluster',$3,$4,$5,'private','10.42.1.100','provisioning')`, legacy.Tenant, lbID, ns, legacy.VPC, subnetID)
		exec(`INSERT INTO network_lb_vip_intents(tenant_id,lb_id,cluster_id,namespace,vpc_id,subnet_id,address) VALUES($1,$2,'test-cluster',$3,$4,$5,'10.42.1.100')`, legacy.Tenant, lbID, ns, legacy.VPC, subnetID)
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		for _, table := range tables {
			var columnsValue string
			if err = owner.QueryRow(ctx, `SELECT string_agg(quote_ident(column_name),',' ORDER BY ordinal_position) FROM information_schema.columns WHERE table_schema='public' AND table_name=$1`, table).Scan(&columnsValue); err != nil {
				t.Fatal(err)
			}
			columns[table] = columnsValue
			before[table] = snapshot(owner, table)
		}
	})
	for _, table := range tables {
		if got := snapshot(db.Owner, table); got != before[table] {
			t.Fatal("0007 changed old data", table)
		}
	}
	auth := biz.WithEgressCaller(ctx, biz.EgressCaller{TenantID: legacy.Tenant})
	lbs, err := biz.NewLoadBalancers(db.Repository, biz.ContextEgressAuthorization{}, []byte(strings.Repeat("c", 32)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = lbs.Get(auth, "", lbID); biz.ReasonOf(err) != biz.ResourceNotFound {
		t.Fatal("dormant U01 identity adopted as product", err)
	}
	items, _, total, err := lbs.List(auth, biz.ListLoadBalancers{ListVPCs: biz.ListVPCs{Limit: 20}})
	if err != nil || len(items) != 0 || total != 0 {
		t.Fatal("dormant U01 identity listed", items, err)
	}
	var occupied, tasks int
	if err = db.Owner.QueryRow(ctx, `SELECT (SELECT count(*) FROM network_lb_vip_intents WHERE tenant_id=$1 AND lb_id=$2 AND released_at IS NULL),(SELECT count(*) FROM network_reconciliations WHERE tenant_id=$1 AND lb_id=$2)`, legacy.Tenant, lbID).Scan(&occupied, &tasks); err != nil || occupied != 1 || tasks != 0 {
		t.Fatal("upgrade lost reservation or invented work", occupied, tasks, err)
	}
	for _, table := range []string{"network_lb_listeners", "network_lb_configurations", "network_lb_members", "network_lb_configuration_members", "network_lb_subnet_refs", "network_lb_components", "network_lb_capabilities"} {
		var safe bool
		if err = db.Owner.QueryRow(ctx, `SELECT has_table_privilege($1,$2,'SELECT') AND has_table_privilege($1,$2,'INSERT') AND has_table_privilege($1,$2,'UPDATE') AND NOT has_table_privilege($1,$2,'DELETE')`, db.RuntimeRole, table).Scan(&safe); err != nil || !safe {
			t.Fatal("new runtime grant contract", table, err)
		}
	}
}

// Upgrade actual pre-collection SQL, preserving the old acceptance snapshot,
// listener ID and Route/Policy CR identities. This is migration evidence.
func TestLBListenersUpgradeFrom0008AndReplay(t *testing.T) {
	ctx := context.Background()
	var legacy legacyPublicFixture
	lbID, subnetID, opID, listenerID := schemaResourceID("lb"), schemaResourceID("subnet"), uuid.NewString(), uuid.NewString()
	var intent biz.LoadBalancerIntent
	var snapshot []byte
	db := testenv.NewDatabase(t, func(owner *pgxpool.Pool, _ string) {
		legacy = seedSchema5Public(t, owner)
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
		for j, name := range []string{"0006_vpc_base_connectivity.sql", "0007_load_balancers.sql", "0008_lb_health_check_port.sql"} {
			body, err := migrations.Files.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			exec(string(body))
			sum := sha256.Sum256(body)
			exec(`INSERT INTO network_schema_version(version,checksum) VALUES($1,$2)`, j+6, hex.EncodeToString(sum[:]))
		}
		ns := "tenant-" + legacy.Tenant
		subOp := uuid.NewString()
		exec(`INSERT INTO network_subnets(tenant_id,subnet_id,vpc_id,name,description,cidr,gateway,state,last_operation_id,created_at,updated_at) VALUES($1,$2,$3,'old-entry','','10.42.1.0/24','10.42.1.1','available',$4,clock_timestamp(),clock_timestamp())`, legacy.Tenant, subnetID, legacy.VPC, subOp)
		exec(`INSERT INTO network_operations(tenant_id,subnet_id,operation_id,kind,state,completed_at,created_at,updated_at) VALUES($1,$2,$3,'create_subnet','succeeded',clock_timestamp(),clock_timestamp(),clock_timestamp())`, legacy.Tenant, subnetID, subOp)
		exec(`INSERT INTO network_provider_bindings(tenant_id,subnet_id,resource_kind,binding_id,cluster_id,namespace,provider_name,provider_uid) VALUES($1,$2,'subnet',gen_random_uuid(),'test-cluster',$3,'old-entry','old-subnet-uid')`, legacy.Tenant, subnetID, ns)
		exec(`INSERT INTO network_load_balancers(tenant_id,lb_id,cluster_id,namespace,vpc_id,subnet_id,exposure,private_ip,name,state,last_operation_id) VALUES($1,$2,'test-cluster',$3,$4,$5,'private','10.42.1.100','old-lb','provisioning',$6)`, legacy.Tenant, lbID, ns, legacy.VPC, subnetID, opID)
		exec(`INSERT INTO network_operations(tenant_id,lb_id,operation_id,kind,state,created_at,updated_at) VALUES($1,$2,$3,'create_load_balancer','queued',clock_timestamp(),clock_timestamp())`, legacy.Tenant, lbID, opID)
		exec(`INSERT INTO network_lb_listeners(tenant_id,cluster_id,namespace,lb_id,listener_id,port) VALUES($1,'test-cluster',$2,$3,$4,80)`, legacy.Tenant, ns, lbID, listenerID)
		exec(`INSERT INTO network_lb_configurations(tenant_id,cluster_id,namespace,lb_id,config_version,name,description,interval_seconds,timeout_seconds,unhealthy_threshold,healthy_threshold,health_check_port) VALUES($1,'test-cluster',$2,$3,1,'old-lb','',5,3,3,1,8080)`, legacy.Tenant, ns, lbID)
		for _, kind := range []string{"route", "policy"} {
			exec(`INSERT INTO network_lb_components(tenant_id,cluster_id,namespace,lb_id,component_id,kind,provider_name,provider_uid) VALUES($1,'test-cluster',$2,$3,gen_random_uuid(),$4,$5,$6)`, legacy.Tenant, ns, lbID, kind, "legacy-"+kind, "legacy-"+kind+"-uid")
		}
		intent = biz.LoadBalancerIntent{TenantID: legacy.Tenant, Kind: "create_load_balancer", IdempotencyKey: "old-key", Name: "old-lb", VPCID: legacy.VPC, SubnetID: subnetID, Exposure: "private", Flavor: "small", PrivateIP: "10.42.1.100", ListenerPort: 80, Health: biz.LoadBalancerHealth{Port: 8080, IntervalSeconds: 5, TimeoutSeconds: 3, UnhealthyThreshold: 3, HealthyThreshold: 1}, Backends: []biz.LoadBalancerBackend{{SubnetID: subnetID, Address: "10.42.1.2", Port: 8080, Weight: 1}}}
		old := biz.LoadBalancerResult{LoadBalancer: biz.LoadBalancer{EgressMetadata: biz.EgressMetadata{ID: lbID, Name: "old-lb", State: biz.Provisioning, Version: 1, LastOperationID: opID}, TenantID: legacy.Tenant, Listener: biz.LoadBalancerListener{ID: listenerID, Port: 80}, DesiredVersion: 1}, Operation: biz.Operation{ID: opID, TenantID: legacy.Tenant, ResourceID: lbID, Kind: "create_load_balancer", State: biz.Queued}}
		snapshot, err = json.Marshal(old)
		if err != nil {
			t.Fatal(err)
		}
		exec(`INSERT INTO network_idempotency(tenant_id,operation_kind,idempotency_key,fingerprint,fingerprint_version,lb_id,operation_id,response,created_at) VALUES($1,'create_load_balancer','old-key',$2,1,$3,$4,$5,clock_timestamp())`, legacy.Tenant, intent.Fingerprint(), lbID, opID, snapshot)
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	})
	var name string
	var port int
	if err := db.Owner.QueryRow(ctx, `SELECT l.name,c.port FROM network_lb_listeners l JOIN network_lb_configuration_listeners c USING(tenant_id,lb_id,listener_id) WHERE l.tenant_id=$1 AND l.lb_id=$2 AND l.listener_id=$3`, legacy.Tenant, lbID, listenerID).Scan(&name, &port); err != nil || name != "http" || port != 80 {
		t.Fatal("old listener was not preserved", name, port, err)
	}
	var count int
	if err := db.Owner.QueryRow(ctx, `SELECT count(*) FROM network_lb_components WHERE tenant_id=$1 AND lb_id=$2 AND listener_id=$3 AND provider_name='legacy-'||kind AND provider_uid='legacy-'||kind||'-uid'`, legacy.Tenant, lbID, listenerID).Scan(&count); err != nil || count != 2 {
		t.Fatal("old Route/Policy identity changed", count, err)
	}
	result, err := db.Repository.AcceptLoadBalancer(ctx, intent, biz.Attribution{})
	if err != nil {
		t.Fatal(err)
	}
	replayed, _ := json.Marshal(result)
	if string(replayed) != string(snapshot) {
		t.Fatal("old receipt replay changed")
	}
	// Ensure runtime readiness recognizes the appended tables and migration set.
	if err := db.Repository.CheckReady(ctx); err != nil {
		t.Fatal(err)
	}
}
