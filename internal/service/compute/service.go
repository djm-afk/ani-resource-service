package compute

import (
	"context"
	"encoding/hex"
	"errors"
	acc "github.com/zhangzhe-ctrl/ani-accelerator-service/api/gen/go/accelerator/v1"
	pb "github.com/zhangzhe-ctrl/ani-resource-service/api/compute/v1"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/compute"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"strings"
	"time"
)

type Service struct {
	pb.UnimplementedComputePresetServiceServer
	repo      biz.PresetRepository
	resolver  *biz.Resolver
	clusterID string
}

func NewService(r biz.PresetRepository, resolver *biz.Resolver, clusterID string) (*Service, error) {
	if r == nil || resolver == nil || !biz.UUID(clusterID) {
		return nil, biz.ErrInvalid
	}
	return &Service{repo: r, resolver: resolver, clusterID: clusterID}, nil
}
func rpcError(e error) error {
	switch {
	case e == nil:
		return nil
	case errors.Is(e, biz.ErrInvalid):
		return status.Error(codes.InvalidArgument, "INVALID_COMPUTE_PRESET_REQUEST")
	case errors.Is(e, biz.ErrDenied):
		return status.Error(codes.PermissionDenied, "COMPUTE_SCOPE_DENIED")
	case errors.Is(e, biz.ErrNotFound):
		return status.Error(codes.NotFound, "COMPUTE_PRESET_NOT_FOUND")
	case errors.Is(e, biz.ErrPresetConflict):
		return status.Error(codes.AlreadyExists, "COMPUTE_PRESET_CODE_CONFLICT")
	case errors.Is(e, biz.ErrPresetDisabled):
		return status.Error(codes.FailedPrecondition, "COMPUTE_PRESET_DISABLED")
	case errors.Is(e, context.Canceled), errors.Is(e, context.DeadlineExceeded):
		return status.FromContextError(e).Err()
	default:
		return status.Error(codes.Unavailable, "COMPUTE_DEPENDENCY_UNAVAILABLE")
	}
}
func gpuReference(g *pb.GpuPresetReference) *biz.GPUReference {
	if g == nil {
		return nil
	}
	return &biz.GPUReference{ProfileID: g.ProfileId, ProfileVersion: g.ProfileVersion, SpecDigest: g.SpecDigest, DevicesPerInstance: g.DevicesPerInstance, ClusterID: g.ClusterId, PoolID: g.PoolId, Mode: g.Mode.String(), ModelKey: g.ModelKey, MemoryMiBPerDevice: g.MemoryMibPerDevice, CoreLimitPercent: g.CoreLimitPercent, AllocationType: g.AllocationType.String(), ResourceKey: g.ResourceKey, ComputeUnitsPerDevice: g.ComputeUnitsPerDevice, AdapterID: g.AdapterId}
}
func wireReference(g *biz.GPUReference) *pb.GpuPresetReference {
	if g == nil {
		return nil
	}
	return &pb.GpuPresetReference{ProfileId: g.ProfileID, ProfileVersion: g.ProfileVersion, SpecDigest: g.SpecDigest, DevicesPerInstance: g.DevicesPerInstance, ClusterId: g.ClusterID, PoolId: g.PoolID, Mode: acc.SupplyMode(acc.SupplyMode_value[g.Mode]), ModelKey: g.ModelKey, MemoryMibPerDevice: g.MemoryMiBPerDevice, CoreLimitPercent: g.CoreLimitPercent, AllocationType: acc.AllocationType(acc.AllocationType_value[g.AllocationType]), ResourceKey: g.ResourceKey, ComputeUnitsPerDevice: g.ComputeUnitsPerDevice, AdapterId: g.AdapterID}
}
func presetData(d *pb.PresetData) biz.Preset {
	return biz.Preset{Code: d.Code, Name: d.Name, Description: d.Description, ComputeType: d.ComputeType, CPUMillis: d.CpuMillis, MemoryMiB: d.MemoryMib, Enabled: d.GetEnabled(), GPU: gpuReference(d.Gpu)}
}
func wirePreset(p biz.Preset) *pb.Preset {
	reason := ""
	if !p.Enabled {
		reason = "COMPUTE_PRESET_DISABLED"
	}
	return &pb.Preset{Selectable: p.Enabled, UnavailableReason: reason, Id: p.ID, Code: p.Code, Name: p.Name, Description: p.Description, ComputeType: p.ComputeType, CpuMillis: p.CPUMillis, MemoryMib: p.MemoryMiB, Enabled: p.Enabled, Gpu: wireReference(p.GPU), CreatedAt: timestamppb.New(p.CreatedAt), UpdatedAt: timestamppb.New(p.UpdatedAt)}
}
func (s *Service) checkedPresetData(d *pb.PresetData) error {
	if d == nil || len(d.ProtoReflect().GetUnknown()) != 0 || (d.Gpu != nil && len(d.Gpu.ProtoReflect().GetUnknown()) != 0) {
		return biz.ErrInvalid
	}
	if d.Gpu != nil && d.Gpu.ClusterId != s.clusterID {
		return biz.ErrInvalid
	}
	return nil
}
func (s *Service) ListPresets(ctx context.Context, r *pb.ListPresetsRequest) (*pb.ListPresetsResponse, error) {
	if r == nil || len(r.ProtoReflect().GetUnknown()) != 0 || !biz.ValidUsage(biz.Usage(r.Usage)) {
		return nil, rpcError(biz.ErrInvalid)
	}
	if e := biz.RequireTenant(ctx, r.TenantId, biz.Usage(r.Usage)); e != nil {
		return nil, rpcError(e)
	}
	f := biz.PresetFilter{Page: r.Page, PageSize: r.PageSize, Name: r.Name, Code: r.Code, ComputeType: r.ComputeType, GPUMode: r.GpuMode, Tenant: true}
	for _, g := range r.AllowedGpuProfiles {
		if g == nil || len(g.ProtoReflect().GetUnknown()) != 0 || g.ClusterId != s.clusterID || !biz.UUID(g.ProfileId) || !biz.UUID(g.PoolId) || g.ProfileVersion == 0 || len(g.SpecDigest) != 64 || strings.ToLower(g.SpecDigest) != g.SpecDigest {
			return nil, rpcError(biz.ErrInvalid)
		}
		if _, e := hex.DecodeString(g.SpecDigest); e != nil {
			return nil, rpcError(biz.ErrInvalid)
		}
		f.AllowedGPUProfiles = append(f.AllowedGPUProfiles, *gpuReference(g))
	}
	return s.listPresets(ctx, f)
}
func (s *Service) listPresets(ctx context.Context, f biz.PresetFilter) (*pb.ListPresetsResponse, error) {
	items, total, has, e := s.repo.ListPresets(ctx, f)
	if e != nil {
		return nil, rpcError(e)
	}
	out := &pb.ListPresetsResponse{Items: []*pb.Preset{}, Total: total, HasGpuCandidates: has}
	for _, p := range items {
		out.Items = append(out.Items, wirePreset(p))
	}
	return out, nil
}
func (s *Service) AdminListPresets(ctx context.Context, r *pb.AdminListPresetsRequest) (*pb.ListPresetsResponse, error) {
	if r == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	if _, e := biz.RequirePlatform(ctx); e != nil {
		return nil, rpcError(e)
	}
	return s.listPresets(ctx, biz.PresetFilter{Page: r.Page, PageSize: r.PageSize, Name: r.Name, Code: r.Code, ComputeType: r.ComputeType, GPUMode: r.GpuMode, Enabled: r.Enabled})
}
func (s *Service) AdminGetPreset(ctx context.Context, r *pb.AdminGetPresetRequest) (*pb.Preset, error) {
	if r == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	if _, e := biz.RequirePlatform(ctx); e != nil {
		return nil, rpcError(e)
	}
	v, e := s.repo.GetPreset(ctx, r.Id)
	if e != nil {
		return nil, rpcError(e)
	}
	return wirePreset(v), nil
}
func (s *Service) AdminCreatePreset(ctx context.Context, r *pb.AdminCreatePresetRequest) (*pb.Preset, error) {
	if r == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	if _, e := biz.RequirePlatform(ctx); e != nil {
		return nil, rpcError(e)
	}
	if e := s.checkedPresetData(r.Data); e != nil {
		return nil, rpcError(e)
	}
	v, e := s.repo.CreatePreset(ctx, presetData(r.Data))
	if e != nil {
		return nil, rpcError(e)
	}
	return wirePreset(v), nil
}
func (s *Service) AdminUpdatePreset(ctx context.Context, r *pb.AdminUpdatePresetRequest) (*pb.Preset, error) {
	if r == nil || r.Data == nil || r.UpdateMask == nil || biz.ValidatePresetMask(r.UpdateMask.Paths) != nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	if _, e := biz.RequirePlatform(ctx); e != nil {
		return nil, rpcError(e)
	}
	for _, path := range r.UpdateMask.Paths {
		if path == "enabled" && r.Data.Enabled == nil {
			return nil, rpcError(biz.ErrInvalid)
		}
		if path == "gpu" && r.Data.Gpu != nil && r.Data.Gpu.ClusterId != s.clusterID {
			return nil, rpcError(biz.ErrInvalid)
		}
	}
	v, e := s.repo.UpdatePreset(ctx, r.Id, presetData(r.Data), r.UpdateMask.Paths)
	if e != nil {
		return nil, rpcError(e)
	}
	return wirePreset(v), nil
}
func (s *Service) AdminSetPresetEnabled(ctx context.Context, r *pb.AdminSetPresetEnabledRequest) (*pb.Preset, error) {
	if r == nil || r.Enabled == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	if _, e := biz.RequirePlatform(ctx); e != nil {
		return nil, rpcError(e)
	}
	v, e := s.repo.SetPresetEnabled(ctx, r.Id, *r.Enabled)
	if e != nil {
		return nil, rpcError(e)
	}
	return wirePreset(v), nil
}
func (s *Service) AdminDeletePreset(ctx context.Context, r *pb.AdminDeletePresetRequest) (*emptypb.Empty, error) {
	if r == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	if _, e := biz.RequirePlatform(ctx); e != nil {
		return nil, rpcError(e)
	}
	if e := s.repo.DeletePreset(ctx, r.Id); e != nil {
		return nil, rpcError(e)
	}
	return &emptypb.Empty{}, nil
}

