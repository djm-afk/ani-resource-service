package data

import (
	"context"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
)

// Observe reads only the target declaration. Informer events schedule this
// method; their stores are never used as a second source of resource state.
func (p *KCProvider) Observe(ctx context.Context, target biz.ProviderTarget) (biz.ProviderObservation, error) {
	if target.PlatformNetwork != nil {
		return p.observePlatformNetwork(ctx, target)
	}
	if egressKind(target.Kind) {
		return p.observeEgress(ctx, target)
	}
	return p.observeDirect(ctx, target)
}
