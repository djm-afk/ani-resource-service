package vm

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	pb "github.com/zhangzhe-ctrl/ani-resource-service/api/vm/v1"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type storedSpecQueryRepo struct {
	snapshotServiceRepo
	items []biz.VM
}

func (r *storedSpecQueryRepo) List(context.Context, string, biz.ListFilter) ([]biz.VM, int64, error) {
	return r.items, int64(len(r.items)), nil
}

func TestStoredSpecQueryCompatibilityDoesNotPoisonListOrChangeAdmission(t *testing.T) {
	s, ctx, request := dnsServiceFixture(t)
	resolved, err := s.ResolveCreateSpec(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	original, err := domainSpec(uuid.NewString(), resolved.CreateSpec)
	if err != nil {
		t.Fatal(err)
	}
	var future map[string]any
	if err = json.Unmarshal(original.Canonical, &future); err != nil {
		t.Fatal(err)
	}
	intent := future["intent"].(map[string]any)
	intent["source_kind"] = "VM_CLONE"
	intent["expected_source_version"] = "7"
	future["clone_source"] = map[string]any{"source_vm_id": uuid.NewString(), "source_version": "7", "disks": []any{}}
	raw, err := json.Marshal(future)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pb.DecodeCreateSpec(raw); err == nil {
		t.Fatal("stored projection must not relax strict command decoder")
	}
	cloneSpec := original
	cloneSpec.Canonical = append([]byte(nil), raw...)
	clone := biz.VM{TenantID: request.TenantId, ID: uuid.NewString(), Name: "old-clone", Spec: cloneSpec, Version: 7, Observed: "Stopped", Desired: "Stopped"}
	normal := biz.VM{TenantID: request.TenantId, ID: uuid.NewString(), Name: "normal", Spec: original, Version: 1}
	bad := clone
	bad.ID, bad.Name = uuid.NewString(), "unreadable"
	bad.Spec.Canonical = []byte("{")
	r := &storedSpecQueryRepo{snapshotServiceRepo: snapshotServiceRepo{current: clone}, items: []biz.VM{clone, normal, bad}}
	s.repo = r
	out, err := s.ListVMs(ctx, &pb.ListVMsRequest{TenantId: request.TenantId})
	if err != nil {
		t.Fatalf("one stored spec broke entire page: %v", err)
	}
	if out.Total != 3 || len(out.Items) != 3 {
		t.Fatalf("rows hidden or count changed: %+v", out)
	}
	if got := out.Items[0]; got.Id != clone.ID || got.Version != 7 || got.ObservedState != "Stopped" || got.FrozenSpec.GetIntent().GetSourceKind() != "VM_CLONE" || got.Reason != "VM_STORED_SPEC_COMPATIBILITY" {
		t.Fatalf("clone projection: %+v", got)
	}
	if got := out.Items[1]; got.Id != normal.ID || got.Reason != "" || got.FrozenSpec == nil {
		t.Fatalf("normal projection changed: %+v", got)
	}
	if got := out.Items[2]; got.Id != bad.ID || got.FrozenSpec != nil || got.Reason != "VM_STORED_SPEC_UNREADABLE" {
		t.Fatalf("bad spec lost metadata/diagnosis: %+v", got)
	}
	if !bytes.Equal(r.current.Spec.Canonical, raw) || !bytes.Equal(r.items[0].Spec.Canonical, raw) {
		t.Fatal("query changed accepted canonical bytes")
	}
	skip := false
	detail, err := s.GetVM(ctx, &pb.GetVMRequest{TenantId: request.TenantId, VmId: clone.ID, IncludeRuntime: &skip})
	if err != nil || detail.Id != clone.ID || detail.FrozenSpec.GetIntent().GetSourceKind() != "VM_CLONE" {
		t.Fatalf("clone detail unavailable: %v %+v", err, detail)
	}
	if _, err = s.GetVM(ctx, &pb.GetVMRequest{TenantId: uuid.NewString(), VmId: clone.ID}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("query bypassed tenant guard: %v", err)
	}
	request.Data.SourceKind = "VM_CLONE"
	if _, err = s.ResolveCreateSpec(ctx, request); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("read compatibility admitted unsupported new clone: %v", err)
	}
}

func TestStoredSpecQueryKeepsExistingReasonAndRejectsWrongKnownFieldTypes(t *testing.T) {
	s, ctx, request := dnsServiceFixture(t)
	resolved, err := s.ResolveCreateSpec(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := domainSpec(uuid.NewString(), resolved.CreateSpec)
	if err != nil {
		t.Fatal(err)
	}
	var future map[string]any
	if err = json.Unmarshal(spec.Canonical, &future); err != nil {
		t.Fatal(err)
	}
	future["future_field"] = true
	spec.Canonical, _ = json.Marshal(future)
	v := biz.VM{ID: uuid.NewString(), TenantID: request.TenantId, Spec: spec, Reason: "EXISTING_NATIVE_REASON"}
	out, err := wireVM(v)
	if err != nil || out.Reason != v.Reason || out.FrozenSpec == nil {
		t.Fatalf("native reason lost: %v %+v", err, out)
	}
	future["cores"] = map[string]any{"bad": "type"}
	v.Spec.Canonical, _ = json.Marshal(future)
	out, err = wireVM(v)
	if err != nil || out.FrozenSpec != nil || out.Reason != v.Reason {
		t.Fatalf("type mismatch fabricated a decoded spec or hid metadata: %v %+v", err, out)
	}
}
