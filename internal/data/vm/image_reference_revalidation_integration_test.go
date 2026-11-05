//go:build instanceintegration

package vm

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
)

func readyPinnedImage(t *testing.T, p *Postgres, pool *pgxpool.Pool, tenant, imageID string) biz.Image {
	t.Helper()
	create := imageCommand(t, biz.ImageCreateAction, tenant, imageID, uuid.NewString(), 0, nil, struct{ Name string }{"integration image"})
	if _, e := p.CreateImage(context.Background(), imageFixture(tenant, imageID), create); e != nil {
		t.Fatal(e)
	}
	if _, e := pool.Exec(context.Background(), `UPDATE vm.images SET state='READY', source_sha256=$3,
		source_requested_bytes=$4, observed_source_capacity_bytes=$4, source_checksum_bytes=$4,
		native='{"Namespace":"ns","PVCName":"pvc","PVCUID":"pvc-uid","SourceVolumeMode":"Block","DVName":"dv","DVUID":"dv-uid"}'::jsonb
		WHERE tenant_id=$1 AND id=$2`, tenant, imageID, strings.Repeat("a", 64), int64(1)<<30); e != nil {
		t.Fatal(e)
	}
	ready, e := p.FindImage(context.Background(), tenant, imageID)
	if e != nil {
		t.Fatal(e)
	}
	return ready
}

func vmRow(t *testing.T, pool *pgxpool.Pool, tenant, vmID string, v biz.VM) {
	t.Helper()
	frozen, e := json.Marshal(v)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = pool.Exec(context.Background(), `INSERT INTO vm.virtual_machines(tenant_id,id,create_operation_id,request_digest,spec_digest,initial_spec,current_configuration,version)
		VALUES($1,$2,$3,$4,$5,$6,$6,1)`, tenant, vmID, uuid.NewString(), strings.Repeat("1", 64), strings.Repeat("2", 64), frozen); e != nil {
		t.Fatalf("fixture VM row: %v", e)
	}
}

// TestVMImagePinIgnoresConsumersAlreadyAccountedFor is the #5 guard: pinImageSources runs
// inside every accepted VM command, so re-validating a source for consumers that already
// have a reference row made start, stop and even delete of an independent VM fail with
// VM_IMAGE_NOT_READY once the source image was deleted or became unavailable.
func TestVMImagePinIgnoresConsumersAlreadyAccountedFor(t *testing.T) {
	ctx := context.Background()
	db := newVMImageDatabase(t)
	if e := Migrate(ctx, db.OwnerDSN, db.RuntimeRole); e != nil {
		t.Fatal(e)
	}
	p, e := OpenPostgres(ctx, db.RuntimeDSN)
	if e != nil {
		t.Fatal(e)
	}
	defer p.Close()
	pool, e := pgxpool.New(ctx, db.RuntimeDSN)
	if e != nil {
		t.Fatal(e)
	}
	defer pool.Close()

	tenant, imageID, vmID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	ready := readyPinnedImage(t, p, pool, tenant, imageID)
	source := ready.Source()
	consumerID := uuid.NewString()
	v := biz.VM{TenantID: tenant, ID: vmID, Version: 1, Spec: biz.Spec{Media: source},
		Disks: []biz.Disk{{ID: consumerID, Role: "system", SizeGB: 10}}}
	vmRow(t, pool, tenant, vmID, v)

	ref, e := biz.NewImageReference(v, consumerID, "SYSTEM_DISK", source)
	if e != nil {
		t.Fatal(e)
	}
	tx, e := pool.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if e = insertImageReferences(ctx, tx, []biz.ImageReference{ref}); e != nil {
		t.Fatalf("pin: %v", e)
	}
	if e = tx.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	// The clone finished: the consumer keeps its independent disk and the pin is released.
	if _, e = pool.Exec(ctx, `UPDATE vm.image_source_references SET state='RELEASED' WHERE tenant_id=$1 AND vm_id=$2 AND consumer_id=$3`,
		tenant, vmID, consumerID); e != nil {
		t.Fatal(e)
	}
	// Then the tenant deletes the source image.
	if _, e = pool.Exec(ctx, `UPDATE vm.images SET state='DELETED', cleanup_state='COMPLETE' WHERE tenant_id=$1 AND id=$2`, tenant, imageID); e != nil {
		t.Fatal(e)
	}

	later, e := p.FindImage(ctx, tenant, imageID)
	if e != nil {
		t.Fatal(e)
	}
	if later.State != biz.StateDeleted {
		t.Fatalf("source is not the tombstone the case needs: %s", later.State)
	}

	after, e := pool.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer after.Rollback(ctx)
	if e = p.pinImageSources(ctx, after, v); e != nil {
		t.Fatalf("a follow-up VM command was blocked by its own deleted source: %v", e)
	}
}

// TestVMImageISOBlankSystemDiskIsNotPinned is the #7 guard: only a DISK_IMAGE is cloned
// into the system disk (native.go gives an INSTALL_ISO boot a blank source), so pinning
// that disk leaves a reference whose release condition can never be met, and the ISO stays
// VM_IMAGE_IN_USE forever.
func TestVMImageISOBlankSystemDiskIsNotPinned(t *testing.T) {
	ctx := context.Background()
	db := newVMImageDatabase(t)
	if e := Migrate(ctx, db.OwnerDSN, db.RuntimeRole); e != nil {
		t.Fatal(e)
	}
	p, e := OpenPostgres(ctx, db.RuntimeDSN)
	if e != nil {
		t.Fatal(e)
	}
	defer p.Close()
	pool, e := pgxpool.New(ctx, db.RuntimeDSN)
	if e != nil {
		t.Fatal(e)
	}
	defer pool.Close()

	tenant, imageID, vmID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	ready := readyPinnedImage(t, p, pool, tenant, imageID)
	source := ready.Source()
	source.Kind = "INSTALL_ISO"
	source.VolumeMode = "Filesystem"
	v := biz.VM{TenantID: tenant, ID: vmID, Version: 1, Spec: biz.Spec{Media: source},
		Disks: []biz.Disk{{ID: uuid.NewString(), Role: "system", SizeGB: 10}}}
	vmRow(t, pool, tenant, vmID, v)

	tx, e := pool.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(ctx)
	if e = p.pinImageSources(ctx, tx, v); e != nil {
		t.Fatalf("ISO pin pass: %v", e)
	}
	var systemPins int
	if e = tx.QueryRow(ctx, `SELECT count(*) FROM vm.image_source_references WHERE tenant_id=$1 AND vm_id=$2 AND consumer_kind='SYSTEM_DISK'`,
		tenant, vmID).Scan(&systemPins); e != nil {
		t.Fatal(e)
	}
	if systemPins != 0 {
		t.Fatalf("a blank ISO system disk was pinned as %d SYSTEM_DISK reference(s); it can never be released", systemPins)
	}
}
