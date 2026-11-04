package data_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	pb "github.com/zhangzhe-ctrl/ani-resource-service/api/network/v1"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	service "github.com/zhangzhe-ctrl/ani-resource-service/internal/service/network"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// This fixture exercises real Network acceptance, PostgreSQL, worker and service
// adapter. Only the external Kubernetes controller's allocation status is supplied.
func manualVNic(t *testing.T, f attachmentFixture, key string) biz.VNic {
	t.Helper()
	ctx := context.Background()
	v, e := f.n.CreateVNic(ctx, biz.CreateVNic{TenantID: f.r.TenantID, SubnetID: f.r.SubnetID, Name: "manual " + key, IdempotencyKey: key})
	if e != nil {
		t.Fatal(e)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, e = f.resourceWorker.Step(ctx); e != nil {
			t.Fatal(e)
		}
		got, e := f.n.GetVNic(ctx, v.TenantID, v.ID)
		if e != nil {
			t.Fatal(e)
		}
		if got.ProviderUID != "" {
			f.api.Mu.Lock()
			o := f.api.Objects["vnics/"+got.Namespace+"/"+got.ProviderName]
			if o == nil {
				f.api.Mu.Unlock()
				t.Fatal("worker lost original object")
			}
			meta := o["metadata"].(map[string]any)
			if _, ok := meta["ownerReferences"]; ok {
				f.api.Mu.Unlock()
				t.Fatal("manual VNIC has runtime owner")
			}
			annotations := meta["annotations"].(map[string]any)
			if annotations["networking.kubercloud.com/allocate_ip_with_dhcp"] != "false" {
				f.api.Mu.Unlock()
				t.Fatal("allocation annotation missing")
			}
			mac := v.ID[len(v.ID)-10:]
			o["spec"].(map[string]any)["macAddress"] = fmt.Sprintf("02:%s:%s:%s:%s:%s", mac[0:2], mac[2:4], mac[4:6], mac[6:8], mac[8:10])
			o["status"] = map[string]any{"primaryIP": "10.42.1.42", "ips": map[string]any{"allocated-ip": "10.42.1.42"}, "conditions": []any{map[string]any{"type": "Valid", "status": "True"}, map[string]any{"type": "Initialized", "status": "True"}, map[string]any{"type": "Ready", "status": "True"}}}
			f.api.Mu.Unlock()
			if e := f.p.NotifyResource(ctx, got.TenantID, got.ID); e != nil {
				t.Fatal(e)
			}
			for time.Now().Before(deadline) {
				time.Sleep(2 * time.Millisecond)
				if _, e = f.resourceWorker.Step(ctx); e != nil {
					t.Fatal(e)
				}
				got, e = f.n.GetVNic(ctx, v.TenantID, v.ID)
				if e != nil {
					t.Fatal(e)
				}
				if got.State == biz.Available {
					if got.PrimaryIP != "10.42.1.42" || got.MACAddress == "" || len(got.IPs) != 1 {
						t.Fatal("allocation facts were not projected", got)
					}
					return got
				}
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("VNIC did not allocate")
	return biz.VNic{}
}
func TestManualVNicPGReplayReservationRetentionAndDelete(t *testing.T) {
	f := newAttachmentFixture(t)
	ctx := context.Background()
	v := manualVNic(t, f, "manual")
	replay, e := f.n.CreateVNic(ctx, biz.CreateVNic{TenantID: f.r.TenantID, SubnetID: f.r.SubnetID, Name: "manual manual", IdempotencyKey: "manual"})
	if e != nil || replay.ID != v.ID || replay.LastOperationID != v.LastOperationID {
		t.Fatal("replay lost durable identity", replay, e)
	}
	if _, e = f.n.GetVNic(ctx, uuid.NewString(), v.ID); biz.ReasonOf(e) != biz.ResourceNotFound {
		t.Fatal("cross tenant VNIC read", e)
	}
	r := biz.VNicReservation{TenantID: v.TenantID, VNicID: v.ID, ConsumerKind: "vm", ConsumerID: "vm-one", Slot: "primary", OperationID: uuid.NewString(), SubnetID: v.SubnetID, Revision: 1}
	reserved, e := f.n.ReserveVNic(ctx, r)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.n.ReserveVNic(ctx, r); e != nil {
		t.Fatal("reservation replay", e)
	}
	r.OperationID = uuid.NewString() // Restart revalidates the same saved configuration.
	rechecked, e := f.n.ReserveVNic(ctx, r)
	if e != nil || len(rechecked.Bindings) != 1 || len(reserved.Bindings) != 1 || rechecked.Bindings[0].ID != reserved.Bindings[0].ID {
		t.Fatal("same configuration revalidation created a second logical binding", rechecked.Bindings, e)
	}

	if _, e = f.n.DeleteVNic(ctx, v.TenantID, v.ID); biz.ReasonOf(e) != biz.ResourceBusy {
		t.Fatal("reserved VNIC deletion accepted", e)
	}
	if _, e = f.n.DeleteSubnet(ctx, v.TenantID, v.SubnetID); biz.ReasonOf(e) != biz.ResourceBusy {
		t.Fatal("retained VNIC did not protect parent", e)
	}
	if e = f.n.ApplyVNicBinding(ctx, v.TenantID, "vm", "vm-one", v.ID, 1); e != nil {
		t.Fatal(e)
	}
	if e = f.n.ReleaseVNicBindings(ctx, v.TenantID, "vm", "vm-one", nil); e != nil {
		t.Fatal(e)
	}
	kept, e := f.n.GetVNic(ctx, v.TenantID, v.ID)
	if e != nil || kept.ProviderUID != v.ProviderUID || kept.PrimaryIP != v.PrimaryIP || len(kept.Bindings) != 0 {
		t.Fatal("instance release deleted independent VNIC", kept, e)
	}
	if _, e = f.n.DeleteVNic(ctx, v.TenantID, v.ID); e != nil {
		t.Fatal(e)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, e = f.resourceWorker.Step(ctx); e != nil {
			t.Fatal(e)
		}
		kept, e = f.n.GetVNic(ctx, v.TenantID, v.ID)
		if e != nil {
			t.Fatal(e)
		}
		if kept.State == biz.Deleted {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("explicit deletion did not complete", kept)
}
func TestManualVNicPGExclusiveReservationAndStableRead(t *testing.T) {
	f := newAttachmentFixture(t)
	v := manualVNic(t, f, "exclusive")
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, consumer := range []string{"vm-a", "vm-b"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			_, e := f.n.ReserveVNic(ctx, biz.VNicReservation{TenantID: v.TenantID, VNicID: v.ID, ConsumerKind: "vm", ConsumerID: id, Slot: "primary", OperationID: uuid.NewString(), SubnetID: v.SubnetID, Revision: 1})
			errs <- e
		}(consumer)
	}
	wg.Wait()
	close(errs)
	success, busy := 0, 0
	for e := range errs {
		if e == nil {
			success++
		} else if biz.ReasonOf(e) == biz.ResourceBusy {
			busy++
		} else {
			t.Fatal(e)
		}
	}
	if success != 1 || busy != 1 {
		t.Fatal("exclusive transaction failed", success, busy)
	}
	f.api.Mu.Lock()
	before := f.api.Creates["vnics"]
	f.api.Mu.Unlock()
	for i := 0; i < 3; i++ {
		items, total, e := f.n.ListVNics(ctx, v.TenantID, biz.VNicFilter{PageWindow: biz.PageWindow{Limit: 20}, Name: "MANUAL"})
		if e != nil || total != 1 || len(items) != 1 || len(items[0].Bindings) != 1 {
			t.Fatal("pure read list projection", items, total, e)
		}
	}
	f.api.Mu.Lock()
	defer f.api.Mu.Unlock()
	if f.api.Creates["vnics"] != before {
		t.Fatal("query created extra VNIC")
	}
}

func TestManualVNicPGServiceTrustedCaller(t *testing.T) {
	f := newAttachmentFixture(t)
	s := service.NewNetworkService(f.n)
	ctx := context.Background()
	r := &pb.CreateVNicRequest{TargetTenantId: f.r.TenantID, Name: "through-service", SubnetId: f.r.SubnetID, IdempotencyKey: "service-create", RequestedIp: "10.42.1.71", RequestedMac: "02:AB:CD:EF:00:71"}
	if _, e := s.CreateVNic(ctx, r); status.Code(e) != codes.PermissionDenied {
		t.Fatal("missing trusted caller accepted", e)
	}
	trusted := biz.WithEgressCaller(ctx, biz.EgressCaller{TenantID: f.r.TenantID, Attribution: biz.Attribution{Actor: "trusted-user", DirectCaller: "ani-governance", CorrelationID: "request-id"}})
	if _, e := s.CreateVNic(trusted, nil); e == nil {
		t.Fatal("nil create accepted")
	}
	value, e := s.CreateVNic(trusted, r)
	if e != nil {
		t.Fatal(e)
	}
	if value.Vnic.RequestedIp != r.RequestedIp || value.Vnic.RequestedMac != "02:ab:cd:ef:00:71" || value.Vnic.PrimaryIp != "" || value.Vnic.MacAddress != "" {
		t.Fatal("service did not preserve request intent separately from actual addresses", value.Vnic)
	}
	var actor, caller, correlation string
	if e = f.owner.QueryRow(ctx, `SELECT actor_ref,caller_ref,correlation_id FROM network_resource_history WHERE tenant_id=$1 AND vnic_id=$2 AND event='create_accepted'`, f.r.TenantID, value.Vnic.Id).Scan(&actor, &caller, &correlation); e != nil {
		t.Fatal(e)
	}
	if actor != "trusted-user" || caller != "ani-governance" || correlation != "request-id" {
		t.Fatal("trusted attribution was lost", actor, caller, correlation)
	}
	cross := proto.Clone(r).(*pb.CreateVNicRequest)
	cross.TargetTenantId = uuid.NewString()
	if _, e = s.CreateVNic(trusted, cross); status.Code(e) != codes.PermissionDenied {
		t.Fatal("cross-tenant service create accepted", e)
	}
	missing := proto.Clone(r).(*pb.CreateVNicRequest)
	missing.TargetTenantId = ""
	if _, e = s.CreateVNic(trusted, missing); e == nil {
		t.Fatal("missing tenant service create accepted")
	}
}

func TestManualVNicPGCurrentProjectionUsesRetainedIdentity(t *testing.T) {
	f := newAttachmentFixture(t)
	ctx := context.Background()
	v := manualVNic(t, f, "current")
	f.r.WorkloadMode = "controller_managed"
	if _, e := f.n.ReserveVNic(ctx, biz.VNicReservation{TenantID: v.TenantID, VNicID: v.ID, ConsumerKind: "container", ConsumerID: f.r.InstanceID, Slot: "primary", OperationID: uuid.NewString(), SubnetID: v.SubnetID, Revision: 1}); e != nil {
		t.Fatal(e)
	}
	a := f.prepare(t)
	c := managedConsumerFor(a, "manual-pod", 1)
	c.ManualVNics = []biz.ManualVNicReference{{VNicID: v.ID, ProviderName: v.ProviderName, ProviderUID: v.ProviderUID}}
	f.api.Mu.Lock()
	pod := managedPodObject(a, "manual-pod")
	pod["metadata"].(map[string]any)["annotations"].(map[string]any)["networking.kubercloud.com/vnic"] = v.ProviderName
	pod["status"].(map[string]any)["podIP"] = v.PrimaryIP
	f.api.Objects["pods/"+a.Namespace+"/notebook-0"] = pod
	f.api.Objects["vnicips/"+a.Namespace+"/manual-ip"] = map[string]any{"apiVersion": "networking.kubercloud.com/v1", "kind": "VNicIP", "metadata": map[string]any{"name": "manual-ip", "namespace": a.Namespace, "uid": "manual-ip-uid", "resourceVersion": "1", "ownerReferences": []any{map[string]any{"apiVersion": "networking.kubercloud.com/v1", "kind": "VNic", "name": v.ProviderName, "uid": v.ProviderUID}}}, "spec": map[string]any{"subnet": a.Plan.PrimaryNetworkRef, "vNic": v.ProviderName, "ipAddress": v.PrimaryIP}, "status": map[string]any{"vNic": v.ProviderName}}
	f.api.Mu.Unlock()
	setManagedConsumer(f, c)
	a = f.step(t, a.ID)
	if a.State != biz.Attached || a.CurrentVNicName != v.ProviderName || a.CurrentVNicUID != v.ProviderUID || a.CurrentMACAddress != v.MACAddress || len(a.CurrentIPs) != 1 || a.CurrentIPs[0] != v.PrimaryIP {
		t.Fatal("manual current view lost the verified runtime VNIC identity", a)
	}
}
