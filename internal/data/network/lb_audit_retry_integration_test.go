package data_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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

// Exercise a real worker and PG persistence while two HTTP Watch expirations
// invalidate an audit. Provider conditions and identities stay unchanged.
type lbAuditRetryTransport struct{ next http.RoundTripper }

func (t lbAuditRetryTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	copy := r.Clone(r.Context())
	copy.Header = r.Header.Clone()
	copy.Header.Set("X-LB-Audit-Retry-Test", "boundary-regression")
	return t.next.RoundTrip(copy)
}

type lbAuditRetryProvider struct {
	*data.KCProvider
	observation                 biz.LoadBalancerObservation
	err                         error
	started, finished, deadline time.Time
}

// Claim's production alternation can select platform work even when the target
// LB is oldest in the tenant lane. This fixture isolates that lane selection
// from the observation experiment. Every lease still comes from real PG; other
// fixture-only leases remain unprocessed until the test database is removed.
// Scheduler fairness, capacity, and recovery of those skipped jobs are not tested.
type lbAuditRetryRepository struct {
	*data.Postgres
	target string
}

func (r *lbAuditRetryRepository) Claim(ctx context.Context, owner string, lease time.Duration) (biz.Work, bool, error) {
	for attempt := 0; attempt < 32; attempt++ {
		work, found, err := r.Postgres.Claim(ctx, owner, lease)
		if err != nil || !found || work.Resource.ID == r.target {
			return work, found, err
		}
	}
	return biz.Work{}, false, fmt.Errorf("LB target %s not selected within 32 real Claims", r.target)
}

func (p *lbAuditRetryProvider) ObserveLoadBalancer(ctx context.Context, w biz.Work) (biz.LoadBalancerObservation, error) {
	p.started = time.Now()
	p.deadline, _ = ctx.Deadline()
	p.observation, p.err = p.KCProvider.ObserveLoadBalancer(ctx, w)
	p.finished = time.Now()
	return p.observation, p.err
}

