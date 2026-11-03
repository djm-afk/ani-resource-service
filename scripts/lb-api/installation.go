package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
)

// This read-only input freeze matches the observer's canonical bundle. It
// captures expectation material, not a permission/readiness/traffic verdict.
// Historical defaults stay fenced; a new task must supply its verified identity.
const installationContext = "kubernetes-admin@ani-platform"
const installationClusterUID = "be57b911-892c-4e75-aa9d-4a05d819c59e"

func installation(args []string) error {
	flags := flag.NewFlagSet("installation", flag.ContinueOnError)
	kubeconfig := flags.String("kubeconfig", "", "existing kubeconfig path")
	output := flags.String("output", "", "new evidence JSON path (never overwritten)")
	expectedContext := flags.String("expected-context", installationContext, "verified target context; never changed")
	expectedUID := flags.String("expected-cluster-uid", installationClusterUID, "verified kube-system UID")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *kubeconfig == "" || *output == "" {
		return errors.New("kubeconfig and output are required")
	}
	loaded, err := clientcmd.LoadFromFile(*kubeconfig)
	if err != nil || loaded.CurrentContext != *expectedContext {
		return fmt.Errorf("expected current context %s; context is never changed", *expectedContext)
	}
	cfg, err := clientcmd.NewNonInteractiveClientConfig(*loaded, *expectedContext, &clientcmd.ConfigOverrides{}, nil).ClientConfig()
	if err != nil {
		return errors.New("cannot load fixed context")
	}
	cfg.QPS, cfg.Burst = 5, 10
	client, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return errors.New("cannot open Kubernetes client")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ns, err := client.Resource(schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}).Get(ctx, "kube-system", metav1.GetOptions{})
	if err != nil {
		return errors.New("cannot read cluster identity")
	}
	if *expectedUID == "" || string(ns.GetUID()) != *expectedUID {
		return errors.New("cluster UID differs from the verified task input")
	}
	bundle := map[string]any{}
	records := []map[string]any{}
	targets := []struct{ group, version, resource, namespace, name string }{
		{"", "v1", "configmaps", "envoy-gateway-system", "envoy-gateway-config"},
	}
	targets = append(targets,
		struct{ group, version, resource, namespace, name string }{"gateway.networking.k8s.io", "v1", "gatewayclasses", "", biz.LoadBalancerGatewayClass},
		struct{ group, version, resource, namespace, name string }{"gateway.envoyproxy.io", "v1alpha1", "envoyproxies", "envoy-gateway-system", biz.LoadBalancerGatewayClass})
	for _, name := range []string{"gateways.gateway.networking.k8s.io", "httproutes.gateway.networking.k8s.io", "backends.gateway.envoyproxy.io", "backendtrafficpolicies.gateway.envoyproxy.io", "envoyproxies.gateway.envoyproxy.io"} {
		targets = append(targets, struct{ group, version, resource, namespace, name string }{"apiextensions.k8s.io", "v1", "customresourcedefinitions", "", name})
	}
	for _, target := range targets {
		endpoint := client.Resource(schema.GroupVersionResource{Group: target.group, Version: target.version, Resource: target.resource})
		var resource dynamic.ResourceInterface = endpoint
		if target.namespace != "" {
			resource = endpoint.Namespace(target.namespace)
		}
		obj, err := resource.Get(ctx, target.name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("cannot read installation object %s/%s/%s", target.resource, target.namespace, target.name)
		}
		content := obj.Object["spec"]
		if target.resource == "configmaps" {
			content = obj.Object["data"]
		}
		key := target.resource + "/" + target.namespace + "/" + target.name
		bundle[key] = map[string]any{"uid": string(obj.GetUID()), "content": content}
		records = append(records, map[string]any{"key": key, "resource_version": obj.GetResourceVersion(), "generation": obj.GetGeneration(), "status": obj.Object["status"]})
	}
	canonical, err := json.Marshal(bundle)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(canonical)
	fingerprint := hex.EncodeToString(sum[:])
	payload := map[string]any{"mode": "read_only_expectation_material", "observed_at": time.Now().UTC(), "context": *expectedContext, "api_server": cfg.Host, "cluster_uid": string(ns.GetUID()), "fingerprint": fingerprint, "bundle": bundle, "records": records, "readiness": "not_verified", "source_to_image_link": "not_verified"}
	file, err := os.OpenFile(*output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.New("cannot create new installation evidence; no existing file was overwritten")
	}
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	err = encoder.Encode(payload)
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]string{"fingerprint": fingerprint, "evidence": *output, "mode": "read_only_expectation_material"})
}
