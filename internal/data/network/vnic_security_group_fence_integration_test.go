package data_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	data "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network"
	"k8s.io/client-go/rest"
)

func TestManualVNicPGSecurityGroupVersionFence(t *testing.T) {
	f := newAttachmentFixture(t)
	ctx := context.Background()
	v := manualVNic(t, f, "sg-fence")
	consumer := "vm-policy-owner"
	if _, e := f.n.ReserveVNic(ctx, biz.VNicReservation{TenantID: v.TenantID, VNicID: v.ID, ConsumerKind: "vm", ConsumerID: consumer, Slot: "primary", OperationID: uuid.NewString(), SubnetID: v.SubnetID, Revision: 1}); e != nil {
		t.Fatal(e)
	}
	if _, e := f.p.SyncSecurityGroupBinding(ctx, v.TenantID, "vm", consumer, v.VPCID, v.Namespace, 4, []string{}); e != nil {
		t.Fatal(e)
	}
	var bumped atomic.Bool
	var patches atomic.Int32
	callbackErrors := make(chan error, 1)
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/vnics/"+v.ProviderName) && bumped.CompareAndSwap(false, true) {
			// A newer owner choice commits after the old worker's selection read but
			// before its provider send. This also proves no DB lock spans provider I/O.
			if _, e := f.p.SyncSecurityGroupBinding(ctx, v.TenantID, "vm", consumer, v.VPCID, v.Namespace, 5, []string{}); e != nil {
				callbackErrors <- e
				http.Error(w, "selection update failed", 500)
				return
			}
		}
		if r.Method == "PATCH" {
			patches.Add(1)
		}
		f.api.ServeHTTP(w, r)
	}))
	defer endpoint.Close()
	if _, e := data.NewKCProvider(f.p, &rest.Config{Host: endpoint.URL, QPS: 1000, Burst: 1000}); e != nil {
		t.Fatal(e)
	}
	if _, e := f.n.SyncVNicSecurityGroups(ctx, v.TenantID, v.ID, "vm", consumer, []string{}); biz.ReasonOf(e) != biz.VersionConflict {
		t.Fatal("old worker wrote after the owner selection version changed", e)
	}
	select {
	case e := <-callbackErrors:
		t.Fatal(e)
	default:
	}
	if !bumped.Load() || patches.Load() != 0 {
		t.Fatal("stale selection reached provider mutation", bumped.Load(), patches.Load())
	}
	if _, e := f.n.SyncVNicSecurityGroups(ctx, v.TenantID, v.ID, "vm", consumer, []string{"sg_11111111111111111111111111111111"}); biz.ReasonOf(e) != biz.VersionConflict {
		t.Fatal("late old nonempty policy overwrote the latest clear selection", e)
	}
	if patches.Load() != 0 {
		t.Fatal("late policy reached provider mutation")
	}
}

