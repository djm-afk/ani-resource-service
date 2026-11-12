package vm

import (
	"fmt"
	"strings"
	"testing"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestRPCErrorReasonDetail pins the wire contract the Governor relies on:
// every rejected VM request carries its stable reason as an errdetails
// ErrorInfo detail, so gateway encoders (Kratos errors.FromError) can put it
// into the public `reason` field. The `message` keeps the full human text.
func TestRPCErrorReasonDetail(t *testing.T) {
	cases := []struct {
		name        string
		err         error
		code        codes.Code
		messagePart string
		reason      string
	}{
		{"changed", biz.ErrChanged, codes.FailedPrecondition, "VM_DEPENDENCY_CHANGED", "VM_DEPENDENCY_CHANGED"},
		{"changed wrapped", fmt.Errorf("%w: install ISO swapped", biz.ErrChanged), codes.FailedPrecondition, "install ISO swapped", "VM_DEPENDENCY_CHANGED"},
		{"invalid", biz.ErrInvalid, codes.InvalidArgument, "VM_INVALID_REQUEST", "VM_INVALID_REQUEST"},
		{"not found", biz.ErrNotFound, codes.NotFound, "VM_NOT_FOUND", "VM_NOT_FOUND"},
		{"busy", biz.ErrBusy, codes.FailedPrecondition, "VM_OPERATION_CONFLICT", "VM_OPERATION_CONFLICT"},
		{"scope denied", biz.ErrDenied, codes.PermissionDenied, "VM_SCOPE_DENIED", "VM_SCOPE_DENIED"},
		{"dependency", biz.ErrDependency, codes.Unavailable, "VM_DEPENDENCY_NOT_READY", "VM_DEPENDENCY_NOT_READY"},
		{"clone not ready", fmt.Errorf("%w: source PVC is not Bound", biz.ErrCloneSourceNotReady), codes.Unavailable, "VM_CLONE_SOURCE_NOT_READY", "VM_CLONE_SOURCE_NOT_READY"},
	}
	for _, c := range cases {
		st := status.Convert(rpcError(c.err))
		if st.Code() != c.code {
			t.Fatalf("%s: code = %v, want %v", c.name, st.Code(), c.code)
		}
		if !strings.Contains(st.Message(), c.messagePart) {
			t.Fatalf("%s: message = %q, want substring %q", c.name, st.Message(), c.messagePart)
		}
		var info *errdetails.ErrorInfo
		for _, d := range st.Details() {
			if i, ok := d.(*errdetails.ErrorInfo); ok {
				info = i
				break
			}
		}
		if info == nil || info.Reason != c.reason {
			t.Fatalf("%s: reason detail = %v, want %q", c.name, info, c.reason)
		}
	}
}
