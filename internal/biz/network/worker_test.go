package biz

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdkTrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/zhangzhe-ctrl/ani-resource-service/internal/telemetry"
)

type earlyCompletionRepository struct {
	WorkRepository
	work      Work
	progress  Progress
	mutations int
	err       error
	finishes  int
}

func (r *earlyCompletionRepository) Claim(context.Context, string, time.Duration) (Work, bool, error) {
	return r.work, true, nil
}
func (r *earlyCompletionRepository) Finish(_ context.Context, _ Work, progress Progress) error {
	r.progress = progress
	r.finishes++
	return r.err
}

// Embedded nil methods panic if an early completion accidentally invokes the
// Provider. Neither branch should issue an external mutation after fencing.
type noProviderCalls struct{ ResourceProvider }

func TestWorkerEarlyCompletionLeaseLossDoesNotTerminateService(t *testing.T) {
	storageFailure := errors.New("storage unavailable")
	for _, work := range []Work{{CancelUnsent: true}, {Resource: ResourceWork{State: Deleted}}} {
		for _, completionErr := range []error{ErrLeaseLost, storageFailure} {
			r := &earlyCompletionRepository{work: work, err: completionErr}
			w, err := NewWorker(r, noProviderCalls{}, uuid.NewString(), DefaultWorkerPolicy())
			if err != nil {
				t.Fatal(err)
			}
			worked, err := w.Step(context.Background())
			if !worked || r.finishes != 1 {
				t.Fatal("completion was skipped or replayed", worked, r.finishes)
			}
			if completionErr == ErrLeaseLost && err != nil {
				t.Fatal("fenced completion terminated worker", err)
			}
			if completionErr == storageFailure && !errors.Is(err, storageFailure) {
				t.Fatal("unrelated persistence failure hidden", err)
			}
		}
	}
}

type failedObservationProvider struct {
	ResourceProvider
	err error
}

func (p failedObservationProvider) Observe(context.Context, ProviderTarget) (ProviderObservation, error) {
	return ProviderObservation{}, p.err
}

func TestWorkerObserverRetainsProviderCauseAfterSuccessfulPersistence(t *testing.T) {
	cause := errors.New("kubernetes request deadline exceeded")
	r := &earlyCompletionRepository{work: Work{Resource: ResourceWork{Kind: "egress_gateway", State: Available}}}
	calls := 0
	w, err := NewWorker(r, failedObservationProvider{err: &ProviderError{Kind: ProviderTemporary, Cause: cause}}, uuid.NewString(), DefaultWorkerPolicy(), func(_ context.Context, _ Work, p Progress, finishErr error) {
		calls++
		if finishErr != nil || !errors.Is(p.ProviderError, cause) || p.Reason != "" || p.State != Available || p.ObservationError != ProviderUnavailable {
			t.Error("provider failure lost or confused with persistence", p, finishErr)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.Step(context.Background()); err != nil || calls != 1 {
		t.Fatal("observer missing", calls, err)
	}
}

func reportedReady(value bool) *bool { return &value }

func (r *earlyCompletionRepository) BeginMutation(context.Context, Work, string, string) error {
	r.mutations++
	return nil
}

type directObservationProvider struct {
	ResourceProvider
	observation ProviderObservation
	creates     int
	deletes     int
}

func (p *directObservationProvider) Observe(context.Context, ProviderTarget) (ProviderObservation, error) {
	return p.observation, nil
}
func (p *directObservationProvider) EnsureVPC(context.Context, ProviderTarget) (ProviderObservation, error) {
	p.creates++
	return ProviderObservation{Exists: true, Identity: "created", Ready: reportedReady(true)}, nil
}
func (p *directObservationProvider) Delete(context.Context, ProviderTarget) error {
	p.deletes++
	return nil
}

func TestWorkerNativeReadyRecoversHistoricalFailedAndPreservesUnknown(t *testing.T) {
	for _, old := range []ResourceState{Provisioning, Available, Degraded, Failed} {
		for _, ready := range []*bool{reportedReady(true), reportedReady(false), nil} {
			r := &earlyCompletionRepository{work: Work{Resource: ResourceWork{Kind: "vpc", State: old, Reason: "previous"}, KnownIdentity: "original"}}
			p := &directObservationProvider{observation: ProviderObservation{Exists: true, Identity: "original", Ready: ready, StatusReason: "native-reason"}}
			w, err := NewWorker(r, p, uuid.NewString(), DefaultWorkerPolicy())
			if err != nil {
				t.Fatal(err)
			}
			if _, err = w.Step(context.Background()); err != nil {
				t.Fatal(err)
			}
			want := old
			wantReason := Reason("native-reason")
			if ready != nil {
				if *ready {
					want = Available
					wantReason = ""
				} else if old == Available || old == Degraded {
					want = Degraded
				} else {
					want = Provisioning
				}
			}
			if r.progress.State != want || !r.progress.Observed || r.progress.Reason != wantReason || r.progress.OperationState != "" || p.creates != 0 {
				t.Fatalf("old=%s ready=%v progress=%+v", old, ready, r.progress)
			}
		}
	}
}

func TestWorkerMissingOriginalNeverRecreatesAndDelete404Completes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		work    Work
		creates int
		state   ResourceState
	}{
		{"active-create-no-UID", Work{Resource: ResourceWork{Kind: "vpc", State: Provisioning}, ActiveOperation: true, Operation: Operation{Kind: "create_vpc"}, PendingAction: "create"}, 1, Available},
		{"active-create-known-UID", Work{Resource: ResourceWork{Kind: "vpc", State: Provisioning}, KnownIdentity: "original", ActiveOperation: true, Operation: Operation{Kind: "create_vpc"}}, 0, Provisioning},
		{"idle-missing", Work{Resource: ResourceWork{Kind: "vpc", State: Available}, KnownIdentity: "original"}, 0, Degraded},
		{"delete-pending-create", Work{Resource: ResourceWork{Kind: "vpc", State: Deleting}, ActiveOperation: true, PendingAction: "create", Operation: Operation{Kind: "delete_vpc"}}, 0, Deleted},
		{"delete-own-child-pending", Work{Resource: ResourceWork{Kind: "vpc", State: Deleting}, CleanupPending: true, ActiveOperation: true, Operation: Operation{Kind: "delete_vpc"}}, 0, Deleted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &earlyCompletionRepository{work: tc.work}
			p := &directObservationProvider{}
			w, err := NewWorker(r, p, uuid.NewString(), DefaultWorkerPolicy())
			if err != nil {
				t.Fatal(err)
			}
			if _, err = w.Step(context.Background()); err != nil {
				t.Fatal(err)
			}
			if p.creates != tc.creates || p.deletes != 0 || r.progress.State != tc.state || !r.progress.Observed {
				t.Fatalf("unexpected direct missing-object result: creates=%d deletes=%d progress=%+v", p.creates, p.deletes, r.progress)
			}
		})
	}
}

