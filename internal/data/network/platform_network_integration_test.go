package data_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	data "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/tests/testenv"
	"k8s.io/client-go/rest"
)

func baseNetworkConfig() *biz.PlatformBaseNetworkConfig {
	return &biz.PlatformBaseNetworkConfig{ClusterUID: "original-cluster", VPC: biz.PlatformNetworkConfigEntry{ProductID: "vpc_" + strings.Repeat("1", 32), NativeNamespace: "kcn-system", NativeName: "kcn-cluster", NativeUID: "original-vpc", DisplayName: "platform base", Published: true, AcceptNew: true}, Subnets: []biz.PlatformNetworkConfigEntry{{ProductID: "subnet_" + strings.Repeat("1", 32), NativeNamespace: "kcn-system", NativeName: "workloads", NativeUID: "original-subnet", DisplayName: "platform workloads", Published: true, AcceptNew: true}}}
}

func TestPlatformDirectoryPGVisibilityAdmissionReplayAndMACScope(t *testing.T) {
	p, owner := database(t)
	n := newNetwork(t, p, time.Minute)
	ctx := context.Background()
	config := baseNetworkConfig()
	if err := p.SyncPlatformBaseNetworkConfig(ctx, config); err != nil {
		t.Fatal(err)
	}
	one, two := uuid.NewString(), uuid.NewString()
	yes, no := true, false
	for _, tenant := range []string{one, two} {
		page, err := n.ListVPCs(ctx, biz.ListVPCs{TenantID: tenant, CanUse: &yes, CanManage: &no})
		if err != nil || page.Total != 1 || len(page.Items) != 1 || page.Items[0].ID != config.VPC.ProductID || page.Items[0].TenantID != "" || page.Items[0].OwnerScope != biz.OwnerScopePlatform {
			t.Fatal(page, err)
		}
		subnet, err := n.GetSubnet(ctx, tenant, config.Subnets[0].ProductID)
		if err != nil || !subnet.CanUse || subnet.CanManage || subnet.UsedIPRange != nil {
			t.Fatal(subnet, err)
		}
	}
	request := biz.CreateVNic{TenantID: one, Name: "first shared", SubnetID: config.Subnets[0].ProductID, IdempotencyKey: "first", RequestedIP: "203.0.113.7", RequestedMAC: "02:ab:cd:00:00:01"}
	v, err := n.CreateVNic(ctx, request)
	if err != nil || v.TenantID != one || v.Reference.OwnerScope != biz.OwnerScopePlatform {
		t.Fatal(v, err)
	}
	// No private product and no native CIDR projection was needed for acceptance.
	var privateCount int
	if err = owner.QueryRow(ctx, `SELECT (SELECT count(*) FROM network_vpcs)+(SELECT count(*) FROM network_subnets)`).Scan(&privateCount); err != nil || privateCount != 0 {
		t.Fatal(privateCount, err)
	}
	other := request
	other.TenantID = two
	if _, err = n.CreateVNic(ctx, other); biz.ReasonOf(err) != biz.AddressConflict {
		t.Fatal("shared MAC reservation was scoped to the wrong tenant", err)
	}
	config.VPC.Published = false
	config.Subnets[0].AcceptNew = false
	if err = p.SyncPlatformBaseNetworkConfig(ctx, config); err != nil {
		t.Fatal(err)
	}
	replay, err := n.CreateVNic(ctx, request)
	if err != nil || replay.ID != v.ID {
		t.Fatal("admission close broke permanent replay", replay, err)
	}
	request.IdempotencyKey = "second"
	request.RequestedMAC = ""
	if _, err = n.CreateVNic(ctx, request); biz.ReasonOf(err) != biz.NetworkNewAdmissionClosed {
		t.Fatal(err)
	}
	known, err := n.GetSubnet(ctx, two, config.Subnets[0].ProductID)
	if err != nil || known.CanUse {
		t.Fatal("unpublished known product cannot be read", known, err)
	}
	list, err := n.ListSubnets(ctx, biz.ListSubnets{TenantID: two})
	if err != nil || list.Total != 0 || len(list.Items) != 0 {
		t.Fatal(list, err)
	}
	for _, resource := range []string{config.VPC.ProductID, config.Subnets[0].ProductID} {
		if strings.HasPrefix(resource, "vpc_") {
			_, err = n.DeleteVPC(ctx, one, resource)
		} else {
			_, err = n.DeleteSubnet(ctx, one, resource)
		}
		if biz.ReasonOf(err) != biz.PermissionDenied {
			t.Fatal("read-only platform directory accepted management", err)
		}
	}
	saved := v.Reference.SubnetBindingID
	resolved, err := p.ResolveAcceptedNetworkSelection(ctx, biz.NetworkSelection{TenantID: one, VPCID: v.VPCID, SubnetID: v.SubnetID, Reference: v.Reference})
	if err != nil || resolved.BindingID != saved {
		t.Fatal("accepted reference followed publication flags", resolved, err)
	}
	config.Subnets[0].NativeUID = "replacement"
	if err = p.SyncPlatformBaseNetworkConfig(ctx, config); biz.ReasonOf(err) != biz.InvalidArgument {
		t.Fatal("config rebound original native UID", err)
	}
}

