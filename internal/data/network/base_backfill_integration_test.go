package data_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/tests/testenv"
)

func TestBaseBackfillCLIProcessRestartKeepsReviewedPlanAndSingleAdmission(t *testing.T) {
	if testing.Short() {
		t.Skip("production command process gate is disabled by -short")
	}
	f, db := newBackfillFixture(t)
	availableVPC(t, f.n, f.w, f.tenant, "process-legacy")
	history := historicalCreateSnapshot(t, f)
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate process build source")
	}
	repositoryRoot := filepath.Clean(filepath.Join(filepath.Dir(filename), "..", "..", ".."))
	binary := filepath.Join(t.TempDir(), "network")
	build := exec.Command("go", "build", "-trimpath", "-o", binary, "./cmd/ani-resource-service")
	build.Dir = repositoryRoot
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build command: %v\n%s", err, output)
	}
	runID := uuid.NewString()
	run := func(action string, extra ...string) []byte {
		t.Helper()
		ctx, cancel := context.WithTimeout(f.ctx, 10*time.Second)
		defer cancel()
		args := append([]string{"-base-connectivity=" + action, "-base-run=" + runID}, extra...)
		command := exec.CommandContext(ctx, binary, args...)
		for _, entry := range os.Environ() {
			key, _, _ := strings.Cut(entry, "=")
			switch key {
			case "NETWORK_TEST_ADMIN_DSN", "ANI_NETWORK_MIGRATION_DSN", "ANI_NETWORK_DATABASE_DSN", "ANI_NETWORK_CLUSTER_ID", "ANI_NETWORK_NAMESPACE_PREFIX", "ANI_NETWORK_KUBECONFIG":
				continue
			}
			command.Env = append(command.Env, entry)
		}
		command.Env = append(command.Env, "ANI_NETWORK_DATABASE_DSN="+db.RuntimeDSN, "ANI_NETWORK_CLUSTER_ID=test-cluster", "ANI_NETWORK_NAMESPACE_PREFIX=tenant-", "ANI_NETWORK_KUBECONFIG=/does-not-exist/backfill-never-opens-provider")
		var stdout, stderr bytes.Buffer
		command.Stdout, command.Stderr = &stdout, &stderr
		if err := command.Run(); err != nil {
			t.Fatalf("command %s: %v; %s", action, err, strings.ReplaceAll(stdout.String()+stderr.String(), db.RuntimeDSN, "<redacted>"))
		}
		if strings.Contains(stdout.String()+stderr.String(), db.RuntimeDSN) {
			t.Fatal("command printed database credentials")
		}
		return stdout.Bytes()
	}
	var plan data.BaseBackfillPlan
	if err := json.Unmarshal(run("plan", "-base-interval=100ms"), &plan); err != nil {
		t.Fatal(err)
	}
	if !plan.Paused || len(plan.Candidates) != 1 {
		t.Fatal("process plan", plan)
	}
	if !bytes.Contains(run("dispatch"), []byte(`"state":"paused"`)) {
		t.Fatal("unreviewed child process admitted")
	}
	run("resume", "-base-reviewed-sha256="+plan.SHA256)
	if !bytes.Contains(run("dispatch"), []byte(`"state":"accepted"`)) {
		t.Fatal("reviewed child process did not admit")
	}
	if !bytes.Contains(run("dispatch"), []byte(`"state":"admission_complete"`)) {
		t.Fatal("restarted process duplicated admission")
	}
	var recovered data.BaseBackfillPlan
	if err := json.Unmarshal(run("status"), &recovered); err != nil {
		t.Fatal(err)
	}
	if recovered.SHA256 != plan.SHA256 || recovered.Candidates[0].OperationState != "queued" || recovered.Candidates[0].State != "accepted" {
		t.Fatal("durable process handoff", recovered)
	}
	if history != historicalCreateSnapshot(t, f) {
		t.Fatal("process admission changed old operation or idempotency snapshot")
	}
}

