package biz

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

func TestImportedPlatformNetworkNeverUsesDeclarationLifecycle(t *testing.T) {
	for _, ready := range []*bool{reportedReady(true), reportedReady(false), nil} {
		repository := &earlyCompletionRepository{work: Work{PlatformNetwork: &PlatformNetworkWorkSpec{Kind: "subnet", UID: "original"}, Resource: ResourceWork{Kind: "subnet", State: Available, Reason: "old"}, KnownIdentity: "original"}}
		provider := &directObservationProvider{observation: ProviderObservation{Exists: true, Identity: "original", Ready: ready, StatusReason: "native", PlatformNetwork: &PlatformNetworkFacts{CIDR: "10.9.0.0/24"}}}
		worker, err := NewWorker(repository, provider, uuid.NewString(), DefaultWorkerPolicy())
		if err != nil {
			t.Fatal(err)
		}
		if _, err = worker.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
		if provider.creates != 0 || provider.deletes != 0 || repository.mutations != 0 || repository.finishes != 1 {
			t.Fatal("imported directory entered declaration lifecycle", provider, repository)
		}
		if !repository.progress.Observed || repository.progress.Backoff || repository.progress.NextDelay != 0 || repository.progress.PlatformNetwork == nil {
			t.Fatal("successful native observation was scheduled as polling", repository.progress)
		}
		if ready == nil && repository.progress.State != Available {
			t.Fatal("unreported native Ready changed stable state", repository.progress)
		}
		if ready != nil && !*ready && repository.progress.State != Degraded {
			t.Fatal("native False was not projected", repository.progress)
		}
	}
}

func TestImportedMissingOrReplacedOriginalNeverCreatesOrChangesProjection(t *testing.T) {
	for _, observation := range []ProviderObservation{{}, {Exists: true, Identity: "replacement"}} {
		repository := &earlyCompletionRepository{work: Work{PlatformNetwork: &PlatformNetworkWorkSpec{Kind: "vpc", UID: "original"}, Resource: ResourceWork{Kind: "vpc", State: Available, Reason: "last-native"}, KnownIdentity: "original"}}
		provider := &directObservationProvider{observation: observation}
		worker, err := NewWorker(repository, provider, uuid.NewString(), DefaultWorkerPolicy())
		if err != nil {
			t.Fatal(err)
		}
		if _, err = worker.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
		want := ProviderMissing
		if observation.Exists {
			want = ProviderOwnership
		}
		if repository.progress.State != Available || repository.progress.Reason != "last-native" || repository.progress.Observed || !repository.progress.Backoff || repository.progress.ObservationError != want {
			t.Fatal(repository.progress)
		}
		if provider.creates != 0 || provider.deletes != 0 || repository.mutations != 0 {
			t.Fatal("imported missing object was recreated")
		}
	}
}

func TestAcceptedAttachmentInternalFieldsDoNotChangePermanentReplay(t *testing.T) {
	original := PrepareAttachment{TenantID: uuid.NewString(), InstanceID: "vm-one", SubnetID: "subnet_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", VPCID: "vpc_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Slot: "primary", RequestKey: "original", SubmissionID: uuid.NewString(), Generation: 1, ClusterID: "cluster", Namespace: "tenant-one", WorkloadMode: "vm_managed", ConsumerType: ConsumerVirtualMachine}
	accepted := original
	accepted.Accepted = true
	accepted.NamespaceUID = "original-namespace-uid"
	accepted.AcceptedReference = NetworkReference{OwnerScope: OwnerScopePlatform, VPCID: original.VPCID, SubnetID: original.SubnetID, SubnetBindingID: uuid.NewString()}
	if original.Fingerprint() != accepted.Fingerprint() {
		t.Fatal("internal owner metadata changed the permanent request fingerprint")
	}
}

func TestNetworkCursorBindsOptionalCapabilityPresenceAndValue(t *testing.T) {
	n := &Network{cursorKey: make([]byte, 32)}
	no := false
	yes := true
	for _, canUse := range []*bool{nil, &no, &yes} {
		encoded := n.encodeCursor(vpcCursor{Version: 1, Kind: "subnet", CanUse: canUse})
		decoded, err := n.decodeCursor(encoded)
		if err != nil {
			t.Fatal(err)
		}
		if !sameOptionalBool(canUse, decoded.CanUse) {
			t.Fatal("capability presence lost")
		}
		for _, other := range []*bool{nil, &no, &yes} {
			if sameOptionalBool(canUse, other) != (n.encodeCursor(vpcCursor{Version: 1, Kind: "subnet", CanUse: other}) == encoded) {
				t.Fatal("different optional filters share a signed cursor")
			}
		}
	}
}
