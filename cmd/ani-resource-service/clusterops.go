package main

import (
	"context"
	"fmt"
	pb "github.com/zhangzhe-ctrl/ani-resource-service/api/clusterops/v1"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/clusterops"
	conf "github.com/zhangzhe-ctrl/ani-resource-service/internal/conf/v1"
	data "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/clusterops"
	networkdata "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network"
	sandboxdata "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/sandbox"
	service "github.com/zhangzhe-ctrl/ani-resource-service/internal/service/clusterops"
	"google.golang.org/grpc"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"strings"
)

type clusterOpsComponents struct {
	service *service.Service
	reader  *sandboxdata.NamespaceRegistrationReader
}

func (c *clusterOpsComponents) Close() {
	if c != nil {
		c.reader.Close()
	}
}
func (c *clusterOpsComponents) Register(s grpc.ServiceRegistrar) {
	pb.RegisterPlatformClusterOpsServiceServer(s, c.service)
}
func openClusterOps(ctx context.Context, cfg *conf.Network, network *networkdata.Postgres, compute *computeComponents) (*clusterOpsComponents, error) {
	if cfg == nil {
		return nil, fmt.Errorf("clusterops requires managed cluster configuration")
	}
	var config *rest.Config
	var err error
	if cfg.Kubeconfig == "" {
		config, err = rest.InClusterConfig()
	} else {
		config, err = clientcmd.BuildConfigFromFlags("", cfg.Kubeconfig)
	}
	if err != nil {
		return nil, fmt.Errorf("clusterops Kubernetes configuration unavailable")
	}
	kube, err := data.NewKubernetes(config)
	if err != nil {
		return nil, err
	}
	var networkSource biz.NamespaceSource = biz.UnavailableNamespaceSource{}
	if network != nil {
		networkSource = network
	}
	var sandboxSource biz.NamespaceSource = biz.UnavailableNamespaceSource{}
	result := new(clusterOpsComponents)
	if compute != nil && compute.settings.ClusterID == cfg.ClusterId {
		raw, readErr := readImagePrivateFile(compute.settings.DatabaseDSNFile)
		if readErr == nil {
			reader, openErr := sandboxdata.OpenNamespaceRegistrationReader(ctx, strings.TrimSpace(string(raw)), cfg.ClusterId)
			if openErr == nil {
				result.reader = reader
				sandboxSource = reader
			}
		}
		clear(raw)
	}
	uc, err := biz.NewUseCase(cfg.ClusterId, kube, biz.NewNamespacePolicy(networkSource, sandboxSource))
	if err != nil {
		result.Close()
		return nil, err
	}
	result.service = service.NewService(uc)
	return result, nil
}
