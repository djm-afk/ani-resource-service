package data_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network"
	"k8s.io/client-go/tools/clientcmd"
)

func TestLBBackendObservationUsesCurrentDatabaseTime(t *testing.T) {
	f := newLBAdmissionFixture(t)
	created, err := f.lbs.Create(f.f.ctx, f.request)
	if err != nil {
		t.Fatal(err)
	}
	// Select the accepted LB deterministically in this isolated PG fixture.
	if _, err = f.f.owner.Exec(f.f.ctx, `UPDATE network_reconciliations SET next_run_at=clock_timestamp()-interval '1 minute' WHERE tenant_id=$1 AND lb_id=$2`, f.f.tenant, created.LoadBalancer.ID); err != nil {
		t.Fatal(err)
	}
	var work biz.Work
	for attempt := 0; attempt < 12; attempt++ {
		var found bool
		work, found, err = f.f.p.Claim(f.f.ctx, uuid.NewString(), time.Minute)
		if err != nil || !found {
			t.Fatalf("claim: found=%v err=%v", found, err)
		}
		if work.Resource.ID == created.LoadBalancer.ID {
			break
		}
	}
	if work.Resource.ID != created.LoadBalancer.ID {
		t.Fatalf("accepted LB not claimed: %s", work.Resource.ID)
	}
	config, err := clientcmd.BuildConfigFromFlags("", f.kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	// Freshness and identity are under test, not client-side rate limiting.
	config.QPS, config.Burst = 1000, 1000
	provider, err := data.NewKCProvider(f.f.p, config)
	if err != nil {
		t.Fatal(err)
	}
	attachment := created.LoadBalancer.Backends[0].AttachmentID
	for _, tc := range []struct {
		name, offset string
		reason       string
		eligible     bool
	}{
		{"renewed_after_claim", "0 seconds", "", true},
		{"future_timestamp", "10 minutes", "", false},
		{"expired_timestamp", "-10 minutes", "", false},
		{"read_unavailable_with_current_native_facts", "0 seconds", string(biz.ProviderUnavailable), true},
		{"fresh_again", "0 seconds", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var observed time.Time
			// Model the concurrent Attachment worker completing a fresh read
			// after LB work was claimed; only this controlled DB is adjusted.
			if err := f.f.owner.QueryRow(f.f.ctx, `UPDATE network_attachments SET observed_at=clock_timestamp()+$3::interval,reason=$4 WHERE tenant_id=$1 AND attachment_id=$2 RETURNING observed_at`, f.f.tenant, attachment, tc.offset, tc.reason).Scan(&observed); err != nil {
				t.Fatal(err)
			}
			if tc.eligible && !observed.After(work.Now) {
				t.Fatal("fixture did not renew after claim", observed, work.Now)
			}
			result, err := provider.ObserveLoadBalancer(f.f.ctx, work)
			if !tc.eligible {
				var unavailable *biz.ProviderError
				if !errors.As(err, &unavailable) || unavailable.Kind != biz.ProviderTemporary || len(result.Members) != 0 {
					t.Fatalf("unconfirmed timestamp fabricated an identity mismatch: %+v %v", result.Members, err)
				}
				return
			}
			if err != nil || len(result.Members) != 1 || result.Members[0].Eligible != tc.eligible {
				t.Fatalf("freshness result=%+v err=%v; observed=%v claim=%v", result.Members, err, observed, work.Now)
			}
		})
	}
	// A real replacement wins over projection uncertainty and is still fenced.
	if _, err = f.f.owner.Exec(f.f.ctx, `UPDATE network_attachments SET observed_at=clock_timestamp()-interval '10 minutes' WHERE tenant_id=$1 AND attachment_id=$2`, f.f.tenant, attachment); err != nil {
		t.Fatal(err)
	}
	ip := f.api.Object("vnicips", "tenant-"+f.f.tenant, "lb-backend-ip")
	ip["metadata"].(map[string]any)["uid"] = uuid.NewString()
	f.api.Change("vnicips", ip, false)
	result, err := provider.ObserveLoadBalancer(f.f.ctx, work)
	if err != nil || len(result.Members) != 1 || result.Members[0].Eligible || result.Members[0].Reason != biz.BackendIdentityMismatch {
		t.Fatalf("replacement accepted: %+v %v", result.Members, err)
	}
}
