package instance

import (
	"context"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/instance"
)

// UseNetworkAdmission keeps the acceptance read after replay and DELETE-first
// handling. It does not share a transaction with the Network repository.
func (p *Postgres) UseNetworkAdmission(admit func(context.Context, string, string, string, biz.NetworkSelection) (biz.NetworkSelection, error)) {
	p.admitNewNetwork = admit
}
