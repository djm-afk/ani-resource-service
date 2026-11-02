package data_test

import (
	"context"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/tenantnamespace"
	"github.com/zhangzhe-ctrl/ani-resource-service/tests/testenv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTenantNamespaceConcurrentAdmissionRejectsUIDConflict(t *testing.T) {
	f := testenv.NewDatabase(t)
	repo := f.Repository
	if err := repo.ConfigureVPCCIDRPresets([]string{"10.42.0.0/16"}); err != nil {
		t.Fatal(err)
	}
	const tenant = "11111111-1111-4111-8111-111111111111"
	var reads atomic.Int32
	repo.ConfigureTenantNamespaceResolver(func(context.Context, string) (tenantnamespace.Identity, error) {
		uid := "uid-a"
		if reads.Add(1) > 1 {
			uid = "uid-b"
		}
		return tenantnamespace.Identity{TenantID: tenant, ClusterID: "test-cluster", Name: "tenant-" + tenant, UID: uid}, nil
	})
	n, err := biz.NewNetwork(repo, []byte("0123456789abcdef0123456789abcdef"), time.Minute, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, key := range []string{"a", "b"} {
		wg.Add(1)
		go func(key string) {
			defer wg.Done()
			_, err := n.CreateVPC(context.Background(), biz.CreateVPC{TenantID: tenant, Name: key, CIDR: "10.42.0.0/16", IdempotencyKey: key})
			results <- err
		}(key)
	}
	wg.Wait()
	close(results)
	success, conflict := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if biz.ReasonOf(err) == biz.PlacementMismatch {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("success=%d conflicts=%d", success, conflict)
	}
	var count int
	var uid string
	if err = f.Owner.QueryRow(context.Background(), "SELECT count(*) FROM network_vpcs WHERE tenant_id=$1", tenant).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if err = f.Owner.QueryRow(context.Background(), "SELECT namespace_uid FROM network_tenant_namespaces WHERE tenant_id=$1 AND cluster_id='test-cluster'", tenant).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	if count != 1 || (uid != "uid-a" && uid != "uid-b") {
		t.Fatalf("count=%d uid=%s", count, uid)
	}
}
func TestTenantNamespaceBackfillIsExactAndNeverOverwritesUID(t *testing.T) {
	f := testenv.NewDatabase(t)
	repo := f.Repository
	ctx := context.Background()
	const a = "11111111-1111-4111-8111-111111111111"
	const b = "22222222-2222-4222-8222-222222222222"
	if _, err := f.Owner.Exec(ctx, "INSERT INTO network_tenant_namespaces(tenant_id,cluster_id,namespace,namespace_uid) VALUES($1,'test-cluster',$2,NULL),($3,'test-cluster',$4,'uid-b'),($1,'unrelated-cluster',$2,NULL)", a, "tenant-"+a, b, "tenant-"+b); err != nil {
		t.Fatal(err)
	}
	repo.ConfigureTenantNamespaceResolver(func(_ context.Context, tenant string) (tenantnamespace.Identity, error) {
		uid := "uid-a"
		if tenant == b {
			uid = "uid-b"
		}
		return tenantnamespace.Identity{TenantID: tenant, ClusterID: "test-cluster", Name: "tenant-" + tenant, UID: uid}, nil
	})
	if err := repo.ConfigureVPCCIDRPresets([]string{"10.42.0.0/16"}); err != nil {
		t.Fatal(err)
	}
	n, err := biz.NewNetwork(repo, []byte("0123456789abcdef0123456789abcdef"), time.Minute, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = n.CreateVPC(ctx, biz.CreateVPC{TenantID: a, Name: "must-refuse-null", CIDR: "10.42.0.0/16", IdempotencyKey: "null-uid"}); biz.ReasonOf(err) != biz.PlacementMismatch {
		t.Fatalf("NULL runtime identity accepted: %v", err)
	}
	plan, err := repo.PlanNamespaceUIDBackfill(ctx)
	if err != nil || plan.NullCount != 1 || len(plan.Rows) != 2 {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	if _, err = repo.ApplyNamespaceUIDBackfill(ctx, "unreviewed"); err == nil {
		t.Fatal("unreviewed plan accepted")
	}
	after, err := repo.ApplyNamespaceUIDBackfill(ctx, plan.SHA256)
	if err != nil || after.NullCount != 0 {
		t.Fatalf("after=%+v err=%v", after, err)
	}
	var unrelated *string
	if err = f.Owner.QueryRow(ctx, "SELECT namespace_uid FROM network_tenant_namespaces WHERE tenant_id=$1 AND cluster_id='unrelated-cluster'", a).Scan(&unrelated); err != nil || unrelated != nil {
		t.Fatalf("unrelated row changed: %v %v", unrelated, err)
	}
	var existing string
	if err = f.Owner.QueryRow(ctx, "SELECT namespace_uid FROM network_tenant_namespaces WHERE tenant_id=$1 AND cluster_id='test-cluster'", b).Scan(&existing); err != nil || existing != "uid-b" {
		t.Fatalf("existing UID changed: %s %v", existing, err)
	}
	repo.ConfigureTenantNamespaceResolver(func(_ context.Context, tenant string) (tenantnamespace.Identity, error) {
		return tenantnamespace.Identity{TenantID: tenant, ClusterID: "test-cluster", Name: "tenant-" + tenant, UID: "replacement"}, nil
	})
	if _, err = repo.PlanNamespaceUIDBackfill(ctx); err == nil {
		t.Fatal("existing original UID was replaceable")
	}
}