func TestWorkerReadFailureKeepsEveryPriorResourceStateAndReason(t *testing.T) {
	for _, old := range []ResourceState{Provisioning, Available, Degraded, Failed, Deleting} {
		r := &earlyCompletionRepository{work: Work{Resource: ResourceWork{Kind: "vpc", State: old, Reason: "last-native-reason"}, KnownIdentity: "original", ActiveOperation: true, Operation: Operation{Kind: "create_vpc"}}}
		w, err := NewWorker(r, failedObservationProvider{err: &ProviderError{Kind: ProviderTemporary}}, uuid.NewString(), DefaultWorkerPolicy())
		if err != nil {
			t.Fatal(err)
		}
		if _, err = w.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
		if r.progress.State != old || r.progress.Reason != "last-native-reason" || r.progress.Observed || r.progress.ObservationError != ProviderUnavailable || !r.progress.Backoff || r.mutations != 0 {
			t.Fatalf("failed read changed last projection: %+v", r.progress)
		}
	}
}

// observeCapture records the trace the observer callback saw, i.e. the context
// the business step actually ran under.
type observeCapture struct {
	traceID, spanID string
}

// capturingRepo returns one already-completed work so no provider is called.
type capturingRepo struct {
	WorkRepository
	work Work
}

func (r *capturingRepo) Claim(context.Context, string, time.Duration) (Work, bool, error) {
	return r.work, true, nil
}
func (r *capturingRepo) Finish(context.Context, Work, Progress) error { return nil }

// A network pass resumes the accepted operation's trace but runs on its own
// span: the persisted traceparent came from a request span that already ended.
func TestWorkerResumesTraceWithNewSpan(t *testing.T) {
	previousProvider, previousPropagator := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	otel.SetTracerProvider(sdkTrace.NewTracerProvider())
	otel.SetTextMapPropagator(propagation.TraceContext{})
	defer func() { otel.SetTracerProvider(previousProvider); otel.SetTextMapPropagator(previousPropagator) }()

	traceID, _ := trace.TraceIDFromHex("0af7651916cd43dd8448eb211c80319c")
	requestSpanID, _ := trace.SpanIDFromHex("b7ad6b7169203331")
	request := trace.NewSpanContext(trace.SpanContextConfig{TraceID: traceID, SpanID: requestSpanID, TraceFlags: trace.FlagsSampled})
	saved := telemetry.Capture(trace.ContextWithSpanContext(context.Background(), request))

	work := Work{
		Operation: Operation{ID: "op-1", TelemetryContext: saved},
		Resource:  ResourceWork{ID: "res-1", Kind: "vpc", State: Deleted},
	}
	var seen observeCapture
	w, err := NewWorker(&capturingRepo{work: work}, noProviderCalls{}, uuid.NewString(), DefaultWorkerPolicy(),
		func(ctx context.Context, _ Work, _ Progress, _ error) {
			sc := trace.SpanContextFromContext(ctx)
			seen.traceID, seen.spanID = sc.TraceID().String(), sc.SpanID().String()
		})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Step(context.Background()); err != nil {
		t.Fatalf("step: %v", err)
	}
	if seen.traceID != traceID.String() {
		t.Errorf("run trace_id = %s, want the accepted trace %s", seen.traceID, traceID)
	}
	if seen.spanID == "" || seen.spanID == requestSpanID.String() {
		t.Errorf("run span_id = %q, want a new span distinct from the request span", seen.spanID)
	}
}
