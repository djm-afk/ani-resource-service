package clusterops

import (
	"context"
	"errors"
	"regexp"
	"strings"
)

type UseCase struct {
	clusterID   string
	displayName string
	kube        KubernetesPort
	policy      *NamespacePolicy
	logSlots    chan struct{}
	logBudget   logBudget
}

func NewUseCase(clusterID string, kube KubernetesPort, policy *NamespacePolicy, displayName ...string) (*UseCase, error) {
	if strings.TrimSpace(clusterID) == "" || kube == nil || policy == nil {
		return nil, NewError("INVALID_ARGUMENT")
	}
	name := clusterID
	if len(displayName) > 1 {
		return nil, NewError("INVALID_ARGUMENT")
	}
	if len(displayName) == 1 && strings.TrimSpace(displayName[0]) != "" {
		name = strings.TrimSpace(displayName[0])
	}
	return &UseCase{displayName: name, clusterID: clusterID, kube: kube, policy: policy, logSlots: make(chan struct{}, 100), logBudget: productionLogBudget}, nil
}
func (u *UseCase) cluster(ctx context.Context, id string) error {
	if err := RequirePlatform(ctx); err != nil {
		return err
	}
	if id == "" {
		return NewError("INVALID_ARGUMENT")
	}
	if id != u.clusterID {
		return NewError("CLUSTER_NOT_CONFIGURED")
	}
	return nil
}

var apiSegment = regexp.MustCompile("^[a-zA-Z0-9][a-zA-Z0-9.-]*$")

