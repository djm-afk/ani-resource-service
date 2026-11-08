package clusterops

import (
	"context"
	"errors"
	pb "github.com/zhangzhe-ctrl/ani-resource-service/api/clusterops/v1"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/clusterops"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"strings"
	"testing"
)

type emptySource struct{}

func (emptySource) ListRegisteredNamespaceNames(context.Context, string) ([]string, error) {
	return nil, nil
}

type secretKube struct {
	biz.KubernetesPort
	raw biz.RawResource
}

func (k secretKube) ResolveResourceType(_ context.Context, _ string, t biz.ResourceTypeRef) (biz.ResourceDescriptor, error) {
	return biz.ResourceDescriptor{Type: t, Kind: "Secret", Namespaced: true, Actions: []string{"get", "list", "create", "update", "delete"}}, nil
}
func (k secretKube) GetResource(context.Context, biz.ResourceRef) (biz.RawResource, error) {
	return k.raw, nil
}
func (k secretKube) ListResources(context.Context, biz.ListQuery) (biz.ResourcePage, error) {
	return biz.ResourcePage{Raw: []biz.RawResource{k.raw}}, nil
}
func (k secretKube) CreateResource(context.Context, biz.CreateCommand) (biz.RawResource, error) {
	return k.raw, nil
}
func (k secretKube) UpdateResource(context.Context, biz.UpdateCommand) (biz.RawResource, error) {
	return k.raw, nil
}
func TestSecretOutletsAndExplicitEditing(t *testing.T) {
	ref := biz.ResourceRef{ClusterID: "cluster", Type: biz.ResourceTypeRef{Version: "v1", Resource: "secrets"}, Namespace: "platform", Name: "x"}
	raw := biz.RawResource{Ref: ref, UID: "uid", ResourceVersion: "rv", JSON: []byte("{\"data\":{\"x\":\"private-sentinel\"}}"), DisplayJSON: []byte("{}"), ManifestYAML: "data: {x: private-sentinel}", SecretKeys: []string{"x"}}
	uc, err := biz.NewUseCase("cluster", secretKube{raw: raw}, biz.NewNamespacePolicy(emptySource{}, emptySource{}))
	if err != nil {
		t.Fatal(err)
	}
	s := NewService(uc)
	ctx := biz.WithCaller(context.Background(), biz.Caller{Platform: true, Actor: "governance:user:7", DirectCaller: "ani-governance", RequestID: "r"})
	pr := protoRef(ref)
	out, err := s.GetResource(ctx, &pb.GetResourceRequest{Ref: pr})
	if err != nil || !out.Resource.SecretDataRedacted || out.ManifestYaml != "" || strings.Contains(out.Resource.ObjectJson, "sentinel") {
		t.Fatal(out, err)
	}
	out, err = s.GetResource(ctx, &pb.GetResourceRequest{Ref: pr, IncludeSecretData: true})
	if err != nil || out.Resource.SecretDataRedacted || !strings.Contains(out.ManifestYaml, "sentinel") {
		t.Fatal(out, err)
	}
	list, err := s.ListResources(ctx, &pb.ListResourcesRequest{ClusterId: "cluster", Type: pr.Type, Namespace: "platform"})
	if err != nil || !list.Items[0].SecretDataRedacted || strings.Contains(list.Items[0].ObjectJson, "sentinel") {
		t.Fatal(list, err)
	}
	created, err := s.CreateResource(ctx, &pb.CreateResourceRequest{Ref: pr, ManifestYaml: "private-sentinel"})
	if err != nil || !created.Resource.SecretDataRedacted || strings.Contains(created.Resource.ObjectJson, "sentinel") {
		t.Fatal(created, err)
	}
	updated, err := s.UpdateResource(ctx, &pb.UpdateResourceRequest{Ref: pr, ExpectedUid: "uid", ExpectedResourceVersion: "rv", ManifestYaml: "private-sentinel"})
	if err != nil || !updated.Resource.SecretDataRedacted || strings.Contains(updated.Resource.ObjectJson, "sentinel") {
		t.Fatal(updated, err)
	}
}
func TestStableErrorDetailsNeverExposeRawFailures(t *testing.T) {
	for reason, code := range errorCodes {
		err := serviceError(biz.NewError(reason))
		s := status.Convert(err)
		if s.Code() != code {
			t.Fatal(reason, s)
		}
		details := s.Details()
		if len(details) != 1 {
			t.Fatal(reason, details)
		}
		info, ok := details[0].(*errdetails.ErrorInfo)
		if !ok || info.Reason != reason || info.Domain != "clusterops.ani.io" {
			t.Fatal(details)
		}
	}
	if err := serviceError(errors.New("private-manifest-sentinel")); status.Code(err) != codes.Internal || strings.Contains(err.Error(), "sentinel") {
		t.Fatal(err)
	}
	for _, err := range []error{context.Canceled, context.DeadlineExceeded} {
		if status.Code(serviceError(err)) != status.Code(status.FromContextError(err).Err()) {
			t.Fatal(err)
		}
	}
}
