package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func decodeRecord(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	line := strings.TrimSpace(buf.String())
	if line == "" {
		t.Fatal("no log record written")
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(line), &out); err != nil {
		t.Fatalf("record is not JSON: %v (%s)", err, line)
	}
	return out
}

func newTestLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(newContractHandler(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
}

func TestContractHandlerAddsFrameworkEventAndDropsEmptyStrings(t *testing.T) {
	var buf bytes.Buffer
	logger := newTestLogger(&buf)
	logger.Info("framework message", "trace_id", "", "span_id", "", "component", "server")

	rec := decodeRecord(t, &buf)
	if rec["event"] != runtimeEvent {
		t.Fatalf("missing framework event: %v", rec["event"])
	}
	for _, key := range []string{"trace_id", "span_id"} {
		if _, ok := rec[key]; ok {
			t.Fatalf("empty %s must be dropped, got %v", key, rec[key])
		}
	}
	if rec["component"] != "server" {
		t.Fatalf("non-empty attribute lost: %v", rec["component"])
	}
}

func TestContractHandlerKeepsRegisteredEvent(t *testing.T) {
	var buf bytes.Buffer
	logger := newTestLogger(&buf)
	logger.Warn("worker attempt failed", "event", "worker.retry_scheduled", "error", "boom")

	rec := decodeRecord(t, &buf)
	if rec["event"] != "worker.retry_scheduled" {
		t.Fatalf("registered event overwritten: %v", rec["event"])
	}
}

func TestContractHandlerHonoursEventBoundWithWith(t *testing.T) {
	var buf bytes.Buffer
	logger := newTestLogger(&buf).With("event", "process.ready", "tenant_id", "")
	logger.Info("ready")

	rec := decodeRecord(t, &buf)
	if rec["event"] != "process.ready" {
		t.Fatalf("event bound through With was lost: %v", rec["event"])
	}
	if _, ok := rec["tenant_id"]; ok {
		t.Fatal("empty attribute bound through With must be dropped")
	}
}

func TestContractHandlerSynthesisesMessage(t *testing.T) {
	var buf bytes.Buffer
	logger := newTestLogger(&buf)
	logger.Info("", "component", "http", "operation", "/a.B/C", "code", "401")

	rec := decodeRecord(t, &buf)
	msg, _ := rec["msg"].(string)
	if msg != "http /a.B/C 401" {
		t.Fatalf("unexpected fallback message: %q", msg)
	}
}

func TestContractHandlerFiltersGroupAttributes(t *testing.T) {
	var buf bytes.Buffer
	logger := newTestLogger(&buf)
	logger.Info("grouped", slog.Group("target", slog.String("kind", "pod"), slog.String("uid", "")))

	rec := decodeRecord(t, &buf)
	group, ok := rec["target"].(map[string]any)
	if !ok {
		t.Fatalf("group missing: %v", rec["target"])
	}
	if group["kind"] != "pod" {
		t.Fatalf("group value lost: %v", group)
	}
	if _, ok := group["uid"]; ok {
		t.Fatal("empty group attribute must be dropped")
	}
}
