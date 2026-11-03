package vm

import (
	"context"
	pb "github.com/zhangzhe-ctrl/ani-resource-service/api/vm/v1"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	"testing"
	"time"
)

type networkViewReader struct{ runtime biz.NetworkInterfaceRuntime }

func (r *networkViewReader) CurrentAddresses(context.Context, biz.VM) ([]biz.Address, error) {
	return nil, nil
}
func (r *networkViewReader) CurrentNetworkInterface(context.Context, biz.VM) (biz.NetworkInterfaceRuntime, error) {
	return r.runtime, nil
}
func (r *networkViewReader) ResolveNetworkInterfaceChoice(_ context.Context, _ biz.VM, c biz.NetworkInterfaceChoice) (biz.NetworkInterfaceChoice, error) {
	if c.Mode == "manual" {
		c.Name = "product-vnic-a"
	}
	return c, nil
}
func TestNetworkViewUsesConfirmedEndpointInsteadOfTargetOrOwnerCache(t *testing.T) {
	tenant := "11111111-1111-4111-8111-111111111111"
	id := "22222222-2222-4222-8222-222222222222"
	now := time.Now().UTC()
	target := biz.NetworkInterfaceChoice{Mode: "manual", VNicID: "vnic_11111111111111111111111111111111", ProviderName: "ani-vnic-original", ProviderUID: "original-uid", IPs: []string{"10.0.0.9"}}
	repo := &snapshotServiceRepo{current: biz.VM{TenantID: tenant, ID: id, Version: 4, Observed: "Running", NetworkInterfaces: []biz.NetworkInterface{{Slot: "primary", Desired: target, Applied: biz.NetworkInterfaceChoice{Mode: "automatic"}, RequestedRevision: 4}}}}
	reader := &networkViewReader{runtime: biz.NetworkInterfaceRuntime{Mode: "automatic", ProviderName: "auto-original", ProviderUID: "auto-uid", MACAddress: "fa:00:11:22:33:44", IPs: []string{"10.0.0.2"}, PodUID: "old-pod", ObservedAt: now, Confirmed: true}}
	service := &Service{repo: repo, addresses: reader}
	ctx := biz.WithCaller(context.Background(), biz.Caller{TenantID: tenant, Actor: "test"})
	view, err := service.GetNetworkInterfaces(ctx, &pb.GetVMRequest{TenantId: tenant, VmId: id})
	if err != nil {
		t.Fatal(err)
	}
	x := view.Items[0]
	if len(x.CurrentIps) != 1 || x.CurrentIps[0] != "10.0.0.2" || x.CurrentVnicName != "auto-original" || x.CurrentMacAddress != reader.runtime.MACAddress || x.Desired.VnicName != "product-vnic-a" || !x.PendingRestart || x.EffectiveNow || !x.ObservedAt.AsTime().Equal(now) {
		t.Fatal("mixed desired and effective network", x)
	}
	cfg := biz.PrimaryNetworkInterface(repo.current)
	cfg.Desired = cfg.Applied
	cfg.RequestedRevision = 5
	cfg.AppliedRevision = 5
	repo.current.NetworkInterfaces = []biz.NetworkInterface{cfg}
	repo.current.Reason = "VM_NETWORK_ADDRESS_NOT_CURRENT"
	view, err = service.GetNetworkInterfaces(ctx, &pb.GetVMRequest{TenantId: tenant, VmId: id})
	if err != nil || !view.Items[0].EffectiveNow || view.Items[0].PendingRestart || view.Items[0].ObservationReason != "" {
		t.Fatal("cancellation hid a still-confirmed automatic endpoint", view, err)
	}
	repo.current.Observed = "Stopped"
	reader.runtime = biz.NetworkInterfaceRuntime{}
	view, err = service.GetNetworkInterfaces(ctx, &pb.GetVMRequest{TenantId: tenant, VmId: id})
	if err != nil || view.Items[0].EffectiveNow || len(view.Items[0].CurrentIps) != 0 {
		t.Fatal("stopped template reported online", view, err)
	}
}

type networkOperationRepo struct {
	snapshotServiceRepo
	operation biz.Operation
}

func (r *networkOperationRepo) GetOperation(context.Context, string, string, string) (biz.Operation, error) {
	return r.operation, nil
}
func TestNetworkViewDoesNotCallUnfinishedSecurityGroupSavePendingRestart(t *testing.T) {
	tenant := "11111111-1111-4111-8111-111111111111"
	id := "22222222-2222-4222-8222-222222222222"
	target := biz.NetworkInterfaceChoice{Mode: "manual", VNicID: "vnic_11111111111111111111111111111111", ProviderName: "ani-vnic-original", ProviderUID: "original"}
	repo := &networkOperationRepo{snapshotServiceRepo: snapshotServiceRepo{current: biz.VM{TenantID: tenant, ID: id, LastOperationID: "save-operation", Observed: "Running", Version: 4, NetworkInterfaces: []biz.NetworkInterface{{Slot: "primary", Desired: target, Applied: biz.NetworkInterfaceChoice{Mode: "automatic"}, RequestedRevision: 4}}}}, operation: biz.Operation{ID: "save-operation", Action: "set-network-interface", State: "running", Reason: "security group receipt pending"}}
	reader := &networkViewReader{runtime: biz.NetworkInterfaceRuntime{Mode: "automatic", ProviderName: "auto-original", IPs: []string{"10.0.0.2"}, PodUID: "old-pod", Confirmed: true}}
	s := &Service{repo: repo, addresses: reader}
	ctx := biz.WithCaller(context.Background(), biz.Caller{TenantID: tenant, Actor: "test"})
	v, e := s.GetNetworkInterfaces(ctx, &pb.GetVMRequest{TenantId: tenant, VmId: id})
	if e != nil {
		t.Fatal(e)
	}
	x := v.Items[0]
	if x.ConfigurationState != "configuring" || x.OperationId != "save-operation" || x.OperationReason == "" || x.PendingRestart || x.EffectiveNow || len(x.CurrentIps) != 1 {
		t.Fatal("unfinished save was reported ready to restart", x)
	}
	repo.operation.State = "succeeded"
	repo.operation.Reason = ""
	v, e = s.GetNetworkInterfaces(ctx, &pb.GetVMRequest{TenantId: tenant, VmId: id})
	if e != nil || v.Items[0].ConfigurationState != "ready" || !v.Items[0].PendingRestart {
		t.Fatal(v, e)
	}
}
