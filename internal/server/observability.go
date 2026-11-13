package server

import (
	"context"
	"errors"
	"fmt"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"go.opentelemetry.io/otel/attribute"
	"log/slog"
	"strings"

	kratosmetrics "github.com/go-kratos/kratos/contrib/otel/v3/metrics"
	kratostracing "github.com/go-kratos/kratos/contrib/otel/v3/tracing"
	"github.com/go-kratos/kratos/v3/middleware"
	"github.com/go-kratos/kratos/v3/middleware/metadata"
	"github.com/go-kratos/kratos/v3/middleware/recovery"
	"github.com/go-kratos/kratos/v3/middleware/validate"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/telemetry"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/telemetry/logfields"
	"go.opentelemetry.io/otel"
	otelprometheus "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	metricSdk "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	traceSdk "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
)

type Observability struct {
	registry          *prometheus.Registry
	meterProvider     *metricSdk.MeterProvider
	tracerProvider    *traceSdk.TracerProvider
	requests          metric.Int64Counter
	seconds           metric.Float64Histogram
	workerAttempts    metric.Int64Counter
	providerReachable metric.Int64Gauge
	// attachmentLogger is bound at wiring so the metrics-only attachment lane
	// can also record a real failure.
	attachmentLogger *slog.Logger
}

func NewObservability(name, version string, readiness *Readiness) (*Observability, error) {
	if err := kratosmetrics.EnableOTELExemplar(); err != nil {
		return nil, fmt.Errorf("enable OpenTelemetry exemplars: %w", err)
	}

	registry := prometheus.NewRegistry()
	if err := registry.Register(prometheus.NewGoCollector()); err != nil {
		return nil, fmt.Errorf("register Go collector: %w", err)
	}
	if err := registry.Register(prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{})); err != nil {
		return nil, fmt.Errorf("register process collector: %w", err)
	}
	exporter, err := otelprometheus.New(
		otelprometheus.WithRegisterer(registry),
		otelprometheus.WithoutScopeInfo(),
	)
	if err != nil {
		return nil, fmt.Errorf("create OpenTelemetry Prometheus exporter: %w", err)
	}

	res := resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(name),
		semconv.ServiceVersion(version),
	)
	meterProvider := metricSdk.NewMeterProvider(
		metricSdk.WithResource(res),
		metricSdk.WithReader(exporter),
		metricSdk.WithView(kratosmetrics.DefaultSecondsHistogramView(kratosmetrics.DefaultServerSecondsHistogramName)),
	)
	tracerProvider := traceSdk.NewTracerProvider(
		traceSdk.WithResource(res),
		traceSdk.WithSampler(traceSdk.ParentBased(traceSdk.AlwaysSample())),
	)

	meter := meterProvider.Meter(name)
	requests, err := kratosmetrics.DefaultRequestsCounter(meter, kratosmetrics.DefaultServerRequestsCounterName)
	if err != nil {
		return nil, fmt.Errorf("create Kratos server request counter: %w", err)
	}
	seconds, err := kratosmetrics.DefaultSecondsHistogram(meter, kratosmetrics.DefaultServerSecondsHistogramName)
	if err != nil {
		return nil, fmt.Errorf("create Kratos server latency histogram: %w", err)
	}
	if _, err := meter.Int64ObservableGauge(
		"ani_runtime_ready",
		metric.WithDescription("Whether lifecycle, worker and Network database readiness checks pass."),
		metric.WithInt64Callback(func(_ context.Context, observer metric.Int64Observer) error {
			if readiness.Ready() {
				observer.Observe(1)
			} else {
				observer.Observe(0)
			}
			return nil
		}),
	); err != nil {
		return nil, fmt.Errorf("create readiness gauge: %w", err)
	}
	otel.SetMeterProvider(meterProvider)
	otel.SetTracerProvider(tracerProvider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	workerAttempts, err := meter.Int64Counter("ani_network_worker_attempts", metric.WithDescription("Durable worker attempts by bounded result."))
	if err != nil {
		return nil, err
	}
	providerReachable, err := meter.Int64Gauge("ani_network_provider_reachable", metric.WithDescription("Whether the most recent provider observation was reachable; absence means not observed yet."))
	if err != nil {
		return nil, err
	}
	return &Observability{
		registry:       registry,
		workerAttempts: workerAttempts, providerReachable: providerReachable,
		meterProvider:  meterProvider,
		tracerProvider: tracerProvider,
		requests:       requests,
		seconds:        seconds,
	}, nil
}

