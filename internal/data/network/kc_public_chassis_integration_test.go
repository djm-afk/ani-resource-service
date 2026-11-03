package data_test

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	data "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/tenantnamespace"
	controlled "github.com/zhangzhe-ctrl/ani-resource-service/tests/net05a/provider"
	"k8s.io/client-go/rest"
)

// Bootstrap creates the durable topology through the existing business fixture.
// The behavior under test always uses KCProvider.Observe/EnsureEgress against
// the real repository and HTTP Kubernetes boundary, not a substituted gate.
type chassisBootstrap struct{ switchBoundary }

func (b *chassisBootstrap) VirtualSwitchInventory(ctx context.Context, cluster string, sw biz.VirtualSwitch, device string) (biz.VirtualSwitchInventory, error) {
	v, err := b.switchBoundary.VirtualSwitchInventory(ctx, cluster, sw, device)
	v.Members = append(v.Members, biz.VirtualSwitchNode{NodeName: "n3", NodeUID: "uid3", Priority: 100, Allocated: true})
	v.Inventory.Items = append(v.Inventory.Items, biz.PreparedDeviceNode{NodeName: "n3", NodeUID: "uid3", Name: device})
	return v, err
}

type publicChassisFixture struct {
	f        *egressFixture
	api      *controlled.Server
	provider *data.KCProvider
	sw       biz.VirtualSwitch
	vlan     biz.PlatformResource
	gateway  biz.PlatformResource
}

func newPublicChassisFixture(t *testing.T) *publicChassisFixture {
	t.Helper()
	f, _ := switchEgress(t)
	f.provider.images = []string{"sha256:" + strings.Repeat("a", 64)}
	boundary := &chassisBootstrap{}
	f.p.UseEgressInfrastructure(boundary)
	var err error
	f.e, err = biz.NewEgress(f.p, boundary, biz.ContextEgressAuthorization{}, []byte(strings.Repeat("s", 32)), time.Minute, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	sw, err := f.e.CreateVirtualSwitch(f.ctx, biz.VirtualSwitchIntent{Name: "public-group", IdempotencyKey: "switch"})
	if err != nil {
		t.Fatal(err)
	}
	vlan, err := f.e.CreatePlatform(f.ctx, biz.PlatformIntent{Kind: "create_vlan", Name: "vlan", IdempotencyKey: "vlan", Vlan: &biz.VlanConfig{ClusterID: "test-cluster", VirtualSwitchID: sw.ID, DeviceName: "ens35", VlanID: 0}})
	if err != nil {
		t.Fatal(err)
	}
	policy := biz.DefaultWorkerPolicy()
	policy.RetryMin = time.Millisecond
	worker, err := biz.NewVirtualSwitchWorker(f.p, boundary, uuid.NewString(), policy)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		if _, err := worker.Step(f.ctx); err != nil {
			t.Fatal(err)
		}
		sw, err = f.e.GetVirtualSwitch(f.ctx, sw.ID)
		if err != nil {
			t.Fatal(err)
		}
		if sw.BindingState == "configured" {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if sw.BindingState != "configured" || len(sw.Nodes) != 3 {
		t.Fatal(sw)
	}
	f.drive(t, func() bool {
		vlan, err = f.e.GetPlatform(f.ctx, "vlan", vlan.ID)
		return err == nil && vlan.State == biz.Available
	})
	gateway := f.platform(t, biz.PlatformIntent{Kind: "create_egress_gateway", Name: "gateway", IdempotencyKey: "gateway"})
	f.pool = f.platform(t, biz.PlatformIntent{Kind: "create_public_pool", Name: "underlay", IdempotencyKey: "underlay", Pool: &biz.PublicPoolConfig{Mode: "underlay", GatewayID: gateway.ID, VlanNetworkID: vlan.ID, CIDR: "192.0.2.0/24", OVNGatewayIP: "192.0.2.2", UpstreamGatewayIP: "192.0.2.1"}})
	api := controlled.New()
	seedEgressInfrastructure(api)
	api.Change("configmaps", map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "kcn-config", "namespace": "kcn-system", "uid": "config-uid"}}, false)
	for _, member := range sw.Nodes {
		api.Change("nodes", map[string]any{"apiVersion": "v1", "kind": "Node", "metadata": map[string]any{"name": member.NodeName, "uid": member.NodeUID, "labels": map[string]any{"networking.kubercloud.com/allocated": "true", "vcluster.networking.kubercloud.com/public-group": "100"}, "annotations": map[string]any{data.VirtualSwitchOwnerKey(sw.GroupName): sw.BindingID, "vcluster.networking.kubercloud.com/public-group": "ens35", "networking.kubercloud.com/chassis": "chassis-" + member.NodeName}}, "status": map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}}, false)
		api.Change("pods", map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{"name": "cni-" + member.NodeName, "namespace": "kcn-system", "uid": "cni-" + member.NodeUID, "labels": map[string]any{"networking.kubercloud.com/app": "cni-ds"}}, "spec": map[string]any{"nodeName": member.NodeName, "containers": []any{map[string]any{"name": "cni"}}}, "status": map[string]any{"phase": "Running", "conditions": []any{map[string]any{"type": "Ready", "status": "True"}}, "containerStatuses": []any{map[string]any{"name": "cni", "ready": true, "imageID": "fixture@sha256:" + strings.Repeat("a", 64)}}}}, false)
	}
	x := &publicChassisFixture{f: f, api: api, sw: sw, vlan: vlan, gateway: gateway}
	x.seed(t, gateway, map[string]any{"scope": "Public", "egressType": "Host"}, map[string]any{"conditions": chassisConditions(), "localIP": "100.64.0.2", "boundResources": map[string]any{"router": "router"}})
	x.seed(t, vlan, map[string]any{"vClusterName": sw.GroupName, "devName": "ens35", "vlanID": int64(0)}, map[string]any{"conditions": chassisConditions(), "subnet": "kcn-system/" + strings.Replace(f.pool.ID, "_", "-", 1)})
	excluded := []any{}
	for _, v := range f.pool.Pool.ExcludedIPs {
		excluded = append(excluded, v)
	}
	x.seed(t, f.pool, map[string]any{"type": "Public", "ipVersion": "IPv4", "cidrBlock": f.pool.Pool.CIDR, "gatewayIP": f.pool.Pool.OVNGatewayIP, "gateway": strings.Replace(gateway.ID, "_", "-", 1), "excludeIPs": excluded, "allowedNamespaces": map[string]any{"from": "All"}, "enableDHCP": false, "natOutgoing": false, "underlayConfig": map[string]any{"vlanNetwork": strings.Replace(vlan.ID, "_", "-", 1), "gatewayIP": "192.0.2.1"}}, map[string]any{"conditions": chassisConditions(), "observedGeneration": int64(1), "underlayState": map[string]any{"ready": true, "vlanNetwork": strings.Replace(vlan.ID, "_", "-", 1), "chassisNode": "n1,n2"}})
	host := httptest.NewServer(api)
	t.Cleanup(host.Close)
	x.provider, err = data.NewKCProvider(f.p, &rest.Config{Host: host.URL, QPS: 1000, Burst: 1000})
	if err != nil {
		t.Fatal(err)
	}
	return x
}

