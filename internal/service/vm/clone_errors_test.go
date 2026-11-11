package vm

import (
	"strings"
	"testing"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestCloneEligibilityRPCCodes pins D3: an unsupported source is a stable
// failed precondition, a not-ready source stays a retryable unavailable.
func TestCloneEligibilityRPCCodes(t *testing.T) {
	unsupported := biz.CloneEligibility{Unsupported: []biz.CloneBlocker{
		{Kind: "UNSUPPORTED", VolumeName: "disk-sys", Reason: "source PVC volumeMode is not Block"},
	}}
	err := rpcError(unsupported.UnsupportedError())
	message := status.Convert(err).Message()
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(message, "VM_CLONE_SOURCE_UNAVAILABLE") || !strings.Contains(message, "disk-sys") {
		t.Fatalf("unsupported mapping = %v %q", status.Code(err), message)
	}

	notReady := biz.CloneEligibility{NotReady: []biz.CloneBlocker{
		{Kind: "NOT_READY", VolumeName: "disk-sys", Reason: "source PVC is not Bound"},
	}}
	err = rpcError(notReady.NotReadyError())
	message = status.Convert(err).Message()
	if status.Code(err) != codes.Unavailable || !strings.Contains(message, "VM_CLONE_SOURCE_NOT_READY") || !strings.Contains(message, "not Bound") {
		t.Fatalf("not-ready mapping = %v %q", status.Code(err), message)
	}

	// An ordinary dependency keeps the generic stable text.
	err = rpcError(biz.ErrDependency)
	if status.Code(err) != codes.Unavailable || status.Convert(err).Message() != "VM_DEPENDENCY_NOT_READY" {
		t.Fatalf("dependency mapping = %v %q", status.Code(err), status.Convert(err).Message())
	}
}