func validResourceName(v string) bool {
	if v == "" || v == "." || v == ".." || strings.ContainsAny(v, "/\\%?#") {
		return false
	}
	for _, r := range v {
		if r < 32 || r == 127 {
			return false
		}
	}
	return true
}
func validSegment(v string) bool {
	return v != "" && v != "." && v != ".." && len(v) <= 253 && apiSegment.MatchString(v)
}
func (u *UseCase) descriptor(ctx context.Context, ref ResourceRef, single bool) (ResourceDescriptor, error) {
	if err := u.cluster(ctx, ref.ClusterID); err != nil {
		return ResourceDescriptor{}, err
	}
	if !validSegment(ref.Type.Version) || !validSegment(ref.Type.Resource) || (ref.Type.Group != "" && !validSegment(ref.Type.Group)) || (single && !validResourceName(ref.Name)) {
		return ResourceDescriptor{}, NewError("INVALID_ARGUMENT")
	}
	d, err := u.kube.ResolveResourceType(ctx, ref.ClusterID, ref.Type)
	if err != nil {
		return d, err
	}
	if (d.Namespaced && single && !validNamespace(ref.Namespace)) || (!d.Namespaced && ref.Namespace != "") || (ref.Namespace != "" && !validNamespace(ref.Namespace)) {
		return d, NewError("INVALID_ARGUMENT")
	}
	return d, nil
}
func hasAction(d ResourceDescriptor, action string) bool {
	for _, a := range d.Actions {
		if a == action {
			return true
		}
	}
	return false
}
func (u *UseCase) write(ctx context.Context, ref ResourceRef, action string) (ResourceDescriptor, error) {
	d, err := u.descriptor(ctx, ref, true)
	if err != nil {
		return d, err
	}
	if !hasAction(d, action) {
		if action == "scale" || action == "restart" {
			return d, NewError("ACTION_NOT_SUPPORTED")
		}
		if !hasAction(d, "create") && !hasAction(d, "update") && !hasAction(d, "delete") {
			return d, NewError("RESOURCE_READ_ONLY")
		}
		return d, NewError("ACTION_NOT_SUPPORTED")
	}
	if needsNamespace(d) {
		protected, err := u.policy.ProtectedNamespaces(ctx, ref.ClusterID)
		if err != nil {
			return d, err
		}
		if err = RequireNamespaceWritable(protected, d, ref); err != nil {
			return d, err
		}
	}
	return d, ctx.Err()
}
func (u *UseCase) ListClusters(ctx context.Context) ([]Cluster, error) {
	if err := RequirePlatform(ctx); err != nil {
		return nil, err
	}
	return []Cluster{{ClusterID: u.clusterID, DisplayName: u.displayName}}, nil
}
func (u *UseCase) ListResourceTypes(ctx context.Context, cluster string) ([]ResourceDescriptor, error) {
	if err := u.cluster(ctx, cluster); err != nil {
		return nil, err
	}
	return u.kube.ListResourceTypes(ctx, cluster)
}
func (u *UseCase) GetClusterOverview(ctx context.Context, cluster string) (ClusterOverview, error) {
	if err := u.cluster(ctx, cluster); err != nil {
		return ClusterOverview{}, err
	}
	return u.kube.GetClusterOverview(ctx, cluster)
}
func document(r RawResource, d ResourceDescriptor, protected map[string]struct{}, policyError error, secretData bool) ResourceDocument {
	out := ResourceDocument{Ref: r.Ref, UID: r.UID, ResourceVersion: r.ResourceVersion, ObjectJSON: r.DisplayJSON, SecretKeys: r.SecretKeys}
	secret := r.Ref.Type.Group == "" && r.Ref.Type.Resource == "secrets"
	out.SecretDataRedacted = secret && !secretData
	if secret && secretData {
		out.ObjectJSON = r.JSON
	}
	if !hasAction(d, "create") && !hasAction(d, "update") && !hasAction(d, "delete") {
		out.ReadOnly = true
		out.ReadOnlyReason = "RESOURCE_READ_ONLY"
	} else if needsNamespace(d) {
		if policyError != nil {
			out.ReadOnly = true
			out.ReadOnlyReason = "NAMESPACE_POLICY_UNAVAILABLE"
		} else if err := RequireNamespaceWritable(protected, d, r.Ref); err != nil {
			out.ReadOnly = true
			out.ReadOnlyReason = "TENANT_NAMESPACE_READ_ONLY"
		}
	}
	return out
}
func (u *UseCase) readPolicy(ctx context.Context, d ResourceDescriptor, cluster string) (map[string]struct{}, error) {
	if needsNamespace(d) {
		return u.policy.ProtectedNamespaces(ctx, cluster)
	}
	return nil, nil
}
func (u *UseCase) ListResources(ctx context.Context, q ListQuery) (ResourcePage, error) {
	d, err := u.descriptor(ctx, ResourceRef{ClusterID: q.ClusterID, Type: q.Type, Namespace: q.Namespace}, false)
	if err != nil {
		return ResourcePage{}, err
	}
	if (d.Namespaced && (q.AllNamespaces != (q.Namespace == ""))) || (!d.Namespaced && q.AllNamespaces) || q.PageSize > 200 || (q.Name != "" && !validResourceName(q.Name)) {
		return ResourcePage{}, NewError("INVALID_ARGUMENT")
	}
	if q.PageSize == 0 {
		q.PageSize = 50
	}
	protected, pe := u.readPolicy(ctx, d, q.ClusterID)
	if ctx.Err() != nil {
		return ResourcePage{}, ctx.Err()
	}
	page, err := u.kube.ListResources(ctx, q)
	if err != nil {
		return ResourcePage{}, err
	}
	for _, r := range page.Raw {
		page.Items = append(page.Items, document(r, d, protected, pe, false))
	}
	page.Raw = nil
	return page, nil
}
func (u *UseCase) GetResource(ctx context.Context, q GetQuery) (ResourceDetail, error) {
	d, err := u.descriptor(ctx, q.Ref, true)
	if err != nil {
		return ResourceDetail{}, err
	}
	protected, pe := u.readPolicy(ctx, d, q.Ref.ClusterID)
	if ctx.Err() != nil {
		return ResourceDetail{}, ctx.Err()
	}
	r, err := u.kube.GetResource(ctx, q.Ref)
	if err != nil {
		return ResourceDetail{}, err
	}
	out := ResourceDetail{Resource: document(r, d, protected, pe, q.IncludeSecretData), ManifestYAML: r.ManifestYAML}
	if out.Resource.SecretDataRedacted {
		out.ManifestYAML = ""
	}
	return out, nil
}
func (u *UseCase) CreateResource(ctx context.Context, c CreateCommand) (MutationResult, error) {
	d, err := u.write(ctx, c.Ref, "create")
	if err != nil {
		return MutationResult{}, err
	}
	r, err := u.kube.CreateResource(ctx, c)
	if err != nil {
		return MutationResult{}, err
	}
	return MutationResult{Resource: document(r, d, nil, nil, false), DryRun: c.DryRun}, nil
}
func (u *UseCase) UpdateResource(ctx context.Context, c UpdateCommand) (MutationResult, error) {
	if err := RequirePlatform(ctx); err != nil {
		return MutationResult{}, err
	}
	if c.ExpectedUID == "" || c.ExpectedResourceVersion == "" {
		return MutationResult{}, NewError("INVALID_ARGUMENT")
	}
	d, err := u.write(ctx, c.Ref, "update")
	if err != nil {
		return MutationResult{}, err
	}
	r, err := u.kube.UpdateResource(ctx, c)
	if err != nil {
		return MutationResult{}, err
	}
	return MutationResult{Resource: document(r, d, nil, nil, false), DryRun: c.DryRun}, nil
}
func (u *UseCase) DeleteResource(ctx context.Context, c DeleteCommand) (DeleteResult, error) {
	if err := RequirePlatform(ctx); err != nil {
		return DeleteResult{}, err
	}
	if c.ExpectedUID == "" {
		return DeleteResult{}, NewError("INVALID_ARGUMENT")
	}
	_, err := u.write(ctx, c.Ref, "delete")
	if err != nil {
		return DeleteResult{}, err
	}
	isCRD := c.Ref.Type.Group == "apiextensions.k8s.io" && c.Ref.Type.Resource == "customresourcedefinitions"
	if isCRD {
		info, readErr := u.kube.GetCRDReadInfo(ctx, c.Ref)
		if readErr != nil {
			var e *Error
			if errors.As(readErr, &e) && e.Reason == "RESOURCE_NOT_FOUND" {
				return DeleteResult{AlreadyAbsent: true}, nil
			}
			return DeleteResult{}, readErr
		}
		if info.UID != c.ExpectedUID {
			return DeleteResult{}, NewError("RESOURCE_UID_CHANGED")
		}
		if err = u.checkCRDDeleteNamespaces(ctx, c.Ref, info); err != nil {
			return DeleteResult{}, err
		}
	}
	if err = ctx.Err(); err != nil {
		return DeleteResult{}, err
	}
	result, err := u.kube.DeleteResource(ctx, c.Ref, c.ExpectedUID)
	if err == nil && isCRD && result.AlreadyAbsent {
		// Once the initial GET found this CRD, a later absence is an error,
		// never a successful result from an incomplete preflight or race.
		return DeleteResult{}, NewError("RESOURCE_NOT_FOUND")
	}
	return result, err
}
func (u *UseCase) checkCRDDeleteNamespaces(ctx context.Context, ref ResourceRef, info CRDReadInfo) error {
	if info.Scope == "Cluster" {
		return nil
	}
	if info.Scope != "Namespaced" {
		return NewError("CRD_INSPECTION_FAILED")
	}
	protected, err := u.policy.ProtectedNamespaces(ctx, ref.ClusterID)
	if err != nil {
		return err
	}
	version := ""
	for _, v := range info.Versions {
		if v.Served {
			if version == "" || v.Storage {
				version = v.Name
			}
			if v.Storage {
				break
			}
		}
	}
	if version == "" {
		return NewError("CRD_INSPECTION_FAILED")
	}
	q := ListQuery{ClusterID: ref.ClusterID, Type: ResourceTypeRef{Group: info.Group, Version: version, Resource: info.Plural}, AllNamespaces: true, PageSize: 200}
	for {
		if err = ctx.Err(); err != nil {
			return err
		}
		page, err := u.kube.ListResources(ctx, q)
		if err != nil {
			return err
		}
		for _, r := range page.Raw {
			if !validNamespace(r.Ref.Namespace) {
				return NewError("CRD_INSPECTION_FAILED")
			}
			if _, ok := protected[r.Ref.Namespace]; ok {
				return NewError("CRD_HAS_PROTECTED_NAMESPACE_INSTANCES")
			}
		}
		if page.NextContinueToken == "" {
			return ctx.Err()
		}
		q.ContinueToken = page.NextContinueToken
	}
}
func (u *UseCase) ScaleWorkload(ctx context.Context, c ScaleCommand) (ScaleResult, error) {
	if err := RequirePlatform(ctx); err != nil {
		return ScaleResult{}, err
	}
	if c.ExpectedUID == "" || c.ExpectedResourceVersion == "" || c.Replicas == nil || *c.Replicas < 0 {
		return ScaleResult{}, NewError("INVALID_ARGUMENT")
	}
	_, err := u.write(ctx, c.Ref, "scale")
	if err != nil {
		return ScaleResult{}, err
	}
	return u.kube.ScaleWorkload(ctx, c)
}
func (u *UseCase) RestartWorkload(ctx context.Context, c RestartCommand) (MutationResult, error) {
	if err := RequirePlatform(ctx); err != nil {
		return MutationResult{}, err
	}
	if c.ExpectedUID == "" || c.ExpectedResourceVersion == "" {
		return MutationResult{}, NewError("INVALID_ARGUMENT")
	}
	d, err := u.write(ctx, c.Ref, "restart")
	if err != nil {
		return MutationResult{}, err
	}
	r, err := u.kube.RestartWorkload(ctx, c)
	if err != nil {
		return MutationResult{}, err
	}
	return MutationResult{Resource: document(r, d, nil, nil, false), DryRun: c.DryRun}, nil
}
