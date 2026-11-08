package clusterops

import "context"

type NamespaceSource interface {
	ListRegisteredNamespaceNames(context.Context, string) ([]string, error)
}
type KubernetesPort interface {
	ListResourceTypes(context.Context, string) ([]ResourceDescriptor, error)
	ResolveResourceType(context.Context, string, ResourceTypeRef) (ResourceDescriptor, error)
	GetCRDReadInfo(context.Context, ResourceRef) (CRDReadInfo, error)
	GetClusterOverview(context.Context, string) (ClusterOverview, error)
	ListResources(context.Context, ListQuery) (ResourcePage, error)
	GetResource(context.Context, ResourceRef) (RawResource, error)
	CreateResource(context.Context, CreateCommand) (RawResource, error)
	UpdateResource(context.Context, UpdateCommand) (RawResource, error)
	DeleteResource(context.Context, ResourceRef, string) (DeleteResult, error)
	ScaleWorkload(context.Context, ScaleCommand) (ScaleResult, error)
	RestartWorkload(context.Context, RestartCommand) (RawResource, error)
	OpenPodLogs(context.Context, PodLogQuery) (OpenedPodLog, error)
}

// UnavailableNamespaceSource represents static configuration failures only.
type UnavailableNamespaceSource struct{}

func (UnavailableNamespaceSource) ListRegisteredNamespaceNames(context.Context, string) ([]string, error) {
	return nil, NewError("NAMESPACE_POLICY_UNAVAILABLE")
}
