package data

import (
	"errors"
	"slices"
	"testing"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func subnetOptionsFixture(options *biz.SubnetOptions) (*unstructured.Unstructured, sqlcgen.NetworkProviderBinding, biz.ProviderTarget, string) {
	parent := "tenant-one/vpc-one"
	b := sqlcgen.NetworkProviderBinding{ResourceKind: "subnet", Namespace: "tenant-one", ProviderName: "subnet-one", ProviderUid: "original-subnet", BindingID: "original-binding"}
	target := biz.ProviderTarget{TenantID: "tenant-one", ResourceID: "subnet-one", Kind: "subnet", CIDR: "10.42.1.0/24", Gateway: "10.42.1.1", KnownIdentity: b.ProviderUid, SubnetOptions: options}
	o := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "networking.kubercloud.com/v1", "kind": "Subnet", "metadata": map[string]any{"name": b.ProviderName, "namespace": b.Namespace, "uid": b.ProviderUid, "resourceVersion": "1", "generation": int64(1), "labels": map[string]any{ownerLabel: "ani-network-service", tenantLabel: target.TenantID, resourceLabel: target.ResourceID, bindingLabel: b.BindingID}}, "spec": subnetProviderSpec(target, parent), "status": map[string]any{"observedGeneration": int64(1), "boundResources": map[string]any{"switch": "switch-one", "gatewayPort": "gateway-one"}, "conditions": []any{map[string]any{"type": "Valid", "status": "True"}, map[string]any{"type": "Initialized", "status": "True"}, map[string]any{"type": "Ready", "status": "True"}}}}}
	return o, b, target, parent
}

func TestKCSubnetOptionsCreationAndObservation(t *testing.T) {
	options := &biz.SubnetOptions{ExcludeIPs: []string{"10.42.1.5", "10.42.1.20..10.42.1.30"}, EnableDHCP: true, DNSServers: []string{"10.96.0.10", "1.1.1.1"}}
	o, b, target, parent := subnetOptionsFixture(options)
	exclusions, _, err := unstructured.NestedStringSlice(o.Object, "spec", "excludeIPs")
	if err != nil || !slices.Equal(exclusions, options.ExcludeIPs) {
		t.Fatal("accepted excluded addresses were not sent as KCN JSON strings", exclusions, err)
	}
	dhcp, _, _ := unstructured.NestedBool(o.Object, "spec", "enableDHCP")
	dns, _, err := unstructured.NestedStringSlice(o.Object, "spec", "dnsServers")
	if !dhcp || err != nil || !slices.Equal(dns, options.DNSServers) {
		t.Fatal("accepted DHCP/DNS were not sent to KCN", dhcp, dns, err)
	}
	observed, err := inspectResource(o, b, target, parent)
	if err != nil || !observed.Ready || observed.Identity != b.ProviderUid {
		t.Fatal("matching nonempty options were treated as an ownership conflict", observed, err)
	}
	_ = unstructured.SetNestedStringSlice(o.Object, []string{options.ExcludeIPs[1], options.ExcludeIPs[0]}, "spec", "excludeIPs")
	if observed, err = inspectResource(o, b, target, parent); err != nil || !observed.Ready {
		t.Fatal("exclude address ordering changed the accepted set", observed, err)
	}
}

func TestKCSubnetOptionsDefaultAndFences(t *testing.T) {
	for _, name := range []string{"legacy-default", "explicit-default", "exclude-drift", "dhcp-drift", "dns-drift", "dns-order", "malformed-exclusions", "malformed-dhcp", "malformed-dns", "replaced-uid", "foreign-tenant", "gateway-drift", "stale-generation"} {
		t.Run(name, func(t *testing.T) {
			var options *biz.SubnetOptions
			if name != "legacy-default" {
				options = &biz.SubnetOptions{}
			}
			if name == "dns-order" {
				options.DNSServers = []string{"10.96.0.10", "1.1.1.1"}
			}
			o, b, target, parent := subnetOptionsFixture(options)
			switch name {
			case "legacy-default":
				for _, field := range []string{"excludeIPs", "enableDHCP", "dnsServers"} {
					unstructured.RemoveNestedField(o.Object, "spec", field)
				}
			case "exclude-drift":
				_ = unstructured.SetNestedStringSlice(o.Object, []string{"10.42.1.50"}, "spec", "excludeIPs")
			case "dhcp-drift":
				_ = unstructured.SetNestedField(o.Object, true, "spec", "enableDHCP")
			case "dns-drift":
				_ = unstructured.SetNestedStringSlice(o.Object, []string{"8.8.8.8"}, "spec", "dnsServers")
			case "dns-order":
				_ = unstructured.SetNestedStringSlice(o.Object, []string{"1.1.1.1", "10.96.0.10"}, "spec", "dnsServers")
			case "malformed-exclusions":
				_ = unstructured.SetNestedField(o.Object, "10.42.1.50", "spec", "excludeIPs")
			case "malformed-dhcp":
				_ = unstructured.SetNestedField(o.Object, "true", "spec", "enableDHCP")
			case "malformed-dns":
				_ = unstructured.SetNestedField(o.Object, "8.8.8.8", "spec", "dnsServers")
			case "replaced-uid":
				o.SetUID("replacement-subnet")
			case "foreign-tenant":
				labels := o.GetLabels()
				labels[tenantLabel] = "foreign-tenant"
				o.SetLabels(labels)
			case "gateway-drift":
				_ = unstructured.SetNestedField(o.Object, "10.42.1.2", "spec", "gatewayIP")
			case "stale-generation":
				o.SetGeneration(2)
			}
			observed, err := inspectResource(o, b, target, parent)
			switch name {
			case "legacy-default", "explicit-default":
				if err != nil || !observed.Ready {
					t.Fatal("accepted empty defaults were changed", observed, err)
				}
			case "stale-generation":
				if err != nil || observed.Ready {
					t.Fatal("options bypassed the existing readiness generation fence", observed, err)
				}
			default:
				var failure *biz.ProviderError
				if !errors.As(err, &failure) || failure.Kind != biz.ProviderConflict || observed.Ready {
					t.Fatal("unaccepted options or identity were adopted", observed, err)
				}
			}
		})
	}
}
