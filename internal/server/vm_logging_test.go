package server

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/go-kratos/kratos/v3/transport"
	vm "github.com/zhangzhe-ctrl/ani-resource-service/api/vm/v1"
)

func TestVMGuestCredentialNotLogged(t *testing.T) {
	var out bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&out, nil))
	req := &vm.AcceptCreateRequest{GuestCredentials: &vm.GuestCredentials{Password: "private-guest-password-sentinel"}}
	ctx := transport.NewServerContext(context.Background(), imageLogTransport{vm.VMService_AcceptCreate_FullMethodName})
	called := false
	_, e := serverMiddlewareChain(logger, (func(_ context.Context, r any) (any, error) {
		called = true
		if r != req {
			t.Fatal("real request not delivered")
		}
		return &vm.Acceptance{Accepted: true}, nil
	}))(ctx, req)
	if e != nil || !called || strings.Contains(out.String(), req.GuestCredentials.Password) {
		t.Fatal("VM credential logger disclosure")
	}
}
