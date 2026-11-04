package data_test

import (
	"context"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/tests/testenv"
	"k8s.io/client-go/rest"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type attachmentConsumer struct {
	mu    sync.Mutex
	value biz.ConsumerSubmission
	err   error
}

func (c *attachmentConsumer) GetSubmission(context.Context, biz.Attachment) (biz.ConsumerSubmission, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.value, c.err
}

type attachmentFixture struct {
	p              *data.Postgres
	owner          *pgxpool.Pool
	n              *biz.Network
	a              *biz.Attachments
	provider       *data.KCProvider
	api            *testenv.KC
	r              biz.PrepareAttachment
	consumer       *attachmentConsumer
	worker         *biz.AttachmentWorker
	resourceWorker *biz.Worker
}

func newAttachmentFixture(t *testing.T) attachmentFixture {
	t.Helper()
	p, owner := database(t)
	n := newNetwork(t, p, time.Minute)
	tenant := uuid.NewString()
	namespace := "tenant-" + tenant
	// Tenant Namespace provisioning is an external platform responsibility.
	// Supply its real contract before admission; Network must not create it.
	api := &testenv.KC{Objects: map[string]map[string]any{
		"namespaces//" + namespace: {"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": namespace, "uid": uuid.NewString(), "labels": map[string]any{"network.ani.io/managed-by": "ani-network-service", "network.ani.io/tenant-id": tenant}}, "status": map[string]any{"phase": "Active"}},
	}, Creates: map[string]int{}, Deletes: map[string]int{}}
	host := httptest.NewServer(api)
	t.Cleanup(host.Close)
	provider, e := data.NewKCProvider(p, &rest.Config{Host: host.URL, QPS: 1000, Burst: 1000})
	if e != nil {
		t.Fatal(e)
	}
	policy := biz.DefaultWorkerPolicy()
	policy.ObserveEvery = 5 * time.Millisecond
	policy.RetryMin = time.Millisecond
	w, e := biz.NewWorker(p, provider, uuid.NewString(), policy)
	if e != nil {
		t.Fatal(e)
	}
	v := availableVPC(t, n, w, tenant, "attachment-parent")
	s, e := n.CreateSubnet(context.Background(), biz.CreateSubnet{TenantID: tenant, VPCID: v.ID, Name: "attachment-subnet", CIDR: "10.42.1.0/24", IdempotencyKey: "subnet"})
	if e != nil {
		t.Fatal(e)
	}
	s = driveSubnet(t, n, w, s, biz.Available)
	a := biz.NewAttachments(p)
	r := biz.PrepareAttachment{TenantID: tenant, VPCID: v.ID, SubnetID: s.ID, InstanceID: "inst_" + uuid.NewString(), Slot: "primary", RequestKey: "primary", SubmissionID: uuid.NewString(), Generation: 1, ClusterID: "test-cluster", Namespace: "tenant-" + tenant}
	c := &attachmentConsumer{err: fmt.Errorf("consumer unavailable")}
	aw, e := biz.NewAttachmentWorker(p, c, provider, uuid.NewString(), policy)
	if e != nil {
		t.Fatal(e)
	}
	return attachmentFixture{p, owner, n, a, provider, api, r, c, aw, w}
}
func (f *attachmentFixture) prepare(t *testing.T) biz.Attachment {
	t.Helper()
	if f.r.ConsumerType == "" {
		switch f.r.WorkloadMode {
		case "controller_managed":
			f.r.ConsumerType = biz.ConsumerContainerInstance
		case "vm_managed":
			f.r.ConsumerType = biz.ConsumerVirtualMachine
		}
	}
	a, e := f.a.Prepare(context.Background(), f.r)
	if e != nil {
		t.Fatal(e)
	}
	return a
}
func (f attachmentFixture) step(t *testing.T, id string) biz.Attachment {
	t.Helper()
	if err := f.p.NotifyAttachment(context.Background(), f.r.TenantID, id); err != nil {
		t.Fatal(err)
	}
	time.Sleep(6 * time.Millisecond)
	if _, e := f.worker.Step(context.Background()); e != nil {
		t.Fatal(e)
	}
	a, e := f.a.Get(context.Background(), f.r.TenantID, id)
	if e != nil {
		t.Fatal(e)
	}
	return a
}
func consumerFor(a biz.Attachment, state, id string) biz.ConsumerSubmission {
	return biz.ConsumerSubmission{ProtocolVersion: 1, TenantID: a.TenantID, InstanceID: a.InstanceID, SubmissionID: a.SubmissionID, AttachmentID: a.ID, ClusterID: a.ClusterID, Namespace: a.Namespace, Generation: a.Generation, State: state, FinalizationID: id}
}
func attachmentPod(a biz.Attachment, name, uid string) map[string]any {
	return map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{"name": name, "namespace": a.Namespace, "uid": uid, "resourceVersion": "1", "labels": map[string]any{"network.ani.io/tenant-id": a.TenantID, "network.ani.io/instance-id": a.InstanceID, "network.ani.io/attachment-id": a.ID, "network.ani.io/submission-id": a.SubmissionID, "network.ani.io/generation": strconv.FormatInt(a.Generation, 10)}, "annotations": map[string]any{"networking.kubercloud.com/subnet": a.Plan.PrimaryNetworkRef}}}
}
func TestAttachmentPermanentIdentityTenantScopeAndParentDeletion(t *testing.T) {
	f := newAttachmentFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	const count = 12
	values := make(chan biz.Attachment, count)
	errs := make(chan error, count)
	var group sync.WaitGroup
	for i := 0; i < count; i++ {
		group.Add(1)
		go func() { defer group.Done(); a, e := f.a.Prepare(ctx, f.r); values <- a; errs <- e }()
	}
	group.Wait()
	close(values)
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	id := ""
	for a := range values {
		if id == "" {
			id = a.ID
		}
		if a.ID != id || a.State != biz.Reserved || a.Plan == nil {
			t.Fatal("prepare race changed identity", a)
		}
	}
	if _, e := f.n.DeleteSubnet(ctx, f.r.TenantID, f.r.SubnetID); biz.ReasonOf(e) != biz.ResourceInUse {
		t.Fatal("reserved failed to protect subnet", e)
	}
	r := f.r
	r.Generation++
	if _, e := f.a.Prepare(ctx, r); biz.ReasonOf(e) != biz.IdempotencyConflict {
		t.Fatal("changed intent accepted", e)
	}
	r = f.r
	r.RequestKey = "another"
	if _, e := f.a.Prepare(ctx, r); biz.ReasonOf(e) != biz.AttachmentConflict {
		t.Fatal("active primary slot duplicated", e)
	}
	other := uuid.NewString()
	if _, e := f.a.Get(ctx, other, id); biz.ReasonOf(e) != biz.ResourceNotFound {
		t.Fatal("cross-tenant attachment exposed", e)
	}
	if _, e := f.a.Confirm(ctx, biz.ConfirmAttachment{TenantID: other, AttachmentID: id, ExpectedVersion: 1, PodName: "pod", PodUID: uuid.NewString()}); biz.ReasonOf(e) != biz.ResourceNotFound {
		t.Fatal("cross-tenant confirm exposed", e)
	}
	// Foreign keys must preserve the complete tenant/parent/binding target.
	statements := []string{
		`UPDATE network_attachments SET tenant_id=$2 WHERE tenant_id=$1`,
		`UPDATE network_attachments SET binding_id=$2::uuid WHERE tenant_id=$1`,
		`INSERT INTO network_attachment_history(tenant_id,history_id,attachment_id,version,event,state,reason) SELECT $2::uuid,gen_random_uuid(),'` + id + `',1,'forged','reserved','' WHERE $1::uuid IS NOT NULL`,
	}
	for _, sql := range statements {
		_, e := f.owner.Exec(ctx, sql, f.r.TenantID, other)
		var pe *pgconn.PgError
		if !errors.As(e, &pe) || pe.Code != "23503" {
			t.Fatalf("attachment cross-target FK missing: %v", e)
		}
	}
	// Dynamic availability changes cannot invalidate already accepted intent.
	if _, e := f.owner.Exec(ctx, `UPDATE network_subnets SET state='degraded',observed_at=clock_timestamp()-interval '1 hour' WHERE tenant_id=$1 AND subnet_id=$2`, f.r.TenantID, f.r.SubnetID); e != nil {
		t.Fatal(e)
	}
	replay := f.prepare(t)
	if replay.ID != id {
		t.Fatal("dynamic state invalidated permanent replay")
	}
}
func TestAttachmentLegacyConfirmAndClosedReleaseUseManagementState(t *testing.T) {
	f := newAttachmentFixture(t)
	ctx := context.Background()
	a := f.prepare(t)
	podName, podUID := "durable-pod", uuid.NewString()
	a, err := f.a.Confirm(ctx, biz.ConfirmAttachment{TenantID: a.TenantID, AttachmentID: a.ID, ExpectedVersion: a.Version, ClusterID: a.ClusterID, Namespace: a.Namespace, PodName: podName, PodUID: podUID})
	if err != nil || a.State != biz.Attached {
		t.Fatal(a, err)
	}
	f.consumer.mu.Lock()
	f.consumer.err = nil
	f.consumer.value = consumerFor(a, "open", "")
	f.consumer.value.PodUIDs = []string{podUID}
	f.consumer.mu.Unlock()
	a = f.step(t, a.ID)
	if a.State != biz.Attached || a.PodUID != podUID {
		t.Fatal("provider absence changed management relationship", a)
	}
	finalization := uuid.NewString()
	a, err = f.a.Release(ctx, biz.ReleaseAttachment{TenantID: a.TenantID, AttachmentID: a.ID, ExpectedVersion: a.Version, FinalizationID: finalization})
	if err != nil || a.State != biz.Releasing {
		t.Fatal(a, err)
	}
	f.consumer.mu.Lock()
	f.consumer.value = consumerFor(a, "closed", finalization)
	f.consumer.mu.Unlock()
	a = f.step(t, a.ID)
	if a.State != biz.Released || a.ReleasedAt == nil {
		t.Fatal(a)
	}
	replay := f.prepare(t)
	if replay.ID != a.ID || replay.State != biz.Released || replay.Plan != nil {
		t.Fatal(replay)
	}
}

