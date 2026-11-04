package data_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	data "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network"
	controlled "github.com/zhangzhe-ctrl/ani-resource-service/tests/net05a/provider"
	"k8s.io/client-go/rest"
)

func TestVMReservedAttachmentReusesSharedRelationAudit(t *testing.T) {
	// Persist a real VM reservation and its managed NAD/Namespace identities.
	// Only Kubernetes is controlled; the actual provider observes the reservation.
	f, _, _ := newAutoVMNetwork(t)
	policy := awaitAutoVMNetwork(t, f)
	f.r.WorkloadMode = "vm_managed"
	f.r.ConsumerType = biz.ConsumerVirtualMachine
	f.r.VMExpectation = &biz.VMNetworkExpectation{NADName: policy.NADName, NADUID: policy.NADUID, NADConfigDigest: policy.NADConfigDigest}
	a := f.prepare(t)
	api := controlled.New()
	api.Backend = f.api
	host := httptest.NewServer(api)
	t.Cleanup(host.Close)
	provider, err := data.NewKCProvider(f.p, &rest.Config{Host: host.URL, QPS: 1000, Burst: 1000})
	if err != nil {
		t.Fatal(err)
	}
	startSlowAudit(t, provider)
	work := biz.AttachmentWork{Attachment: a, Relations: []byte("[]"), Plan: biz.PodPrimaryPlan{PrimaryNetworkRef: a.VMPrimary.KCNSubnetRef, VMPrimary: a.VMPrimary}}
	first, err := provider.ObserveAttachment(context.Background(), work)
	if err != nil || first.Exists || first.HasDependencies || first.Proof.CollectedAt.IsZero() {
		t.Fatalf("reserved VM relation observation: %+v %v", first, err)
	}
	before, _ := api.Counts()
	for range 3 {
		next, err := provider.ObserveAttachment(context.Background(), work)
		if err != nil || next.Exists || next.HasDependencies {
			t.Fatalf("stable reserved VM relation observation: %+v %v", next, err)
		}
	}
	after, _ := api.Counts()
	for _, gvr := range []string{"pods", "vnics", "vnicips"} {
		if after["LIST/"+gvr] != before["LIST/"+gvr] {
			t.Fatalf("VM observation repeated a full relation scan: %s %d -> %d", gvr, before["LIST/"+gvr], after["LIST/"+gvr])
		}
	}
	last, err := provider.ObserveAttachment(context.Background(), work)
	if err != nil || !last.Proof.CollectedAt.Equal(first.Proof.CollectedAt) {
		t.Fatalf("reusing VM relations renewed their collection time: first=%s next=%s err=%v", first.Proof.CollectedAt, last.Proof.CollectedAt, err)
	}
}

