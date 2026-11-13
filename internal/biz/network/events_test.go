package biz

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/zhangzhe-ctrl/ani-resource-service/internal/telemetry"
)

// TestNetworkEventEmitsContractFields covers the shared record shape: empty
// strings never reach the encoder, and the grouped target survives as an object.
func TestNetworkEventEmitsContractFields(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	identity := []any{"resource_type", "vpc", "resource_id", "vpc-1", "tenant_id", ""}
	stepFailed(context.Background(), logger, WorkerMain, StepObserveProvider, identity,
		k8sTarget{kind: "VPC", namespace: "ns-1", name: "vpc-a", uid: "uid-1"},
		&ProviderError{Kind: ProviderTemporary, Cause: errText("apiserver unreachable")}, ProviderUnavailable)

	var record map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(buf.String())), &record); err != nil {
		t.Fatalf("bad JSON: %v %s", err, buf.String())
	}
	if record["event"] != "step.failed" || record["worker"] != WorkerMain || record["step"] != StepObserveProvider {
		t.Errorf("identity fields wrong: %v", record)
	}
	if record["reason"] != string(ProviderUnavailable) {
		t.Errorf("reason = %v", record["reason"])
	}
	if _, present := record["tenant_id"]; present {
		t.Errorf("empty tenant_id must be dropped: %v", record)
	}
	if !strings.Contains(record["error"].(string), "apiserver unreachable") {
		t.Errorf("unwrapped provider cause lost: %v", record["error"])
	}
	target, ok := record["target"].(map[string]any)
	if !ok {
		t.Fatalf("grouped target missing: %v", record)
	}
	if target["kind"] != "VPC" || target["name"] != "vpc-a" || target["uid"] != "uid-1" {
		t.Errorf("target fields = %v", target)
	}
}

// TestTargetValueObjectRendersGroup confirms the k8sTarget adapter uses the
// shared telemetry value object rather than inventing field names.
func TestTargetValueObjectRendersGroup(t *testing.T) {
	kv := k8sTarget{clusterID: "cluster-a", kind: "Node", name: "node-a"}.kv()
	if len(kv) != 1 {
		t.Fatalf("expected one grouped attr, got %v", kv)
	}
	if _, ok := kv[0].(slog.Attr); !ok {
		t.Fatalf("expected slog.Attr, got %T", kv[0])
	}
	group := kv[0].(slog.Attr).Value.Group()
	keys := map[string]string{}
	for _, a := range group {
		keys[a.Key] = a.Value.String()
	}
	if keys["kind"] != "Node" || keys["name"] != "node-a" || keys["cluster_id"] != "cluster-a" {
		t.Errorf("group = %v", keys)
	}
}

func TestTelemetryTargetEmptyProducesNothing(t *testing.T) {
	if kv := (k8sTarget{}).kv(); kv != nil {
		t.Errorf("empty target must render nothing, got %v", kv)
	}
	if kv := (telemetry.Target{}).KV(); kv != nil {
		t.Errorf("empty value object must render nothing, got %v", kv)
	}
}

