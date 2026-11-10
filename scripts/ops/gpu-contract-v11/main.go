// gpu-contract-v11 is an offline, manifest-driven cutover tool. It is never
// registered with the service and does not delete owner resources or refund.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	acc "github.com/zhangzhe-ctrl/ani-accelerator-service/api/gen/go/accelerator/v1"
	gp "github.com/zhangzhe-ctrl/ani-accelerator-service/pkg/gpuprotocol"
	compute "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/compute"
	"google.golang.org/protobuf/encoding/protojson"
)

type instanceTarget struct {
	TenantID   string          `json:"tenant_id"`
	InstanceID string          `json:"instance_id"`
	Row        json.RawMessage `json:"original_row"`
	Operations json.RawMessage `json:"original_operations"`
	Outbox     json.RawMessage `json:"original_release_outbox"`
}
type presetTarget struct {
	ID      string                `json:"id"`
	Row     json.RawMessage       `json:"original_row"`
	GPU     *compute.GPUReference `json:"new_gpu_reference"`
	Profile json.RawMessage       `json:"new_profile"`
	Remove  bool                  `json:"remove_unconvertible"`
}

func validatePresetSnapshot(item presetTarget) error {
	profile := new(acc.GpuProfile)
	if err := protojson.Unmarshal(item.Profile, profile); err != nil {
		return fmt.Errorf("new Profile JSON: %w", err)
	}
	g := item.GPU
	s := profile.Spec
	if s == nil || !compute.UUID(profile.BaselineId) || profile.ProfileId != g.ProfileID || profile.ProfileVersion != g.ProfileVersion {
		return fmt.Errorf("new Profile identity differs from managed reference")
	}
	fields, err := gp.ProfileSpecFromProto(s)
	if err != nil {
		return err
	}
	if err = gp.ValidateProfileSpecFields(fields, g.AdapterID); err != nil {
		return err
	}
	digest, err := gp.ProfileSpecDigest(s, profile.BaselineId)
	if err != nil {
		return err
	}
	if digest != profile.SpecDigest || digest != g.SpecDigest ||
		s.Mode.String() != g.Mode || s.AllocationType.String() != g.AllocationType ||
		s.ModelKey != g.ModelKey || s.ResourceKey != g.ResourceKey || s.MemoryMibPerDevice != g.MemoryMiBPerDevice ||
		s.CoreLimitPercent != g.CoreLimitPercent || s.ComputeUnitsPerDevice != g.ComputeUnitsPerDevice ||
		g.DevicesPerInstance > s.MaxDevicesPerReplica {
		return fmt.Errorf("new Profile specification or digest differs from managed reference")
	}
	return nil
}

type manifest struct {
	Version   int              `json:"schema_version"`
	Instances []instanceTarget `json:"instances"`
	Presets   []presetTarget   `json:"presets"`
}

const oldGPU = `snapshot #>> '{Spec,compute_type}'='gpu'
 AND (snapshot #>> '{Spec,GPUSummary,AllocationType}' IS NULL
      OR snapshot #>> '{Spec,GPUSummary,AdapterID}' IS NULL) AND archived_at IS NULL`

func privateFile(name string) ([]byte, error) {
	info, err := os.Stat(name)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("a regular private file is required")
	}
	return os.ReadFile(name)
}

