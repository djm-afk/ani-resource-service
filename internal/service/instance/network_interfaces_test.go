package instance

import (
	"context"
	"testing"

	pb "github.com/zhangzhe-ctrl/ani-resource-service/api/instance/v1"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/instance"
)

type networkViewPort struct {
	biz.NetworkPort
	observation biz.NetworkObservation
}

func (p *networkViewPort) Observe(context.Context, biz.Instance) (biz.NetworkObservation, error) {
	return p.observation, nil
}

func TestNetworkViewUsesCurrentNetworkReasonInsteadOfOwnerCache(t *testing.T) {
	const tenant = "11111111-1111-4111-8111-111111111111"
	const id = "22222222-2222-4222-8222-222222222222"
	repo := &diagnosticRepo{view: biz.Instance{TenantID: tenant, ID: id, Observed: biz.Running, Reason: "INSTANCE_DEPENDENCY_UNAVAILABLE", Spec: biz.Spec{InstanceKind: biz.KindContainer, Network: &biz.NetworkSelection{}}, Network: &biz.NetworkRuntime{}}}
	network := &networkViewPort{observation: biz.NetworkObservation{Confirmed: true, CurrentMode: "automatic", CurrentVNicName: "auto-current", CurrentIPs: []string{"10.0.0.2"}, State: "attached"}}
	s := &Service{repo: repo, network: network}
	ctx := biz.WithCaller(context.Background(), biz.Caller{TenantID: tenant, Actor: "test"})
	r := &pb.GetInstanceRequest{TenantId: tenant, InstanceId: id}
	view, err := s.GetNetworkInterfaces(ctx, r)
	if err != nil || !view.Items[0].EffectiveNow || view.Items[0].ObservationReason != "" {
		t.Fatal("owner diagnostic hid confirmed network", view, err)
	}
	network.observation.Reason = "NETWORK_PROVIDER_UNAVAILABLE"
	view, err = s.GetNetworkInterfaces(ctx, r)
	if err != nil || view.Items[0].EffectiveNow || view.Items[0].ObservationReason != network.observation.Reason {
		t.Fatal("current network failure was hidden", view, err)
	}
}