func newBackfillFixture(t *testing.T) (*egressFixture, *testenv.Database) {
	t.Helper()
	db := testenv.NewDatabase(t)
	configureRepositoryVPCPresets(t, db.Repository)
	infra := testEgressInfrastructure{}
	e, err := biz.NewEgress(db.Repository, infra, biz.ContextEgressAuthorization{}, []byte("0123456789abcdef0123456789abcdef"), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	f := &egressFixture{p: db.Repository, owner: db.Owner, e: e, n: newNetwork(t, db.Repository, time.Minute), tenant: uuid.NewString(), provider: &egressProvider{}}
	f.w = egressWorker(t, f.p, f.provider)
	f.ctx = biz.WithEgressCaller(context.Background(), biz.EgressCaller{TenantID: f.tenant, PlatformAdministrator: true})
	f.pool = readyIntranetPool(t, f, f.platform(t, intranetIntent("backfill-pool", "10.234.254.0/24", "10.234.254.1")))
	f.pool = setIntranetPool(t, f, biz.PlatformIntent{Kind: "set_default_intranet_pool", ID: f.pool.ID, IdempotencyKey: "backfill-default"})
	return f, db
}

func planBackfill(t *testing.T, f *egressFixture) data.BaseBackfillPlan {
	t.Helper()
	plan, err := f.p.PlanBaseBackfill(f.ctx, data.BaseBackfillPlanInput{RunID: uuid.NewString(), Interval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Paused || plan.ReviewedAt != nil {
		t.Fatal("plan started admission", plan)
	}
	return plan
}

func resumeBackfill(t *testing.T, f *egressFixture, plan data.BaseBackfillPlan) {
	t.Helper()
	if _, err := f.p.SetBaseBackfillPaused(f.ctx, plan.RunID, false, plan.SHA256); err != nil {
		t.Fatal(err)
	}
}

func dueBackfill(t *testing.T, f *egressFixture, runID string) {
	t.Helper()
	// Controlled clock positioning after independently proving the persisted rate
	// gate. This only advances the task-owned fixture's next admission deadline.
	if _, err := f.owner.Exec(f.ctx, "UPDATE network_base_backfill_runs SET next_admission_at=clock_timestamp() WHERE run_id=$1", runID); err != nil {
		t.Fatal(err)
	}
}

func historicalCreateSnapshot(t *testing.T, f *egressFixture) string {
	t.Helper()
	var out string
	err := f.owner.QueryRow(f.ctx, `SELECT jsonb_build_object(
 'operations',(SELECT jsonb_agg(to_jsonb(o) ORDER BY operation_id) FROM network_operations o WHERE kind='create_vpc'),
 'idempotency',(SELECT jsonb_agg(to_jsonb(i) ORDER BY tenant_id,idempotency_key) FROM network_idempotency i WHERE operation_kind='create_vpc'))::text`).Scan(&out)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestBaseBackfillReviewedSnapshotPoolPinningRatePauseAndConnectionRecovery(t *testing.T) {
	f, db := newBackfillFixture(t)
	second := readyIntranetPool(t, f, f.platform(t, intranetIntent("backfill-second", "10.235.254.0/24", "10.235.254.1")))
	availableVPC(t, f.n, f.w, f.tenant, "legacy-one")
	availableVPC(t, f.n, f.w, uuid.NewString(), "legacy-two")
	history := historicalCreateSnapshot(t, f)
	plan := planBackfill(t, f)
	if len(plan.Candidates) != 2 {
		t.Fatal("tenant-scoped candidate inventory", plan)
	}
	for _, c := range plan.Candidates {
		if c.State != "pending" || c.Snapshot.Namespace == "" || c.Snapshot.BindingID == "" || c.Snapshot.ProviderUID == "" {
			t.Fatal("candidate identity missing", c)
		}
	}
	// Ordinary worker observations may advance versions while the reviewed plan
	// is paused. Stable placement/UID and current eligibility remain authoritative.
	observedCandidate := plan.Candidates[0].Snapshot
	f.drive(t, func() bool {
		current, err := f.n.GetVPC(f.ctx, observedCandidate.TenantID, observedCandidate.VPCID)
		return err == nil && current.Version > observedCandidate.VPCVersion
	})
	if got, err := f.p.AdmitNextBaseBackfill(f.ctx, plan.RunID); err != nil || got.State != "paused" {
		t.Fatal("unreviewed plan admitted", got, err)
	}
	if _, err := f.p.SetBaseBackfillPaused(f.ctx, plan.RunID, false, strings.Repeat("0", 64)); biz.ReasonOf(err) != biz.IdempotencyConflict {
		t.Fatal("wrong review digest accepted", err)
	}
	setIntranetPool(t, f, biz.PlatformIntent{Kind: "set_default_intranet_pool", ID: second.ID, IdempotencyKey: "switch-after-plan"})
	replay, err := f.p.PlanBaseBackfill(f.ctx, data.BaseBackfillPlanInput{RunID: plan.RunID, Interval: time.Hour})
	if err != nil || replay.SHA256 != plan.SHA256 || replay.PoolID != f.pool.ID {
		t.Fatal("plan followed a new default", replay, err)
	}
	resumeBackfill(t, f, plan)
	type answer struct {
		admission data.BaseBackfillAdmission
		err       error
	}
	answers := make(chan answer, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); a, e := f.p.AdmitNextBaseBackfill(f.ctx, plan.RunID); answers <- answer{a, e} }()
	}
	wg.Wait()
	close(answers)
	accepted, waiting := 0, 0
	for result := range answers {
		if result.err != nil {
			t.Fatal(result.err)
		}
		switch result.admission.State {
		case "accepted":
			accepted++
		case "waiting":
			waiting++
			if result.admission.WaitMS < 1000 {
				t.Fatal("shared persisted interval missing", result)
			}
		default:
			t.Fatal(result)
		}
	}
	if accepted != 1 || waiting != 1 {
		t.Fatal("concurrent admission rate was bypassed", accepted, waiting)
	}
	if _, err = f.p.SetBaseBackfillPaused(f.ctx, plan.RunID, true, ""); err != nil {
		t.Fatal(err)
	}
	// A separate restricted-role connection simulates loss of dispatcher memory;
	// its first read and admission observe the same paused, already accepted run.
	restarted, err := data.OpenPostgres(f.ctx, db.RuntimeDSN, data.Placement{ClusterID: "test-cluster", NamespacePrefix: "tenant-"})
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	got, err := restarted.GetBaseBackfill(f.ctx, plan.RunID)
	if err != nil || got.SHA256 != plan.SHA256 || !got.Paused {
		t.Fatal("durable plan recovery", got, err)
	}
	if result, err := restarted.AdmitNextBaseBackfill(f.ctx, plan.RunID); err != nil || result.State != "paused" {
		t.Fatal(result, err)
	}
	if _, err = restarted.SetBaseBackfillPaused(f.ctx, plan.RunID, false, plan.SHA256); err != nil {
		t.Fatal(err)
	}
	dueBackfill(t, f, plan.RunID)
	if result, err := restarted.AdmitNextBaseBackfill(f.ctx, plan.RunID); err != nil || result.State != "accepted" {
		t.Fatal(result, err)
	}
	dueBackfill(t, f, plan.RunID)
	if result, err := restarted.AdmitNextBaseBackfill(f.ctx, plan.RunID); err != nil || result.State != "admission_complete" {
		t.Fatal("duplicate admission", result, err)
	}
	var count int
	if err = f.owner.QueryRow(f.ctx, "SELECT count(*) FROM network_vpc_base_connectivity WHERE pool_id=$1 AND pool_revision=$2", plan.PoolID, plan.PoolRevision).Scan(&count); err != nil || count != 2 {
		t.Fatal("fixed pool or repeated allocation", count, err)
	}
	if history != historicalCreateSnapshot(t, f) {
		t.Fatal("backfill rewrote historic successful operations or idempotency responses")
	}
	if err = f.owner.QueryRow(f.ctx, "SELECT count(*) FROM network_vpcs WHERE state<>'available' OR base_connectivity_required").Scan(&count); err != nil || count != 0 {
		t.Fatal("backfill admission changed legacy availability", count, err)
	}
}

func TestBaseBackfillClosedPoolRollsBackAndResumesSameIntent(t *testing.T) {
	f, _ := newBackfillFixture(t)
	availableVPC(t, f.n, f.w, f.tenant, "legacy")
	plan := planBackfill(t, f)
	resumeBackfill(t, f, plan)
	setIntranetPool(t, f, biz.PlatformIntent{Kind: "set_intranet_pool_allocation", ID: f.pool.ID, Enabled: false, IdempotencyKey: "close-before-admission"})
	result, err := f.p.AdmitNextBaseBackfill(f.ctx, plan.RunID)
	if err != nil || result.State != "blocked" || result.Reason != "BASE_CONNECTIVITY_NOT_READY" {
		t.Fatal(result, err)
	}
	got, err := f.p.GetBaseBackfill(f.ctx, plan.RunID)
	if err != nil || !got.Paused || got.Candidates[0].State != "pending" || got.SHA256 != plan.SHA256 {
		t.Fatal("failed plan not recoverable", got, err)
	}
	var resources, operations int
	if err = f.owner.QueryRow(f.ctx, "SELECT (SELECT count(*) FROM network_eips),(SELECT count(*) FROM network_operations WHERE kind='ensure_vpc_base_connectivity')").Scan(&resources, &operations); err != nil || resources != 0 || operations != 0 {
		t.Fatal("savepoint leaked partial acceptance", resources, operations, err)
	}
	setIntranetPool(t, f, biz.PlatformIntent{Kind: "set_intranet_pool_allocation", ID: f.pool.ID, Enabled: true, IdempotencyKey: "reopen-same-pool"})
	resumeBackfill(t, f, plan)
	dueBackfill(t, f, plan.RunID)
	result, err = f.p.AdmitNextBaseBackfill(f.ctx, plan.RunID)
	if err != nil || result.State != "accepted" {
		t.Fatal(result, err)
	}
}

func TestBaseBackfillRejectsChangedProviderIdentityAndRacesDeletion(t *testing.T) {
	t.Run("changed_identity", func(t *testing.T) {
		f, _ := newBackfillFixture(t)
		v := availableVPC(t, f.n, f.w, f.tenant, "legacy")
		plan := planBackfill(t, f)
		resumeBackfill(t, f, plan)
		if _, err := f.owner.Exec(f.ctx, "UPDATE network_provider_bindings SET provider_uid=$3 WHERE tenant_id=$1 AND vpc_id=$2", f.tenant, v.ID, uuid.NewString()); err != nil {
			t.Fatal(err)
		}
		result, err := f.p.AdmitNextBaseBackfill(f.ctx, plan.RunID)
		if err != nil || result.State != "conflict" {
			t.Fatal("changed UID admitted", result, err)
		}
		got, err := f.p.GetBaseBackfill(f.ctx, plan.RunID)
		if err != nil || got.SHA256 != plan.SHA256 || got.Candidates[0].OperationID != "" {
			t.Fatal("snapshot or no-op conflict drift", got, err)
		}
	})
	t.Run("delete_competition", func(t *testing.T) {
		f, _ := newBackfillFixture(t)
		v := availableVPC(t, f.n, f.w, f.tenant, "legacy")
		plan := planBackfill(t, f)
		resumeBackfill(t, f, plan)
		var admission data.BaseBackfillAdmission
		var admitErr, deleteErr error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); admission, admitErr = f.p.AdmitNextBaseBackfill(f.ctx, plan.RunID) }()
		go func() { defer wg.Done(); _, deleteErr = f.n.DeleteVPC(f.ctx, f.tenant, v.ID) }()
		wg.Wait()
		if admitErr != nil || deleteErr != nil {
			t.Fatal("admission/delete serialization", admission, admitErr, deleteErr)
		}
		if admission.State != "accepted" && admission.State != "conflict" {
			t.Fatal(admission)
		}
		f.drive(t, func() bool {
			got, err := f.n.GetVPC(f.ctx, f.tenant, v.ID)
			return err == nil && got.State == biz.Deleted
		})
		var remaining int
		if err := f.owner.QueryRow(f.ctx, "SELECT count(*) FROM network_eips WHERE tenant_id=$1 AND state<>'deleted'", f.tenant).Scan(&remaining); err != nil || remaining != 0 {
			t.Fatal("deletion left base resources", remaining, err)
		}
	})
}

