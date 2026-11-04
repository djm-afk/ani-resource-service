package server

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
)

type loadBalancerDiagnosticsBoundary struct {
	biz.WorkRepository
	biz.ResourceProvider
	work    biz.Work
	failure error
}

func (b *loadBalancerDiagnosticsBoundary) Claim(context.Context, string, time.Duration) (biz.Work, bool, error) {
	return b.work, true, nil
}
func (b *loadBalancerDiagnosticsBoundary) Finish(context.Context, biz.Work, biz.Progress) error {
	return nil
}
func (b *loadBalancerDiagnosticsBoundary) BeginLoadBalancerMutation(context.Context, biz.Work, biz.LoadBalancerComponent, string, string) error {
	panic("failed observation must not begin a mutation")
}
func (b *loadBalancerDiagnosticsBoundary) ObserveLoadBalancer(context.Context, biz.Work) (biz.LoadBalancerObservation, error) {
	return biz.LoadBalancerObservation{}, b.failure
}
func (b *loadBalancerDiagnosticsBoundary) MutateLoadBalancer(context.Context, biz.Work, biz.LoadBalancerComponent, string, biz.LoadBalancerObservation) (biz.LoadBalancerComponentObservation, error) {
	panic("failed observation must not mutate the provider")
}

func TestLoadBalancerWorkerObservationFailureReachesFormalDiagnostics(t *testing.T) {
	o, err := NewObservability("test-lb-diagnostics", "test", NewReadiness(func() bool { return true }))
	if err != nil {
		t.Fatal(err)
	}
	defer o.Shutdown(context.Background())
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	cause := errors.New("diagnostic cause from Kubernetes LB observation")
	boundary := &loadBalancerDiagnosticsBoundary{
		work: biz.Work{
			Resource:  biz.ResourceWork{Kind: "load_balancer", ID: "lb-diagnostics", State: biz.Available, LoadBalancer: &biz.LoadBalancerWork{}},
			BindingID: "lb-binding-id",
			Operation: biz.Operation{ID: "lb-operation-id"},
			Epoch:     79,
		},
		failure: &biz.ProviderError{Kind: biz.ProviderTemporary, Cause: cause},
	}
	calls := 0
	worker, err := biz.NewWorker(boundary, boundary, "44444444-4444-4444-8444-444444444444", biz.DefaultWorkerPolicy(), func(ctx context.Context, work biz.Work, progress biz.Progress, finishErr error) {
		calls++
		if finishErr != nil || !errors.Is(progress.ProviderError, cause) {
			t.Errorf("observation cause did not survive persistence: %v, %v", progress.ProviderError, finishErr)
		}
		if progress.State != biz.Available || progress.Reason != "" || progress.ObservationError != biz.ProviderUnavailable || !progress.Backoff || progress.Observed {
			t.Errorf("diagnostics changed observation failure behavior: %+v", progress)
		}
		o.ObserveWork(ctx, logger, work, progress, finishErr)
	})
	if err != nil {
		t.Fatal(err)
	}
	if worked, err := worker.Step(context.Background()); !worked || err != nil || calls != 1 {
		t.Fatalf("worker diagnostic handoff: worked=%v calls=%d err=%v", worked, calls, err)
	}
	for _, want := range []string{cause.Error(), `"msg":"network provider request failed"`, `"resource_type":"load_balancer"`, "lb-diagnostics", "lb-binding-id", "lb-operation-id", `"epoch":79`} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("missing LB diagnostic %q: %s", want, output.String())
		}
	}
}

func TestWorkerDiagnosticsLogProviderCauseAndResourceContext(t *testing.T) {
	o, err := NewObservability("test", "test", NewReadiness(func() bool { return true }))
	if err != nil {
		t.Fatal(err)
	}
	defer o.Shutdown(context.Background())
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	work := biz.Work{Resource: biz.ResourceWork{Kind: "vlan", ID: "vlan-id"}, BindingID: "binding-id", Operation: biz.Operation{ID: "operation-id"}}
	o.ObserveWork(context.Background(), logger, work, biz.Progress{ObservationError: biz.ProviderUnavailable, ProviderError: &biz.ProviderError{Kind: biz.ProviderTemporary, Cause: errors.New("client rate limiter wait: context deadline exceeded")}}, nil)
	for _, want := range []string{"client rate limiter wait: context deadline exceeded", "vlan-id", "binding-id", "operation-id"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("missing diagnostic %q: %s", want, output.String())
		}
	}
}

func TestVirtualSwitchDiagnosticsLogNodeCauseWithoutClaimingObservation(t *testing.T) {
	o, err := NewObservability("test-virtual-switch", "test", NewReadiness(func() bool { return true }))
	if err != nil {
		t.Fatal(err)
	}
	defer o.Shutdown(context.Background())
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	work := biz.VirtualSwitchWork{Switch: biz.VirtualSwitch{EgressMetadata: biz.EgressMetadata{ID: "switch-id", LastOperationID: "operation-id"}, BindingID: "binding-id", BindingState: "configured"}}
	progress := biz.VirtualSwitchProgress{BindingState: "configured", ObservationError: biz.ProviderUnavailable, Action: "inspect", NodeName: "node-a", NodeUID: "node-uid", ProviderError: &biz.ProviderError{Kind: biz.ProviderTemporary, Cause: errors.New("node GET timed out")}}
	o.ObserveVirtualSwitchWork(context.Background(), logger, work, progress, nil)
	for _, want := range []string{"node GET timed out", "switch-id", "binding-id", "operation-id", "node-a", "node-uid", `"action":"inspect"`, `"observed":false`, `"result":"read_error"`} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("missing diagnostic %q: %s", want, output.String())
		}
	}
	if strings.Contains(output.String(), `"result":"observed"`) {
		t.Fatalf("temporary failure reported successful observation: %s", output.String())
	}
}
