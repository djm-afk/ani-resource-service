package data_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	data "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network"
	"k8s.io/client-go/rest"
)

func TestManualVNicPGCreateResponseLossRecoversOriginalIdentity(t *testing.T) {
	f := newAttachmentFixture(t)
	ctx := context.Background()
	type createdIdentity struct {
		name, uid string
		err       error
	}
	lost := make(chan createdIdentity, 1)
	var posts atomic.Int32
	var dropped atomic.Bool
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/vnics") {
			posts.Add(1)
			if dropped.CompareAndSwap(false, true) {
				// Apply the actual HTTP request to the external KCN fixture, then
				// close the connection before its successful response reaches KCProvider.
				response := httptest.NewRecorder()
				f.api.ServeHTTP(response, r)
				var object struct {
					Metadata struct{ Name, UID string }
				}
				err := json.Unmarshal(response.Body.Bytes(), &object)
				if err == nil && (response.Code != http.StatusCreated || object.Metadata.Name == "" || object.Metadata.UID == "") {
					err = fmt.Errorf("create did not succeed before response loss: HTTP %d", response.Code)
				}
				connection, _, hijackErr := w.(http.Hijacker).Hijack()
				if hijackErr == nil {
					_ = connection.Close()
				} else if err == nil {
					err = hijackErr
				}
				lost <- createdIdentity{object.Metadata.Name, object.Metadata.UID, err}
				return
			}
		}
		f.api.ServeHTTP(w, r)
	}))
	t.Cleanup(host.Close)
	newWorker := func() {
		t.Helper()
		provider, err := data.NewKCProvider(f.p, &rest.Config{Host: host.URL, QPS: 1000, Burst: 1000})
		if err != nil {
			t.Fatal(err)
		}
		policy := biz.DefaultWorkerPolicy()
		policy.ObserveEvery, policy.RetryMin = 5*time.Millisecond, time.Millisecond
		f.provider = provider
		f.resourceWorker, err = biz.NewWorker(f.p, provider, uuid.NewString(), policy)
		if err != nil {
			t.Fatal(err)
		}
	}
	newWorker()
	accepted, err := f.n.CreateVNic(ctx, biz.CreateVNic{TenantID: f.r.TenantID, SubnetID: f.r.SubnetID, Name: "manual lost-response", IdempotencyKey: "lost-response"})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for !dropped.Load() && time.Now().Before(deadline) {
		if _, err = f.resourceWorker.Step(ctx); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	var original createdIdentity
	select {
	case original = <-lost:
		if original.err != nil {
			t.Fatal(original.err)
		}
	default:
		t.Fatal("did not lose the successful VNIC create response")
	}
	var pending, uid string
	if err = f.owner.QueryRow(ctx, `SELECT pending_action,provider_uid FROM network_provider_bindings WHERE tenant_id=$1 AND vnic_id=$2`, accepted.TenantID, accepted.ID).Scan(&pending, &uid); err != nil {
		t.Fatal(err)
	}
	if pending != "create" || uid != "" {
		t.Fatalf("response loss did not leave the durable uncertain create: pending=%q uid=%q", pending, uid)
	}
	// Discard both production adapter and worker. Recovery uses PG and the
	// original external name/UID, without an in-memory successful response.
	newWorker()
	recovered := manualVNic(t, f, "lost-response")
	operation, err := f.n.GetOperation(ctx, accepted.TenantID, accepted.LastOperationID)
	if err != nil || operation.State != biz.Succeeded || recovered.ID != accepted.ID || recovered.LastOperationID != accepted.LastOperationID || recovered.ProviderName != original.name || recovered.ProviderUID != original.uid || posts.Load() != 1 {
		t.Fatal("recovery changed the original identity or repeated POST", recovered, operation, posts.Load(), err)
	}
	f.api.Mu.Lock()
	defer f.api.Mu.Unlock()
	if f.api.Creates["vnics"] != 1 || f.api.Invalid != "" {
		t.Fatal("recovery created a second external VNIC", f.api.Creates["vnics"], f.api.Invalid)
	}
}

func TestManualVNicPGReservationRacesDelete(t *testing.T) {
	f := newAttachmentFixture(t)
	ctx := context.Background()
	for attempt := 0; attempt < 4; attempt++ {
		v := manualVNic(t, f, fmt.Sprintf("reserve-delete-%d", attempt))
		start := make(chan struct{})
		reserved, deleted := make(chan error, 1), make(chan error, 1)
		go func() {
			<-start
			_, err := f.n.ReserveVNic(ctx, biz.VNicReservation{TenantID: v.TenantID, VNicID: v.ID, ConsumerKind: "vm", ConsumerID: "racing-vm", Slot: "primary", OperationID: uuid.NewString(), SubnetID: v.SubnetID, Revision: 1})
			reserved <- err
		}()
		go func() {
			<-start
			_, err := f.n.DeleteVNic(ctx, v.TenantID, v.ID)
			deleted <- err
		}()
		close(start)
		reserveErr, deleteErr := <-reserved, <-deleted
		if (reserveErr == nil) == (deleteErr == nil) {
			t.Fatalf("exactly one concurrent acceptance must succeed: reserve=%v delete=%v", reserveErr, deleteErr)
		}
		if deleteErr == nil {
			if biz.ReasonOf(reserveErr) != biz.ResourceBusy {
				t.Fatal("reservation did not reject deleting VNIC", reserveErr)
			}
			deadline := time.Now().Add(time.Second)
			for time.Now().Before(deadline) {
				if _, err := f.resourceWorker.Step(ctx); err != nil {
					t.Fatal(err)
				}
				got, err := f.n.GetVNic(ctx, v.TenantID, v.ID)
				if err != nil {
					t.Fatal(err)
				}
				if got.State == biz.Deleted {
					break
				}
				time.Sleep(2 * time.Millisecond)
			}
		} else if biz.ReasonOf(deleteErr) != biz.ResourceBusy {
			t.Fatal("deletion did not reject reserved VNIC", deleteErr)
		}
		var state string
		var activeBindings int
		if err := f.owner.QueryRow(ctx, `SELECT state,(SELECT count(*) FROM network_vnic_bindings b WHERE b.tenant_id=v.tenant_id AND b.vnic_id=v.vnic_id AND b.released_at IS NULL) FROM network_vnics v WHERE tenant_id=$1 AND vnic_id=$2`, v.TenantID, v.ID).Scan(&state, &activeBindings); err != nil {
			t.Fatal(err)
		}
		if deleteErr == nil && (state != string(biz.Deleted) || activeBindings != 0) {
			t.Fatalf("deleted VNIC retained an active binding: state=%s active=%d", state, activeBindings)
		}
		if reserveErr == nil && (state != string(biz.Available) || activeBindings != 1) {
			t.Fatalf("winning reservation was not retained: state=%s active=%d", state, activeBindings)
		}
	}
}
