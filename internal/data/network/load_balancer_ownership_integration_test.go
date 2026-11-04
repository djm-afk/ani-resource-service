package data_test

import (
	"testing"

	"github.com/google/uuid"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
)

func TestLBOwnUIDReplacementClosesOriginalDeletion(t *testing.T) {
	for _, kind := range []string{"gateway", "route"} {
		t.Run(kind, func(t *testing.T) {
			controller := &lbControllerFixture{}
			f := newLBAdmissionFixture(t, controller.http)
			created, err := f.lbs.Create(f.f.ctx, f.request)
			if err != nil {
				t.Fatal(err)
			}
			lb := lbState(t, f, created.LoadBalancer.ID, biz.Available)
			var namespace, name, originalUID string
			query := `SELECT namespace,provider_name,provider_uid FROM network_provider_bindings WHERE tenant_id=$1 AND lb_id=$2`
			resource := "gateways"
			if kind == "route" {
				query = `SELECT namespace,provider_name,provider_uid FROM network_lb_components WHERE tenant_id=$1 AND lb_id=$2 AND kind='route'`
				resource = "httproutes"
			}
			if err = f.f.owner.QueryRow(f.f.ctx, query, f.f.tenant, lb.ID).Scan(&namespace, &name, &originalUID); err != nil {
				t.Fatal(err)
			}
			replacement := f.api.Object(resource, namespace, name)
			replacementUID := uuid.NewString()
			replacement["metadata"].(map[string]any)["uid"] = replacementUID
			f.api.Change(resource, replacement, false)
			zero := uint32(0)
			backend := lb.Backends[0]
			updated, err := f.lbs.Update(f.f.ctx, biz.UpdateLoadBalancer{ID: lb.ID, ExpectedVersion: lb.Version, IdempotencyKey: "replace-weight", LoadBalancerMutableInput: biz.LoadBalancerMutableInput{Name: lb.Name, Health: f.request.Health, Backends: []biz.LoadBalancerBackendInput{{ID: backend.ID, SubnetID: backend.SubnetID, Address: backend.Address, Port: backend.Port, Weight: &zero}}}})
			if err != nil {
				t.Fatal(err)
			}
			f.f.drive(t, func() bool {
				op, err := f.lbs.GetOperation(f.f.ctx, "", updated.Operation.ID)
				return err == nil && op.State == biz.Blocked && op.Reason == biz.ProviderOwnership
			})
			if _, err = f.lbs.Delete(f.f.ctx, "", lb.ID); err != nil {
				t.Fatal(err)
			}
			lbState(t, f, lb.ID, biz.Deleted)
			object := f.api.Object(resource, namespace, name)
			if object == nil || object["metadata"].(map[string]any)["uid"] != replacementUID {
				t.Fatal("deletion touched a replacement object")
			}
			var savedUID string
			if err = f.f.owner.QueryRow(f.f.ctx, query, f.f.tenant, lb.ID).Scan(&namespace, &name, &savedUID); err != nil || savedUID != originalUID {
				t.Fatal("original management identity changed", savedUID, originalUID, err)
			}
			lbRecoveryOccupancy(t, f, lb.ID, 0, 0)
		})
	}
}

func TestLBDeletionDoesNotInspectForeignServiceOrNat(t *testing.T) {
	controller := &lbControllerFixture{}
	f := newLBAdmissionFixture(t, controller.http)
	eip := f.f.eip(t, "independent-eip")
	request := f.request
	request.Exposure, request.PublicEIPID = "public_private", eip.ID
	created, err := f.lbs.Create(f.f.ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	lb := lbState(t, f, created.LoadBalancer.ID, biz.Available)
	var namespace, eipName string
	if err = f.f.owner.QueryRow(f.f.ctx, `SELECT namespace,provider_name FROM network_provider_bindings WHERE tenant_id=$1 AND eip_id=$2`, f.f.tenant, eip.ID).Scan(&namespace, &eipName); err != nil {
		t.Fatal(err)
	}
	service := map[string]any{"apiVersion": "v1", "kind": "Service", "metadata": map[string]any{"namespace": namespace, "name": "foreign-service", "uid": uuid.NewString(), "annotations": map[string]any{"networking.kubercloud.com/lb_eips": eipName}}, "spec": map[string]any{"type": "LoadBalancer"}}
	nat := map[string]any{"apiVersion": "networking.kubercloud.com/v1", "kind": "Nat", "metadata": map[string]any{"namespace": namespace, "name": "foreign-nat", "uid": uuid.NewString()}, "spec": map[string]any{"eip": eipName}}
	f.api.Change("services", service, false)
	f.api.Change("nats", nat, false)
	if _, err = f.lbs.Delete(f.f.ctx, "", lb.ID); err != nil {
		t.Fatal(err)
	}
	lbState(t, f, lb.ID, biz.Deleted)
	if f.api.Object("services", namespace, "foreign-service") == nil || f.api.Object("nats", namespace, "foreign-nat") == nil {
		t.Fatal("Res deleted another controller's object")
	}
	lbRecoveryOccupancy(t, f, lb.ID, 0, 0)
}
