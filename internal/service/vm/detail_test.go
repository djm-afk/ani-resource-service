package vm

import (
	"context"
	"testing"

	"github.com/google/uuid"
	pb "github.com/zhangzhe-ctrl/ani-resource-service/api/vm/v1"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type detailTestNative struct {
	biz.NativePort
	calls int
	err   error
}

func (n *detailTestNative) ReadRuntime(context.Context, biz.VM) (biz.Runtime, error) {
	n.calls++
	return biz.Runtime{State: "AVAILABLE", AgentState: "CONNECTED", Hostname: "guest-a"}, n.err
}

type detailTestRepo struct{ snapshotServiceRepo }

func (r *detailTestRepo) List(context.Context, string, biz.ListFilter) ([]biz.VM, int64, error) {
	return []biz.VM{r.current}, 1, nil
}

type countingRelations struct {
	biz.RelationsPort
	calls int
}

func (r *countingRelations) ReadRelations(context.Context, biz.VM) biz.Relations {
	r.calls++
	return biz.Relations{}
}

func TestDetailRuntimeIsTenantBoundAndNeverReadByList(t *testing.T) {
	s, ctx, request := dnsServiceFixture(t)
	resolved, err := s.ResolveCreateSpec(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	spec, err := domainSpec(id, resolved.CreateSpec)
	if err != nil {
		t.Fatal(err)
	}
	r := &detailTestRepo{snapshotServiceRepo: snapshotServiceRepo{current: biz.VM{ID: id, TenantID: request.TenantId, Name: "VM", Spec: spec, Native: biz.ObjectRef{UID: "vm-uid"}}}}
	n := &detailTestNative{}
	s.repo, s.native = r, n
	out, err := s.GetVM(ctx, &pb.GetVMRequest{TenantId: request.TenantId, VmId: id})
	if err != nil || out.Runtime.Hostname != "guest-a" || n.calls != 1 {
		t.Fatalf("detail not wired: %v %+v", err, out)
	}
	_, err = s.ListVMs(ctx, &pb.ListVMsRequest{TenantId: request.TenantId})
	if err != nil || n.calls != 1 {
		t.Fatalf("list runtime fanout: %v calls=%d", err, n.calls)
	}
	_, err = s.GetVM(ctx, &pb.GetVMRequest{TenantId: uuid.NewString(), VmId: id})
	if status.Code(err) != codes.PermissionDenied || n.calls != 1 {
		t.Fatalf("tenant scope bypass: %v", err)
	}
	n.err = biz.ErrDependency
	out, err = s.GetVM(ctx, &pb.GetVMRequest{TenantId: request.TenantId, VmId: id})
	if err != nil || out.Runtime.State != "UNAVAILABLE" || out.Runtime.Hostname != "" || out.Id != id {
		t.Fatalf("dependency hid detail or leaked unverified guest: %v %+v", err, out)
	}
	skip := false
	n.calls = 0
	out, err = s.GetVM(ctx, &pb.GetVMRequest{TenantId: request.TenantId, VmId: id, IncludeRuntime: &skip})
	if err != nil || n.calls != 0 || out.Runtime.State != "NOT_QUERIED" || out.Runtime.Hostname != "" {
		t.Fatalf("explicit include_runtime=false still read runtime: %v %+v calls=%d", err, out, n.calls)
	}
}

func TestListDisksIsAPersistedProjectionOnly(t *testing.T) {
	s, ctx, request := dnsServiceFixture(t)
	resolved, err := s.ResolveCreateSpec(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	spec, err := domainSpec(id, resolved.CreateSpec)
	if err != nil {
		t.Fatal(err)
	}
	v := biz.VM{ID: id, TenantID: request.TenantId, Name: "VM", Spec: spec, Native: biz.ObjectRef{UID: "vm-uid"}}
	d := biz.NewInitialDisk(id, uuid.NewString(), "data", "data", 1)
	d.DV.Namespace = v.Spec.Namespace
	v.Disks = []biz.Disk{d, {ID: uuid.NewString(), Role: "data", Tombstone: true}}
	r := &detailTestRepo{snapshotServiceRepo: snapshotServiceRepo{current: v}}
	n := &detailTestNative{}
	rel := &countingRelations{}
	s.repo, s.native, s.relations = r, n, rel
	out, err := s.ListDisks(ctx, &pb.GetVMRequest{TenantId: request.TenantId, VmId: id})
	if err != nil || len(out.Items) != 1 || out.Items[0].DiskId != d.ID || out.Items[0].Name != "data" {
		t.Fatalf("disk list projection: %v %+v", err, out)
	}
	if n.calls != 0 || rel.calls != 0 {
		t.Fatalf("disk list paid detail work: runtime=%d relations=%d", n.calls, rel.calls)
	}
}
