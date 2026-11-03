package data_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
)

// Real admission, PostgreSQL, the durable worker and KC adapter must depend on
// the VPC's own Provider evidence, independently of its aggregate exit state.
func TestLBRuntimeIndependentOfBaseConnectivity(t *testing.T) {
	for _, base := range []string{"missing", "degraded"} {
		t.Run(base, func(t *testing.T) {
			controller := &lbControllerFixture{}
			f := newLBAdmissionFixture(t, controller.http)
			if base == "missing" {
				if _, err := f.f.owner.Exec(f.f.ctx, `DELETE FROM network_vpc_base_connectivity WHERE tenant_id=$1 AND vpc_id=$2`, f.f.tenant, f.vpc.ID); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := f.f.owner.Exec(f.f.ctx, `UPDATE network_vpc_base_connectivity SET state='degraded',provider_ready=false,observed_at=clock_timestamp()-interval '2 hour' WHERE tenant_id=$1 AND vpc_id=$2`, f.f.tenant, f.vpc.ID); err != nil {
					t.Fatal(err)
				}
			}
			// This aggregate fault must not erase the already observed VPC UID
			// and generation saved by the actual parent worker.
			if _, err := f.f.owner.Exec(f.f.ctx, `UPDATE network_vpcs SET state='degraded',reason='BASE_CONNECTIVITY_NOT_READY',observed_at=clock_timestamp()-interval '2 hour' WHERE tenant_id=$1 AND vpc_id=$2`, f.f.tenant, f.vpc.ID); err != nil {
				t.Fatal(err)
			}
			created, err := f.lbs.Create(f.f.ctx, f.request)
			if err != nil {
				t.Fatal("own VPC Provider proof did not permit creation", err)
			}
			deferredConfigured(t, f, created.LoadBalancer.ID, 1)
			if _, err := f.lbs.Delete(f.f.ctx, "", created.LoadBalancer.ID); err != nil {
				t.Fatal(err)
			}
			lbState(t, f, created.LoadBalancer.ID, biz.Deleted)
			request := deferredEmptyLBRequest(f)
			request.IdempotencyKey = "save-without-base"
			saved, err := f.lbs.Create(f.f.ctx, request)
			if err != nil || saved.Operation.State != biz.Succeeded || saved.LoadBalancer.ProviderTargetVersion != 0 {
				t.Fatal("empty save without base", saved, err)
			}
			if _, err := f.f.owner.Exec(f.f.ctx, `UPDATE network_vpcs SET state='provisioning',reason='BASE_CONNECTIVITY_NOT_READY',observed_at=clock_timestamp()-interval '2 hour' WHERE tenant_id=$1 AND vpc_id=$2`, f.f.tenant, f.vpc.ID); err != nil {
				t.Fatal(err)
			}
			listener := deferredListenerInput(saved.LoadBalancer.Listeners[0], f.request.Backends)
			activated, err := f.lbs.Update(f.f.ctx, biz.UpdateLoadBalancer{ID: saved.LoadBalancer.ID, ExpectedVersion: saved.LoadBalancer.Version, IdempotencyKey: "activate-without-base", LoadBalancerMutableInput: biz.LoadBalancerMutableInput{Listeners: []biz.LoadBalancerListenerInput{listener}, UpdateMask: []string{"listeners"}}})
			if err != nil {
				t.Fatal("own VPC Provider proof did not permit activation", err)
			}
			deferredConfigured(t, f, activated.LoadBalancer.ID, 2)
			if _, err := f.lbs.Delete(f.f.ctx, "", activated.LoadBalancer.ID); err != nil {
				t.Fatal(err)
			}
			lbState(t, f, activated.LoadBalancer.ID, biz.Deleted)
			var occupied int
			if err := f.f.owner.QueryRow(f.f.ctx, `SELECT count(*) FROM network_lb_vip_intents WHERE tenant_id=$1 AND released_at IS NULL`, f.f.tenant).Scan(&occupied); err != nil || occupied != 0 {
				t.Fatal("confirmed deletion did not release VIP reservations", occupied, err)
			}
		})
	}
}

