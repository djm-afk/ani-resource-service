//go:build sandboxintegration

package sandbox_test

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/sandbox"
	data "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/sandbox"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
)

func sandboxClusterRole() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRole",
		"metadata": map[string]any{"name": "ani-sandbox-tenant", "uid": "cluster-role-uid"},
		"rules": []any{
			map[string]any{"apiGroups": []any{""}, "resources": []any{"pods", "persistentvolumeclaims"}, "verbs": []any{"get", "create", "delete"}},
			map[string]any{"apiGroups": []any{""}, "resources": []any{"pods/exec"}, "verbs": []any{"create"}},
			map[string]any{"apiGroups": []any{""}, "resources": []any{"secrets"}, "verbs": []any{"create"}},
		},
	}}
}

func TestTenantSpaceJournaledProviderPostgres(t *testing.T) {
	for _, lost := range []string{"", "namespaces", "roles"} {
		t.Run("lost="+lost, func(t *testing.T) {
			f := sandboxDatabase(t)
			tenant, op := uuid.NewString(), uuid.NewString()
			ctx := tenantSpaceContext(tenant)
			if _, err := f.repo.OpenTenantSpace(ctx, tenant, op); err != nil {
				t.Fatal(err)
			}
			client := fake.NewSimpleDynamicClient(runtime.NewScheme(), sandboxClusterRole())
			client.PrependReactor("create", "*", func(action ktesting.Action) (bool, runtime.Object, error) {
				obj := action.(ktesting.CreateAction).GetObject().(*unstructured.Unstructured).DeepCopy()
				obj.SetUID(types.UID("server-" + uuid.NewString()))
				if action.GetResource().Resource == "namespaces" {
					obj.Object["status"] = map[string]any{"phase": "Active"}
				}
				if err := client.Tracker().Create(action.GetResource(), obj, action.GetNamespace()); err != nil {
					t.Fatal(err)
				}
				if action.GetResource().Resource == lost {
					return true, nil, errors.New("response lost after provider accepted write")
				}
				return true, obj, nil
			})
			namespace := data.TenantNamespace{Client: client, NamespacePrefix: "tenant-"}
			access := data.TenantAccess{Client: client, NamespacePrefix: "tenant-", ServiceAccountNamespace: "ani-system", ServiceAccountName: "ani-resource-service"}
			uid, err := namespace.ProvisionJournaled(ctx, tenant, op, f.repo)
			if lost != "namespaces" && err != nil {
				t.Fatalf("namespace: %v", err)
			}
			if lost == "namespaces" && err == nil {
				t.Fatal("lost namespace response was accepted")
			}
			if err == nil {
				err = access.EnsureJournaled(ctx, tenant, op, uid, f.repo)
				if lost == "" && err != nil {
					t.Fatalf("access: %v", err)
				}
				if lost == "roles" && err == nil {
					t.Fatal("lost role response was accepted")
				}
			}
			space, err := f.repo.GetTenantSpace(ctx, tenant)
			if err != nil {
				t.Fatal(err)
			}
			if lost == "" {
				if space.PendingID != "" || space.NamespaceUID != uid {
					t.Fatalf("space: %#v", space)
				}
				var applied int
				if err = f.runtime.QueryRow(ctx, "SELECT count(*) FROM sandbox.tenant_space_mutations WHERE tenant_id=$1 AND outcome='applied'", tenant).Scan(&applied); err != nil {
					t.Fatal(err)
				}
				if applied != 5 {
					t.Fatalf("applied writes = %d, want 5", applied)
				}
				if _, err = namespace.ProvisionJournaled(ctx, tenant, op, f.repo); err != nil {
					t.Fatalf("namespace replay: %v", err)
				}
				if err = access.EnsureJournaled(ctx, tenant, op, uid, f.repo); err != nil {
					t.Fatalf("access replay: %v", err)
				}
				var replayCount int
				if err = f.runtime.QueryRow(ctx, "SELECT count(*) FROM sandbox.tenant_space_mutations WHERE tenant_id=$1", tenant).Scan(&replayCount); err != nil || replayCount != applied {
					t.Fatalf("replay created journal entry: %d %v", replayCount, err)
				}
				return
			}
			wantKind := "namespace_create"
			if lost == "roles" {
				wantKind = "pull_secret_role"
			}
			if space.PendingID == "" || space.PendingKind != wantKind {
				t.Fatalf("lost response not retained: %#v", space)
			}
			closed, err := f.repo.CloseTenantSpace(ctx, tenant, op)
			if err != nil || closed.Phase != "closing" {
				t.Fatalf("close pending: %#v %v", closed, err)
			}
			if _, err = namespace.ProvisionJournaled(ctx, tenant, op, f.repo); !errors.Is(err, biz.ErrConflict) {
				t.Fatalf("retry while closing = %v", err)
			}
			creates := 0
			for _, action := range client.Actions() {
				if action.GetVerb() == "create" && action.GetResource().Resource == lost {
					creates++
				}
			}
			if creates != 1 {
				t.Fatalf("%s create attempts: %d", lost, creates)
			}
		})
	}
}