func TestAttachmentUnknownConsumerDoesNotExpireAndEpochRejectsLateWrite(t *testing.T) {
	f := newAttachmentFixture(t)
	ctx := context.Background()
	a := f.prepare(t)
	a = f.step(t, a.ID)
	if a.State != biz.Reserved || a.Reason != "" {
		t.Fatal("reserved was guessed unused", a)
	}
	time.Sleep(6 * time.Millisecond)
	work, found, e := f.p.ClaimAttachment(ctx, uuid.NewString(), time.Millisecond)
	if e != nil || !found {
		t.Fatal(found, e)
	}
	time.Sleep(3 * time.Millisecond)
	successor, found, e := f.p.ClaimAttachment(ctx, uuid.NewString(), time.Second)
	if e != nil || !found || successor.Epoch <= work.Epoch {
		t.Fatal("lease recovery missing", e)
	}
	e = f.p.FinishAttachment(ctx, work, biz.AttachmentProgress{State: biz.Released, FinalizationID: uuid.NewString(), NextDelay: time.Second})
	if !errors.Is(e, biz.ErrLeaseLost) {
		t.Fatal("old attachment lease wrote", e)
	}
	before, e := f.a.Get(ctx, a.TenantID, a.ID)
	if e != nil {
		t.Fatal(e)
	}
	after, e := f.a.Get(ctx, a.TenantID, a.ID)
	if e != nil || before.Version != after.Version {
		t.Fatal("GET advanced attachment")
	}
}
func TestAttachmentPrepareRacesDeleteSubnet(t *testing.T) {
	for i := 0; i < 6; i++ {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			f := newAttachmentFixture(t)
			ctx := context.Background()
			start := make(chan struct{})
			errs := make(chan error, 2)
			go func() { <-start; _, e := f.a.Prepare(ctx, f.r); errs <- e }()
			go func() { <-start; _, e := f.n.DeleteSubnet(ctx, f.r.TenantID, f.r.SubnetID); errs <- e }()
			close(start)
			a, b := <-errs, <-errs
			for _, err := range []error{a, b} {
				if err != nil && biz.ReasonOf(err) != biz.NetworkNotReady && biz.ReasonOf(err) != biz.ResourceInUse {
					t.Fatalf("unexpected race error: %v", err)
				}
			}
			if (a == nil) == (b == nil) {
				t.Fatalf("prepare/delete need exactly one winner: %v %v", a, b)
			}
			var count int
			var state string
			if e := f.owner.QueryRow(ctx, `SELECT s.state,(SELECT count(*) FROM network_attachments a WHERE a.tenant_id=s.tenant_id AND a.subnet_id=s.subnet_id) FROM network_subnets s WHERE s.tenant_id=$1 AND s.subnet_id=$2`, f.r.TenantID, f.r.SubnetID).Scan(&state, &count); e != nil {
				t.Fatal(e)
			}
			if state == "deleting" && count != 0 {
				t.Fatal("deleting subnet admitted attachment")
			}
		})
	}
}

