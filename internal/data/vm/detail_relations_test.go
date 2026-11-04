package vm

import (
	"context"
	"testing"

	network "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
)

type detailNetworkStub struct{ seen []string }

func (r *detailNetworkStub) GetVisibleVPC(_ context.Context, tenant, id string) (network.VPC, error) {
	r.seen = append(r.seen, tenant+":"+id)
	return network.VPC{Name: "Current VPC"}, nil
}
func (r *detailNetworkStub) GetVisibleSubnet(_ context.Context, tenant, id string) (network.Subnet, error) {
	r.seen = append(r.seen, tenant+":"+id)
	return network.Subnet{Name: "must not escape an error"}, network.Fail(network.ResourceNotFound, "missing")
}
func (r *detailNetworkStub) GetSecurityGroup(_ context.Context, tenant, id string) (network.SecurityGroup, error) {
	r.seen = append(r.seen, tenant+":"+id)
	return network.SecurityGroup{EgressMetadata: network.EgressMetadata{Name: "Current SG"}}, nil
}
func TestDetailRelationNamesUseTenantAndCurrentSecurityGroups(t *testing.T) {
	r := &detailNetworkStub{}
	v := biz.VM{TenantID: "trusted-tenant", Spec: biz.Spec{Network: biz.NetworkSelection{VPCID: "vpc-a", SubnetID: "subnet-a"}, SecurityGroupIDs: []string{"old-group"}}, SecurityGroupsInitialized: true, SecurityGroupIDs: []string{"current-group"}}
	out := NewDetailRelations(r).ReadRelations(context.Background(), v)
	if out.VPC.Name != "Current VPC" || out.Subnet.Name != "" || out.Subnet.State != "NOT_FOUND" || len(out.SecurityGroups) != 1 || out.SecurityGroups[0].ID != "current-group" {
		t.Fatalf("incorrect or leaking relations: %+v", out)
	}
	for _, seen := range r.seen {
		if len(seen) < len("trusted-tenant:") || seen[:len("trusted-tenant:")] != "trusted-tenant:" {
			t.Fatalf("unscoped lookup: %s", seen)
		}
	}
	v.SecurityGroupIDs = nil
	out = NewDetailRelations(r).ReadRelations(context.Background(), v)
	if len(out.SecurityGroups) != 0 {
		t.Fatal("explicitly cleared security groups restored the create-time binding")
	}
}
