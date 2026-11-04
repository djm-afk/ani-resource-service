package data_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network"
	"k8s.io/client-go/rest"
)

func TestConfirmedProviderRejectionKeepsOperationHistoryAndDeletionRecoverable(t *testing.T) {
	repository, _ := database(t)
	n := newNetwork(t, repository, time.Minute)
	api := &controlledKC{}
	var rejected atomic.Int32
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/vpcs") {
			rejected.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(422)
			_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"Status","status":"Failure","reason":"Invalid","code":422}`))
			return
		}
		api.ServeHTTP(w, r)
	}))
	defer host.Close()
	provider, err := data.NewKCProvider(repository, &rest.Config{Host: host.URL})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := biz.NewWorker(repository, provider, uuid.NewString(), fastPolicy())
	if err != nil {
		t.Fatal(err)
	}
	v := create(t, n, "rejected")
	runStep(t, worker)
	ctx := context.Background()
	failed, err := n.GetVPC(ctx, v.TenantID, v.ID)
	if err != nil || failed.State != biz.Failed || failed.Reason != biz.ProviderRejected {
		t.Fatalf("definite creation rejection: %+v %v", failed, err)
	}
	op, err := n.GetOperation(ctx, v.TenantID, v.LastOperationID)
	if err != nil || op.State != biz.OpFailed || op.Reason != biz.ProviderRejected {
		t.Fatalf("failed operation: %+v %v", op, err)
	}
	if err = repository.NotifyResource(ctx, v.TenantID, v.ID); err != nil {
		t.Fatal(err)
	}
	runStep(t, worker)
	current, err := n.GetVPC(ctx, v.TenantID, v.ID)
	if err != nil || current.State != biz.Failed || current.Reason != biz.ProviderMissing || rejected.Load() != 1 {
		t.Fatalf("current absence recreated an object or retained an old operation reason: %+v %v", current, err)
	}
	historic, err := n.GetOperation(ctx, v.TenantID, v.LastOperationID)
	if err != nil || !reflect.DeepEqual(op, historic) {
		t.Fatalf("current observation changed completed operation: %+v %v", historic, err)
	}
	deleting, err := n.DeleteVPC(ctx, v.TenantID, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	runStep(t, worker)
	final, err := n.GetVPC(ctx, v.TenantID, v.ID)
	if err != nil || final.State != biz.Deleted || final.LastOperationID != deleting.LastOperationID || rejected.Load() != 1 {
		t.Fatalf("failed create cleanup: %+v %v", final, err)
	}
}

func TestActualAdapterUsesOriginalIdentityAndOwnDeletionOnly(t *testing.T) {
	for _, fault := range []string{"foreign-owner", "changed-uid", "status-children", "unreported-children"} {
		t.Run(fault, func(t *testing.T) {
			repository, _ := database(t)
			n := newNetwork(t, repository, time.Minute)
			api := &controlledKC{}
			var childLists atomic.Int32
			host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/subnets") {
					childLists.Add(1)
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"apiVersion":"networking.kubercloud.com/v1","kind":"SubnetList","items":[{"metadata":{"name":"unexpected"},"spec":{"gateway":"foreign/parent"}}]}`))
					return
				}
				api.ServeHTTP(w, r)
			}))
			defer host.Close()
			provider, err := data.NewKCProvider(repository, &rest.Config{Host: host.URL})
			if err != nil {
				t.Fatal(err)
			}
			worker, err := biz.NewWorker(repository, provider, uuid.NewString(), fastPolicy())
			if err != nil {
				t.Fatal(err)
			}
			v := create(t, n, fault)
			runStep(t, worker)
			ctx := context.Background()
			before, err := n.GetVPC(ctx, v.TenantID, v.ID)
			if err != nil || before.State != biz.Available {
				t.Fatal(before, err)
			}
			api.mu.Lock()
			metadata := api.object["metadata"].(map[string]any)
			switch fault {
			case "foreign-owner":
				metadata["labels"].(map[string]any)["network.ani.io/managed-by"] = "external"
			case "changed-uid":
				metadata["uid"] = "replacement-uid"
			case "status-children":
				api.object["status"].(map[string]any)["subnets"] = map[string]any{"foreign/child": "10.42.1.0/24"}
			}
			api.mu.Unlock()
			if fault == "foreign-owner" || fault == "changed-uid" {
				if err = repository.NotifyResource(ctx, v.TenantID, v.ID); err != nil {
					t.Fatal(err)
				}
				runStep(t, worker)
				current, err := n.GetVPC(ctx, v.TenantID, v.ID)
				if err != nil || current.State != before.State || current.Reason != before.Reason || !current.ObservedAt.Equal(*before.ObservedAt) {
					t.Fatalf("identity conflict replaced the original resource projection: %+v %v", current, err)
				}
			}
			deleting, err := n.DeleteVPC(ctx, v.TenantID, v.ID)
			if err != nil {
				t.Fatal(err)
			}
			want := biz.Succeeded
			if fault == "foreign-owner" {
				want = biz.Blocked
			}
			var op biz.Operation
			awaitNET05A(t, time.Second, func() bool {
				runStep(t, worker)
				op, err = n.GetOperation(ctx, v.TenantID, deleting.LastOperationID)
				if err != nil {
					t.Fatal(err)
				}
				return op.State == want
			})
			if fault == "foreign-owner" && op.Reason != biz.ProviderOwnership {
				t.Fatalf("own identity conflict lost its operation reason: %+v", op)
			}
			api.mu.Lock()
			defer api.mu.Unlock()
			if fault == "foreign-owner" || fault == "changed-uid" {
				if api.deletes != 0 || api.object == nil {
					t.Fatal("foreign or replacement object was deleted")
				}
			} else if api.deletes != 1 || api.object != nil {
				t.Fatal("child status blocked deletion of the original object")
			}
			if childLists.Load() != 0 {
				t.Fatal("deletion scanned child resources")
			}
		})
	}
}

