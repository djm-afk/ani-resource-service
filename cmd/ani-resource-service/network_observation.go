package main

import (
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
	observation, err := provider.EnableObservation(network.Worker.RequestTimeout.AsDuration())
	if err != nil {
		return nil, nil, err
	}
	return provider, observation, nil
}