// SetAttachmentLogger binds the logger the attachment lane uses for failures.
func (o *Observability) SetAttachmentLogger(logger *slog.Logger) { o.attachmentLogger = logger }

func (o *Observability) Gatherer() prometheus.Gatherer {
	return o.registry
}

func (o *Observability) ServerMiddleware(logger *slog.Logger) []middleware.Middleware {
	// The RPC completion record now comes from the native stats handler
	// (rpc_observability.go). kratostracing.Server and requestLogging would add a
	// second server span and a duplicate completion, so they are not installed
	// on the gRPC listeners. The panic recovery stays and is bound to the shared
	// process.panic event.
	return []middleware.Middleware{
		recovery.Recovery(recovery.WithLogger(logger.With("event", "process.panic"))),
		metadata.Server(),
		kratosmetrics.Server(
			kratosmetrics.WithRequests(o.requests),
			kratosmetrics.WithSeconds(o.seconds),
		),
		validate.Validator(),
	}
}

// AdminMiddleware is the middleware chain for the admin HTTP listener. It keeps
// request tracing at the HTTP transport and adds the admin observability filter
// (see admin.go); RPC completion logging is not relevant here.
func (o *Observability) AdminMiddleware(logger *slog.Logger) []middleware.Middleware {
	return []middleware.Middleware{
		recovery.Recovery(recovery.WithLogger(logger.With("event", "process.panic"))),
		metadata.Server(),
		kratostracing.Server(kratostracing.WithTracerProvider(o.tracerProvider)),
		kratosmetrics.Server(
			kratosmetrics.WithRequests(o.requests),
			kratosmetrics.WithSeconds(o.seconds),
		),
		validate.Validator(),
	}
}

func (o *Observability) Shutdown(ctx context.Context) error {
	return errors.Join(
		o.meterProvider.Shutdown(ctx),
		o.tracerProvider.Shutdown(ctx),
	)
}

