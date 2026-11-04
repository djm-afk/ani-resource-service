package data_test

import (
	"context"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"strings"
	"sync"
	"testing"
	"time"
)

// Only the Kubernetes/NIC boundary is replaced. Authorization, acceptance,
// transactions, operations, replay, leases and both workers are production code.
type switchBoundary struct {
	testEgressInfrastructure
	mu          sync.Mutex
	applied     map[string]bool
	lose        bool
	unavailable bool
}

func (b *switchBoundary) VirtualSwitchInventory(_ context.Context, cluster string, sw biz.VirtualSwitch, device string) (biz.VirtualSwitchInventory, error) {
	if b.unavailable {
		return biz.VirtualSwitchInventory{}, biz.Fail(biz.DependencyUnavailable, "facts unavailable")
	}
	members := []biz.VirtualSwitchNode{{NodeName: "n1", NodeUID: "uid1", Priority: 100, Allocated: true}, {NodeName: "n2", NodeUID: "uid2", Priority: 100, Allocated: true}}
	items := []biz.PreparedDeviceNode{}
	for _, n := range members {
		items = append(items, biz.PreparedDeviceNode{NodeName: n.NodeName, NodeUID: n.NodeUID, Name: device})
	}
	return biz.VirtualSwitchInventory{Members: members, Fingerprint: strings.Repeat("a", 64), Inventory: biz.PreparedDeviceInventory{Items: items, Fingerprint: strings.Repeat("b", 64)}}, nil
}
func (b *switchBoundary) InspectVirtualSwitchNode(_ context.Context, sw biz.VirtualSwitch, n biz.VirtualSwitchNode) (biz.VirtualSwitchNode, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n.ResourceVersion = "rv1"
	if b.applied[n.NodeUID] {
		n.Applied = true
		n.Unknown = false
		n.State = "applied"
	} else if sw.State == biz.Deleting {
		n.Applied = false
		n.Unknown = false
		n.State = "cleaned"
	}
	return n, nil
}
func (b *switchBoundary) PatchVirtualSwitchNode(_ context.Context, sw biz.VirtualSwitch, n biz.VirtualSwitchNode, deleting bool) (biz.VirtualSwitchNode, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !n.Prepared {
		return n, errors.New("missing durable prepare")
	}
	if b.applied == nil {
		b.applied = map[string]bool{}
	}
	b.applied[n.NodeUID] = !deleting
	if b.lose {
		b.lose = false
		return n, &biz.ProviderError{Kind: biz.ProviderUncertain}
	}
	n.Unknown = false
	n.Applied = !deleting
	n.State = "applied"
	if deleting {
		n.State = "cleaned"
	}
	return n, nil
}
func switchEgress(t *testing.T) (*egressFixture, *switchBoundary) {
	t.Helper()
	p, owner := database(t)
	boundary := &switchBoundary{}
	p.UseVirtualSwitchInfrastructure(boundary)
	e, err := biz.NewEgress(p, boundary, biz.ContextEgressAuthorization{}, []byte(strings.Repeat("s", 32)), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	ctx := biz.WithEgressCaller(context.Background(), biz.EgressCaller{PlatformAdministrator: true, Attribution: biz.Attribution{Actor: "platform-user", DirectCaller: "gov", CorrelationID: "switch-test"}})
	provider := &egressProvider{}
	return &egressFixture{p: p, owner: owner, e: e, ctx: ctx, provider: provider, w: egressWorker(t, p, provider)}, boundary
}

func TestVirtualSwitchDurableTwoStepAcceptanceAndRecovery(t *testing.T) {
	f, boundary := switchEgress(t)
	sw, err := f.e.CreateVirtualSwitch(f.ctx, biz.VirtualSwitchIntent{Name: "test-group", IdempotencyKey: "switch-create"})
	if err != nil {
		t.Fatal(err)
	}
	if sw.ClusterID != "" || sw.BindingID != "" || sw.BindingState != "unbound" || sw.State != biz.Available || len(sw.Nodes) != 0 {
		t.Fatalf("invalid unbound switch: %+v", sw)
	}
	op, err := f.e.GetPlatformOperation(f.ctx, sw.LastOperationID)
	if err != nil || op.State != biz.Succeeded || op.ResourceID != sw.ID {
		t.Fatal(op, err)
	}
	input := biz.PlatformIntent{Kind: "create_vlan", Name: "first", IdempotencyKey: "vlan-first", Vlan: &biz.VlanConfig{VlanID: 0, ClusterID: "test-cluster", VirtualSwitchID: sw.ID, DeviceName: "ens35"}}
	vlan, err := f.e.CreatePlatform(f.ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if vlan.Vlan.DeviceID == "" || vlan.Vlan.VirtualSwitchID != sw.ID || len(vlan.Vlan.Nodes) != 2 {
		t.Fatal(vlan)
	}
	// Replay precedes a now-unavailable external inventory; it returns the exact
	// acceptance snapshot, without a second registration or operation.
	boundary.unavailable = true
	replayed, err := f.e.CreatePlatform(f.ctx, input)
	if err != nil || replayed.ID != vlan.ID || replayed.LastOperationID != vlan.LastOperationID {
		t.Fatal(replayed, err)
	}
	boundary.unavailable = false
	changed := input
	copyVlan := *input.Vlan
	copyVlan.VlanID = 1
	changed.Vlan = &copyVlan
	if _, err = f.e.CreatePlatform(f.ctx, changed); err == nil {
		t.Fatal("v2 conflicting replay admitted")
	}
	if _, err = f.e.DeleteVirtualSwitch(f.ctx, sw.ID); err == nil {
		t.Fatal("referenced switch deleted")
	}
	if _, err = f.w.Step(f.ctx); err != nil {
		t.Fatal(err)
	}
	if f.provider.creates != 0 {
		t.Fatal("VLAN created before group configuration")
	}
	policy := biz.DefaultWorkerPolicy()
	policy.ObserveEvery = 10 * time.Millisecond
	policy.RetryMin = time.Millisecond
	boundary.lose = true
	worker, err := biz.NewVirtualSwitchWorker(f.p, boundary, uuid.NewString(), policy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = worker.Step(f.ctx); err != nil {
		t.Fatal(err)
	}
	progress, err := f.e.GetVirtualSwitch(f.ctx, sw.ID)
	if err != nil || !progress.Nodes[0].Unknown || !progress.Nodes[0].Prepared {
		t.Fatal(progress, err)
	}
	worker, err = biz.NewVirtualSwitchWorker(f.p, boundary, uuid.NewString(), policy)
	if err != nil {
		t.Fatal(err)
	}
	driveSwitch := func(want string) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := worker.Step(f.ctx); err != nil {
				t.Fatal(err)
			}
			got, err := f.e.GetVirtualSwitch(f.ctx, sw.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.BindingState == want {
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
		t.Fatal("switch did not converge to " + want)
	}
	driveSwitch("configured")
	f.drive(t, func() bool {
		v, err := f.e.GetPlatform(f.ctx, "vlan", vlan.ID)
		return err == nil && v.State == biz.Available
	})
	if _, err = f.e.DeletePlatform(f.ctx, "vlan", vlan.ID); err != nil {
		t.Fatal(err)
	}
	f.drive(t, func() bool {
		v, err := f.e.GetPlatform(f.ctx, "vlan", vlan.ID)
		return err == nil && v.State == biz.Deleted
	})
	if _, err = f.e.DeleteVirtualSwitch(f.ctx, sw.ID); err != nil {
		t.Fatal(err)
	}
	driveSwitch("cleaned")
	var active int
	if err = f.owner.QueryRow(f.ctx, "SELECT count(*) FROM network_device_adoptions WHERE virtual_switch_id=$1 AND NOT retired", sw.ID).Scan(&active); err != nil || active != 0 {
		t.Fatal(active, err)
	}
	// A deleted group's name can be reused only after its label cleanup completed.
	newSwitch, err := f.e.CreateVirtualSwitch(f.ctx, biz.VirtualSwitchIntent{Name: sw.Name, IdempotencyKey: "switch-recreate"})
	if err != nil || newSwitch.ID == sw.ID {
		t.Fatal(newSwitch, err)
	}
}

func TestVirtualSwitchDeleteWaitsForOldClaimLeaseExpiry(t *testing.T) {
	f, boundary := switchEgress(t)
	sw, err := f.e.CreateVirtualSwitch(f.ctx, biz.VirtualSwitchIntent{Name: "lease-drain", IdempotencyKey: "lease-drain-create"})
	if err != nil {
		t.Fatal(err)
	}
	vlan, err := f.e.CreatePlatform(f.ctx, biz.PlatformIntent{Kind: "create_vlan", Name: "lease-drain-vlan", IdempotencyKey: "lease-drain-vlan", Vlan: &biz.VlanConfig{VlanID: 0, ClusterID: "test-cluster", VirtualSwitchID: sw.ID, DeviceName: "ens35"}})
	if err != nil {
		t.Fatal(err)
	}
	policy := biz.DefaultWorkerPolicy()
	if policy.Lease != 20*time.Second {
		t.Fatal("this regression exercises the real 20s lease", policy.Lease)
	}
	// Only ordinary reconciliation cadence is shortened, as in the existing
	// switch lifecycle fixture. The lease and request timeout remain unchanged.
	policy.ObserveEvery, policy.RetryMin = 10*time.Millisecond, time.Millisecond
	worker, err := biz.NewVirtualSwitchWorker(f.p, boundary, uuid.NewString(), policy)
	if err != nil {
		t.Fatal(err)
	}
	driveSwitch := func(want string) {
		t.Helper()
		awaitNET05A(t, 3*time.Second, func() bool {
			if _, err := worker.Step(f.ctx); err != nil {
				t.Fatal(err)
			}
			v, err := f.e.GetVirtualSwitch(f.ctx, sw.ID)
			return err == nil && v.BindingState == want
		})
	}
	driveSwitch("configured")
	f.drive(t, func() bool {
		v, err := f.e.GetPlatform(f.ctx, "vlan", vlan.ID)
		return err == nil && v.State == biz.Available
	})
	if _, err = f.e.DeletePlatform(f.ctx, "vlan", vlan.ID); err != nil {
		t.Fatal(err)
	}
	f.drive(t, func() bool {
		v, err := f.e.GetPlatform(f.ctx, "vlan", vlan.ID)
		return err == nil && v.State == biz.Deleted
	})

	var old biz.VirtualSwitchWork
	oldOwner := uuid.NewString()
	awaitNET05A(t, 3*time.Second, func() bool {
		var found bool
		old, found, err = f.p.ClaimVirtualSwitch(f.ctx, oldOwner, policy.Lease)
		if err != nil {
			t.Fatal(err)
		}
		return found
	})
	type facts struct {
		version, epoch                                     int64
		state, bindingState, bindingID, operation, opState string
		owner, nodes                                       string
		leaseUntil, completed                              *time.Time
		nextRun, now                                       time.Time
	}
	readFacts := func() facts {
		t.Helper()
		var v facts
		if err := f.owner.QueryRow(f.ctx, `SELECT s.version,s.state,s.binding_state,s.binding_id::text,s.last_operation_id::text,o.state,
coalesce(r.lease_owner::text,''),r.lease_epoch,r.lease_until,r.next_run_at,clock_timestamp(),o.completed_at,s.nodes::text
FROM network_virtual_switches s JOIN network_virtual_switch_reconciliations r USING(resource_id)
JOIN network_virtual_switch_operations o ON o.operation_id=s.last_operation_id WHERE s.resource_id=$1`, sw.ID).Scan(&v.version, &v.state, &v.bindingState, &v.bindingID, &v.operation, &v.opState, &v.owner, &v.epoch, &v.leaseUntil, &v.nextRun, &v.now, &v.completed, &v.nodes); err != nil {
			t.Fatal(err)
		}
		return v
	}
	claimed := readFacts()
	if claimed.owner != old.Owner || claimed.epoch != old.Epoch || claimed.leaseUntil == nil || !claimed.leaseUntil.After(claimed.now) {
		t.Fatalf("old claim was not durably leased: %+v", claimed)
	}
	deleted, err := f.e.DeleteVirtualSwitch(f.ctx, sw.ID)
	if err != nil {
		t.Fatal(err)
	}
	accepted := readFacts()
	if deleted.ID != old.Switch.ID || accepted.version != old.Switch.Version+1 || accepted.state != "deleting" || accepted.bindingState != "cleaning" || accepted.opState != "queued" || accepted.completed != nil || accepted.nextRun.After(accepted.now) || accepted.bindingID != old.Switch.BindingID {
		t.Fatalf("delete did not schedule the new version: %+v", accepted)
	}
	assertBlocked := func(checkpoint string) {
		t.Helper()
		if err := f.p.FinishVirtualSwitch(f.ctx, old, biz.VirtualSwitchProgress{BindingState: "configured", Observed: true}, policy.ObserveEvery); !errors.Is(err, biz.ErrLeaseLost) {
			t.Fatalf("%s: stale Finish accepted: %v", checkpoint, err)
		}
		if err := f.p.SaveVirtualSwitchNode(f.ctx, old, 0, old.Switch.Nodes[0]); !errors.Is(err, biz.ErrLeaseLost) {
			t.Fatalf("%s: stale node observation accepted: %v", checkpoint, err)
		}
		v := readFacts()
		if v.version != accepted.version || v.state != accepted.state || v.bindingState != accepted.bindingState || v.operation != accepted.operation || v.opState != "queued" || v.nodes != accepted.nodes || v.owner != claimed.owner || v.epoch != claimed.epoch || v.leaseUntil == nil || !v.leaseUntil.Equal(*claimed.leaseUntil) || !v.leaseUntil.After(v.now) || v.nextRun.After(v.now) {
			t.Fatalf("%s: old work changed intent or released the live lease: %+v", checkpoint, v)
		}
		if worked, err := worker.Step(f.ctx); err != nil || worked {
			t.Fatalf("%s: new worker claimed before natural expiry: worked=%v err=%v", checkpoint, worked, err)
		}
		t.Logf("%s: state=%s binding=%s operation=%s old_epoch=%d lease_remaining=%s next_run_due=true", checkpoint, v.state, v.bindingState, v.opState, v.epoch, v.leaseUntil.Sub(v.now))
	}
	assertBlocked("after-delete")
	checkpoint15 := old.Now.Add(15 * time.Second)
	for {
		v := readFacts()
		if !v.now.Before(checkpoint15) {
			break
		}
		time.Sleep(min(100*time.Millisecond, checkpoint15.Sub(v.now)))
	}
	assertBlocked("at-15s")
	for {
		v := readFacts()
		if !v.now.Before(*claimed.leaseUntil) {
			break
		}
		time.Sleep(min(100*time.Millisecond, claimed.leaseUntil.Sub(v.now)))
	}
	// Step performs the next claim itself; a manual new claim here would add
	// another lease and would no longer exercise the natural recovery path.
	driveSwitch("cleaned")
	final := readFacts()
	if final.version != accepted.version || final.bindingID != accepted.bindingID || final.state != "deleted" || final.bindingState != "cleaned" || final.operation != accepted.operation || final.opState != "succeeded" || final.completed == nil || final.completed.Before(*claimed.leaseUntil) || final.epoch <= old.Epoch || final.owner != "" || final.leaseUntil != nil {
		t.Fatalf("same object did not finish after natural expiry: %+v", final)
	}
	if err := f.p.FinishVirtualSwitch(f.ctx, old, biz.VirtualSwitchProgress{BindingState: "configured", Observed: true}, policy.ObserveEvery); !errors.Is(err, biz.ErrLeaseLost) {
		t.Fatal("late old Finish revived the deleted switch", err)
	}
	if err := f.p.SaveVirtualSwitchNode(f.ctx, old, 0, old.Switch.Nodes[0]); !errors.Is(err, biz.ErrLeaseLost) {
		t.Fatal("late old node observation revived the deleted switch", err)
	}
	afterLate := readFacts()
	if afterLate.version != final.version || afterLate.state != final.state || afterLate.bindingState != final.bindingState || afterLate.operation != final.operation || afterLate.nodes != final.nodes || afterLate.epoch != final.epoch {
		t.Fatal("late old work changed completed deletion", afterLate)
	}
	var active int
	if err := f.owner.QueryRow(f.ctx, "SELECT count(*) FROM network_device_adoptions WHERE virtual_switch_id=$1 AND NOT retired", sw.ID).Scan(&active); err != nil || active != 0 {
		t.Fatal("product deletion retained active adoptions", active, err)
	}
	t.Logf("natural-expiry deletion completed: version=%d old_epoch=%d final_epoch=%d delete_elapsed=%s", final.version, old.Epoch, final.epoch, final.completed.Sub(accepted.now))
}

func TestVirtualSwitchScopePagingAndAtomicConcurrentVlan(t *testing.T) {
	f, _ := switchEgress(t)
	if _, err := f.e.CreateVirtualSwitch(context.Background(), biz.VirtualSwitchIntent{Name: "denied", IdempotencyKey: "denied"}); err == nil {
		t.Fatal("unauthenticated platform create allowed")
	}
	for _, name := range []string{"has.dot", "-bad", strings.Repeat("a", 64)} {
		if _, err := f.e.CreateVirtualSwitch(f.ctx, biz.VirtualSwitchIntent{Name: name, IdempotencyKey: name}); err == nil {
			t.Fatal("invalid group name allowed", name)
		}
	}
	var sw biz.VirtualSwitch
	for j := 0; j < 5; j++ {
		var err error
		sw, err = f.e.CreateVirtualSwitch(f.ctx, biz.VirtualSwitchIntent{Name: fmt.Sprintf("group-%d", j), IdempotencyKey: fmt.Sprintf("key-%d", j)})
		if err != nil {
			t.Fatal(err)
		}
	}
	items, total, err := f.e.ListVirtualSwitches(f.ctx, biz.VirtualSwitchFilter{Page: 3, PageSize: 2})
	if err != nil || total != 5 || len(items) != 1 {
		t.Fatal(items, total, err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for j := 0; j < 2; j++ {
		go func(index int) {
			<-start
			_, err := f.e.CreatePlatform(f.ctx, biz.PlatformIntent{Kind: "create_vlan", Name: "same-slot", IdempotencyKey: fmt.Sprintf("concurrent-%d", index), Vlan: &biz.VlanConfig{VlanID: 5, ClusterID: "test-cluster", VirtualSwitchID: sw.ID, DeviceName: "ens35"}})
			results <- err
		}(j)
	}
	close(start)
	success := 0
	for j := 0; j < 2; j++ {
		if <-results == nil {
			success++
		}
	}
	if success != 1 {
		t.Fatal("concurrent VLAN successes", success)
	}
	for table, want := range map[string]int{"network_device_adoptions": 1, "network_vlan_networks": 1, "network_platform_idempotency": 1} {
		var count int
		if err = f.owner.QueryRow(f.ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil || count != want {
			t.Fatal(table, count, err)
		}
	}
}