func newVMRelationAudit(t *testing.T) (attachmentFixture, biz.Attachment, biz.ConsumerSubmission, *data.KCProvider, *controlled.Server) {
	t.Helper()
	f, managedAPI, _ := newAutoVMNetwork(t)
	policy := awaitAutoVMNetwork(t, f)
	f.r.WorkloadMode = "vm_managed"
	f.r.ConsumerType = biz.ConsumerVirtualMachine
	f.r.VMExpectation = &biz.VMNetworkExpectation{NADName: policy.NADName, NADUID: policy.NADUID, NADConfigDigest: policy.NADConfigDigest}
	a := f.prepare(t)
	api := controlled.New()
	api.Backend = f.api
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/apis/kubevirt.io/") {
			gvr := "virtualmachines"
			if strings.Contains(r.URL.Path, "/virtualmachineinstances/") {
				gvr = "virtualmachineinstances"
			}
			name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			object := api.Object(gvr, a.Namespace, name)
			w.Header().Set("Content-Type", "application/json")
			if object == nil {
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": "v1", "kind": "Status", "reason": "NotFound", "code": 404})
			} else {
				_ = json.NewEncoder(w).Encode(object)
			}
			return
		}
		if strings.Contains(r.URL.Path, "/network-attachment-definitions") || strings.Contains(r.URL.Path, "/serviceaccounts") {
			managedAPI.ServeHTTP(w, r)
			return
		}
		api.ServeHTTP(w, r)
	}))
	t.Cleanup(host.Close)
	provider, err := data.NewKCProvider(f.p, &rest.Config{Host: host.URL, QPS: 1000, Burst: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if err = provider.ConfigureVMNetworks([]data.ManagedVMNAD{policy}); err != nil {
		t.Fatal(err)
	}
	return f, a, vmConsumerFor(a), provider, api
}

func seedVMLauncher(api *controlled.Server, a biz.Attachment, c biz.ConsumerSubmission) {
	root, vmi, pod := c.RootController, c.Controllers[0].Reference, c.CurrentPods[0].Reference
	labels := map[string]any{"network.ani.io/tenant-id": a.TenantID, "network.ani.io/instance-id": a.InstanceID, "network.ani.io/attachment-id": a.ID, "network.ani.io/submission-id": a.SubmissionID, "network.ani.io/generation": "1"}
	metadata := func(ref biz.WorkloadReference) map[string]any {
		return map[string]any{"name": ref.Name, "namespace": ref.Namespace, "uid": ref.UID, "labels": labels, "annotations": map[string]any{"networking.kubercloud.com/subnet": a.VMPrimary.KCNSubnetRef}}
	}
	owner := func(ref biz.WorkloadReference) []any {
		return []any{map[string]any{"apiVersion": ref.APIVersion, "kind": ref.Kind, "name": ref.Name, "uid": ref.UID, "controller": true}}
	}
	nad := a.Namespace + "/" + a.VMPrimary.NADName
	spec := map[string]any{"networks": []any{map[string]any{"name": "default", "multus": map[string]any{"default": true, "networkName": nad}}}, "domain": map[string]any{"devices": map[string]any{"interfaces": []any{map[string]any{"name": "default", "bridge": map[string]any{}}}}}}
	api.Change("virtualmachines", map[string]any{"apiVersion": root.APIVersion, "kind": root.Kind, "metadata": metadata(root), "spec": map[string]any{"template": map[string]any{"metadata": map[string]any{"labels": labels, "annotations": map[string]any{"networking.kubercloud.com/subnet": a.VMPrimary.KCNSubnetRef}}, "spec": spec}}}, false)
	vmiMeta := metadata(vmi)
	vmiMeta["ownerReferences"] = owner(root)
	api.Change("virtualmachineinstances", map[string]any{"apiVersion": vmi.APIVersion, "kind": vmi.Kind, "metadata": vmiMeta, "spec": spec, "status": map[string]any{"phase": "Running", "interfaces": []any{map[string]any{"name": "default", "ipAddress": "10.42.1.2"}}}}, false)
	podMeta := metadata(pod)
	podMeta["ownerReferences"] = owner(vmi)
	podMeta["labels"] = map[string]any{"network.ani.io/tenant-id": a.TenantID, "network.ani.io/instance-id": a.InstanceID, "network.ani.io/attachment-id": a.ID, "network.ani.io/submission-id": a.SubmissionID, "network.ani.io/generation": "1", "kubevirt.io": "virt-launcher"}
	podMeta["annotations"] = map[string]any{"networking.kubercloud.com/subnet": a.VMPrimary.KCNSubnetRef, "k8s.v1.cni.cncf.io/network-status": `[{"name":"` + nad + `","default":true,"interface":"eth0"}]`}
	api.Change("pods", map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": podMeta}, false)
	vnic := biz.WorkloadReference{APIVersion: "networking.kubercloud.com/v1", Kind: "VNic", Namespace: a.Namespace, Name: "vm-nic", UID: "vnic-uid"}
	vnicMeta := metadata(vnic)
	vnicMeta["ownerReferences"] = owner(pod)
	api.Change("vnics", map[string]any{"apiVersion": vnic.APIVersion, "kind": vnic.Kind, "metadata": vnicMeta, "spec": map[string]any{"type": "VETH", "subnet": a.VMPrimary.KCNSubnetRef}}, false)
	ip := biz.WorkloadReference{APIVersion: "networking.kubercloud.com/v1", Kind: "VNicIP", Namespace: a.Namespace, Name: "vm-ip", UID: "ip-uid"}
	ipMeta := metadata(ip)
	ipMeta["ownerReferences"] = owner(vnic)
	api.Change("vnicips", map[string]any{"apiVersion": ip.APIVersion, "kind": ip.Kind, "metadata": ipMeta, "spec": map[string]any{"subnet": a.VMPrimary.KCNSubnetRef, "vNic": "vm-nic", "ipAddress": "10.42.1.2"}, "status": map[string]any{"vNic": "vm-nic"}}, false)
}

func TestVMLiveSharedAuditKeepsNativeIdentityAndVMIProof(t *testing.T) {
	_, a, consumer, provider, api := newVMRelationAudit(t)
	seedVMLauncher(api, a, consumer)
	startSlowAudit(t, provider)
	work := biz.AttachmentWork{Attachment: a, Relations: []byte("[]"), ConsumerSnapshot: &consumer, Plan: biz.PodPrimaryPlan{PrimaryNetworkRef: a.VMPrimary.KCNSubnetRef, VMPrimary: a.VMPrimary}}
	first, err := provider.ObserveAttachment(context.Background(), work)
	if err != nil || !first.VerifiedCurrent || first.PodUID != consumer.CurrentPods[0].Reference.UID {
		t.Fatalf("real VM owner/address chain was not verified: %+v %v", first, err)
	}
	before, _ := api.Counts()
	next, err := provider.ObserveAttachment(context.Background(), work)
	if err != nil || !next.VerifiedCurrent || next.Proof.Hash != first.Proof.Hash || !next.Proof.CollectedAt.Equal(first.Proof.CollectedAt) {
		t.Fatalf("unchanged live VM facts were not reused: %+v %v", next, err)
	}
	after, _ := api.Counts()
	for _, gvr := range []string{"pods", "vnics", "vnicips"} {
		if before["LIST/"+gvr] != after["LIST/"+gvr] {
			t.Fatalf("live VM repeated a full relation scan: %s", gvr)
		}
	}
	// VMI is directly verified and does not emit an event into the shared GVRs.
	// A changed VMI fact must not reuse the same-content exception for an audit
	// older than the previously applied fact.
	applied := time.Now()
	vmi := api.Object("virtualmachineinstances", a.Namespace, consumer.Controllers[0].Reference.Name)
	vmi["status"].(map[string]any)["phase"] = "Succeeded"
	api.Change("virtualmachineinstances", vmi, false)
	work.Requirement = biz.ObservationRequirement{AppliedAt: applied, Hash: first.Proof.Hash}
	next, err = provider.ObserveAttachment(context.Background(), work)
	if err != nil || next.VerifiedCurrent || next.Proof.Hash == first.Proof.Hash || next.Proof.CollectedAt.Before(applied) {
		t.Fatalf("VMI-only change reused an old VM proof: %+v %v", next, err)
	}
	// Precise native identity checks remain active even when relation lists are
	// reused. A same-name VMI cannot replace the frozen owner UID.
	vmi["metadata"].(map[string]any)["uid"] = "replacement-vmi"
	api.Change("virtualmachineinstances", vmi, false)
	_, err = provider.ObserveAttachment(context.Background(), work)
	var conflict *biz.ProviderError
	if !errors.As(err, &conflict) || conflict.Kind != biz.ProviderConflict {
		t.Fatalf("shared VM relations accepted a replacement native VMI: %v", err)
	}
}

func TestVMConsumerControllerWakesAndRejectsUnlabelledLauncher(t *testing.T) {
	f, a, consumer, provider, api := newVMRelationAudit(t)
	setManagedConsumer(f, consumer)
	policy := biz.DefaultWorkerPolicy()
	policy.ObserveEvery = time.Hour
	var err error
	f.worker, err = biz.NewAttachmentWorker(f.p, provider, f.consumer, uuid.NewString(), policy)
	if err != nil {
		t.Fatal(err)
	}
	// The real worker persists the owner's snapshot before a launcher exists.
	// No Provider relations or current Pod names have yet been witnessed.
	a = f.step(t, a.ID)
	if a.ConsumerSnapshot == nil || a.PodUID != "" || a.State != biz.Reserved {
		t.Fatalf("owner-only VM snapshot was not durably accepted: %+v", a)
	}
	observer := startSlowAudit(t, provider)
	awaitNET05A(t, 3*time.Second, func() bool {
		return observer.Snapshot()["notifications_total"] > 0 && observer.Snapshot()["pending_keys"] == 0
	})
	a = f.step(t, a.ID)
	if _, found, err := f.p.ClaimAttachment(context.Background(), uuid.NewString(), time.Minute); err != nil || found {
		t.Fatalf("stable VM was not scheduled outside the event test window: found=%v err=%v", found, err)
	}
	controller := consumer.Controllers[0].Reference
	api.Change("pods", map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{"namespace": a.Namespace, "name": "unknown-launcher", "uid": "unknown-pod", "ownerReferences": []any{map[string]any{"apiVersion": controller.APIVersion, "kind": controller.Kind, "name": controller.Name, "uid": controller.UID, "controller": true}}}}, false)
	var work biz.AttachmentWork
	awaitNET05A(t, 3*time.Second, func() bool {
		var found bool
		work, found, err = f.p.ClaimAttachment(context.Background(), uuid.NewString(), time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		return found
	})
	work.ConsumerSnapshot = a.ConsumerSnapshot
	_, err = provider.ObserveAttachment(context.Background(), work)
	var conflict *biz.ProviderError
	if !errors.As(err, &conflict) || conflict.Kind != biz.ProviderConflict {
		t.Fatalf("unlabelled VMI-owned launcher was hidden by shared candidate selection: %v", err)
	}
}

func TestStoppedVMReservationParksAndReusesUnchangedAudit(t *testing.T) {
	f, a, consumer, provider, api := newVMRelationAudit(t)
	seedVMLauncher(api, a, consumer)
	setManagedConsumer(f, consumer)
	policy := biz.DefaultWorkerPolicy()
	policy.ObserveEvery = time.Millisecond
	var err error
	f.worker, err = biz.NewAttachmentWorker(f.p, provider, f.consumer, uuid.NewString(), policy)
	if err != nil {
		t.Fatal(err)
	}
	a = f.step(t, a.ID)
	if a.State != biz.Attached {
		t.Fatal("initial VM did not attach", a)
	}
	for _, item := range []struct{ kind, name string }{{"virtualmachineinstances", "vm"}, {"pods", "launcher"}, {"vnics", "vm-nic"}, {"vnicips", "vm-ip"}} {
		api.Change(item.kind, api.Object(item.kind, a.Namespace, item.name), true)
	}
	consumer.SnapshotRevision++
	consumer.DesiredRunState = "stopped"
	consumer.RetiredPods, consumer.CurrentPods = consumer.CurrentPods, nil
	setManagedConsumer(f, consumer)
	startSlowAudit(t, provider)
	initial := biz.AttachmentWork{Attachment: a, Relations: []byte("[]"), ConsumerSnapshot: &consumer, RequireFreshRelations: true, Plan: biz.PodPrimaryPlan{PrimaryNetworkRef: a.VMPrimary.KCNSubnetRef, VMPrimary: a.VMPrimary}}
	if _, err = provider.ObserveAttachment(context.Background(), initial); err != nil {
		t.Fatal("stopped VM initial audit", err)
	}
	if err = f.p.NotifyAttachment(context.Background(), a.TenantID, a.ID); err != nil {
		t.Fatal(err)
	}
	a = f.step(t, a.ID)
	var parked bool
	if err = f.owner.QueryRow(context.Background(), "SELECT next_check_at='infinity'::timestamptz FROM network_attachments WHERE tenant_id=$1 AND attachment_id=$2", a.TenantID, a.ID).Scan(&parked); err != nil || !parked || a.State != biz.Reserved || a.Reason != "" || len(a.VMCurrentAddresses) != 0 {
		t.Fatalf("stopped VM kept a per-object timer or an address: parked=%v attachment=%+v err=%v", parked, a, err)
	}
	work := biz.AttachmentWork{Attachment: a, Relations: []byte("[]"), ConsumerSnapshot: &consumer, Plan: biz.PodPrimaryPlan{PrimaryNetworkRef: a.VMPrimary.KCNSubnetRef, VMPrimary: a.VMPrimary}}
	first, err := provider.ObserveAttachment(context.Background(), work)
	if err != nil || first.Exists || first.HasDependencies {
		t.Fatalf("stopped VM facts: %+v %v", first, err)
	}
	before, _ := api.Counts()
	work.Requirement.RequestedGeneration = first.Proof.CoveredGeneration + 1
	next, err := provider.ObserveAttachment(context.Background(), work)
	if err != nil || !next.Proof.CollectedAt.Equal(first.Proof.CollectedAt) || next.Proof.CoveredGeneration != work.Requirement.RequestedGeneration {
		t.Fatalf("stopped VM hint renewed or starved unchanged evidence: %+v %v", next, err)
	}
	after, _ := api.Counts()
	if before["LIST/pods"] != after["LIST/pods"] {
		t.Fatal("stopped VM forced another complete audit")
	}
	// Final closure remains a post-owner-response proof even for a parked VM.
	work.RequireFreshRelations = true
	work.Requirement.RequestedGeneration = 0
	closed, err := provider.ObserveAttachment(context.Background(), work)
	if err != nil || !closed.Proof.CollectedAt.After(first.Proof.CollectedAt) {
		t.Fatalf("closure reused the stopped projection: %+v %v", closed, err)
	}
}
