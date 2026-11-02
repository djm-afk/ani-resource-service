package data_test

import (
	"context"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/tenantnamespace"
	"testing"
	"time"
)

func TestNET05ANotificationsDuringLeasePreservePendingAndPublicVersion(t *testing.T) {
	for _, window := range []string{"before_claim", "during_observe", "before_finish", "after_finish"} {
		t.Run(window, func(t *testing.T) {
			p, db := database(t)
			ctx := context.Background()
			n := newNetwork(t, p, time.Minute)
			tenant := uuid.NewString()
			identity := tenantnamespace.Identity{TenantID: tenant, ClusterID: "test-cluster", Name: "tenant-" + tenant, UID: uuid.NewString()}
			p.ConfigureTenantNamespaceResolver(func(_ context.Context, requested string) (tenantnamespace.Identity, error) {
				if requested != identity.TenantID {
					return tenantnamespace.Identity{}, errors.New("unknown fixture tenant")
				}
				return identity, nil
			})
			v, e := n.CreateVPC(ctx, biz.CreateVPC{TenantID: tenant, Name: "notify", CIDR: "10.42.0.0/16", IdempotencyKey: "notify"})
			if e != nil {
				t.Fatal(e)
			}
			if window == "before_claim" {
				if e = p.NotifyResource(ctx, tenant, v.ID); e != nil {
					t.Fatal(e)
				}
			}
			w, ok, e := p.Claim(ctx, uuid.NewString(), time.Second)
			if e != nil || !ok {
				t.Fatalf("claim: %v %v", ok, e)
			}
			if window == "during_observe" || window == "before_finish" {
				if e = p.NotifyResource(ctx, tenant, v.ID); e != nil {
					t.Fatal(e)
				}
			}
			var version, epoch, requested int64
			var owner string
			e = db.QueryRow(ctx, `SELECT v.version,r.lease_epoch,r.lease_owner::text,r.requested_generation FROM network_vpcs v JOIN network_reconciliations r USING(tenant_id,vpc_id) WHERE v.tenant_id=$1 AND v.vpc_id=$2`, tenant, v.ID).Scan(&version, &epoch, &owner, &requested)
			if e != nil || version != v.Version || epoch != w.Epoch || owner != w.Owner {
				t.Fatalf("notify altered version/lease: %d %d %s %v", version, epoch, owner, e)
			}
			proof := biz.ObservationProof{CollectedAt: w.Now, Hash: "fact-A", CoveredGeneration: w.Requirement.RequestedGeneration}
			e = p.Finish(ctx, w, biz.Progress{State: biz.Provisioning, OperationState: biz.Retrying, Observed: true, Proof: proof, NextDelay: time.Hour})
			if e != nil {
				t.Fatal(e)
			}
			if window == "after_finish" {
				if e = p.NotifyResource(ctx, tenant, v.ID); e != nil {
					t.Fatal(e)
				}
			}
			var processed int64
			var due bool
			e = db.QueryRow(ctx, `SELECT requested_generation,processed_generation,next_run_at<=clock_timestamp() FROM network_reconciliations WHERE tenant_id=$1 AND vpc_id=$2`, tenant, v.ID).Scan(&requested, &processed, &due)
			if e != nil || processed != w.Requirement.RequestedGeneration || due != (window != "before_claim") {
				t.Fatalf("notification lost: requested=%d processed=%d due=%v %v", requested, processed, due, e)
			}
		})
	}
}
func TestNET05AStormCannotBypassBackoffOrCrossTenant(t *testing.T) {
	p, db := database(t)
	ctx := context.Background()
	n := newNetwork(t, p, time.Minute)
	tenant := uuid.NewString()
	identity := tenantnamespace.Identity{TenantID: tenant, ClusterID: "test-cluster", Name: "tenant-" + tenant, UID: uuid.NewString()}
	p.ConfigureTenantNamespaceResolver(func(_ context.Context, requested string) (tenantnamespace.Identity, error) {
		if requested != identity.TenantID {
			return tenantnamespace.Identity{}, errors.New("unknown fixture tenant")
		}
		return identity, nil
	})
	v, e := n.CreateVPC(ctx, biz.CreateVPC{TenantID: tenant, Name: "backoff", CIDR: "10.42.0.0/16", IdempotencyKey: "backoff"})
	if e != nil {
		t.Fatal(e)
	}
	w, _, e := p.Claim(ctx, uuid.NewString(), time.Second)
	if e != nil {
		t.Fatal(e)
	}
	if e = p.Finish(ctx, w, biz.Progress{State: biz.Provisioning, OperationState: biz.Retrying, Reason: biz.ProviderUnavailable, Backoff: true, NextDelay: time.Second}); e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 100; i++ {
		if e = p.NotifyResource(ctx, tenant, v.ID); e != nil {
			t.Fatal(e)
		}
	}
	if e = p.NotifyResource(ctx, uuid.NewString(), v.ID); e != nil {
		t.Fatal(e)
	}
	var requested int64
	var due bool
	e = db.QueryRow(ctx, `SELECT requested_generation,next_run_at<retry_not_before OR next_run_at<=clock_timestamp() FROM network_reconciliations WHERE tenant_id=$1 AND vpc_id=$2`, tenant, v.ID).Scan(&requested, &due)
	if e != nil || requested != 101 || due {
		t.Fatalf("storm bypassed backoff/tenant: %d %v %v", requested, due, e)
	}
}
func TestNET05AOldCacheAfterNewClaimCannotRegressOrCompleteNotification(t *testing.T) {
	p, db := database(t)
	ctx := context.Background()
	n := newNetwork(t, p, time.Minute)
	tenant := uuid.NewString()
	identity := tenantnamespace.Identity{TenantID: tenant, ClusterID: "test-cluster", Name: "tenant-" + tenant, UID: uuid.NewString()}
	p.ConfigureTenantNamespaceResolver(func(_ context.Context, requested string) (tenantnamespace.Identity, error) {
		if requested != identity.TenantID {
			return tenantnamespace.Identity{}, errors.New("unknown fixture tenant")
		}
		return identity, nil
	})
	v, e := n.CreateVPC(ctx, biz.CreateVPC{TenantID: tenant, Name: "proof", CIDR: "10.42.0.0/16", IdempotencyKey: "proof"})
	if e != nil {
		t.Fatal(e)
	}
	first, _, e := p.Claim(ctx, uuid.NewString(), time.Second)
	if e != nil {
		t.Fatal(e)
	}
	proof := biz.ObservationProof{CollectedAt: first.Now, Hash: "new-fact", CoveredGeneration: first.Requirement.RequestedGeneration}
	if e = p.Finish(ctx, first, biz.Progress{State: biz.Available, OperationState: biz.Succeeded, Identity: "uid", Observed: true, Proof: proof, NextDelay: time.Hour}); e != nil {
		t.Fatal(e)
	}
	if e = p.NotifyResource(ctx, tenant, v.ID); e != nil {
		t.Fatal(e)
	}
	second, ok, e := p.Claim(ctx, uuid.NewString(), time.Second)
	if e != nil || !ok {
		t.Fatalf("second claim %v %v", ok, e)
	}
	old := biz.ObservationProof{CollectedAt: first.Now.Add(-time.Second), Hash: "old-fact", CoveredGeneration: second.Requirement.RequestedGeneration}
	if e = p.Finish(ctx, second, biz.Progress{State: biz.Degraded, Identity: "uid", Observed: true, Proof: old, NextDelay: time.Hour}); !errors.Is(e, biz.ErrLeaseLost) {
		t.Fatalf("accepted old cache: %v", e)
	}
	var processed int64
	if e = db.QueryRow(ctx, `SELECT processed_generation FROM network_reconciliations WHERE tenant_id=$1 AND vpc_id=$2`, tenant, v.ID).Scan(&processed); e != nil {
		t.Fatal(e)
	}
	actual, e := n.GetVPC(ctx, tenant, v.ID)
	if e != nil || actual.State != biz.Available || processed != first.Requirement.RequestedGeneration {
		t.Fatalf("old cache changed fact/coverage: %+v %d %v", actual, processed, e)
	}
	if actual.ObservedAt == nil || !actual.ObservedAt.Equal(first.Now) {
		t.Fatal("commit time replaced real collection time")
	}
}
func TestNET05AAttachmentNotificationDuringClaimPreservesLeaseAndWake(t *testing.T) {
	f := newAttachmentFixture(t)
	ctx := context.Background()
	a := f.prepare(t)
	w, ok, e := f.p.ClaimAttachment(ctx, uuid.NewString(), time.Second)
	if e != nil || !ok {
		t.Fatalf("claim %v %v", ok, e)
	}
	if e = f.p.NotifyAttachment(ctx, a.TenantID, a.ID); e != nil {
		t.Fatal(e)
	}
	proof := biz.ObservationProof{CollectedAt: w.Now, Hash: "relations", CoveredGeneration: w.Requirement.RequestedGeneration}
	if e = f.p.FinishAttachment(ctx, w, biz.AttachmentProgress{State: biz.Reserved, Observed: true, Proof: proof, Relations: []byte("[]"), NextDelay: time.Hour}); e != nil {
		t.Fatal(e)
	}
	next, ok, e := f.p.ClaimAttachment(ctx, uuid.NewString(), time.Second)
	if e != nil || !ok || next.Requirement.RequestedGeneration <= w.Requirement.RequestedGeneration {
		t.Fatalf("attachment wake lost %v %v", ok, e)
	}
}

