package vm

import (
	"context"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/flowcontrol"
)

// RuntimeReader keeps optional detail/QGA reads outside the power command
// budget. Both Kubernetes and KubeVirt reads share one small private budget;
// concurrent viewers cannot create an unbounded queue of guest queries.
type RuntimeReader struct {
	native *Native
	active chan struct{}
}

func NewRuntimeReader(config *rest.Config) (*RuntimeReader, error) {
	if config == nil {
		return nil, biz.ErrInvalid
	}
	cfg := rest.CopyConfig(config)
	cfg.RateLimiter = flowcontrol.NewTokenBucketRateLimiter(2, 4)
	client, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	native, err := NewNative(client, cfg)
	if err != nil {
		return nil, err
	}
	return &RuntimeReader{native: native, active: make(chan struct{}, 2)}, nil
}

func (r *RuntimeReader) ReadRuntime(ctx context.Context, v biz.VM) (biz.Runtime, error) {
	select {
	case r.active <- struct{}{}:
		defer func() { <-r.active }()
		return r.native.ReadRuntime(ctx, v)
	default:
		return biz.Runtime{}, biz.ErrDependency
	}
}
