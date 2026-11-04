package data_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	mathrand "math/rand"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/server"
	controlled "github.com/zhangzhe-ctrl/ani-resource-service/tests/net05a/provider"
	"github.com/zhangzhe-ctrl/ani-resource-service/tests/testenv"
	"k8s.io/client-go/rest"
)

type capacityConsumer struct {
	pods map[string]biz.WorkloadReference
}

func (c capacityConsumer) GetSubmission(_ context.Context, a biz.Attachment) (biz.ConsumerSubmission, error) {
	return biz.ConsumerSubmission{ProtocolVersion: 1, TenantID: a.TenantID, InstanceID: a.InstanceID, SubmissionID: a.SubmissionID, AttachmentID: a.ID, ClusterID: a.ClusterID, Namespace: a.Namespace, Generation: a.Generation, State: "open", PodUIDs: []string{c.pods[a.ID].UID}, CurrentPods: []biz.PodReference{{Reference: c.pods[a.ID]}}}, nil
}

type capacityResource struct{ kind, tenant, id, namespace, name string }
type capacityChange struct {
	resource capacityResource
	state    string
	at       time.Time
}

type capacityTiming struct {
	mu       sync.Mutex
	maximum  map[string]float64
	attempts map[string]int
}
type capacityStepper struct {
	stepper interface {
		Step(context.Context) (bool, error)
	}
	kind   string
	timing *capacityTiming
}

func (s capacityStepper) Step(ctx context.Context) (bool, error) {
	started := time.Now()
	worked, e := s.stepper.Step(ctx)
	if worked {
		s.timing.mu.Lock()
		s.timing.maximum[s.kind] = max(s.timing.maximum[s.kind], time.Since(started).Seconds())
		s.timing.attempts[s.kind]++
		s.timing.mu.Unlock()
	}
	return worked, e
}
func (s *capacityTiming) snapshot() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := map[string]any{}
	for k, v := range s.maximum {
		m[k+"_max_seconds"] = v
		m[k+"_attempts"] = s.attempts[k]
	}
	return m
}

