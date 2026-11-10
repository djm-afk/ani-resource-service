package server

import (
	"context"
	"io"
	"log/slog"

	"github.com/go-kratos/kratos/v3/middleware"
)

// discardLogger returns a JSON logger that writes nowhere, for tests that only
// need a non-nil logger on the listener constructors.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// serverMiddlewareChain runs handler through the same middleware chain the gRPC
// listeners install, so request-body disclosure tests exercise the real path.
func serverMiddlewareChain(logger *slog.Logger, handler middleware.Handler) middleware.Handler {
	obs, err := NewObservability("ani-resource-service-test", "test", NewReadiness())
	if err != nil {
		// Fall back to an empty chain rather than hiding the failure: the test
		// would then assert on the wrong surface.
		panic(err)
	}
	chain := obs.ServerMiddleware(logger)
	for i := len(chain) - 1; i >= 0; i-- {
		handler = chain[i](handler)
	}
	return func(ctx context.Context, req any) (any, error) {
		return handler(ctx, req)
	}
}
