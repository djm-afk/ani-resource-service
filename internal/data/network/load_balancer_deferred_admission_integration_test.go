package data_test

import (
	"context"
	"encoding/json"
	"github.com/google/uuid"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"reflect"
	"strings"
	"testing"
)

func deferredEmptyLBRequest(f *lbAdmissionFixture) biz.CreateLoadBalancer {
	request := f.request
	port := uint32(80)
	request.LoadBalancerMutableInput = biz.LoadBalancerMutableInput{Name: "deferred", Listeners: []biz.LoadBalancerListenerInput{{Name: "http", Protocol: "HTTP", Port: &port}}}
	return request
}

func assertLBProviderMutationCounts(t *testing.T, f *lbAdmissionFixture, before map[string]int64) {
	t.Helper()
	after, _ := f.api.Counts()
	for key, count := range after {
		if (strings.HasPrefix(key, "POST/") || strings.HasPrefix(key, "PUT/") || strings.HasPrefix(key, "PATCH/") || strings.HasPrefix(key, "DELETE/")) && count != before[key] {
			t.Fatalf("database-only operation mutated Provider %s: %d -> %d", key, before[key], count)
		}
	}
}

// Real authorization, admission and PG run with a Kubernetes protocol fixture.
// Parent read age and absent capability do not gate saving or activating intent.
func TestLBDeferredDatabaseAdmissionReplayOfflineAndDeletion(t *testing.T) {
	f := newLBAdmissionFixture(t)
	before, _ := f.api.Counts()
	if _, err := f.f.owner.Exec(f.f.ctx, `DELETE FROM network_lb_capabilities; UPDATE network_vpcs SET observed_at=clock_timestamp()-interval '2 hour'; UPDATE network_subnets SET observed_at=clock_timestamp()-interval '2 hour'`); err != nil {
		t.Fatal(err)
	}
	request := deferredEmptyLBRequest(f)
	accepted, err := f.lbs.Create(f.f.ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	lb := accepted.LoadBalancer
	if lb.State != biz.Provisioning || lb.ConfigurationState != "waiting_for_backends" || lb.DesiredVersion != 1 || lb.ProviderTargetVersion != 0 || lb.AppliedVersion != 0 || lb.ObservedAt != nil || accepted.Operation.State != biz.Succeeded || len(lb.Listeners) != 1 || lb.Listeners[0].DeploymentState != "waiting_for_backends" {
		t.Fatalf("invalid saved configuration receipt: %+v", accepted)
	}
	var occupied, components, waitingWork int
	if err = f.f.owner.QueryRow(f.f.ctx, `SELECT (SELECT count(*) FROM network_lb_vip_intents WHERE tenant_id=$1 AND lb_id=$2 AND released_at IS NULL), (SELECT count(*) FROM network_lb_components WHERE tenant_id=$1 AND lb_id=$2), (SELECT count(*) FROM network_reconciliations WHERE tenant_id=$1 AND lb_id=$2 AND next_run_at='infinity'::timestamptz)`, f.f.tenant, lb.ID).Scan(&occupied, &components, &waitingWork); err != nil || occupied != 1 || components != 0 || waitingWork != 1 {
		t.Fatal("incomplete reserved/waiting database state", occupied, components, waitingWork, err)
	}
	replay, err := f.lbs.Create(f.f.ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(accepted)
	b, _ := json.Marshal(replay)
	if string(a) != string(b) {
		t.Fatal("idempotent database receipt changed")
	}
	changed := request
	changed.Name = "other-intent"
	if _, err = f.lbs.Create(f.f.ctx, changed); biz.ReasonOf(err) != biz.IdempotencyConflict {
		t.Fatal("changed intent replay", err)
	}
	other := biz.WithEgressCaller(context.Background(), biz.EgressCaller{TenantID: uuid.NewString()})
	if _, err = f.lbs.Get(other, "", lb.ID); biz.ReasonOf(err) != biz.ResourceNotFound {
		t.Fatal("cross tenant read", err)
	}
	updated, err := f.lbs.Update(f.f.ctx, biz.UpdateLoadBalancer{ID: lb.ID, ExpectedVersion: lb.Version, IdempotencyKey: "empty-rename", LoadBalancerMutableInput: biz.LoadBalancerMutableInput{Name: "saved-again", UpdateMask: []string{"name"}}})
	if err != nil || updated.Operation.State != biz.Succeeded || updated.LoadBalancer.DesiredVersion != 2 || updated.LoadBalancer.ProviderTargetVersion != 0 || updated.LoadBalancer.ObservedAt != nil {
		t.Fatal("database edit did not complete", updated, err)
	}
	if _, err = f.lbs.Update(f.f.ctx, biz.UpdateLoadBalancer{ID: lb.ID, ExpectedVersion: lb.Version, IdempotencyKey: "old-version", LoadBalancerMutableInput: biz.LoadBalancerMutableInput{Name: "stale", UpdateMask: []string{"name"}}}); biz.ReasonOf(err) != biz.VersionConflict {
		t.Fatal("stale optimistic update", err)
	}
	// Activating this declaration does not require parent runtime acknowledgements.
	vpc := f.api.Object("vpcs", "tenant-"+f.f.tenant, strings.Replace(f.vpc.ID, "_", "-", 1))
	vpc["status"].(map[string]any)["conditions"] = []any{}
	f.api.Change("vpcs", vpc, false)
	activation := updated.LoadBalancer.Listeners[0]
	activationInput := biz.LoadBalancerListenerInput{ID: activation.ID, Name: activation.Name, Protocol: activation.Protocol, Port: &activation.Port, Backends: f.request.Backends}
	if _, err = f.lbs.Update(f.f.ctx, biz.UpdateLoadBalancer{ID: lb.ID, ExpectedVersion: updated.LoadBalancer.Version, IdempotencyKey: "offline-activation", LoadBalancerMutableInput: biz.LoadBalancerMutableInput{Listeners: []biz.LoadBalancerListenerInput{activationInput}, UpdateMask: []string{"listeners"}}}); err != nil {
		t.Fatal("parent runtime acknowledgement gated activation", err)
	}
	deleted, err := f.lbs.Delete(f.f.ctx, "", lb.ID)
	if err != nil || deleted.LoadBalancer.State != biz.Deleted || deleted.Operation.State != biz.Succeeded || deleted.LoadBalancer.ObservedAt != nil {
		t.Fatal("never-dispatched logical deletion", deleted, err)
	}
	replay, err = f.lbs.Create(f.f.ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	b, _ = json.Marshal(replay)
	if string(a) != string(b) {
		t.Fatal("logical deletion changed original acceptance replay")
	}
	var rows, released int
	if err = f.f.owner.QueryRow(f.f.ctx, `SELECT (SELECT count(*) FROM network_load_balancers WHERE tenant_id=$1 AND lb_id=$2), (SELECT count(*) FROM network_lb_vip_intents WHERE tenant_id=$1 AND lb_id=$2 AND released_at IS NOT NULL)`, f.f.tenant, lb.ID).Scan(&rows, &released); err != nil || rows != 1 || released != 1 {
		t.Fatal("logical deletion lost receipt or reservation", rows, released, err)
	}
	assertLBProviderMutationCounts(t, f, before)
}

func TestLBDeferredDatabaseAdmissionRollsBackWholeReceipt(t *testing.T) {
	f := newLBAdmissionFixture(t)
	before, _ := f.api.Counts()
	if _, err := f.f.owner.Exec(f.f.ctx, `CREATE FUNCTION reject_deferred_history() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected deferred admission history failure'; END $$; CREATE TRIGGER reject_deferred_history BEFORE INSERT ON network_resource_history FOR EACH ROW EXECUTE FUNCTION reject_deferred_history()`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.lbs.Create(f.f.ctx, deferredEmptyLBRequest(f)); err == nil {
		t.Fatal("injected atomic acceptance failure passed")
	}
	for _, table := range []string{"network_load_balancers", "network_lb_configurations", "network_lb_listeners", "network_lb_subnet_refs", "network_lb_vip_intents", "network_idempotency", "network_operations"} {
		var count int
		query := "SELECT count(*) FROM " + table + " WHERE tenant_id=$1"
		if table == "network_idempotency" || table == "network_operations" {
			query += " AND lb_id IS NOT NULL"
		}
		if err := f.f.owner.QueryRow(f.f.ctx, query, f.f.tenant).Scan(&count); err != nil || count != 0 {
			t.Fatal("failed admission left partial receipt", table, count, err)
		}
	}
	assertLBProviderMutationCounts(t, f, before)
}

func TestLBDeferredUnknownCreateCannotUseDatabaseDeletionShortcut(t *testing.T) {
	f := newLBAdmissionFixture(t)
	accepted, err := f.lbs.Create(f.f.ctx, deferredEmptyLBRequest(f))
	if err != nil {
		t.Fatal(err)
	}
	// Inject the exact persistent unknown-request evidence. This is a recovery
	// negative test, not evidence that a Provider CREATE was executed.
	if _, err = f.f.owner.Exec(f.f.ctx, `UPDATE network_provider_bindings SET create_dispatched=true,pending_action='create',pending_since=clock_timestamp() WHERE tenant_id=$1 AND lb_id=$2`, f.f.tenant, accepted.LoadBalancer.ID); err != nil {
		t.Fatal(err)
	}
	before, _ := f.api.Counts()
	deleted, err := f.lbs.Delete(f.f.ctx, "", accepted.LoadBalancer.ID)
	if err != nil || deleted.LoadBalancer.State != biz.Deleting || deleted.Operation.State != biz.Queued {
		t.Fatal("unknown CREATE bypassed cleanup", deleted, err)
	}
	var occupied int
	if err = f.f.owner.QueryRow(f.f.ctx, `SELECT count(*) FROM network_lb_vip_intents WHERE tenant_id=$1 AND lb_id=$2 AND released_at IS NULL`, f.f.tenant, accepted.LoadBalancer.ID).Scan(&occupied); err != nil || occupied != 1 {
		t.Fatal("unknown CREATE released VIP", occupied, err)
	}
	assertLBProviderMutationCounts(t, f, before)
}

func TestLBDeferredMixedDatabaseEditPreservesRuntimeFaultAndProjection(t *testing.T) {
	c := &lbControllerFixture{}
	f := newLBAdmissionFixture(t, c.http)
	created, err := f.lbs.Create(f.f.ctx, f.request)
	if err != nil {
		t.Fatal(err)
	}
	lb := lbState(t, f, created.LoadBalancer.ID, biz.Available)
	var namespace, gateway string
	if err = f.f.owner.QueryRow(f.f.ctx, `SELECT namespace,provider_name FROM network_provider_bindings WHERE tenant_id=$1 AND lb_id=$2`, f.f.tenant, lb.ID).Scan(&namespace, &gateway); err != nil {
		t.Fatal(err)
	}
	priorGateway := f.api.Object("gateways", namespace, gateway)
	before, _ := f.api.Counts()
	// Persist a known runtime fault and stale observation. A database-only edit
	// must not manufacture recovery, refresh health, or require fresh capability.
	if _, err = f.f.owner.Exec(f.f.ctx, `UPDATE network_load_balancers SET state='degraded',configuration_state='degraded',reason='PROVIDER_UNAVAILABLE',observed_at=clock_timestamp()-interval '2 hour' WHERE tenant_id=$1 AND lb_id=$2`, f.f.tenant, lb.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = f.f.owner.Exec(f.f.ctx, `DELETE FROM network_lb_capabilities`); err != nil {
		t.Fatal(err)
	}
	lb, err = f.lbs.Get(f.f.ctx, "", lb.ID)
	if err != nil {
		t.Fatal(err)
	}
	port := uint32(8081)
	active := lb.Listeners[0]
	weight := active.Backends[0].Weight
	backend := active.Backends[0]
	listeners := []biz.LoadBalancerListenerInput{{ID: active.ID, Name: active.Name, Protocol: active.Protocol, Port: &active.Port, Backends: []biz.LoadBalancerBackendInput{{ID: backend.ID, SubnetID: backend.SubnetID, Address: backend.Address, Port: backend.Port, Weight: &weight}}, Health: f.request.Health}, {Name: "waiting", Protocol: "HTTP", Port: &port}}
	result, err := f.lbs.Update(f.f.ctx, biz.UpdateLoadBalancer{ID: lb.ID, ExpectedVersion: lb.Version, IdempotencyKey: "mixed-empty", LoadBalancerMutableInput: biz.LoadBalancerMutableInput{Listeners: listeners, UpdateMask: []string{"listeners"}}})
	if err != nil {
		t.Fatal(err)
	}
	current := result.LoadBalancer
	if result.Operation.State != biz.Succeeded || current.DesiredVersion != 2 || current.ProviderTargetVersion != 1 || current.AppliedVersion != 1 || current.State != biz.Degraded || current.ConfigurationState != "degraded" || current.Reason != lb.Reason || current.ObservedAt == nil || !current.ObservedAt.Equal(*lb.ObservedAt) {
		t.Fatal("empty listener edit altered runtime evidence", result)
	}
	if !reflect.DeepEqual(priorGateway, f.api.Object("gateways", namespace, gateway)) {
		t.Fatal("database-only edit changed Gateway")
	}
	assertLBProviderMutationCounts(t, f, before)
	if current.Listeners[1].DeploymentState != "waiting_for_backends" {
		t.Fatal("empty listener not waiting", current.Listeners)
	}
	// GET is read-only even after runtime facts become stale.
	for j := 0; j < 2; j++ {
		if _, err = f.lbs.Get(f.f.ctx, "", lb.ID); err != nil {
			t.Fatal(err)
		}
	}
	assertLBProviderMutationCounts(t, f, before)
}

func TestLBDeferredSchemaProviderTargetAndImmutableRetirement(t *testing.T) {
	f := newLBAdmissionFixture(t)
	saved, err := f.lbs.Create(f.f.ctx, deferredEmptyLBRequest(f))
	if err != nil {
		t.Fatal(err)
	}
	tenant, id := f.f.tenant, saved.LoadBalancer.ID
	reject := func(query string, args ...any) {
		t.Helper()
		if _, err := f.f.owner.Exec(f.f.ctx, query, args...); err == nil {
			t.Fatal("invalid deferred schema state accepted", query)
		}
	}
	reject(`UPDATE network_load_balancers SET provider_target_version=0 WHERE tenant_id=$1 AND lb_id=$2`, tenant, id)
	reject(`UPDATE network_load_balancers SET applied_version=1 WHERE tenant_id=$1 AND lb_id=$2`, tenant, id)
	reject(`UPDATE network_load_balancers SET provider_target_version=2 WHERE tenant_id=$1 AND lb_id=$2`, tenant, id)
	var deferrable bool
	var definition string
	if err = f.f.owner.QueryRow(f.f.ctx, `SELECT condeferrable AND condeferred,pg_get_constraintdef(oid) FROM pg_constraint WHERE conname='network_lb_provider_target_configuration'`).Scan(&deferrable, &definition); err != nil || !deferrable || !strings.Contains(definition, "tenant_id, cluster_id, namespace, lb_id, provider_target_version") {
		t.Fatal("Provider target lost deferred tenant/placement/configuration FK", definition, err)
	}
	// These rows test constraints only; they do not claim runtime cleanup proof.
	var cluster, namespace, bindingID, providerName string
	if err = f.f.owner.QueryRow(f.f.ctx, `SELECT cluster_id,namespace,binding_id,provider_name FROM network_provider_bindings WHERE tenant_id=$1 AND lb_id=$2`, tenant, id).Scan(&cluster, &namespace, &bindingID, &providerName); err != nil {
		t.Fatal(err)
	}
	insert := `INSERT INTO network_lb_retired_bindings(tenant_id,cluster_id,namespace,lb_id,binding_id,provider_name,provider_uid,cleanup_target_version,operation_id,cleanup_completed_at) VALUES($1,$2,$3,$4,$5,$6,'retired-gateway',1,$7,clock_timestamp())`
	reject(insert, uuid.NewString(), cluster, namespace, id, bindingID, providerName, saved.Operation.ID)
	reject(insert, tenant, cluster, "foreign-namespace", id, bindingID, providerName, saved.Operation.ID)
	reject(insert, tenant, cluster, namespace, id, bindingID, providerName, f.vpc.LastOperationID)
	if _, err = f.f.owner.Exec(f.f.ctx, insert, tenant, cluster, namespace, id, bindingID, providerName, saved.Operation.ID); err != nil {
		t.Fatal(err)
	}
	reject(`UPDATE network_lb_retired_bindings SET provider_uid='replaced' WHERE tenant_id=$1 AND lb_id=$2`, tenant, id)
	reject(`DELETE FROM network_lb_retired_bindings WHERE tenant_id=$1 AND lb_id=$2`, tenant, id)
	if _, err = f.f.owner.Exec(f.f.ctx, `UPDATE network_operations SET state='failed' WHERE tenant_id=$1 AND operation_id=$2`, tenant, saved.Operation.ID); err == nil {
		t.Fatal("completed retirement lost succeeded operation FK")
	}
}

// An observer may legitimately increment the durable requested generation for
// an empty product. Claim must park that work from persistent facts, with no
// Provider read, synthetic observed_at, or polling loop.
func TestLBDeferredWaitingWakeParksWithoutProviderObservation(t *testing.T) {
	f := newLBAdmissionFixture(t)
	saved, err := f.lbs.Create(f.f.ctx, deferredEmptyLBRequest(f))
	if err != nil {
		t.Fatal(err)
	}
	f.f.stopObservation()
	if _, err = f.f.owner.Exec(f.f.ctx, `UPDATE network_reconciliations SET next_run_at='infinity'::timestamptz; UPDATE network_platform_reconciliations SET next_run_at='infinity'::timestamptz`); err != nil {
		t.Fatal(err)
	}
	before, _ := f.api.Counts()
	for j := 0; j < 2; j++ {
		if _, err = f.f.owner.Exec(f.f.ctx, `UPDATE network_reconciliations SET requested_generation=requested_generation+1,next_run_at=clock_timestamp() WHERE tenant_id=$1 AND lb_id=$2`, f.f.tenant, saved.LoadBalancer.ID); err != nil {
			t.Fatal(err)
		}
		worked, err := f.f.w.Step(f.f.ctx)
		if err != nil || worked {
			t.Fatal("waiting intent became Provider work", worked, err)
		}
		var parked bool
		if err = f.f.owner.QueryRow(f.f.ctx, `SELECT next_run_at='infinity'::timestamptz AND lease_owner IS NULL AND lease_until IS NULL FROM network_reconciliations WHERE tenant_id=$1 AND lb_id=$2`, f.f.tenant, saved.LoadBalancer.ID).Scan(&parked); err != nil || !parked {
			t.Fatal("waiting wake did not park its lease", parked, err)
		}
		current, err := f.lbs.Get(f.f.ctx, "", saved.LoadBalancer.ID)
		if err != nil || current.ObservedAt != nil || current.ProviderTargetVersion != 0 || current.AppliedVersion != 0 || current.ConfigurationState != "waiting_for_backends" {
			t.Fatal("waiting wake manufactured Provider evidence", current, err)
		}
	}
	after, _ := f.api.Counts()
	if !reflect.DeepEqual(before, after) {
		t.Fatal("waiting wake performed Provider methods", before, after)
	}
	if worked, err := f.f.w.Step(f.f.ctx); err != nil || worked {
		t.Fatal("parked waiting work entered repeated loop", worked, err)
	}
}

// Cleanup is established by the actual admission/PG/worker/KC adapter chain.
// The sole changed external fact is a subsequent IPAM allocation by another
// consumer; deleting the already-retired product must use its own release fact.
func TestLBDeferredRetiredProductDeletionIgnoresSubsequentExternalVIPAllocation(t *testing.T) {
	controller := &lbControllerFixture{}
	f := newLBAdmissionFixture(t, controller.http)
	eip := f.f.eip(t, "retired-deletion-eip")
	request := deferredEmptyLBRequest(f)
	request.Exposure, request.PublicEIPID = "public_private", eip.ID
	created, err := f.lbs.Create(f.f.ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	saved := created.LoadBalancer
	activated, err := f.lbs.Update(f.f.ctx, biz.UpdateLoadBalancer{ID: saved.ID, ExpectedVersion: saved.Version, IdempotencyKey: "retired-deletion-activate", LoadBalancerMutableInput: biz.LoadBalancerMutableInput{Listeners: []biz.LoadBalancerListenerInput{deferredListenerInput(saved.Listeners[0], f.request.Backends)}, UpdateMask: []string{"listeners"}}})
	if err != nil {
		t.Fatal(err)
	}
	running := deferredConfigured(t, f, saved.ID, 2)
	binding := deferredBinding(t, f, running.ID)
	if binding.uid == "" || activated.LoadBalancer.ProviderTargetVersion != 2 {
		t.Fatal("actual activation did not persist Gateway target", binding, activated)
	}
	cleared, err := f.lbs.Update(f.f.ctx, biz.UpdateLoadBalancer{ID: running.ID, ExpectedVersion: running.Version, IdempotencyKey: "retired-deletion-clear", LoadBalancerMutableInput: biz.LoadBalancerMutableInput{Listeners: []biz.LoadBalancerListenerInput{deferredListenerInput(running.Listeners[0], nil)}, UpdateMask: []string{"listeners"}}})
	if err != nil {
		t.Fatal(err)
	}
	var waiting biz.LoadBalancer
	f.f.drive(t, func() bool {
		waiting, err = f.lbs.Get(f.f.ctx, "", running.ID)
		return err == nil && waiting.ConfigurationState == "waiting_for_backends" && waiting.ProviderTargetVersion == 3 && waiting.AppliedVersion == 3
	})
	cleanupOperation, err := f.lbs.GetOperation(f.f.ctx, "", cleared.Operation.ID)
	if err != nil || cleanupOperation.State != biz.Succeeded {
		t.Fatal("actual cleanup did not complete", cleanupOperation, err)
	}
	var retirementBefore string
	var retiredUID string
	var cleanupTarget int64
	if err = f.f.owner.QueryRow(f.f.ctx, `SELECT to_jsonb(r)::text,provider_uid,cleanup_target_version FROM network_lb_retired_bindings r WHERE tenant_id=$1 AND lb_id=$2 AND binding_id=$3`, f.f.tenant, waiting.ID, binding.id).Scan(&retirementBefore, &retiredUID, &cleanupTarget); err != nil || retiredUID != binding.uid || cleanupTarget != 3 {
		t.Fatal("actual cleanup lacks matching immutable retirement", retiredUID, cleanupTarget, err)
	}
	f.f.stopObservation()
	var namespace, subnetName string
	if err = f.f.owner.QueryRow(f.f.ctx, `SELECT namespace,provider_name FROM network_provider_bindings WHERE tenant_id=$1 AND subnet_id=$2`, f.f.tenant, f.subnet.ID).Scan(&namespace, &subnetName); err != nil {
		t.Fatal(err)
	}
	subnet := f.api.Object("subnets", namespace, subnetName)
	if subnet == nil {
		t.Fatal("entry subnet missing after actual cleanup")
	}
	status := subnet["status"].(map[string]any)
	status["v4usingIPrange"] = waiting.PrivateIP
	status["v4availableIPrange"] = "10.42.1.2-10.42.1.99,10.42.1.101-10.42.1.254"
	f.api.Change("subnets", subnet, false)
	externalAllocation := f.api.Object("subnets", namespace, subnetName)
	backendPod := f.api.Object("pods", namespace, "lb-backend")
	before, _ := f.api.Counts()
	deleted, err := f.lbs.Delete(f.f.ctx, "", waiting.ID)
	if err != nil || deleted.LoadBalancer.State != biz.Deleted || deleted.Operation.State != biz.Succeeded {
		t.Fatal("retired product rechecked unrelated allocation instead of completing deletion", deleted, err)
	}
	if deleted.LoadBalancer.ObservedAt == nil || waiting.ObservedAt == nil || !deleted.LoadBalancer.ObservedAt.Equal(*waiting.ObservedAt) || deleted.LoadBalancer.ProviderTargetVersion != 3 || deleted.LoadBalancer.AppliedVersion != 3 {
		t.Fatal("database deletion changed actual retirement observation", deleted.LoadBalancer, waiting)
	}
	after, _ := f.api.Counts()
	if !reflect.DeepEqual(before, after) {
		t.Fatal("retired product deletion called Provider", before, after)
	}
	var retirementAfter string
	var unreleased int
	if err = f.f.owner.QueryRow(f.f.ctx, `SELECT to_jsonb(r)::text FROM network_lb_retired_bindings r WHERE tenant_id=$1 AND lb_id=$2 AND binding_id=$3`, f.f.tenant, waiting.ID, binding.id).Scan(&retirementAfter); err != nil || retirementAfter != retirementBefore {
		t.Fatal("product deletion changed immutable retirement", retirementAfter, err)
	}
	if err = f.f.owner.QueryRow(f.f.ctx, `SELECT (SELECT count(*) FROM network_lb_vip_intents WHERE tenant_id=$1 AND lb_id=$2 AND released_at IS NULL)+(SELECT count(*) FROM network_lb_subnet_refs WHERE tenant_id=$1 AND lb_id=$2 AND released_at IS NULL)+(SELECT count(*) FROM network_eip_claims WHERE tenant_id=$1 AND lb_id=$2 AND released_at IS NULL)`, f.f.tenant, waiting.ID).Scan(&unreleased); err != nil || unreleased != 0 {
		t.Fatal("retired product deletion retained product reservations", unreleased, err)
	}
	if !reflect.DeepEqual(externalAllocation, f.api.Object("subnets", namespace, subnetName)) || !reflect.DeepEqual(backendPod, f.api.Object("pods", namespace, "lb-backend")) {
		t.Fatal("product deletion changed other address consumer or business backend")
	}
	preservedEIP, err := f.f.e.GetEIP(f.f.ctx, "", eip.ID)
	if err != nil || preservedEIP.State == biz.Deleted || preservedEIP.State == biz.Deleting {
		t.Fatal("product deletion removed independent EIP", preservedEIP, err)
	}
	preservedVPC, err := f.f.n.GetVPC(f.f.ctx, f.f.tenant, f.vpc.ID)
	if err != nil || preservedVPC.State == biz.Deleted || preservedVPC.State == biz.Deleting {
		t.Fatal("product deletion removed parent VPC", preservedVPC, err)
	}
	replay, err := f.lbs.Create(f.f.ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	originalReceipt, _ := json.Marshal(created)
	replayedReceipt, _ := json.Marshal(replay)
	if string(originalReceipt) != string(replayedReceipt) {
		t.Fatal("retired product deletion changed initial acceptance replay")
	}
}
