package service

import (
	"context"
	"testing"
	"time"

	networkv1 "github.com/zhangzhe-ctrl/ani-resource-service/api/network/v1"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestLBHealthPortWire(t *testing.T) {
	port := uint32(8080)
	in, err := lbMutable("lb", "", nil, &networkv1.LoadBalancerHealthCheck{Port: &port})
	if err != nil || in.Health.Port == nil || *in.Health.Port != port {
		t.Fatalf("input: %+v %v", in, err)
	}
	out := wireLoadBalancer(biz.LoadBalancer{Health: biz.LoadBalancerHealth{Port: port}})
	if out.HealthCheck.Port == nil || *out.HealthCheck.Port != port {
		t.Fatal(out)
	}
	if wireLoadBalancer(biz.LoadBalancer{}).HealthCheck.Port != nil {
		t.Fatal("legacy endpoint default must remain absent")
	}
}

func TestLBListenerSetWire(t *testing.T) {
	portA, portB, healthA, healthB := uint32(80), uint32(8081), uint32(8080), uint32(9090)
	input, err := lbListenersInput(&networkv1.LoadBalancerListenerSet{Items: []*networkv1.LoadBalancerListenerInput{
		{Id: "a", Name: "first", Port: &portA, Backends: []*networkv1.LoadBalancerBackendInput{{Address: "10.42.2.2", Port: healthA}}, HealthCheck: &networkv1.LoadBalancerHealthCheck{Port: &healthA}},
		{Id: "b", Name: "second", Port: &portB, Backends: []*networkv1.LoadBalancerBackendInput{{Address: "10.42.2.3", Port: healthB}}, HealthCheck: &networkv1.LoadBalancerHealthCheck{Port: &healthB}},
	}})
	if err != nil || len(input) != 2 || *input[0].Health.Port != healthA || *input[1].Health.Port != healthB || input[0].Backends[0].Address == input[1].Backends[0].Address {
		t.Fatal("listener wire lost independent configuration", input, err)
	}
	output := wireLoadBalancer(biz.LoadBalancer{Listeners: []biz.LoadBalancerListener{
		{ID: "a", Name: "first", Port: portA, Health: biz.LoadBalancerHealth{Port: healthA}, Backends: []biz.LoadBalancerBackend{{Address: "10.42.2.2", Port: healthA}}},
		{ID: "b", Name: "second", Port: portB, Health: biz.LoadBalancerHealth{Port: healthB}, Backends: []biz.LoadBalancerBackend{{Address: "10.42.2.3", Port: healthB}}},
	}})
	if len(output.Listeners) != 2 || output.Listeners[0].GetHealthCheck().GetPort() != healthA || output.Listeners[1].GetHealthCheck().GetPort() != healthB || output.Listeners[1].GetName() != "second" {
		t.Fatal("listener response lost health or identity", output)
	}
	omitted, err := lbListenersInput(nil)
	if err != nil || omitted != nil {
		t.Fatal("omitted collection changed", err)
	}
	empty, err := lbListenersInput(&networkv1.LoadBalancerListenerSet{})
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatal("empty collection presence lost", err)
	}
	if _, err = lbListenersInput(&networkv1.LoadBalancerListenerSet{Items: []*networkv1.LoadBalancerListenerInput{nil}}); biz.ReasonOf(err) != biz.InvalidArgument {
		t.Fatal("nil listener accepted", err)
	}
	for _, protocol := range []networkv1.LoadBalancerListenerProtocol{networkv1.LoadBalancerListenerProtocol_LOAD_BALANCER_LISTENER_PROTOCOL_TCP, networkv1.LoadBalancerListenerProtocol_LOAD_BALANCER_LISTENER_PROTOCOL_UDP, networkv1.LoadBalancerListenerProtocol_LOAD_BALANCER_LISTENER_PROTOCOL_SSL} {
		l := &networkv1.LoadBalancerListenerInput{Protocol: protocol, SessionPersistence: &networkv1.LoadBalancerSessionPersistence{Mode: networkv1.LoadBalancerSessionMode_LOAD_BALANCER_SESSION_MODE_DISABLED}}
		if _, err := lbListenersInput(&networkv1.LoadBalancerListenerSet{Items: []*networkv1.LoadBalancerListenerInput{l}}); biz.ReasonOf(err) != biz.InvalidArgument {
			t.Fatal("non-HTTP Cookie accepted", protocol, err)
		}
	}
	zero := uint32(0)
	if _, err := lbListenersInput(&networkv1.LoadBalancerListenerSet{Items: []*networkv1.LoadBalancerListenerInput{{Protocol: networkv1.LoadBalancerListenerProtocol_LOAD_BALANCER_LISTENER_PROTOCOL_UDP, MaxConnections: &zero}}}); biz.ReasonOf(err) != biz.InvalidArgument {
		t.Fatal("UDP connection parameter accepted", err)
	}

}

