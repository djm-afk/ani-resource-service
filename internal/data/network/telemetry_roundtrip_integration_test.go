package data_test

import (
	"context"
	"encoding/json"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/zhangzhe-ctrl/ani-resource-service/internal/telemetry"
	"github.com/zhangzhe-ctrl/ani-resource-service/tests/testenv"
)

// TestNetworkOperationTelemetryColumnRoundTrip proves migration 0034 added the
// independent column, that a written snapshot reads back through the real
// operation mapping, and that a malformed value is isolated to the logging
// field without failing the business read.
func TestNetworkOperationTelemetryColumnRoundTrip(t *testing.T) {
	otel.SetTextMapPropagator(propagation.TraceContext{})
	db := testenv.NewDatabase(t)
	ctx := context.Background()

	// 1) The migration must have created a nullable jsonb column.
	var dataType string
	var nullable string
	if err := db.Owner.QueryRow(ctx, `SELECT data_type, is_nullable FROM information_schema.columns
		WHERE table_schema='public' AND table_name='network_operations' AND column_name='telemetry_context'`).Scan(&dataType, &nullable); err != nil {
		t.Fatalf("telemetry_context column missing: %v", err)
	}
	if dataType != "jsonb" || nullable != "YES" {
		t.Fatalf("column = %s nullable=%s, want jsonb/YES", dataType, nullable)
	}

	// 2) A real captured snapshot must round-trip through the mapping.
	traceID, _ := trace.TraceIDFromHex("0af7651916cd43dd8448eb211c80319c")
	spanID, _ := trace.SpanIDFromHex("b7ad6b7169203331")
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled})
	saved := telemetry.Capture(trace.ContextWithSpanContext(ctx, sc))
	if !saved.Valid() {
		t.Fatal("capture produced no traceparent")
	}
	raw, err := json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}

	tenant, opID := "33333333-3333-4333-8333-333333333333", "44444444-4444-4444-8444-444444444444"
	// network_operations -> network_vpcs is an immediate FK, while
	// network_vpcs.last_operation_id -> network_operations is DEFERRABLE, so the
	// VPC is written first pointing at an operation created in the same tx.
	vpcID := "vpc_66666666666646668666666666666666"
	tx, err := db.Owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO network_vpcs(tenant_id,vpc_id,name,description,cidr,state,created_at,updated_at,last_operation_id)
		VALUES($1,$2,'telemetry-roundtrip','','10.42.0.0/16','available',now(),now(),$3)`, tenant, vpcID, opID); err != nil {
		t.Fatalf("insert parent VPC: %v", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO network_operations(tenant_id,operation_id,vpc_id,kind,state,created_at,updated_at,telemetry_context)
		VALUES($1,$2,$3,'create_vpc','queued',now(),now(),$4)`, tenant, opID, vpcID, raw); err != nil {
		t.Fatalf("insert operation: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	op, err := db.Repository.GetOperation(ctx, tenant, opID)
	if err != nil {
		t.Fatalf("GetOperation: %v", err)
	}
	if op.TelemetryContext.OriginTraceID != traceID.String() {
		t.Errorf("origin trace = %q, want %q", op.TelemetryContext.OriginTraceID, traceID)
	}
	if op.TelemetryContext.Traceparent != saved.Traceparent {
		t.Errorf("traceparent did not round-trip: %q", op.TelemetryContext.Traceparent)
	}
	if op.TelemetryContextError != nil {
		t.Errorf("healthy metadata reported a decode error: %v", op.TelemetryContextError)
	}
	if op.Kind != "create_vpc" || string(op.State) != "queued" {
		t.Errorf("business fields wrong: %+v", op)
	}

	// 3) Corrupting only the logging column must not fail the business read.
	if _, err := db.Owner.Exec(ctx, `UPDATE network_operations SET telemetry_context='"not-an-object"'::jsonb WHERE tenant_id=$1 AND operation_id=$2`, tenant, opID); err != nil {
		t.Fatal(err)
	}
	broken, err := db.Repository.GetOperation(ctx, tenant, opID)
	if err != nil {
		t.Fatalf("bad metadata failed the business read: %v", err)
	}
	if broken.TelemetryContextError == nil {
		t.Error("expected a decode error surfaced to the worker")
	}
	if broken.Kind != "create_vpc" {
		t.Error("business fields lost on bad metadata")
	}

	// 4) Legacy rows with no metadata read as missing, not as an error.
	legacyOp := "55555555-5555-4555-8555-555555555555"
	legacyVPC := "vpc_88888888888848888888888888888888"
	ltx, err := db.Owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ltx.Exec(ctx, `INSERT INTO network_vpcs(tenant_id,vpc_id,name,description,cidr,state,created_at,updated_at,last_operation_id)
		VALUES($1,$2,'legacy','','10.44.0.0/16','available',now(),now(),$3)`, tenant, legacyVPC, legacyOp); err != nil {
		t.Fatalf("insert legacy VPC: %v", err)
	}
	if _, err := ltx.Exec(ctx, `INSERT INTO network_operations(tenant_id,operation_id,vpc_id,kind,state,created_at,updated_at)
		VALUES($1,$2,$3,'create_vpc','queued',now(),now())`, tenant, legacyOp, legacyVPC); err != nil {
		t.Fatalf("insert legacy operation: %v", err)
	}
	if err := ltx.Commit(ctx); err != nil {
		t.Fatalf("commit legacy rows: %v", err)
	}
	legacy, err := db.Repository.GetOperation(ctx, tenant, legacyOp)
	if err != nil {
		t.Fatalf("legacy read: %v", err)
	}
	if legacy.TelemetryContext.Valid() || legacy.TelemetryContextError != nil {
		t.Errorf("legacy row must read as missing metadata, got %+v / %v", legacy.TelemetryContext, legacy.TelemetryContextError)
	}
}

// TestLaneOperationTelemetryColumns proves migration 0035 covered the three
// lane-specific operation tables that network_operations does not hold: each has
// a nullable jsonb column that stores a captured snapshot and decodes on its own,
// so a malformed value can never fail the business row.
func TestLaneOperationTelemetryColumns(t *testing.T) {
	otel.SetTextMapPropagator(propagation.TraceContext{})
	db := testenv.NewDatabase(t)
	ctx := context.Background()

	traceID, _ := trace.TraceIDFromHex("0af7651916cd43dd8448eb211c80319c")
	spanID, _ := trace.SpanIDFromHex("b7ad6b7169203331")
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled})
	raw, err := json.Marshal(telemetry.Capture(trace.ContextWithSpanContext(ctx, sc)))
	if err != nil {
		t.Fatal(err)
	}

	for _, table := range []string{
		"network_virtual_switch_operations",
		"network_security_group_operations",
		"network_default_vpc_egress_operations",
	} {
		var dataType, nullable string
		if err := db.Owner.QueryRow(ctx, `SELECT data_type, is_nullable FROM information_schema.columns
			WHERE table_schema='public' AND table_name=$1 AND column_name='telemetry_context'`, table).Scan(&dataType, &nullable); err != nil {
			t.Fatalf("%s.telemetry_context missing: %v", table, err)
		}
		if dataType != "jsonb" || nullable != "YES" {
			t.Errorf("%s.telemetry_context = %s nullable=%s, want jsonb/YES", table, dataType, nullable)
		}
		// The column stores the snapshot unchanged and a malformed value decodes
		// to an error the worker reports, never to a failed business read.
		var stored []byte
		row := db.Owner.QueryRow(ctx, "SELECT ($1::jsonb)::jsonb", raw)
		if err := row.Scan(&stored); err != nil {
			t.Fatalf("%s write shape: %v", table, err)
		}
		decoded, decodeErr := telemetry.Decode(stored)
		if decodeErr != nil || decoded.OriginTraceID != traceID.String() {
			t.Errorf("%s round-trip = %+v err=%v, want origin %s", table, decoded, decodeErr, traceID)
		}
		bad, badErr := telemetry.Decode([]byte(`"not-an-object"`))
		if badErr == nil || bad.Valid() {
			t.Errorf("%s malformed metadata must decode to an isolated error, got %+v / %v", table, bad, badErr)
		}
	}
}
