package data_test

import (
	"context"
	"sync"
	"testing"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
)

func TestManualVNicPGRequestedAddressesAndMACReservation(t *testing.T) {
	f := newAttachmentFixture(t)
	ctx := context.Background()
	r := biz.CreateVNic{TenantID: f.r.TenantID, SubnetID: f.r.SubnetID, Name: "custom-address", IdempotencyKey: "custom-address", RequestedIP: "10.42.1.71", RequestedMAC: "02:AB:CD:EF:00:71"}
	v, err := f.n.CreateVNic(ctx, r)
	if err != nil || v.RequestedIP != r.RequestedIP || v.RequestedMAC != "02:ab:cd:ef:00:71" || v.PrimaryIP != "" || v.MACAddress != "" {
		t.Fatal("intent was not persisted separately from observed facts", v, err)
	}
	r.RequestedMAC = "02-ab-cd-ef-00-71"
	replay, err := f.n.CreateVNic(ctx, r)
	if err != nil || replay.ID != v.ID || replay.LastOperationID != v.LastOperationID {
		t.Fatal("normalized intent replay changed identity", replay, err)
	}
	r.RequestedIP = "10.42.1.72"
	if _, err = f.n.CreateVNic(ctx, r); biz.ReasonOf(err) != biz.IdempotencyConflict {
		t.Fatal("changed address replay accepted", err)
	}
	r.IdempotencyKey = "duplicate-mac"
	if _, err = f.n.CreateVNic(ctx, r); biz.ReasonOf(err) != biz.AddressConflict {
		t.Fatal("reserved manual MAC accepted twice", err)
	}
	r.RequestedMAC, r.RequestedIP = "02:ab:cd:ef:00:72", "10.42.2.71"
	if _, err = f.n.CreateVNic(ctx, r); biz.ReasonOf(err) != biz.InvalidArgument {
		t.Fatal("outside Subnet address accepted", err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, key := range []string{"concurrent-a", "concurrent-b"} {
		wg.Add(1)
		go func(key string) {
			defer wg.Done()
			_, err := f.n.CreateVNic(ctx, biz.CreateVNic{TenantID: r.TenantID, SubnetID: r.SubnetID, Name: key, IdempotencyKey: key, RequestedMAC: "02:ab:cd:ef:00:73"})
			errs <- err
		}(key)
	}
	wg.Wait()
	close(errs)
	accepted, conflicts := 0, 0
	for err := range errs {
		if err == nil {
			accepted++
		} else if biz.ReasonOf(err) == biz.AddressConflict {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if accepted != 1 || conflicts != 1 {
		t.Fatal("concurrent MAC reservation", accepted, conflicts)
	}
}

func TestManualVNicPGProviderAllocatedMACIsNotAUserReservation(t *testing.T) {
	f := newAttachmentFixture(t)
	v := manualVNic(t, f, "auto-mac")
	_, err := f.n.CreateVNic(context.Background(), biz.CreateVNic{TenantID: v.TenantID, SubnetID: v.SubnetID, Name: "custom-mac", IdempotencyKey: "custom-mac", RequestedMAC: v.MACAddress})
	if err != nil {
		t.Fatal("provider allocated output became an input reservation", err)
	}
}
