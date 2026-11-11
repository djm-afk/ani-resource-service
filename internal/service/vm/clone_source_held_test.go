package vm

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// holdingRepo answers only the clone reference port. It embeds the repository
// interface so the resolve precheck can be exercised without a database; any
// other method stays unimplemented and would panic, proving the helper never
// reaches them.
type holdingRepo struct {
	biz.Repository
	held bool
	err  error
}

func (holdingRepo) ReleaseCloneSourceHELD(context.Context, biz.CloneSourceRelease) error {
	return nil
}

func (h holdingRepo) SourceHeld(context.Context, string, string) (bool, error) {
	return h.held, h.err
}

// plainRepo predates the clone reference port, so resolve must keep working.
type plainRepo struct{ biz.Repository }

// TestCloneSourceHeldReadsTheReferencePort covers the resolve precheck: a held
// source is reported, a free one is not, a port error is propagated, and a
// repository without the port is treated as free rather than failing resolve.
func TestCloneSourceHeldReadsTheReferencePort(t *testing.T) {
	tenant, source := uuid.NewString(), uuid.NewString()
	held, err := cloneSourceHeld(context.Background(), holdingRepo{held: true}, tenant, source)
	if err != nil || !held {
		t.Fatalf("held source = %v %v, want true nil", held, err)
	}
	held, err = cloneSourceHeld(context.Background(), holdingRepo{held: false}, tenant, source)
	if err != nil || held {
		t.Fatalf("free source = %v %v, want false nil", held, err)
	}
	if _, err = cloneSourceHeld(context.Background(), plainRepo{}, tenant, source); err != nil {
		t.Fatalf("repository without the port must resolve: %v", err)
	}
	if _, err = cloneSourceHeld(context.Background(), holdingRepo{err: biz.ErrDependency}, tenant, source); !errors.Is(err, biz.ErrDependency) {
		t.Fatalf("port error = %v, want ErrDependency", err)
	}
}

// TestBusySourceRPCCode pins the wire contract the Governor relies on to answer
// a second clone of a busy source with 409 instead of accepting it and leaving
// a target that never lands.
func TestBusySourceRPCCode(t *testing.T) {
	err := rpcError(biz.ErrBusy)
	if status.Code(err) != codes.FailedPrecondition || status.Convert(err).Message() != "VM_OPERATION_CONFLICT" {
		t.Fatalf("busy mapping = %v %q", status.Code(err), status.Convert(err).Message())
	}
}
