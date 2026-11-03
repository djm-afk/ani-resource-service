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

type attachmentAuditTransport struct{ next http.RoundTripper }

func (t attachmentAuditTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	copy := r.Clone(r.Context())
	copy.Header = r.Header.Clone()
	copy.Header.Set("X-Attachment-Audit-Test", "true")
	return t.next.RoundTrip(copy)
}

func TestAttachmentOrdinaryProjectionWaitsAndCleanupSurvivesRelists(t *testing.T) {
	// End one real HTTP Watch while each of the first two complete audits is
	// in flight. The third audit is stable, inside the original caller budget.
	type watchSession struct {
		cancel context.CancelFunc
		ctx    context.Context
	}
	var observation atomic.Pointer[data.KCObservation]
	var armed atomic.Bool
	var keepInvalidating atomic.Bool
	var collections atomic.Int32
	var boundaries atomic.Int32
	var watchMu sync.Mutex
	active := map[*watchSession]bool{}
	f := newLBAdmissionFixture(t, func(w http.ResponseWriter, r *http.Request, api *controlled.Server) bool {
		if r.Header.Get("X-Attachment-Audit-Test") != "true" {
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
		// Wait for the observed continuity boundary, without depending on an
		// arbitrary delay or a stale bootstrap Watch handle.
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
	config, err := clientcmd.BuildConfigFromFlags("", f.kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	config.QPS, config.Burst = 1000, 1000
	config.WrapTransport = func(rt http.RoundTripper) http.RoundTripper { return attachmentAuditTransport{rt} }
	provider, err := data.NewKCProvider(f.f.p, config)
	if err != nil {
		t.Fatal(err)
	}
	observer := startSlowAudit(t, provider)
	observation.Store(observer)
	var attachmentID string
	var relations []byte
	if err = f.f.owner.QueryRow(f.f.ctx, `SELECT attachment_id,provider_relations FROM network_attachments WHERE tenant_id=$1 AND instance_id='lb-backend'`, f.f.tenant).Scan(&attachmentID, &relations); err != nil {
		t.Fatal(err)
	}
	a, err := biz.NewAttachments(f.f.p, time.Minute).Get(f.f.ctx, f.f.tenant, attachmentID)
	if err != nil {
		t.Fatal(err)
	}
	work := biz.AttachmentWork{Attachment: a, Plan: *a.Plan, Now: time.Now(), Relations: relations, RequireFreshRelations: true}
	first, err := provider.ObserveAttachment(f.f.ctx, work)
	if err != nil || !first.Exists || first.PodUID != a.PodUID {
		t.Fatalf("initial Attachment facts: %+v %v", first, err)
	}
	work.RequireFreshRelations = false
	// Parent-only durable hints must not starve an unchanged attachment view.
	work.Requirement.RequestedGeneration = first.Proof.CoveredGeneration + 1
	unchanged, unchangedErr := provider.ObserveAttachment(f.f.ctx, work)
	if unchangedErr != nil || !unchanged.Exists || unchanged.Proof.CollectedAt != first.Proof.CollectedAt || unchanged.Proof.CoveredGeneration != work.Requirement.RequestedGeneration {
		t.Fatalf("unrelated hint starved or renewed unchanged facts: %+v %v", unchanged, unchangedErr)
	}
	work.Requirement.RequestedGeneration = 0
	before := observer.Snapshot()["notifications_total"]
	pod := f.api.Object("pods", a.Namespace, a.PodName)
	annotations := pod["metadata"].(map[string]any)["annotations"].(map[string]any)
	annotations["audit-test"] = "changed"
	changedAt := time.Now()
	f.api.Change("pods", pod, false)
	awaitNET05A(t, 3*time.Second, func() bool { return observer.Snapshot()["notifications_total"] > before })
	armed.Store(true)
	ctx, cancel := context.WithTimeout(f.f.ctx, 12*time.Second)
	defer cancel()
	// Stable projection does not repeat a cluster-wide collection. Cleanup
	// independently requests a post-closure collection and must survive relists.
	ordinary, ordinaryErr := provider.ObserveAttachment(ctx, work)
	if ordinaryErr == nil || !ordinary.Proof.CollectedAt.IsZero() || collections.Load() != 0 {
		t.Fatalf("ordinary projection started a full collection or returned stale proof: %+v %v", ordinary, ordinaryErr)
	}
	work.RequireFreshRelations = true
	next, err := provider.ObserveAttachment(ctx, work)
	if err != nil || !next.Exists || next.PodUID != a.PodUID || next.Proof.CollectedAt.Before(changedAt) || boundaries.Load() != 2 {
		t.Fatalf("stable audit within caller budget was not consumed after two Watch boundaries: collections=%d boundaries=%d observation=%+v err=%v", collections.Load(), boundaries.Load(), next, err)
	}
	armed.Store(false)
	// Repeated invalidations must not extend a caller's deadline or produce a
	// usable observation after the budget ends. Other audit waiters keep their
	// existing independent collection lifecycle.
	before = observer.Snapshot()["notifications_total"]
	pod = f.api.Object("pods", a.Namespace, a.PodName)
	pod["metadata"].(map[string]any)["annotations"].(map[string]any)["audit-test"] = "deadline"
	f.api.Change("pods", pod, false)
	awaitNET05A(t, 3*time.Second, func() bool { return observer.Snapshot()["notifications_total"] > before })
	keepInvalidating.Store(true)
	armed.Store(true)
	limited, stop := context.WithTimeout(f.f.ctx, 200*time.Millisecond)
	started := time.Now()
	blocked, err := provider.ObserveAttachment(limited, work)
	stop()
	armed.Store(false)
	keepInvalidating.Store(false)
	if !errors.Is(err, context.DeadlineExceeded) || !blocked.Proof.CollectedAt.IsZero() || time.Since(started) > time.Second {
		t.Fatalf("audit retries exceeded caller budget or returned usable facts: %+v %v", blocked, err)
	}
	before = observer.Snapshot()["notifications_total"]
	ip := f.api.Object("vnicips", a.Namespace, "lb-backend-ip")
	ip["metadata"].(map[string]any)["uid"] = uuid.NewString()
	f.api.Change("vnicips", ip, false)
	awaitNET05A(t, 3*time.Second, func() bool { return observer.Snapshot()["notifications_total"] > before })
	_, err = provider.ObserveAttachment(ctx, work)
	var failure *biz.ProviderError
	if !errors.As(err, &failure) || failure.Kind != biz.ProviderConflict {
		t.Fatalf("fresh collection accepted replacement VNicIP UID: %v", err)
	}
}