func TestProviderReadFailurePreservesProjectionAndNativeReadyIgnoresGeneration(t *testing.T) {
	repository, owner := database(t)
	n := newNetwork(t, repository, time.Minute)
	api := &controlledKC{}
	var outage atomic.Bool
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if outage.Load() {
			http.Error(w, "offline", 503)
			return
		}
		api.ServeHTTP(w, r)
	}))
	defer host.Close()
	provider, err := data.NewKCProvider(repository, &rest.Config{Host: host.URL})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := biz.NewWorker(repository, provider, uuid.NewString(), fastPolicy())
	if err != nil {
		t.Fatal(err)
	}
	v := create(t, n, "outage")
	runStep(t, worker)
	ctx := context.Background()
	before, err := n.GetVPC(ctx, v.TenantID, v.ID)
	if err != nil || before.State != biz.Available {
		t.Fatal(before, err)
	}
	history, err := n.GetOperation(ctx, v.TenantID, v.LastOperationID)
	if err != nil || history.State != biz.Succeeded {
		t.Fatal(history, err)
	}
	outage.Store(true)
	if err = repository.NotifyResource(ctx, v.TenantID, v.ID); err != nil {
		t.Fatal(err)
	}
	runStep(t, worker)
	got, err := n.GetVPC(ctx, v.TenantID, v.ID)
	if err != nil || got.State != before.State || got.Reason != before.Reason || !got.ObservedAt.Equal(*before.ObservedAt) || !got.ObservationStale {
		t.Fatalf("read failure changed the successful resource projection: %+v %v", got, err)
	}
	var observationError string
	if err = owner.QueryRow(ctx, `SELECT observation_error FROM network_reconciliations WHERE tenant_id=$1 AND vpc_id=$2`, v.TenantID, v.ID).Scan(&observationError); err != nil || observationError != string(biz.ProviderUnavailable) {
		t.Fatal("read failure lost its separate diagnostic", observationError, err)
	}
	outage.Store(false)
	api.mu.Lock()
	api.object["status"].(map[string]any)["observedGeneration"] = float64(0)
	api.mu.Unlock()
	if err = repository.NotifyResource(ctx, v.TenantID, v.ID); err != nil {
		t.Fatal(err)
	}
	awaitNET05A(t, time.Second, func() bool {
		runStep(t, worker)
		got, err = n.GetVPC(ctx, v.TenantID, v.ID)
		if err != nil {
			t.Fatal(err)
		}
		return !got.ObservationStale
	})
	if got.State != biz.Available || got.Reason != "" {
		t.Fatalf("supplemental generation overrode native Ready: %+v", got)
	}
	op, err := n.GetOperation(ctx, v.TenantID, v.LastOperationID)
	if err != nil || !reflect.DeepEqual(history, op) {
		t.Fatalf("observation rewrote completed operation: %+v %v", op, err)
	}
}
