package data_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
)

type failingPreflightVMNAD struct{ err error }

func (n failingPreflightVMNAD) ResolveVMNetwork(context.Context, biz.VMNetworkSelection, biz.VMNetworkExpectation) (biz.VMPrimaryPlan, error) {
	return biz.VMPrimaryPlan{}, n.err
}

func TestVMNetworkSelectionUsesManagementPlacementWithoutRefreshingProjection(t *testing.T) {
	f, _, _ := newAutoVMNetwork(t)
	policy := awaitAutoVMNetwork(t, f)
	f.r.WorkloadMode = "vm_managed"
	f.r.ConsumerType = biz.ConsumerVirtualMachine
	f.r.VMExpectation = &biz.VMNetworkExpectation{NADName: policy.NADName, NADUID: policy.NADUID, NADConfigDigest: policy.NADConfigDigest}
	ctx := context.Background()
	for _, table := range []string{"network_vpcs", "network_subnets"} {
		if _, err := f.owner.Exec(ctx, "UPDATE "+table+" SET observed_at=clock_timestamp()-interval '5 minutes' WHERE tenant_id=$1", f.r.TenantID); err != nil {
			t.Fatal(err)
		}
	}
	plan, err := f.p.ResolveVMNetworkSelection(ctx, f.r)
	if err != nil || plan.NADUID != policy.NADUID {
		t.Fatalf("old projection blocked current valid network: %+v %v", plan, err)
	}
	var stillStale bool
	if err := f.owner.QueryRow(ctx, "SELECT observed_at < clock_timestamp()-interval '60 seconds' FROM network_vpcs WHERE tenant_id=$1 AND vpc_id=$2", f.r.TenantID, f.r.VPCID).Scan(&stillStale); err != nil || !stillStale {
		t.Fatal("preflight refreshed durable observation", err)
	}
	// Prepare shares the current Res management selection.
	if _, err := f.a.Prepare(ctx, f.r); err != nil {
		t.Fatal("Prepare retained a hidden TTL gate", err)
	}
	f.api.Mu.Lock()
	for key, obj := range f.api.Objects {
		if strings.HasPrefix(key, "subnets/") {
			obj["metadata"].(map[string]any)["uid"] = "replacement-uid"
		}
	}
	f.api.Mu.Unlock()
	if _, err := f.p.ResolveVMNetworkSelection(ctx, f.r); err != nil {
		t.Fatal("selection audited the underlying Subnet instead of its Res record", err)
	}
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
		if err := f.p.SetVMNetworkResolver(failingPreflightVMNAD{failure}); err != nil {
			t.Fatal(err)
		}
		var output bytes.Buffer
		old := slog.Default()
		slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
		_, err := f.p.ResolveVMNetworkSelection(context.Background(), f.r)
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
