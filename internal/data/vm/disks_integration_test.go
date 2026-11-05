//go:build instanceintegration

package vm

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	"github.com/zhangzhe-ctrl/ani-resource-service/tests/testenv"
)

type diskNativeBoundary struct {
	biz.NativePort
	disks    map[string]biz.Disk
	created  int
	loseACK  bool
	attached map[string]bool
	deleted  int
}

func (n *diskNativeBoundary) EnsureVolume(_ context.Context, v biz.VM, d biz.Disk) (biz.Disk, error) {
	if existing, ok := n.disks[d.ID]; ok {
		return existing, nil
	}
	d.DV.UID = uuid.NewString()
	d.Preparation = "WaitForFirstConsumer"
	n.disks[d.ID] = d
	n.created++
	if n.loseACK {
		n.loseACK = false
		return d, biz.ErrUnknown
	}
	return d, nil
}
func (n *diskNativeBoundary) Observe(_ context.Context, v biz.VM) (biz.Observation, error) {
	return biz.Observation{VM: v.Native, Stopped: true, State: "Stopped"}, nil
}
func (n *diskNativeBoundary) ValidateDiskAttachment(context.Context, biz.VM, biz.Disk, bool) (biz.Observation, error) {
	return biz.Observation{}, nil
}
func (n *diskNativeBoundary) EnsureDiskAttachment(_ context.Context, _ biz.VM, d biz.Disk, attached bool) (bool, error) {
	n.attached[d.ID] = attached
	return true, nil
}
func (n *diskNativeBoundary) DiskUnreferenced(_ context.Context, _ biz.VM, d biz.Disk) (bool, error) {
	return !n.attached[d.ID], nil
}
func (n *diskNativeBoundary) DeleteVolume(_ context.Context, _ biz.VM, d biz.Disk) (bool, error) {
	delete(n.disks, d.ID)
	n.deleted++
	return true, nil
}

func TestVMDisksDurableWFFCReplayDetachAndDelete(t *testing.T) {
	ctx := context.Background()
	db := testenv.NewDatabase(t)
	if e := Migrate(ctx, db.Owner.Config().ConnString(), db.RuntimeRole); e != nil {
		t.Fatal(e)
	}
	p, e := OpenPostgres(ctx, db.RuntimeDSN)
	if e != nil {
		t.Fatal(e)
	}
	defer p.Close()
	tenant, id, create := uuid.NewString(), uuid.NewString(), uuid.NewString()
	_, e = p.AcceptCreate(ctx, biz.Acceptance{TenantID: tenant, VMID: id, OperationID: create, RequestDigest: stringDigest('a'), Spec: biz.Spec{Canonical: []byte(`{"fixture":"disk-native-boundary"}`), Digest: stringDigest('b'), Name: "disks"}})
	if e != nil {
		t.Fatal(e)
	}
	o, v, e := p.Claim(ctx, uuid.NewString(), time.Minute)
	if e != nil {
		t.Fatal(e)
	}
	v.Native.UID = uuid.NewString()
	v.Observed = "Stopped"
	o.State = "succeeded"
	if e = p.Save(ctx, o, v); e != nil {
		t.Fatal(e)
	}
	n := &diskNativeBoundary{disks: map[string]biz.Disk{}, attached: map[string]bool{}, loseACK: true}
	command := func(action, key string, version int64, fields map[string]any) biz.Command {
		raw, _ := json.Marshal(fields)
		h := stringDigest('c')
		if action != "create-disk" {
			h = stringDigest('d')
		}
		return biz.Command{TenantID: tenant, VMID: id, Action: action, Key: key, ExpectedVersion: version, Intent: raw, RequestDigest: h}
	}
	c := command("create-disk", "disk-a", 1, map[string]any{"name": "data-a", "size_gb": "2"})
	accepted, e := p.AcceptCommand(ctx, c)
	if e != nil {
		t.Fatal(e)
	}
	w, _ := biz.NewWorker(p, n, preparingVMNetwork{})
	if e = w.RunOnce(ctx); e != nil {
		t.Fatal(e)
	}
	unknown, e := p.GetOperation(ctx, tenant, id, accepted.ID)
	if e != nil || unknown.State != "unknown" || unknown.Pending == nil || n.created != 1 {
		t.Fatalf("lost ACK was not durable: %+v %v", unknown, e)
	}
	p2, e := OpenPostgres(ctx, db.RuntimeDSN)
	if e != nil {
		t.Fatal(e)
	}
	defer p2.Close()
	w, _ = biz.NewWorker(p2, n, preparingVMNetwork{})
	run := func(operation string) {
		t.Helper()
		deadline := time.Now().Add(4 * time.Second)
		for time.Now().Before(deadline) {
			if _, e := w.Step(ctx); e != nil {
				t.Fatal(e)
			}
			o, e := p2.GetOperation(ctx, tenant, id, operation)
			if e != nil {
				t.Fatal(e)
			}
			if o.State == "succeeded" {
				return
			}
			if o.State == "failed" {
				t.Fatalf("failed: %+v", o)
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatal("operation did not complete without a consumer")
	}
	run(accepted.ID)
	replay, e := p2.AcceptCommand(ctx, c)
	if e != nil || replay.ID != accepted.ID || n.created != 1 {
		t.Fatalf("create replay replaced disk: %+v %v", replay, e)
	}
	v, e = p2.Get(ctx, tenant, id)
	if e != nil || len(v.Disks) != 1 {
		t.Fatal(e, v)
	}
	d := v.Disks[0]
	if d.Attached || d.Preparation != "WaitForFirstConsumer" || d.DV.UID == "" {
		t.Fatalf("WFFC treated as unavailable declaration: %+v", d)
	}
	foreign := command("attach-disk", "foreign", v.Version, map[string]any{"disk_id": uuid.NewString()})
	if _, e = p2.AcceptCommand(ctx, foreign); !errors.Is(e, biz.ErrNotFound) {
		t.Fatalf("foreign logical disk accepted: %v", e)
	}
	for _, action := range []string{"attach-disk", "detach-disk", "attach-disk", "detach-disk", "delete-disk"} {
		v, _ = p2.Get(ctx, tenant, id)
		c = command(action, action+uuid.NewString(), v.Version, map[string]any{"disk_id": d.ID})
		o, e := p2.AcceptCommand(ctx, c)
		if e != nil {
			t.Fatal(e)
		}
		run(o.ID)
	}
	v, _ = p2.Get(ctx, tenant, id)
	if !v.Disks[0].Tombstone || v.Disks[0].ID != d.ID || v.Disks[0].Serial != d.Serial || n.created != 1 || n.deleted != 1 {
		t.Fatalf("disk identity/content lifetime changed: %+v created=%d deleted=%d", v.Disks, n.created, n.deleted)
	}
}
