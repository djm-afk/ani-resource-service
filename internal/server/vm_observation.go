package server

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// VMObservationServer owns only background state projection. Its dependency
// failures neither stop command workers nor alter the API's shared readiness.
type VMObservationServer struct {
	stepper Stepper
	logger  *slog.Logger
	poll    time.Duration
	mu      sync.Mutex
	started bool
	stopped bool
	cancel  context.CancelFunc
	done    chan struct{}
}

func NewVMObservationServer(stepper Stepper, logger *slog.Logger, poll time.Duration) *VMObservationServer {
	return &VMObservationServer{stepper: stepper, logger: logger, poll: poll, done: make(chan struct{})}
}

func (s *VMObservationServer) Start(parent context.Context) error {
	s.mu.Lock()
	if s.started || s.stopped {
		s.mu.Unlock()
		return fmt.Errorf("VM observation server cannot be started twice or after stop")
	}
	s.started = true
	ctx, cancel := context.WithCancel(parent)
	s.cancel = cancel
	s.mu.Unlock()
	defer close(s.done)
	defer cancel()
	if s.stepper == nil || s.logger == nil || s.poll <= 0 {
		return fmt.Errorf("invalid VM observation runtime dependencies")
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	failures := 0
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}
		worked, err := s.stepper.Step(ctx)
		if ctx.Err() != nil {
			return nil
		}
		delay := s.poll
		if err != nil {
			if failures < 6 {
				failures++
			}
			delay = 250 * time.Millisecond * time.Duration(1<<uint(failures-1))
			if delay > 5*time.Second {
				delay = 5 * time.Second
			}
			// Storage errors can contain connection strings. Log a bounded reason
			// rather than the raw dependency error, and retry this lane only.
			s.logger.Warn("VM background observation attempt failed", "reason", "VM_OBSERVATION_UNAVAILABLE")
		} else {
			failures = 0
			if worked {
				delay = time.Millisecond
			}
		}
		timer.Reset(delay)
	}
}

func (s *VMObservationServer) Stop(ctx context.Context) error {
	s.mu.Lock()
	s.stopped = true
	started, cancel := s.started, s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if !started {
		return nil
	}
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
