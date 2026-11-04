package data_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	data "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network"
	"k8s.io/client-go/tools/clientcmd"
)

func TestReleasedProtocolAttachmentPGRecoversAfterDurableViolation(t *testing.T) {
	for _, mode := range []string{"vm_managed", "controller_managed"} {
		t.Run(mode, func(t *testing.T) {
			if mode == "vm_managed" {
				// The dedicated VM fixture supplies its exact managed NAD policy.
				f, a, consumer, provider, _ := newVMRelationAudit(t)
				f.provider = provider
				f.r.WorkloadMode = mode
				testReleasedProtocolPGRecovery(t, f, a, consumer)
				return
			}
			f := newAttachmentFixture(t)
			f.r.WorkloadMode = mode
			f.r.ConsumerType = biz.ConsumerContainerInstance
			a := f.prepare(t)
			testReleasedProtocolPGRecovery(t, f, a, managedConsumerFor(a, "", 1))
		})
	}
}

func testReleasedProtocolPGRecovery(t *testing.T, f attachmentFixture, a biz.Attachment, c biz.ConsumerSubmission) {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	c.State, c.DesiredRunState, c.ClosedAt, c.FinalizationID = "closed", "deleted", &now, uuid.NewString()
	c.RetiredPods, c.CurrentPods = c.CurrentPods, nil
	setManagedConsumer(f, c)
	provider := &vmClosureProvider{provider: f.provider}
	policy := biz.DefaultWorkerPolicy()
	policy.ObserveEvery, policy.StaleAfter = 5*time.Millisecond, 20*time.Millisecond
	var err error
	f.worker, err = biz.NewAttachmentWorker(f.p, provider, f.consumer, uuid.NewString(), policy)
	if err != nil {
		t.Fatal(err)
	}
	a = f.step(t, a.ID)
	if a.State != biz.Released || attachmentProtocolBlocked(t, f, a.ID) {
		t.Fatal("normal closure did not release", a)
	}
	provider.conflict = true
	if err = f.p.NotifyAttachment(ctx, a.TenantID, a.ID); err != nil {
		t.Fatal(err)
	}
	a = f.step(t, a.ID)
	if a.State != biz.Released || !attachmentProtocolBlocked(t, f, a.ID) {
		t.Fatal("late provider refusal did not fence retained release", a)
	}
	provider.conflict = false
	if a.WorkloadMode == "vm_managed" {
		// This adapter supplies a hash-bearing complete proof. The old
		// non-shared controller adapter uses the worker's post-Claim fallback
		// without a hash, so injecting an older time there would instead fail
		// the ordinary Finish fence and retain the lease for explicit recovery.
		provider.stale = true
		time.Sleep(25 * time.Millisecond)
		a = f.step(t, a.ID)
		if !attachmentProtocolBlocked(t, f, a.ID) {
			t.Fatal("old same-content proof cleared a later durable violation", a)
		}
	}
	provider.stale = false
	time.Sleep(25 * time.Millisecond)
	a = f.step(t, a.ID)
	var parked bool
	if err = f.owner.QueryRow(ctx, `SELECT next_check_at='infinity'::timestamptz FROM network_attachments WHERE tenant_id=$1 AND attachment_id=$2`, a.TenantID, a.ID).Scan(&parked); err != nil {
		t.Fatal(err)
	}
	if a.State != biz.Released || a.Reason != "" || attachmentProtocolBlocked(t, f, a.ID) || !parked {
		t.Fatal("closed unchanged retained identity did not recover and park", a, parked)
	}
}