func TestNET05ACapacity(t *testing.T) {
	count, _ := strconv.Atoi(os.Getenv("NET05A_CAPACITY_ATTACHMENTS"))
	if count == 0 {
		t.Skip("explicit fixed remote capacity run required")
	}
	if count != 100 && count != 1000 && count != 2000 {
		t.Fatal("unfrozen capacity dataset")
	}
	replicas, _ := strconv.Atoi(os.Getenv("NET05A_CAPACITY_REPLICAS"))
	if replicas != 1 && replicas != 2 {
		t.Fatal("unfrozen replica count")
	}
	out := os.Getenv("NET05A_CAPACITY_OUTPUT")
	if out == "" {
		t.Fatal("capacity evidence output required")
	}
	if e := os.MkdirAll(out, 0700); e != nil {
		t.Fatal(e)
	}
	// Seed acceptance and controlled Provider identities in one deterministic order
	// before starting any concurrent executor. Same baseline/new inputs, no secret
	// depends on this test-only UUID source. Restore crypto randomness afterwards.
	uuid.SetRand(mathrand.New(mathrand.NewSource(5102026)))
	defer uuid.SetRand(nil)
	fixture := testenv.NewDatabase(t)
	p, db := fixture.Repository, fixture.Owner
	ctx := context.Background()
	api := controlled.New()
	host := httptest.NewServer(api)
	defer host.Close()
	provider, e := data.NewKCProvider(p, &rest.Config{Host: host.URL, QPS: 40, Burst: 80})
	if e != nil {
		t.Fatal(e)
	}
	n := newNetwork(t, p, time.Minute)
	policy := biz.DefaultWorkerPolicy()
	worker, e := biz.NewWorker(p, provider, uuid.NewString(), policy)
	if e != nil {
		t.Fatal(e)
	}
	resources := []capacityResource{}
	subnets := []biz.Subnet{}
	for tenantIndex := 0; tenantIndex < 10; tenantIndex++ {
		tenant := uuid.NewString()
		for vpcIndex := 0; vpcIndex < 2; vpcIndex++ {
			v := availableVPC(t, n, worker, tenant, fmt.Sprintf("cap-%d-%d", tenantIndex, vpcIndex))
			resources = append(resources, capacityResource{"vpc", tenant, v.ID, "tenant-" + tenant, "vpc-" + v.ID[4:]})
			for j := 0; j < 5; j++ {
				s, e := n.CreateSubnet(ctx, biz.CreateSubnet{TenantID: tenant, VPCID: v.ID, Name: fmt.Sprintf("cap-subnet-%d-%d-%d", tenantIndex, vpcIndex, j), CIDR: fmt.Sprintf("10.42.%d.0/24", j+1), IdempotencyKey: fmt.Sprintf("s-%d-%d", vpcIndex, j)})
				if e != nil {
					t.Fatal(e)
				}
				s = driveSubnet(t, n, worker, s, biz.Available)
				subnets = append(subnets, s)
				resources = append(resources, capacityResource{"subnet", tenant, s.ID, "tenant-" + tenant, "subnet-" + s.ID[7:]})
			}
		}
	}
	attachments := biz.NewAttachments(p)
	consumer := capacityConsumer{map[string]biz.WorkloadReference{}}
	padding := strings.Repeat("x", 512)
	for i := 0; i < count; i++ {
		// Seed through the real use cases; refresh parents through the worker as needed.
		if i%100 == 0 {
			for j := 0; j < 150; j++ {
				if _, err := worker.Step(ctx); err != nil {
					t.Fatal(err)
				}
			}
		}
		s := subnets[i%len(subnets)]
		a, e := attachments.Prepare(ctx, biz.PrepareAttachment{TenantID: s.TenantID, VPCID: s.VPCID, SubnetID: s.ID, InstanceID: "inst_" + strings.ReplaceAll(uuid.NewString(), "-", ""), Slot: "primary", RequestKey: fmt.Sprintf("capacity-%d", i), SubmissionID: uuid.NewString(), Generation: 1, ClusterID: "test-cluster", Namespace: "tenant-" + s.TenantID})
		if e != nil {
			t.Fatal(e)
		}
		name := fmt.Sprintf("pod-%d", i)
		podUID, nicUID := uuid.NewString(), uuid.NewString()
		consumer.pods[a.ID] = biz.WorkloadReference{APIVersion: "v1", Kind: "Pod", Namespace: a.Namespace, Name: name, UID: podUID}
		pod := attachmentPod(a, name, podUID)
		pod["metadata"].(map[string]any)["annotations"].(map[string]any)["net05a.test/padding"] = padding
		api.Change("pods", pod, false)
		nicName := fmt.Sprintf("nic-%d", i)
		api.Change("vnics", map[string]any{"apiVersion": "networking.kubercloud.com/v1", "kind": "VNic", "metadata": map[string]any{"namespace": a.Namespace, "name": nicName, "uid": nicUID, "annotations": map[string]any{"net05a.test/padding": padding}, "ownerReferences": []any{map[string]any{"apiVersion": "v1", "kind": "Pod", "name": name, "uid": podUID}}}, "spec": map[string]any{"type": "VETH", "subnet": a.Plan.PrimaryNetworkRef}}, false)
		api.Change("vnicips", map[string]any{"apiVersion": "networking.kubercloud.com/v1", "kind": "VNicIP", "metadata": map[string]any{"namespace": a.Namespace, "name": fmt.Sprintf("ip-%d", i), "uid": uuid.NewString(), "annotations": map[string]any{"net05a.test/padding": padding}, "ownerReferences": []any{map[string]any{"apiVersion": "networking.kubercloud.com/v1", "kind": "VNic", "name": nicName, "uid": nicUID}}}, "spec": map[string]any{"vNic": nicName, "subnet": a.Plan.PrimaryNetworkRef}}, false)
	}
	for i, s := range subnets {
		api.Change("eips", map[string]any{"apiVersion": "networking.kubercloud.com/v1", "kind": "EIP", "metadata": map[string]any{"namespace": "tenant-" + s.TenantID, "name": fmt.Sprintf("eip-%d", i), "uid": uuid.NewString(), "annotations": map[string]any{"net05a.test/padding": padding}}, "spec": map[string]any{"subnet": "tenant-" + s.TenantID + "/subnet-" + s.ID[7:]}}, false)
	}
	api.Backend.Mu.Lock()
	body, _ := json.Marshal(api.Backend.Objects)
	objectCount := len(api.Backend.Objects)
	api.Backend.Mu.Unlock()
	digest := sha256.Sum256(body)
	uuid.SetRand(nil)
	pgStats := func() map[string]float64 {
		var tx, updates, wal float64
		err := db.QueryRow(ctx, `SELECT (SELECT (xact_commit+xact_rollback)::float8 FROM pg_stat_database WHERE datname=current_database()), (SELECT coalesce(sum(n_tup_upd),0)::float8 FROM pg_stat_user_tables),(SELECT wal_bytes::float8 FROM pg_stat_wal)`).Scan(&tx, &updates, &wal)
		if err != nil {
			t.Fatal(err)
		}
		return map[string]float64{"transactions": tx, "updates": updates, "wal_bytes": wal}
	}
	coldPG := pgStats()
	var coldCPU syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &coldCPU)
	coldRequests, coldBytes := api.Counts()
	measurementStart := time.Now()
	live, cancel := context.WithCancel(ctx)
	var group sync.WaitGroup
	defer func() { cancel(); group.Wait() }()
	observers := []capacityMetrics{}
	timings := &capacityTiming{maximum: map[string]float64{}, attempts: map[string]int{}}
	for replica := 0; replica < replicas; replica++ {
		replicaDB, e := data.OpenPostgres(ctx, fixture.RuntimeDSN, data.Placement{ClusterID: "test-cluster", NamespacePrefix: "tenant-"})
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(replicaDB.Close)
		provider, e := data.NewKCProvider(replicaDB, &rest.Config{Host: host.URL, QPS: 40, Burst: 80})
		if e != nil {
			t.Fatal(e)
		}
		resourceWorker, e := biz.NewWorker(replicaDB, provider, uuid.NewString(), policy)
		if e != nil {
			t.Fatal(e)
		}
		attachmentWorker, e := biz.NewAttachmentWorker(replicaDB, consumer, provider, uuid.NewString(), policy)
		if e != nil {
			t.Fatal(e)
		}
		execution := server.NewWorkerServer(capacityStepper{resourceWorker, "resource", timings}, replicaDB, slog.New(slog.NewTextHandler(io.Discard, nil)), 100*time.Millisecond, capacityStepper{attachmentWorker, "attachment", timings})
		observer, launch := configureCapacity(t, provider, execution)
		observers = append(observers, observer)
		group.Add(1)
		go func() { defer group.Done(); launch(live) }()
	}
	summary := map[string]any{"seed": 5102026, "attachments": count, "replicas": replicas, "replica_isolation": "independent runtime DB pools, leases, informers and executors in one measured Go process; not process-isolation evidence", "objects": objectCount, "object_bytes": len(body), "dataset_sha256": hex.EncodeToString(digest[:]), "variant": capacityVariant, "phase_results": []any{}}
	file, e := os.Create(filepath.Join(out, "samples.jsonl"))
	if e != nil {
		t.Fatal(e)
	}
	defer file.Close()
	encoder := json.NewEncoder(file)
	pending := []capacityChange{}
	sequence := 0
	ready := map[string]bool{}
	coldConverged := -1.0

	fairness := map[string]map[string]float64{}
	for _, phase := range []struct {
		name     string
		duration time.Duration
		rate     int
	}{{"cold_start", 90 * time.Second, 0}, {"steady", 90 * time.Second, 0}, {"normal_changes", 90 * time.Second, 10}, {"storm", 30 * time.Second, 200}, {"reconnect", 90 * time.Second, 0}} {
		started := time.Now()
		if phase.name == "cold_start" {
			started = measurementStart
		}
		beforePG := pgStats()
		var beforeCPU syscall.Rusage
		_ = syscall.Getrusage(syscall.RUSAGE_SELF, &beforeCPU)
		if phase.name == "cold_start" {
			beforePG = coldPG
			beforeCPU = coldCPU
		}
		maximumRSS := int64(0)
		emitted, missed, overwritten := 0, 0, 0
		pending = nil
		if phase.name == "reconnect" {
			api.Disconnect(410)
		}
		latencies := []float64{}
		maxEvidence, maxQueue := 0.0, 0.0
		beforeRequests, beforeBytes := api.Counts()
		if phase.name == "cold_start" {
			beforeRequests, beforeBytes = coldRequests, coldBytes
		}
		lastSample := time.Time{}
		nextEvent := time.Now()
		ticker := time.NewTicker(5 * time.Millisecond)
		drainDeadline := started.Add(phase.duration)
		if phase.name == "normal_changes" {
			drainDeadline = drainDeadline.Add(5 * time.Second)
		}
		for time.Now().Before(started.Add(phase.duration)) || (len(pending) > 0 && time.Now().Before(drainDeadline)) {
			<-ticker.C
			for phase.rate > 0 && !time.Now().Before(nextEvent) && nextEvent.Before(started.Add(phase.duration)) {
				resource := resources[sequence%len(resources)]
				sequence++
				current := ready[resource.id]
				desired := "False"
				want := "degraded"
				if current {
					desired = "True"
					want = "available"
				}
				ready[resource.id] = !current
				emitted++
				if len(pending) > 0 {
					filtered := pending[:0]
					for _, change := range pending {
						if change.resource.id == resource.id {
							missed++
							overwritten++
						} else {
							filtered = append(filtered, change)
						}
					}
					pending = filtered
				}
				plural := resource.kind + "s"
				obj := api.Object(plural, resource.namespace, resource.name)
				obj["status"].(map[string]any)["conditions"].([]any)[2].(map[string]any)["status"] = desired
				at := time.Now()
				api.Change(plural, obj, false)
				if phase.name == "normal_changes" {
					pending = append(pending, capacityChange{resource, want, at})
				}
				nextEvent = nextEvent.Add(time.Second / time.Duration(phase.rate))
			}
			if time.Since(lastSample) < 100*time.Millisecond {
				continue
			}
			lastSample = time.Now()
			rows, e := db.Query(ctx, `SELECT 'vpc',tenant_id,vpc_id,state,observed_at FROM network_vpcs UNION ALL SELECT 'subnet',tenant_id,subnet_id,state,observed_at FROM network_subnets`)
			if e != nil {
				t.Fatal(e)
			}
			states := map[string]string{}
			observedTimes := map[string]time.Time{}
			for rows.Next() {
				var kind, tenant, id, state string
				var observed *time.Time
				if e = rows.Scan(&kind, &tenant, &id, &state, &observed); e != nil {
					t.Fatal(e)
				}
				states[id] = state
				if observed != nil {
					observedTimes[id] = *observed
				}
				if observed != nil && time.Since(*observed).Seconds() > maxEvidence {
					maxEvidence = time.Since(*observed).Seconds()
				}
			}
			rows.Close()
			remaining := pending[:0]
			for _, change := range pending {
				if states[change.resource.id] == change.state && !observedTimes[change.resource.id].Before(change.at) {
					latencies = append(latencies, time.Since(change.at).Seconds())
				} else {
					remaining = append(remaining, change)
				}
			}
			pending = remaining
			var attached int
			var age, queue float64
			e = db.QueryRow(ctx, `SELECT count(*) FILTER(WHERE state='attached'),coalesce(max(extract(epoch FROM clock_timestamp()-observed_at)),0)::float8,coalesce(greatest(0,extract(epoch FROM clock_timestamp()-min(next_check_at) FILTER(WHERE next_check_at<>'infinity'::timestamptz))),0)::float8 FROM network_attachments`).Scan(&attached, &age, &queue)
			if e != nil {
				t.Fatal(e)
			}
			if age > maxEvidence {
				maxEvidence = age
			}
			if queue > maxQueue {
				maxQueue = queue
			}
			if phase.name == "cold_start" && attached == count && coldConverged < 0 {
				coldConverged = time.Since(started).Seconds()
			}
			if int(time.Since(started).Milliseconds())%1000 < 110 {
				var usage syscall.Rusage
				_ = syscall.Getrusage(syscall.RUSAGE_SELF, &usage)
				maximumRSS = max(maximumRSS, usage.Maxrss*1024)
				fairRows, err := db.Query(ctx, `SELECT kind,tenant_id,parent_id,count(*),coalesce(max(extract(epoch FROM clock_timestamp()-observed_at)),0)::float8 FROM (SELECT 'vpc' AS kind,tenant_id,vpc_id AS parent_id,observed_at FROM network_vpcs UNION ALL SELECT 'subnet',tenant_id,vpc_id,observed_at FROM network_subnets UNION ALL SELECT 'attachment',tenant_id,subnet_id,observed_at FROM network_attachments) f GROUP BY kind,tenant_id,parent_id`)
				if err != nil {
					t.Fatal(err)
				}
				for fairRows.Next() {
					var kind, tenant, parent string
					var total int
					var age float64
					if err = fairRows.Scan(&kind, &tenant, &parent, &total, &age); err != nil {
						t.Fatal(err)
					}
					key := kind + "/" + tenant + "/" + parent
					entry := fairness[key]
					if entry == nil {
						entry = map[string]float64{"rows": float64(total)}
						fairness[key] = entry
					}
					entry["maximum_observation_age_seconds"] = max(entry["maximum_observation_age_seconds"], age)
				}
				fairRows.Close()
				stats := map[string]any{}
				for i, o := range observers {
					if o != nil {
						stats[strconv.Itoa(i)] = o.Snapshot()
					}
				}
				var memory runtime.MemStats
				runtime.ReadMemStats(&memory)
				_ = encoder.Encode(map[string]any{"phase": phase.name, "elapsed_seconds": time.Since(started).Seconds(), "attached": attached, "heap_alloc": memory.HeapAlloc, "heap_sys": memory.HeapSys, "rss_high_water_bytes": maximumRSS, "maximum_observation_age_seconds": maxEvidence, "queue_age_seconds": queue, "observer": stats, "worker": timings.snapshot()})
			}
		}
		ticker.Stop()
		afterRequests, afterBytes := api.Counts()
		for k, v := range beforeRequests {
			afterRequests[k] -= v
		}
		for k, v := range beforeBytes {
			afterBytes[k] -= v
		}
		for _, change := range pending {
			if phase.name == "normal_changes" {
				missed++
				latencies = append(latencies, max(5.0, time.Since(change.at).Seconds()))
			}
		}
		pending = nil
		// Overwritten changes are right-censored failures, never removed from the SLO.
		for i := 0; i < overwritten; i++ {
			latencies = append(latencies, phase.duration.Seconds())
		}
		sort.Float64s(latencies)
		percentile := func(q float64) float64 {
			if len(latencies) == 0 {
				return -1
			}
			return latencies[min(len(latencies)-1, int(float64(len(latencies)-1)*q))]
		}
		afterPG := pgStats()
		for k, v := range beforePG {
			afterPG[k] -= v
		}
		var afterCPU syscall.Rusage
		_ = syscall.Getrusage(syscall.RUSAGE_SELF, &afterCPU)
		cpu := func(r syscall.Rusage) float64 {
			return float64(r.Utime.Sec+r.Stime.Sec) + float64(r.Utime.Usec+r.Stime.Usec)/1e6
		}
		result := map[string]any{"pg_delta": afterPG, "cpu_seconds": cpu(afterCPU) - cpu(beforeCPU), "rss_high_water_bytes": maximumRSS, "emitted_events": emitted, "missed_changes": missed, "phase": phase.name, "event_phase_seconds": phase.duration.Seconds(), "measurement_tail_seconds": max(0.0, time.Since(started).Seconds()-phase.duration.Seconds()), "seconds": time.Since(started).Seconds(), "requests": afterRequests, "bytes": afterBytes, "p50": percentile(.5), "p95": percentile(.95), "p99": percentile(.99), "latency_samples": len(latencies), "unapplied_changes": len(pending), "max_observation_age_seconds": maxEvidence, "max_queue_age_seconds": maxQueue}
		summary["phase_results"] = append(summary["phase_results"].([]any), result)
		summary["cold_converged_seconds"] = coldConverged
		summary["fairness"] = fairness
		summary["worker"] = timings.snapshot()
		b, _ := json.MarshalIndent(summary, "", "  ")
		if e = os.WriteFile(filepath.Join(out, "summary.json"), append(b, '\n'), 0600); e != nil {
			t.Fatal(e)
		}
		t.Logf("capacity %s %d replicas=%d phase=%s p95=%.3f p99=%.3f age=%.3f", capacityVariant, count, replicas, phase.name, percentile(.95), percentile(.99), maxEvidence)
	}
}
