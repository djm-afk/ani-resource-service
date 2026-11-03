package data_test

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	data "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network"
	controlled "github.com/zhangzhe-ctrl/ani-resource-service/tests/net05a/provider"
	"k8s.io/client-go/rest"
)

func TestManualVNicPGSharedAuditAfterNotebookRecreation(t *testing.T) {
	f := newAttachmentFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	v := manualVNic(t, f, "shared-recreation")
	if _, err := f.n.ReserveVNic(ctx, biz.VNicReservation{TenantID: v.TenantID, VNicID: v.ID, ConsumerKind: "container", ConsumerID: f.r.InstanceID, Slot: "primary", OperationID: uuid.NewString(), SubnetID: v.SubnetID, Revision: 4}); err != nil {
		t.Fatal(err)
	}
	f.r.WorkloadMode = "controller_managed"
	a := f.prepare(t) // Stopped/recreated attachment has no current Pod name yet.
	c := managedConsumerFor(a, "new-pod", 9)
	oldSet := c.Controllers[0]
	c.Controllers[0].Reference.UID = "new-set"
	c.CurrentPods[0].Owner = c.Controllers[0].Reference
	c.Controllers = append(c.Controllers, oldSet)
	c.RetiredPods = []biz.PodReference{{Reference: biz.WorkloadReference{APIVersion: "v1", Kind: "Pod", Namespace: a.Namespace, Name: "notebook-0", UID: "old-pod"}, Owner: oldSet.Reference}}
	c.ManualVNics = []biz.ManualVNicReference{{VNicID: v.ID, ProviderName: v.ProviderName, ProviderUID: v.ProviderUID}}
	f.api.Mu.Lock()
	managedControllerObjects(f, a, c)
	set := f.api.Objects["statefulsets/"+a.Namespace+"/notebook"]
	set["metadata"].(map[string]any)["uid"] = "new-set"
	pod := managedPodObject(a, "new-pod")
	pod["metadata"].(map[string]any)["ownerReferences"].([]any)[0].(map[string]any)["uid"] = "new-set"
	pod["metadata"].(map[string]any)["annotations"].(map[string]any)["networking.kubercloud.com/vnic"] = v.ProviderName
	pod["status"].(map[string]any)["podIP"] = v.PrimaryIP
	f.api.Objects["pods/"+a.Namespace+"/notebook-0"] = pod
	manual := f.api.Objects["vnics/"+a.Namespace+"/"+v.ProviderName]
	manual["metadata"].(map[string]any)["annotations"].(map[string]any)["pod.networking.kubercloud.com/notebook-0"] = "node-one/eth0"
	f.api.Objects["vnicips/"+a.Namespace+"/manual-ip"] = map[string]any{"apiVersion": "networking.kubercloud.com/v1", "kind": "VNicIP", "metadata": map[string]any{"name": "manual-ip", "namespace": a.Namespace, "uid": "manual-ip-uid", "resourceVersion": "1", "ownerReferences": []any{map[string]any{"apiVersion": "networking.kubercloud.com/v1", "kind": "VNic", "name": v.ProviderName, "uid": v.ProviderUID}}}, "spec": map[string]any{"subnet": a.Plan.PrimaryNetworkRef, "vNic": v.ProviderName, "ipAddress": v.PrimaryIP}, "status": map[string]any{"vNic": v.ProviderName}}
	f.api.Mu.Unlock()
	work := biz.AttachmentWork{Attachment: a, Plan: *a.Plan, ConsumerSnapshot: &c, ConsumerPodUIDs: []string{"new-pod", "old-pod"}, Relations: []byte("[]")}
	direct, err := f.provider.ObserveAttachment(ctx, work)
	if err != nil || !direct.VerifiedCurrent || direct.PodUID != "new-pod" {
		t.Fatalf("independent provider identity/address proof failed: %+v %v", direct, err)
	}
	api := controlled.New()
	api.Backend = f.api
	host := httptest.NewServer(api)
	t.Cleanup(host.Close)
	provider, err := data.NewKCProvider(f.p, &rest.Config{Host: host.URL, QPS: 1000, Burst: 1000})
	if err != nil {
		t.Fatal(err)
	}
	startSlowAudit(t, provider)
	shared, err := provider.ObserveAttachment(ctx, work)
	if err != nil || !shared.VerifiedCurrent || shared.PodUID != direct.PodUID {
		t.Fatalf("shared audit omitted accepted manual VNIC after same-name controller recreation: %+v %v", shared, err)
	}
}