func (o *Observability) ObserveWork(ctx context.Context, logger *slog.Logger, work biz.Work, progress biz.Progress, err error) {
	if progress.ProviderError != nil {
		// The worker already reported the same cause at its origin; this record
		// keeps the existing failure identity and adds the required step. A
		// genuine concurrent/lease conflict is the only class that stays WARN;
		// the level is decided from the typed kind, never from a string.
		cause := progress.ProviderError
		kind := biz.ProviderFailure("")
		var failure *biz.ProviderError
		if errors.As(cause, &failure) {
			kind = failure.Kind
			if failure.Cause != nil {
				cause = failure.Cause
			}
		}
		level := slog.LevelError
		if kind == biz.ProviderConflict || kind == biz.ProviderInUse {
			level = slog.LevelWarn
		}
		attrs := []any{"event", biz.EventStepFailed, "worker", biz.WorkerMain,
			"step", providerFailureStep(work), "dependency", "kubernetes",
			"resource_type", work.Resource.Kind, "resource_id", work.Resource.ID,
			"reason", stepReason(progress.Reason, progress.OperationReason),
			"result", "failed", "epoch", int(work.Epoch)}
		if work.Operation.ID != "" {
			attrs = append(attrs, "operation_id", work.Operation.ID)
		}
		if work.Resource.TenantID != "" {
			attrs = append(attrs, "tenant_id", work.Resource.TenantID)
		}
		if work.BindingID != "" {
			attrs = append(attrs, "binding_id", work.BindingID)
		}
		attrs = append(attrs, logfields.ErrorFields(cause)...)
		logger.Log(ctx, level, "network provider request failed", attrs...)
	}
	result := "observed"
	if work.ActiveOperation {
		result = string(progress.OperationState)
	}
	if errors.Is(err, biz.ErrLeaseLost) {
		result = "lease_lost"
	} else if err != nil {
		result = "database_error"
	}
	if progress.ObservationError != "" && err == nil {
		result = "read_error"
	}
	if err == nil && (progress.Observed || progress.ObservationError != "") {
		reachable := int64(1)
		if progress.ObservationError != "" {
			reachable = 0
		}
		o.providerReachable.Record(ctx, reachable, metric.WithAttributes(attribute.String("kind", work.Resource.Kind)))
	}
	o.workerAttempts.Add(ctx, 1, metric.WithAttributes(attribute.String("result", result), attribute.String("kind", work.Resource.Kind)))
	if !work.ActiveOperation && err == nil && work.Resource.State == progress.State && work.Resource.Reason == progress.Reason {
		return
	}
	// The reconciliation record uses the contract's step/operation vocabulary;
	// the removed resource_state/attempt_result had no schema entry.
	event, level, failingStep := biz.EventStepSucceeded, slog.LevelInfo, biz.StepPersistProgress
	switch {
	case err != nil:
		event, level = biz.EventStepFailed, slog.LevelError
	case progress.OperationState == biz.OpFailed:
		event, level = biz.EventOperationFailed, slog.LevelError
	case progress.OperationState == biz.Succeeded:
		event, level = biz.EventOperationSuccess, slog.LevelInfo
	case progress.Backoff || progress.OperationState == biz.Blocked:
		event, level = biz.EventStepWaiting, slog.LevelInfo
	}
	attrs := []any{"event", event, "worker", biz.WorkerMain, "step", failingStep,
		"resource_type", work.Resource.Kind, "resource_id", work.Resource.ID,
		"operation_id", work.Operation.ID, "tenant_id", work.Resource.TenantID,
		"action", work.Operation.Kind, "epoch", int(work.Epoch), "attempt", int(work.Attempt)}
	// step.waiting/step.failed require reason; step.waiting also fixes
	// result=waiting and step.failed fixes result=failed.
	if event == biz.EventOperationSuccess || event == biz.EventStepSucceeded {
		attrs = append(attrs, "result", "succeeded")
	}
	reason := progress.Reason
	if reason == "" {
		reason = progress.OperationReason
	}
	if reason == "" && (event == biz.EventStepWaiting || event == biz.EventStepFailed) {
		// Both events require a non-blank reason; a backed-off reconciliation
		// always has a durable state, so the provider-not-ready state is used
		// only when neither field carried one.
		reason = biz.ProviderNotReady
	}
	if reason != "" {
		attrs = append(attrs, "reason", string(reason))
	}
	switch event {
	case biz.EventStepWaiting:
		attrs = append(attrs, "result", "waiting")
	case biz.EventStepFailed:
		attrs = append(attrs, "result", "failed")
	}
	if err != nil {
		attrs = append(attrs, logfields.ErrorFields(err)...)
	}
	logger.Log(ctx, level, "network reconciliation", attrs...)
}

// providerFailureStep names the stage a provider failure belongs to. It is
// derived from the durable operation kind only, so it never invents a step.
func providerFailureStep(work biz.Work) string {
	switch kind := work.Operation.Kind; {
	case strings.HasPrefix(kind, "create_"), kind == "bind_snat":
		return biz.StepEnsureVPC
	case kind == "set_snat_enabled":
		return biz.StepUpdateEgress
	case strings.HasPrefix(kind, "delete_"):
		return biz.StepDeleteProvider
	default:
		return biz.StepObserveProvider
	}
}

func (o *Observability) ObserveAttachmentWork(ctx context.Context, work biz.AttachmentWork, p biz.AttachmentProgress, err error) {
	// The attachment lane previously recorded metrics only. A real provider or
	// read failure must be visible with the existing identity.
	if p.ProviderError != nil || p.ObservationError != "" || err != nil {
		o.logAttachmentFailure(o.attachmentLogger, ctx, work, p, err)
	}
	result := "observed"
	if errors.Is(err, biz.ErrLeaseLost) {
		result = "lease_lost"
	} else if err != nil {
		result = "database_error"
	}
	if err == nil && p.ObservationError != "" {
		result = "read_error"
	}
	o.workerAttempts.Add(ctx, 1, metric.WithAttributes(attribute.String("kind", "attachment"), attribute.String("result", result)))
	if err == nil && (p.Observed || p.ObservationError != "") {
		reachable := int64(1)
		if p.ObservationError != "" {
			reachable = 0
		}
		o.providerReachable.Record(ctx, reachable, metric.WithAttributes(attribute.String("kind", "attachment")))
	}
}

