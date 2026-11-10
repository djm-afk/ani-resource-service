//go:build instanceintegration

package vm

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	"github.com/zhangzhe-ctrl/ani-resource-service/tests/testenv"
)

// Only native systems are replaced. Admission, leases, checkpoints and state
// persistence use the actual repository and PostgreSQL runtime role.
type preparingVMNative struct {
	biz.NativePort
	calls  int
	result error
}

func (n *preparingVMNative) ValidateSource(context.Context, biz.Source) error { return nil }
func (n *preparingVMNative) EnsureVM(context.Context, biz.VM) (biz.ObjectRef, error) {
	n.calls++
	return biz.ObjectRef{}, n.result
}

type preparingVMNetwork struct{ biz.NetworkPort }

func (n preparingVMNetwork) Prepare(context.Context, biz.VM) (biz.NetworkPlan, error) {
	return biz.NetworkPlan{AttachmentID: uuid.NewString()}, nil
}

func TestVMWorkerPersistsNativeFailureWithoutStoppingService(t *testing.T) {
	for _, tc := range []struct {
		reason error
		state  string
	}{{biz.ErrDependency, "running"}, {biz.ErrChanged, "failed"}} {
		t.Run(tc.state, func(t *testing.T) {
			ctx := context.Background()
			db := testenv.NewDatabase(t)
			if e := Migrate(ctx, db.Owner.Config().ConnString(), db.RuntimeRole); e != nil {
				t.Fatal(e)
			}
			p, e := OpenPostgres(ctx, db.RuntimeDSN)
			if e != nil {
				t.Fatal(e)
			}
			defer p.Close()
			tenant, id, op := uuid.NewString(), uuid.NewString(), uuid.NewString()
			_, e = p.AcceptCreate(ctx, biz.Acceptance{TenantID: tenant, VMID: id, OperationID: op, RequestDigest: stringDigest('a'), Spec: biz.Spec{Canonical: []byte(`{"worker":"native-boundary"}`), Digest: stringDigest('b'), Name: "worker"}})
			if e != nil {
				t.Fatal(e)
			}
			native := &preparingVMNative{result: tc.reason}
			w, e := biz.NewWorker(p, native, preparingVMNetwork{}, discardLoggerVMLane())
			if e != nil {
				t.Fatal(e)
			}
			deadline := time.Now().Add(4 * time.Second)
			for native.calls == 0 && time.Now().Before(deadline) {
				if _, e = w.Step(ctx); e != nil {
					t.Fatalf("persisted native outcome terminated supervisor: %v", e)
				}
				if native.calls == 0 {
					time.Sleep(50 * time.Millisecond)
				}
			}
			saved, e := p.GetOperation(ctx, tenant, id, op)
			if e != nil || native.calls != 1 || saved.State != tc.state || saved.Reason != tc.reason.Error() {
				t.Fatalf("operation outcome missing: %+v calls=%d err=%v", saved, native.calls, e)
			}
		})
	}
}
