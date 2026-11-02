//go:build sandboxintegration

package sandbox_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	pb "github.com/zhangzhe-ctrl/ani-resource-service/api/sandbox/v1"
	compute "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/compute"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/sandbox"
	data "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/sandbox"
	service "github.com/zhangzhe-ctrl/ani-resource-service/internal/service/sandbox"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type unusedSpaceCompute struct{}

func (unusedSpaceCompute) Resolve(context.Context, string, string, compute.Usage) (compute.Selection, error) {
	return compute.Selection{}, errors.New("not used")
}

type unusedSpaceNamespace struct{}

func (unusedSpaceNamespace) EnsureSandboxNamespaceUID(context.Context, string) (string, error) {
	return "", errors.New("not used")
}
func (unusedSpaceNamespace) ResolveSandboxNamespaceUID(context.Context, string) (string, error) {
	return "", errors.New("not used")
}

func tenantSpaceContext(tenant string) context.Context {
	return biz.WithCaller(context.Background(), biz.Caller{TenantID: tenant, Actor: biz.TenantSpaceActor})
}

func TestTenantSpaceServicePostgres(t *testing.T) {
	f := sandboxDatabase(t)
	handler, err := service.NewService(f.repo, unusedSpaceCompute{}, service.Config{ClusterID: uuid.NewString(), NamespacePrefix: "tenant-", NamespaceResolver: unusedSpaceNamespace{}, RuntimeClass: "kata", WorkspaceSize: "5Gi"})
	if err != nil {
		t.Fatal(err)
	}
	tenant, op := uuid.NewString(), uuid.NewString()
	request := &pb.OpenTenantSpaceRequest{TenantId: tenant, OperationId: op}
	user := biz.WithCaller(context.Background(), biz.Caller{TenantID: tenant, Actor: "governance:user:42"})
	if _, err := handler.OpenTenantSpace(user, request); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("ordinary user accepted: %v", err)
	}
	if _, err := handler.OpenTenantSpace(tenantSpaceContext(uuid.NewString()), request); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("other tenant accepted: %v", err)
	}
	ctx := tenantSpaceContext(tenant)
	opened, err := handler.OpenTenantSpace(ctx, request)
	if err != nil || opened.Space.GetPhase() != "open" || opened.Space.GetOperationId() != op {
		t.Fatalf("open: %v %v", opened, err)
	}
	closed, err := handler.CloseTenantSpace(ctx, &pb.CloseTenantSpaceRequest{TenantId: tenant, OperationId: op})
	if err != nil || closed.Space.GetPhase() != "closed" {
		t.Fatalf("close: %v %v", closed, err)
	}
	if _, err := handler.OpenTenantSpace(ctx, request); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("late open: %v", err)
	}
}

