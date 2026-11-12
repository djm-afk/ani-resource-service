package sandbox

import (
	"context"

	pb "github.com/zhangzhe-ctrl/ani-resource-service/api/sandbox/v1"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/sandbox"
)

func tenantSpaceProto(space biz.TenantSpace) *pb.TenantSpace {
	return &pb.TenantSpace{TenantId: space.TenantID, OperationId: space.OperationID, Phase: space.Phase, PendingId: space.PendingID, PendingKind: space.PendingKind}
}

func (s *Service) tenantSpaceJournal(ctx context.Context, tenant string) (biz.TenantSpaceJournal, error) {
	if err := biz.RequireTenantSpaceController(ctx, tenant); err != nil {
		return nil, rpcErrorCtx(ctx, err)
	}
	journal, ok := s.repo.(biz.TenantSpaceJournal)
	if !ok {
		return nil, rpcErrorCtx(ctx, biz.ErrDependencyUnavailable)
	}
	return journal, nil
}

func (s *Service) OpenTenantSpace(ctx context.Context, req *pb.OpenTenantSpaceRequest) (*pb.OpenTenantSpaceResponse, error) {
	if req == nil || hasUnknown(req.ProtoReflect()) || !biz.UUID(req.GetOperationId()) {
		return nil, rpcErrorCtx(ctx, biz.ErrInvalid)
	}
	journal, err := s.tenantSpaceJournal(ctx, req.GetTenantId())
	if err != nil {
		return nil, err
	}
	space, err := journal.OpenTenantSpace(ctx, req.GetTenantId(), req.GetOperationId())
	if err != nil {
		return nil, rpcErrorCtx(ctx, err)
	}
	return &pb.OpenTenantSpaceResponse{Space: tenantSpaceProto(space)}, nil
}

func (s *Service) CloseTenantSpace(ctx context.Context, req *pb.CloseTenantSpaceRequest) (*pb.CloseTenantSpaceResponse, error) {
	if req == nil || hasUnknown(req.ProtoReflect()) || !biz.UUID(req.GetOperationId()) {
		return nil, rpcErrorCtx(ctx, biz.ErrInvalid)
	}
	journal, err := s.tenantSpaceJournal(ctx, req.GetTenantId())
	if err != nil {
		return nil, err
	}
	space, err := journal.CloseTenantSpace(ctx, req.GetTenantId(), req.GetOperationId())
	if err != nil {
		return nil, rpcErrorCtx(ctx, err)
	}
	return &pb.CloseTenantSpaceResponse{Space: tenantSpaceProto(space)}, nil
}

func (s *Service) GetTenantSpace(ctx context.Context, req *pb.GetTenantSpaceRequest) (*pb.GetTenantSpaceResponse, error) {
	if req == nil || hasUnknown(req.ProtoReflect()) {
		return nil, rpcErrorCtx(ctx, biz.ErrInvalid)
	}
	journal, err := s.tenantSpaceJournal(ctx, req.GetTenantId())
	if err != nil {
		return nil, err
	}
	space, err := journal.GetTenantSpace(ctx, req.GetTenantId())
	if err != nil {
		return nil, rpcErrorCtx(ctx, err)
	}
	return &pb.GetTenantSpaceResponse{Space: tenantSpaceProto(space)}, nil
}
