package biz

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/zhangzhe-ctrl/ani-resource-service/internal/telemetry"
)

// TestEmitContractSample writes one record per Network event through the real
// helpers so the offline contract validator (tools/verify_logs.py) can check the
// vocabulary end to end. It only runs when ANI_NETWORK_LOG_SAMPLE names a file.
// It is not runtime evidence: the fields come from this test's fixture.
func TestEmitContractSample(t *testing.T) {
	path := os.Getenv("ANI_NETWORK_LOG_SAMPLE")
	if path == "" {
		t.Skip("set ANI_NETWORK_LOG_SAMPLE to emit a contract sample")
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	logger := slog.New(sampleHandler{inner: slog.NewJSONHandler(f, &slog.HandlerOptions{
		Level:       slog.LevelDebug,
		ReplaceAttr: sampleReplaceAttr,
	})}).With(
		slog.String("service.name", "ani-resource-service"),
		slog.String("service.version", "sample"),
		slog.String("service.instance.id", "ani-resource-service-sample"),
	)
	// A noop tracer produces no span, so the sample installs the SDK provider the
	// runtime already owns; without it trace_id/span_id would be absent.
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample())))
	t.Cleanup(func() { otel.SetTracerProvider(previous) })
	ctx, span := telemetry.StartRun(context.Background(), "sample")
	defer span.End()

	work := Work{
		Resource:        ResourceWork{Kind: "vpc", ID: "vpc_8a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d", TenantID: "3f2b7c1e-5d4a-4c8b-9a11-0c2d3e4f5a6b"},
		Operation:       Operation{ID: "9c1d2e3f-4a5b-4c6d-8e7f-90a1b2c3d4e5", Kind: "create_vpc"},
		ActiveOperation: true, BindingID: "9c1d2e3f-4a5b-4c6d-8e7f-90a1b2c3d4e6", Epoch: 3, ClaimedGeneration: 2, Attempt: 1,
	}
	id := work.eventIdentity()
	tgt := k8sTarget{clusterID: "primary", kind: "VPC", namespace: "tenant-3f2b7c1e-5d4a-4c8b-9a11-0c2d3e4f5a6b", name: "vpc-8a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d"}
	cause := ProviderError{Kind: ProviderTemporary, Cause: sampleError(`vpc.networking.kubercloud.com is forbidden: User "system:serviceaccount:ani-system:ani-resource-service" cannot create resource "vpcs"`)}

	runStarted(ctx, logger, WorkerMain, id)
	networkEvent(ctx, logger, slog.LevelInfo, EventOperationStarted, append([]any{"operation_id", work.Operation.ID, "action", work.Operation.Kind}, id...)...)
	stepStarted(ctx, logger, WorkerMain, StepEnsureVPC, id)
	dependencyFailed(ctx, logger, WorkerMain, "kubernetes", work.Operation.Kind, id, tgt, &cause)
	operationConflict(ctx, logger, WorkerMain, StepObserveProvider, id, ProviderOwnership, nil)
	networkEvent(ctx, logger, slog.LevelWarn, EventOperationRetry, append([]any{"operation_id", work.Operation.ID, "reason", string(ProviderUnavailable), "attempt", int(work.Attempt)}, id...)...)
	operationLeaseLost(ctx, logger, WorkerMain, StepPersistProgress, id)
	persistFailed(ctx, logger, WorkerMain, "finish", StepPersistProgress, id, sampleError("ERROR: duplicate key violates unique constraint (SQLSTATE 23505)"))
	operationFailed(ctx, logger, WorkerMain, id, work.Operation.ID, ProviderRejected, &cause)
	operationSucceeded(ctx, logger, WorkerMain, id, work.Operation.ID)
	stepWaiting(ctx, logger, WorkerMain, StepObserveProvider, id, ProviderNotReady)
	stepSucceeded(ctx, logger, WorkerMain, StepEnsureVPC, id)
	networkEvent(ctx, logger, slog.LevelInfo, EventResourceMutated, "resource_type", "attachment", "resource_id", "att_1", "attachment_id", "att_1", "action", "prepare_attachment", "result", "succeeded")
	networkEvent(ctx, logger, slog.LevelInfo, EventResourceReplayed, "resource_type", "attachment", "resource_id", "att_1", "attachment_id", "att_1", "action", "prepare_attachment", "result", "replayed")
	networkEvent(ctx, logger, slog.LevelWarn, EventOperationConflict, "worker", WorkerAttachment, "step", StepPersistAttachment, "reason", string(LeaseLost), "attachment_id", "att_1", "resource_type", "attachment")
	networkEvent(ctx, logger, slog.LevelWarn, EventRetryScheduled, "worker", WorkerObservation, "reason", "notify_observed_object", "resource_id", "vpc-a")
	stepFailed(ctx, logger, WorkerVirtualSwitch, StepObserveSwitch, []any{"resource_type", "virtual_switch", "resource_id", "vsw_1"}, k8sTarget{kind: "Node", name: "node-a", uid: "uid-a"}, sampleError("node GET timed out"), ProviderUnavailable)
	runCompleted(ctx, logger, WorkerMain, id, 0)

	for _, replayed := range []bool{false, true} {
		event, msg := "operation.accepted", "network operation accepted"
		if replayed {
			event, msg = "operation.replayed", "network operation replayed"
		}
		attrs := []any{"event", event, "action", "create_vpc", "resource_type", "vpc",
			"resource_id", work.Resource.ID, "tenant_id", work.Resource.TenantID, "operation_id", work.Operation.ID}
		if replayed {
			attrs = append(attrs, "result", "replayed", "origin_trace_id", telemetry.Capture(ctx).OriginTraceID)
		}
		logger.InfoContext(ctx, msg, attrs...)
	}
}