func TestReleasedProtocolAttachmentSharesAuditWithoutHidingLateRelations(t *testing.T) {
	_, a, c, provider, api := newVMRelationAudit(t)
	observer := startSlowAudit(t, provider)
	c.State, c.DesiredRunState = "closed", "deleted"
	now := time.Now()
	c.ClosedAt, c.FinalizationID = &now, uuid.NewString()
	c.RetiredPods, c.CurrentPods = c.CurrentPods, nil
	w := biz.AttachmentWork{Attachment: a, Plan: biz.PodPrimaryPlan{PrimaryNetworkRef: a.VMPrimary.KCNSubnetRef, VMPrimary: a.VMPrimary}, ConsumerSnapshot: &c, ProtocolBlocked: true, RequireFreshRelations: true, Relations: []byte("[]")}
	w.Attachment.State = biz.Released
	first, err := provider.ObserveAttachment(context.Background(), w)
	if err != nil || first.Exists || first.HasDependencies {
		t.Fatalf("baseline closed relations: %+v %v", first, err)
	}
	w.RequireFreshRelations = false
	before, _ := api.Counts()
	for i := 0; i < 5; i++ {
		next, err := provider.ObserveAttachment(context.Background(), w)
		if err != nil || !next.Proof.CollectedAt.Equal(first.Proof.CollectedAt) {
			t.Fatalf("retained release renewed or rejected valid shared proof: %+v %v", next, err)
		}
	}
	after, _ := api.Counts()
	if before["LIST/pods"] != after["LIST/pods"] || before["LIST/eips"] != after["LIST/eips"] {
		t.Fatal("retained protocol retry repeated a complete audit")
	}
	// An orphan without the Network labels still invalidates the tombstone.
	notifications := observer.Snapshot()["notifications_total"]
	api.Change("eips", map[string]any{"apiVersion": "networking.kubercloud.com/v1", "kind": "EIP", "metadata": map[string]any{"name": "late-eip", "namespace": a.Namespace, "uid": uuid.NewString()}, "spec": map[string]any{"subnet": w.Plan.PrimaryNetworkRef}}, false)
	awaitNET05A(t, 3*time.Second, func() bool { return observer.Snapshot()["notifications_total"] > notifications })
	if next, err := provider.ObserveAttachment(context.Background(), w); err == nil || !next.Proof.CollectedAt.IsZero() {
		t.Fatalf("late relation reused an invalid audit: %+v %v", next, err)
	}
	w.RequireFreshRelations = true
	if next, err := provider.ObserveAttachment(context.Background(), w); err != nil || !next.HasDependencies {
		t.Fatalf("fresh complete audit hid late orphan: %+v %v", next, err)
	}
}

func TestDeletedLoadBalancerReusesSharedAuditButActiveCleanupRefreshes(t *testing.T) {
	controller := &lbControllerFixture{}
	f := newLBAdmissionFixture(t, controller.http)
	created, err := f.lbs.Create(f.f.ctx, f.request)
	if err != nil {
		t.Fatal(err)
	}
	lbState(t, f, created.LoadBalancer.ID, biz.Available)
	if _, err = f.lbs.Delete(f.f.ctx, "", created.LoadBalancer.ID); err != nil {
		t.Fatal(err)
	}
	lbState(t, f, created.LoadBalancer.ID, biz.Deleted)
	f.f.stopObservation()
	w := deferredClaimSnapshot(t, f, created.LoadBalancer.ID)
	config, err := clientcmd.BuildConfigFromFlags("", f.kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	config.QPS, config.Burst = 1000, 1000
	provider, err := data.NewKCProvider(f.f.p, config)
	if err != nil {
		t.Fatal(err)
	}
	if err = provider.ConfigureLoadBalancer(seedLBInstallation(f.api)); err != nil {
		t.Fatal(err)
	}
	startSlowAudit(t, provider)
	initial := w
	initial.ActiveOperation = true
	first, err := provider.ObserveLoadBalancer(f.f.ctx, initial)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := f.api.Counts()
	for i := 0; i < 5; i++ {
		next, err := provider.ObserveLoadBalancer(f.f.ctx, w)
		if err != nil || !next.Proof.CollectedAt.Equal(first.Proof.CollectedAt) {
			t.Fatalf("deleted LB forced a newer complete audit: %+v %v", next, err)
		}
	}
	after, _ := f.api.Counts()
	if before["LIST/pods"] != after["LIST/pods"] {
		t.Fatal("deleted LB repeated complete relation lists")
	}
	active := w
	active.Resource.State = biz.Deleting
	current, err := provider.ObserveLoadBalancer(f.f.ctx, active)
	if err != nil || !current.Proof.CollectedAt.After(first.Proof.CollectedAt) {
		t.Fatalf("active cleanup reused old tombstone proof: %+v %v", current, err)
	}
}
