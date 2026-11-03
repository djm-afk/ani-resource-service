package data_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network"
	controlled "github.com/zhangzhe-ctrl/ani-resource-service/tests/net05a/provider"
	"k8s.io/client-go/tools/clientcmd"
)

type egressAuditTransport struct{ next http.RoundTripper }

func (t egressAuditTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	copy := r.Clone(r.Context())
	copy.Header = r.Header.Clone()
	copy.Header.Set("X-Egress-Audit-Test", "true")
	return t.next.RoundTrip(copy)
}

func TestEgressOrdinaryProjectionWaitsAndCriticalReadSurvivesRelists(t *testing.T) {
	// Two real HTTP Watch expirations invalidate two complete collections. A
	// subsequent stable collection must still be usable within the same call.
	type watchSession struct {
		cancel context.CancelFunc
		ctx    context.Context
	}
	var observation atomic.Pointer[data.KCObservation]
	var armed, keepInvalidating atomic.Bool
	var collections, boundaries atomic.Int32
	var watchMu sync.Mutex
	active := map[*watchSession]bool{}
	f, api, _, kube := newBaseKCFixture(t, func(w http.ResponseWriter, r *http.Request, api *controlled.Server) bool {
		if r.Header.Get("X-Egress-Audit-Test") != "true" {
			return false
		}
		if r.URL.Query().Get("watch") == "true" && strings.HasSuffix(r.URL.Path, "/pods") {
			ctx, cancel := context.WithCancel(r.Context())
			session := &watchSession{cancel: cancel, ctx: ctx}
			watchMu.Lock()
			active[session] = true
			watchMu.Unlock()
			defer func() { cancel(); watchMu.Lock(); delete(active, session); watchMu.Unlock() }()
			api.ServeHTTP(w, r.WithContext(ctx))
			return true
		}
		if !armed.Load() || r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/eips") || r.URL.Query().Get("watch") == "true" || r.URL.Query().Get("resourceVersion") != "" || (collections.Add(1) > 2 && !keepInvalidating.Load()) {
			return false
		}
		response := httptest.NewRecorder()
		api.ServeHTTP(response, r)
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		var sessions []*watchSession
		for len(sessions) == 0 {
			watchMu.Lock()
			for session := range active {
				if session.ctx.Err() == nil {
					sessions = append(sessions, session)
				}
			}
			watchMu.Unlock()
			if len(sessions) > 0 {
				break
			}
			select {
			case <-ticker.C:
			case <-r.Context().Done():
				return true
			}
		}
		boundary := observation.Load().Snapshot()["source_boundaries_total"]
		api.Disconnect(410)
		for observation.Load().Snapshot()["source_boundaries_total"] <= boundary {
			select {
			case <-ticker.C:
			case <-r.Context().Done():
				return true
			}
		}
		boundaries.Add(1)
		for key, values := range response.Header() {
			w.Header()[key] = values
		}
		w.WriteHeader(response.Code)
		_, _ = w.Write(response.Body.Bytes())
		return true
	})
	vpc := baseState(t, f, createBaseVPC(t, f, "retry-audit").ID, biz.Available)
	eipID, snatID := baseIDs(t, f, vpc.ID)
	target := biz.ProviderTarget{TenantID: f.tenant, ResourceID: snatID, Kind: "snat", VPCID: vpc.ID, Egress: &biz.EgressWorkSpec{EIPID: eipID, DesiredEnabled: true}, Direct: true}
	if err := f.owner.QueryRow(f.ctx, `SELECT binding_id,provider_uid FROM network_provider_bindings WHERE tenant_id=$1 AND snat_id=$2`, f.tenant, snatID).Scan(&target.BindingID, &target.KnownIdentity); err != nil {
		t.Fatal(err)
	}
	config, err := clientcmd.BuildConfigFromFlags("", kube)
	if err != nil {
		t.Fatal(err)
	}
	config.QPS, config.Burst = 1000, 1000
	config.WrapTransport = func(rt http.RoundTripper) http.RoundTripper { return egressAuditTransport{rt} }
	provider, err := data.NewKCProvider(f.p, config)
	if err != nil {
		t.Fatal(err)
	}
	observer := startSlowAudit(t, provider)
	observation.Store(observer)
	first, err := provider.Observe(f.ctx, target)
	if err != nil || !first.Ready || first.Egress == nil || first.Egress.AppliedEnabled == nil || !*first.Egress.AppliedEnabled {
		t.Fatalf("initial SNAT facts: %+v %v", first, err)
	}
	target.Direct = false
	ns, name := "tenant-"+f.tenant, strings.Replace(snatID, "_", "-", 1)
	change := func(value string) time.Time {
		before := observer.Snapshot()["notifications_total"]
		o := api.Object("snats", ns, name)
		o["metadata"].(map[string]any)["annotations"] = map[string]any{"egress-audit-test": value}
		at := time.Now()
		api.Change("snats", o, false)
		awaitNET05A(t, 3*time.Second, func() bool { return observer.Snapshot()["notifications_total"] > before })
		return at
	}
	changedAt := change("renewed")
	armed.Store(true)
	ctx, cancel := context.WithTimeout(f.ctx, 12*time.Second)
	defer cancel()
	next, err := provider.Observe(ctx, target)
	if err == nil || !next.Proof.CollectedAt.IsZero() || collections.Load() != 0 {
		t.Fatalf("ordinary observation started a full collection or used invalid proof: %+v %v", next, err)
	}
	target.Direct = true
	next, err = provider.Observe(ctx, target)
	if err != nil || !next.Ready || next.Egress == nil || next.Egress.AppliedEnabled == nil || !*next.Egress.AppliedEnabled || next.Proof.CollectedAt.Before(changedAt) || boundaries.Load() != 2 {
		t.Fatalf("stable audit within caller budget was not consumed: collections=%d boundaries=%d observation=%+v err=%v", collections.Load(), boundaries.Load(), next, err)
	}
	armed.Store(false)
	change("deadline")
	keepInvalidating.Store(true)
	armed.Store(true)
	limited, stop := context.WithTimeout(f.ctx, 200*time.Millisecond)
	started := time.Now()
	blocked, err := provider.Observe(limited, target)
	stop()
	armed.Store(false)
	keepInvalidating.Store(false)
	if !errors.Is(err, context.DeadlineExceeded) || !blocked.Proof.CollectedAt.IsZero() || time.Since(started) > time.Second {
		t.Fatalf("retries exceeded caller budget or returned usable proof: %+v %v", blocked, err)
	}
	before := observer.Snapshot()["notifications_total"]
	o := api.Object("snats", ns, name)
	o["metadata"].(map[string]any)["uid"] = uuid.NewString()
	api.Change("snats", o, false)
	awaitNET05A(t, 3*time.Second, func() bool { return observer.Snapshot()["notifications_total"] > before })
	_, err = provider.Observe(ctx, target)
	var failure *biz.ProviderError
	if !errors.As(err, &failure) || failure.Kind != biz.ProviderConflict {
		t.Fatalf("refreshed audit accepted replacement SNAT UID: %v", err)
	}
}
