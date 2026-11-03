package data_test

import (
	"context"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"slices"
	"testing"
)

func TestSubnetPGCreationOptionsPersistenceReplayAndWorker(t *testing.T) {
	f := newAttachmentFixture(t)
	n, w := f.n, f.resourceWorker
	ctx := context.Background()
	tenant := f.r.TenantID
	gw := "10.42.5.254"
	r := biz.CreateSubnet{TenantID: tenant, VPCID: f.r.VPCID, Name: "custom-options", CIDR: "10.42.5.0/24", Gateway: &gw, IdempotencyKey: "custom-options", SubnetOptions: biz.SubnetOptions{ExcludeIPs: []string{"10.42.5.10..10.42.5.20"}, EnableDHCP: true, DNSServers: []string{"10.96.0.10", "1.1.1.1"}}}
	s, err := n.CreateSubnet(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	got, err := n.GetSubnet(ctx, tenant, s.ID)
	if err != nil || got.Gateway != gw || !got.EnableDHCP || !slices.Equal(got.ExcludeIPs, r.ExcludeIPs) || !slices.Equal(got.DNSServers, r.DNSServers) {
		t.Fatal("accepted options were not persisted", got, err)
	}
	page, err := n.ListSubnets(ctx, biz.ListSubnets{TenantID: tenant, VPCID: f.r.VPCID, Name: r.Name, Limit: 20})
	if err != nil || len(page.Items) != 1 || !slices.Equal(page.Items[0].DNSServers, r.DNSServers) {
		t.Fatal("list lost persisted options", page, err)
	}
	replay, err := n.CreateSubnet(ctx, r)
	if err != nil || replay.ID != s.ID || replay.LastOperationID != s.LastOperationID {
		t.Fatal("replay changed identity", replay, err)
	}
	r.EnableDHCP = false
	if _, err = n.CreateSubnet(ctx, r); biz.ReasonOf(err) != biz.IdempotencyConflict {
		t.Fatal("changed options reused accepted key", err)
	}
	driveSubnet(t, n, w, s, biz.Available)
}