func TestTenantSpaceWorkerReadyAndClosePostgres(t *testing.T) {
	f := sandboxDatabase(t)
	tenant, op := uuid.NewString(), uuid.NewString()
	ctx := tenantSpaceContext(tenant)
	if _, err := f.repo.OpenTenantSpace(ctx, tenant, op); err != nil {
		t.Fatal(err)
	}
	client := fake.NewSimpleDynamicClient(runtime.NewScheme(), sandboxClusterRole())
	client.PrependReactor("create", "*", func(action ktesting.Action) (bool, runtime.Object, error) {
		obj := action.(ktesting.CreateAction).GetObject().(*unstructured.Unstructured).DeepCopy()
		obj.SetUID(types.UID("server-" + uuid.NewString()))
		if action.GetResource().Resource == "namespaces" {
			obj.Object["status"] = map[string]any{"phase": "Active"}
		}
		if err := client.Tracker().Create(action.GetResource(), obj, action.GetNamespace()); err != nil {
			t.Fatal(err)
		}
		return true, obj, nil
	})
	client.PrependReactor("create", "selfsubjectreviews", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, &unstructured.Unstructured{Object: map[string]any{"status": map[string]any{"userInfo": map[string]any{"username": "system:serviceaccount:ani-system:ani-resource-service"}}}}, nil
	})
	provider := data.TenantSpaceProvisioner{Namespace: data.TenantNamespace{Client: client, NamespacePrefix: "tenant-"}, Access: data.TenantAccess{Client: client, NamespacePrefix: "tenant-"}}
	worker, err := biz.NewTenantSpaceWorker(f.repo, provider)
	if err != nil {
		t.Fatal(err)
	}
	if worked, err := worker.Step(ctx); !worked || err != nil {
		t.Fatalf("step: %v %v", worked, err)
	}
	space, err := f.repo.GetTenantSpace(ctx, tenant)
	if err != nil || space.Phase != "ready" || space.NamespaceUID == "" {
		t.Fatalf("ready: %#v %v", space, err)
	}
	if replay, err := f.repo.OpenTenantSpace(ctx, tenant, op); err != nil || replay.Phase != "ready" {
		t.Fatalf("ready replay: %#v %v", replay, err)
	}
	if worked, err := worker.Step(ctx); worked || err != nil {
		t.Fatalf("ready scheduled again: %v %v", worked, err)
	}
	closed, err := f.repo.CloseTenantSpace(ctx, tenant, op)
	if err != nil || closed.Phase != "closed" {
		t.Fatalf("close ready: %#v %v", closed, err)
	}
	if err = f.repo.MarkTenantSpaceReady(ctx, tenant, op, space.NamespaceUID); !errors.Is(err, biz.ErrConflict) {
		t.Fatalf("closed marked ready: %v", err)
	}
}

func TestTenantSpaceCloseRacesSandboxAcceptPostgres(t *testing.T) {
	f := sandboxDatabase(t)
	for i := 0; i < 12; i++ {
		tenant, op := uuid.NewString(), uuid.NewString()
		ctx := tenantSpaceContext(tenant)
		if _, err := f.repo.OpenTenantSpace(ctx, tenant, op); err != nil {
			t.Fatal(err)
		}
		uid := "namespace-" + uuid.NewString()
		if _, err := f.repo.PinTenantSpaceNamespace(ctx, tenant, op, uid); err != nil {
			t.Fatal(err)
		}
		if err := f.repo.MarkTenantSpaceReady(ctx, tenant, op, uid); err != nil {
			t.Fatal(err)
		}
		command := biz.Create{TenantID: tenant, ID: uuid.NewString(), OperationID: uuid.NewString(), RequestDigest: strings.Repeat("a", 64), Spec: biz.CreateSpec{ImageDigest: "sha256:test", NamespaceUID: uid}}
		start := make(chan struct{})
		var acceptErr, closeErr error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); <-start; _, acceptErr = f.repo.AcceptCreate(ctx, command) }()
		go func() { defer wg.Done(); <-start; _, closeErr = f.repo.CloseTenantSpace(ctx, tenant, op) }()
		close(start)
		wg.Wait()
		if acceptErr == nil {
			if !errors.Is(closeErr, biz.ErrConflict) {
				t.Fatalf("accepted create but close = %v", closeErr)
			}
			if _, err := f.runtime.Exec(ctx, `UPDATE sandbox.sandboxes SET desired='deleted',observed='deleted',provider_refs='{"CleanupReceipt":"confirmed"}'::jsonb WHERE tenant_id=$1`, tenant); err != nil {
				t.Fatal(err)
			}
			if _, err := f.repo.CloseTenantSpace(ctx, tenant, op); err != nil {
				t.Fatalf("close after cleanup: %v", err)
			}
			if ack, err := f.repo.AcceptCreate(ctx, command); err != nil || !ack.Replayed {
				t.Fatalf("accepted replay after close: %#v %v", ack, err)
			}
		} else if !errors.Is(acceptErr, biz.ErrConflict) || closeErr != nil {
			t.Fatalf("accept = %v close = %v", acceptErr, closeErr)
		}
	}
}
