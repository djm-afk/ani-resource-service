package main

import (
	"context"
	"encoding/base64"
	"log/slog"
	"os"
	"time"

	kratos "github.com/go-kratos/kratos/v3"
	networkv1 "github.com/zhangzhe-ctrl/ani-resource-service/api/network/v1"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	conf "github.com/zhangzhe-ctrl/ani-resource-service/internal/conf/v1"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/server"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/service/network"
)

// Explicit read composition: the real repository and use case, with no KC,
// observation or mutation worker. It never falls back to the legacy listener.
func runVPCRead(bc *conf.Bootstrap, logger *slog.Logger) error {
	if err := bc.Validate(); err != nil {
		return err
	}
	tlsConfig, err := server.GovernanceTLS(os.Getenv("ANI_NETWORK_CLIENT_CA"), os.Getenv("ANI_NETWORK_TLS_CERT"), os.Getenv("ANI_NETWORK_TLS_KEY"))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	repository, err := data.OpenPostgres(ctx, bc.Network.DatabaseDsn, data.Placement{ClusterID: bc.Network.ClusterId, NamespacePrefix: "tenant-"})
	cancel()
	if err != nil {
		return err
	}
	defer repository.Close()
	key, _ := base64.StdEncoding.DecodeString(bc.Network.CursorSigningKey)
	network, err := biz.NewNetwork(repository, key, bc.Network.Worker.StaleAfter.AsDuration(), time.Now)
	if err != nil {
		return err
	}
	readiness := server.NewReadiness(func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		return repository.CheckReady(ctx) == nil
	})
	obs, err := server.NewObservability(Name, Version, readiness)
	if err != nil {
		return err
	}
	defer func() {
		if err := obs.Shutdown(context.Background()); err != nil {
			logger.Error("observability shutdown", "error", err)
		}
	}()
	middlewares := obs.ServerMiddleware(logger)
	gs, err := server.NewGRPCServer(bc.Server.Grpc, readiness, tlsConfig, true, middlewares...)
	if err != nil {
		return err
	}
	networkv1.RegisterNetworkServiceServer(gs, service.NewNetworkService(network))
	admin := server.NewAdminServer(bc.Server.Admin, readiness, obs.Gatherer(), middlewares...)
	return kratos.New(kratos.ID(id), kratos.Name(Name), kratos.Version(Version), kratos.Logger(logger),
		kratos.Server(gs, admin),
		kratos.AfterStart(func(context.Context) error { readiness.Set(true); return nil }),
		kratos.BeforeStop(func(context.Context) error { readiness.Set(false); return nil }),
		kratos.StopTimeout(bc.Server.ShutdownTimeout.AsDuration())).Run()
}
