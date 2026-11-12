package biz

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/zhangzhe-ctrl/ani-resource-service/internal/telemetry"
)

// The operation payload is written back to the business column and re-decoded
// by the next claim; logging metadata and its decode error must stay out.
func TestOperationJSONExcludesTelemetry(t *testing.T) {
	op := Operation{
		ID: "op-1", TenantID: "t-1", Kind: "create_vpc", State: "queued",
		TelemetryContext:      telemetry.TelemetryContext{Traceparent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"},
		TelemetryContextError: errors.New("bad telemetry"),
	}
	raw, err := json.Marshal(op)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("traceparent")) || bytes.Contains(raw, []byte("TelemetryContext")) {
		t.Fatalf("business payload contains logging metadata: %s", raw)
	}
	var decoded Operation
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("business payload must decode after a telemetry decode error: %v\n%s", err, raw)
	}
	if decoded.ID != op.ID || decoded.Kind != op.Kind {
		t.Fatalf("business fields lost: %+v", decoded)
	}
	if decoded.TelemetryContextError != nil || decoded.TelemetryContext.Valid() {
		t.Fatalf("telemetry fields must decode as absent: %+v", decoded)
	}
}
