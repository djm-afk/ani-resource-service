// Package clusterops implements administrator operations on native resources.
package clusterops

import (
	"io"
	"time"
)

const (
	UnaryTimeout      = 30 * time.Second
	KubernetesTimeout = 15 * time.Second
	NamespaceTimeout  = 2 * time.Second
)

type Cluster struct{ ClusterID, DisplayName string }
type ResourceTypeRef struct{ Group, Version, Resource string }
type ResourceDescriptor struct {
	Type           ResourceTypeRef
	Kind           string
	Namespaced     bool
	Actions        []string
	CustomResource bool
	CRDName        string
}
type ResourceRef struct {
	ClusterID       string
	Type            ResourceTypeRef
	Namespace, Name string
}
type RawResource struct {
	Ref                  ResourceRef
	UID, ResourceVersion string
	JSON, DisplayJSON    []byte
	ManifestYAML         string
	SecretKeys           []string
}
type ResourceDocument struct {
	Ref                  ResourceRef
	UID, ResourceVersion string
	ObjectJSON           []byte
	ReadOnly             bool
	ReadOnlyReason       string
	SecretDataRedacted   bool
	SecretKeys           []string
}
type ResourcePage struct {
	// Raw is adapter-only input to the usecase. Services only return Items.
	Raw                                    []RawResource
	Items                                  []ResourceDocument
	NextContinueToken, ListResourceVersion string
}
type ListQuery struct {
	ClusterID                                         string
	Type                                              ResourceTypeRef
	Namespace                                         string
	AllNamespaces                                     bool
	PageSize                                          uint32
	ContinueToken, Name, LabelSelector, FieldSelector string
}
type GetQuery struct {
	Ref               ResourceRef
	IncludeSecretData bool
}
type ResourceDetail struct {
	Resource     ResourceDocument
	ManifestYAML string
}
type CreateCommand struct {
	Ref          ResourceRef
	ManifestYAML string
	DryRun       bool
}
type UpdateCommand struct {
	Ref                                                ResourceRef
	ManifestYAML, ExpectedUID, ExpectedResourceVersion string
	DryRun                                             bool
}
type DeleteCommand struct {
	Ref         ResourceRef
	ExpectedUID string
}
type DeleteResult struct{ DeletionRequested, AlreadyAbsent bool }
type MutationResult struct {
	Resource ResourceDocument
	DryRun   bool
}
type ScaleCommand struct {
	Ref                                  ResourceRef
	ExpectedUID, ExpectedResourceVersion string
	Replicas                             *int32
	DryRun                               bool
}
type ScaleResult struct {
	Ref                               ResourceRef
	UID, ResourceVersion              string
	DesiredReplicas, ObservedReplicas int32
	DryRun                            bool
}
type RestartCommand struct {
	Ref                                  ResourceRef
	ExpectedUID, ExpectedResourceVersion string
	DryRun                               bool
}
type ClusterOverview struct {
	ClusterID, KubernetesVersion                       string
	NodesTotal, NodesReady, NamespacesTotal, PodsTotal int64
	PodPhaseCounts                                     map[string]int64
	Capacity, Allocatable                              map[string]string
	SampledAt                                          time.Time
}
type CRDVersion struct {
	Name            string
	Served, Storage bool
}
type CRDReadInfo struct {
	UID, Scope, Group, Plural string
	Versions                  []CRDVersion
}
type PodLogQuery struct {
	ClusterID, Namespace, PodName, Container, ExpectedPodUID string
	Follow, Previous, Timestamps                             bool
	TailLines, SinceSeconds, LimitBytes                      *int64
	SinceTime                                                *time.Time
}
type OpenedPodLog struct {
	Reader            io.ReadCloser
	PodUID, Container string
}
type LogFrameType int

const (
	LogReady LogFrameType = iota + 1
	LogData
	LogEnd
	LogError
)

type LogFrame struct {
	Type                               LogFrameType
	Data                               []byte
	PodUID, Container, Reason, Message string
}
