package server

import (
	"context"
	"log/slog"
	"sync"
	"time"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/sandbox"
)

type SandboxWorkerServer struct {
	worker     Stepper
	additional []Stepper
	logger     *slog.Logger
	mu         sync.Mutex
	cancel     context.CancelFunc
	done       chan struct{}
}

func NewSandboxWorkerServer(worker *biz.Worker, logger *slog.Logger, additional ...Stepper) *SandboxWorkerServer {
	return &SandboxWorkerServer{worker: worker, additional: additional, logger: logger, done: make(chan struct{})}
}
func (s *SandboxWorkerServer) Start(parent context.Context) error {
	s.mu.Lock()
	ctx, cancel := context.WithCancel(parent)
	s.cancel = cancel
	s.mu.Unlock()
	defer close(s.done)
	var group sync.WaitGroup
	for _, worker := range s.additional {
		group.Add(1)
		go func() { defer group.Done(); s.runLane(ctx, worker) }()
	}
	defer func() { cancel(); group.Wait() }()
	s.runLane(ctx, s.worker)
	return nil
}

func (s *SandboxWorkerServer) runLane(ctx context.Context, worker Stepper) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		worked, err := worker.Step(ctx)
		if ctx.Err() != nil {
			return
		}
		delay := time.Second
		if err != nil {
			s.logger.Warn("sandbox worker attempt failed", "error", err)
			delay = 5 * time.Second
		} else if worked {
			delay = time.Millisecond
		}
		timer.Reset(delay)
	}
}
func (s *SandboxWorkerServer) Stop(ctx context.Context) error {
	s.mu.Lock()
	cancel := s.cancel
	s.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
