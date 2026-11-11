package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	acc "github.com/zhangzhe-ctrl/ani-accelerator-service/api/gen/go/accelerator/v1"
	gp "github.com/zhangzhe-ctrl/ani-accelerator-service/pkg/gpuprotocol"
	compute "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/compute"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/instance"
	computedata "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/compute"
	instancedata "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/instance"
	instancemigrations "github.com/zhangzhe-ctrl/ani-resource-service/migrations/instance"
	"google.golang.org/protobuf/encoding/protojson"
)

func testDatabase(t *testing.T) (*pgx.Conn, string) {
	t.Helper()
	adminDSN := os.Getenv("NETWORK_TEST_ADMIN_DSN")
	if adminDSN == "" {
		t.Skip("requires an explicitly owned local PostgreSQL test container")
	}
	cfg, err := pgx.ParseConfig(adminDSN)
	if err != nil || (cfg.Host != "127.0.0.1" && cfg.Host != "localhost") {
		t.Fatal("this test only accepts its local task container")
	}
	ctx := context.Background()
	admin, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	key := strings.ReplaceAll(uuid.NewString(), "-", "")
	dbName, role := "acc_v11_res_"+key, "acc_v11_runtime_"+key
	password := uuid.NewString()
	if _, err = admin.Exec(ctx, "CREATE ROLE "+pgx.Identifier{role}.Sanitize()+" LOGIN PASSWORD '"+password+"'"); err != nil {
		t.Fatal(err)
	}
	if _, err = admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{dbName}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE "+pgx.Identifier{dbName}.Sanitize()+" WITH (FORCE)")
		_, _ = admin.Exec(context.Background(), "DROP ROLE "+pgx.Identifier{role}.Sanitize())
		_ = admin.Close(context.Background())
	})
	address, err := url.Parse(adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	address.Path = "/" + dbName
	ownerDSN := address.String()
	if err = instancedata.Migrate(ctx, ownerDSN, role); err != nil {
		t.Fatal(err)
	}
	if err = computedata.Migrate(ctx, ownerDSN, role); err != nil {
		t.Fatal(err)
	}
	owner, err := pgx.Connect(ctx, ownerDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close(context.Background()) })
	address.User = url.UserPassword(role, password)
	return owner, address.String()
}

