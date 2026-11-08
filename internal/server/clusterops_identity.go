package server

import (
	"context"
	pb "github.com/zhangzhe-ctrl/ani-resource-service/api/clusterops/v1"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/clusterops"
	"google.golang.org/grpc"
)

var clusterOpsPlatformMethods = map[string]struct{}{
	pb.PlatformClusterOpsService_ListClusters_FullMethodName:       {},
	pb.PlatformClusterOpsService_GetClusterOverview_FullMethodName: {},
	pb.PlatformClusterOpsService_ListResourceTypes_FullMethodName:  {},
	pb.PlatformClusterOpsService_ListResources_FullMethodName:      {},
	pb.PlatformClusterOpsService_GetResource_FullMethodName:        {},
	pb.PlatformClusterOpsService_CreateResource_FullMethodName:     {},
	pb.PlatformClusterOpsService_UpdateResource_FullMethodName:     {},
	pb.PlatformClusterOpsService_DeleteResource_FullMethodName:     {},
	pb.PlatformClusterOpsService_ScaleWorkload_FullMethodName:      {},
	pb.PlatformClusterOpsService_RestartWorkload_FullMethodName:    {},
}

const ClusterOpsLogsMethod = pb.PlatformClusterOpsService_StreamPodLogs_FullMethodName

func withClusterOpsCaller(ctx context.Context, p governancePrincipal) context.Context {
	return biz.WithCaller(ctx, biz.Caller{Platform: p.Platform, Actor: p.Actor, DirectCaller: p.Workload, RequestID: p.RequestID})
}

type clusterOpsServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *clusterOpsServerStream) Context() context.Context { return s.ctx }
