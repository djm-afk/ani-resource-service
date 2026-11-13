package main

import (
	"context"
	"fmt"
	"time"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/compute"
	conf "github.com/zhangzhe-ctrl/ani-resource-service/internal/conf/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Explicit paths never fall back to the invoking user's default context.
func targetKubernetesConfig(path string) (*rest.Config, error) {
	if path == "" {
		return rest.InClusterConfig()
	}
	return clientcmd.BuildConfigFromFlags("", path)
}

// Each composition resolves once, before opening databases or starting writers.
func resolveTargetCluster(ctx context.Context, network *conf.Network) (*rest.Config, error) {
	if network == nil {
		return nil, fmt.Errorf("target cluster requires network configuration")
	}
	cfg, err := targetKubernetesConfig(network.Kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("target Kubernetes configuration: %w", err)
	}
	uid, err := readTargetClusterUID(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if network.ClusterId != "" && network.ClusterId != uid {
		return nil, fmt.Errorf("network.cluster_id %q differs from target kube-system UID %q", network.ClusterId, uid)
	}
	network.ClusterId = uid
	return cfg, nil
}

func readTargetClusterUID(ctx context.Context, cfg *rest.Config) (string, error) {
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	copy := rest.CopyConfig(cfg)
	copy.Timeout = 10 * time.Second
	client, err := kubernetes.NewForConfig(copy)
	if err != nil {
		return "", fmt.Errorf("target Kubernetes client: %w", err)
	}
	ns, err := client.CoreV1().Namespaces().Get(bounded, "kube-system", metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("target identity requires get core namespaces/kube-system: %w", err)
	}
	uid := string(ns.UID)
	if !biz.UUID(uid) {
		return "", fmt.Errorf("target kube-system metadata.uid is not a canonical UUID")
	}
	return uid, nil
}

// Callers change only their private copy's timeouts and rate budgets.
func resolvedKubernetesConfig(network *conf.Network, resolved ...*rest.Config) (*rest.Config, error) {
	if len(resolved) > 1 {
		return nil, fmt.Errorf("one resolved Kubernetes target is required")
	}
	if len(resolved) == 1 {
		if resolved[0] == nil {
			return nil, fmt.Errorf("resolved Kubernetes target is missing")
		}
		return rest.CopyConfig(resolved[0]), nil
	}
	if network == nil {
		return nil, fmt.Errorf("network Kubernetes configuration is required")
	}
	return targetKubernetesConfig(network.Kubeconfig)
}

func resolvePullSecretTarget(ctx context.Context, image *conf.Image, network *conf.Network, target *rest.Config) (*rest.Config, error) {
	return resolvePullSecretConnection(ctx, image, network, target, targetKubernetesConfig)
}

// The loader preserves each domain's connection credentials, including Image's
// empty-path in-cluster ServiceAccount contract.
func resolvePullSecretConnection(ctx context.Context, image *conf.Image, network *conf.Network, target *rest.Config, load func(string) (*rest.Config, error)) (*rest.Config, error) {
	if image == nil || !image.Enabled || image.PullSecretClusterId == "" {
		return nil, nil
	}
	if image.PullSecretClusterId != network.ClusterId {
		return nil, fmt.Errorf("image.pull_secret_cluster_id differs from target kube-system UID")
	}
	if image.PullSecretKubeconfig == network.Kubeconfig {
		return rest.CopyConfig(target), nil
	}
	cfg, err := load(image.PullSecretKubeconfig)
	if err != nil {
		return nil, fmt.Errorf("image pull secret Kubernetes configuration: %w", err)
	}
	uid, err := readTargetClusterUID(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if uid != network.ClusterId {
		return nil, fmt.Errorf("image pull secret connection points to a different kube-system UID")
	}
	return cfg, nil
}
