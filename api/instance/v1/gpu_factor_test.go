package instancev1

import (
	"bytes"
	"testing"

	acc "github.com/zhangzhe-ctrl/ani-accelerator-service/api/gen/go/accelerator/v1"
	"google.golang.org/protobuf/proto"
)

func TestFrozenGpuMemoryFactorContract(t *testing.T) {
	execution := executionFixture()
	execution.Resources["volcano.sh/vgpu-memory"] = "384"
	execution.PodAnnotations["accelerator.ani.io/memory-block-mib"] = "10"
	execution.ExecutionDigest, _ = ExecutionDigest(execution)
	if err := ValidateGpuExecution(execution); err != nil {
		t.Fatal("frozen F10 execution rejected", err)
	}
	for _, factor := range []string{"0", "1", "010", "-10", "100", ""} {
		t.Run("factor="+factor, func(t *testing.T) {
			invalid := proto.Clone(execution).(*acc.GpuExecution)
			invalid.PodAnnotations["accelerator.ani.io/memory-block-mib"] = factor
			invalid.ExecutionDigest, _ = ExecutionDigest(invalid)
			if ValidateGpuExecution(invalid) == nil {
				t.Fatal("invalid frozen factor accepted", factor)
			}
		})
	}
}

func TestFrozenGpuMemoryFactorPayloadReplay(t *testing.T) {
	for _, factor := range []string{"", "10"} {
		t.Run("factor="+factor, func(t *testing.T) {
			execution := executionFixture()
			if factor != "" {
				execution.Resources["volcano.sh/vgpu-memory"] = "384"
				execution.PodAnnotations["accelerator.ani.io/memory-block-mib"] = factor
				execution.ExecutionDigest, _ = ExecutionDigest(execution)
			}
			spec := currentBusiness()
			spec.ComputeType = "gpu"
			spec.GpuExecution = execution
			spec.GpuBusinessSpec = fixtureBusinessSpec(spec.PresetId, 8192)
			if factor != "" {
				spec.GpuBusinessSpec.MemoryMibPerDevice = 3840
			}
			frozen, err := CanonicalBusinessPayload(spec)
			if err != nil {
				t.Fatal(err)
			}
			restored, err := DecodeBusinessPayload(frozen)
			if err != nil || !proto.Equal(restored, spec) {
				t.Fatal("frozen GPU execution changed on replay", err)
			}
			replayed, err := CanonicalBusinessPayload(restored)
			if err != nil || !bytes.Equal(frozen, replayed) || restored.GpuExecution.ExecutionDigest != execution.ExecutionDigest {
				t.Fatal("frozen GPU bytes or digest changed", err)
			}
		})
	}
}