func TestReleasedAttachmentDoesNotReopenForLateObjects(t *testing.T) {
	f := newAttachmentFixture(t)
	ctx := context.Background()
	a := f.prepare(t)
	original := a
	f.consumer.mu.Lock()
	f.consumer.value, f.consumer.err = consumerFor(a, "closed", uuid.NewString()), nil
	f.consumer.mu.Unlock()
	a = f.step(t, a.ID)
	if a.State != biz.Released {
		t.Fatal(a)
	}
	subnet, err := f.n.DeleteSubnet(ctx, a.TenantID, a.SubnetID)
	if err != nil {
		t.Fatal(err)
	}
	driveSubnet(t, f.n, f.resourceWorker, subnet, biz.Deleted)
	f.api.Mu.Lock()
	f.api.Objects["pods/"+a.Namespace+"/late"] = attachmentPod(original, "late", uuid.NewString())
	f.api.Mu.Unlock()
	a = f.step(t, a.ID)
	if a.State != biz.Released || a.Reason != "" {
		t.Fatal(a)
	}
	if _, err = f.n.CreateSubnet(ctx, biz.CreateSubnet{TenantID: a.TenantID, VPCID: a.VPCID, Name: "reuse", CIDR: subnet.CIDR, IdempotencyKey: "reuse"}); err != nil {
		t.Fatal("late provider object blocked local CIDR reuse", err)
	}
}

func TestAttachmentAddressReadUsesExactCurrentPod(t *testing.T) {
	f := newAttachmentFixture(t)
	a := f.prepare(t)
	a.State, a.PodName, a.PodUID = biz.Attached, "current", "current-uid"
	var reads atomic.Int64
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/api/v1/namespaces/"+a.Namespace+"/pods/current" {
			t.Errorf("unexpected provider request: %s %s", r.Method, r.URL.Path)
		}
		reads.Add(1)
		f.api.ServeHTTP(w, r)
	}))
	defer host.Close()
	provider, err := data.NewKCProvider(f.p, &rest.Config{Host: host.URL})
	if err != nil {
		t.Fatal(err)
	}
	c := consumerFor(a, "open", "")
	value, err := provider.ObserveAttachmentAddresses(context.Background(), a, c)
	if err != nil || reads.Load() != 1 || len(value.CurrentIPs) != 0 {
		t.Fatal(value, err, reads.Load())
	}
}