func chassisConditions() []any {
	return []any{map[string]any{"type": "Valid", "status": "True"}, map[string]any{"type": "Initialized", "status": "True"}, map[string]any{"type": "Ready", "status": "True"}}
}

func (x *publicChassisFixture) target(t *testing.T, r biz.PlatformResource) biz.ProviderTarget {
	t.Helper()
	target := biz.ProviderTarget{Kind: r.Kind, ResourceID: r.ID, Direct: true}
	if err := x.f.owner.QueryRow(x.f.ctx, "SELECT binding_id,provider_uid FROM network_platform_resources WHERE resource_id=$1", r.ID).Scan(&target.BindingID, &target.KnownIdentity); err != nil {
		t.Fatal(err)
	}
	return target
}
func (x *publicChassisFixture) seed(t *testing.T, r biz.PlatformResource, spec, status map[string]any) {
	t.Helper()
	target := x.target(t, r)
	kind, plural, namespace := "Subnet", "subnets", "kcn-system"
	if r.Kind == "vlan" {
		kind, plural, namespace = "VlanNetwork", "vlannetworks", ""
	}
	if r.Kind == "egress_gateway" {
		kind, plural, namespace = "EIPGateway", "eipgateways", ""
	}
	metadata := map[string]any{"name": strings.Replace(r.ID, "_", "-", 1), "uid": target.KnownIdentity, "generation": int64(1), "labels": map[string]any{"network.ani.io/managed-by": "ani-network-service", "network.ani.io/resource-id": r.ID, "network.ani.io/binding-id": target.BindingID}}
	if namespace != "" {
		metadata["namespace"] = namespace
	}
	x.api.Change(plural, map[string]any{"apiVersion": "networking.kubercloud.com/v1", "kind": kind, "metadata": metadata, "spec": spec, "status": status}, false)
}
func (x *publicChassisFixture) setReady(node, status string) {
	n := x.api.Object("nodes", "", node)
	n["status"].(map[string]any)["conditions"] = []any{map[string]any{"type": "Ready", "status": status}}
	x.api.Change("nodes", n, false)
	p := x.api.Object("pods", "kcn-system", "cni-"+node)
	p["status"].(map[string]any)["conditions"] = []any{map[string]any{"type": "Ready", "status": status}}
	x.api.Change("pods", p, false)
}