func TestNET05AFairDueClaimsAcrossKindsTenantsAndParents(t *testing.T) {
	p, _ := database(t)
	ctx := context.Background()
	n := newNetwork(t, p, time.Minute)
	// Only explicitly prepared tenants have an external Namespace identity;
	// their original UID is stable across both parents and every later claim.
	identities := map[string]tenantnamespace.Identity{}
	p.ConfigureTenantNamespaceResolver(func(_ context.Context, requested string) (tenantnamespace.Identity, error) {
		identity, ok := identities[requested]
		if !ok {
			return tenantnamespace.Identity{}, errors.New("unknown fixture tenant")
		}
		return identity, nil
	})
	worker := subnetWorker(t, p)
	as := biz.NewAttachments(p, time.Minute)
	resources := map[string]string{}
	attachments := map[string]string{}
	parents := []biz.VPC{}
	for i := 0; i < 3; i++ {
		tenant := uuid.NewString()
		identities[tenant] = tenantnamespace.Identity{TenantID: tenant, ClusterID: "test-cluster", Name: "tenant-" + tenant, UID: uuid.NewString()}
		for j := 0; j < 2; j++ {
			v := availableVPC(t, n, worker, tenant, fmt.Sprintf("fair-%d", j))
			parents = append(parents, v)
			resources[v.ID] = tenant
			s, e := n.CreateSubnet(ctx, biz.CreateSubnet{TenantID: tenant, VPCID: v.ID, Name: "fair", CIDR: "10.42.1.0/24", IdempotencyKey: fmt.Sprintf("fair-%d", j)})
			if e != nil {
				t.Fatal(e)
			}
			s = driveSubnet(t, n, worker, s, biz.Available)
			resources[s.ID] = tenant
			a, e := as.Prepare(ctx, biz.PrepareAttachment{TenantID: tenant, VPCID: v.ID, SubnetID: s.ID, InstanceID: "inst_" + uuid.NewString(), Slot: "primary", RequestKey: "fair", SubmissionID: uuid.NewString(), Generation: 1, ClusterID: "test-cluster", Namespace: "tenant-" + tenant})
			if e != nil {
				t.Fatal(e)
			}
			attachments[a.ID] = tenant
		}
	}
	for id, tenant := range resources {
		if e := p.NotifyResource(ctx, tenant, id); e != nil {
			t.Fatal(e)
		}
	}
	seen := map[string]bool{}
	for i := 0; i < 3*len(resources); i++ {
		// Continuously replenish VPC pressure without resetting old work's due age.
		for _, v := range parents {
			if e := p.NotifyResource(ctx, v.TenantID, v.ID); e != nil {
				t.Fatal(e)
			}
		}
		w, found, e := p.Claim(ctx, uuid.NewString(), time.Second)
		if e != nil || !found {
			t.Fatalf("resource claim %v %v", found, e)
		}
		seen[w.Resource.ID] = true
		progress := biz.Progress{State: w.Resource.State, Reason: w.Resource.Reason, Identity: w.KnownIdentity, Observed: true, Proof: biz.ObservationProof{CollectedAt: w.Now, Hash: "fair", CoveredGeneration: w.Requirement.RequestedGeneration}, NextDelay: time.Hour}
		if e = p.Finish(ctx, w, progress); e != nil {
			t.Fatal(e)
		}
	}
	if len(seen) != len(resources) {
		t.Fatalf("resource kind/tenant/parent starved: %d of %d", len(seen), len(resources))
	}
	for len(attachments) > 0 {
		w, found, e := p.ClaimAttachment(ctx, uuid.NewString(), time.Second)
		if e != nil || !found {
			t.Fatalf("attachment claim %v %v", found, e)
		}
		if _, ok := attachments[w.Attachment.ID]; !ok {
			t.Fatal("repeat claim starved another parent")
		}
		delete(attachments, w.Attachment.ID)
		if e = p.FinishAttachment(ctx, w, biz.AttachmentProgress{State: biz.Reserved, Observed: true, Proof: biz.ObservationProof{CollectedAt: w.Now, Hash: "fair", CoveredGeneration: w.Requirement.RequestedGeneration}, Relations: []byte("[]"), NextDelay: time.Hour}); e != nil {
			t.Fatal(e)
		}
	}
	if len(attachments) != 0 {
		t.Fatal("attachment parent/tenant starved")
	}
}