func TestLBAdmissionRejectsInvalidVPCProviderProof(t *testing.T) {
	f := newLBAdmissionFixture(t)
	saved, err := f.lbs.Create(f.f.ctx, deferredEmptyLBRequest(f))
	if err != nil {
		t.Fatal(err)
	}
	var ready bool
	var observed *time.Time
	var observedUID, pendingAction string
	var pendingSince *time.Time
	var generation, observedGeneration int64
	if err = f.f.owner.QueryRow(f.f.ctx, `SELECT provider_ready,provider_observed_at,provider_observed_uid,provider_generation,provider_observed_generation,pending_action,pending_since FROM network_provider_bindings WHERE tenant_id=$1 AND vpc_id=$2`, f.f.tenant, f.vpc.ID).Scan(&ready, &observed, &observedUID, &generation, &observedGeneration, &pendingAction, &pendingSince); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, bindingChange, parentState string
		change                           func(map[string]any)
		reason                           biz.Reason
	}{
		{name: "unready", change: func(o map[string]any) { o["status"].(map[string]any)["conditions"] = []any{} }, reason: biz.ParentNotReady},
		{name: "different_uid", change: func(o map[string]any) { o["metadata"].(map[string]any)["uid"] = "replacement" }},
		{name: "missing_generation", change: func(o map[string]any) { delete(o["metadata"].(map[string]any), "generation") }, reason: biz.ParentNotReady},
		{name: "lagging_generation", change: func(o map[string]any) { o["status"].(map[string]any)["observedGeneration"] = 0 }, reason: biz.ParentNotReady},
		{name: "pending_mutation", bindingChange: "pending_action='update',pending_since=clock_timestamp()", reason: biz.ProviderOwnership},
		{name: "deleting", parentState: "deleting", reason: biz.ParentNotReady},
		{name: "deleted", parentState: "deleted", reason: biz.ParentNotReady},
		{name: "failed", parentState: "failed", reason: biz.ParentNotReady},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := f.api.Object("vpcs", "tenant-"+f.f.tenant, strings.Replace(f.vpc.ID, "_", "-", 1))
			defer f.api.Change("vpcs", original, false)
			if tc.change != nil {
				changed := f.api.Object("vpcs", "tenant-"+f.f.tenant, strings.Replace(f.vpc.ID, "_", "-", 1))
				tc.change(changed)
				f.api.Change("vpcs", changed, false)
			}
			rejected := func(err error) bool {
				if tc.reason != "" {
					return biz.ReasonOf(err) == tc.reason
				}
				var failure *biz.ProviderError
				return errors.As(err, &failure) && failure.Kind == biz.ProviderConflict
			}
			defer func() {
				if _, err := f.f.owner.Exec(f.f.ctx, `UPDATE network_provider_bindings SET provider_ready=$3,provider_observed_at=$4,provider_observed_uid=$5,provider_generation=$6,provider_observed_generation=$7,pending_action=$8,pending_since=$9 WHERE tenant_id=$1 AND vpc_id=$2`, f.f.tenant, f.vpc.ID, ready, observed, observedUID, generation, observedGeneration, pendingAction, pendingSince); err != nil {
					t.Error(err)
				}
				if _, err := f.f.owner.Exec(f.f.ctx, `UPDATE network_vpcs SET state=$3 WHERE tenant_id=$1 AND vpc_id=$2`, f.f.tenant, f.vpc.ID, string(f.vpc.State)); err != nil {
					t.Error(err)
				}
			}()
			if tc.bindingChange != "" {
				if _, err := f.f.owner.Exec(f.f.ctx, "UPDATE network_provider_bindings SET "+tc.bindingChange+" WHERE tenant_id=$1 AND vpc_id=$2", f.f.tenant, f.vpc.ID); err != nil {
					t.Fatal(err)
				}
			}
			if tc.parentState != "" {
				if _, err := f.f.owner.Exec(f.f.ctx, `UPDATE network_vpcs SET state=$3 WHERE tenant_id=$1 AND vpc_id=$2`, f.f.tenant, f.vpc.ID, tc.parentState); err != nil {
					t.Fatal(err)
				}
			}
			request := f.request
			request.PrivateIP, request.IdempotencyKey = "10.42.1.101", uuid.NewString()
			if _, err := f.lbs.Create(f.f.ctx, request); !rejected(err) {
				t.Fatal("invalid VPC self proof permitted creation", err)
			}
			listener := deferredListenerInput(saved.LoadBalancer.Listeners[0], f.request.Backends)
			if _, err := f.lbs.Update(f.f.ctx, biz.UpdateLoadBalancer{ID: saved.LoadBalancer.ID, ExpectedVersion: saved.LoadBalancer.Version, IdempotencyKey: uuid.NewString(), LoadBalancerMutableInput: biz.LoadBalancerMutableInput{Listeners: []biz.LoadBalancerListenerInput{listener}, UpdateMask: []string{"listeners"}}}); !rejected(err) {
				t.Fatal("invalid VPC self proof permitted activation", err)
			}
		})
	}
	var count int
	if err := f.f.owner.QueryRow(f.f.ctx, `SELECT count(*) FROM network_lb_configurations WHERE tenant_id=$1`, f.f.tenant).Scan(&count); err != nil || count != 1 {
		t.Fatal("rejected admission saved a partial configuration", count, err)
	}
}

func TestLBAdmissionUsesCurrentVPCInsteadOfOldProjection(t *testing.T) {
	f := newLBAdmissionFixture(t)
	if _, err := f.f.owner.Exec(f.f.ctx, `UPDATE network_provider_bindings SET provider_ready=false,provider_observed_at=clock_timestamp()-interval '2 hour',provider_observed_uid='old-observation',provider_generation=0,provider_observed_generation=0 WHERE tenant_id=$1 AND vpc_id=$2`, f.f.tenant, f.vpc.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.lbs.Create(f.f.ctx, f.request); err != nil {
		t.Fatal("current original VPC proof must permit admission", err)
	}
	var age float64
	if err := f.f.owner.QueryRow(f.f.ctx, `SELECT extract(epoch from clock_timestamp()-provider_observed_at) FROM network_provider_bindings WHERE tenant_id=$1 AND vpc_id=$2`, f.f.tenant, f.vpc.ID).Scan(&age); err != nil || age < 3600 {
		t.Fatal("admission must not refresh projected observation time", age, err)
	}
}
