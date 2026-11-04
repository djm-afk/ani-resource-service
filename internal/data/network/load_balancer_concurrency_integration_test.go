package data_test

import (
	"context"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"sync"
	"testing"
	"time"
)

func TestLBConcurrentSNATClaimBothArrivalOrders(t *testing.T) {
	for _, first := range []string{"lb", "snat"} {
		t.Run(first, func(t *testing.T) {
			f := newLBAdmissionFixture(t)
			eip := f.f.eip(t, "contended-eip")
			r := f.request
			r.Exposure = "public"
			r.PublicEIPID = eip.ID
			r.PrivateIP = ""
			tx, err := f.f.owner.Begin(f.f.ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.Background())
			if _, err = tx.Exec(f.f.ctx, `SELECT vpc_id FROM network_vpcs WHERE tenant_id=$1 AND vpc_id=$2 FOR UPDATE`, f.f.tenant, f.vpc.ID); err != nil {
				t.Fatal(err)
			}
			results := map[string]chan error{"lb": make(chan error, 1), "snat": make(chan error, 1)}
			start := func(kind string) {
				go func() {
					var err error
					if kind == "lb" {
						_, err = f.lbs.Create(f.f.ctx, r)
					} else {
						_, err = f.f.e.BindVPCSnat(f.f.ctx, biz.EgressIntent{VPCID: f.vpc.ID, EIPID: eip.ID, IdempotencyKey: "concurrent-snat"})
					}
					results[kind] <- err
				}()
			}
			waitLocks := func(count int) {
				awaitNET05A(t, 3*time.Second, func() bool {
					var blocked int
					err := f.f.owner.QueryRow(f.f.ctx, `SELECT count(*) FROM pg_locks WHERE NOT granted AND pid IN (SELECT pid FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid())`).Scan(&blocked)
					return err == nil && blocked >= count
				})
			}
			start(first)
			waitLocks(1)
			second := "lb"
			if first == "lb" {
				second = "snat"
			}
			start(second)
			waitLocks(2)
			if err = tx.Commit(f.f.ctx); err != nil {
				t.Fatal(err)
			}
			if err = <-results[first]; err != nil {
				t.Fatal("first admission failed", err)
			}
			if err = <-results[second]; biz.ReasonOf(err) != biz.EIPInUse {
				t.Fatal("second claim did not lose", err)
			}
			// Both arrival orders retain the EIP projection contract previously
			// checked in a separate sequential admission fixture.
			value, err := f.f.e.GetEIP(f.f.ctx, "", eip.ID)
			if err != nil {
				t.Fatal(err)
			}
			if value.BindingState != "reserved" || value.BindingTarget == nil {
				t.Fatal("claim projection missing", value)
			}
			if first == "lb" && (value.BindingID != "" || value.BindingTarget.Kind != "load_balancer") {
				t.Fatal("legacy field made LB EIP appear free", value)
			}
			var count int
			if err = f.f.owner.QueryRow(f.f.ctx, `SELECT count(*) FROM network_eip_claims WHERE tenant_id=$1 AND eip_id=$2 AND released_at IS NULL`, f.f.tenant, eip.ID).Scan(&count); err != nil || count != 1 {
				t.Fatal("not one durable claim", count, err)
			}
		})
	}
}
func TestLBConcurrentVIPAndEntryDeletion(t *testing.T) {
	for _, race := range []string{"vip", "entry-delete"} {
		t.Run(race, func(t *testing.T) {
			f := newLBAdmissionFixture(t)
			var group sync.WaitGroup
			start := make(chan struct{})
			result := make([]error, 2)
			for i := 0; i < 2; i++ {
				group.Add(1)
				go func(i int) {
					defer group.Done()
					<-start
					if race == "entry-delete" && i == 1 {
						_, result[i] = f.f.n.DeleteSubnet(f.f.ctx, f.f.tenant, f.subnet.ID)
					} else {
						r := f.request
						r.IdempotencyKey = fmt.Sprintf("vip-%d", i)
						_, result[i] = f.lbs.Create(f.f.ctx, r)
					}
				}(i)
			}
			close(start)
			group.Wait()
			success := 0
			for _, err := range result {
				if err == nil {
					success++
				} else {
					reason := biz.ReasonOf(err)
					if reason != biz.VIPInUse && reason != biz.ParentNotReady && reason != biz.ResourceInUse {
						t.Fatal("unexpected race outcome", err)
					}
				}
			}
			if success != 1 {
				t.Fatal("conflicting requests were both accepted or both refused", result)
			}
		})
	}
}
func TestLBNewRelationsRejectCrossTenantSQLAndAPI(t *testing.T) {
	f := newLBAdmissionFixture(t, (&lbControllerFixture{}).http)
	r, err := f.lbs.Create(f.f.ctx, f.request)
	if err != nil {
		t.Fatal(err)
	}
	other := uuid.NewString()
	ns := "tenant-" + f.f.tenant
	for _, statement := range []string{
		`INSERT INTO network_lb_listeners(tenant_id,cluster_id,namespace,lb_id,listener_id,port) VALUES($1,'test-cluster',$2,$3,gen_random_uuid(),8080)`,
		`INSERT INTO network_lb_configurations(tenant_id,cluster_id,namespace,lb_id,config_version,name,description,interval_seconds,timeout_seconds,unhealthy_threshold,healthy_threshold) VALUES($1,'test-cluster',$2,$3,1,'foreign','',5,3,3,1)`,
		`INSERT INTO network_lb_components(tenant_id,cluster_id,namespace,lb_id,component_id,kind,provider_name,listener_id) VALUES($1,'test-cluster',$2,$3,gen_random_uuid(),'route','foreign-route',(SELECT listener_id FROM network_lb_listeners WHERE lb_id=$3 AND name='http'))`,
	} {
		_, err := f.f.owner.Exec(f.f.ctx, statement, other, ns, r.LoadBalancer.ID)
		var pg *pgconn.PgError
		if !errors.As(err, &pg) || pg.Code != "23503" {
			t.Fatal("cross-tenant FK did not reject", err)
		}
	}
	lbState(t, f, r.LoadBalancer.ID, biz.Available)
	for _, table := range []string{"network_load_balancers", "network_lb_listeners", "network_lb_configurations", "network_lb_members", "network_lb_configuration_members", "network_lb_configuration_listeners", "network_lb_listener_members", "network_lb_subnet_refs", "network_lb_components", "network_lb_vip_intents", "network_provider_bindings", "network_operations", "network_reconciliations", "network_idempotency", "network_resource_history"} {
		t.Run(table, func(t *testing.T) {
			var count int
			if err := f.f.owner.QueryRow(f.f.ctx, "SELECT count(*) FROM "+table+" WHERE tenant_id=$1 AND lb_id=$2", f.f.tenant, r.LoadBalancer.ID).Scan(&count); err != nil || count == 0 {
				t.Fatal("missing relation fixture", table, count, err)
			}
			_, err := f.f.owner.Exec(f.f.ctx, "UPDATE "+table+" SET tenant_id=$3 WHERE tenant_id=$1 AND lb_id=$2", f.f.tenant, r.LoadBalancer.ID, other)
			var pg *pgconn.PgError
			if !errors.As(err, &pg) || pg.Code != "23503" {
				t.Fatal("LB tenant relation could be detached", table, err)
			}
		})
	}
	ctx := biz.WithEgressCaller(context.Background(), biz.EgressCaller{TenantID: other, Attribution: biz.Attribution{Actor: "other", DirectCaller: "fixture"}})
	if _, err = f.lbs.Get(ctx, "", r.LoadBalancer.ID); biz.ReasonOf(err) != biz.ResourceNotFound {
		t.Fatal(err)
	}
	if _, err = f.lbs.Delete(ctx, "", r.LoadBalancer.ID); biz.ReasonOf(err) != biz.ResourceNotFound {
		t.Fatal(err)
	}
	if _, err = f.lbs.GetOperation(ctx, "", r.Operation.ID); biz.ReasonOf(err) != biz.ResourceNotFound {
		t.Fatal(err)
	}
	list, _, _, err := f.lbs.List(ctx, biz.ListLoadBalancers{ListVPCs: biz.ListVPCs{Limit: 20}})
	if err != nil || len(list) != 0 {
		t.Fatal("tenant list leaked", list, err)
	}
	if _, err = f.lbs.Update(ctx, biz.UpdateLoadBalancer{ID: r.LoadBalancer.ID, ExpectedVersion: r.LoadBalancer.Version, IdempotencyKey: "foreign-update", LoadBalancerMutableInput: f.request.LoadBalancerMutableInput}); biz.ReasonOf(err) != biz.ResourceNotFound {
		t.Fatal(err)
	}
}