func (o *Observability) ObserveVirtualSwitchWork(ctx context.Context, logger *slog.Logger, work biz.VirtualSwitchWork, progress biz.VirtualSwitchProgress, err error) {
	if progress.ProviderError != nil {
		cause := progress.ProviderError
		kind := biz.ProviderFailure("")
		var failure *biz.ProviderError
		if errors.As(cause, &failure) {
			kind = failure.Kind
			if failure.Cause != nil {
				cause = failure.Cause
			}
		}
		level := slog.LevelError
		if kind == biz.ProviderConflict || kind == biz.ProviderInUse {
			level = slog.LevelWarn
		}
		// Node identity is a real Kubernetes coordinate; the removed
		// node_name/node_uid/binding_state/provider_failure fields have no schema.
		attrs := []any{"event", biz.EventStepFailed, "worker", biz.WorkerVirtualSwitch,
			"step", switchStep(progress.Action), "dependency", "kubernetes", "action", progress.Action,
			"resource_type", "virtual_switch", "resource_id", work.Switch.ID,
			"reason", string(progress.Reason), "cluster_id", work.Switch.ClusterID}
		if work.Switch.LastOperationID != "" {
			attrs = append(attrs, "operation_id", work.Switch.LastOperationID)
		}
		if work.Switch.BindingID != "" {
			attrs = append(attrs, "binding_id", work.Switch.BindingID)
		}
		if progress.NodeName != "" || progress.NodeUID != "" {
			attrs = append(attrs, telemetry.Target{ClusterID: work.Switch.ClusterID, Kind: "Node", Name: progress.NodeName, UID: progress.NodeUID}.KV()...)
		}
		attrs = append(attrs, logfields.ErrorFields(cause)...)
		logger.Log(ctx, level, "virtual switch provider request failed", attrs...)
	}
	result := progress.BindingState
	if progress.Observed {
		result = "observed"
	} else if progress.ProviderError != nil {
		result = "provider_error"
	}
	if errors.Is(err, biz.ErrLeaseLost) {
		result = "lease_lost"
	} else if err != nil {
		result = "database_error"
	}
	if err == nil && progress.ObservationError != "" {
		result = "read_error"
	}
	o.workerAttempts.Add(ctx, 1, metric.WithAttributes(attribute.String("kind", "virtual_switch"), attribute.String("result", result)))
	if err == nil && (progress.Observed || progress.ObservationError != "") {
		reachable := int64(1)
		if progress.ObservationError != "" {
			reachable = 0
		}
		o.providerReachable.Record(ctx, reachable, metric.WithAttributes(attribute.String("kind", "virtual_switch")))
	}
	if err == nil && work.Switch.BindingState == progress.BindingState && work.Switch.Reason == progress.Reason {
		return
	}
	event, level := biz.EventStepSucceeded, slog.LevelInfo
	if err != nil {
		event, level = biz.EventStepFailed, slog.LevelError
	} else if progress.ProviderError != nil {
		event, level = biz.EventStepWaiting, slog.LevelInfo
	}
	attrs := []any{"event", event, "worker", biz.WorkerVirtualSwitch, "step", switchStep(progress.Action),
		"resource_type", "virtual_switch", "resource_id", work.Switch.ID,
		"operation_id", work.Switch.LastOperationID, "binding_id", work.Switch.BindingID,
		"cluster_id", work.Switch.ClusterID, "action", progress.Action,
		"epoch", int(work.Epoch)}
	// step.waiting/step.failed require a non-blank reason; the empty-string
	// filter in the contract handler would otherwise drop the field entirely.
	reason := progress.Reason
	if reason == "" {
		reason = progress.OperationReason
	}
	if reason == "" && (event == biz.EventStepWaiting || event == biz.EventStepFailed) {
		reason = biz.ProviderNotReady
	}
	if reason != "" {
		attrs = append(attrs, "reason", string(reason))
	}
	switch event {
	case biz.EventStepSucceeded:
		attrs = append(attrs, "result", "succeeded")
	case biz.EventStepWaiting:
		attrs = append(attrs, "result", "waiting")
	case biz.EventStepFailed:
		attrs = append(attrs, "result", "failed")
	}
	if err != nil {
		attrs = append(attrs, logfields.ErrorFields(err)...)
	}
	logger.Log(ctx, level, "virtual switch reconciliation", attrs...)
}

