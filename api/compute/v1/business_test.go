package computev1

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	accelerator "github.com/zhangzhe-ctrl/ani-accelerator-service/api/gen/go/accelerator/v1"
	"google.golang.org/protobuf/proto"
)

func TestSelectionV1CanonicalContract(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	s := &ComputeSelection{SchemaVersion: 1, PresetId: id, Code: "cpu", Name: "CPU", ComputeType: "cpu", CpuMillis: 1250, MemoryMib: 1536}
	const cpu = `{"schema_version":1,"preset_id":"11111111-1111-4111-8111-111111111111","code":"cpu","name":"CPU","compute_type":"cpu","cpu_millis":1250,"memory_mib":1536,"gpu":null}`
	verify := func(want string) {
		t.Helper()
		b, e := CanonicalSelection(s)
		if e != nil || string(b) != want {
			t.Fatal("canonical contract", string(b), e)
		}
		sum := sha256.Sum256([]byte("compute-selection-v1\n" + want))
		digest, e := SelectionDigest(s)
		if e != nil || digest != hex.EncodeToString(sum[:]) {
			t.Fatal("versioned selection digest", digest, e)
		}
		s.SelectionDigest = digest
		if e = ValidateSelection(s); e != nil {
			t.Fatal(e)
		}
		changed := proto.Clone(s).(*ComputeSelection)
		changed.MemoryMib++
		if ValidateSelection(changed) == nil {
			t.Fatal("changed resources retained old content identity")
		}
	}
	verify(cpu)
	s.Code = "gpu"
	s.Name = "GPU"
	s.ComputeType = "gpu"
	s.Gpu = &GpuSelection{ProfileId: id, ProfileVersion: 2, SpecDigest: strings.Repeat("a", 64), DevicesPerUnit: 1, ClusterId: id, PoolId: id, Mode: accelerator.SupplyMode_SHARED_FIXED, ModelKey: "t4", MemoryMibPerDevice: 8192, CoreLimitPercent: 25, AllocationType: accelerator.AllocationType_DYNAMIC_SHARED, ResourceKey: "volcano.sh/vgpu-number", AdapterId: "volcano-hami-mib-v1"}
	verify(`{"schema_version":1,"preset_id":"11111111-1111-4111-8111-111111111111","code":"gpu","name":"GPU","compute_type":"gpu","cpu_millis":1250,"memory_mib":1536,"gpu":{"profile_id":"11111111-1111-4111-8111-111111111111","profile_version":2,"spec_digest":"` + strings.Repeat("a", 64) + `","devices_per_unit":1,"cluster_id":"11111111-1111-4111-8111-111111111111","pool_id":"11111111-1111-4111-8111-111111111111","mode":"SHARED_FIXED","model_key":"t4","memory_mib_per_device":8192,"core_limit_percent":25,"allocation_type":"DYNAMIC_SHARED","resource_key":"volcano.sh/vgpu-number","compute_units_per_device":0,"adapter_id":"volcano-hami-mib-v1"}}`)
	for _, change := range []func(*ComputeSelection){
		func(v *ComputeSelection) { v.SchemaVersion = 2 }, func(v *ComputeSelection) { v.ComputeType = "" }, func(v *ComputeSelection) { v.CpuMillis = 0 }, func(v *ComputeSelection) { v.Gpu.DevicesPerUnit = 2 }, func(v *ComputeSelection) { v.Gpu.SpecDigest = strings.Repeat("A", 64) }, func(v *ComputeSelection) { v.ProtoReflect().SetUnknown([]byte{0x98, 0x06, 0x01}) }, func(v *ComputeSelection) { v.Gpu.ProtoReflect().SetUnknown([]byte{0x98, 0x06, 0x01}) },
	} {
		changed := proto.Clone(s).(*ComputeSelection)
		change(changed)
		if _, e := CanonicalSelection(changed); e == nil {
			t.Fatal("invalid or unknown selection accepted", changed)
		}
	}
}
