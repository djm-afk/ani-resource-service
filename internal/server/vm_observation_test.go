package server_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/zhangzhe-ctrl/ani-resource-service/internal/server"
)

type failedVMObservation struct{ attempts chan time.Time }

func (s failedVMObservation) Step(context.Context) (bool, error) {
	s.attempts <- time.Now()
	return false, errors.New("postgres://private-user:private-password@private-host/vm")
}

func TestVMObservationDependencyFailureRetriesUntilStop(t *testing.T) {
	attempts := make(chan time.Time, 8)
	var logs bytes.Buffer
	s := server.NewVMObservationServer(failedVMObservation{attempts}, slog.New(slog.NewJSONHandler(&logs, nil)), 5*time.Millisecond)
	done := make(chan error, 1)
	go func() { done <- s.Start(context.Background()) }()
	var first time.Time
	for i := 0; i < 3; i++ {
		select {
		case at := <-attempts:
			if i == 0 {
				first = at
			} else if i == 2 && at.Sub(first) < 600*time.Millisecond {
				t.Fatalf("observation failures spun without backoff: %v", at.Sub(first))
			}
		case err := <-done:
			t.Fatalf("dependency failure terminated lifecycle server: %v", err)
		case <-time.After(2 * time.Second):
			t.Fatal("observation failure did not retry")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Stop(ctx); err != nil {
		t.Fatalf("observation could not stop during retry backoff: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("observation Start did not finish after Stop")
	}
	if text := logs.String(); !strings.Contains(text, "VM_OBSERVATION_UNAVAILABLE") || strings.Contains(text, "private-") || strings.Contains(text, "postgres://") {
		t.Fatalf("unsafe or missing observation diagnostic: %s", text)
	}
}
