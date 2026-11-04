package data

import (
	"context"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func (p *KCProvider) observePlatformNetwork(ctx context.Context, t biz.ProviderTarget) (biz.ProviderObservation, error) {
	spec := t.PlatformNetwork
	if spec == nil || spec.UID == "" || spec.Namespace == "" || spec.Name == "" || spec.ClusterID != p.repository.placement.ClusterID {
		return biz.ProviderObservation{}, &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	gvr, kind := kcVPCs, "VPC"
	if spec.Kind == "subnet" {
		gvr, kind = kcSubnets, "Subnet"
	} else if spec.Kind != "vpc" {
		return biz.ProviderObservation{}, &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	object, err := p.client.Resource(gvr).Namespace(spec.Namespace).Get(ctx, spec.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return biz.ProviderObservation{}, nil
	}
	if err != nil {
		return biz.ProviderObservation{}, readFailure(err)
	}
	if object.GetKind() != kind || object.GetAPIVersion() != "networking.kubercloud.com/v1" || object.GetName() != spec.Name || object.GetNamespace() != spec.Namespace || string(object.GetUID()) != spec.UID {
		return biz.ProviderObservation{}, &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	ready, reason := primaryCondition(object, "Ready")
	facts := &biz.PlatformNetworkFacts{CIDR: crString(object, "spec", "cidrBlock")}
	if spec.Kind == "subnet" {
		facts.Gateway = crString(object, "spec", "gatewayIP")
		facts.ExcludeIPs, _, _ = unstructured.NestedStringSlice(object.Object, "spec", "excludeIPs")
		facts.DNSServers, _, _ = unstructured.NestedStringSlice(object.Object, "spec", "dnsServers")
		facts.EnableDHCP, _, _ = unstructured.NestedBool(object.Object, "spec", "enableDHCP")
		facts.Available = subnetIPRange(object, "v4availableIPrange")
		facts.Used = subnetIPRange(object, "v4usingIPrange")
	}
	return biz.ProviderObservation{Exists: true, Identity: spec.UID, Ready: ready, StatusReason: reason, PlatformNetwork: facts}, nil
}
