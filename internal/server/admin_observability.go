package server

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/zhangzhe-ctrl/ani-resource-service/internal/telemetry/logfields"
)

// adminHeaderTraceID is the response header carrying the admin request trace.
const adminHeaderTraceID = "X-Trace-ID"

type adminCaptureKey struct{}

type adminCapture struct {
	err  error
	code int
}

// adminObservabilityFilter is the outermost filter for the admin HTTP listener.
// It covers health, metrics, 404/405 and panics with one trace and one
// completion record. Health success stays DEBUG; real dependency errors are
// logged separately by the handler that observed them.
func adminObservabilityFilter(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ctx, sc := ensureAdminSpan(r.Context())
			traceID := ""
			if sc.IsValid() {
				traceID = sc.TraceID().String()
				w.Header().Set(adminHeaderTraceID, traceID)
			}

			capture := &adminCapture{}
			ctx = context.WithValue(ctx, adminCaptureKey{}, capture)
			r = r.WithContext(ctx)
			rw := &adminResponseWriter{ResponseWriter: w, capture: capture}

			path := r.URL.Path
			startLevel := slog.LevelInfo
			if isAdminHealthPath(path) {
				startLevel = slog.LevelDebug
			}
			logAdmin(logger, ctx, startLevel, "admin request started",
				"event", "http.request.started", "http.method", r.Method, "http.path", path)

			defer func() {
				if rec := recover(); rec != nil {
					logger.ErrorContext(ctx, "admin request panicked",
						"event", "process.panic",
						"error", fmt.Sprintf("panic: %v", rec),
						"error_type", "panic", "error_source", "local",
						"stack", string(debug.Stack()),
						"http.path", path, "http.method", r.Method)
					if !rw.wroteHead {
						http.Error(rw, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
					}
				}
				code := rw.statusOr(200)
				attrs := []any{
					"event", "http.request.completed",
					"http.method", r.Method,
					"http.path", path,
					"http.status_code", code,
					"result", adminResult(code),
					"duration_ms", float64(time.Since(start).Microseconds()) / 1000.0,
				}
				if code >= 500 && capture.err != nil {
					attrs = append(attrs, logfields.ErrorFields(capture.err)...)
				}
				logAdmin(logger, ctx, adminLevel(path, code), "admin request completed", attrs...)
			}()

			next.ServeHTTP(rw, r)
		})
	}
}

func isAdminHealthPath(path string) bool {
	switch path {
	case "/healthz", "/readyz", "/metrics":
		return true
	}
	return false
}

func adminLevel(path string, code int) slog.Level {
	if isAdminHealthPath(path) {
		if code < 400 {
			return slog.LevelDebug
		}
		return slog.LevelWarn
	}
	switch {
	case code < 400:
		return slog.LevelInfo
	case code < 500:
		return slog.LevelWarn
	default:
		return slog.LevelError
	}
}

func adminResult(code int) string {
	switch {
	case code < 400:
		return "succeeded"
	case code < 500:
		return "rejected"
	default:
		return "failed"
	}
}

func logAdmin(logger *slog.Logger, ctx context.Context, level slog.Level, msg string, attrs ...any) {
	if logger == nil {
		return
	}
	switch {
	case level < slog.LevelInfo:
		logger.DebugContext(ctx, msg, attrs...)
	case level < slog.LevelWarn:
		logger.InfoContext(ctx, msg, attrs...)
	case level < slog.LevelError:
		logger.WarnContext(ctx, msg, attrs...)
	default:
		logger.ErrorContext(ctx, msg, attrs...)
	}
}

// ensureAdminSpan derives a valid span context so the entry always correlates,
// even if the tracer provider were a noop.
func ensureAdminSpan(ctx context.Context) (context.Context, trace.SpanContext) {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		return ctx, sc
	}
	var traceBytes [16]byte
	var spanBytes [8]byte
	_, _ = rand.Read(traceBytes[:])
	_, _ = rand.Read(spanBytes[:])
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID(traceBytes),
		SpanID:  trace.SpanID(spanBytes),
	})
	return trace.ContextWithSpanContext(ctx, sc), sc
}

type adminResponseWriter struct {
	http.ResponseWriter
	capture     *adminCapture
	writtenCode int
	wroteHead   bool
}

func (w *adminResponseWriter) WriteHeader(code int) {
	if !w.wroteHead {
		w.wroteHead = true
		w.writtenCode = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *adminResponseWriter) Write(b []byte) (int, error) {
	if !w.wroteHead {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

func (w *adminResponseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		if !w.wroteHead {
			w.WriteHeader(http.StatusOK)
		}
		f.Flush()
	}
}

func (w *adminResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *adminResponseWriter) statusOr(fallback int) int {
	if w.writtenCode != 0 {
		return w.writtenCode
	}
	return fallback
}

var (
	_ http.Flusher                              = (*adminResponseWriter)(nil)
	_ interface{ Unwrap() http.ResponseWriter } = (*adminResponseWriter)(nil)
)
