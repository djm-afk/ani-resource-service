package vm

import (
	"context"
	"reflect"
	"testing"

	network "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
)

type currentNetworkRepository struct {
	biz.Repository
	current biz.VM
	reads   int
}

func (r *currentNetworkRepository) Get(context.Context, string, string) (biz.VM, error) {
	r.reads++
	return r.current, nil
}

func TestVMNetworkSubmissionIsAPureCurrentRecordRead(t *testing.T) {
	v := biz.VM{TenantID: "tenant", ID: "vm", Spec: biz.Spec{ClusterID: "cluster", Namespace: "namespace"}, Network: biz.NetworkPlan{AttachmentID: "attachment", SubmissionID: "submission", Generation: 1, SnapshotRevision: 7, Consumer: &biz.NetworkConsumerState{State: "open", DesiredRunState: "running", CurrentVMI: &biz.ObjectRef{Kind: "VirtualMachineInstance", Namespace: "namespace", Name: "vm", UID: "vmi"}, CurrentLauncher: &biz.ObjectRef{Kind: "Pod", Namespace: "namespace", Name: "launcher", UID: "pod"}}}}
	repo := &currentNetworkRepository{current: v}
	adapter := &Network{repo: repo}
	a := network.Attachment{ID: "attachment", TenantID: "tenant", InstanceID: "vm", SubmissionID: "submission", Generation: 1}
	first, err := adapter.GetVMNetworkSubmission(context.Background(), a)
	if err != nil || first.SnapshotRevision != 7 || len(first.CurrentPods) != 1 || first.CurrentPods[0].Reference.UID != "pod" || first.CurrentPods[0].Owner.UID != "vmi" {
		t.Fatal(first, err)
	}
	second, err := adapter.GetVMNetworkSubmission(context.Background(), a)
	if err != nil || repo.reads != 2 || !reflect.DeepEqual(first, second) || !reflect.DeepEqual(repo.current, v) {
		t.Fatal("submission GET mutated its owner", second, err)
	}
}
