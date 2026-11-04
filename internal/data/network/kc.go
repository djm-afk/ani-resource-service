package data

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/tenantnamespace"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

var kcVPCs = schema.GroupVersionResource{Group: "networking.kubercloud.com", Version: "v1", Resource: "vpcs"}
var kcSubnets = schema.GroupVersionResource{Group: "networking.kubercloud.com", Version: "v1", Resource: "subnets"}
var namespaces = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}

const ownerLabel = "network.ani.io/managed-by"
const tenantLabel = "network.ani.io/tenant-id"
const resourceLabel = "network.ani.io/resource-id"
const bindingLabel = "network.ani.io/binding-id"

// KCProvider implements only the fixed networking.kubercloud.com/v1 VPC
// contract. It does not import the kc repository or operate on OVN.
type KCProvider struct {
	namespaceIdentityReader func(context.Context, string) (tenantnamespace.Identity, error)
	vmAutoNetwork           *VMAutoNetworkConfig
	vmNetworks              map[string]ManagedVMNAD
	repository              *Postgres
	client                  dynamic.Interface
	watchClient             dynamic.Interface
	observation             *KCObservation
	io                      *providerIO
}

type KCClientPolicy struct {
	QPS   float32
	Burst int
}

func OpenKCProvider(repository *Postgres, kubeconfig string, policy ...KCClientPolicy) (*KCProvider, error) {
	var config *rest.Config
	var err error
	if kubeconfig == "" {
		config, err = rest.InClusterConfig()
	} else {
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	if err != nil {
		return nil, fmt.Errorf("Kubernetes credentials/configuration could not be loaded")
	}
	if len(policy) > 1 {
		return nil, fmt.Errorf("invalid Kubernetes request budget")
	}
	if len(policy) == 1 {
		config.QPS = policy[0].QPS
		config.Burst = policy[0].Burst
	}
	return NewKCProvider(repository, config)
}

func NewKCProvider(repository *Postgres, config *rest.Config) (*KCProvider, error) {
	if repository == nil || config == nil {
		return nil, fmt.Errorf("PostgreSQL mapping and Kubernetes configuration required")
	}
	copy := rest.CopyConfig(config)
	copy.Timeout = 10 * time.Second
	copy.UserAgent = "ani-network-service/net-05a"
	stats := &providerIO{values: map[string]float64{}}
	previousWrap := copy.WrapTransport
	copy.WrapTransport = func(rt http.RoundTripper) http.RoundTripper {
		if previousWrap != nil {
			rt = previousWrap(rt)
		}
		return measuredTransport{rt, stats}
	}
	client, err := dynamic.NewForConfig(copy)
	if err != nil {
		return nil, fmt.Errorf("Kubernetes client configuration invalid")
	}
	watchConfig := rest.CopyConfig(copy)
	watchConfig.Timeout = 0
	watchClient, err := dynamic.NewForConfig(watchConfig)
	if err != nil {
		return nil, fmt.Errorf("Kubernetes watch configuration invalid")
	}
	provider := &KCProvider{repository: repository, client: client, watchClient: watchClient, io: stats, namespaceIdentityReader: repository.tenantNamespaceIdentity}
	repository.ConfigureTenantNamespaceResolver(provider.resolveTenantNamespace)
	repository.networkProvider = provider
	return provider, nil
}

func (p *KCProvider) binding(ctx context.Context, target biz.ProviderTarget) (sqlcgen.NetworkProviderBinding, error) {
	value, err := p.repository.readQueries(ctx).GetBinding(ctx, sqlcgen.GetBindingParams{TenantID: target.TenantID, ResourceID: target.ResourceID})
	if err != nil {
		return value, &biz.ProviderError{Kind: biz.ProviderTemporary, Cause: err}
	}
	if (target.Kind != "" && target.Kind != value.ResourceKind) || value.BindingID != target.BindingID || value.ClusterID != p.repository.placement.ClusterID ||
		(value.ProviderUid != "" && target.KnownIdentity != "" && value.ProviderUid != target.KnownIdentity) {
		return value, &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	return value, nil
}

func (p *KCProvider) observeDirect(ctx context.Context, target biz.ProviderTarget) (biz.ProviderObservation, error) {
	binding, err := p.binding(ctx, target)
	if err != nil {
		return biz.ProviderObservation{}, err
	}
	object, err := p.client.Resource(kcResource(binding)).Namespace(binding.Namespace).Get(ctx, binding.ProviderName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return biz.ProviderObservation{}, nil
	}
	if err != nil {
		return biz.ProviderObservation{}, readFailure(err)
	}
	return p.inspect(ctx, object, binding, target)
}

// managedObjectExists identifies the original object owned by this binding.
// A replacement with the same name is never adopted. During deletion its
// presence does not extend the lifetime of the original UID.
func managedObjectExists(object *unstructured.Unstructured, binding sqlcgen.NetworkProviderBinding, target biz.ProviderTarget, kind string) (bool, error) {
	if object == nil {
		return false, &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	known := target.KnownIdentity
	if known == "" {
		known = binding.ProviderUid
	}
	if known != "" && string(object.GetUID()) != known {
		if target.Deleting {
			return false, nil
		}
		return false, &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	labels := object.GetLabels()
	if object.GetAPIVersion() != "networking.kubercloud.com/v1" || object.GetKind() != kind ||
		object.GetName() != binding.ProviderName || object.GetNamespace() != binding.Namespace || object.GetUID() == "" ||
		labels[ownerLabel] != "ani-network-service" || labels[tenantLabel] != target.TenantID ||
		labels[resourceLabel] != target.ResourceID || labels[bindingLabel] != binding.BindingID {
		return false, &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	return true, nil
}

// primaryCondition projects the controller's own result without reconstructing
// readiness from generations, subordinate conditions, or generated resources.
func primaryCondition(object *unstructured.Unstructured, conditionType string) (*bool, biz.Reason) {
	conditions, _, _ := unstructured.NestedSlice(object.Object, "status", "conditions")
	for _, raw := range conditions {
		condition, ok := raw.(map[string]any)
		if !ok || condition["type"] != conditionType {
			continue
		}
		reason, _ := condition["reason"].(string)
		switch condition["status"] {
		case "True":
			ready := true
			return &ready, biz.Reason(reason)
		case "False":
			ready := false
			return &ready, biz.Reason(reason)
		default:
			return nil, biz.Reason(reason)
		}
	}
	return nil, ""
}

func inspectResource(object *unstructured.Unstructured, binding sqlcgen.NetworkProviderBinding, target biz.ProviderTarget) (biz.ProviderObservation, error) {
	kind := "VPC"
	if binding.ResourceKind == "subnet" {
		kind = "Subnet"
	}
	exists, err := managedObjectExists(object, binding, target, kind)
	if err != nil || !exists {
		return biz.ProviderObservation{}, err
	}
	ready, reason := primaryCondition(object, "Ready")
	value := biz.ProviderObservation{Exists: true, Identity: string(object.GetUID()), Ready: ready, StatusReason: reason}
	if binding.ResourceKind == "subnet" {
		value.SubnetIPRanges.Available = subnetIPRange(object, "v4availableIPrange")
		value.SubnetIPRanges.Used = subnetIPRange(object, "v4usingIPrange")
	}
	return value, nil
}

// subnetIPRange preserves the provider's comma-separated IPv4 ranges verbatim.
// An empty string is a known empty set; a missing/non-string field is unknown.
func subnetIPRange(object *unstructured.Unstructured, field string) *string {
	value, found, err := unstructured.NestedString(object.Object, "status", field)
	if err != nil || !found {
		return nil
	}
	return &value
}

func subnetProviderSpec(target biz.ProviderTarget, parent string) map[string]any {
	options := biz.SubnetOptions{}
	if target.SubnetOptions != nil {
		options = *target.SubnetOptions
	}
	spec := map[string]any{"type": "VPC", "ipVersion": "IPv4", "cidrBlock": target.CIDR, "gateway": parent, "gatewayIP": target.Gateway, "allowedNamespaces": map[string]any{"from": "Same"}, "enableDHCP": options.EnableDHCP}
	// Unstructured requires JSON arrays, including when accepted defaults are empty.
	_ = unstructured.SetNestedStringSlice(spec, options.ExcludeIPs, "excludeIPs")
	_ = unstructured.SetNestedStringSlice(spec, options.DNSServers, "dnsServers")
	return spec
}

func (p *KCProvider) EnsureVPC(ctx context.Context, target biz.ProviderTarget) (biz.ProviderObservation, error) {
	target.Kind = "vpc"
	return p.ensure(ctx, target)
}
func (p *KCProvider) EnsureSubnet(ctx context.Context, target biz.ProviderTarget) (biz.ProviderObservation, error) {
	target.Kind = "subnet"
	return p.ensure(ctx, target)
}
func (p *KCProvider) ensure(ctx context.Context, target biz.ProviderTarget) (biz.ProviderObservation, error) {
	binding, err := p.binding(ctx, target)
	if err != nil {
		return biz.ProviderObservation{}, err
	}
	if err := p.verifyTenantNamespace(ctx, binding, true); err != nil {
		return biz.ProviderObservation{}, err
	}
	resource := p.client.Resource(kcResource(binding)).Namespace(binding.Namespace)
	existing, err := resource.Get(ctx, binding.ProviderName, metav1.GetOptions{})
	if err == nil {
		return p.inspect(ctx, existing, binding, target)
	}
	if !apierrors.IsNotFound(err) {
		return biz.ProviderObservation{}, readFailure(err)
	}
	if target.KnownIdentity != "" || binding.ProviderUid != "" {
		return biz.ProviderObservation{}, &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	if binding.ResourceKind == "vpc" {
		tx, err := p.repository.pool.Begin(ctx)
		if err != nil {
			return biz.ProviderObservation{}, readFailure(err)
		}
		defer tx.Rollback(ctx)
		q := p.repository.queries.WithTx(tx)
		if err = q.LockPlatformCluster(ctx, sqlcgen.LockPlatformClusterParams{ClusterID: binding.ClusterID}); err != nil {
			return biz.ProviderObservation{}, readFailure(err)
		}
		// Accepted requests survive later whitelist changes; accepted pool conflicts
		// are rechecked immediately before the first POST without node probes.
		if err = p.repository.validateVPCPlatformCIDR(ctx, q, target.CIDR); err != nil {
			return biz.ProviderObservation{}, &biz.ProviderError{Kind: biz.ProviderTemporary, Cause: err}
		}
	}
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "networking.kubercloud.com/v1", "kind": "VPC",
		"metadata": map[string]any{"name": binding.ProviderName, "namespace": binding.Namespace, "labels": map[string]any{
			ownerLabel: "ani-network-service", tenantLabel: target.TenantID, resourceLabel: target.ResourceID, bindingLabel: binding.BindingID}},
		"spec": map[string]any{"cidrBlock": target.CIDR, "ipVersion": "IPv4", "allowedNamespaces": map[string]any{"from": "Same"}},
	}}
	if binding.ResourceKind == "subnet" {
		parent, err := p.parentReference(ctx, binding, target)
		if err != nil {
			return biz.ProviderObservation{}, err
		}
		object.SetKind("Subnet")
		object.Object["spec"] = subnetProviderSpec(target, parent)
	}
	created, err := resource.Create(ctx, object, metav1.CreateOptions{FieldManager: "ani-network-service", FieldValidation: "Strict"})
	if apierrors.IsAlreadyExists(err) {
		created, err = resource.Get(ctx, binding.ProviderName, metav1.GetOptions{})
		if err != nil {
			return biz.ProviderObservation{}, &biz.ProviderError{Kind: biz.ProviderUncertain, Cause: err}
		}
	}
	if err != nil {
		return biz.ProviderObservation{}, mutationFailure(err)
	}
	// A malformed success cannot disprove the POST. Preserve pending_create
	// until a later GET supplies an owned object and stable UID.
	value, err := p.inspect(ctx, created, binding, target)
	if err != nil {
		return biz.ProviderObservation{}, &biz.ProviderError{Kind: biz.ProviderUncertain, Cause: err}
	}
	return value, nil
}

func (p *KCProvider) Delete(ctx context.Context, target biz.ProviderTarget) error {
	if target.Kind == "vnic" {
		return p.deleteVNic(ctx, target)
	}
	if egressKind(target.Kind) {
		return p.deleteEgress(ctx, target)
	}
	binding, err := p.binding(ctx, target)
	if err != nil {
		return err
	}
	resource := p.client.Resource(kcResource(binding)).Namespace(binding.Namespace)
	object, err := resource.Get(ctx, binding.ProviderName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return readFailure(err)
	}
	target.Deleting = true
	observed, err := p.inspect(ctx, object, binding, target)
	if err != nil || !observed.Exists || object.GetDeletionTimestamp() != nil {
		return err
	}
	uid := types.UID(observed.Identity)
	revision := object.GetResourceVersion()
	err = resource.Delete(ctx, binding.ProviderName, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &revision}})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return mutationFailure(err)
	}
	return nil
}

func readFailure(err error) error { return &biz.ProviderError{Kind: biz.ProviderTemporary, Cause: err} }
func mutationFailure(err error) error {
	kind := biz.ProviderUncertain
	switch {
	case apierrors.IsConflict(err):
		kind = biz.ProviderConflict
	case apierrors.IsInvalid(err) || apierrors.IsBadRequest(err):
		kind = biz.ProviderReject
	case apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) || apierrors.IsTooManyRequests(err):
		kind = biz.ProviderTemporary
	default:
		var status apierrors.APIStatus
		if errors.As(err, &status) && status.Status().Code == http.StatusRequestEntityTooLarge {
			kind = biz.ProviderReject
		}
	}
	return &biz.ProviderError{Kind: kind, Cause: err}
}

func kcResource(b sqlcgen.NetworkProviderBinding) schema.GroupVersionResource {
	if b.ResourceKind == "vnic" {
		return kcManualVNics
	}
	if b.ResourceKind == "subnet" {
		return kcSubnets
	}
	return kcVPCs
}
func (p *KCProvider) parentReference(ctx context.Context, b sqlcgen.NetworkProviderBinding, t biz.ProviderTarget) (string, error) {
	parent, err := p.repository.readQueries(ctx).GetBinding(ctx, sqlcgen.GetBindingParams{TenantID: t.TenantID, ResourceID: t.VPCID})
	if err != nil {
		return "", readFailure(err)
	}
	if parent.ResourceKind != "vpc" || parent.ClusterID != b.ClusterID || parent.Namespace != b.Namespace || parent.ProviderName == "" {
		return "", &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	return parent.Namespace + "/" + parent.ProviderName, nil
}
func (p *KCProvider) inspect(ctx context.Context, object *unstructured.Unstructured, binding sqlcgen.NetworkProviderBinding, target biz.ProviderTarget) (biz.ProviderObservation, error) {
	if binding.ResourceKind == "vnic" {
		return p.inspectVNic(ctx, object, binding, target)
	}
	return inspectResource(object, binding, target)
}
