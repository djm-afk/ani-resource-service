package vm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/zhangzhe-ctrl/ani-resource-service/internal/telemetry"
)

// stubRepo returns one claimed operation, then reports "nothing to do". It
// records every Save so a test can assert durable state and inspect errors.
type stubRepo struct {
	Repository
	op      Operation
	vm      VM
	saveErr error
	claimed bool
	saves   []Operation
}

func (s *stubRepo) Claim(context.Context, string, time.Duration) (Operation, VM, error) {
	if s.claimed {
		return Operation{}, VM{}, ErrNotFound
	}
	s.claimed = true
	return s.op, s.vm, nil
}

func (s *stubRepo) Save(_ context.Context, o Operation, _ VM) error {
	s.saves = append(s.saves, o)
	return s.saveErr
}

func (s *stubRepo) Checkpoint(context.Context, Operation, VM) error { return nil }

// failingNative fails the first native call Create makes, with a real provider
// error, and embeds the interface so the rest stays unimplemented on purpose.
type failingNative struct {
	NativePort
	err error
}

func (f *failingNative) ValidateSource(context.Context, Source) error { return f.err }

type noopNetwork struct{ NetworkPort }

func decodeRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("bad JSON: %v\n%s", err, line)
		}
		out = append(out, m)
	}
	return out
}

func findEvent(recs []map[string]any, name string) map[string]any {
	for _, r := range recs {
		if r["event"] == name {
			return r
		}
	}
	return nil
}

func traceTelemetry() telemetry.TelemetryContext {
	traceID, _ := trace.TraceIDFromHex("0af7651916cd43dd8448eb211c80319c")
	spanID, _ := trace.SpanIDFromHex("b7ad6b7169203331")
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled})
	return telemetry.Capture(trace.ContextWithSpanContext(context.Background(), sc))
}

func TestRunOnceRecordsStepFailedEvenWhenSaveReturnsNil(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	repo := &stubRepo{
		op: Operation{TenantID: "t-1", ID: "op-1", VMID: "v-1", Action: "create", State: "accepted", TelemetryContext: traceTelemetry()},
		vm: VM{TenantID: "t-1", ID: "v-1", Name: "vm-a"},
	}
	w := &Worker{repo: repo, logger: logger, native: &failingNative{err: errors.New("cannot get persistentvolumeclaims")}, network: noopNetwork{}}

	// RunOnce swallows the dependency error after saving state; it must not return it.
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce returned %v; native failures must not escape", err)
	}
	if len(repo.saves) != 1 {
		t.Fatalf("expected one save, got %d", len(repo.saves))
	}
	recs := decodeRecords(t, &buf)
	stepFailed := findEvent(recs, "step.failed")
	if stepFailed == nil {
		t.Fatalf("failure was written to state and RunOnce returned nil, but no step.failed ERROR: %v", recs)
	}
	// The captured request trace must be attached to the run record.
	if stepFailed["operation_id"] != "op-1" {
		t.Errorf("step.failed lost the operation identity: %v", stepFailed)
	}
	if stepFailed["error"] == nil {
		t.Errorf("step.failed must retain the real error: %v", stepFailed)
	}
}

type waitingNative struct {
	NativePort
	observation Observation
}

func (n *waitingNative) ObserveLifecycle(context.Context, VM) (Observation, error) {
	return n.observation, nil
}

func TestRunOnceLogsKnownWaitAsInfoWithoutStepFailed(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	repo := &stubRepo{
		op: Operation{TenantID: "t-1", ID: "op-1", VMID: "v-1", Action: "restart", Step: "network-stopping"},
		vm: VM{TenantID: "t-1", ID: "v-1", Name: "vm-a"},
	}
	w := &Worker{repo: repo, logger: logger, native: &waitingNative{observation: Observation{Stopped: false}}, network: noopNetwork{}}
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce returned %v; a known wait must not escape", err)
	}
	recs := decodeRecords(t, &buf)
	waiting := findEvent(recs, "step.waiting")
	if waiting == nil {
		t.Fatalf("normal restart wait was not recorded as step.waiting: %v", recs)
	}
	if waiting["level"] != "INFO" || waiting["step"] != "network-stopping" || waiting["reason"] != "VM_RESTART_WAITING_OLD_INSTANCE" {
		t.Errorf("step.waiting fields = %v", waiting)
	}
	if findEvent(recs, "step.failed") != nil {
		t.Errorf("normal wait must not be logged as step.failed: %v", recs)
	}
}

