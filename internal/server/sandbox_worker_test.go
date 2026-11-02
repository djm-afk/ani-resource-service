package server

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

type sandboxStepFunc func(context.Context) (bool, error)

func (f sandboxStepFunc) Step(ctx context.Context) (bool, error) { return f(ctx) }

func TestSandboxWorkerHasSeparateTenantProvisioningLane(t *testing.T) {
	started := make(chan struct{}, 1)
	sandboxRan := make(chan struct{}, 1)
	blocking := sandboxStepFunc(func(ctx context.Context) (bool, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return false, ctx.Err()
	})
	sandbox := sandboxStepFunc(func(context.Context) (bool, error) {
		select {
		case sandboxRan <- struct{}{}:
		default:
		}
		return false, nil
	})
	server := &SandboxWorkerServer{worker: sandbox, additional: []Stepper{blocking}, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), done: make(chan struct{})}
	finished := make(chan error, 1)
	go func() { finished <- server.Start(context.Background()) }()
	for _, ch := range []<-chan struct{}{started, sandboxRan} {
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
			t.Fatal("worker lane did not run")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := server.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
}
