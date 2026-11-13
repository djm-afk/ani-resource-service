package telemetry

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	traceSdk "go.opentelemetry.io/otel/sdk/trace"
)

// InstallDefault installs the process-wide TracerProvider and W3C propagator
// that StartRun/Capture depend on. The service process installs them as part of
// its observability wiring; a standalone command has no such wiring, so without
// this call every span it opens is a no-op and every captured context is empty.
func InstallDefault() func(context.Context) error {
	provider := traceSdk.NewTracerProvider(
		traceSdk.WithSampler(traceSdk.ParentBased(traceSdk.AlwaysSample())),
	)
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	return provider.Shutdown
}