func TestRunOnceRealDependencyFailureStillLogsStepFailed(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	repo := &stubRepo{
		op: Operation{TenantID: "t-1", ID: "op-1", VMID: "v-1", Action: "create"},
		vm: VM{TenantID: "t-1", ID: "v-1", Name: "vm-a"},
	}
	w := &Worker{repo: repo, logger: logger, native: &failingNative{err: errors.New("apiserver unreachable")}, network: noopNetwork{}}
	_ = w.RunOnce(context.Background())
	if findEvent(decodeRecords(t, &buf), "step.failed") == nil {
		t.Fatal("a real dependency failure must stay an ERROR step.failed")
	}
}

func TestRunOnceReasonIsStableAndRedacted(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	repo := &stubRepo{
		op: Operation{TenantID: "t-1", ID: "op-1", VMID: "v-1", Action: "create"},
		vm: VM{TenantID: "t-1", ID: "v-1", Name: "vm-a"},
	}
	w := &Worker{repo: repo, logger: logger,
		native:  &failingNative{err: errors.New("provider call failed: token=sk-live-SECRETVALUE")},
		network: noopNetwork{},
	}
	_ = w.RunOnce(context.Background())

	recs := decodeRecords(t, &buf)
	scheduled := findEvent(recs, "operation.retry_scheduled")
	if scheduled == nil {
		t.Fatalf("missing retry scheduled record: %v", recs)
	}
	reason, _ := scheduled["reason"].(string)
	if !strings.HasPrefix(reason, "VM_") {
		t.Errorf("retry reason = %q, want a stable VM_* classification", reason)
	}
	if strings.Contains(reason, "SECRETVALUE") || strings.Contains(reason, "provider call failed") {
		t.Errorf("retry reason leaked the raw error: %q", reason)
	}
	failed := findEvent(recs, "step.failed")
	if failed == nil {
		t.Fatal("missing step.failed")
	}
	if msg, _ := failed["error"].(string); strings.Contains(msg, "SECRETVALUE") {
		t.Errorf("step.failed error was not redacted: %q", msg)
	}
}

func TestRunOnceRecordsPersistFailureSeparately(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	repo := &stubRepo{
		op:      Operation{TenantID: "t-1", ID: "op-1", VMID: "v-1", Action: "create", TelemetryContext: traceTelemetry()},
		vm:      VM{TenantID: "t-1", ID: "v-1", Name: "vm-a"},
		saveErr: errors.New("db write failed"),
	}
	w := &Worker{repo: repo, logger: logger, native: &failingNative{err: errors.New("provider down")}, network: noopNetwork{}}
	_ = w.RunOnce(context.Background())

	recs := decodeRecords(t, &buf)
	if findEvent(recs, "step.failed") == nil {
		t.Error("missing step.failed for the original provider error")
	}
	persist := findEvent(recs, "state.persist_failed")
	if persist == nil {
		t.Fatalf("missing state.persist_failed; both errors must be visible: %v", recs)
	}
	if persist["error"] == nil {
		t.Error("state.persist_failed must keep the DB error")
	}
}

func TestRunOnceRecordsMissedTelemetryContext(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	repo := &stubRepo{
		op: Operation{TenantID: "t-1", ID: "op-1", VMID: "v-1", Action: "create"}, // no telemetry
		vm: VM{TenantID: "t-1", ID: "v-1", Name: "vm-a"},
	}
	w := &Worker{repo: repo, logger: logger, native: &failingNative{err: errors.New("x")}, network: noopNetwork{}}
	_ = w.RunOnce(context.Background())

	if findEvent(decodeRecords(t, &buf), "telemetry.context_missing") == nil {
		t.Fatal("missing telemetry context must be recorded")
	}
}

func TestRunOnceInvalidTelemetryDoesNotStopBusiness(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	repo := &stubRepo{
		op: Operation{TenantID: "t-1", ID: "op-1", VMID: "v-1", Action: "create",
			TelemetryContextError: errors.New("bad telemetry")},
		vm: VM{TenantID: "t-1", ID: "v-1", Name: "vm-a"},
	}
	w := &Worker{repo: repo, logger: logger, native: &failingNative{err: errors.New("x")}, network: noopNetwork{}}
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("bad telemetry must not fail the run: %v", err)
	}
	if len(repo.saves) != 1 {
		t.Fatal("business work did not continue after bad telemetry")
	}
	if findEvent(decodeRecords(t, &buf), "telemetry.context_invalid") == nil {
		t.Error("malformed telemetry must be recorded")
	}
}
