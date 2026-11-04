package main

import (
	"context"
	"time"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	conf "github.com/zhangzhe-ctrl/ani-resource-service/internal/conf/v1"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network"
)

// Both runtime modes use the same request budgets and observation lifecycle.
func openNetworkProvider(repository *data.Postgres, network *conf.Network) (*data.KCProvider, *data.KCObservation, error) {
	clientPolicy := data.KCClientPolicy{QPS: 5, Burst: 10}
	if o := network.Observation; o != nil {
		clientPolicy = data.KCClientPolicy{QPS: float32(o.RequestQps), Burst: int(o.RequestBurst)}
	}
	provider, err := data.OpenKCProvider(repository, network.Kubeconfig, clientPolicy)
	if err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err = repository.SyncPlatformBaseNetworkConfig(ctx, platformBaseNetworkConfig(network.PlatformBaseNetwork)); err != nil {
		return nil, nil, err
	}
	observation, err := provider.EnableObservation(network.Worker.RequestTimeout.AsDuration())
	if err != nil {
		return nil, nil, err
	}
	return provider, observation, nil
}

func platformBaseNetworkConfig(c *conf.PlatformBaseNetwork) *biz.PlatformBaseNetworkConfig {
	if c == nil {
		return nil
	}
	v := c.GetVpc()
	out := &biz.PlatformBaseNetworkConfig{ClusterUID: c.GetClusterUid(), VPC: biz.PlatformNetworkConfigEntry{ProductID: v.GetProductId(), NativeNamespace: v.GetNativeNamespace(), NativeName: v.GetNativeName(), NativeUID: v.GetNativeUid(), DisplayName: v.GetDisplayName(), Description: v.GetDescription(), Published: v.GetPublished(), AcceptNew: v.GetAcceptNew()}}
	for _, s := range c.GetSubnets() {
		out.Subnets = append(out.Subnets, biz.PlatformNetworkConfigEntry{ProductID: s.GetProductId(), NativeNamespace: s.GetNativeNamespace(), NativeName: s.GetNativeName(), NativeUID: s.GetNativeUid(), DisplayName: s.GetDisplayName(), Description: s.GetDescription(), Published: s.GetPublished(), AcceptNew: s.GetAcceptNew()})
	}
	return out
}