func TestBaseBackfillLegacyActivationRequiresOwnDeclarations(t *testing.T) {
	f, _ := newBackfillFixture(t)
	v := availableVPC(t, f.n, f.w, f.tenant, "legacy")
	history := historicalCreateSnapshot(t, f)
	if _, err := f.p.ActivateLegacyBaseAggregation(f.ctx); err == nil {
		t.Fatal("aggregation enabled before new path")
	}
	if _, err := f.p.SetNewVPCBaseConnectivity(f.ctx, true); err != nil {
		t.Fatal(err)
	}
	if _, err := f.p.ActivateLegacyBaseAggregation(f.ctx); biz.ReasonOf(err) != biz.BaseConnectivityNotReady {
		t.Fatal("missing legacy base accepted", err)
	}
	plan := planBackfill(t, f)
	resumeBackfill(t, f, plan)
	result, err := f.p.AdmitNextBaseBackfill(f.ctx, plan.RunID)
	if err != nil || result.State != "accepted" {
		t.Fatal(result, err)
	}
	f.drive(t, func() bool {
		var ready bool
		err := f.owner.QueryRow(f.ctx, "SELECT b.state='ready' AND o.state='succeeded' FROM network_vpc_base_connectivity b JOIN network_operations o ON o.tenant_id=b.tenant_id AND o.operation_id=b.operation_id WHERE b.tenant_id=$1 AND b.vpc_id=$2", f.tenant, v.ID).Scan(&ready)
		return err == nil && ready
	})
	if history != historicalCreateSnapshot(t, f) {
		t.Fatal("ensure completion changed historic create")
	}
	// Old observation timestamps do not veto an accepted management declaration.
	if _, err = f.owner.Exec(f.ctx, "UPDATE network_vpc_base_connectivity SET observed_at=clock_timestamp()-interval '2 minutes' WHERE tenant_id=$1 AND vpc_id=$2", f.tenant, v.ID); err != nil {
		t.Fatal(err)
	}
	rollout, err := f.p.ActivateLegacyBaseAggregation(f.ctx)
	if err != nil || !rollout.LegacyAggregationEnabled {
		t.Fatal(rollout, err)
	}
	if _, err = f.p.SetNewVPCBaseConnectivity(f.ctx, false); biz.ReasonOf(err) != biz.ResourceBusy {
		t.Fatal("legacy admission reopened after aggregation", err)
	}
}
