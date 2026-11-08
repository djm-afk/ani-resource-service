package clusterops

import (
	"context"
	pb "github.com/zhangzhe-ctrl/ani-resource-service/api/clusterops/v1"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/clusterops"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type Service struct {
	pb.UnimplementedPlatformClusterOpsServiceServer
	usecase *biz.UseCase
}

func NewService(u *biz.UseCase) *Service { return &Service{usecase: u} }

var _ pb.PlatformClusterOpsServiceServer = (*Service)(nil)

func domainType(t *pb.ResourceTypeRef) biz.ResourceTypeRef {
	return biz.ResourceTypeRef{Group: t.GetGroup(), Version: t.GetVersion(), Resource: t.GetResource()}
}
func domainRef(r *pb.ResourceRef) biz.ResourceRef {
	return biz.ResourceRef{ClusterID: r.GetClusterId(), Type: domainType(r.GetType()), Namespace: r.GetNamespace(), Name: r.GetName()}
}
func protoType(t biz.ResourceTypeRef) *pb.ResourceTypeRef {
	return &pb.ResourceTypeRef{Group: t.Group, Version: t.Version, Resource: t.Resource}
}
func protoRef(r biz.ResourceRef) *pb.ResourceRef {
	return &pb.ResourceRef{ClusterId: r.ClusterID, Type: protoType(r.Type), Namespace: r.Namespace, Name: r.Name}
}
func protoDocument(r biz.ResourceDocument) *pb.ResourceDocument {
	return &pb.ResourceDocument{Ref: protoRef(r.Ref), Uid: r.UID, ResourceVersion: r.ResourceVersion, ObjectJson: string(r.ObjectJSON), ReadOnly: r.ReadOnly, ReadOnlyReason: r.ReadOnlyReason, SecretDataRedacted: r.SecretDataRedacted, SecretKeys: r.SecretKeys}
}
func protoMutation(r biz.MutationResult) *pb.MutationResponse {
	return &pb.MutationResponse{Resource: protoDocument(r.Resource), DryRun: r.DryRun}
}
func (s *Service) ListClusters(ctx context.Context, _ *emptypb.Empty) (*pb.ListClustersResponse, error) {
	items, err := s.usecase.ListClusters(ctx)
	if err != nil {
		return nil, serviceError(err)
	}
	out := new(pb.ListClustersResponse)
	for _, c := range items {
		out.Items = append(out.Items, &pb.Cluster{ClusterId: c.ClusterID, DisplayName: c.DisplayName})
	}
	return out, nil
}
func (s *Service) GetClusterOverview(ctx context.Context, q *pb.GetClusterOverviewRequest) (*pb.ClusterOverview, error) {
	r, err := s.usecase.GetClusterOverview(ctx, q.GetClusterId())
	if err != nil {
		return nil, serviceError(err)
	}
	return &pb.ClusterOverview{ClusterId: r.ClusterID, KubernetesVersion: r.KubernetesVersion, NodesTotal: r.NodesTotal, NodesReady: r.NodesReady, NamespacesTotal: r.NamespacesTotal, PodsTotal: r.PodsTotal, PodPhaseCounts: r.PodPhaseCounts, Capacity: r.Capacity, Allocatable: r.Allocatable, SampledAt: timestamppb.New(r.SampledAt)}, nil
}
func (s *Service) ListResourceTypes(ctx context.Context, q *pb.ListResourceTypesRequest) (*pb.ListResourceTypesResponse, error) {
	items, err := s.usecase.ListResourceTypes(ctx, q.GetClusterId())
	if err != nil {
		return nil, serviceError(err)
	}
	out := new(pb.ListResourceTypesResponse)
	for _, d := range items {
		out.Items = append(out.Items, &pb.ResourceDescriptor{Type: protoType(d.Type), Kind: d.Kind, Namespaced: d.Namespaced, Actions: d.Actions, CustomResource: d.CustomResource, CrdName: d.CRDName})
	}
	return out, nil
}
func (s *Service) ListResources(ctx context.Context, q *pb.ListResourcesRequest) (*pb.ListResourcesResponse, error) {
	r, err := s.usecase.ListResources(ctx, biz.ListQuery{ClusterID: q.GetClusterId(), Type: domainType(q.GetType()), Namespace: q.GetNamespace(), AllNamespaces: q.GetAllNamespaces(), PageSize: q.GetPageSize(), ContinueToken: q.GetContinueToken(), Name: q.GetName(), LabelSelector: q.GetLabelSelector(), FieldSelector: q.GetFieldSelector()})
	if err != nil {
		return nil, serviceError(err)
	}
	out := &pb.ListResourcesResponse{NextContinueToken: r.NextContinueToken, ListResourceVersion: r.ListResourceVersion}
	for _, d := range r.Items {
		out.Items = append(out.Items, protoDocument(d))
	}
	return out, nil
}
func (s *Service) GetResource(ctx context.Context, q *pb.GetResourceRequest) (*pb.GetResourceResponse, error) {
	r, err := s.usecase.GetResource(ctx, biz.GetQuery{Ref: domainRef(q.GetRef()), IncludeSecretData: q.GetIncludeSecretData()})
	if err != nil {
		return nil, serviceError(err)
	}
	return &pb.GetResourceResponse{Resource: protoDocument(r.Resource), ManifestYaml: r.ManifestYAML}, nil
}
func (s *Service) CreateResource(ctx context.Context, q *pb.CreateResourceRequest) (*pb.MutationResponse, error) {
	r, err := s.usecase.CreateResource(ctx, biz.CreateCommand{Ref: domainRef(q.GetRef()), ManifestYAML: q.GetManifestYaml(), DryRun: q.GetDryRun()})
	if err != nil {
		return nil, serviceError(err)
	}
	return protoMutation(r), nil
}
func (s *Service) UpdateResource(ctx context.Context, q *pb.UpdateResourceRequest) (*pb.MutationResponse, error) {
	r, err := s.usecase.UpdateResource(ctx, biz.UpdateCommand{Ref: domainRef(q.GetRef()), ManifestYAML: q.GetManifestYaml(), ExpectedUID: q.GetExpectedUid(), ExpectedResourceVersion: q.GetExpectedResourceVersion(), DryRun: q.GetDryRun()})
	if err != nil {
		return nil, serviceError(err)
	}
	return protoMutation(r), nil
}
func (s *Service) DeleteResource(ctx context.Context, q *pb.DeleteResourceRequest) (*pb.DeleteResourceResponse, error) {
	r, err := s.usecase.DeleteResource(ctx, biz.DeleteCommand{Ref: domainRef(q.GetRef()), ExpectedUID: q.GetExpectedUid()})
	if err != nil {
		return nil, serviceError(err)
	}
	return &pb.DeleteResourceResponse{DeletionRequested: r.DeletionRequested, AlreadyAbsent: r.AlreadyAbsent}, nil
}
func (s *Service) ScaleWorkload(ctx context.Context, q *pb.ScaleWorkloadRequest) (*pb.ScaleWorkloadResponse, error) {
	var replicas *int32
	if q != nil {
		replicas = q.Replicas
	}
	r, err := s.usecase.ScaleWorkload(ctx, biz.ScaleCommand{Ref: domainRef(q.GetRef()), ExpectedUID: q.GetExpectedUid(), ExpectedResourceVersion: q.GetExpectedResourceVersion(), Replicas: replicas, DryRun: q.GetDryRun()})
	if err != nil {
		return nil, serviceError(err)
	}
	return &pb.ScaleWorkloadResponse{Ref: protoRef(r.Ref), Uid: r.UID, ResourceVersion: r.ResourceVersion, DesiredReplicas: r.DesiredReplicas, ObservedReplicas: r.ObservedReplicas, DryRun: r.DryRun}, nil
}
func (s *Service) RestartWorkload(ctx context.Context, q *pb.RestartWorkloadRequest) (*pb.MutationResponse, error) {
	r, err := s.usecase.RestartWorkload(ctx, biz.RestartCommand{Ref: domainRef(q.GetRef()), ExpectedUID: q.GetExpectedUid(), ExpectedResourceVersion: q.GetExpectedResourceVersion(), DryRun: q.GetDryRun()})
	if err != nil {
		return nil, serviceError(err)
	}
	return protoMutation(r), nil
}
func (s *Service) StreamPodLogs(q *pb.StreamPodLogsRequest, stream pb.PlatformClusterOpsService_StreamPodLogsServer) error {
	if q == nil || stream == nil {
		return serviceError(biz.NewError("INVALID_ARGUMENT"))
	}
	query := biz.PodLogQuery{ClusterID: q.GetClusterId(), Namespace: q.GetNamespace(), PodName: q.GetPodName(), Container: q.GetContainer(), ExpectedPodUID: q.GetExpectedPodUid(), Follow: q.GetFollow(), Previous: q.GetPrevious(), Timestamps: q.GetTimestamps(), TailLines: q.TailLines, SinceSeconds: q.SinceSeconds, LimitBytes: q.LimitBytes}
	if q.SinceTime != nil {
		if q.SinceTime.CheckValid() != nil {
			return serviceError(biz.NewError("INVALID_ARGUMENT"))
		}
		t := q.SinceTime.AsTime()
		query.SinceTime = &t
	}
	return serviceError(s.usecase.StreamPodLogs(stream.Context(), query, func(f biz.LogFrame) error {
		return stream.Send(&pb.PodLogFrame{Type: pb.PodLogFrameType(f.Type), Data: f.Data, PodUid: f.PodUID, Container: f.Container, Reason: f.Reason, Message: f.Message})
	}))
}
