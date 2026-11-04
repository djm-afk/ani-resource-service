package service

import (
	"context"
	"testing"
	"time"

	networkv1 "github.com/zhangzhe-ctrl/ani-resource-service/api/network/v1"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Only the operation read is supplied. Embedded, unused ports deliberately have
// no implementation: a query unexpectedly taking a write/Provider path fails.
type platformOperationRepositoryStub struct {
	biz.EgressRepository
	operation   biz.Operation
	requestedID string
	reads       int
}

func (r *platformOperationRepositoryStub) GetPlatformOperation(_ context.Context, id string) (biz.Operation, error) {
	r.requestedID = id
	r.reads++
	return r.operation, nil
}

type unusedOperationInfrastructure struct {
	biz.IntranetPoolDefaultsInfrastructure
}

func TestGetPlatformOperationPreservesPlatformKinds(t *testing.T) {
	for _, tc := range []struct {
		kind string
		want networkv1.OperationKind
	}{
		{"create_intranet_pool", networkv1.OperationKind_OPERATION_KIND_CREATE_INTRANET_POOL},
		{"delete_intranet_pool", networkv1.OperationKind_OPERATION_KIND_DELETE_INTRANET_POOL},
		{"set_intranet_pool_allocation", networkv1.OperationKind_OPERATION_KIND_SET_INTRANET_POOL_ALLOCATION},
		{"set_default_intranet_pool", networkv1.OperationKind_OPERATION_KIND_SET_DEFAULT_INTRANET_POOL},
		{"verify_intranet_pool", networkv1.OperationKind_OPERATION_KIND_VERIFY_INTRANET_POOL},
		{"create_virtual_switch", networkv1.OperationKind_OPERATION_KIND_CREATE_VIRTUAL_SWITCH},
		{"delete_virtual_switch", networkv1.OperationKind_OPERATION_KIND_DELETE_VIRTUAL_SWITCH},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			now := time.Date(2026, time.September, 14, 12, 0, 0, 0, time.UTC)
			repository := &platformOperationRepositoryStub{operation: biz.Operation{
				ID: "16aec409-b221-417f-b056-d505b679ae11", ResourceID: "pool_11111111111111111111111111111111",
				Kind: tc.kind, State: biz.Succeeded, CreatedAt: now, UpdatedAt: now, CompletedAt: &now,
			}}
			egress, err := biz.NewEgress(repository, unusedOperationInfrastructure{}, biz.ContextEgressAuthorization{}, []byte("0123456789abcdef0123456789abcdef"), func() time.Time { return now })
			if err != nil {
				t.Fatal(err)
			}
			service := NewPlatformNetworkService(egress)
			request := &networkv1.GetPlatformOperationRequest{OperationId: repository.operation.ID}
			if _, err := service.GetPlatformOperation(context.Background(), request); status.Code(err) != codes.PermissionDenied || repository.reads != 0 {
				t.Fatal("untrusted operation query reached repository", err, repository.reads)
			}
			ctx := biz.WithEgressCaller(context.Background(), biz.EgressCaller{PlatformAdministrator: true})
			response, err := service.GetPlatformOperation(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			operation := response.GetOperation()
			if operation.GetKind() != tc.want || operation.GetKind() == networkv1.OperationKind_OPERATION_KIND_UNSPECIFIED {
				t.Fatalf("platform operation kind %q projected as %v; want %v", tc.kind, operation.GetKind(), tc.want)
			}
			if repository.reads != 1 || repository.requestedID != request.GetOperationId() || operation.GetId() != request.GetOperationId() || operation.GetResourceId() != repository.operation.ResourceID || operation.GetState() != networkv1.OperationState_OPERATION_STATE_SUCCEEDED || operation.GetCompletedAt() == nil || !operation.GetCompletedAt().AsTime().Equal(now) {
				t.Fatal("platform operation query changed durable result", response, repository.reads, repository.requestedID)
			}
		})
	}
}
