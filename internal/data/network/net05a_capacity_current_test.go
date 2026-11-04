package data_test

import (
	"context"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/server"
	"testing"
	"time"
)

const capacityVariant = "net05a"

type capacityMetrics interface{ Snapshot() map[string]float64 }

func configureCapacity(t *testing.T, p *data.KCProvider, execution *server.WorkerServer) (capacityMetrics, func(context.Context)) {
	t.Helper()
	o, e := p.EnableObservation(5 * time.Second)
	if e != nil {
		t.Fatal(e)
	}
	if e = execution.SetConcurrency(2); e != nil {
		t.Fatal(e)
	}
	return o, func(ctx context.Context) {
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- o.Start(ctx) }()
		e := execution.Start(ctx)
		cancel()
		<-done
		if e != nil {
			t.Error(e)
		}
	}
}
