package instancev1

import (
	acc "github.com/zhangzhe-ctrl/ani-accelerator-service/api/gen/go/accelerator/v1"
	"github.com/zhangzhe-ctrl/ani-accelerator-service/pkg/gpuprotocol"
)

// ExecutionDigest uses Accelerator's fixed wire-contract field set.
func ExecutionDigest(g *acc.GpuExecution) (string, error) {
	if g == nil || hasUnknown(g.ProtoReflect()) {
		return "", ErrInvalidBusiness
	}
	return gpuprotocol.GpuExecutionDigest(g)
}
func ValidateGpuExecution(g *acc.GpuExecution) error {
	if g == nil || (g.RuntimeClassName != "" && ValidateGPURuntimeClassName(g.RuntimeClassName) != nil) {
		return ErrInvalidBusiness
	}
	if err := gpuprotocol.ValidateGpuExecution(g); err != nil {
		return ErrInvalidBusiness
	}
	return nil
}