func inspect(ctx context.Context, tx pgx.Tx) (manifest, error) {
	out := manifest{Version: 1, Instances: []instanceTarget{}, Presets: []presetTarget{}}
	rows, err := tx.Query(ctx, "SELECT tenant_id::text,instance_id::text,to_jsonb(i) FROM instance.instances i WHERE "+oldGPU+" ORDER BY tenant_id,instance_id")
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var item instanceTarget
		if err = rows.Scan(&item.TenantID, &item.InstanceID, &item.Row); err != nil {
			rows.Close()
			return out, err
		}
		out.Instances = append(out.Instances, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	for n := range out.Instances {
		item := &out.Instances[n]
		if err = tx.QueryRow(ctx, `SELECT coalesce(jsonb_agg(to_jsonb(o) ORDER BY operation_id),'[]'::jsonb)
 FROM instance.instance_operations o WHERE tenant_id=$1 AND instance_id=$2`, item.TenantID, item.InstanceID).Scan(&item.Operations); err != nil {
			return out, err
		}
		if err = tx.QueryRow(ctx, `SELECT coalesce(jsonb_agg(to_jsonb(o) ORDER BY event_id),'[]'::jsonb)
 FROM instance.instance_quota_release_outbox o WHERE tenant_id=$1 AND instance_id=$2`, item.TenantID, item.InstanceID).Scan(&item.Outbox); err != nil {
			return out, err
		}
	}
	rows, err = tx.Query(ctx, "SELECT id::text,to_jsonb(p) FROM compute.presets p WHERE compute_type='gpu' ORDER BY id")
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var item presetTarget
		if err = rows.Scan(&item.ID, &item.Row); err != nil {
			return out, err
		}
		out.Presets = append(out.Presets, item)
	}
	return out, rows.Err()
}

func sameJSON(a, b json.RawMessage) bool {
	var x, y any
	da, db := json.NewDecoder(bytes.NewReader(a)), json.NewDecoder(bytes.NewReader(b))
	da.UseNumber()
	db.UseNumber()
	if da.Decode(&x) != nil || db.Decode(&y) != nil {
		return false
	}
	aa, _ := json.Marshal(x)
	bb, _ := json.Marshal(y)
	return bytes.Equal(aa, bb)
}

