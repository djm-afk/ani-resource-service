package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/telemetry/logfields"
)

type DatabaseReadiness interface{ CheckReady(context.Context) error }

// WorkerServer participates in the same Kratos lifecycle as the transports.
// It owns no task queue; stopping it leaves durable work for the next process.
type WorkerServer struct {
	worker           Stepper
	additional       []Stepper
	database         DatabaseReadiness
	logger           *slog.Logger
	poll             time.Duration
	concurrency      int
	mu               sync.Mutex
	started, stopped bool
	cancel           context.CancelFunc
	done             chan struct{}
	alive            atomic.Bool
	databaseReady    atomic.Bool
	databaseChecked  atomic.Int64
}

type Stepper interface {
	Step(context.Context) (bool, error)
}

func NewWorkerServer(worker Stepper, database DatabaseReadiness, logger *slog.Logger, poll time.Duration, additional ...Stepper) *WorkerServer {
	return &WorkerServer{worker: worker, database: database, logger: logger, poll: poll, concurrency: 1, additional: additional, done: make(chan struct{})}
}
func (s *WorkerServer) Ready() bool {
	return s.alive.Load() && s.databaseReady.Load() && time.Since(time.Unix(0, s.databaseChecked.Load())) < 3*time.Second
}
func (s *WorkerServer) Start(parent context.Context) (err error) {
	s.mu.Lock()
	if s.started || s.stopped {
		s.mu.Unlock()
		return fmt.Errorf("worker cannot be started twice or after stop")
	}
	s.started = true
	ctx, cancel := context.WithCancel(parent)
	s.cancel = cancel
	s.mu.Unlock()
	defer func() {
		s.alive.Store(false)
		cancel()
		close(s.done)
		if recover() != nil {
			err = fmt.Errorf("network worker terminated unexpectedly")
		}
	}()
	if s.worker == nil || s.database == nil || s.logger == nil || s.poll <= 0 {
		return fmt.Errorf("invalid worker runtime dependencies")
	}
	healthDone := make(chan struct{})
	go func() { defer close(healthDone); s.monitorDatabase(ctx) }()
	defer func() { cancel(); <-healthDone }()
	s.alive.Store(true)
	// Each resource lane has reserved capacity; owner RPCs cannot consume it.
	lanes := append([]Stepper{s.worker}, s.additional...)
	results := make(chan error, len(lanes)*s.concurrency)
	var group sync.WaitGroup
	for _, stepper := range lanes {
		for i := 0; i < s.concurrency; i++ {
			group.Add(1)
			go func() { defer group.Done(); results <- s.runLane(ctx, stepper) }()
		}
	}
	select {
	case <-ctx.Done():
	case err = <-results:
	}
	cancel()
	group.Wait()
	return err
}

// SetConcurrency is only valid before Start. It bounds each kind separately.
func (s *WorkerServer) SetConcurrency(n int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started || n < 1 || n > 4 {
		return fmt.Errorf("invalid worker concurrency")
	}
	s.concurrency = n
	return nil
}
func (s *WorkerServer) runLane(ctx context.Context, stepper Stepper) (err error) {
	startedAt := time.Now()
	worker := LaneName(stepper)
	defer func() {
		// Capture the real panic value: the previous code discarded it and left
		// only a fixed sentence, so run_failed could not report the cause.
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("network worker terminated unexpectedly: %v", recovered)
		}
		if err != nil {
			attrs := append([]any{"event", biz.EventRunFailed, "worker", worker,
				"duration_ms", float64(time.Since(startedAt).Microseconds()) / 1000.0}, logfields.ErrorFields(err)...)
			s.logger.Error("network worker lane exited", attrs...)
		}
	}()
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}
		worked, stepErr := stepper.Step(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if stepErr != nil {
			if biz.ReasonOf(stepErr) != biz.DependencyUnavailable && !errors.Is(stepErr, context.DeadlineExceeded) {
				return fmt.Errorf("network worker execution failed: %w", stepErr)
			}
			s.databaseReady.Store(false)
			s.logger.Warn("network worker database attempt failed", "reason", string(biz.DependencyUnavailable))
		}
		delay := s.poll
		if worked && stepErr == nil {
			delay = time.Millisecond
		}
		timer.Reset(delay)
	}
}

// laneNames maps each registered lane to its contract worker identity.
var laneNames = map[string]string{
	"*biz.Worker":                 biz.WorkerMain,
	"*biz.AttachmentWorker":       biz.WorkerAttachment,
	"*biz.VirtualSwitchWorker":    biz.WorkerVirtualSwitch,
	"*biz.SecurityGroupWorker":    biz.WorkerSecurityGroup,
	"*biz.DefaultVPCEgressWorker": biz.WorkerDefaultEgress,
}

// LaneName resolves a lane's fixed worker name; an unknown lane keeps the main
// identity rather than inventing a new one.
func LaneName(stepper Stepper) string {
	if stepper == nil {
		return biz.WorkerMain
	}
	name := fmt.Sprintf("%T", stepper)
	if mapped, ok := laneNames[name]; ok {
		return mapped
	}
	return biz.WorkerMain
}

func (s *WorkerServer) monitorDatabase(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		probe, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		err := s.database.CheckReady(probe)
		cancel()
		s.databaseReady.Store(err == nil)
		s.databaseChecked.Store(time.Now().UnixNano())
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (s *WorkerServer) Stop(ctx context.Context) error {
	s.mu.Lock()
	s.stopped = true
	cancel := s.cancel
	started := s.started
	s.mu.Unlock()
	s.alive.Store(false)
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