func TestPlatformAttachmentPGReplayPrecedesNamespaceAndIsolationSetup(t *testing.T) {
	p, owner := database(t)
	ctx := context.Background()
	config := baseNetworkConfig()
	tenant := uuid.NewString()
	namespace := "tenant-" + tenant
	api := &testenv.KC{Objects: map[string]map[string]any{"namespaces//" + namespace: {"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": namespace, "uid": "original-namespace", "labels": map[string]any{"network.ani.io/managed-by": "ani-network-service", "network.ani.io/tenant-id": tenant}}, "status": map[string]any{"phase": "Active"}}}, Creates: map[string]int{}, Deletes: map[string]int{}}
	server := httptest.NewServer(api)
	defer server.Close()
	if _, err := data.NewKCProvider(p, &rest.Config{Host: server.URL, QPS: 1000, Burst: 1000}); err != nil {
		t.Fatal(err)
	}
	if err := p.SyncPlatformBaseNetworkConfig(ctx, config); err != nil {
		t.Fatal(err)
	}
	selected, err := p.ResolveNewNetworkSelection(ctx, biz.NetworkSelection{TenantID: tenant, VPCID: config.VPC.ProductID, SubnetID: config.Subnets[0].ProductID})
	if err != nil {
		t.Fatal(err)
	}
	// Owner first accepted while open; Network preparation starts after closing.
	config.Subnets[0].AcceptNew = false
	if err = p.SyncPlatformBaseNetworkConfig(ctx, config); err != nil {
		t.Fatal(err)
	}
	attachments := biz.NewAttachments(p)
	request := biz.PrepareAttachment{TenantID: tenant, InstanceID: "container-one", VPCID: selected.VPCID, SubnetID: selected.SubnetID, ClusterID: selected.ClusterID, Namespace: selected.Namespace, NamespaceUID: selected.NamespaceUID, AcceptedReference: selected.Reference, Slot: "primary", RequestKey: "primary", SubmissionID: uuid.NewString(), Generation: 1, WorkloadMode: "controller_managed", ConsumerType: biz.ConsumerContainerInstance}
	accepted, err := attachments.PrepareAccepted(ctx, request)
	if err != nil || accepted.Reference.OwnerScope != biz.OwnerScopePlatform || accepted.BasicIsolationName != "ani-tenant-"+tenant {
		t.Fatal(accepted, err)
	}
	var ownTenant string
	var privateVPC, privateSubnet *string
	if err = owner.QueryRow(ctx, `SELECT tenant_id::text,vpc_id,subnet_id FROM network_attachments WHERE attachment_id=$1`, accepted.ID).Scan(&ownTenant, &privateVPC, &privateSubnet); err != nil || ownTenant != tenant || privateVPC != nil || privateSubnet != nil {
		t.Fatal(ownTenant, privateVPC, privateSubnet, err)
	}
	api.Mu.Lock()
	delete(api.Objects, "namespaces//"+namespace)
	delete(api.Objects, "basicnetworkisolations//ani-tenant-"+tenant)
	api.Mu.Unlock()
	replay, err := attachments.PrepareAccepted(ctx, request)
	if err != nil || replay.ID != accepted.ID || replay.BasicIsolationName != accepted.BasicIsolationName {
		t.Fatal("permanent replay performed dynamic preparation first", replay, err)
	}
	external := request
	external.RequestKey, external.InstanceID = "external", "external-one"
	external.WorkloadMode, external.ConsumerType = "", ""
	if _, err = attachments.Prepare(ctx, external); err == nil {
		t.Fatal("public path accepted internal platform reference")
	}
	// Original isolation UID is retained; missing original cannot be recreated.
	if _, err = p.EnsureTenantBasicIsolation(ctx, tenant); biz.ReasonOf(err) != biz.ProviderOwnership {
		t.Fatal(err)
	}
	api.Mu.Lock()
	creates := api.Creates["basicnetworkisolations"]
	api.Mu.Unlock()
	if creates != 1 {
		t.Fatal("missing original isolation declaration was recreated", creates)
	}
}