// TestNetworkEventHasNoDuplicateKeys composes a helper's own fields with a lane
// identity that repeats the same keys, then requires the encoded record to carry
// each key once. The JSON contract rejects duplicate keys, so this is NG27's
// earliest detection point.
func TestNetworkEventHasNoDuplicateKeys(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	// A real Work identity already carries action/operation_id/resource_*.
	work := Work{
		Resource:  ResourceWork{Kind: "vpc", ID: "vpc-1", TenantID: "tenant-1"},
		Operation: Operation{ID: "op-1", Kind: "create_vpc"},
		BindingID: "binding-1",
		Epoch:     3,
	}
	cases := []struct {
		name  string
		emit  func()
		check func(t *testing.T, record map[string]any)
	}{
		{
			name: "operation.started repeats identity keys",
			emit: func() {
				networkEvent(context.Background(), logger, slog.LevelInfo, EventOperationStarted,
					append([]any{"operation_id", work.Operation.ID, "action", work.Operation.Kind,
						"resource_type", work.Resource.Kind, "resource_id", work.Resource.ID,
						"tenant_id", work.Resource.TenantID}, work.eventIdentity()...)...)
			},
			check: func(t *testing.T, r map[string]any) {
				if r["event"] != EventOperationStarted || r["operation_id"] != "op-1" {
					t.Errorf("record = %v", r)
				}
			},
		},
		{
			name: "dependency.failed repeats action",
			emit: func() {
				dependencyFailed(context.Background(), logger, WorkerMain, "kubernetes", work.Operation.Kind,
					work.eventIdentity(), k8sTarget{kind: "VPC"}, errText("boom"))
			},
			check: func(t *testing.T, r map[string]any) {
				if r["action"] != "create_vpc" || r["dependency"] != "kubernetes" {
					t.Errorf("record = %v", r)
				}
			},
		},
		{
			name: "operation.failed repeats operation_id",
			emit: func() {
				operationFailed(context.Background(), logger, WorkerMain, work.eventIdentity(),
					work.Operation.ID, ProviderRejected, errText("rejected"))
			},
			check: func(t *testing.T, r map[string]any) {
				if r["operation_id"] != "op-1" || r["result"] != "failed" {
					t.Errorf("record = %v", r)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf.Reset()
			tc.emit()
			line := strings.TrimSpace(buf.String())
			// Strict decode: duplicate keys become a hard error.
			var record map[string]any
			dec := json.NewDecoder(strings.NewReader(line))
			if err := dec.Decode(&record); err != nil {
				t.Fatalf("strict decode failed (duplicate key?): %v\n%s", err, line)
			}
			dup, err := hasDuplicateKey(line)
			if err != nil {
				t.Fatalf("scan keys: %v", err)
			}
			if dup != "" {
				t.Fatalf("duplicate JSON key %q: %s", dup, line)
			}
			tc.check(t, record)
		})
	}
}

// hasDuplicateKey walks the top-level tokens and returns the first repeated key.
func hasDuplicateKey(line string) (string, error) {
	dec := json.NewDecoder(strings.NewReader(line))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return "", err
	}
	seen := map[string]struct{}{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return "", err
		}
		key, ok := tok.(string)
		if !ok {
			return "", nil
		}
		if _, dup := seen[key]; dup {
			return key, nil
		}
		seen[key] = struct{}{}
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return "", err
		}
	}
	return "", nil
}

type errText string

func (e errText) Error() string { return string(e) }

// TestStepFailureAndWaitingAlwaysCarryReason is a real-machine regression:
// step.waiting/step.failed require reason per the contract, and the handler
// drops empty strings, so an omitted reason produced a validation failure.
func TestStepFailureAndWaitingAlwaysCarryReason(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	identity := []any{"resource_type", "vpc", "resource_id", "vpc-1"}

	cases := []struct {
		name  string
		emit  func()
		wants map[string]any
	}{
		{
			name: "step.failed with blank reason",
			emit: func() {
				stepFailed(context.Background(), logger, WorkerMain, StepEnsureVPC, identity, k8sTarget{}, errText("boom"), "")
			},
			wants: map[string]any{"event": "step.failed", "result": "failed"},
		},
		{
			name: "step.waiting with blank reason",
			emit: func() {
				stepWaiting(context.Background(), logger, WorkerMain, StepObserveProvider, identity, "")
			},
			wants: map[string]any{"event": "step.waiting", "result": "waiting"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf.Reset()
			tc.emit()
			var r map[string]any
			if err := json.Unmarshal([]byte(strings.TrimSpace(buf.String())), &r); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if reason, _ := r["reason"].(string); reason == "" {
				t.Fatalf("required reason missing: %v", r)
			}
			for k, want := range tc.wants {
				if r[k] != want {
					t.Errorf("%s = %v, want %v", k, r[k], want)
				}
			}
		})
	}
}

// TestStageOutcomeNeverReportsSuccessForAFailedObservation is review finding P1:
// the persistence result used to decide the stage event, so a saved backoff
// emitted step.succeeded on the same trace that carried the provider error.
func TestStageOutcomeNeverReportsSuccessForAFailedObservation(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	identity := []any{"resource_type", "vpc", "resource_id", "vpc-1"}

	cases := []struct {
		name     string
		progress Progress
		stageErr error
		want     string
		notWant  string
	}{
		{
			name:     "provider failure with saved backoff",
			progress: Progress{ProviderError: &ProviderError{Kind: ProviderTemporary, Cause: errText("apiserver 403")}, OperationReason: ProviderUnavailable, Backoff: true},
			want:     "step.failed",
			notWant:  "step.succeeded",
		},
		{
			name:     "conflict is a warn concurrency outcome",
			progress: Progress{ProviderError: &ProviderError{Kind: ProviderConflict}, OperationReason: ProviderOwnership, Backoff: true},
			want:     "operation.conflict",
			notWant:  "step.succeeded",
		},
		{
			name:     "backoff without a provider error is waiting",
			progress: Progress{OperationReason: ProviderNotReady, Backoff: true},
			want:     "step.waiting",
			notWant:  "step.succeeded",
		},
		{
			name:     "lost lease is not a failed stage",
			progress: Progress{},
			stageErr: ErrLeaseLost,
			want:     "operation.lease_lost",
			notWant:  "step.failed",
		},
		{
			name:     "a clean observation succeeds",
			progress: Progress{Observed: true},
			want:     "step.succeeded",
			notWant:  "step.failed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf.Reset()
			reportStageOutcome(context.Background(), logger, WorkerMain, StepPlatformNetwork, identity, k8sTarget{}, tc.progress, tc.stageErr)
			var r map[string]any
			if err := json.Unmarshal([]byte(strings.TrimSpace(buf.String())), &r); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if r["event"] != tc.want {
				t.Errorf("event = %v, want %v", r["event"], tc.want)
			}
			if tc.notWant != "" && r["event"] == tc.notWant {
				t.Errorf("emitted %s, which contradicts the recorded result", tc.notWant)
			}
		})
	}
}

