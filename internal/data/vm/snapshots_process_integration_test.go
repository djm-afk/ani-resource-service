//go:build instanceintegration

package vm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	"github.com/zhangzhe-ctrl/ani-resource-service/tests/testenv"
)

const snapshotProcessInputEnv = "ANI_VM_SNAPSHOT_PROCESS_INPUT"

type snapshotProcessInput struct {
	DSN, TenantID, VMID, OperationID, Ledger, StopStep string
	ExitCode                                           int
	VM                                                 biz.ObjectRef
	Snapshot                                           biz.Snapshot
	OldDisk, NewDisk                                   biz.Disk
}

// Only the external native effects use a file. All operation progress and disk
// mapping are written by the real Worker and PostgreSQL repository.
type snapshotProcessEffect struct {
	Action, VMID, OperationID string
	Target                    biz.ObjectRef
	Storage                   []biz.ObjectRef
}

type snapshotProcessRepository struct {
	biz.Repository
	step string
	code int
}

func (r snapshotProcessRepository) Checkpoint(ctx context.Context, op biz.Operation, v biz.VM) error {
	if err := r.Repository.Checkpoint(ctx, op, v); err != nil {
		return err
	}
	if op.Step == r.step {
		fmt.Printf("committed checkpoint=%s operation=%s pid=%d exit=%d\n", op.Step, op.ID, os.Getpid(), r.code)
		_ = os.Stdout.Sync()
		os.Exit(r.code)
	}
	return nil
}

type snapshotProcessNative struct {
	biz.NativePort
	biz.SnapshotNativePort
	input snapshotProcessInput
}

