package vm

import (
	"context"
	"testing"
)

type createInventoryNative struct {
	NativePort
	reads int
}

func (n *createInventoryNative) EnsureVolume(_ context.Context, _ VM, d Disk) (Disk, error) {
	n.reads++
	d.PVC = ObjectRef{Kind: "PersistentVolumeClaim", Namespace: "ns", Name: "declared-volume", UID: "actual-pvc"}
	d.Preparation = "Pending"
	return d, nil
}
func TestCreateRefreshesDeclaredVolumeInventoryWithoutWaitingForBinding(t *testing.T) {
	native := &createInventoryNative{}
	worker := &Worker{native: native}
	v := VM{ID: "20000000-0000-4000-8000-000000000001", TenantID: "10000000-0000-4000-8000-000000000001", SecurityGroupsInitialized: true, Native: ObjectRef{UID: "vm-uid"}, Network: NetworkPlan{AttachmentID: "attachment"},
		Spec:  Spec{Media: Source{Kind: "DISK_IMAGE"}, StartAfterCreate: false},
		Disks: []Disk{{ID: "disk", DV: ObjectRef{Name: "declared-volume", UID: "registered-dv"}}}}
	op := Operation{Action: CreateAction}
	if err := worker.create(context.Background(), &op, &v); err != nil {
		t.Fatal(err)
	}
	if native.reads != 1 || op.State != "succeeded" || v.Disks[0].PVC.UID != "actual-pvc" || v.Disks[0].Preparation != "Pending" {
		t.Fatal("inventory registration waited for binding or lost UID", v.Disks, op)
	}
}
