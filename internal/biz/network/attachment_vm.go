package biz

import (
	"context"
	"encoding/hex"
	"strings"
	"time"
)

type VMNetworkExpectation struct {
	NADName, NADUID, NADConfigDigest string
}

type VMNetworkSelection struct {
	Reference                                                                      NetworkReference
	TenantID, VPCID, SubnetID, ClusterID, Namespace, BindingRevision, KCNSubnetRef string
}

type VMPrimaryPlan struct {
	FormatVersion                                             int32
	Namespace, NADName, NADUID, NADConfigDigest, KCNSubnetRef string
	TenantID, InstanceID, AttachmentID, SubmissionID          string
	Generation                                                int64
}

type VMCurrentAddress struct {
	NICName, Address, LauncherPodUID, VMIUID string
}

type VMNetworkResolver interface {
	ResolveVMNetwork(context.Context, VMNetworkSelection, VMNetworkExpectation) (VMPrimaryPlan, error)
}

// VMNetworkPreparer is an optional, ancillary part of Network's existing
// durable Subnet reconciliation. Its failure does not change ordinary network
// availability; it only prepares the NAD declaration owned by this service.
type VMNetworkPreparer interface {
	PrepareVMNetwork(context.Context, ProviderTarget) error
}

// Retry only the unfinished Res declaration after the Subnet projection has
// committed. It must not rewrite that projection or a completed operation.
type VMNetworkPreparationRepository interface {
	RetryVMNetworkPreparation(context.Context, Work, time.Duration) error
}

// VMReader is implemented by the VM domain. Instance IDs in this mode are VM
// IDs; this port must never fall through to the existing Instance repository.
type VMReader interface {
	GetVMNetworkSubmission(context.Context, Attachment) (ConsumerSubmission, error)
}

type vmConsumerRouter struct {
	legacy InstanceConsumer
	vm     VMReader
}

func NewVMConsumerRouter(legacy InstanceConsumer, vm VMReader) InstanceConsumer {
	return &vmConsumerRouter{legacy: legacy, vm: vm}
}

func (r *vmConsumerRouter) GetSubmission(ctx context.Context, a Attachment) (ConsumerSubmission, error) {
	if a.WorkloadMode == "vm_managed" {
		if r.vm == nil {
			return ConsumerSubmission{}, Fail(DependencyUnavailable, "VM network reader unavailable")
		}
		return r.vm.GetVMNetworkSubmission(ctx, a)
	}
	if a.WorkloadMode != "" && a.WorkloadMode != "single_pod" && a.WorkloadMode != "controller_managed" {
		return ConsumerSubmission{}, Fail(InvalidArgument, "unknown consumer mode")
	}
	if r.legacy == nil {
		return ConsumerSubmission{}, Fail(DependencyUnavailable, "instance network reader unavailable")
	}
	return r.legacy.GetSubmission(ctx, a)
}

func ValidVMNetworkExpectation(e *VMNetworkExpectation) bool {
	if e == nil || !namespaceName.MatchString(e.NADName) || e.NADUID == "" || len(e.NADUID) > 128 || len(e.NADConfigDigest) != 64 || strings.ToLower(e.NADConfigDigest) != e.NADConfigDigest {
		return false
	}
	_, err := hex.DecodeString(e.NADConfigDigest)
	return err == nil
}

func validVMConsumer(a Attachment, c ConsumerSubmission) bool {
	return validCurrentConsumer(a, c, 3)
}