func (n snapshotProcessNative) effect(action string, target biz.ObjectRef, storage []biz.ObjectRef) error {
	f, err := os.OpenFile(n.input.Ledger, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err = json.NewEncoder(f).Encode(snapshotProcessEffect{action, n.input.VMID, n.input.OperationID, target, storage}); err != nil {
		return err
	}
	return f.Sync()
}

func (n snapshotProcessNative) Observe(_ context.Context, v biz.VM) (biz.Observation, error) {
	if v.Native != n.input.VM {
		return biz.Observation{}, biz.ErrOwnership
	}
	return biz.Observation{VM: v.Native, Stopped: true, State: "Stopped"}, nil
}

func (n snapshotProcessNative) ObserveSnapshot(_ context.Context, _ biz.VM, s biz.Snapshot) (biz.Snapshot, error) {
	if s.ID != n.input.Snapshot.ID || s.Native != n.input.Snapshot.Native {
		return s, biz.ErrOwnership
	}
	return s, nil
}

func (n snapshotProcessNative) EnsureRestore(_ context.Context, _ biz.VM, s biz.Snapshot, ref biz.ObjectRef) (biz.RestoreObservation, error) {
	if s.ID != n.input.Snapshot.ID || ref.Namespace != n.input.VM.Namespace || ref.Name != "vm-restore-"+n.input.OperationID {
		return biz.RestoreObservation{}, biz.ErrOwnership
	}
	ref.UID = uuid.NewSHA1(uuid.NameSpaceOID, []byte(n.input.OperationID)).String()
	if err := n.effect("ensure_restore", ref, nil); err != nil {
		return biz.RestoreObservation{}, err
	}
	return biz.RestoreObservation{Native: ref, Complete: true, OutputsKnown: true, Disks: []biz.Disk{n.input.NewDisk}}, nil
}

func (n snapshotProcessNative) ApplyRestoredConfiguration(_ context.Context, v biz.VM, _ biz.Snapshot) (bool, error) {
	if len(v.Disks) != 1 || !reflect.DeepEqual(v.Disks[0], n.input.NewDisk) || len(v.RetiredDisks) != 1 || !reflect.DeepEqual(v.RetiredDisks[0], n.input.OldDisk) {
		return false, biz.ErrOwnership
	}
	return true, n.effect("apply_configuration", v.Disks[0].PVC, nil)
}

func (n snapshotProcessNative) ObserveRetiredVolume(_ context.Context, _ biz.VM, disk biz.Disk) (biz.Disk, error) {
	if !reflect.DeepEqual(disk, n.input.OldDisk) {
		return disk, biz.ErrOwnership
	}
	return disk, nil
}

func (n snapshotProcessNative) DeleteVolume(_ context.Context, _ biz.VM, disk biz.Disk) (bool, error) {
	if !reflect.DeepEqual(disk, n.input.OldDisk) {
		return false, biz.ErrOwnership
	}
	return true, n.effect("delete_old_volume", disk.PVC, disk.Cleanup)
}

type snapshotProcessNetwork struct{ biz.NetworkPort }

func TestSnapshotRestoreProcessChild(t *testing.T) {
	path := os.Getenv(snapshotProcessInputEnv)
	if path == "" {
		t.Skip("only invoked by the actual process-exit integration test")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("read private child input", err)
	}
	var in snapshotProcessInput
	if json.Unmarshal(body, &in) != nil {
		t.Fatal("invalid private child input")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	repo, err := OpenPostgres(ctx, in.DSN)
	if err != nil {
		t.Fatal("open child repository")
	}
	defer repo.Close()
	worker, err := biz.NewWorker(snapshotProcessRepository{repo, in.StopStep, in.ExitCode}, snapshotProcessNative{input: in}, snapshotProcessNetwork{}, discardLoggerVMLane())
	if err != nil {
		t.Fatal(err)
	}
	for ctx.Err() == nil {
		if _, err = worker.Step(ctx); err != nil {
			t.Fatal("real worker step", err)
		}
		op, getErr := repo.GetOperation(ctx, in.TenantID, in.VMID, in.OperationID)
		if getErr != nil {
			t.Fatal("read original restore operation", getErr)
		}
		if op.State == "succeeded" {
			fmt.Printf("original operation=%s converged pid=%d\n", op.ID, os.Getpid())
			return
		}
		if op.State == "failed" {
			t.Fatalf("restore failed: %s", op.Reason)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("worker did not reach the requested checkpoint or completion")
}

func TestSnapshotRestoreActualProcessExitRecovery(t *testing.T) {
	ctx := context.Background()
	db := testenv.NewDatabase(t)
	if err := Migrate(ctx, db.Owner.Config().ConnString(), db.RuntimeRole); err != nil {
		t.Fatal(err)
	}
	repo, err := OpenPostgres(ctx, db.RuntimeDSN)
	if err != nil {
		t.Fatal("open fixture repository")
	}
	in := snapshotProcessInput{DSN: db.RuntimeDSN, TenantID: uuid.NewString(), VMID: uuid.NewString(), Ledger: filepath.Join(t.TempDir(), "native-effects.jsonl")}
	createID := uuid.NewString()
	disk := biz.NewInitialDisk(in.VMID, "system", "system", "system", 8)
	accepted, err := repo.AcceptCreate(ctx, biz.Acceptance{TenantID: in.TenantID, VMID: in.VMID, OperationID: createID, RequestDigest: stringDigest('a'), Spec: biz.Spec{Canonical: json.RawMessage(`{"process_exit_fixture":true}`), Digest: stringDigest('b'), Name: "snapshot-process-exit", Namespace: "tenant-process-test", NamespaceUID: uuid.NewString(), BootDevice: "SYSTEM_DISK", InitialDisks: []biz.Disk{disk}}})
	if err != nil {
		t.Fatal(err)
	}
	create, v, err := repo.Claim(ctx, uuid.NewString(), time.Minute)
	if err != nil || create.ID != accepted.ID {
		t.Fatal("claim fixture create", err)
	}
	v.Native = biz.ObjectRef{Kind: "VirtualMachine", Namespace: v.Spec.Namespace, Name: "vm-" + in.VMID, UID: uuid.NewString()}
	v.Observed, v.Desired = "Stopped", "Stopped"
	v.Disks[0].DV.Namespace, v.Disks[0].DV.UID = v.Spec.Namespace, uuid.NewString()
	v.Disks[0].PVC = biz.ObjectRef{Kind: "PersistentVolumeClaim", Namespace: v.Spec.Namespace, Name: v.Disks[0].DV.Name, UID: uuid.NewString()}
	v.Disks[0].Cleanup = []biz.ObjectRef{{Kind: "PersistentVolume", Name: "original-pv", UID: uuid.NewString()}}
	in.VM, in.OldDisk = v.Native, v.Disks[0]
	in.NewDisk = in.OldDisk
	in.NewDisk.DV = biz.ObjectRef{}
	in.NewDisk.PVC = biz.ObjectRef{Kind: "PersistentVolumeClaim", Namespace: v.Spec.Namespace, Name: "restored-system", UID: uuid.NewString()}
	in.NewDisk.Cleanup = []biz.ObjectRef{{Kind: "PersistentVolume", Name: "restored-pv", UID: uuid.NewString()}}
	in.Snapshot, err = biz.NewSnapshot(v, uuid.NewString(), "process-exit")
	if err != nil {
		t.Fatal(err)
	}
	in.Snapshot.Native.UID = uuid.NewString()
	in.Snapshot.Content = biz.ObjectRef{Kind: "VirtualMachineSnapshotContent", Namespace: v.Spec.Namespace, Name: "process-content", UID: uuid.NewString()}
	in.Snapshot.Ready = true
	in.Snapshot.Included = []string{in.OldDisk.VolumeName}
	in.Snapshot.Complete = biz.SnapshotComplete(in.Snapshot)
	v.Snapshots, create.State = []biz.Snapshot{in.Snapshot}, "succeeded"
	if err = repo.Save(ctx, create, v); err != nil {
		t.Fatal(err)
	}
	plan, err := biz.PreviewSnapshotRestore(v, in.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	intent, _ := json.Marshal(map[string]any{"snapshot_id": in.Snapshot.ID, "preview_digest": plan.Digest, "destructive_confirmation": true, "expected_version": v.Version})
	command := biz.Command{TenantID: in.TenantID, VMID: in.VMID, Action: "restore-snapshot", Key: "one-original-process-restore", ExpectedVersion: v.Version, RequestDigest: stringDigest('c'), Intent: intent}
	op, err := repo.AcceptCommand(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	in.OperationID = op.ID
	repo.Close()

	steps := []string{"native_restore_completed", "mapping_committed", "cleanup_completed", ""}
	for phase, step := range steps {
		in.StopStep, in.ExitCode = step, 73+phase
		if step == "" {
			in.ExitCode = 0
		}
		runSnapshotProcessChild(t, in)
		// Wait has already confirmed this exact process exited. A fresh pool
		// reads the committed business state before only its dead lease moves.
		fresh, openErr := OpenPostgres(ctx, db.RuntimeDSN)
		if openErr != nil {
			t.Fatal("open independent recovery repository")
		}
		current, readErr := fresh.Get(ctx, in.TenantID, in.VMID)
		durable, opErr := fresh.GetOperation(ctx, in.TenantID, in.VMID, in.OperationID)
		if readErr != nil || opErr != nil || durable.ID != op.ID || current.Version != op.IntentVersion || current.LastOperationID != op.ID || durable.Snapshot == nil {
			fresh.Close()
			t.Fatal("original operation or version lost after process exit", readErr, opErr)
		}
		progress := durable.Snapshot
		if !progress.NativeRestoreCompleted || progress.MappingCommitted != (phase >= 1) || progress.CleanupCompleted != (phase >= 2) {
			t.Fatalf("phase %d lost durable checkpoints: %+v", phase, progress)
		}
		wantCurrent, wantRetired := in.OldDisk, in.NewDisk
		if phase >= 1 {
			wantCurrent, wantRetired = in.NewDisk, in.OldDisk
		}
		if len(current.Disks) != 1 || !reflect.DeepEqual(current.Disks[0], wantCurrent) || !reflect.DeepEqual(progress.RestoredDisks, []biz.Disk{in.NewDisk}) {
			t.Fatal("current mapping or restored physical refs changed across process exit")
		}
		if phase < 2 && (len(current.RetiredDisks) != 1 || !reflect.DeepEqual(current.RetiredDisks[0], wantRetired)) || phase >= 2 && len(current.RetiredDisks) != 0 {
			t.Fatal("old/new pending cleanup inventory was lost")
		}
		var raw []byte
		if err = fresh.pool.QueryRow(ctx, "SELECT record FROM vm.snapshots WHERE tenant_id=$1 AND vm_id=$2 AND snapshot_id=$3", in.TenantID, in.VMID, in.Snapshot.ID).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var frozen biz.Snapshot
		if json.Unmarshal(raw, &frozen) != nil || !reflect.DeepEqual(frozen.Inventory, []biz.Disk{in.OldDisk}) {
			t.Fatal("restore modified immutable snapshot inventory")
		}
		if phase < 3 {
			if durable.State == "succeeded" || durable.Step != step {
				t.Fatal("checkpoint was replaced by premature completion")
			}
			if _, _, claimErr := fresh.Claim(ctx, uuid.NewString(), time.Minute); !errors.Is(claimErr, biz.ErrNotFound) {
				t.Fatal("dead worker lease was not fenced before explicit expiry", claimErr)
			}
			tag, expireErr := db.Owner.Exec(ctx, "UPDATE vm.operations SET lease_until=now()-interval '1 second',next_attempt_at=now()-interval '1 second' WHERE tenant_id=$1 AND id=$2 AND lease_token=$3 AND lease_until>now()", in.TenantID, in.OperationID, durable.Lease)
			if expireErr != nil || tag.RowsAffected() != 1 {
				t.Fatal("expire only the confirmed dead process lease", expireErr)
			}
		} else if durable.State != "succeeded" || durable.Pending != nil {
			t.Fatal("fourth process did not converge the original operation")
		}
		fresh.Close()
	}
	body, err := os.ReadFile(in.Ledger)
	if err != nil {
		t.Fatal(err)
	}
	var effects []snapshotProcessEffect
	for _, line := range bytes.Split(bytes.TrimSpace(body), []byte{'\n'}) {
		var effect snapshotProcessEffect
		if json.Unmarshal(line, &effect) != nil || effect.VMID != in.VMID || effect.OperationID != in.OperationID {
			t.Fatal("invalid external effect identity")
		}
		effects = append(effects, effect)
	}
	wantRestore := biz.ObjectRef{Kind: "VirtualMachineRestore", Namespace: in.VM.Namespace, Name: "vm-restore-" + in.OperationID, UID: uuid.NewSHA1(uuid.NameSpaceOID, []byte(in.OperationID)).String()}
	if len(effects) != 3 || effects[0].Action != "ensure_restore" || effects[0].Target != wantRestore || effects[1].Action != "apply_configuration" || effects[1].Target != in.NewDisk.PVC || effects[2].Action != "delete_old_volume" || effects[2].Target != in.OldDisk.PVC || !reflect.DeepEqual(effects[2].Storage, in.OldDisk.Cleanup) {
		t.Fatalf("native effects were repeated or changed: %+v", effects)
	}
	t.Log("PG+BOUNDARY: three committed checkpoints survived actual exits 73/74/75; fourth process completed the same operation; native restore/apply/old-volume-delete each once")
}

func runSnapshotProcessChild(t *testing.T, in snapshotProcessInput) {
	t.Helper()
	body, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "private-child.json")
	if err = os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestSnapshotRestoreProcessChild$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), snapshotProcessInputEnv+"="+path)
	output, runErr := cmd.CombinedOutput()
	code := 0
	if runErr != nil {
		var exit *exec.ExitError
		if !errors.As(runErr, &exit) {
			t.Fatal("start/wait child process", runErr)
		}
		code = exit.ExitCode()
	}
	t.Logf("waited actual child pid=%d step=%q exit=%d\n%s", cmd.Process.Pid, in.StopStep, code, output)
	if ctx.Err() != nil || code != in.ExitCode {
		t.Fatalf("unexpected child exit: want %d, got %d", in.ExitCode, code)
	}
}