func (s *Service) ResolveComputePreset(ctx context.Context, r *pb.ResolveComputePresetRequest) (*pb.ComputeSelection, error) {
	if r == nil || len(r.ProtoReflect().GetUnknown()) != 0 || !biz.ValidUsage(biz.Usage(r.Usage)) {
		return nil, rpcError(biz.ErrInvalid)
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	v, e := s.resolver.Resolve(ctx, r.TenantId, r.PresetId, biz.Usage(r.Usage))
	if e != nil {
		return nil, rpcError(e)
	}
	out := &pb.ComputeSelection{SchemaVersion: pb.SelectionSchemaVersion, PresetId: v.PresetID, Code: v.Code, Name: v.Name, ComputeType: v.ComputeType, CpuMillis: v.CPUMillis, MemoryMib: v.MemoryMiB}
	if g := v.GPU; g != nil {
		out.Gpu = &pb.GpuSelection{ProfileId: g.ProfileID, ProfileVersion: g.ProfileVersion, SpecDigest: g.SpecDigest, DevicesPerUnit: g.DevicesPerInstance, ClusterId: g.ClusterID, PoolId: g.PoolID, Mode: acc.SupplyMode(acc.SupplyMode_value[g.Mode]), ModelKey: g.ModelKey, MemoryMibPerDevice: g.MemoryMiBPerDevice, CoreLimitPercent: g.CoreLimitPercent, AllocationType: acc.AllocationType(acc.AllocationType_value[g.AllocationType]), ResourceKey: g.ResourceKey, ComputeUnitsPerDevice: g.ComputeUnitsPerDevice, AdapterId: g.AdapterID}
	}
	out.SelectionDigest, e = pb.SelectionDigest(out)
	if e != nil {
		return nil, rpcError(e)
	}
	return out, nil
}
