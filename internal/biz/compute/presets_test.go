package compute

import (
	"math"
	"strings"
	"testing"
)

func TestPresetFinalObjectValidation(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	p := Preset{ID: id, Code: "cpu-one", Name: "CPU", ComputeType: TypeCPU, CPUMillis: 1250, MemoryMiB: 1536}
	if err := ValidatePreset(p); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*Preset){"zero CPU": func(p *Preset) { p.CPUMillis = 0 }, "negative memory": func(p *Preset) { p.MemoryMiB = -1 }, "overflow bytes": func(p *Preset) { p.MemoryMiB = math.MaxInt64 }, "invalid code": func(p *Preset) { p.Code = "CPU" }, "name length": func(p *Preset) { p.Name = strings.Repeat("界", 129) }, "missing type": func(p *Preset) { p.ComputeType = "" }, "CPU carries GPU": func(p *Preset) { p.GPU = &GPUReference{} }} {
		t.Run(name, func(t *testing.T) {
			changed := p
			change(&changed)
			if ValidatePreset(changed) == nil {
				t.Fatal("invalid preset accepted")
			}
		})
	}
	for _, mask := range [][]string{nil, {"name", "name"}, {"code"}, {"gpu.profile_id"}, {"*"}, {"data.name"}} {
		if ValidatePresetMask(mask) == nil {
			t.Fatal("invalid mask", mask)
		}
	}
	g := GPUReference{AllocationType: "PHYSICAL", ResourceKey: "volcano.sh/vgpu-number", AdapterID: "volcano-hami-mib-v1", ProfileID: id, ProfileVersion: 2, SpecDigest: strings.Repeat("a", 64), DevicesPerInstance: 1, ClusterID: id, PoolID: id, Mode: "WHOLE_EXCLUSIVE", ModelKey: "A100", MemoryMiBPerDevice: 15360, CoreLimitPercent: 100}
	for _, n := range []uint32{1, 2, 4, math.MaxInt32} {
		g.DevicesPerInstance = n
		if ValidateGPUReference(&g) != nil {
			t.Fatal("supported whole quantity", n)
		}
	}
	g.DevicesPerInstance = 0
	if ValidateGPUReference(&g) == nil {
		t.Fatal("zero whole quantity accepted")
	}
	g.DevicesPerInstance = 2
	g.Mode = "SHARED_FIXED"
	g.AllocationType = "DYNAMIC_SHARED"
	m := int64(6144)
	g.MemoryMiBPerDevice = m
	g.CoreLimitPercent = 25
	if ValidateGPUReference(&g) == nil {
		t.Fatal("shared two devices accepted")
	}
	g.DevicesPerInstance = 1
	if ValidateGPUReference(&g) != nil {
		t.Fatal("shared one device rejected")
	}
	g.CoreLimitPercent = 100
	if ValidateGPUReference(&g) == nil {
		t.Fatal("shared full core allocation accepted")
	}
	g.CoreLimitPercent = 99
	if ValidateGPUReference(&g) != nil {
		t.Fatal("shared 99 percent rejected")
	}
}
