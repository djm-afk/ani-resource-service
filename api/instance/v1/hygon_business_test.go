package instancev1

import (
	"bytes"
	"strings"
	"testing"

	acc "github.com/zhangzhe-ctrl/ani-accelerator-service/api/gen/go/accelerator/v1"
	"google.golang.org/protobuf/proto"
)

func TestHygonBusinessPayloadRoundTripRejectsRetiredGPU(t *testing.T) {
	for _, fixed := range []bool{false, true} {
		s := currentBusiness()
		s.ComputeType = "gpu"
		b := &acc.GpuBusinessSpec{ProfileId: s.PresetId, ProfileVersion: 1, ModelKey: "hcu", Mode: acc.SupplyMode_WHOLE_EXCLUSIVE, AllocationType: acc.AllocationType_PHYSICAL, DevicesPerReplica: 1, ResourceKey: "hygon.com/hcu", AdapterId: "hygon-hcu-v1", SpecDigest: strings.Repeat("a", 64)}
		if fixed {
			b.Mode = acc.SupplyMode_SHARED_FIXED
			b.AllocationType = acc.AllocationType_FIXED_PARTITION
			b.ResourceKey = "hygon.com/hcu-share-4c-16g"
			b.MemoryMibPerDevice = 16385
			b.ComputeUnitsPerDevice = 4
		}
		s.GpuBusinessSpec = b
		s.GpuExecution = &acc.GpuExecution{SchedulerName: "volcano", QueueName: "default", NodeSelector: map[string]string{"accelerator.ani.io/supply-group": s.PresetId, "accelerator.ani.io/model-key": "hcu", "accelerator.ani.io/baseline-id": s.PresetId}, Resources: map[string]string{b.ResourceKey: "1"}}
		s.GpuExecution.ExecutionDigest, _ = ExecutionDigest(s.GpuExecution)
		raw, err := CanonicalBusinessPayload(s)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(raw, []byte("gpu_display")) || !bytes.Contains(raw, []byte("gpu_business_spec")) {
			t.Fatal("GPU canonical retained retired fields")
		}
		restored, err := DecodeBusinessPayload(raw)
		if err != nil || !proto.Equal(restored, s) {
			t.Fatal("Hygon frozen business changed", err)
		}
		legacy := append(append([]byte{}, raw[:len(raw)-1]...), []byte(`,"gpu_display":null}`)...)
		if _, err = DecodeBusinessPayload(legacy); err == nil {
			t.Fatal("GPU accepted retired display even as null")
		}
		s.GpuBusinessSpec.ResourceKey = "hygon.com/different"
		invalid, err := CanonicalBusinessPayload(s)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = DecodeBusinessPayload(invalid); err == nil {
			t.Fatal("execution mismatch accepted")
		}
	}
}