// TestSecurityGroupTerminalMatchesPersistedState is review finding P1: the worker
// used to announce operation.failed for every ProviderReject, while the
// repository persists deleting targets as blocked and observation failures as
// retrying.
func TestSecurityGroupTerminalMatchesPersistedState(t *testing.T) {
	reject := &ProviderError{Kind: ProviderReject}
	cases := []struct {
		name   string
		work   SecurityGroupWork
		result SecurityGroupResult
		err    error
		want   bool
	}{
		{"create rejected", SecurityGroupWork{TargetType: "security_group", Group: SecurityGroup{}}, SecurityGroupResult{}, reject, true},
		{"deleting target stays blocked", SecurityGroupWork{TargetType: "security_group", Group: SecurityGroup{EgressMetadata: EgressMetadata{State: Deleting}}}, SecurityGroupResult{}, reject, false},
		{"observation error stays retrying", SecurityGroupWork{TargetType: "security_group"}, SecurityGroupResult{ObservationError: ProviderUnavailable}, reject, false},
		{"conflict is not a failure", SecurityGroupWork{TargetType: "security_group"}, SecurityGroupResult{}, &ProviderError{Kind: ProviderConflict}, false},
		{"no error", SecurityGroupWork{TargetType: "security_group"}, SecurityGroupResult{}, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SecurityGroupOperationTerminal(tc.work, tc.result, tc.err); got != tc.want {
				t.Errorf("terminal = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestTerminalOutcomeRequiresACommittedWrite is review finding P1: a failed
// finish leaves the operation retrying in the database, so reporting
// operation.failed there contradicts the row; and a stored failure must carry
// the provider cause instead of a blank error.
func TestTerminalOutcomeRequiresACommittedWrite(t *testing.T) {
	cases := []struct {
		name       string
		progress   Progress
		finishErr  error
		wantEvent  string
		wantHasErr bool
	}{
		{
			name:       "stored failure keeps the provider cause",
			progress:   Progress{OperationState: OpFailed, OperationReason: ProviderRejected, ProviderError: &ProviderError{Kind: ProviderReject, Cause: errText("quota denied")}},
			wantEvent:  "operation.failed",
			wantHasErr: true,
		},
		{
			name:      "failed save is not a failed operation",
			progress:  Progress{OperationState: OpFailed, OperationReason: ProviderRejected, ProviderError: &ProviderError{Kind: ProviderReject}},
			finishErr: errText("SQLSTATE 40001"),
			wantEvent: "",
		},
		{
			name:      "stored success emits succeeded",
			progress:  Progress{OperationState: Succeeded},
			wantEvent: "operation.succeeded",
		},
		{
			name:      "retrying is not terminal",
			progress:  Progress{OperationState: Retrying, Backoff: true},
			wantEvent: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&buf, nil))
			terminalOutcome(context.Background(), logger, WorkerMain, "op-1", []any{"resource_type", "vpc", "resource_id", "vpc-1"}, tc.progress, tc.finishErr)
			out := strings.TrimSpace(buf.String())
			if tc.wantEvent == "" {
				if out != "" {
					t.Fatalf("expected no event, got %s", out)
				}
				return
			}
			var r map[string]any
			if err := json.Unmarshal([]byte(out), &r); err != nil {
				t.Fatalf("decode: %v (%s)", err, out)
			}
			if r["event"] != tc.wantEvent {
				t.Errorf("event = %v, want %v", r["event"], tc.wantEvent)
			}
			_, hasErr := r["error"]
			if hasErr != tc.wantHasErr {
				t.Errorf("error present = %v, want %v (%v)", hasErr, tc.wantHasErr, r)
			}
			if tc.wantHasErr && !strings.Contains(r["error"].(string), "quota denied") {
				t.Errorf("provider cause lost: %v", r["error"])
			}
		})
	}
}
