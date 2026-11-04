package main

import (
	"github.com/google/uuid"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	conf "github.com/zhangzhe-ctrl/ani-resource-service/internal/conf/v1"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/server"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/service/network"
)

func wireDefaultVPCEgress(repository *data.Postgres, provider *data.KCProvider, network *conf.Network, policy biz.WorkerPolicy) (*service.PlatformDefaultEgressService, server.Stepper, error) {
	guard := network.GetPlatformDefaultVpcEgress()
	if guard == nil {
		return nil, nil, nil
	}
	identity := biz.DefaultVPCIdentity{ClusterID: network.GetClusterId(), ClusterUID: guard.GetClusterUid(), Namespace: guard.GetNamespace(), Name: guard.GetName(), UID: guard.GetUid()}
	adapter, err := data.NewDefaultEgressKCProvider(provider, identity)
	if err != nil {
		return nil, nil, err
	}
	repo, err := data.NewDefaultVPCEgressRepository(repository, identity)
	if err != nil {
		return nil, nil, err
	}
	usecase, err := biz.NewDefaultVPCEgress(repo, biz.ContextEgressAuthorization{})
	if err != nil {
		return nil, nil, err
	}
	worker, err := biz.NewDefaultVPCEgressWorker(repo, adapter, uuid.NewString(), policy)
	if err != nil {
		return nil, nil, err
	}
	return service.NewPlatformDefaultEgressService(usecase), worker, nil
}
