package instance

import (
	"context"
	"strings"
	"testing"

	compute "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/compute"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/instance"
)

// 候选占用标记的最小替身：只替换 repo 的 guard/引用读取边界。
// 候选展示与实际挂载共用同一规则；受理期权威校验在 Postgres 事务内另行覆盖。
type candidatesRepo struct {
	biz.Repository
	compute.PresetRepository
	deleting map[string]bool
	refs     []biz.VolumeReference
}

func (r *candidatesRepo) PVCDeleting(_ context.Context, _, _, _, name string) (bool, error) {
	return r.deleting[name], nil
}

func (r *candidatesRepo) VolumeReferences(context.Context, string, string, string) ([]biz.VolumeReference, error) {
	return r.refs, nil
}

func newCandidatesService(t *testing.T, repo *candidatesRepo) *Service {
	return newCandidatesServiceFor(t, repo)
}

// candidatesRepoLike 与 cpuIntake 相同：biz.Repository + compute.PresetRepository。
type candidatesRepoLike interface {
	biz.Repository
	compute.PresetRepository
}

func newCandidatesServiceFor(t *testing.T, repo candidatesRepoLike) *Service {
	t.Helper()
	images, environment := &cpuImageBoundary{}, &cpuEnvironmentBoundary{}
	s, e := NewService(repo, newTestComputeResolver(t, repo, "22222222-2222-4222-8222-222222222222"), images, environment, Config{NewIntentsEnabled: true, NamespacePrefix: "tenant-", ClusterID: "22222222-2222-4222-8222-222222222222", ServiceAccount: "workload", StorageClass: "ani-block", WorkspaceSize: "1Gi", Shell: "/bin/bash", UID: 1000, GID: 1000})
	if e != nil {
		t.Fatal(e)
	}
	return s
}

func TestMarkMountablePVCsMarksOccupiedAndDeleting(t *testing.T) {
	const tenant = "11111111-1111-4111-8111-111111111111"
	const cluster = "22222222-2222-4222-8222-222222222222"
	repo := &candidatesRepo{
		deleting: map[string]bool{"deleting-vol": true},
		refs: []biz.VolumeReference{
			{PVCName: "mounted", PVCUID: "uid-mounted", State: "active"},
			{PVCName: "preparing", PVCUID: "uid-preparing", State: "pending"},
			{PVCName: "retiring", PVCUID: "uid-retiring", State: "retiring"},
		},
	}
	s := newCandidatesService(t, repo)
	in := []biz.MountablePVC{
		{PVCName: "free", PVCUID: "uid-free", Selectable: true},
		{PVCName: "mounted", PVCUID: "uid-mounted", Selectable: true},
		{PVCName: "preparing", PVCUID: "uid-preparing", Selectable: true},
		{PVCName: "retiring", PVCUID: "uid-retiring", Selectable: true},
		{PVCName: "deleting-vol", PVCUID: "uid-deleting", Selectable: true},
		// 同名不同 UID（删除后重建）不算占用。
		{PVCName: "mounted", PVCUID: "uid-rebuilt", Selectable: true},
		// 已有原因的项不被占用标记覆盖。
		{PVCName: "blocked", PVCUID: "uid-block", Selectable: false, UnavailableReason: "PVC_FILESYSTEM_REQUIRED"},
	}
	out, err := s.markMountablePVCs(context.Background(), tenant, cluster, "tenant-"+tenant, in)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		name       string
		selectable bool
		reason     string
	}{
		{"free", true, ""},
		{"mounted", false, "PVC_IN_USE"},
		{"preparing", false, "PVC_IN_USE"},
		{"retiring", false, "PVC_IN_USE"},
		{"deleting-vol", false, "PVC_DELETION_IN_PROGRESS"},
		{"mounted", true, ""},
		{"blocked", false, "PVC_FILESYSTEM_REQUIRED"},
	}
	if len(out) != len(want) {
		t.Fatalf("candidate count %d != %d", len(out), len(want))
	}
	for i, w := range want {
		if out[i].PVCName != w.name || out[i].Selectable != w.selectable || out[i].UnavailableReason != w.reason {
			t.Fatalf("candidate[%d] = %s selectable=%v reason=%q, want %s selectable=%v reason=%q", i, out[i].PVCName, out[i].Selectable, out[i].UnavailableReason, w.name, w.selectable, w.reason)
		}
	}
}

func TestMarkMountablePVCsRequiresGuardReader(t *testing.T) {
	const tenant = "11111111-1111-4111-8111-111111111111"
	repo := &cpuIntake{}
	s := newCandidatesServiceFor(t, repo)
	if _, err := s.markMountablePVCs(context.Background(), tenant, "22222222-2222-4222-8222-222222222222", "tenant-"+tenant, []biz.MountablePVC{{PVCName: "x", Selectable: true}}); err == nil || !strings.Contains(err.Error(), "INSTANCE_DEPENDENCY_UNAVAILABLE") {
		t.Fatal("missing guard reader must fail closed", err)
	}
}