func TestTenantSpaceCloseFenceAndPendingReplayPostgres(t *testing.T) {
	f := sandboxDatabase(t)
	tenant, op, id := uuid.NewString(), uuid.NewString(), uuid.NewString()
	ctx := tenantSpaceContext(tenant)
	space, err := f.repo.OpenTenantSpace(ctx, tenant, op)
	if err != nil || space.Phase != "open" {
		t.Fatalf("open: %#v %v", space, err)
	}
	started, err := f.repo.BeginTenantSpaceMutation(ctx, tenant, op, id, "namespace_create")
	if err != nil || !started.Dispatch || started.Outcome != "pending" {
		t.Fatalf("begin: %#v %v", started, err)
	}
	replay, err := f.repo.BeginTenantSpaceMutation(ctx, tenant, op, id, "namespace_create")
	if err != nil || replay.Dispatch || replay.Outcome != "pending" {
		t.Fatalf("pending replay: %#v %v", replay, err)
	}
	if _, err = f.repo.BeginTenantSpaceMutation(ctx, tenant, op, id, "namespace_marker"); !errors.Is(err, biz.ErrConflict) {
		t.Fatalf("changed mutation kind: %v", err)
	}
	space, err = f.repo.CloseTenantSpace(ctx, tenant, op)
	if err != nil || space.Phase != "closing" || space.PendingID != id {
		t.Fatalf("close with pending write: %#v %v", space, err)
	}
	if _, err = f.repo.BeginTenantSpaceMutation(ctx, tenant, op, uuid.NewString(), "namespace_marker"); !errors.Is(err, biz.ErrConflict) {
		t.Fatalf("closing accepted another mutation: %v", err)
	}
	if _, err = f.repo.FinishTenantSpaceMutation(ctx, tenant, op, id, "unknown", "uncertain"); !errors.Is(err, biz.ErrInvalid) {
		t.Fatalf("unknown cleared fence: %v", err)
	}
	space, err = f.repo.GetTenantSpace(ctx, tenant)
	if err != nil || space.Phase != "closing" || space.PendingID != id {
		t.Fatalf("unknown was not retained: %#v %v", space, err)
	}
	restarted, err := data.OpenPostgres(ctx, f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	replay, err = restarted.BeginTenantSpaceMutation(ctx, tenant, op, id, "namespace_create")
	if err != nil || replay.Dispatch || replay.Outcome != "pending" {
		t.Fatalf("pending after restart: %#v %v", replay, err)
	}
	finished, err := restarted.FinishTenantSpaceMutation(ctx, tenant, op, id, "applied", "namespace-uid-1")
	if err != nil || finished.Outcome != "applied" {
		t.Fatalf("finish: %#v %v", finished, err)
	}
	space, err = f.repo.GetTenantSpace(ctx, tenant)
	if err != nil || space.Phase != "closed" || space.PendingID != "" || space.NamespaceUID != "namespace-uid-1" {
		t.Fatalf("finish did not close fence: %#v %v", space, err)
	}
	if _, err = f.repo.PinTenantSpaceNamespace(ctx, tenant, op, "namespace-uid-1"); !errors.Is(err, biz.ErrConflict) {
		t.Fatalf("closed space allowed pin: %v", err)
	}
	replay, err = f.repo.BeginTenantSpaceMutation(ctx, tenant, op, id, "namespace_create")
	if err != nil || replay.Dispatch || replay.Outcome != "applied" || replay.Receipt != "namespace-uid-1" {
		t.Fatalf("completed replay: %#v %v", replay, err)
	}
	if _, err = f.repo.FinishTenantSpaceMutation(ctx, tenant, op, id, "applied", "different"); !errors.Is(err, biz.ErrConflict) {
		t.Fatalf("changed receipt accepted: %v", err)
	}
	if _, err = f.repo.OpenTenantSpace(ctx, tenant, op); !errors.Is(err, biz.ErrConflict) {
		t.Fatalf("closed space reopened: %v", err)
	}
	if _, err = f.repo.GetTenantSpace(tenantSpaceContext(uuid.NewString()), tenant); !errors.Is(err, biz.ErrDenied) {
		t.Fatalf("cross-tenant read: %v", err)
	}
}

func TestTenantSpaceCloseBeforeOpenAndMutationHistoryPostgres(t *testing.T) {
	f := sandboxDatabase(t)
	tenant, op := uuid.NewString(), uuid.NewString()
	ctx := tenantSpaceContext(tenant)
	space, err := f.repo.CloseTenantSpace(ctx, tenant, op)
	if err != nil || space.Phase != "closed" {
		t.Fatalf("close first: %#v %v", space, err)
	}
	if _, err = f.repo.OpenTenantSpace(ctx, tenant, op); !errors.Is(err, biz.ErrConflict) {
		t.Fatalf("late open accepted: %v", err)
	}
	if _, err = f.repo.OpenTenantSpace(ctx, tenant, uuid.NewString()); !errors.Is(err, biz.ErrConflict) {
		t.Fatalf("different open accepted: %v", err)
	}

	tenant, op = uuid.NewString(), uuid.NewString()
	ctx = tenantSpaceContext(tenant)
	if _, err = f.repo.OpenTenantSpace(ctx, tenant, op); err != nil {
		t.Fatal(err)
	}
	if _, err = f.repo.PinTenantSpaceNamespace(ctx, tenant, op, "namespace-uid-existing"); err != nil {
		t.Fatal(err)
	}
	if _, err = f.repo.PinTenantSpaceNamespace(ctx, tenant, op, "namespace-uid-replacement"); !errors.Is(err, biz.ErrOwnership) {
		t.Fatalf("replacement namespace accepted: %v", err)
	}
	first, second := uuid.NewString(), uuid.NewString()
	for _, id := range []string{first, second} {
		if m, err := f.repo.BeginTenantSpaceMutation(ctx, tenant, op, id, "namespace_marker"); err != nil || !m.Dispatch {
			t.Fatalf("begin %s: %#v %v", id, m, err)
		}
		if _, err := f.repo.FinishTenantSpaceMutation(ctx, tenant, op, id, "not_sent", "rejected-before-send"); err != nil {
			t.Fatal(err)
		}
	}
	old, err := f.repo.BeginTenantSpaceMutation(ctx, tenant, op, first, "namespace_marker")
	if err != nil || old.Dispatch || old.Outcome != "not_sent" {
		t.Fatalf("older mutation replayed as new: %#v %v", old, err)
	}
}

func TestTenantSpaceCloseRacesProviderWritePostgres(t *testing.T) {
	f := sandboxDatabase(t)
	for i := 0; i < 20; i++ {
		tenant, op, id := uuid.NewString(), uuid.NewString(), uuid.NewString()
		ctx := tenantSpaceContext(tenant)
		if _, err := f.repo.OpenTenantSpace(ctx, tenant, op); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		var mutation biz.TenantSpaceMutation
		var closed biz.TenantSpace
		var beginErr, closeErr error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			mutation, beginErr = f.repo.BeginTenantSpaceMutation(ctx, tenant, op, id, "namespace_create")
		}()
		go func() { defer wg.Done(); <-start; closed, closeErr = f.repo.CloseTenantSpace(ctx, tenant, op) }()
		close(start)
		wg.Wait()
		if closeErr != nil {
			t.Fatal(closeErr)
		}
		if beginErr == nil {
			if !mutation.Dispatch || closed.Phase != "closing" || closed.PendingID != id {
				t.Fatalf("write won without closing fence: %#v %#v", mutation, closed)
			}
			if _, err := f.repo.FinishTenantSpaceMutation(ctx, tenant, op, id, "not_sent", "not-sent"); err != nil {
				t.Fatal(err)
			}
		} else if !errors.Is(beginErr, biz.ErrConflict) || closed.Phase != "closed" {
			t.Fatalf("close won but write was not rejected: %#v %v", closed, beginErr)
		}
		final, err := f.repo.GetTenantSpace(ctx, tenant)
		if err != nil || final.Phase != "closed" || final.PendingID != "" {
			t.Fatalf("final: %#v %v", final, err)
		}
	}
}

func TestTenantSpaceOpenCloseRacePostgres(t *testing.T) {
	f := sandboxDatabase(t)
	for i := 0; i < 20; i++ {
		tenant, op := uuid.NewString(), uuid.NewString()
		ctx := tenantSpaceContext(tenant)
		start := make(chan struct{})
		var openErr, closeErr error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); <-start; _, openErr = f.repo.OpenTenantSpace(ctx, tenant, op) }()
		go func() { defer wg.Done(); <-start; _, closeErr = f.repo.CloseTenantSpace(ctx, tenant, op) }()
		close(start)
		wg.Wait()
		if closeErr != nil || (openErr != nil && !errors.Is(openErr, biz.ErrConflict)) {
			t.Fatalf("open=%v close=%v", openErr, closeErr)
		}
		state, err := f.repo.GetTenantSpace(ctx, tenant)
		if err != nil || state.Phase != "closed" || state.PendingID != "" {
			t.Fatalf("race left open tenant space: %#v %v", state, err)
		}
	}
}