func TestLBWireNeverInfersHealthFromConfiguration(t *testing.T) {
	v := biz.LoadBalancer{ConfigurationState: "configured", DesiredVersion: 3, AppliedVersion: 2}
	r := wireLoadBalancer(v)
	if r.DataPlaneState != networkv1.LoadBalancerDataPlaneState_LOAD_BALANCER_DATA_PLANE_STATE_UNKNOWN || r.AppliedVersion != 2 || r.DesiredVersion != 3 {
		t.Fatal(r)
	}
	for _, kind := range []string{"create_load_balancer", "update_load_balancer", "delete_load_balancer"} {
		o := wireOperation(biz.Operation{Kind: kind, ResourceType: "load_balancer", State: biz.Queued, CreatedAt: time.Now(), UpdatedAt: time.Now()})
		if o.Kind == networkv1.OperationKind_OPERATION_KIND_UNSPECIFIED || o.ResourceType != networkv1.ResourceType_RESOURCE_TYPE_LOAD_BALANCER {
			t.Fatal(o)
		}
	}
}
func TestLBWireRejectsUnknownEnumsAndMapsStableErrors(t *testing.T) {
	s := &TenantLoadBalancerService{}
	if _, err := s.CreateLoadBalancer(context.Background(), &networkv1.CreateLoadBalancerRequest{Exposure: 99}); status.Code(err) != codes.InvalidArgument {
		t.Fatal(err)
	}
	if _, err := s.ListLoadBalancers(context.Background(), &networkv1.ListLoadBalancersRequest{State: 99}); status.Code(err) != codes.InvalidArgument {
		t.Fatal(err)
	}
	if _, err := lbMutable("lb", "", nil, &networkv1.LoadBalancerHealthCheck{Protocol: 99}); biz.ReasonOf(err) != biz.InvalidArgument {
		t.Fatal(err)
	}
	for reason, code := range map[biz.Reason]codes.Code{biz.LoadBalancerNotReady: codes.Unavailable, biz.BackendIdentityMismatch: codes.FailedPrecondition, biz.VIPInUse: codes.FailedPrecondition} {
		if got := status.Code(rpcError(biz.Fail(reason, reason.Message()))); got != code {
			t.Fatalf("%s: %v", reason, got)
		}
	}
}

func TestLBWireDeferredConfigurationAndListenerFacts(t *testing.T) {
	v := biz.LoadBalancer{ConfigurationState: "waiting_for_backends", DesiredVersion: 4, ProviderTargetVersion: 4, AppliedVersion: 4, Listeners: []biz.LoadBalancerListener{{ID: "saved-listener", Name: "http", Protocol: "HTTP", Port: 80, DeploymentState: "waiting_for_backends", DeploymentReason: biz.WaitingForBackends}}}
	r := wireLoadBalancer(v)
	if r.ConfigurationState != networkv1.LoadBalancerConfigurationState_LOAD_BALANCER_CONFIGURATION_STATE_WAITING_FOR_BACKENDS || r.ProviderTargetVersion != 4 || r.AppliedVersion != 4 || r.DesiredVersion != 4 || r.DataPlaneState != networkv1.LoadBalancerDataPlaneState_LOAD_BALANCER_DATA_PLANE_STATE_UNKNOWN || len(r.Listeners) != 1 || r.Listeners[0].DeploymentState != "waiting_for_backends" || r.Listeners[0].Reason != string(biz.WaitingForBackends) || r.Listener.DeploymentState != "waiting_for_backends" {
		t.Fatal("deferred response lost durable facts", r)
	}
}