func TestExistingPublicPoolObservesHealthyChassisWithoutRetiringOfflineMember(t *testing.T) {
	x := newPublicChassisFixture(t)
	target := x.target(t, x.f.pool)
	before, err := x.f.e.GetVirtualSwitch(x.f.ctx, x.sw.ID)
	if err != nil {
		t.Fatal(err)
	}
	x.setReady("n3", "Unknown")
	got, err := x.provider.Observe(context.Background(), target)
	if err != nil || !got.Exists || !got.Ready {
		t.Fatalf("owned existing pool with healthy n1/n2 chassis did not observe ready: %+v %v", got, err)
	}
	after, err := x.f.e.GetVirtualSwitch(x.f.ctx, x.sw.ID)
	if err != nil || fmt.Sprint(before.Nodes) != fmt.Sprint(after.Nodes) || after.BindingID != before.BindingID || len(after.Nodes) != 3 {
		t.Fatal("observation changed frozen membership or binding", before, after, err)
	}
	counts, _ := x.api.Counts()
	for method, n := range counts {
		if (strings.HasPrefix(method, "POST/") || strings.HasPrefix(method, "PATCH/") || strings.HasPrefix(method, "DELETE/")) && n != 0 {
			t.Fatal("read-only observation performed write", counts)
		}
	}
}

func TestExistingPublicPoolRequiresCurrentOwnedChassisAndCompleteMembership(t *testing.T) {
	for _, failure := range []string{"healthy", "selected_node_unready", "selected_cni_unready", "replacement_uid", "unselected_replacement_uid", "missing_frozen_member", "selected_ack_missing", "unselected_ack_missing", "foreign_chassis", "duplicate_chassis"} {
		t.Run(failure, func(t *testing.T) {
			x := newPublicChassisFixture(t)
			if failure != "healthy" {
				x.setReady("n3", "Unknown")
			}
			switch failure {
			case "selected_node_unready":
				x.setReady("n1", "Unknown")
			case "selected_cni_unready":
				p := x.api.Object("pods", "kcn-system", "cni-n1")
				p["status"].(map[string]any)["conditions"] = []any{map[string]any{"type": "Ready", "status": "False"}}
				x.api.Change("pods", p, false)
			case "replacement_uid", "unselected_replacement_uid":
				name := "n1"
				if failure == "unselected_replacement_uid" {
					name = "n3"
				}
				n := x.api.Object("nodes", "", name)
				n["metadata"].(map[string]any)["uid"] = "replacement-uid"
				x.api.Change("nodes", n, false)
			case "missing_frozen_member":
				x.api.Change("nodes", x.api.Object("nodes", "", "n3"), true)
			case "selected_ack_missing", "unselected_ack_missing":
				name := "n1"
				if failure == "unselected_ack_missing" {
					name = "n3"
				}
				n := x.api.Object("nodes", "", name)
				delete(n["metadata"].(map[string]any)["annotations"].(map[string]any), "vcluster.networking.kubercloud.com/public-group")
				x.api.Change("nodes", n, false)
			case "foreign_chassis", "duplicate_chassis":
				p := x.api.Object("subnets", "kcn-system", strings.Replace(x.f.pool.ID, "_", "-", 1))
				chassis := "n1,not-a-member"
				if failure == "duplicate_chassis" {
					chassis = "n1,n1"
				}
				p["status"].(map[string]any)["underlayState"].(map[string]any)["chassisNode"] = chassis
				x.api.Change("subnets", p, false)
			}
			got, err := x.provider.Observe(context.Background(), x.target(t, x.f.pool))
			if failure == "healthy" {
				if err != nil || !got.Exists || !got.Ready {
					t.Fatal("healthy original semantics regressed", got, err)
				}
			} else if err == nil && got.Ready {
				t.Fatal("unproven chassis or incomplete binding became ready", failure, got)
			}
		})
	}
}