type sampleError string

func (e sampleError) Error() string { return string(e) }

// sampleHandler mirrors the production handler's two contract obligations that a
// bare JSON handler lacks: trace_id/span_id from the active span, and dropping
// empty strings. Without it the sample would fail the validator for reasons that
// belong to the encoder, not to the event vocabulary.
type sampleHandler struct{ inner slog.Handler }

func (h sampleHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}
func (h sampleHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return sampleHandler{h.inner.WithAttrs(attrs)}
}
func (h sampleHandler) WithGroup(name string) slog.Handler {
	return sampleHandler{h.inner.WithGroup(name)}
}

func (h sampleHandler) Handle(ctx context.Context, r slog.Record) error {
	out := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	hasTrace := false
	r.Attrs(func(a slog.Attr) bool {
		if a.Value.Kind() == slog.KindString && a.Value.String() == "" {
			return true
		}
		if a.Key == "trace_id" || a.Key == "span_id" {
			hasTrace = true
		}
		out.AddAttrs(a)
		return true
	})
	if !hasTrace {
		if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
			out.AddAttrs(slog.String("trace_id", sc.TraceID().String()), slog.String("span_id", sc.SpanID().String()))
		}
	}
	return h.inner.Handle(ctx, out)
}

func sampleReplaceAttr(_ []string, a slog.Attr) slog.Attr {
	if a.Key == slog.TimeKey {
		if t, ok := a.Value.Any().(time.Time); ok {
			return slog.String(slog.TimeKey, t.UTC().Format(time.RFC3339Nano))
		}
	}
	if a.Key == slog.LevelKey {
		if lv, ok := a.Value.Any().(slog.Level); ok {
			name := "DEBUG"
			switch {
			case lv >= slog.LevelError:
				name = "ERROR"
			case lv >= slog.LevelWarn:
				name = "WARN"
			case lv >= slog.LevelInfo:
				name = "INFO"
			}
			return slog.String(slog.LevelKey, name)
		}
	}
	return a
}
