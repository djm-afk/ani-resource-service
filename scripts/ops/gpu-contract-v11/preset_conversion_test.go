package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	acc "github.com/zhangzhe-ctrl/ani-accelerator-service/api/gen/go/accelerator/v1"
	gp "github.com/zhangzhe-ctrl/ani-accelerator-service/pkg/gpuprotocol"
	compute "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/compute"
	instancemigrations "github.com/zhangzhe-ctrl/ani-resource-service/migrations/instance"
	"google.golang.org/protobuf/encoding/protojson"
)

func presetConversionFixture(t *testing.T) presetTarget {
	t.Helper()
	id, cluster, pool := uuid.NewString(), uuid.NewString(), uuid.NewString()
	profile := &acc.GpuProfile{ProfileId: id, ProfileVersion: 1, BaselineId: uuid.NewString(), Spec: &acc.ProfileSpec{
		GroupId: uuid.NewString(), Mode: acc.SupplyMode_WHOLE_EXCLUSIVE, ModelKey: "t4", CoreLimitPercent: 100,
		MaxDevicesPerReplica: 2, IsolationClass: "WHOLE_DEVICE_EXCLUSIVE", MemoryMibPerDevice: 16384,
		AllocationType: acc.AllocationType_PHYSICAL, ResourceKey: gp.NvidiaNumberResource,
	}}
	var err error
	profile.SpecDigest, err = gp.ProfileSpecDigest(profile.Spec, profile.BaselineId)
	if err != nil {
		t.Fatal(err)
	}
	profileJSON, err := protojson.Marshal(profile)
	if err != nil {
		t.Fatal(err)
	}
	legacy := map[string]any{"profile_id": id, "profile_version": 1, "spec_digest": strings.Repeat("a", 64),
		"devices_per_instance": 1, "cluster_id": cluster, "pool_id": pool, "mode": "WHOLE_EXCLUSIVE",
		"model_key": "t4", "memory_mib_per_device": 16384, "core_limit_percent": 100}
	row, err := json.Marshal(map[string]any{"id": id, "gpu": legacy})
	if err != nil {
		t.Fatal(err)
	}
	return presetTarget{ID: id, Row: row, Profile: profileJSON, GPU: &compute.GPUReference{
		ProfileID: id, ProfileVersion: 1, SpecDigest: profile.SpecDigest, DevicesPerInstance: 1,
		ClusterID: cluster, PoolID: pool, Mode: "WHOLE_EXCLUSIVE", ModelKey: "t4", MemoryMiBPerDevice: 16384,
		CoreLimitPercent: 100, AllocationType: "PHYSICAL", ResourceKey: gp.NvidiaNumberResource, AdapterID: gp.NvidiaAdapterID,
	}}
}

func TestPresetConversionPreservesOriginalBusinessValues(t *testing.T) {
	control := presetConversionFixture(t)
	if err := validatePresetSnapshot(control); err != nil {
		t.Fatal("contract additions and a new digest should preserve a one-device preset", err)
	}
	for name, mutate := range map[string]func(*compute.GPUReference){
		"profile": func(g *compute.GPUReference) { g.ProfileID = uuid.NewString() },
		"version": func(g *compute.GPUReference) { g.ProfileVersion++ },
		"devices": func(g *compute.GPUReference) { g.DevicesPerInstance = 2 },
		"cluster": func(g *compute.GPUReference) { g.ClusterID = uuid.NewString() },
		"pool":    func(g *compute.GPUReference) { g.PoolID = uuid.NewString() },
		"mode":    func(g *compute.GPUReference) { g.Mode = "SHARED_FIXED" },
		"model":   func(g *compute.GPUReference) { g.ModelKey = "a100" },
		"memory":  func(g *compute.GPUReference) { g.MemoryMiBPerDevice *= 2 },
		"core":    func(g *compute.GPUReference) { g.CoreLimitPercent = 50 },
	} {
		t.Run(name, func(t *testing.T) {
			item := presetConversionFixture(t)
			mutate(item.GPU)
			if err := validatePresetSnapshot(item); err == nil || !strings.Contains(err.Error(), "changes original GPU") {
				t.Fatal("changed original business value was not rejected before Profile comparison", err)
			}
		})
	}
	for _, row := range []json.RawMessage{nil, json.RawMessage(`{}`), json.RawMessage(`{"gpu":null}`)} {
		item := presetConversionFixture(t)
		item.Row = row
		if err := validatePresetSnapshot(item); err == nil {
			t.Fatal("missing original GPU reference accepted")
		}
	}
}

func TestPGPresetConversionRejectsDeviceDriftWithoutChangingOriginal(t *testing.T) {
	db, _ := testDatabase(t)
	ctx := context.Background()
	// Restore the shipped old constraint so the input is a real legacy row.
	history, err := instancemigrations.Files.ReadFile("004_gpu_execution.sql")
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(history), "ALTER TABLE instance.compute_presets ADD CONSTRAINT")
	end := strings.Index(string(history)[start:], " NOT VALID;") + start + len(" NOT VALID;")
	constraint := strings.ReplaceAll(strings.ReplaceAll(string(history)[start:end], "instance.compute_presets", "compute.presets"), "compute_presets_check", "presets_check")
	if _, err = db.Exec(ctx, "ALTER TABLE compute.presets DROP CONSTRAINT presets_check; "+constraint); err != nil {
		t.Fatal(err)
	}
	item := presetConversionFixture(t)
	var row struct {
		GPU json.RawMessage `json:"gpu"`
	}
	if err = json.Unmarshal(item.Row, &row); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(ctx, `INSERT INTO compute.presets(id,code,name,compute_type,cpu_millis,memory_mib,enabled,gpu,created_by,updated_by) VALUES($1,'one-device','One device','gpu',1000,1024,true,$2,'fixture','fixture')`, item.ID, row.GPU); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	input, err := inspect(ctx, tx)
	if err != nil {
		t.Fatal(err)
	}
	input.Presets[0].Profile, input.Presets[0].GPU = item.Profile, item.GPU
	input.Presets[0].GPU.DevicesPerInstance = 2
	if err = apply(ctx, tx, input); err == nil || !strings.Contains(err.Error(), "changes original GPU") {
		t.Fatal("same Profile max=2 allowed one-device preset to become two devices", err)
	}
	var after json.RawMessage
	if err = tx.QueryRow(ctx, "SELECT to_jsonb(p) FROM compute.presets p WHERE id=$1", item.ID).Scan(&after); err != nil || !sameJSON(after, input.Presets[0].Row) {
		t.Fatal("rejected conversion changed the original row", err)
	}
	input.Presets[0].GPU.DevicesPerInstance = 1
	if err = apply(ctx, tx, input); err != nil {
		t.Fatal("one-device conversion rejected", err)
	}
	var gpu compute.GPUReference
	if err = tx.QueryRow(ctx, "SELECT gpu FROM compute.presets WHERE id=$1", item.ID).Scan(&gpu); err != nil || gpu != *input.Presets[0].GPU {
		t.Fatal("valid conversion did not persist the exact new reference", gpu, err)
	}
}
