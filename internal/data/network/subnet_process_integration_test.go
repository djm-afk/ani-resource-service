package data_test

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	networkv1 "github.com/zhangzhe-ctrl/ani-resource-service/api/network/v1"
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

func TestSubnetProcessesRecoverProviderSuccessAndUnknownDeletion(t *testing.T) {
	if os.Getenv("NETWORK_TEST_ADMIN_DSN") == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "network")
	build := exec.Command("go", "build", "-race", "-trimpath", "-o", binary, "./cmd/ani-resource-service")
	build.Dir = root
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build recovery process: %v\n%s", err, output)
	}
	t.Run("after-provider-success", func(t *testing.T) {
		const fault = "after-provider-success"
		fixture := testenv.NewDatabase(t)
		signingKey := testenv.SigningKey()
		// Keep Subnet-specific post-write recovery and original-object deletion.
		// Generic T1 process recovery remains in the VPC process test; Subnet CIDR
		// occupancy and expired-writer rejection remain in the database tests.
		migration := exec.Command(binary, "-migrate")
		migration.Env = append(os.Environ(), "ANI_NETWORK_MIGRATION_DSN="+fixture.Owner.Config().ConnString(), "ANI_NETWORK_RUNTIME_ROLE="+fixture.RuntimeRole)
		if output, err := migration.CombinedOutput(); err != nil {
			t.Fatalf("explicit migration replay failed: %v %s", err, output)
		}
		protocol := controlled.New()
		renewFacts := seedProcessVPCPlatformFacts(t, protocol)
		config := processVPCPresetConfig(t, root, "10.42.0.0/16")
		api := protocol.Backend
		reached := make(chan struct{})
		release := make(chan struct{})
		var captured atomic.Bool
		deleteReached := make(chan struct{})
		deleteRelease := make(chan struct{})
		var capturedDelete atomic.Bool
		host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			renewFacts(r)
			isObject := strings.Contains(r.URL.Path, "/subnets/")
			captureCreate := r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/subnets")
			if captureCreate && captured.CompareAndSwap(false, true) {
				recorder := httptest.NewRecorder()
				api.ServeHTTP(recorder, r)
				close(reached)
				<-release
				for key, values := range recorder.Header() {
					w.Header()[key] = values
				}
				w.WriteHeader(recorder.Code)
				_, _ = w.Write(recorder.Body.Bytes())
				return
			}
			if r.Method == "DELETE" && isObject && capturedDelete.CompareAndSwap(false, true) {
				recorder := httptest.NewRecorder()
				api.ServeHTTP(recorder, r)
				close(deleteReached)
				<-deleteRelease
				for key, values := range recorder.Header() {
					w.Header()[key] = values
				}
				w.WriteHeader(recorder.Code)
				_, _ = w.Write(recorder.Body.Bytes())
				return
			}
			protocol.ServeHTTP(w, r)
		}))
		defer host.Close()
		defer close(release)
		defer close(deleteRelease)
		kubeconfig := testenv.Kubeconfig(t, host.URL)
		start := func() *networkProcess {
			return startNetworkProcess(t, root, binary, fixture.RuntimeDSN, kubeconfig, signingKey, "subnet", "config="+config)
		}
		first := start()
		tenant := uuid.NewString()
		client, closeClient := first.client(t, tenant)
		parent, err := client.CreateVPC(context.Background(), &networkv1.CreateVPCRequest{TenantId: tenant, Name: "parent", Cidr: "10.42.0.0/16", IdempotencyKey: "parent"})
		if err != nil {
			t.Fatal(err)
		}
		waitRPCState(t, client, tenant, parent.Vpc.Id, networkv1.ResourceState_RESOURCE_STATE_AVAILABLE)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		accepted, err := client.CreateSubnet(ctx, &networkv1.CreateSubnetRequest{TenantId: tenant, VpcId: parent.Vpc.Id, Name: fault, Cidr: "10.42.1.0/24", IdempotencyKey: "restart"})
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-reached:
		case <-time.After(8 * time.Second):
			t.Fatal("process did not reach selected crash point")
		}
		first.kill(t)
		closeClient() // No Gateway/Core or original requester remains.
		release <- struct{}{}
		api.Mu.Lock()
		createCountBefore := api.Creates["subnets"]
		api.Mu.Unlock()
		second := start()
		contender := start() // A distinct service process shares the same PG lease authority.
		resumed, closeResumed := second.client(t, tenant)
		defer closeResumed()
		current := waitSubnetRPCState(t, resumed, tenant, accepted.Subnet.Id, networkv1.ResourceState_RESOURCE_STATE_AVAILABLE)
		if current.LastOperationId != accepted.Subnet.LastOperationId {
			t.Fatal("restart changed operation identity")
		}
		api.Mu.Lock()
		creates := api.Creates["subnets"]
		var saved []byte
		var savedKey string
		for key, object := range api.Objects {
			metadata, _ := object["metadata"].(map[string]any)
			labels, _ := metadata["labels"].(map[string]any)
			if object["kind"] == "Subnet" && labels["network.ani.io/resource-id"] == accepted.Subnet.Id {
				saved, _ = json.Marshal(object)
				savedKey = key
			}
		}
		api.Mu.Unlock()
		if len(saved) == 0 {
			t.Fatal("created tenant Subnet fixture is missing")
		}
		if creates != 1 {
			t.Fatalf("provider success replay created %d objects", creates)
		}
		contender.stop(t)
		ctx, cancel = context.WithTimeout(context.Background(), 3*time.Second)
		deleting, err := resumed.DeleteSubnet(ctx, &networkv1.DeleteSubnetRequest{TenantId: tenant, SubnetId: current.Id})
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-deleteReached:
		case <-time.After(8 * time.Second):
			output, _ := os.ReadFile(second.logPath)
			t.Fatalf("process never issued DELETE; process trace: %s", output)
		}
		second.kill(t)
		deleteRelease <- struct{}{}
		deletionProcess := start()
		cleanup, closeCleanup := deletionProcess.client(t, tenant)
		defer closeCleanup()
		deleted := waitSubnetRPCState(t, cleanup, tenant, current.Id, networkv1.ResourceState_RESOURCE_STATE_DELETED)
		ctx, cancel = context.WithTimeout(context.Background(), 3*time.Second)
		completed, err := cleanup.GetOperation(ctx, &networkv1.GetOperationRequest{TenantId: tenant, OperationId: deleting.Subnet.LastOperationId})
		cancel()
		if err != nil || completed.Operation.State != networkv1.OperationState_OPERATION_STATE_SUCCEEDED {
			t.Fatalf("delete did not complete: %v %v", completed, err)
		}
		if deleted.LastOperationId != deleting.Subnet.LastOperationId {
			t.Fatal("delete recovery changed operation identity")
		}
		// A later object with the same name belongs to a different lifecycle.
		// Restarting Res must retain the completed delete without reclaiming it.
		deletionProcess.kill(t)
		api.Mu.Lock()
		var restored map[string]any
		_ = json.Unmarshal(saved, &restored)
		restored["metadata"].(map[string]any)["uid"] = uuid.NewString()
		api.Objects[savedKey] = restored
		api.Mu.Unlock()
		fourth := start()
		restarted, closeRestarted := fourth.client(t, tenant)
		defer closeRestarted()
		waitSubnetRPCState(t, restarted, tenant, current.Id, networkv1.ResourceState_RESOURCE_STATE_DELETED)
		api.Mu.Lock()
		untouched := api.Objects[savedKey] != nil && api.Deletes["subnets"] == 1
		api.Mu.Unlock()
		if !untouched {
			t.Fatal("completed delete reclaimed a later object")
		}
		ctx, cancel = context.WithTimeout(context.Background(), 3*time.Second)
		historic, err := restarted.GetOperation(ctx, &networkv1.GetOperationRequest{TenantId: tenant, OperationId: deleting.Subnet.LastOperationId})
		cancel()
		if err != nil || !historic.Operation.CompletedAt.AsTime().Equal(completed.Operation.CompletedAt.AsTime()) {
			t.Fatalf("restart rewrote historic operation: %v %v", historic, err)
		}
		fourth.stop(t)
		t.Logf("%s: Subnet=%s create_op=%s create_count_before_crash=%d final_creates=%d delete_op=%s; five distinct service processes (including two concurrent replicas) used the same database and API state", fault, current.Id, accepted.Subnet.LastOperationId, createCountBefore, creates, deleting.Subnet.LastOperationId)
	})
}
func waitSubnetRPCState(t *testing.T, client networkv1.NetworkServiceClient, tenant, id string, state networkv1.ResourceState) *networkv1.Subnet {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	last := ""
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		v, err := client.GetSubnet(ctx, &networkv1.GetSubnetRequest{TenantId: tenant, SubnetId: id})
		cancel()
		if err == nil {
			if v.Subnet.State == state {
				return v.Subnet
			}
			last = fmt.Sprintf("state=%s reason=%s", v.Subnet.State, v.Subnet.Reason)
		} else {
			last = err.Error()
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("RPC never reached %s: %s", state, last)
	return nil
}
