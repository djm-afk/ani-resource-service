package data_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	data "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network"
	"k8s.io/apimachinery/pkg/runtime/schema"
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

// Configuration confirmation is local; application is projected later from
// the single current VNic's native appliedSecurityGroups field.
func TestManualVNicSGConfigurationAndNativeApplicationAreSeparate(t *testing.T) {
	f := newAttachmentFixture(t)
	ctx := context.Background()
	v := manualVNic(t, f, "sg-receipt")
	consumer := "vm-membership-owner"
	if _, err := f.n.ReserveVNic(ctx, biz.VNicReservation{TenantID: v.TenantID, VNicID: v.ID, ConsumerKind: "vm", ConsumerID: consumer, Slot: "primary", OperationID: uuid.NewString(), SubnetID: v.SubnetID, Revision: 1}); err != nil {
		t.Fatal(err)
	}
	group := biz.SecurityGroup{EgressMetadata: biz.EgressMetadata{ID: "sg_" + strings.ReplaceAll(uuid.NewString(), "-", ""), Name: "receipt-group", Version: 1, State: biz.Provisioning}, TenantID: v.TenantID, ClusterID: v.ClusterID, Namespace: v.Namespace, ProviderName: "ani-sg-receipt", ProviderUID: uuid.NewString(), BindingID: uuid.NewString()}
	payload, err := json.Marshal(group)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.owner.Exec(ctx, `INSERT INTO network_security_groups(tenant_id,security_group_id,version,state,payload) VALUES($1,$2,1,'provisioning',$3)`, group.TenantID, group.ID, payload); err != nil {
		t.Fatal(err)
	}
	if _, err = f.p.SyncSecurityGroupBinding(ctx, v.TenantID, "vm", consumer, v.VPCID, v.Namespace, 4, []string{group.ID}); err != nil {
		t.Fatal("nonready SG declaration blocked own configuration", err)
	}
	var reverseReads atomic.Int32
	var failRead atomic.Bool
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/securitygroups") {
			reverseReads.Add(1)
			http.Error(w, "unexpected reverse read", 500)
			return
		}
		if failRead.Load() && r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/vnics/"+v.ProviderName) {
			http.Error(w, "temporary read failure", 503)
			return
		}
		f.api.ServeHTTP(w, r)
	}))
	defer host.Close()
	provider, err := data.NewKCProvider(f.p, &rest.Config{Host: host.URL, QPS: 1000, Burst: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.n.SyncVNicSecurityGroups(ctx, v.TenantID, v.ID, "vm", consumer, []string{group.ID}); err != nil {
		t.Fatal("configuration waited for application", err)
	}
	read := func() biz.SecurityGroupBinding {
		t.Helper()
		var raw []byte
		if err := f.owner.QueryRow(ctx, `SELECT payload FROM network_security_group_targets WHERE tenant_id=$1 AND target_type='vnic' AND target_id=$2`, v.TenantID, v.ID).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var binding biz.SecurityGroupBinding
		if err := json.Unmarshal(raw, &binding); err != nil {
			t.Fatal(err)
		}
		return binding
	}
	configured := read()
	if configured.State != "configured" || len(configured.AppliedSecurityGroupIDs) != 0 || configured.ObservedAt != nil {
		t.Fatal("configuration fabricated application", configured)
	}
	f.api.Mu.Lock()
	object := f.api.Objects["vnics/"+v.Namespace+"/"+v.ProviderName]
	object["status"].(map[string]any)["appliedSecurityGroups"] = map[string]any{group.ProviderName: int64(1)}
	f.api.Mu.Unlock()
	worker := biz.NewSecurityGroupWorker(f.p, provider, uuid.NewString(), biz.DefaultWorkerPolicy())
	if found, err := worker.Step(ctx); err != nil || !found {
		t.Fatal(found, err)
	}
	applied := read()
	if applied.State != "applied" || len(applied.AppliedSecurityGroupIDs) != 1 || applied.AppliedSecurityGroupIDs[0] != group.ID || applied.ObservedAt == nil || reverseReads.Load() != 0 {
		t.Fatal("native VNic application was not projected", applied, reverseReads.Load())
	}
	failRead.Store(true)
	if err = f.p.NotifyObservedObject(ctx, schema.GroupVersionResource{Group: "networking.kubercloud.com", Version: "v1", Resource: "vnics"}, v.Namespace, v.ProviderName); err != nil {
		t.Fatal(err)
	}
	if found, err := worker.Step(ctx); err != nil || !found {
		t.Fatal(found, err)
	}
	after := read()
	if after.State != applied.State || after.Reason != applied.Reason || !slices.Equal(after.AppliedSecurityGroupIDs, applied.AppliedSecurityGroupIDs) || !after.ObservedAt.Equal(*applied.ObservedAt) {
		t.Fatal("read error rewrote native application", after, applied)
	}
}
