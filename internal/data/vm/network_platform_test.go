package vm

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	network "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
)

type vmPreparedAttachmentRepository struct {
	network.AttachmentRepository
	requests        []network.PrepareAttachment
	bindingRevision string
}

func (r *vmPreparedAttachmentRepository) PrepareAttachment(_ context.Context, request network.PrepareAttachment) (network.Attachment, error) {
	r.requests = append(r.requests, request)
	revision := r.bindingRevision
	if revision == "" {
		revision = request.AcceptedReference.SubnetBindingID
	}
	return network.Attachment{ID: "att_" + strings.Repeat("c", 32), TenantID: request.TenantID, InstanceID: request.InstanceID, VPCID: request.VPCID, SubnetID: request.SubnetID, WorkloadMode: request.WorkloadMode, SubmissionID: request.SubmissionID, Generation: request.Generation, Version: 1, BindingRevision: revision, VMPrimary: &network.VMPrimaryPlan{FormatVersion: 1, Namespace: request.Namespace, NADName: request.VMExpectation.NADName, NADUID: request.VMExpectation.NADUID, NADConfigDigest: request.VMExpectation.NADConfigDigest, KCNSubnetRef: "kcn-system/workloads"}}, nil
}

func TestVMPreparePersistsActualNADBeforeReplayableAttachment(t *testing.T) {
	ctx := context.Background()
	tenant, id := uuid.NewString(), uuid.NewString()
	vpc, subnet, binding := "vpc_"+strings.Repeat("a", 32), "subnet_"+strings.Repeat("b", 32), uuid.NewString()
	digest := strings.Repeat("d", 64)
	v := biz.VM{TenantID: tenant, ID: id, Desired: "Running", Spec: biz.Spec{Namespace: "tenant-" + tenant, NamespaceUID: uuid.NewString(), ClusterID: uuid.NewString(), Canonical: json.RawMessage(`{"network":{"nad_uid":""}}`), Digest: "immutable-digest", ServiceAccount: "ani-vm-workload", Network: biz.NetworkSelection{VPCID: vpc, SubnetID: subnet, NADName: "ani-vm-primary-v1", NADDigest: digest, NADConfig: `{"type":"kc-networking","server_socket":"/run/kcn.sock"}`, Reference: network.NetworkReference{OwnerScope: network.OwnerScopePlatform, VPCID: vpc, SubnetID: subnet, SubnetBindingID: binding}}}}
	attachments := &vmPreparedAttachmentRepository{}
	adapter, err := NewNetwork(network.NewAttachments(attachments), &currentNetworkRepository{})
	if err != nil {
		t.Fatal(err)
	}
	preparations := 0
	adapter.UseTenantPreparation(func(context.Context, biz.VM) (biz.NetworkPlan, error) {
		preparations++
		return biz.NetworkPlan{Namespace: v.Spec.Namespace, NADName: v.Spec.Network.NADName, NADUID: "actual-nad-uid", NADDigest: digest, BindingRevision: binding, KCNSubnetRef: "kcn-system/workloads", BasicIsolationName: "ani-tenant-" + tenant}, nil
	})
	partial, err := adapter.Prepare(ctx, v)
	if err != nil || preparations != 1 || len(attachments.requests) != 0 || partial.NADUID == "" || partial.AttachmentID != "" || partial.Consumer != nil || partial.SnapshotRevision != 0 {
		t.Fatalf("first prepare did not stop at the owner save boundary: %+v %v", partial, err)
	}
	v.Network = partial
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	v, err = vmFrom(raw)
	if err != nil || v.Spec.Network.NADUID != "" || v.Spec.Network.NADConfig == "" || v.Spec.Network.Reference.OwnerScope != network.OwnerScopePlatform || string(v.Spec.Canonical) != `{"network":{"nad_uid":""}}` {
		t.Fatalf("partial save rewrote or lost frozen owner data: %+v %v", v.Spec.Network, err)
	}
	adapter.UseTenantPreparation(func(context.Context, biz.VM) (biz.NetworkPlan, error) {
		t.Fatal("second-round replay reopened namespace, template, or new admission")
		return biz.NetworkPlan{}, nil
	})
	full, err := adapter.Prepare(ctx, v)
	if err != nil || full.AttachmentID == "" || full.SnapshotRevision != 1 || full.Consumer == nil || full.BasicIsolationName != partial.BasicIsolationName {
		t.Fatalf("full prepare lost actual identity or isolation: %+v %v", full, err)
	}
	// Simulate a process exit after Attachment commit and before the VM save.
	replayed, err := adapter.Prepare(ctx, v)
	if err != nil || !reflect.DeepEqual(full, replayed) || len(attachments.requests) != 2 {
		t.Fatalf("lost full-save replay differed: %+v %v", replayed, err)
	}
	first, second := attachments.requests[0], attachments.requests[1]
	if !first.Accepted || first.AcceptedReference != v.Spec.Network.Reference || first.VMExpectation.NADUID != partial.NADUID || first.RequestKey != "vm-"+id || first.SubmissionID != uuid.NewSHA1(uuid.NameSpaceOID, []byte("ani-vm-network/"+id)).String() || first.Fingerprint() != second.Fingerprint() {
		t.Fatal("second prepare changed permanent request identity")
	}
	// An older committed Attachment can return its original BindingID:UID
	// token after recovery created a partial plan containing only BindingID.
	attachments.bindingRevision = binding + ":original-subnet-uid"
	replayed, err = adapter.Prepare(ctx, v)
	if err != nil || replayed.BindingRevision != attachments.bindingRevision || attachments.requests[2].Fingerprint() != first.Fingerprint() {
		t.Fatalf("legacy replay token was rejected or rewritten: %+v %v", replayed, err)
	}
	attachments.bindingRevision = uuid.NewString() + ":original-subnet-uid"
	if _, err = adapter.Prepare(ctx, v); !errors.Is(err, biz.ErrChanged) {
		t.Fatal("legacy replay accepted a different binding", err)
	}
}

func TestOldPrivateVMSnapshotNeverInfersPlatform(t *testing.T) {
	for _, revision := range []string{"binding-old", "binding-old:original-subnet-uid"} {
		t.Run(revision, func(t *testing.T) {
			v, err := vmFrom([]byte(`{"Spec":{"Canonical":{"immutable":true},"Network":{"VPCID":"vpc-old","SubnetID":"subnet-old","NADUID":"original"}},"Network":{"BindingRevision":"` + revision + `"}}`))
			if err != nil || v.Spec.Network.Reference.OwnerScope != network.OwnerScopeTenant || v.Spec.Network.Reference.SubnetBindingID != "binding-old" || v.Network.BindingRevision != revision || string(v.Spec.Canonical) != `{"immutable":true}` {
				t.Fatal("legacy private normalization changed immutable input", v, err)
			}
		})
	}
}

func TestVMNetworkAdmissionAndOwnershipErrorsRetainTheirDomain(t *testing.T) {
	for _, tc := range []struct{ input, want error }{
		{network.Fail(network.NetworkNewAdmissionClosed, "private detail"), biz.ErrNetworkAdmissionClosed},
		{network.Fail(network.ProviderOwnership, "private detail"), biz.ErrOwnership},
		{&network.ProviderError{Kind: network.ProviderConflict}, biz.ErrOwnership},
		{network.Fail(network.AttachmentConflict, "private detail"), biz.ErrChanged},
	} {
		if got := MapNetworkError(tc.input); !errors.Is(got, tc.want) {
			t.Fatalf("network error fell through to dependency: %v", got)
		}
	}
}