func TestPGStaticReferenceUpgradeAndCPUOriginalReplay(t *testing.T) {
	db, runtimeDSN := testDatabase(t)
	ctx := context.Background()
	// Recreate the actually shipped version-1 constraint from immutable SQL.
	history, err := instancemigrations.Files.ReadFile("004_gpu_execution.sql")
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(history), "ALTER TABLE instance.compute_presets ADD CONSTRAINT")
	end := strings.Index(string(history)[start:], " NOT VALID;") + start + len(" NOT VALID;")
	oldConstraint := strings.ReplaceAll(strings.ReplaceAll(string(history)[start:end], "instance.compute_presets", "compute.presets"), "compute_presets_check", "presets_check")
	if _, err = db.Exec(ctx, "ALTER TABLE compute.presets DROP CONSTRAINT presets_check; DELETE FROM compute.schema_version WHERE version=2; "+oldConstraint); err != nil {
		t.Fatal(err)
	}
	presetID, profileID, clusterID, poolID := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	legacy := map[string]any{"profile_id": profileID, "profile_version": 1, "spec_digest": strings.Repeat("a", 64), "devices_per_instance": 1, "cluster_id": clusterID, "pool_id": poolID, "mode": "SHARED_FIXED", "model_key": "t4", "memory_mib_per_device": 8192, "core_limit_percent": 25}
	oldJSON, _ := json.Marshal(legacy)
	if _, err = db.Exec(ctx, `INSERT INTO compute.presets(id,code,name,compute_type,cpu_millis,memory_mib,enabled,gpu,created_by,updated_by) VALUES($1,'original-gpu','Original GPU','gpu',1000,1024,true,$2,'fixture','fixture')`, presetID, oldJSON); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	input, err := inspect(ctx, tx)
	if err != nil {
		t.Fatal(err)
	}
	profile := &acc.GpuProfile{ProfileId: profileID, ProfileVersion: 1, BaselineId: uuid.NewString(), Spec: &acc.ProfileSpec{GroupId: uuid.NewString(), Mode: acc.SupplyMode_SHARED_FIXED, ModelKey: "t4", CoreLimitPercent: 25, MaxDevicesPerReplica: 1, IsolationClass: "SOFTWARE_COOPERATIVE", MemoryMibPerDevice: 8192, AllocationType: acc.AllocationType_DYNAMIC_SHARED, ResourceKey: "volcano.sh/vgpu-number"}}
	profile.SpecDigest, err = gp.ProfileSpecDigest(profile.Spec, profile.BaselineId)
	if err != nil {
		t.Fatal(err)
	}
	input.Presets[0].Profile, err = protojson.Marshal(profile)
	if err != nil {
		t.Fatal(err)
	}
	input.Presets[0].GPU = &compute.GPUReference{ProfileID: profileID, ProfileVersion: 1, SpecDigest: profile.SpecDigest, DevicesPerInstance: 1, ClusterID: clusterID, PoolID: poolID, Mode: "SHARED_FIXED", ModelKey: "t4", MemoryMiBPerDevice: 8192, CoreLimitPercent: 25, AllocationType: "DYNAMIC_SHARED", ResourceKey: "volcano.sh/vgpu-number", AdapterID: "volcano-hami-mib-v1"}
	if err = apply(ctx, tx, input); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var ownerRole string
	if err = db.QueryRow(ctx, "SELECT current_user").Scan(&ownerRole); err != nil {
		t.Fatal(err)
	}
	runtimeConfig, err := pgx.ParseConfig(runtimeDSN)
	if err != nil {
		t.Fatal(err)
	}
	// Reuse the owner connection's URL; its configured connection string was
	// built for the isolated database in testDatabase.
	if err = computedata.Migrate(ctx, db.Config().ConnString(), runtimeConfig.User); err != nil {
		t.Fatal("static reference upgrade failed", err)
	}
	presets, err := computedata.OpenPostgres(ctx, runtimeDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer presets.Close()
	platform := compute.WithScope(ctx, compute.Scope{Platform: true, Actor: "governance:user:42", RequestID: uuid.NewString(), Workload: "ani-governance", Method: "/compute.v1.ComputePresetService/AdminGetPreset"})
	converted, err := presets.GetPreset(platform, presetID)
	if err != nil || converted.ID != presetID || !converted.Enabled || converted.GPU.SpecDigest != profile.SpecDigest {
		t.Fatal("deterministic static conversion lost identity or state", converted, err)
	}
	tenant, id, operation := uuid.NewString(), uuid.NewString(), uuid.NewString()
	spec := biz.Spec{WorkspaceMountPath: "/workspace", InstanceKind: biz.KindContainer, ComputeType: biz.TypeCPU, Name: "original-cpu", ImageID: "img_" + strings.Repeat("a", 32), ImageScope: "platform", ImageReference: "registry.invalid/work@sha256:" + strings.Repeat("c", 64), ImageDigest: "sha256:" + strings.Repeat("c", 64), PresetID: uuid.NewString(), Scheduling: biz.Scheduling{PodPlacement: "NONE"}, CPU: "1000m", Memory: "1024Mi", UID: 1000, GID: 1000, Shell: "/bin/sh", ServiceAccount: "workload", StorageClass: "ani-block", WorkspaceSize: "1Gi", ClusterID: clusterID, Namespace: "tenant-cpu", NotebookName: "instance-" + id, PVCName: "workspace-" + id}
	command := biz.Create{TenantID: tenant, ID: id, OperationID: operation, RequestHash: strings.Repeat("d", 64), BusinessDigest: strings.Repeat("e", 64), Spec: spec}
	repo, err := instancedata.OpenPostgres(ctx, runtimeDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	repo.UseIntentGate(true)
	first, err := repo.AcceptCreate(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	repo.UseIntentGate(false)
	second, err := repo.AcceptCreate(ctx, command)
	if err != nil || first.OperationID != second.OperationID || first.ResourceID != second.ResourceID {
		t.Fatal("original CPU retry changed after GPU cutover", first, second, err)
	}
	var count int
	if err = db.QueryRow(ctx, "SELECT count(*) FROM instance.instance_operations WHERE tenant_id=$1 AND instance_id=$2", tenant, id).Scan(&count); err != nil || count != 1 {
		t.Fatal("CPU retry duplicated operation", count, err)
	}
}

func TestPGArchivePreservesOriginalFactsAndHistoryScope(t *testing.T) {
	db, runtimeDSN := testDatabase(t)
	ctx := context.Background()
	tenant, id, create, event := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	snapshot, err := json.Marshal(map[string]any{
		"TenantID": tenant, "ID": id, "CreateOperationID": create, "CreateRequestHash": strings.Repeat("a", 64), "BusinessDigest": strings.Repeat("b", 64),
		"Desired": "deleted", "ComputeCleaned": true, "WorkspaceCleaned": true, "VolumeReferencesReleased": true, "QuotaReturned": true,
		"Spec":            map[string]any{"instance_kind": "container", "compute_type": "gpu", "ClusterID": id, "Namespace": "tenant-history", "GPUSummary": map[string]any{"legacy": "unchanged"}},
		"RetiredGPUField": map[string]any{"unknown": "must never enter a business decoder"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(ctx, `INSERT INTO instance.instances(tenant_id,instance_id,create_operation_id,create_request_hash,version,desired,snapshot,runtime_desired,mounts_initialized) VALUES($1,$2,$3,$4,1,'deleted',$5,'deleted',true)`, tenant, id, create, strings.Repeat("a", 64), snapshot); err != nil {
		t.Fatal(err)
	}
	ack, _ := json.Marshal(biz.ACK{OperationID: create, ResourceID: id, Accepted: true})
	if _, err = db.Exec(ctx, `INSERT INTO instance.instance_operations(tenant_id,operation_id,instance_id,action,idempotency_key,request_hash,version,ack,state) VALUES($1,$2,$3,'create',$2::uuid::text,$4,1,$5,'completed')`, tenant, create, id, strings.Repeat("a", 64), ack); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(ctx, `INSERT INTO instance.instance_quota_release_outbox(tenant_id,instance_id,event_id,original_create_operation_id,acknowledged,cleanup_completed_at,cleanup_version) VALUES($1,$2,$3,$4,false,clock_timestamp(),1)`, tenant, id, event, create); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(ctx, "INSERT INTO instance.instance_work(tenant_id,instance_id) VALUES($1,$2)", tenant, id); err != nil {
		t.Fatal(err)
	}
	repo, err := instancedata.OpenPostgres(ctx, runtimeDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	repo.UseIntentGate(true)
	cpu := newCPUCommand(tenant, uuid.NewString(), uuid.NewString(), id)
	if _, err = repo.AcceptCreate(ctx, cpu); err != nil {
		t.Fatal(err)
	}
	var originalCPU json.RawMessage
	if err = db.QueryRow(ctx, "SELECT snapshot FROM instance.instances WHERE tenant_id=$1 AND instance_id=$2", tenant, cpu.ID).Scan(&originalCPU); err != nil {
		t.Fatal(err)
	}
	var cpuEnvelope struct{ Spec struct{ GPU json.RawMessage } }
	if err = json.Unmarshal(originalCPU, &cpuEnvelope); err != nil {
		t.Fatal(err)
	}
	var emptyGPU biz.GPU
	if len(cpuEnvelope.Spec.GPU) == 0 || bytes.Equal(bytes.TrimSpace(cpuEnvelope.Spec.GPU), []byte("null")) || json.Unmarshal(cpuEnvelope.Spec.GPU, &emptyGPU) != nil || !emptyGPU.Empty() {
		t.Fatal("CPU fixture must contain its nonnull empty GPU value object")
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	input, err := inspect(ctx, tx)
	if err != nil {
		t.Fatal(err)
	}
	if len(input.Instances) != 1 || input.Instances[0].InstanceID != id {
		t.Fatal("nonnull CPU GPU value object was classified as a GPU")
	}
	if err = apply(ctx, tx, input); err == nil {
		t.Fatal("archived an unacknowledged original refund")
	}
	_ = tx.Rollback(ctx)
	var archived bool
	if err = db.QueryRow(ctx, "SELECT archived_at IS NOT NULL FROM instance.instances WHERE tenant_id=$1 AND instance_id=$2", tenant, id).Scan(&archived); err != nil || archived {
		t.Fatal("failed cutover changed row", err)
	}
	if _, err = db.Exec(ctx, "UPDATE instance.instance_quota_release_outbox SET acknowledged=true WHERE tenant_id=$1 AND instance_id=$2", tenant, id); err != nil {
		t.Fatal(err)
	}
	tx, err = db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	input, err = inspect(ctx, tx)
	if err != nil {
		t.Fatal(err)
	}
	if err = apply(ctx, tx, input); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var afterSnapshot, afterAck, afterOutbox json.RawMessage
	if err = db.QueryRow(ctx, "SELECT snapshot FROM instance.instances WHERE tenant_id=$1 AND instance_id=$2", tenant, id).Scan(&afterSnapshot); err != nil {
		t.Fatal(err)
	}
	if !sameJSON(afterSnapshot, snapshot) {
		t.Fatal("archive rewrote accepted GPU snapshot")
	}
	if err = db.QueryRow(ctx, "SELECT ack FROM instance.instance_operations WHERE tenant_id=$1 AND operation_id=$2", tenant, create).Scan(&afterAck); err != nil || !sameJSON(afterAck, ack) {
		t.Fatal("original ACK changed", err)
	}
	if err = db.QueryRow(ctx, "SELECT jsonb_agg(to_jsonb(o) ORDER BY event_id) FROM instance.instance_quota_release_outbox o WHERE tenant_id=$1 AND instance_id=$2", tenant, id).Scan(&afterOutbox); err != nil || !sameJSON(afterOutbox, input.Instances[0].Outbox) {
		t.Fatal("release facts changed", err)
	}
	caller := biz.WithCaller(ctx, biz.Caller{TenantID: tenant, Actor: "governance:user:42"})
	if _, err = repo.Get(caller, tenant, id); !errors.Is(err, biz.ErrNotFound) {
		t.Fatal("archived GPU reached active decoder", err)
	}
	if _, count, err := repo.List(caller, tenant, biz.ListFilter{ObservedState: biz.Deleted}); err != nil || count != 0 {
		t.Fatal("archive reached active list", count, err)
	}
	scope, err := repo.ReadMetricScope(caller, tenant, id)
	if err != nil || scope.CreateOperationID != create || scope.Namespace != "tenant-history" {
		t.Fatal("archived metric scope was lost", scope, err)
	}
	if operation, err := repo.GetOperation(caller, tenant, create); err != nil || operation.RequestHash != strings.Repeat("a", 64) || operation.ACK.OperationID != create {
		t.Fatal("original operation history lost", operation, err)
	}
	var afterCPU json.RawMessage
	if err = db.QueryRow(ctx, "SELECT snapshot FROM instance.instances WHERE tenant_id=$1 AND instance_id=$2 AND archived_at IS NULL", tenant, cpu.ID).Scan(&afterCPU); err != nil || !sameJSON(originalCPU, afterCPU) {
		t.Fatal("active CPU row or original hash changed", err)
	}
	if work, err := repo.Claim(ctx, "archive-test", time.Second); err != nil || work == nil || work.Instance.ID != cpu.ID {
		t.Fatal("CPU work was removed or archived GPU work was scheduled", work, err)
	}
}

func newCPUCommand(tenant, id, operation, cluster string) biz.Create {
	spec := biz.Spec{WorkspaceMountPath: "/workspace", InstanceKind: biz.KindContainer, ComputeType: biz.TypeCPU, Name: "original-cpu", ImageID: "img_" + strings.Repeat("a", 32), ImageScope: "platform", ImageReference: "registry.invalid/work@sha256:" + strings.Repeat("c", 64), ImageDigest: "sha256:" + strings.Repeat("c", 64), PresetID: uuid.NewString(), Scheduling: biz.Scheduling{PodPlacement: "NONE"}, CPU: "1000m", Memory: "1024Mi", UID: 1000, GID: 1000, Shell: "/bin/sh", ServiceAccount: "workload", StorageClass: "ani-block", WorkspaceSize: "1Gi", ClusterID: cluster, Namespace: "tenant-cpu", NotebookName: "instance-" + id, PVCName: "workspace-" + id}
	return biz.Create{TenantID: tenant, ID: id, OperationID: operation, RequestHash: strings.Repeat("d", 64), BusinessDigest: strings.Repeat("e", 64), Spec: spec}
}

func TestPGHygonPresetReferenceRoundTripAndSQL(t *testing.T) {
	_, runtimeDSN := testDatabase(t)
	ctx := context.Background()
	repo, err := computedata.OpenPostgres(ctx, runtimeDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	platform := compute.WithScope(ctx, compute.Scope{Platform: true, Actor: "governance:user:42", RequestID: uuid.NewString(), Workload: "ani-governance", Method: "/compute.v1.ComputePresetService/AdminCreatePreset"})
	id := uuid.NewString()
	for _, fixed := range []bool{false, true} {
		gpu := &compute.GPUReference{ProfileID: id, ProfileVersion: 1, SpecDigest: strings.Repeat("a", 64), DevicesPerInstance: 2, ClusterID: id, PoolID: id, Mode: "WHOLE_EXCLUSIVE", ModelKey: "hcu", AllocationType: "PHYSICAL", ResourceKey: "hygon.com/hcu", AdapterID: "hygon-hcu-v1"}
		code := "hcu-whole"
		if fixed {
			code = "hcu-fixed"
			gpu.Mode = "SHARED_FIXED"
			gpu.AllocationType = "FIXED_PARTITION"
			gpu.DevicesPerInstance = 1
			gpu.MemoryMiBPerDevice = 16385
			gpu.ComputeUnitsPerDevice = 4
			gpu.ResourceKey = "hygon.com/hcu-share-4c-16g"
		}
		preset, err := repo.CreatePreset(platform, compute.Preset{Code: code, Name: code, ComputeType: "gpu", CPUMillis: 1000, MemoryMiB: 1024, GPU: gpu})
		if err != nil {
			t.Fatal("SQL rejected Hygon static contract", err)
		}
		restored, err := repo.GetPreset(platform, preset.ID)
		if err != nil || *restored.GPU != *gpu {
			t.Fatal("new reference lost fields", restored.GPU, err)
		}
		gpu.CoreLimitPercent = 25
		if _, err = repo.UpdatePreset(platform, preset.ID, compute.Preset{GPU: gpu}, []string{"gpu"}); !errors.Is(err, compute.ErrInvalid) {
			t.Fatal("Hygon accepted NVIDIA core percentage", err)
		}
	}
}
