package vm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/flowcontrol"
)

func TestRuntimeReadsLeaveCommandBudgetAvailable(t *testing.T) {
	v := biz.VM{TenantID: uuid.NewString(), ID: uuid.NewString(), Spec: biz.Spec{Namespace: "tenant-runtime", NamespaceUID: uuid.NewString(), Digest: stringOfDigest()}, Native: biz.ObjectRef{Name: "budget-vm", UID: uuid.NewString()}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/virtualmachineinstances/"):
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"Status","reason":"NotFound","code":404}`))
		case strings.Contains(r.URL.Path, "/virtualmachines/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": "kubevirt.io/v1", "kind": "VirtualMachine", "metadata": map[string]any{"name": v.Native.Name, "namespace": v.Spec.Namespace, "uid": v.Native.UID, "labels": labelVM(v)}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": v.Spec.Namespace, "uid": v.Spec.NamespaceUID, "labels": map[string]string{"network.ani.io/tenant-id": v.TenantID, "network.ani.io/managed-by": "ani-network-service"}}})
		}
	}))
	defer server.Close()
	commandBudget := flowcontrol.NewTokenBucketRateLimiter(1, 1)
	cfg := &rest.Config{Host: server.URL, RateLimiter: commandBudget}
	reader, err := NewRuntimeReader(cfg)
	if err != nil {
		t.Fatal(err)
	}
	value, err := reader.ReadRuntime(context.Background(), v)
	if err != nil || value.State != "NOT_RUNNING" {
		t.Fatalf("runtime read: %+v %v", value, err)
	}
	command, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, err = command.Resource(nsResource).Get(ctx, v.Spec.Namespace, metav1.GetOptions{}); err != nil {
		t.Fatalf("optional runtime reads consumed the command request budget: %v", err)
	}
}

func TestRuntimeReadersBoundConcurrentRequests(t *testing.T) {
	entered := make(chan struct{}, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-r.Context().Done()
	}))
	defer server.Close()
	reader, err := NewRuntimeReader(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	v := biz.VM{Spec: biz.Spec{Namespace: "tenant-runtime"}}
	done := make(chan struct{}, 2)
	for range 2 {
		go func() { _, _ = reader.ReadRuntime(ctx, v); done <- struct{}{} }()
	}
	for range 2 {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("runtime request did not reach external boundary")
		}
	}
	if _, err = reader.ReadRuntime(ctx, v); !errors.Is(err, biz.ErrDependency) {
		t.Fatalf("third viewer queued unbounded runtime work: %v", err)
	}
	cancel()
	for range 2 {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("canceled runtime request did not release its slot")
		}
	}
}