func apply(ctx context.Context, tx pgx.Tx, input manifest) error {
	if input.Version != 1 || input.Instances == nil || input.Presets == nil {
		return fmt.Errorf("invalid cutover manifest")
	}
	// This lock exists only in the one-time maintenance transaction. There is
	// no runtime admission barrier or background migration service.
	if _, err := tx.Exec(ctx, `LOCK TABLE instance.instances,instance.instance_operations,
 instance.instance_quota_release_outbox,instance.instance_work,
 instance.borrowed_volume_references,instance.network_submissions,compute.presets
 IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return err
	}
	actual, err := inspect(ctx, tx)
	if err != nil {
		return err
	}
	if len(actual.Instances) != len(input.Instances) || len(actual.Presets) != len(input.Presets) {
		return fmt.Errorf("manifest object set changed")
	}
	for n, item := range input.Instances {
		seen := actual.Instances[n]
		if item.TenantID != seen.TenantID || item.InstanceID != seen.InstanceID ||
			!sameJSON(item.Row, seen.Row) || !sameJSON(item.Operations, seen.Operations) || !sameJSON(item.Outbox, seen.Outbox) {
			return fmt.Errorf("instance %s original row, ACK or outbox changed", item.InstanceID)
		}
		var closed bool
		err = tx.QueryRow(ctx, `SELECT desired='deleted' AND runtime_desired='deleted'
 AND snapshot->>'ComputeCleaned'='true' AND snapshot->>'WorkspaceCleaned'='true'
 AND snapshot->>'VolumeReferencesReleased'='true' AND snapshot->>'QuotaReturned'='true'
 AND coalesce(snapshot->'Pending','null'::jsonb)='null'::jsonb
 AND recovery_fence IS NULL
 AND (coalesce(snapshot->'Network','null'::jsonb)='null'::jsonb OR snapshot #>> '{Network,Cleaned}'='true')
 AND NOT EXISTS(SELECT 1 FROM instance.instance_operations o WHERE o.tenant_id=i.tenant_id AND o.instance_id=i.instance_id AND o.state IN ('accepted','blocked'))
 AND NOT EXISTS(SELECT 1 FROM instance.instance_quota_release_outbox o WHERE o.tenant_id=i.tenant_id AND o.instance_id=i.instance_id AND NOT o.acknowledged)
 AND NOT EXISTS(SELECT 1 FROM instance.borrowed_volume_references r WHERE r.tenant_id=i.tenant_id AND r.instance_id=i.instance_id AND r.state<>'released')
 AND NOT EXISTS(SELECT 1 FROM instance.network_submissions s WHERE s.tenant_id=i.tenant_id AND s.instance_id=i.instance_id AND s.state<>'closed')
 AND NOT EXISTS(SELECT 1 FROM instance.instance_work w WHERE w.tenant_id=i.tenant_id AND w.instance_id=i.instance_id AND w.lease_until>clock_timestamp())
 FROM instance.instances i WHERE tenant_id=$1 AND instance_id=$2`, item.TenantID, item.InstanceID).Scan(&closed)
		if err != nil {
			return fmt.Errorf("instance %s cleanup inspection: %w", item.InstanceID, err)
		}
		if !closed {
			return fmt.Errorf("instance %s must first complete original owner cleanup and refund", item.InstanceID)
		}
		if _, err = tx.Exec(ctx, "UPDATE instance.instances SET archived_at=clock_timestamp() WHERE tenant_id=$1 AND instance_id=$2 AND archived_at IS NULL", item.TenantID, item.InstanceID); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, "DELETE FROM instance.instance_work WHERE tenant_id=$1 AND instance_id=$2", item.TenantID, item.InstanceID); err != nil {
			return err
		}
	}
	for n, item := range input.Presets {
		seen := actual.Presets[n]
		if item.ID != seen.ID || !sameJSON(item.Row, seen.Row) || item.Remove == (item.GPU != nil) {
			return fmt.Errorf("preset %s requires an unchanged original row and exactly one explicit conversion/removal", item.ID)
		}
		if item.Remove {
			_, err = tx.Exec(ctx, "DELETE FROM compute.presets WHERE id=$1", item.ID)
		} else {
			if err = compute.ValidateGPUReference(item.GPU); err != nil {
				return fmt.Errorf("preset %s invalid static conversion: %w", item.ID, err)
			}
			if err = validatePresetSnapshot(item); err != nil {
				return fmt.Errorf("preset %s: %w", item.ID, err)
			}
			var raw []byte
			raw, err = json.Marshal(item.GPU)
			if err == nil {
				_, err = tx.Exec(ctx, "UPDATE compute.presets SET gpu=$2 WHERE id=$1", item.ID, raw)
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func run() error {
	dsnFile := flag.String("dsn-file", "", "private owner DSN file")
	path := flag.String("manifest", "", "private manifest file")
	mode := flag.String("mode", "inspect", "inspect or apply")
	backup := flag.String("backup-file", "", "existing private pre-cutover backup for apply")
	flag.Parse()
	dsn, err := privateFile(*dsnFile)
	if err != nil {
		return err
	}
	defer clear(dsn)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	conn, err := pgx.Connect(ctx, string(bytes.TrimSpace(dsn)))
	if err != nil {
		return fmt.Errorf("cutover database connection failed")
	}
	defer conn.Close(ctx)
	options := pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}
	if *mode == "apply" {
		options = pgx.TxOptions{IsoLevel: pgx.Serializable}
	} else if *mode != "inspect" {
		return fmt.Errorf("mode must be inspect or apply")
	}
	tx, err := conn.BeginTx(ctx, options)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if *mode == "inspect" {
		output, e := inspect(ctx, tx)
		if e != nil {
			return e
		}
		data, e := json.MarshalIndent(output, "", "  ")
		if e != nil {
			return e
		}
		// Exclusive creation avoids overwriting a reviewed or historical manifest.
		file, e := os.OpenFile(*path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			return e
		}
		_, e = file.Write(data)
		closeErr := file.Close()
		return errors.Join(e, closeErr)
	}
	backupInfo, err := os.Stat(*backup)
	if err != nil || !backupInfo.Mode().IsRegular() || backupInfo.Size() == 0 || backupInfo.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("apply requires the existing private pre-cutover backup")
	}
	data, err := privateFile(*path)
	if err != nil {
		return err
	}
	var input manifest
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&input); err != nil {
		return err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return fmt.Errorf("manifest has trailing content")
	}
	if err = apply(ctx, tx, input); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	fmt.Printf("archived=%d converted_or_removed_presets=%d; original hashes ACKs and release facts retained\n", len(input.Instances), len(input.Presets))
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
