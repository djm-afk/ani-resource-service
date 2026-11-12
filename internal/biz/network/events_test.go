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