// stepReason returns a non-blank reason for step.waiting/step.failed, which the
// contract marks required. The empty-string filter in the handler would
// otherwise drop the field and fail validation.
func stepReason(values ...biz.Reason) string {
	for _, v := range values {
		if v != "" {
			return string(v)
		}
	}
	return string(biz.ProviderNotReady)
}

// switchStep maps the persisted switch action to the fixed step vocabulary.
func switchStep(action string) string {
	if action == "patch" {
		return biz.StepApplySwitch
	}
	return biz.StepObserveSwitch
}

// logAttachmentFailure records a real attachment provider/read failure with the
// existing attachment/submission identity. The lane has no business operation,
// so operation_id is omitted rather than fabricated. Provider conflicts are the
// only class that stays WARN.
func (o *Observability) logAttachmentFailure(logger *slog.Logger, ctx context.Context, work biz.AttachmentWork, p biz.AttachmentProgress, err error) {
	if logger == nil {
		return
	}
	identity := []any{"worker", biz.WorkerAttachment, "resource_type", "attachment", "resource_id", work.Attachment.ID}
	if work.Attachment.ID != "" {
		identity = append(identity, "attachment_id", work.Attachment.ID)
	}
	if work.Attachment.SubmissionID != "" {
		identity = append(identity, "submission_id", work.Attachment.SubmissionID)
	}
	if work.Attachment.TenantID != "" {
		identity = append(identity, "tenant_id", work.Attachment.TenantID)
	}
	if p.ProviderError != nil || p.ObservationError != "" {
		level := slog.LevelError
		cause := error(p.ProviderError)
		kind := biz.ProviderFailure("")
		var failure *biz.ProviderError
		if errors.As(p.ProviderError, &failure) {
			kind = failure.Kind
			if failure.Cause != nil {
				cause = failure.Cause
			}
			if kind == biz.ProviderConflict || kind == biz.ProviderInUse {
				level = slog.LevelWarn
			}
		}
		if cause == nil && p.ObservationError != "" {
			cause = errors.New(string(p.ObservationError))
		}
		step := biz.StepGetSubmission
		if p.Addresses != nil || p.Observed {
			step = biz.StepObserveAttachment
		}
		attrs := append([]any{"event", biz.EventStepFailed, "step", step, "dependency", "kubernetes", "action", "attach",
			"reason", stepReason(p.Reason, p.ObservationError), "result", "failed"}, identity...)
		if work.Plan.Namespace != "" || p.PodName != "" {
			attrs = append(attrs, telemetry.Target{Kind: "Pod",
				Namespace: work.Plan.Namespace, Name: p.PodName, UID: p.PodUID}.KV()...)
		}
		if cause != nil {
			attrs = append(attrs, logfields.ErrorFields(cause)...)
		}
		logger.Log(ctx, level, "attachment provider request failed", attrs...)
	}
	// The Finish write failure is a separate cause and must keep its own
	// SQLSTATE instead of being replaced by the provider error above. A lost
	// lease is a concurrency outcome, not a persistence failure, and this lane
	// has no operation, so it is operation.conflict rather than lease_lost.
	if errors.Is(err, biz.ErrLeaseLost) {
		attrs := append([]any{"event", biz.EventOperationConflict, "step", biz.StepPersistAttachment,
			"reason", string(biz.LeaseLost)}, identity...)
		logger.WarnContext(ctx, "attachment lease lost", attrs...)
	} else if err != nil {
		attrs := append([]any{"event", biz.EventPersistFailed, "dependency", "postgresql",
			"action", "finish_attachment", "step", biz.StepPersistAttachment}, identity...)
		attrs = append(attrs, logfields.ErrorFields(err)...)
		logger.ErrorContext(ctx, "attachment state save failed", attrs...)
	}
}
