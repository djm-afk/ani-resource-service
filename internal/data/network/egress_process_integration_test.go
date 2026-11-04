package data_test

import (
	"context"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	controlled "github.com/zhangzhe-ctrl/ani-resource-service/tests/net05a/provider"
	"github.com/zhangzhe-ctrl/ani-resource-service/tests/testenv"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestEgressServiceProcessesRecoverLostCreateAndDelete(t *testing.T) {
	if os.Getenv("NETWORK_TEST_ADMIN_DSN") == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "network-egress")
	build := exec.Command("go", "build", "-race", "-trimpath", "-o", binary, "./cmd/ani-resource-service")
	build.Dir = root
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build egress recovery process: %v\n%s", err, output)
	}
	createReached, createRelease := make(chan struct{}), make(chan struct{})
	deleteReached, deleteRelease := make(chan struct{}), make(chan struct{})
	defer close(createRelease)
	defer close(deleteRelease)
	var sawCreate, sawDelete atomic.Bool
	intercept := func(w http.ResponseWriter, r *http.Request, api *controlled.Server) bool {
		create := r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/eips") && sawCreate.CompareAndSwap(false, true)
		deletion := r.Method == "DELETE" && strings.Contains(r.URL.Path, "/eips/") && sawDelete.CompareAndSwap(false, true)
		if !create && !deletion {
			return false
		}
		rec := httptest.NewRecorder()
		api.Backend.ServeHTTP(rec, r)
		if create {
			close(createReached)
			<-createRelease
		} else {
			close(deleteReached)
			<-deleteRelease
		}
		for k, vs := range rec.Header() {
			w.Header()[k] = vs
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
		return true
	}
	f, api, db, kubeconfig := newEgressKCFixture(t, intercept)
	accepted, err := f.e.CreateEIP(f.ctx, biz.EgressIntent{Name: "process-crash", IdempotencyKey: "process-crash"})
	if err != nil {
		t.Fatal(err)
	}
	start := func() *networkProcess {
		return startNetworkProcess(t, root, binary, db.RuntimeDSN, kubeconfig, testenv.SigningKey(), "egress")
	}
	first := start()
	select {
	case <-createReached:
	case <-time.After(12 * time.Second):
		t.Fatal("service never reached EIP POST crash point")
	}
	first.kill(t)
	createRelease <- struct{}{}
	second := start()
	waitState := func(state biz.ResourceState) {
		t.Helper()
		awaitNET05A(t, 15*time.Second, func() bool { v, err := f.e.GetEIP(f.ctx, "", accepted.ID); return err == nil && v.State == state })
	}
	waitState(biz.Available)
	api.Backend.Mu.Lock()
	creates := api.Backend.Creates["eips"]
	api.Backend.Mu.Unlock()
	if creates != 1 {
		t.Fatal("crash recovery submitted duplicate EIP", creates)
	}
	current, err := f.e.GetEIP(f.ctx, "", accepted.ID)
	if err != nil || current.LastOperationID != accepted.LastOperationID {
		t.Fatal("process restart lost receipt", err)
	}
	deleting, err := f.e.DeleteEIP(f.ctx, "", accepted.ID)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-deleteReached:
	case <-time.After(12 * time.Second):
		t.Fatal("service never reached EIP DELETE crash point")
	}
	second.kill(t)
	deleteRelease <- struct{}{}
	third := start()
	waitState(biz.Deleted)
	current, err = f.e.GetEIP(f.ctx, "", accepted.ID)
	if err != nil || current.LastOperationID != deleting.LastOperationID {
		t.Fatal("delete restart lost operation", err)
	}
	var retired bool
	var pending string
	if err = f.owner.QueryRow(f.ctx, `SELECT r.retired,b.pending_action FROM network_reconciliations r JOIN network_provider_bindings b USING(tenant_id,eip_id) WHERE r.tenant_id=$1 AND r.eip_id=$2`, f.tenant, accepted.ID).Scan(&retired, &pending); err != nil || !retired || pending != "" {
		t.Fatal("completed deletion did not retire its task", retired, pending, err)
	}
	third.stop(t)
	replay, err := f.e.CreateEIP(context.WithoutCancel(f.ctx), biz.EgressIntent{Name: "process-crash", IdempotencyKey: "process-crash"})
	if err != nil || replay.ID != accepted.ID || replay.State != biz.Provisioning {
		t.Fatal("completed process recovery changed immutable creation receipt", err)
	}
}