func lbAuditRetryObjects(api *controlled.Server) string {
	api.Backend.Mu.Lock()
	defer api.Backend.Mu.Unlock()
	body, _ := json.Marshal(api.Backend.Objects)
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func TestLBOrdinaryObservationWaitsForSharedAuditAndCriticalRelistCompletes(t *testing.T) {
	// The retry policy is exposure-independent. Keep the combined public/private
	// entry here; all three exposures retain actual-adapter lifecycle coverage.
	for _, exposure := range []string{"public_private"} {
		t.Run(exposure, func(t *testing.T) {
			type watchSession struct {
				ctx    context.Context
				cancel context.CancelFunc
			}
			var observer atomic.Pointer[data.KCObservation]
			var armed, captured, repeat, deny atomic.Bool
			var denied atomic.Int32
			var fallbackCollections atomic.Int32
			var boundaries atomic.Int32
			var mu sync.Mutex
			active := map[*watchSession]bool{}
			controller := &lbControllerFixture{}
			f := newLBAdmissionFixture(t, func(w http.ResponseWriter, r *http.Request, api *controlled.Server) bool {
				if strings.HasSuffix(r.URL.Path, "/subjectaccessreviews") && r.Method == http.MethodPost {
					var request map[string]any
					if json.NewDecoder(r.Body).Decode(&request) != nil {
						http.Error(w, "invalid review", http.StatusBadRequest)
						return true
					}
					request["status"] = map[string]any{"allowed": true}
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(request)
					return true
				}
				if r.Header.Get("X-LB-Audit-Retry-Test") != "boundary-regression" {
					return controller.http(w, r, api)
				}
				if r.URL.Query().Get("watch") == "true" && strings.HasSuffix(r.URL.Path, "/pods") {
					ctx, cancel := context.WithCancel(r.Context())
					session := &watchSession{ctx, cancel}
					mu.Lock()
					active[session] = true
					mu.Unlock()
					defer func() { cancel(); mu.Lock(); delete(active, session); mu.Unlock() }()
					api.ServeHTTP(w, r.WithContext(ctx))
					return true
				}
				if deny.Load() && r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/eips") && r.URL.Query().Get("watch") != "true" && r.URL.Query().Get("resourceVersion") == "" {
					denied.Add(1)
					http.Error(w, "forbidden", http.StatusForbidden)
					return true
				}
				if !armed.Load() || r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/eips") || r.URL.Query().Get("watch") == "true" || r.URL.Query().Get("resourceVersion") != "" || (!repeat.Load() && !captured.CompareAndSwap(false, true)) {
					return controller.http(w, r, api)
				}
				if repeat.Load() {
					fallbackCollections.Add(1)
				}
				response := httptest.NewRecorder()
				api.ServeHTTP(response, r)
				ticker := time.NewTicker(time.Millisecond)
				defer ticker.Stop()
				// Two actual expirations/relist boundaries while this LIST remains in flight.
				// Waiting on observed boundaries avoids a guessed sleep duration.
				for i := 0; i < 2; i++ {
					var session *watchSession
					for session == nil {
						mu.Lock()
						for candidate := range active {
							if candidate.ctx.Err() == nil {
								session = candidate
								break
							}
						}
						mu.Unlock()
						if session != nil {
							break
						}
						select {
						case <-ticker.C:
						case <-r.Context().Done():
							return true
						}
					}
					before := observer.Load().Snapshot()["source_boundaries_total"]
					api.Disconnect(410)
					for observer.Load().Snapshot()["source_boundaries_total"] <= before {
						select {
						case <-ticker.C:
						case <-r.Context().Done():
							return true
						}
					}
					boundaries.Add(1)
				}
				for key, values := range response.Header() {
					w.Header()[key] = values
				}
				w.WriteHeader(response.Code)
				_, _ = w.Write(response.Body.Bytes())
				return true
			})
			request := f.request
			request.Exposure = exposure
			if exposure == "public_private" {
				request.PublicEIPID = f.f.eip(t, "retry-entry").ID
			}
			created, err := f.lbs.Create(f.f.ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			initial := lbState(t, f, created.LoadBalancer.ID, biz.Available)
			if initial.ConfigurationState != "configured" {
				t.Fatal("lifecycle precondition", initial)
			}

			expected := seedLBInstallation(f.api)
			config, err := clientcmd.BuildConfigFromFlags("", f.kubeconfig)
			if err != nil {
				t.Fatal(err)
			}
			// Audit invalidation and caller deadlines are under test, not client
			// rate limiting against this in-memory HTTP fixture.
			config.QPS, config.Burst = 1000, 1000
			config.WrapTransport = func(rt http.RoundTripper) http.RoundTripper { return lbAuditRetryTransport{rt} }
			provider, err := data.NewKCProvider(f.f.p, config)
			if err != nil {
				t.Fatal(err)
			}
			if err = provider.ConfigureLoadBalancer(expected); err != nil {
				t.Fatal(err)
			}
			options := data.DefaultObservationOptions()
			options.AuditInterval, options.AuditJitter, options.AuditTimeout = 15*time.Second, time.Second, 15*time.Second
			obs, err := provider.EnableObservation(options)
			if err != nil {
				t.Fatal(err)
			}
			observer.Store(obs)
			ctx, cancel := context.WithCancel(f.f.ctx)
			done := make(chan error, 1)
			go func() { done <- obs.Start(ctx) }()
			t.Cleanup(func() {
				cancel()
				if err := <-done; err != nil {
					t.Error(err)
				}
			})
			// Startup Watch boundaries can invalidate the immediate first audit.
			// Allow that bounded collection, the configured next interval/jitter,
			// and one further collection. This initializes the fixture only; it
			// does not extend either the measured request or the fault window.
			initializationBudget := 2*options.AuditTimeout + options.AuditInterval + options.AuditJitter + options.FlushInterval
			initializationCtx, stopInitialization := context.WithTimeout(f.f.ctx, initializationBudget)
			defer stopInitialization()
			initializationTicker := time.NewTicker(25 * time.Millisecond)
			defer initializationTicker.Stop()
			var sourcesReadyAt time.Time
			for {
				var ready bool
				if f.f.owner.QueryRow(initializationCtx, `SELECT ready FROM network_lb_capabilities WHERE cluster_id='test-cluster' AND fingerprint=$1`, expected.Fingerprint).Scan(&ready) == nil && ready && obs.Snapshot()["source_synced"] == 1 {
					if sourcesReadyAt.IsZero() {
						sourcesReadyAt = time.Now()
					}
					var observedAt time.Time
					if f.f.owner.QueryRow(initializationCtx, `SELECT observed_at FROM network_lb_capabilities WHERE cluster_id='test-cluster'`).Scan(&observedAt) == nil && observedAt.After(sourcesReadyAt) {
						break
					}
				}
				select {
				case <-initializationTicker.C:
				case <-initializationCtx.Done():
					var capability string
					diagnosticCtx, stopDiagnostic := context.WithTimeout(f.f.ctx, time.Second)
					capabilityErr := f.f.owner.QueryRow(diagnosticCtx, `SELECT to_jsonb(c)::text FROM network_lb_capabilities c WHERE cluster_id='test-cluster'`).Scan(&capability)
					stopDiagnostic()
					requests, bytes := f.api.Counts()
					t.Fatalf("NET-LB-OBS-01 initialization expired: budget=%s capability=%s capability_error=%v observer=%v api_requests=%v api_bytes=%v", initializationBudget, capability, capabilityErr, obs.Snapshot(), requests, bytes)
				}
			}
			stopInitialization()
			probe := &lbAuditRetryProvider{KCProvider: provider}
			policy := biz.DefaultWorkerPolicy()
			policy.Lease, policy.RequestTimeout, policy.ObserveEvery, policy.StaleAfter = 120*time.Second, 30*time.Second, 5*time.Second, 60*time.Second
			// The live runner used 30s; DefaultWorkerPolicy/config.yaml use 60s.
			// Record the actual policy below rather than implying all defaults match.
			policy.RetryMax = 30 * time.Second
			var finishedWork biz.Work
			var finishError error
			repository := &lbAuditRetryRepository{Postgres: f.f.p, target: initial.ID}
			worker, err := biz.NewWorker(repository, probe, uuid.NewString(), policy, func(_ context.Context, w biz.Work, p biz.Progress, err error) {
				finishedWork, finishError = w, err
			})
			if err != nil {
				t.Fatal(err)
			}
			step := func() {
				// Scheduling only: no SQL writes to health, identity, or proof data.
				if _, err := f.f.owner.Exec(f.f.ctx, `UPDATE network_reconciliations SET next_run_at=clock_timestamp()-interval '1 hour' WHERE tenant_id=$1 AND lb_id=$2`, f.f.tenant, initial.ID); err != nil {
					t.Fatal(err)
				}
				if worked, err := worker.Step(f.f.ctx); err != nil || !worked {
					t.Fatalf("step: worked=%v err=%v", worked, err)
				}
				if finishedWork.Resource.ID != initial.ID {
					t.Fatalf("wrong work selected: %s", finishedWork.Resource.ID)
				}
			}
			step()
			baseline, err := f.lbs.Get(f.f.ctx, "", initial.ID)
			if err != nil || probe.err != nil || baseline.ConfigurationState != "configured" || baseline.ObservationStale {
				t.Fatalf("measured-provider precondition: lb=%+v adapter=%v read=%v", baseline, probe.err, err)
			}
			// A durable parent hint is not a changed Provider fact. Reuse the
			// complete unchanged view without renewing its collection timestamp.
			hinted := finishedWork
			hinted.Requirement.RequestedGeneration++
			hinted.Requirement.Hash = probe.observation.Proof.Hash
			hinted.Requirement.AppliedAt = time.Now()
			lists := obs.Snapshot()["requests_LIST_pods_total"]
			hintResult, hintErr := provider.ObserveLoadBalancer(f.f.ctx, hinted)
			if hintErr != nil || !hintResult.GeneratedReady || !hintResult.Proof.CollectedAt.Equal(probe.observation.Proof.CollectedAt) || hintResult.Proof.CoveredGeneration != hinted.Requirement.RequestedGeneration || obs.Snapshot()["requests_LIST_pods_total"] != lists {
				t.Fatalf("unchanged LB hint must reuse the original proof without LIST: proof=%+v error=%v", hintResult.Proof, hintErr)
			}
			attachmentID := baseline.Backends[0].AttachmentID
			if _, err = f.f.owner.Exec(f.f.ctx, `UPDATE network_attachments SET observed_at=clock_timestamp()-interval '10 minutes' WHERE tenant_id=$1 AND attachment_id=$2`, f.f.tenant, attachmentID); err != nil {
				t.Fatal(err)
			}
			step()
			pending, err := f.lbs.Get(f.f.ctx, "", initial.ID)
			if err != nil || pending.State != biz.Available || pending.ConfigurationState != "configured" || !pending.ObservationStale || pending.Reason != biz.ProviderUnavailable || !pending.ObservedAt.Equal(*baseline.ObservedAt) {
				t.Fatalf("old attachment projection fabricated a backend failure or renewed time: %+v %v", pending, err)
			}
			if _, err = f.f.owner.Exec(f.f.ctx, `UPDATE network_attachments SET observed_at=clock_timestamp() WHERE tenant_id=$1 AND attachment_id=$2`, f.f.tenant, attachmentID); err != nil {
				t.Fatal(err)
			}
			beforeNotifications := obs.Snapshot()["notifications_total"]
			pod := f.api.Object("pods", "tenant-"+f.f.tenant, "lb-backend")
			metadata := pod["metadata"].(map[string]any)
			annotations, _ := metadata["annotations"].(map[string]any)
			if annotations == nil {
				annotations = map[string]any{}
				metadata["annotations"] = annotations
			}
			annotations["net-lb-obs-01"] = "boundary-trigger"
			f.api.Change("pods", pod, false)
			awaitNET05A(t, 3*time.Second, func() bool { return obs.Snapshot()["notifications_total"] > beforeNotifications })
			factsBefore := lbAuditRetryObjects(f.api)
			armed.Store(true)
			step() // An ordinary invalidated projection must not start a LIST.
			actual, err := f.lbs.Get(f.f.ctx, "", initial.ID)
			if err != nil || actual.State != biz.Available || !actual.ObservationStale || probe.err == nil || captured.Load() {
				t.Fatalf("ordinary observation fabricated health or started a full collection: state=%s stale=%v error=%v captured=%v", actual.State, actual.ObservationStale, probe.err, captured.Load())
			}
			criticalWork := finishedWork
			criticalWork.ActiveOperation = true
			criticalCtx, stopCritical := context.WithTimeout(f.f.ctx, policy.RequestTimeout)
			criticalResult, criticalErr := provider.ObserveLoadBalancer(criticalCtx, criticalWork)
			stopCritical()
			armed.Store(false)
			if criticalErr != nil || !criticalResult.GeneratedReady || criticalResult.Proof.CollectedAt.IsZero() {
				t.Fatalf("critical observation did not survive relists: %+v %v", criticalResult, criticalErr)
			}
			actual, err = f.lbs.Get(f.f.ctx, "", initial.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !captured.Load() || boundaries.Load() != 2 {
				t.Fatalf("trigger not covered: captured=%v boundaries=%d", captured.Load(), boundaries.Load())
			}
			if factsBefore != lbAuditRetryObjects(f.api) {
				t.Fatal("Provider facts changed during the controlled boundary-only attempt")
			}
			if finishError != nil {
				t.Fatalf("Finish failed: %v", finishError)
			}
			if probe.finished.After(probe.deadline.Add(time.Second)) {
				t.Fatal("adapter exceeded caller deadline")
			}
			if actual.ConfigurationState != "configured" || !actual.ObservationStale || actual.State != biz.Available {
				t.Fatalf("stable CR facts degraded: exposure=%s state=%s reason=%s stale=%v elapsed=%s remaining=%s error=%v", exposure, actual.ConfigurationState, actual.Reason, actual.ObservationStale, probe.finished.Sub(probe.started), probe.deadline.Sub(probe.finished), probe.err)
			}
			t.Logf("%s retains configured/available with a stale warning; critical observation covers %d relist boundaries", exposure, boundaries.Load())
			// A continuously invalidated audit must not extend the caller's
			// deadline or return partial proof. No-deadline callers get only
			// the original bounded fallback, never an infinite retry loop.
			work := finishedWork
			work.ActiveOperation = true
			repeat.Store(true)
			armed.Store(true)
			limited, stop := context.WithTimeout(f.f.ctx, 6*time.Second)
			started := time.Now()
			blocked, failure := provider.ObserveLoadBalancer(limited, work)
			stop()
			if !errors.Is(failure, context.DeadlineExceeded) || !blocked.Proof.CollectedAt.IsZero() || time.Since(started) > 7*time.Second {
				t.Fatalf("audit retry exceeded deadline or returned proof: %+v %v", blocked, failure)
			}
			armed.Store(false)
			repeat.Store(false)
			// Cancellation only detaches the caller from a shared audit. Stop
			// that observer before measuring the no-deadline fallback, so the
			// preceding canceled collection cannot consume this case's budget.
			stopCtx, stopObserver := context.WithTimeout(f.f.ctx, options.AuditTimeout+time.Second)
			if err = obs.Stop(stopCtx); err != nil {
				stopObserver()
				t.Fatal(err)
			}
			stopObserver()
			fallbackConfig := *config
			// Rate limiting is not under test. Complete fixture LISTs promptly
			// while retaining the actual watch renewals and audit timeouts.
			fallbackConfig.QPS, fallbackConfig.Burst = 1000, 1000
			fallbackProvider, err := data.NewKCProvider(f.f.p, &fallbackConfig)
			if err != nil {
				t.Fatal(err)
			}
			if err = fallbackProvider.ConfigureLoadBalancer(expected); err != nil {
				t.Fatal(err)
			}
			fallbackOptions := options
			fallbackOptions.AuditTimeout = 3 * time.Second
			// Only the measured request initiates collections after startup.
			fallbackOptions.AuditInterval, fallbackOptions.AuditJitter = time.Minute, time.Millisecond
			fallbackObserver, err := fallbackProvider.EnableObservation(fallbackOptions)
			if err != nil {
				t.Fatal(err)
			}
			observer.Store(fallbackObserver)
			fallbackCtx, cancelFallback := context.WithCancel(f.f.ctx)
			fallbackDone := make(chan error, 1)
			go func() { fallbackDone <- fallbackObserver.Start(fallbackCtx) }()
			t.Cleanup(func() {
				cancelFallback()
				if err := <-fallbackDone; err != nil {
					t.Error(err)
				}
			})
			awaitNET05A(t, 2*fallbackOptions.AuditTimeout+time.Second, func() bool {
				metrics := fallbackObserver.Snapshot()
				_, collected := metrics["audit_evidence_age_seconds"]
				_, collecting := metrics["audit_collecting_seconds"]
				return metrics["source_synced"] == 1 && collected && !collecting
			})
			fallbackCollections.Store(0)
			repeat.Store(true)
			armed.Store(true)
			// One initial collection plus at most one fresh fallback, each
			// bounded by AuditTimeout; allow one second for local scheduling.
			fallbackBudget := 2*fallbackOptions.AuditTimeout + time.Second
			started = time.Now()
			blocked, failure = fallbackProvider.ObserveLoadBalancer(f.f.ctx, work)
			elapsed, collections := time.Since(started), fallbackCollections.Load()
			if failure == nil || !blocked.Proof.CollectedAt.IsZero() || elapsed > fallbackBudget || collections < 1 || collections > 2 {
				t.Fatalf("unbounded caller did not stop after bounded fallback: elapsed=%s budget=%s collections=%d observation=%+v err=%v", elapsed, fallbackBudget, collections, blocked, failure)
			}
			armed.Store(false)
			repeat.Store(false)
			// A real read error is not an audit invalidation retry signal.
			deny.Store(true)
			bounded, stop := context.WithTimeout(f.f.ctx, 10*time.Second)
			blocked, failure = fallbackProvider.ObserveLoadBalancer(bounded, work)
			stop()
			deny.Store(false)
			if failure == nil || errors.Is(failure, context.DeadlineExceeded) || denied.Load() != 1 || !blocked.Proof.CollectedAt.IsZero() {
				t.Fatalf("read failure was retried or returned proof: denied=%d observation=%+v err=%v", denied.Load(), blocked, failure)
			}
			canceled, stop := context.WithCancel(f.f.ctx)
			stop()
			blocked, failure = fallbackProvider.ObserveLoadBalancer(canceled, work)
			if !errors.Is(failure, context.Canceled) || !blocked.Proof.CollectedAt.IsZero() {
				t.Fatalf("cancellation returned usable proof: %+v %v", blocked, failure)
			}

		})
	}
}