func TestLBUpdateContendsWithParentDeletionInBothOrders(t *testing.T) {
	for _, parent := range []string{"vpc", "entry-subnet"} {
		for _, first := range []string{"update", "delete"} {
			t.Run(parent+"/"+first+"-first", func(t *testing.T) {
				f := newLBAdmissionFixture(t, (&lbControllerFixture{}).http)
				accepted, err := f.lbs.Create(f.f.ctx, f.request)
				if err != nil {
					t.Fatal(err)
				}
				lb := lbState(t, f, accepted.LoadBalancer.ID, biz.Available)
				update := biz.UpdateLoadBalancer{ID: lb.ID, ExpectedVersion: lb.Version, IdempotencyKey: "contended-update", LoadBalancerMutableInput: biz.LoadBalancerMutableInput{Health: f.request.Health, Name: "updated", Backends: []biz.LoadBalancerBackendInput{{ID: lb.Backends[0].ID, SubnetID: lb.Backends[0].SubnetID, Address: lb.Backends[0].Address, Port: lb.Backends[0].Port}}}}
				tx, err := f.f.owner.Begin(f.f.ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback(context.Background())
				if _, err = tx.Exec(f.f.ctx, `SELECT vpc_id FROM network_vpcs WHERE tenant_id=$1 AND vpc_id=$2 FOR UPDATE`, f.f.tenant, f.vpc.ID); err != nil {
					t.Fatal(err)
				}
				var updated biz.LoadBalancerResult
				results := map[string]chan error{"update": make(chan error, 1), "delete": make(chan error, 1)}
				start := func(kind string) {
					go func() {
						var e error
						if kind == "update" {
							updated, e = f.lbs.Update(f.f.ctx, update)
						} else if parent == "vpc" {
							_, e = f.f.n.DeleteVPC(f.f.ctx, f.f.tenant, f.vpc.ID)
						} else {
							_, e = f.f.n.DeleteSubnet(f.f.ctx, f.f.tenant, f.subnet.ID)
						}
						results[kind] <- e
					}()
				}
				waitLocks := func(count int) {
					awaitNET05A(t, 3*time.Second, func() bool {
						var blocked int
						e := f.f.owner.QueryRow(f.f.ctx, `SELECT count(*) FROM pg_locks WHERE NOT granted AND pid IN (SELECT pid FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid())`).Scan(&blocked)
						return e == nil && blocked >= count
					})
				}
				start(first)
				waitLocks(1)
				second := "update"
				if first == "update" {
					second = "delete"
				}
				start(second)
				waitLocks(2)
				if err = tx.Commit(f.f.ctx); err != nil {
					t.Fatal(err)
				}
				if err = <-results["update"]; err != nil {
					t.Fatal("referenced-parent update rejected", err)
				}
				if err = <-results["delete"]; biz.ReasonOf(err) != biz.ResourceInUse {
					t.Fatal("referenced parent deletion accepted", err)
				}
				if updated.LoadBalancer.DesiredVersion != lb.DesiredVersion+1 || updated.LoadBalancer.ProviderTargetVersion != lb.ProviderTargetVersion {
					t.Fatal("metadata update changed the Provider configuration", updated)
				}
				f.f.drive(t, func() bool {
					current, e := f.lbs.Get(f.f.ctx, "", lb.ID)
					return e == nil && current.DesiredVersion == updated.LoadBalancer.DesiredVersion &&
						current.ProviderTargetVersion == updated.LoadBalancer.ProviderTargetVersion &&
						current.AppliedVersion == updated.LoadBalancer.ProviderTargetVersion && current.Name == update.Name &&
						current.ConfigurationState == "configured" && current.State == biz.Available
				})
				v, e := f.f.n.GetVPC(f.f.ctx, f.f.tenant, f.vpc.ID)
				if e != nil || (v.State != biz.Available && !(v.State == biz.Degraded && v.Reason == biz.BaseConnectivityNotReady)) {
					t.Fatal("parent was closed during update", v, e)
				}
				s, e := f.f.n.GetSubnet(f.f.ctx, f.f.tenant, f.subnet.ID)
				if e != nil || s.State != biz.Available {
					t.Fatal("entry was closed during update", s, e)
				}
				if _, err = f.lbs.Delete(f.f.ctx, "", lb.ID); err != nil {
					t.Fatal(err)
				}
				lbState(t, f, lb.ID, biz.Deleted)
			})
		}
	}
}

func TestLBRejectsSystemIntranetEIPWithoutChangingBaseClaim(t *testing.T) {
	f := newLBAdmissionFixture(t)
	eip, snat := baseIDs(t, f.f, f.vpc.ID)
	request := f.request
	request.Exposure, request.PublicEIPID, request.PrivateIP = "public", eip, ""
	if _, err := f.lbs.Create(f.f.ctx, request); biz.ReasonOf(err) != biz.ResourceNotFound {
		t.Fatal("system EIP accepted by tenant LB", err)
	}
	var target string
	if err := f.f.owner.QueryRow(f.f.ctx, `SELECT snat_id FROM network_eip_claims WHERE tenant_id=$1 AND eip_id=$2 AND released_at IS NULL`, f.f.tenant, eip).Scan(&target); err != nil || target != snat {
		t.Fatal("base claim was changed", target, err)
	}
	var count int
	if err := f.f.owner.QueryRow(f.f.ctx, `SELECT count(*) FROM network_load_balancers WHERE tenant_id=$1`, f.f.tenant).Scan(&count); err != nil || count != 0 {
		t.Fatal("rejected system EIP left a partial LB", count, err)
	}
}
