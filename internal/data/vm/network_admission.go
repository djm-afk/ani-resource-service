package vm

import (
	"context"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
)

// UseNetworkAdmission installs the read-only acceptance callback. It runs once
// under the VM resource lock, after permanent replay and before the owner insert.
func (p *Postgres) UseNetworkAdmission(admit func(context.Context, string, biz.Spec) (biz.NetworkSelection, error)) {
	p.admitNewNetwork = admit
}
