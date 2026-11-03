package data_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
)

func TestVMBlockedAttachmentPGReleasesRetainedManualIP(t *testing.T) {
	f, a, c, kc, _ := newVMRelationAudit(t)
	ctx := context.Background()
	v := manualVNic(t, f, "retained-on-release")
	if _, err := f.n.ReserveVNic(ctx, biz.VNicReservation{TenantID: v.TenantID, VNicID: v.ID, ConsumerKind: "vm", ConsumerID: a.InstanceID, Slot: "primary", OperationID: uuid.NewString(), SubnetID: v.SubnetID, Revision: 1}); err != nil {
		t.Fatal(err)
	}
	c.ManualVNics = []biz.ManualVNicReference{{VNicID: v.ID, ProviderName: v.ProviderName, ProviderUID: v.ProviderUID}}
	f.api.Mu.Lock()
	f.api.Objects["vnicips/"+a.Namespace+"/manual-ip"] = map[string]any{"apiVersion": "networking.kubercloud.com/v1", "kind": "VNicIP", "metadata": map[string]any{"name": "manual-ip", "namespace": a.Namespace, "uid": "manual-ip-uid", "resourceVersion": "1", "ownerReferences": []any{map[string]any{"apiVersion": "networking.kubercloud.com/v1", "kind": "VNic", "name": v.ProviderName, "uid": v.ProviderUID}}}, "spec": map[string]any{"subnet": a.VMPrimary.KCNSubnetRef, "vNic": v.ProviderName, "ipAddress": v.PrimaryIP}, "status": map[string]any{"vNic": v.ProviderName}}
	f.api.Mu.Unlock()
	provider := &vmClosureProvider{provider: kc, conflict: true}
	policy := biz.DefaultWorkerPolicy()
	policy.ObserveEvery, policy.RetryMin = time.Millisecond, time.Millisecond
	var finishErr error
	var err error
	f.worker, err = biz.NewAttachmentWorker(f.p, provider, f.consumer, uuid.NewString(), policy, func(_ context.Context, _ biz.AttachmentWork, _ biz.AttachmentProgress, err error) { finishErr = err })
	if err != nil {
		t.Fatal(err)
	}
	setManagedConsumer(f, c)
	a = f.step(t, a.ID)
	if !attachmentProtocolBlocked(t, f, a.ID) {
		t.Fatal("missing durable provider fence")
	}
	closed := time.Now().UTC()
	c.SnapshotRevision++
	c.State, c.DesiredRunState, c.FinalizationID, c.ClosedAt = "closed", "deleted", uuid.NewString(), &closed
	c.RetiredPods, c.CurrentPods = c.CurrentPods, nil
	setManagedConsumer(f, c)
	provider.conflict = false
	a = f.step(t, a.ID)
	if a.State != biz.Releasing || a.SnapshotRevision != c.SnapshotRevision {
		t.Fatal("accepted closed high water was not retained", a)
	}
	a = f.step(t, a.ID)
	if finishErr != nil || a.State != biz.Released || a.Reason != "" || attachmentProtocolBlocked(t, f, a.ID) {
		t.Fatalf("retained manual VNIC/IP prevented fresh closed-consumer release: state=%s reason=%s finish=%v", a.State, a.Reason, finishErr)
	}
	var raw []byte
	if err = f.owner.QueryRow(ctx, `SELECT provider_relations FROM network_attachments WHERE tenant_id=$1 AND attachment_id=$2`, a.TenantID, a.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var relations []map[string]any
	if err = json.Unmarshal(raw, &relations); err != nil {
		t.Fatal(err)
	}
	retained := false
	for _, relation := range relations {
		if relation["UID"] == "manual-ip-uid" {
			retained = true
			if relation["Current"] == true || relation["AddressEligible"] == true {
				t.Fatal("retained manual IP still authorizes an exited endpoint", relation)
			}
		}
	}
	if !retained {
		t.Fatal("release discarded the witnessed manual IP identity")
	}
	f.api.Mu.Lock()
	defer f.api.Mu.Unlock()
	if f.api.Objects["vnics/"+a.Namespace+"/"+v.ProviderName] == nil || f.api.Objects["vnicips/"+a.Namespace+"/manual-ip"] == nil || len(f.api.Deletes) != 0 {
		t.Fatal("attachment release removed an independent manual resource")
	}
}
