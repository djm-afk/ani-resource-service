package server

import (
	"context"
	"log/slog"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"

	"github.com/zhangzhe-ctrl/ani-resource-service/internal/telemetry"
)

// rpcServerStatsHandler wraps the standard OTel server handler: it delegates
// all span/tag work and emits exactly one rpc.server.completed per RPC.
type rpcServerStatsHandler struct {
	delegate stats.Handler
	logger   *slog.Logger
}

type serverCallKey struct{}

type serverCallState struct {
	method    string
	beginTime time.Time
}

// TagRPC returns the delegate's span context so later HandleRPC sees the server
// span, and remembers the full method for the completion record.
func (h *rpcServerStatsHandler) TagRPC(ctx context.Context, info *stats.RPCTagInfo) context.Context {
	ctx = h.delegate.TagRPC(ctx, info)
	return context.WithValue(ctx, serverCallKey{}, &serverCallState{
		method:    info.FullMethodName,
		beginTime: time.Now(),
	})
}

// HandleRPC records the single completion event on stats.End.
func (h *rpcServerStatsHandler) HandleRPC(ctx context.Context, s stats.RPCStats) {
	h.delegate.HandleRPC(ctx, s)
	end, ok := s.(*stats.End)
	if !ok || h.logger == nil {
		return
	}
	state, _ := ctx.Value(serverCallKey{}).(*serverCallState)
	method := ""
	begin := time.Now()
	if state != nil {
		method = state.method
		begin = state.beginTime
	}
	code := codes.OK
	if end.Error != nil {
		code = status.Code(end.Error)
	}
	duration := end.EndTime.Sub(begin)
	if duration < 0 {
		duration = 0
	}

	level := serverRPCLevel(method, code)
	attrs := []any{
		"event", "rpc.server.completed",
		"rpc.method", method,
		"rpc.code", code.String(),
		"result", serverRPCResult(code, false),
		"duration_ms", float64(duration.Microseconds()) / 1000.0,
	}
	if end.Error != nil {
		attrs = append(attrs, telemetry.ErrorFields(end.Error)...)
	}
	h.logAt(ctx, level, "inbound rpc completed", attrs...)
}

func (h *rpcServerStatsHandler) TagConn(ctx context.Context, info *stats.ConnTagInfo) context.Context {
	return h.delegate.TagConn(ctx, info)
}

func (h *rpcServerStatsHandler) HandleConn(ctx context.Context, s stats.ConnStats) {
	h.delegate.HandleConn(ctx, s)
}

func (h *rpcServerStatsHandler) logAt(ctx context.Context, level slog.Level, msg string, attrs ...any) {
	switch {
	case level < slog.LevelInfo:
		h.logger.DebugContext(ctx, msg, attrs...)
	case level < slog.LevelWarn:
		h.logger.InfoContext(ctx, msg, attrs...)
	case level < slog.LevelError:
		h.logger.WarnContext(ctx, msg, attrs...)
	default:
		h.logger.ErrorContext(ctx, msg, attrs...)
	}
}

// serverRPCLevel applies the fixed health-method exception then the code rule.
func serverRPCLevel(method string, code codes.Code) slog.Level {
	if isHealthMethod(method) {
		if code == codes.OK {
			return slog.LevelDebug
		}
		return slog.LevelWarn
	}
	return rpcCodeLevel(code, false)
}

// rpcCodeLevel applies the contract's gRPC code ladder.
func rpcCodeLevel(code codes.Code, callerCanceled bool) slog.Level {
	switch code {
	case codes.OK:
		return slog.LevelInfo
	case codes.InvalidArgument, codes.Unauthenticated, codes.PermissionDenied,
		codes.NotFound, codes.AlreadyExists, codes.FailedPrecondition,
		codes.Aborted, codes.OutOfRange, codes.ResourceExhausted:
		return slog.LevelWarn
	case codes.Canceled:
		if callerCanceled {
			return slog.LevelInfo
		}
		return slog.LevelError
	default:
		return slog.LevelError
	}
}

// serverRPCResult maps a code to the contract result vocabulary.
func serverRPCResult(code codes.Code, callerCanceled bool) string {
	switch code {
	case codes.OK:
		return "succeeded"
	case codes.Canceled:
		if callerCanceled {
			return "canceled"
		}
		return "failed"
	case codes.InvalidArgument, codes.Unauthenticated, codes.PermissionDenied,
		codes.NotFound, codes.AlreadyExists, codes.FailedPrecondition,
		codes.Aborted, codes.OutOfRange, codes.ResourceExhausted:
		return "rejected"
	default:
		return "failed"
	}
}

func isHealthMethod(method string) bool {
	return method == "/grpc.health.v1.Health/Check" || method == "/grpc.health.v1.Health/Watch"
}

// newServerStatsHandler builds the wrapper used by both listeners.
func newServerStatsHandler(logger *slog.Logger) *rpcServerStatsHandler {
	return &rpcServerStatsHandler{delegate: otelgrpc.NewServerHandler(), logger: logger}
}
