package server

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"
)

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

func TestServerRPCLevels(t *testing.T) {
	cases := []struct {
		code  codes.Code
		level slog.Level
	}{
		{codes.OK, slog.LevelInfo},
		{codes.InvalidArgument, slog.LevelWarn},
		{codes.PermissionDenied, slog.LevelWarn},
		{codes.NotFound, slog.LevelWarn},
		{codes.Unavailable, slog.LevelError},
		{codes.Internal, slog.LevelError},
		{codes.Canceled, slog.LevelError},
	}
	for _, c := range cases {
		if got := rpcCodeLevel(c.code, false); got != c.level {
			t.Errorf("rpcCodeLevel(%v) = %v, want %v", c.code, got, c.level)
		}
	}
	// Confirmed caller cancellation is informational.
	if got := rpcCodeLevel(codes.Canceled, true); got != slog.LevelInfo {
		t.Errorf("confirmed cancel should be INFO, got %v", got)
	}
}

func TestServerRPCHealthException(t *testing.T) {
	if got := serverRPCLevel("/grpc.health.v1.Health/Check", codes.OK); got != slog.LevelDebug {
		t.Errorf("health OK should be DEBUG, got %v", got)
	}
	if got := serverRPCLevel("/grpc.health.v1.Health/Check", codes.Unavailable); got != slog.LevelWarn {
		t.Errorf("health failure should be WARN, got %v", got)
	}
}

// noopDelegate is a stats.Handler that performs no span work.
type noopDelegate struct{}

func (noopDelegate) TagRPC(ctx context.Context, _ *stats.RPCTagInfo) context.Context { return ctx }
func (noopDelegate) HandleRPC(context.Context, stats.RPCStats)                       {}
func (noopDelegate) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context {
	return ctx
}
func (noopDelegate) HandleConn(context.Context, stats.ConnStats) {}

func TestServerCompletionRecordedOnce(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h := rpcServerStatsHandler{delegate: noopDelegate{}, logger: logger}
	ctx := h.TagRPC(context.Background(), &stats.RPCTagInfo{FullMethodName: "/vm.v1.VMService/Get"})
	h.HandleRPC(ctx, &stats.End{BeginTime: time.Now(), EndTime: time.Now().Add(5 * time.Millisecond)})

	recs := decodeRecords(t, &buf)
	if len(recs) != 1 {
		t.Fatalf("expected exactly one completion record, got %d: %v", len(recs), recs)
	}
	if recs[0]["event"] != "rpc.server.completed" || recs[0]["rpc.code"] != "OK" || recs[0]["result"] != "succeeded" {
		t.Errorf("completion record = %v", recs[0])
	}
	if recs[0]["rpc.method"] != "/vm.v1.VMService/Get" {
		t.Errorf("rpc.method = %v", recs[0]["rpc.method"])
	}
}

func TestServerCompletionFailureCarriesError(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	h := rpcServerStatsHandler{delegate: noopDelegate{}, logger: logger}
	ctx := h.TagRPC(context.Background(), &stats.RPCTagInfo{FullMethodName: "/vm.v1.VMService/Create"})
	h.HandleRPC(ctx, &stats.End{BeginTime: time.Now(), EndTime: time.Now(), Error: status.Error(codes.Unavailable, "connection refused")})

	recs := decodeRecords(t, &buf)
	last := recs[len(recs)-1]
	if last["event"] != "rpc.server.completed" || last["rpc.code"] != "Unavailable" || last["result"] != "failed" {
		t.Errorf("failure record = %v", last)
	}
	if last["error"] == nil || last["error_source"] != "grpc" {
		t.Errorf("failure record must retain the real error: %v", last)
	}
}

func TestAdminFilterSetsTraceAndRecords(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h := adminObservabilityFilter(logger)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Header().Get(adminHeaderTraceID) == "" {
		t.Fatal("admin response missing X-Trace-ID")
	}
	recs := decodeRecords(t, &buf)
	var started, done map[string]any
	for _, r := range recs {
		switch r["event"] {
		case "http.request.started":
			started = r
		case "http.request.completed":
			done = r
		}
	}
	if started == nil || done == nil {
		t.Fatalf("missing admin started/completed: %v", recs)
	}
	if done["level"] != "DEBUG" {
		t.Errorf("healthy admin completion should be DEBUG, got %v", done["level"])
	}
}

func TestAdminFilter404Warn(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h := adminObservabilityFilter(logger)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/nope", nil))
	recs := decodeRecords(t, &buf)
	done := recs[len(recs)-1]
	if done["level"] != "WARN" || done["result"] != "rejected" {
		t.Errorf("404 completion = %v", done)
	}
}
