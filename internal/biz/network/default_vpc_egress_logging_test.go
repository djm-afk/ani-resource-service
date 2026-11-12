package biz

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

type loggingEgressRepo struct {
	DefaultEgressWorkRepository
	work      DefaultEgressWork
	finishErr error
}

func (r *loggingEgressRepo) ClaimDefaultVPCEgress(context.Context, string, time.Duration) (DefaultEgressWork, bool, error) {
	return r.work, true, nil
}

func (r *loggingEgressRepo) FinishDefaultVPCEgress(context.Context, DefaultEgressWork, DefaultEgressProgress) error {
	return r.finishErr
}

type loggingEgressProvider struct {
	DefaultEgressProvider
	observeErr error
}

func (p *loggingEgressProvider) ObserveDefaultVPCEgress(context.Context, DefaultEgressWork) (DefaultEgressObservation, error) {
	return DefaultEgressObservation{}, p.observeErr
}

func TestDefaultEgressLogsProviderAndPersistFailures(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	repo := &loggingEgressRepo{
		work: DefaultEgressWork{Egress: DefaultVPCEgress{
			EgressMetadata: EgressMetadata{ID: "eg-1", LastOperationID: "op-1"},
			Identity:       DefaultVPCIdentity{ClusterID: "cluster-1"},
		}},
		finishErr: errors.New("write failed: SQLSTATE 40001"),
	}
	provider := &loggingEgressProvider{observeErr: errors.New("apiserver unreachable")}
	worker, err := NewDefaultVPCEgressWorker(repo, provider, uuid.NewString(), DefaultWorkerPolicy())
	if err != nil {
		t.Fatal(err)
	}
	worker.SetLogger(logger)
	_, _ = worker.Step(context.Background())

	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("bad JSON %v: %s", err, line)
		}
		records = append(records, record)
	}
	find := func(event string) map[string]any {
		for _, record := range records {
			if record["event"] == event {
				return record
			}
		}
		return nil
	}
	providerFailure := find("dependency.failed")
	if providerFailure == nil {
		t.Fatalf("missing dependency.failed: %v", records)
	}
	if providerFailure["dependency"] != "kubernetes" || providerFailure["action"] != "observe_default_vpc_egress" ||
		providerFailure["operation_id"] != "op-1" || providerFailure["resource_id"] != "eg-1" {
		t.Errorf("provider failure fields = %v", providerFailure)
	}
	if msg, _ := providerFailure["error"].(string); !strings.Contains(msg, "apiserver unreachable") {
		t.Errorf("provider failure lost the cause: %v", providerFailure["error"])
	}

	persistFailure := find("state.persist_failed")
	if persistFailure == nil {
		t.Fatalf("missing state.persist_failed: %v", records)
	}
	if persistFailure["action"] != "finish_default_vpc_egress" || persistFailure["operation_id"] != "op-1" {
		t.Errorf("persist failure fields = %v", persistFailure)
	}
	if msg, _ := persistFailure["error"].(string); !strings.Contains(msg, "40001") {
		t.Errorf("persist failure lost the database cause: %v", persistFailure["error"])
	}
}
