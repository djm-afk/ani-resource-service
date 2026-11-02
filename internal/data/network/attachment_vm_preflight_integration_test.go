package data_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
)

type failingPreflightVMNAD struct{ err error }

func (n failingPreflightVMNAD) ResolveVMNetwork(context.Context, biz.VMNetworkSelection, biz.VMNetworkExpectation) (biz.VMPrimaryPlan, error) {
	return biz.VMPrimaryPlan{}, n.err
}

func TestVMNetworkPreflightPGLogsFreshnessWithoutRefreshingIt(t *testing.T) {
	f := newAttachmentFixture(t)
	f.r.WorkloadMode = "vm_managed"
	f.r.VMExpectation = &biz.VMNetworkExpectation{NADName: "main", NADUID: "nad-uid", NADConfigDigest: strings.Repeat("a", 64)}
	resolver := &vmNADBoundary{}
	if err := f.p.SetVMNetworkResolver(resolver, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := f.owner.Exec(context.Background(), "UPDATE network_vpcs SET observed_at=clock_timestamp()-interval '61 seconds' WHERE tenant_id=$1 AND vpc_id=$2", f.r.TenantID, f.r.VPCID); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	if _, err := f.p.ResolveVMNetworkSelection(context.Background(), f.r, time.Minute); biz.ReasonOf(err) != biz.NetworkNotReady || resolver.calls != 0 {
		t.Fatalf("stale selection admitted or reached NAD resolver: %v calls=%d", err, resolver.calls)
	}
	var record map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &record); err != nil {
		t.Fatalf("missing freshness diagnostic: %v; log=%q", err, output.String())
	}
	if record["stage"] != "network_freshness" || record["reason"] != string(biz.NetworkNotReady) || record["tenant_id"] != f.r.TenantID || record["vpc_id"] != f.r.VPCID || record["subnet_id"] != f.r.SubnetID || record["vpc_state"] != "available" || record["subnet_state"] != "available" || record["freshness_limit_ms"] != float64(60000) {
		t.Fatalf("freshness failure cannot be located: %+v", record)
	}
	vpcAge, ok := record["vpc_observation_age_ms"].(float64)
	if !ok || vpcAge < 61000 {
		t.Fatalf("missing stale VPC age: %+v", record)
	}
	subnetAge, ok := record["subnet_observation_age_ms"].(float64)
	if !ok || subnetAge >= 60000 {
		t.Fatalf("fresh Subnet was not distinguished from stale VPC: %+v", record)
	}
	var stillStale bool
	if err := f.owner.QueryRow(context.Background(), "SELECT observed_at < clock_timestamp()-interval '60 seconds' FROM network_vpcs WHERE tenant_id=$1 AND vpc_id=$2", f.r.TenantID, f.r.VPCID).Scan(&stillStale); err != nil || !stillStale {
		t.Fatalf("read-only preflight changed observation: stale=%v error=%v", stillStale, err)
	}
	// Recover through the real worker and Kubernetes boundary; the admission
	// check itself must never refresh the stale persisted observation.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var lastErr error
	for ctx.Err() == nil {
		if _, err := f.resourceWorker.Step(ctx); err != nil {
			t.Fatal(err)
		}
		plan, err := f.p.ResolveVMNetworkSelection(ctx, f.r, time.Minute)
		lastErr = err
		if err == nil {
			if resolver.calls == 0 || plan.NADUID != f.r.VMExpectation.NADUID {
				t.Fatalf("recovered admission did not resolve its frozen NAD: %+v calls=%d", plan, resolver.calls)
			}
			return
		}
		if biz.ReasonOf(err) != biz.NetworkNotReady {
			t.Fatalf("observation recovery failed: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("real network observation did not restore admission: %v", lastErr)
}

func TestVMNetworkPreflightPGLogsOnlyAllowlistedReason(t *testing.T) {
	f := newAttachmentFixture(t)
	f.r.WorkloadMode = "vm_managed"
	f.r.VMExpectation = &biz.VMNetworkExpectation{NADName: "main", NADUID: "nad-uid", NADConfigDigest: strings.Repeat("a", 64)}
	const secret = "postgres://vm:secret-password@db/private?token=secret-token"
	for _, failure := range []error{
		errors.New(secret),
		&biz.Error{Reason: biz.AttachmentConflict, Message: secret, Cause: errors.New(secret)},
		&biz.Error{Reason: biz.Reason(secret), Message: secret},
	} {
		if err := f.p.SetVMNetworkResolver(failingPreflightVMNAD{failure}, time.Minute); err != nil {
			t.Fatal(err)
		}
		var output bytes.Buffer
		old := slog.Default()
		slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
		_, err := f.p.ResolveVMNetworkSelection(context.Background(), f.r, time.Minute)
		slog.SetDefault(old)
		if err != failure {
			t.Fatalf("preflight changed resolver error: %v", err)
		}
		if strings.Contains(output.String(), "secret-password") || strings.Contains(output.String(), "secret-token") {
			t.Fatal("preflight logged provider credentials or arbitrary reason")
		}
		var record map[string]any
		if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &record); err != nil || record["stage"] != "network_identity" {
			t.Fatalf("missing network identity diagnostic: %v; log=%q", err, output.String())
		}
		expected := biz.DependencyUnavailable
		if biz.ReasonOf(failure) == biz.AttachmentConflict {
			expected = biz.AttachmentConflict
		}
		if record["reason"] != string(expected) {
			t.Fatalf("lost safe network reason: %+v", record)
		}
	}
}
