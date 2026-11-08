package server

import (
	"context"
	"crypto/tls"
	"fmt"
	"time"

	"github.com/go-kratos/kratos/v3/middleware"
	"github.com/go-kratos/kratos/v3/transport"
	kratosgrpc "github.com/go-kratos/kratos/v3/transport/grpc"
	clusteropsbiz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/clusterops"

	conf "github.com/zhangzhe-ctrl/ani-resource-service/internal/conf/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
)

// NewGRPCServer is the sole internal business listener constructor. Its
// transport and caller policy are shared by full, governance and vpc-read
// compositions; changing composition never enables a plaintext bypass.
func NewGRPCServer(c *conf.Server_GRPC, readiness *Readiness, tlsConfig *tls.Config, readOnly bool, middlewares ...middleware.Middleware) (*kratosgrpc.Server, error) {
	if c == nil || tlsConfig == nil || tlsConfig.MinVersion < tls.VersionTLS13 ||
		tlsConfig.ClientAuth != tls.RequireAndVerifyClientCert || tlsConfig.ClientCAs == nil ||
		len(tlsConfig.Certificates) == 0 || tlsConfig.VerifyConnection == nil {
		return nil, fmt.Errorf("internal business listener requires configured TLS 1.3 mutual authentication")
	}
	chain := make([]middleware.Middleware, 0, len(middlewares)+1)
	chain = append(chain, unaryRequestTimeout(c.Timeout.AsDuration()))
	chain = append(chain, middlewares...)
	server := kratosgrpc.NewServer(
		kratosgrpc.CustomHealth(),
		kratosgrpc.Network(c.Network),
		kratosgrpc.Address(c.Addr),
		kratosgrpc.Timeout(0),
		kratosgrpc.TLSConfig(tlsConfig.Clone()),
		kratosgrpc.UnaryInterceptor(governanceUnary(readOnly)),
		kratosgrpc.StreamInterceptor(GovernanceStreams(readOnly)),
		kratosgrpc.Options(grpc.UnknownServiceHandler(func(any, grpc.ServerStream) error {
			return status.Error(codes.PermissionDenied, "internal RPC is not registered")
		})),
		kratosgrpc.Middleware(chain...),
		kratosgrpc.DisableReflection(),
	)
	grpc_health_v1.RegisterHealthServer(server, &networkHealth{readiness: readiness})
	return server, nil
}

// Preserve the configured unary budget in every composition. The exact
// administrator methods need their own 30s budget before logging and handlers.
func unaryRequestTimeout(defaultTimeout time.Duration) middleware.Middleware {
	return func(next middleware.Handler) middleware.Handler {
		return func(ctx context.Context, req any) (any, error) {
			budget := defaultTimeout
			if tr, ok := transport.FromServerContext(ctx); ok {
				if _, ok := clusterOpsPlatformMethods[tr.Operation()]; ok {
					budget = clusteropsbiz.UnaryTimeout
				}
			}
			if budget > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, budget)
				defer cancel()
			}
			return next(ctx, req)
		}
	}
}
