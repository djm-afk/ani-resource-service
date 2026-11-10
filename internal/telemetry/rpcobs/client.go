package rpcobs

import (
	"context"
	"log/slog"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"

	"github.com/zhangzhe-ctrl/ani-resource-service/internal/telemetry/logfields"
)

// ClientStatsHandler wraps a standard OTel client stats handler: it delegates
// all span/tag work and emits exactly one rpc.client.completed per RPC. The
// standard handler performs the W3C Inject at the real outbound boundary.
func ClientStatsHandler(peer string, logger *slog.Logger) stats.Handler {
	return &clientStatsHandler{delegate: otelgrpc.NewClientHandler(), peer: peer, logger: logger}
}

// ClientDialOptions returns the dial options that instrument one outbound
// connection without touching TLS, identity, timeout or retry settings.
func ClientDialOptions(peer string, logger *slog.Logger) []grpc.DialOption {
	return []grpc.DialOption{grpc.WithStatsHandler(ClientStatsHandler(peer, logger))}
}

type clientStatsHandler struct {
	delegate stats.Handler
	peer     string
	logger   *slog.Logger
}

type clientCallKey struct{}

type clientCallState struct {
	method    string
	beginTime time.Time
}

func (h *clientStatsHandler) TagRPC(ctx context.Context, info *stats.RPCTagInfo) context.Context {
	ctx = h.delegate.TagRPC(ctx, info)
	return context.WithValue(ctx, clientCallKey{}, &clientCallState{method: info.FullMethodName, beginTime: time.Now()})
}

func (h *clientStatsHandler) HandleRPC(ctx context.Context, s stats.RPCStats) {
	h.delegate.HandleRPC(ctx, s)
	end, ok := s.(*stats.End)
	if !ok || h.logger == nil {
		return
	}
	state, _ := ctx.Value(clientCallKey{}).(*clientCallState)
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
	level := rpcCodeLevel(code, false)
	attrs := []any{
		"event", "rpc.client.completed",
		"peer.service", h.peer,
		"rpc.method", method,
		"rpc.code", code.String(),
		"result", RPCResult(code, false),
		"duration_ms", float64(duration.Microseconds()) / 1000.0,
	}
	if end.Error != nil {
		attrs = append(attrs, logfields.ErrorFields(end.Error)...)
	}
	logAt(h.logger, ctx, level, "outbound rpc completed", attrs...)
	if IsDependencyFault(code) && end.Error != nil {
		failAttrs := append([]any{
			"event", "dependency.failed",
			"dependency", h.peer,
			"action", rpcAction(method),
			"rpc.method", method,
			"rpc.code", code.String(),
		}, logfields.ErrorFields(end.Error)...)
		h.logger.ErrorContext(ctx, "outbound dependency failed", failAttrs...)
	}
}

func (h *clientStatsHandler) TagConn(ctx context.Context, info *stats.ConnTagInfo) context.Context {
	return h.delegate.TagConn(ctx, info)
}

func (h *clientStatsHandler) HandleConn(ctx context.Context, s stats.ConnStats) {
	h.delegate.HandleConn(ctx, s)
}

// RPCResult maps a gRPC code to the contract result vocabulary.
func RPCResult(code codes.Code, callerCanceled bool) string {
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

// IsDependencyFault reports codes that mean the peer could not serve the call.
func IsDependencyFault(code codes.Code) bool {
	switch code {
	case codes.Unknown, codes.Internal, codes.Unavailable, codes.DeadlineExceeded,
		codes.DataLoss, codes.Unimplemented, codes.Canceled:
		return true
	}
	return false
}

// RPCCodeLevel applies the contract's gRPC code ladder.
func RPCCodeLevel(code codes.Code, callerCanceled bool) slog.Level {
	return rpcCodeLevel(code, callerCanceled)
}

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

func rpcAction(method string) string {
	for i := len(method) - 1; i >= 0; i-- {
		if method[i] == '/' {
			return method[i+1:]
		}
	}
	return method
}

func logAt(logger *slog.Logger, ctx context.Context, level slog.Level, msg string, attrs ...any) {
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
