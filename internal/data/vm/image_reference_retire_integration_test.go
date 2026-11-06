//go:build instanceintegration

package vm

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
)

// retirePinnedVM inserts one VM row plus its HELD reference. For a retiring VM the
// system disk was deleted mid-clone: the record still carries the historical DV/PVC
// UID while the cluster has neither object, which is exactly the state P2 leaves.
func retirePinnedVM(t *testing.T, pool *pgxpool.Pool, p *Postgres, tenant, imageID string, closing bool,
	observed, desired string) (biz.VM, biz.ImageReference) {
	t.Helper()
	ctx := context.Background()
	ready := readyPinnedImage(t, p, pool, tenant, imageID)
	source := ready.Source()
	vmID, diskID := uuid.NewString(), uuid.NewString()
	v := biz.VM{TenantID: tenant, ID: vmID, Version: 4, Closing: closing, Desired: desired, Observed: observed,
		Spec: biz.Spec{Media: source},
		Disks: []biz.Disk{{ID: diskID, Role: "system", SizeGB: 10, Tombstone: closing,
			DV:  biz.ObjectRef{Kind: "DataVolume", Namespace: source.Namespace, Name: "vd-" + diskID, UID: uuid.NewString()},
			PVC: biz.ObjectRef{Kind: "PersistentVolumeClaim", Namespace: source.Namespace, Name: "vd-" + diskID, UID: uuid.NewString()}}}}
	vmRow(t, pool, tenant, vmID, v)

	ref, e := biz.NewImageReference(v, diskID, "SYSTEM_DISK", source)
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
	return v, ref
}

func referenceState(t *testing.T, pool *pgxpool.Pool, ref biz.ImageReference) (string, string) {
	t.Helper()
	var state, reason string
	if e := pool.QueryRow(context.Background(), `SELECT state,reason FROM vm.image_source_references
		WHERE tenant_id=$1 AND vm_id=$2 AND consumer_kind=$3 AND consumer_id=$4`,
		ref.TenantID, ref.VMID, ref.ConsumerKind, ref.ConsumerID).Scan(&state, &reason); e != nil {
		t.Fatal(e)
	}
	return state, reason
}

func emptyCloneCluster(t *testing.T) *ImageNative {
	t.Helper()
	c := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		dvResource: "DataVolumeList", pvcResource: "PersistentVolumeClaimList"})
	n, e := NewImageNative(c, ImageUploadConfig{ClusterID: "cluster-a", UploadDeadline: time.Hour,
		TokenTTL: time.Minute, VerifierActiveDeadlineSeconds: 60})
	if e != nil {
		t.Fatal(e)
	}
	return n
}

// TestVMImageRetireReleasesThePinOnceTheCloneTargetIsGone is the P2 guard at the data
// layer: a VM deleted before its clone reached Succeeded never satisfies the completion
// or the "empty UID" proof, so without an absence-based release the source stays pinned
// forever and its delete keeps returning VM_IMAGE_IN_USE.
func TestVMImageRetireReleasesThePinOnceTheCloneTargetIsGone(t *testing.T) {
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

	_, ref := retirePinnedVM(t, pool, p, uuid.NewString(), uuid.NewString(), true, "Deleted", "Deleted")
	w, e := biz.NewReferenceWorker(p, p, emptyCloneCluster(t), slog.New(slog.DiscardHandler))
	if e != nil {
		t.Fatal(e)
	}
	if _, e = w.Step(ctx); e != nil {
		t.Fatalf("reference step: %v", e)
	}
	state, reason := referenceState(t, pool, ref)
	if state != "RELEASED" || reason != biz.ReferenceReleasedRetired {
		t.Fatalf("a deleted VM whose clone target is gone kept the pin: state=%s reason=%s", state, reason)
	}
}

// TestVMImageRetireKeepsThePinWhileTheVMLives is the boundary: absence of a clone target
// is only evidence after the consumer is retiring, never while the VM is still intended
// to run.
func TestVMImageRetireKeepsThePinWhileTheVMLives(t *testing.T) {
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

	_, ref := retirePinnedVM(t, pool, p, uuid.NewString(), uuid.NewString(), false, "Running", "Running")
	w, e := biz.NewReferenceWorker(p, p, emptyCloneCluster(t), slog.New(slog.DiscardHandler))
	if e != nil {
		t.Fatal(e)
	}
	if _, e = w.Step(ctx); e != nil {
		t.Fatalf("reference step: %v", e)
	}
	state, reason := referenceState(t, pool, ref)
	if state != "HELD" || reason != "" {
		t.Fatalf("a live VM released its pin on a transiently absent target: state=%s reason=%s", state, reason)
	}
}