// The SGs are pre-existing fixtures. Real VNIC acceptance, reservation, owner
// selection, adapter confirmation and reference release run against PostgreSQL.
func TestManualVNicPGSecurityGroupMembershipIsApplicationReceipt(t *testing.T) {
	f := newAttachmentFixture(t)
	ctx := context.Background()
	v := manualVNic(t, f, "sg-receipt")
	consumer := "vm-membership-owner"
	if _, e := f.n.ReserveVNic(ctx, biz.VNicReservation{TenantID: v.TenantID, VNicID: v.ID, ConsumerKind: "vm", ConsumerID: consumer, Slot: "primary", OperationID: uuid.NewString(), SubnetID: v.SubnetID, Revision: 1}); e != nil {
		t.Fatal(e)
	}
	g := biz.SecurityGroup{EgressMetadata: biz.EgressMetadata{ID: "sg_" + strings.ReplaceAll(uuid.NewString(), "-", ""), Name: "receipt-group", Version: 1, State: biz.Available}, TenantID: v.TenantID, ClusterID: v.ClusterID, Namespace: v.Namespace, ProviderName: "ani-sg-receipt", ProviderUID: uuid.NewString(), BindingID: uuid.NewString()}
	payload, e := json.Marshal(g)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.owner.Exec(ctx, `INSERT INTO network_security_groups(tenant_id,security_group_id,version,state,payload) VALUES($1,$2,1,'available',$3)`, g.TenantID, g.ID, payload); e != nil {
		t.Fatal(e)
	}
	if _, e = f.p.SyncSecurityGroupBinding(ctx, v.TenantID, "vm", consumer, v.VPCID, v.Namespace, 4, []string{g.ID}); e != nil {
		t.Fatal(e)
	}
	var groupResponse atomic.Value
	setGroupStatus := func(members []string, observed int64, ready string) {
		o := map[string]any{"apiVersion": "networking.kubercloud.com/v1", "kind": "SecurityGroup", "metadata": map[string]any{"name": g.ProviderName, "namespace": g.Namespace, "uid": g.ProviderUID, "resourceVersion": "1", "generation": 1, "labels": map[string]any{"network.ani.io/managed-by": "ani-network-service", "network.ani.io/tenant-id": g.TenantID, "network.ani.io/resource-id": g.ID, "network.ani.io/binding-id": g.BindingID}}, "spec": map[string]any{"allowSameGroupTraffic": false}, "status": map[string]any{"observedGeneration": observed, "allowSameGroupTraffic": false, "appliedVNics": members, "conditions": []any{map[string]any{"type": "Valid", "status": "True", "observedGeneration": 1}, map[string]any{"type": "Ready", "status": ready, "observedGeneration": 1}}}}
		raw, e := json.Marshal(o)
		if e != nil {
			t.Fatal(e)
		}
		groupResponse.Store(raw)
	}
	setVNicSelection := func(names []any, version string) {
		f.api.Mu.Lock()
		defer f.api.Mu.Unlock()
		o := f.api.Objects["vnics/"+v.Namespace+"/"+v.ProviderName]
		o["spec"].(map[string]any)["securityGroups"] = names
		ann := o["metadata"].(map[string]any)["annotations"].(map[string]any)
		ann["network.ani.io/vnic-consumer"] = "vm:" + consumer
		ann["network.ani.io/vnic-security-groups-version"] = version
		delete(o["status"].(map[string]any), "appliedSecurityGroups") // KCN does not promise this receipt.
	}
	setVNicSelection([]any{g.ProviderName}, "4")
	setGroupStatus([]string{v.ProviderName}, 1, "True")
	var patches atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/securitygroups/"+g.ProviderName) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(groupResponse.Load().([]byte))
			return
		}
		if r.Method == "PATCH" {
			patches.Add(1)
		}
		f.api.ServeHTTP(w, r)
	}))
	defer endpoint.Close()
	if _, e = data.NewKCProvider(f.p, &rest.Config{Host: endpoint.URL, QPS: 1000, Burst: 1000}); e != nil {
		t.Fatal(e)
	}
	confirmed, e := f.n.SyncVNicSecurityGroups(ctx, v.TenantID, v.ID, "vm", consumer, []string{g.ID})
	if e != nil || len(confirmed.SecurityGroupIDs) != 1 || confirmed.SecurityGroupIDs[0] != g.ID {
		t.Fatal("SG appliedVNics did not confirm application without a VNIC status receipt", confirmed.SecurityGroupIDs, e)
	}
	expectPending := func(ids []string) {
		t.Helper()
		_, e := f.n.SyncVNicSecurityGroups(ctx, v.TenantID, v.ID, "vm", consumer, ids)
		var providerErr *biz.ProviderError
		if !errors.As(e, &providerErr) || providerErr.Kind != biz.ProviderTemporary {
			t.Fatal("unconfirmed SG application was accepted", e)
		}
	}
	setGroupStatus([]string{v.ProviderName}, 0, "True")
	if _, e = f.n.SyncVNicSecurityGroups(ctx, v.TenantID, v.ID, "vm", consumer, []string{g.ID}); e != nil {
		t.Fatal("membership receipt incorrectly waited for SG observedGeneration", e)
	}
	setGroupStatus([]string{v.ProviderName}, 1, "False")
	if _, e = f.n.SyncVNicSecurityGroups(ctx, v.TenantID, v.ID, "vm", consumer, []string{g.ID}); e != nil {
		t.Fatal("membership receipt incorrectly waited for SG Ready", e)
	}
	setGroupStatus([]string{}, 1, "True")
	expectPending([]string{g.ID})
	setGroupStatus([]string{v.ProviderName}, 1, "True")
	if _, e = f.p.SyncSecurityGroupBinding(ctx, v.TenantID, "vm", consumer, v.VPCID, v.Namespace, 5, []string{}); e != nil {
		t.Fatal(e)
	}
	setVNicSelection([]any{}, "5")
	expectPending([]string{}) // Old SG still contains this VNIC.
	setGroupStatus([]string{}, 0, "False")
	cleared, e := f.n.SyncVNicSecurityGroups(ctx, v.TenantID, v.ID, "vm", consumer, []string{})
	if e != nil || len(cleared.SecurityGroupIDs) != 0 {
		t.Fatal("old SG removal did not confirm clearing", cleared.SecurityGroupIDs, e)
	}
	if patches.Load() != 0 {
		t.Fatal("confirmation rewrote the already applied VNIC declaration", patches.Load())
	}
}
