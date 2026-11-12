package biz

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/zhangzhe-ctrl/ani-resource-service/internal/telemetry"
)

// NG18: a terminal pass must open a new trace and must not report missing
// metadata, while an unfinished operation still resumes the accepted trace and
// reports a malformed value once per pass.
func TestActiveRunTerminalVersusActive(t *testing.T) {
	initTraceForTest(t)
	active := changeTrace(t, "0af7651916cd43dd8448eb211c80319c", "b7ad6b7169203331")
	want := telemetry.Capture(active)

	// Active: resumes the accepted trace, opens its own span.
	ctx, span, event, err := activeRun(active, true, want, nil, "network.virtual_switch")
	if err != nil || event != "" {
		t.Fatalf("active resume = %q/%v", event, err)
	}
	if got := telemetry.Capture(ctx).OriginTraceID; got != want.OriginTraceID {
		t.Errorf("active pass trace = %s, want %s", got, want.OriginTraceID)
	}
	span.End()

	// Terminal: new trace; a stored value (valid or absent) must be ignored and
	// no context_missing/context_invalid event may be produced. The lane base has
	// no span of its own, so this proves the accepted trace is not reused.
	ctx, span, event, err = activeRun(context.Background(), false, want, nil, "network.virtual_switch")
	if err != nil || event != "" {
		t.Fatalf("terminal must not report metadata events, got %q/%v", event, err)
	}
	if got := telemetry.Capture(ctx).OriginTraceID; got == want.OriginTraceID {
		t.Errorf("terminal pass inherited the accepted trace: %s", got)
	}
	span.End()

	// Active with malformed metadata: one event, business continues.
	ctx, span, event, err = activeRun(active, true, telemetry.TelemetryContext{}, errMalformed, "network.security_group")
	if event != "telemetry.context_invalid" || err == nil {
		t.Fatalf("malformed active = %q/%v", event, err)
	}
	span.End()
	_ = ctx
}

var errMalformed = &malformedError{}

type malformedError struct{}

func (e *malformedError) Error() string { return "decode telemetry context: invalid" }

type terminalEgressRepo struct {
	DefaultEgressWorkRepository
	work DefaultEgressWork
}

func (r *terminalEgressRepo) ClaimDefaultVPCEgress(context.Context, string, time.Duration) (DefaultEgressWork, bool, error) {
	return r.work, true, nil
}

func (r *terminalEgressRepo) FinishDefaultVPCEgress(context.Context, DefaultEgressWork, DefaultEgressProgress) error {
	return nil
}

type terminalEgressProvider struct{ DefaultEgressProvider }

func (p *terminalEgressProvider) ObserveDefaultVPCEgress(context.Context, DefaultEgressWork) (DefaultEgressObservation, error) {
	return DefaultEgressObservation{Ready: boolPtr(true)}, nil
}

func boolPtr(v bool) *bool { return &v }

// TestTerminalDefaultEgressStartsNewTrace proves the default-egress lane no
// longer resumes the finished operation's trace (RECONCILIATION §2#12).
func TestTerminalDefaultEgressStartsNewTrace(t *testing.T) {
	initTraceForTest(t)
	accepted := changeTrace(t, "0af7651916cd43dd8448eb211c80319c", "b7ad6b7169203331")
	saved := telemetry.Capture(accepted)

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	repo := &terminalEgressRepo{work: DefaultEgressWork{
		Egress:          DefaultVPCEgress{EgressMetadata: EgressMetadata{ID: "eg-1", LastOperationID: "op-1"}, OperationState: "succeeded"},
		ActiveOperation: false, TelemetryContext: saved,
	}}
	worker, err := NewDefaultVPCEgressWorker(repo, &terminalEgressProvider{}, uuid.NewString(), DefaultWorkerPolicy())
	if err != nil {
		t.Fatal(err)
	}
	worker.SetLogger(logger)
	if _, err := worker.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	records := parseRecords(t, buf.String())
	for _, r := range records {
		if r["event"] == "telemetry.context_missing" || r["event"] == "telemetry.context_invalid" {
			t.Fatalf("terminal pass reported metadata problem: %v", r)
		}
	}
	if !hasEvent(records, "worker.run_started") || !hasEvent(records, "worker.run_completed") {
		t.Fatalf("terminal pass missing run events: %v", records)
	}
	// A terminal observation must NOT be attributed to the accepted trace.
	if id, _ := records[0]["trace_id"].(string); id == saved.OriginTraceID {
		t.Fatalf("terminal default egress inherited the accepted trace %s", id)
	}
}

func parseRecords(t *testing.T, output string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("bad JSON %v: %s", err, line)
		}
		out = append(out, record)
	}
	return out
}

func hasEvent(records []map[string]any, event string) bool {
	for _, r := range records {
		if r["event"] == event {
			return true
		}
	}
	return false
}