func TestNewPhysicalEgressRechecksAllFrozenMembersBeforePOST(t *testing.T) {
	for _, kind := range []string{"vlan", "public_pool"} {
		t.Run(kind, func(t *testing.T) {
			x := newPublicChassisFixture(t)
			var pending biz.PlatformResource
			var err error
			vlanInput := biz.PlatformIntent{Kind: "create_vlan", Name: "new-physical", IdempotencyKey: "new-physical", Vlan: &biz.VlanConfig{ClusterID: "test-cluster", VirtualSwitchID: x.sw.ID, DeviceName: "ens35", VlanID: 1}}
			if kind == "vlan" {
				pending, err = x.f.e.CreatePlatform(x.f.ctx, vlanInput)
			} else {
				vlan := x.f.platform(t, vlanInput)
				x.seed(t, vlan, map[string]any{"devName": "ens35", "vClusterName": x.sw.GroupName, "vlanID": int64(1)}, map[string]any{"conditions": chassisConditions()})
				pending, err = x.f.e.CreatePlatform(x.f.ctx, biz.PlatformIntent{Kind: "create_public_pool", Name: "new-underlay", IdempotencyKey: "new-underlay", Pool: &biz.PublicPoolConfig{Mode: "underlay", GatewayID: x.gateway.ID, VlanNetworkID: vlan.ID, CIDR: "198.51.100.0/24", OVNGatewayIP: "198.51.100.2", UpstreamGatewayIP: "198.51.100.1"}})
			}
			if err != nil {
				t.Fatal(err)
			}
			target := x.target(t, pending)
			if target.KnownIdentity != "" {
				t.Fatal("test must exercise a real pending physical POST", target)
			}
			// The node fails after durable admission, immediately before the
			// real adapter's last POST prerequisite check.
			x.setReady("n3", "Unknown")
			got, err := x.provider.EnsureEgress(context.Background(), target)
			if err == nil {
				t.Fatal("new physical configuration bypassed offline frozen member", got)
			}
			counts, _ := x.api.Counts()
			for method, n := range counts {
				if (strings.HasPrefix(method, "POST/") || strings.HasPrefix(method, "PATCH/")) && n != 0 {
					t.Fatal("failed preparation sent a physical write", counts)
				}
			}
			x.setReady("n3", "True")
			got, err = x.provider.EnsureEgress(context.Background(), target)
			if err != nil || !got.Exists {
				t.Fatal("healthy full group no longer permits its physical POST", got, err)
			}
			counts, _ = x.api.Counts()
			plural := "vlannetworks"
			if kind == "public_pool" {
				plural = "subnets"
			}
			if counts["POST/"+plural] != 1 {
				t.Fatal("physical POST not sent exactly once", counts)
			}
		})
	}
}

func TestProductEIPUsesExistingPoolChassisWithoutNewInterfacePreparation(t *testing.T) {
	x := newPublicChassisFixture(t)
	identity := tenantnamespace.Identity{TenantID: uuid.NewString(), ClusterID: "test-cluster", UID: uuid.NewString()}
	identity.Name = "tenant-" + identity.TenantID
	x.f.p.ConfigureTenantNamespaceResolver(func(context.Context, string) (tenantnamespace.Identity, error) { return identity, nil })
	x.api.Change("namespaces", map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": identity.Name, "uid": identity.UID, "labels": map[string]any{"network.ani.io/managed-by": "ani-network-service", "network.ani.io/tenant-id": identity.TenantID}}, "status": map[string]any{"phase": "Active"}}, false)
	x.f.setPool(t, biz.PlatformIntent{Kind: "set_pool_allocation", ID: x.f.pool.ID, Enabled: true, IdempotencyKey: "open"})
	x.f.setPool(t, biz.PlatformIntent{Kind: "set_default_pool", ID: x.f.pool.ID, IdempotencyKey: "default"})
	ctx := biz.WithEgressCaller(context.Background(), biz.EgressCaller{TenantID: identity.TenantID, Attribution: biz.Attribution{Actor: "tenant-test", DirectCaller: "fixture"}})
	eip, err := x.f.e.CreateEIP(ctx, biz.EgressIntent{Name: "product-eip", IdempotencyKey: "product-eip"})
	if err != nil {
		t.Fatal(err)
	}
	target := biz.ProviderTarget{TenantID: identity.TenantID, ResourceID: eip.ID, Kind: "eip"}
	if err := x.f.owner.QueryRow(ctx, "SELECT binding_id,provider_uid FROM network_provider_bindings WHERE tenant_id=$1 AND eip_id=$2", identity.TenantID, eip.ID).Scan(&target.BindingID, &target.KnownIdentity); err != nil {
		t.Fatal(err)
	}
	x.setReady("n3", "Unknown")
	got, err := x.provider.EnsureEgress(ctx, target)
	if err != nil || !got.Exists || !got.Ready || got.Egress == nil || got.Egress.Address == "" {
		t.Fatal("product EIP incorrectly required a new physical preparation", got, err)
	}
	counts, _ := x.api.Counts()
	if counts["POST/eips"] != 1 || counts["POST/vlannetworks"] != 0 || counts["PATCH/nodes"] != 0 {
		t.Fatal("EIP changed physical interface configuration", counts)
	}
}
