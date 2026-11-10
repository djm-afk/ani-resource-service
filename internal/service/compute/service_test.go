package compute

import (
	"context"
	"strings"
	"testing"

	pb "github.com/zhangzhe-ctrl/ani-resource-service/api/compute/v1"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/compute"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const testTenant = "11111111-1111-4111-8111-111111111111"
const testPreset = "22222222-2222-4222-8222-222222222222"
const testCluster = "33333333-3333-4333-8333-333333333333"

// Only the persistence boundary is replaced; the service, scope, resolver,
// preset validation and canonical selection implementation are real.
type presetReadProbe struct {
	biz.PresetRepository
	preset biz.Preset
	reads  int
	filter biz.PresetFilter
}

func (r *presetReadProbe) GetPreset(_ context.Context, id string) (biz.Preset, error) {
	r.reads++
	if id != r.preset.ID {
		return biz.Preset{}, biz.ErrNotFound
	}
	return r.preset, nil
}

func (r *presetReadProbe) ListPresets(_ context.Context, filter biz.PresetFilter) ([]biz.Preset, int64, bool, error) {
	r.filter = filter
	return []biz.Preset{r.preset}, 1, r.preset.GPU != nil, nil
}

func tenantScope(method string, usage pb.Usage) context.Context {
	return biz.WithScope(context.Background(), biz.Scope{TenantID: testTenant, Actor: "governance:user:42", RequestID: testPreset, Workload: "ani-governance", Method: method, Usage: biz.Usage(usage)})
}

func TestVirtualMachineResolveUsesSameSingleReadSelection(t *testing.T) {
	repo := &presetReadProbe{preset: biz.Preset{ID: testPreset, Code: "cpu", Name: "CPU", ComputeType: biz.TypeCPU, CPUMillis: 1500, MemoryMiB: 1536, Enabled: true}}
	resolver, err := biz.NewResolver(repo, testCluster)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(repo, resolver, testCluster)
	if err != nil {
		t.Fatal(err)
	}
	var digest string
	for _, usage := range []pb.Usage{pb.Usage_CONTAINER_INSTANCE, pb.Usage_TRAINING, pb.Usage_INFERENCE, pb.Usage_VIRTUAL_MACHINE} {
		before := repo.reads
		selection, err := service.ResolveComputePreset(tenantScope(biz.ResolveMethod, usage), &pb.ResolveComputePresetRequest{TenantId: testTenant, PresetId: testPreset, Usage: usage})
		if err != nil {
			t.Fatal(usage, err)
		}
		if repo.reads != before+1 || selection.CpuMillis != 1500 || selection.MemoryMib != 1536 || selection.SchemaVersion != 1 || pb.ValidateSelection(selection) != nil {
			t.Fatal("selection did not preserve the single-read resource contract", selection)
		}
		if digest != "" && digest != selection.SelectionDigest {
			t.Fatal("usage changed the existing canonical digest")
		}
		digest = selection.SelectionDigest
	}
	before := repo.reads
	_, err = service.ResolveComputePreset(tenantScope(biz.ResolveMethod, pb.Usage_CONTAINER_INSTANCE), &pb.ResolveComputePresetRequest{TenantId: testTenant, PresetId: testPreset, Usage: pb.Usage_VIRTUAL_MACHINE})
	if status.Code(err) != codes.PermissionDenied || repo.reads != before {
		t.Fatal("wrong delegated usage reached persistence", err)
	}
	repo.preset.Enabled = false
	_, err = service.ResolveComputePreset(tenantScope(biz.ResolveMethod, pb.Usage_VIRTUAL_MACHINE), &pb.ResolveComputePresetRequest{TenantId: testTenant, PresetId: testPreset, Usage: pb.Usage_VIRTUAL_MACHINE})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatal("disabled VM selection accepted", err)
	}
}

func TestVirtualMachineKeepsGenericGPUValidationAndDirectoryFilter(t *testing.T) {
	gpu := &biz.GPUReference{AllocationType: "PHYSICAL", ResourceKey: "volcano.sh/vgpu-number", AdapterID: "volcano-hami-mib-v1", ProfileID: testPreset, ProfileVersion: 1, SpecDigest: strings.Repeat("a", 64), DevicesPerInstance: 1, ClusterID: testCluster, PoolID: testTenant, Mode: "WHOLE_EXCLUSIVE", ModelKey: "T4", MemoryMiBPerDevice: 15360, CoreLimitPercent: 100}
	repo := &presetReadProbe{preset: biz.Preset{ID: testPreset, Code: "gpu", Name: "GPU", ComputeType: biz.TypeGPU, CPUMillis: 1500, MemoryMiB: 1536, Enabled: true, GPU: gpu}}
	resolver, _ := biz.NewResolver(repo, testCluster)
	service, _ := NewService(repo, resolver, testCluster)
	selection, err := service.ResolveComputePreset(tenantScope(biz.ResolveMethod, pb.Usage_VIRTUAL_MACHINE), &pb.ResolveComputePresetRequest{TenantId: testTenant, PresetId: testPreset, Usage: pb.Usage_VIRTUAL_MACHINE})
	if err != nil || selection.Gpu == nil || pb.ValidateSelection(selection) != nil {
		t.Fatal("VM added a backend GPU prohibition", err)
	}
	_, err = service.ListPresets(tenantScope(biz.ListMethod, pb.Usage_VIRTUAL_MACHINE), &pb.ListPresetsRequest{TenantId: testTenant, Usage: pb.Usage_VIRTUAL_MACHINE, AllowedGpuProfiles: []*pb.GpuPresetReference{wireReference(gpu)}})
	if err != nil || !repo.filter.Tenant || len(repo.filter.AllowedGPUProfiles) != 1 || repo.filter.AllowedGPUProfiles[0].SpecDigest != gpu.SpecDigest {
		t.Fatal("VM lost the trusted GPU intersection", err)
	}
	gpu.ClusterID = testTenant
	_, err = service.ResolveComputePreset(tenantScope(biz.ResolveMethod, pb.Usage_VIRTUAL_MACHINE), &pb.ResolveComputePresetRequest{TenantId: testTenant, PresetId: testPreset, Usage: pb.Usage_VIRTUAL_MACHINE})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatal("VM bypassed generic GPU cluster validation", err)
	}
}
