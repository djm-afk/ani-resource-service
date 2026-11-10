package compute

import (
	"context"
	"errors"
	"fmt"
	"github.com/google/uuid"
	gp "github.com/zhangzhe-ctrl/ani-accelerator-service/pkg/gpuprotocol"
	"math"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

var ErrInvalid = errors.New("INVALID_COMPUTE_PRESET_REQUEST")
var ErrDenied = errors.New("COMPUTE_SCOPE_DENIED")
var ErrNotFound = errors.New("COMPUTE_PRESET_NOT_FOUND")

const TypeCPU = "cpu"
const TypeGPU = "gpu"

func UUID(v string) bool { u, e := uuid.Parse(v); return e == nil && u != uuid.Nil && u.String() == v }
func PageWindow(page, size uint32) (uint32, uint32, int64, error) {
	if page == 0 {
		page = 1
	}
	if size == 0 {
		size = 20
	}
	if size > 100 {
		return 0, 0, 0, ErrInvalid
	}
	return page, size, int64(page-1) * int64(size), nil
}

var ErrPresetConflict = errors.New("COMPUTE_PRESET_CODE_CONFLICT")
var ErrPresetDisabled = errors.New("COMPUTE_PRESET_DISABLED")
var presetCode = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

type GPUReference struct {
	AllocationType        string `json:"allocation_type"`
	ResourceKey           string `json:"resource_key"`
	ComputeUnitsPerDevice uint32 `json:"compute_units_per_device"`
	AdapterID             string `json:"adapter_id"`
	ProfileID             string `json:"profile_id"`
	ProfileVersion        uint64 `json:"profile_version"`
	SpecDigest            string `json:"spec_digest"`
	DevicesPerInstance    uint32 `json:"devices_per_instance"`
	ClusterID             string `json:"cluster_id"`
	PoolID                string `json:"pool_id"`
	Mode                  string `json:"mode"`
	ModelKey              string `json:"model_key"`
	MemoryMiBPerDevice    int64  `json:"memory_mib_per_device"`
	CoreLimitPercent      uint32 `json:"core_limit_percent"`
}
type Preset struct {
	ID, Code, Name, Description, ComputeType string
	CPUMillis, MemoryMiB                     int64
	Enabled                                  bool
	GPU                                      *GPUReference
	CreatedAt, UpdatedAt                     time.Time
	CreatedBy, UpdatedBy                     string
}
type PresetFilter struct {
	Page, PageSize                   uint32
	Name, Code, ComputeType, GPUMode string
	Enabled                          *bool
	Tenant                           bool
	AllowedGPUProfiles               []GPUReference
}
type PresetRepository interface {
	CreatePreset(context.Context, Preset) (Preset, error)
	GetPreset(context.Context, string) (Preset, error)
	ListPresets(context.Context, PresetFilter) ([]Preset, int64, bool, error)
	UpdatePreset(context.Context, string, Preset, []string) (Preset, error)
	SetPresetEnabled(context.Context, string, bool) (Preset, error)
	DeletePreset(context.Context, string) error
}

func ValidateGPUReference(g *GPUReference) error {
	if g == nil || !UUID(g.ClusterID) || !UUID(g.PoolID) {
		return ErrInvalid
	}
	if err := gp.ValidateBusinessSpecFields(g.BusinessFields()); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return nil
}

func ModeNumber(name string) int32 {
	return map[string]int32{"WHOLE_EXCLUSIVE": gp.ModeWholeExclusive, "SHARED_FIXED": gp.ModeSharedFixed}[name]
}
func AllocationNumber(name string) int32 {
	return map[string]int32{"PHYSICAL": gp.AllocationPhysical, "DYNAMIC_SHARED": gp.AllocationDynamicShared, "FIXED_PARTITION": gp.AllocationFixedPartition}[name]
}
func (g *GPUReference) BusinessFields() *gp.BusinessSpecFields {
	if g == nil {
		return nil
	}
	return &gp.BusinessSpecFields{ProfileID: g.ProfileID, ProfileVersion: g.ProfileVersion, SpecDigest: g.SpecDigest, ModelKey: g.ModelKey, Mode: ModeNumber(g.Mode), AllocationType: AllocationNumber(g.AllocationType), DevicesPerReplica: g.DevicesPerInstance, MemoryMiBPerDevice: g.MemoryMiBPerDevice, CoreLimitPercent: g.CoreLimitPercent, ComputeUnitsPerDevice: g.ComputeUnitsPerDevice, ResourceKey: g.ResourceKey, AdapterID: g.AdapterID}
}
func ValidatePreset(p Preset) error {
	if !UUID(p.ID) || !presetCode.MatchString(p.Code) || strings.TrimSpace(p.Name) == "" || utf8.RuneCountInString(p.Name) > 128 || utf8.RuneCountInString(p.Description) > 1024 || p.CPUMillis <= 0 || p.MemoryMiB <= 0 || p.MemoryMiB > math.MaxInt64/(1024*1024) {
		return ErrInvalid
	}
	if p.ComputeType == TypeCPU {
		if p.GPU != nil {
			return ErrInvalid
		}
		return nil
	}
	if p.ComputeType != TypeGPU {
		return ErrInvalid
	}
	return ValidateGPUReference(p.GPU)
}
func ValidatePresetMask(paths []string) error {
	if len(paths) == 0 {
		return ErrInvalid
	}
	seen := map[string]bool{}
	for _, p := range paths {
		if seen[p] {
			return ErrInvalid
		}
		seen[p] = true
		switch p {
		case "name", "description", "compute_type", "cpu_millis", "memory_mib", "enabled", "gpu":
		default:
			return ErrInvalid
		}
	}
	return nil
}
func MergePreset(current, changes Preset, paths []string) (Preset, error) {
	if err := ValidatePresetMask(paths); err != nil {
		return Preset{}, err
	}
	for _, p := range paths {
		switch p {
		case "name":
			current.Name = changes.Name
		case "description":
			current.Description = changes.Description
		case "compute_type":
			current.ComputeType = changes.ComputeType
		case "cpu_millis":
			current.CPUMillis = changes.CPUMillis
		case "memory_mib":
			current.MemoryMiB = changes.MemoryMiB
		case "enabled":
			current.Enabled = changes.Enabled
		case "gpu":
			current.GPU = changes.GPU
		}
	}
	return current, ValidatePreset(current)
}
func ValidatePresetFilter(f PresetFilter) error {
	if _, _, _, err := PageWindow(f.Page, f.PageSize); err != nil {
		return err
	}
	if utf8.RuneCountInString(strings.TrimSpace(f.Name)) > 128 || utf8.RuneCountInString(strings.TrimSpace(f.Code)) > 64 {
		return ErrInvalid
	}
	if f.ComputeType != "" && f.ComputeType != TypeCPU && f.ComputeType != TypeGPU {
		return ErrInvalid
	}
	if f.GPUMode != "" && f.GPUMode != "WHOLE_EXCLUSIVE" && f.GPUMode != "SHARED_FIXED" {
		return ErrInvalid
	}
	return nil
}
