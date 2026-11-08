package clusterops

import (
	"context"
	"encoding/json"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/clusterops"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"time"
)

func (k *Kubernetes) ScaleWorkload(ctx context.Context, c biz.ScaleCommand) (biz.ScaleResult, error) {
	get := func(ctx context.Context) (*autoscalingv1.Scale, error) {
		if c.Ref.Type.Resource == "deployments" {
			return k.typed.AppsV1().Deployments(c.Ref.Namespace).GetScale(ctx, c.Ref.Name, metav1.GetOptions{})
		}
		return k.typed.AppsV1().StatefulSets(c.Ref.Namespace).GetScale(ctx, c.Ref.Name, metav1.GetOptions{})
	}
	scale, err := apiCall(ctx, get)
	if err != nil {
		return biz.ScaleResult{}, err
	}
	if err = checkIdentity(scale.UID, scale.ResourceVersion, c.ExpectedUID, c.ExpectedResourceVersion); err != nil {
		return biz.ScaleResult{}, err
	}
	scale.Spec.Replicas = *c.Replicas
	options := metav1.UpdateOptions{FieldManager: fieldManager, FieldValidation: "Strict", DryRun: dryRun(c.DryRun)}
	saved, err := apiCall(ctx, func(ctx context.Context) (*autoscalingv1.Scale, error) {
		if c.Ref.Type.Resource == "deployments" {
			return k.typed.AppsV1().Deployments(c.Ref.Namespace).UpdateScale(ctx, c.Ref.Name, scale, options)
		}
		return k.typed.AppsV1().StatefulSets(c.Ref.Namespace).UpdateScale(ctx, c.Ref.Name, scale, options)
	})
	if err != nil {
		return biz.ScaleResult{}, err
	}
	return biz.ScaleResult{Ref: c.Ref, UID: string(saved.UID), ResourceVersion: saved.ResourceVersion, DesiredReplicas: saved.Spec.Replicas, ObservedReplicas: saved.Status.Replicas, DryRun: c.DryRun}, nil
}
func (k *Kubernetes) RestartWorkload(ctx context.Context, c biz.RestartCommand) (biz.RawResource, error) {
	obj, err := k.get(ctx, c.Ref)
	if err != nil {
		return biz.RawResource{}, err
	}
	if err = checkIdentity(obj.GetUID(), obj.GetResourceVersion(), c.ExpectedUID, c.ExpectedResourceVersion); err != nil {
		return biz.RawResource{}, err
	}
	paused, _, _ := unstructured.NestedBool(obj.Object, "spec", "paused")
	strategy, _, _ := unstructured.NestedString(obj.Object, "spec", "updateStrategy", "type")
	partition, _, _ := unstructured.NestedInt64(obj.Object, "spec", "updateStrategy", "rollingUpdate", "partition")
	if (c.Ref.Type.Resource == "deployments" && paused) || strategy == "OnDelete" || (c.Ref.Type.Resource == "statefulsets" && partition > 0) {
		return biz.RawResource{}, biz.NewError("RESTART_STRATEGY_UNSUPPORTED")
	}
	patch, _ := json.Marshal(map[string]any{
		"metadata": map[string]any{"uid": c.ExpectedUID, "resourceVersion": c.ExpectedResourceVersion},
		"spec":     map[string]any{"template": map[string]any{"metadata": map[string]any{"annotations": map[string]any{"kubectl.kubernetes.io/restartedAt": time.Now().UTC().Format(time.RFC3339Nano)}}}},
	})
	saved, err := apiCall(ctx, func(ctx context.Context) (*unstructured.Unstructured, error) {
		return k.resource(c.Ref).Patch(ctx, c.Ref.Name, types.MergePatchType, patch, metav1.PatchOptions{FieldManager: fieldManager, FieldValidation: "Strict", DryRun: dryRun(c.DryRun)})
	})
	if err != nil {
		return biz.RawResource{}, err
	}
	return project(c.Ref, saved)
}
