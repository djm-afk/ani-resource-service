package main

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	pb "github.com/zhangzhe-ctrl/ani-resource-service/api/clusterops/v1"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/clusterops"
	conf "github.com/zhangzhe-ctrl/ani-resource-service/internal/conf/v1"
	sandboxdata "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/sandbox"
	"github.com/zhangzhe-ctrl/ani-resource-service/tests/testenv"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// Two independent task databases exercise the real read adapters; no owner
// worker, namespace initialization, or live tenant registration is involved.
func TestClusterOpsNamespacesPostgresBothSourcesFailureAndRecovery(t *testing.T) {
	network := testenv.NewDatabase(t)
	sandbox := testenv.NewDatabase(t)
	ctx := context.Background()
	for _, sql := range []string{
		"CREATE SCHEMA sandbox",
		"CREATE TABLE sandbox.tenant_spaces(tenant_id uuid PRIMARY KEY, phase text NOT NULL, namespace_uid text)",
		"GRANT USAGE ON SCHEMA sandbox TO " + pgx.Identifier{sandbox.RuntimeRole}.Sanitize(),
		"GRANT SELECT ON sandbox.tenant_spaces TO " + pgx.Identifier{sandbox.RuntimeRole}.Sanitize(),
	} {
		if _, err := sandbox.Owner.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	networkOnly, both := uuid.NewString(), uuid.NewString()
	for _, tenant := range []string{networkOnly, both} {
		if _, err := network.Owner.Exec(ctx, "INSERT INTO network_tenant_namespaces(tenant_id,cluster_id,namespace,namespace_uid) VALUES($1,'test-cluster',$2,NULL)", tenant, "tenant-"+tenant); err != nil {
			t.Fatal(err)
		}
	}
	sandboxTenants := []string{both}
	for _, phase := range []string{"open", "ready", "closing", "closed"} {
		tenant := uuid.NewString()
		sandboxTenants = append(sandboxTenants, tenant)
		if _, err := sandbox.Owner.Exec(ctx, "INSERT INTO sandbox.tenant_spaces(tenant_id,phase,namespace_uid) VALUES($1,$2,NULL)", tenant, phase); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := sandbox.Owner.Exec(ctx, "INSERT INTO sandbox.tenant_spaces(tenant_id,phase) VALUES($1,'closed')", both); err != nil {
		t.Fatal(err)
	}
	reader, err := sandboxdata.OpenNamespaceRegistrationReader(ctx, sandbox.RuntimeDSN, "test-cluster")
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	policy := biz.NewNamespacePolicy(network.Repository, reader)
	names, err := policy.ProtectedNamespaces(ctx, "test-cluster")
	if err != nil || len(names) != 6 {
		t.Fatal(names, err)
	}
	for _, tenant := range append(sandboxTenants, networkOnly) {
		if _, ok := names["tenant-"+tenant]; !ok {
			t.Fatal("registration omitted", tenant)
		}
	}
	if _, err = reader.ListRegisteredNamespaceNames(ctx, "other-cluster"); err == nil {
		t.Fatal("reader mixed cluster configurations")
	}
	if _, err = sandbox.Owner.Exec(ctx, "ALTER TABLE sandbox.tenant_spaces RENAME TO unavailable_tenant_spaces"); err != nil {
		t.Fatal(err)
	}
	// Constructor remains usable when the dependency is temporarily unreadable.
	recovering, err := sandboxdata.OpenNamespaceRegistrationReader(ctx, sandbox.RuntimeDSN, "test-cluster")
	if err != nil {
		t.Fatal("constructor performed a readiness query", err)
	}
	defer recovering.Close()
	if _, err = policy.ProtectedNamespaces(ctx, "test-cluster"); err == nil {
		t.Fatal("partial set treated as complete")
	}
	if _, err = recovering.ListRegisteredNamespaceNames(ctx, "test-cluster"); err == nil {
		t.Fatal("unreadable source treated as empty")
	}
	if _, err = sandbox.Owner.Exec(ctx, "ALTER TABLE sandbox.unavailable_tenant_spaces RENAME TO tenant_spaces"); err != nil {
		t.Fatal(err)
	}
	if names, err = policy.ProtectedNamespaces(ctx, "test-cluster"); err != nil || len(names) != 6 {
		t.Fatal("existing policy did not recover", names, err)
	}
	if recovered, err := recovering.ListRegisteredNamespaceNames(ctx, "test-cluster"); err != nil || len(recovered) != 5 {
		t.Fatal("reader permanently froze", recovered, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = policy.ProtectedNamespaces(canceled, "test-cluster"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}

	var posts atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1" {
			_ = json.NewEncoder(w).Encode(metav1.APIResourceList{GroupVersion: "v1", APIResources: []metav1.APIResource{{Name: "configmaps", Kind: "ConfigMap", Namespaced: true, Verbs: metav1.Verbs{"create", "list", "get", "update", "delete"}}}})
			return
		}
		if r.Method != "POST" {
			t.Error("unexpected native request", r.Method)
		}
		posts.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "x", "namespace": "platform", "uid": "uid", "resourceVersion": "rv"}})
	}))
	defer api.Close()
	dsnFile := filepath.Join(t.TempDir(), "dsn")
	if err = os.WriteFile(dsnFile, []byte(sandbox.RuntimeDSN), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &conf.Network{ClusterId: "test-cluster", Kubeconfig: testenv.Kubeconfig(t, api.URL)}
	caller := biz.WithCaller(ctx, biz.Caller{Platform: true, Actor: "governance:user:7", DirectCaller: "ani-governance", RequestID: "r"})
	request := &pb.CreateResourceRequest{Ref: &pb.ResourceRef{ClusterId: "test-cluster", Type: &pb.ResourceTypeRef{Version: "v1", Resource: "configmaps"}, Namespace: "platform", Name: "x"}, ManifestYaml: "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: x}"}
	for _, compute := range []*computeComponents{nil, {settings: computeSettings{ClusterID: "other-cluster", DatabaseDSNFile: dsnFile}}, {settings: computeSettings{ClusterID: "test-cluster", DatabaseDSNFile: dsnFile}}} {
		components, e := openClusterOps(ctx, cfg, network.Repository, compute)
		if e != nil {
			t.Fatal(e)
		}
		_, e = components.service.CreateResource(caller, request)
		components.Close()
		matching := compute != nil && compute.settings.ClusterID == cfg.ClusterId
		if matching {
			if e != nil {
				t.Fatal("matching sources unavailable", e)
			}
		} else if status.Code(e) != codes.Unavailable {
			t.Fatal("missing/mismatched compute source treated as zero tenants", e)
		}
	}
	if posts.Load() != 1 {
		t.Fatal("namespace write reached API with incomplete sources", posts.Load())
	}
	var count int
	if err = network.Owner.QueryRow(ctx, "SELECT count(*) FROM network_operations").Scan(&count); err != nil || count != 0 {
		t.Fatal("registration read started owner lifecycle", count, err)
	}
}
