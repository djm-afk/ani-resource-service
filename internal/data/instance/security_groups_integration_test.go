//go:build instanceintegration

package instance_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/google/uuid"
	pb "github.com/zhangzhe-ctrl/ani-resource-service/api/instance/v1"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/instance"
)

type securityGroupRuntimeBoundary struct {
	biz.Provider
	names []string
}

func (b *securityGroupRuntimeBoundary) SetSecurityGroups(_ context.Context, _ biz.Instance, names []string) error {
	b.names = append([]string{}, names...)
	return nil
}

type securityGroupReferenceBoundary struct{ confirms int }

func (*securityGroupReferenceBoundary) ResolveSecurityGroups(context.Context, string, string, string, []string) ([]string, error) {
	return []string{"sg-selected"}, nil
}
func (*securityGroupReferenceBoundary) SyncSecurityGroupBinding(context.Context, string, string, string, string, string, int64, []string) ([]string, error) {
	return []string{"sg-selected"}, nil
}
func (b *securityGroupReferenceBoundary) ConfirmSecurityGroupBinding(context.Context, string, string, string, int64, []string) error {
	b.confirms++
	return nil
}
func (*securityGroupReferenceBoundary) ReleaseSecurityGroupBinding(context.Context, string, string, string, int64) error {
	return nil
}

// This test verifies the actual Instance admission/lease/operation/current JSON
// transactions; SG catalog and Kubernetes are the two substituted boundaries.
func TestSecurityGroupsPersistentCommandReplayAndImmutableCreate(t *testing.T) {
	f := database(t)
	c := cpuCreate()
	ctx := biz.WithCaller(context.Background(), biz.Caller{TenantID: c.TenantID, Actor: "security-group-persistence-test"})
	old := "sg_11111111111111111111111111111111"
	selected := "sg_22222222222222222222222222222222"
	c.Spec.SecurityGroupIDs = []string{old}
	c.Spec.Network = &biz.NetworkSelection{VPCID: "vpc_11111111111111111111111111111111", SubnetID: "subnet_11111111111111111111111111111111"}
	if _, err := f.repo.AcceptCreate(ctx, c); err != nil {
		t.Fatal(err)
	}
	// Complete only the fixture's prior operation, so this test isolates the new
	// command's transaction; CREATE runtime has its separate vertical tests.
	if _, err := f.runtime.Exec(ctx, "UPDATE instance.instance_operations SET state='completed' WHERE tenant_id=$1 AND operation_id=$2", c.TenantID, c.OperationID); err != nil {
		t.Fatal(err)
	}
	raw, err := pb.CanonicalSecurityGroupIntent(c.ID, 1, []string{selected})
	if err != nil {
		t.Fatal(err)
	}
	cmd := biz.Command{TenantID: c.TenantID, ID: c.ID, OperationID: uuid.NewString(), Key: uuid.NewString(), Action: "set-security-groups", ExpectedVersion: 1, CanonicalIntent: raw, RequestHash: pb.CommandIntentDigest(raw), SecurityGroupIDs: []string{selected}}
	ack, err := f.repo.AcceptCommand(ctx, cmd)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := f.repo.AcceptCommand(ctx, cmd)
	if err != nil || !replay.Replayed || replay.OperationID != ack.OperationID {
		t.Fatal("same key replay changed", replay, err)
	}
	changed := cmd
	changed.RequestHash = pb.CommandIntentDigest([]byte("other"))
	if _, err = f.repo.AcceptCommand(ctx, changed); !errors.Is(err, biz.ErrConflict) {
		t.Fatal("changed intent replay accepted", err)
	}
	if _, err = f.repo.Get(ctx, uuid.NewString(), c.ID); !errors.Is(err, biz.ErrNotFound) {
		t.Fatal("cross tenant read", err)
	}
	accepted, err := f.repo.GetOperation(ctx, c.TenantID, ack.OperationID)
	if err != nil || !reflect.DeepEqual(accepted.Execution.SecurityGroupIDs, []string{selected}) {
		t.Fatal("target intent not durable", accepted, err)
	}
	runtime := &securityGroupRuntimeBoundary{}
	refs := &securityGroupReferenceBoundary{}
	worker, _ := biz.NewWorker(f.repo, runtime, "sg-persistence", discardLogger())
	_ = worker.UseSecurityGroups(refs)
	got := step(t, f, worker, c)
	if !reflect.DeepEqual(got.SecurityGroupIDs, []string{selected}) || !reflect.DeepEqual(got.Spec.SecurityGroupIDs, []string{old}) || got.Version != 2 || !got.SecurityGroupsInitialized || got.Pending != nil || refs.confirms != 1 {
		t.Fatal("current selection not atomically persisted", got)
	}
	terminal, err := f.repo.GetOperation(ctx, c.TenantID, ack.OperationID)
	if err != nil || terminal.State != "completed" || terminal.Execution.Phase != "done" {
		t.Fatal(terminal, err)
	}
	again, err := f.repo.LookupCommandReplay(ctx, cmd)
	if err != nil || again.OperationID != ack.OperationID || !again.Replayed {
		t.Fatal("terminal replay changed", again, err)
	}
}
