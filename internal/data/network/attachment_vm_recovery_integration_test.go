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
	"k8s.io/client-go/rest"
)

type vmClosureProvider struct {
	provider        *data.KCProvider
	conflict, stale bool
}

func (p *vmClosureProvider) ObserveAttachment(ctx context.Context, w biz.AttachmentWork) (biz.AttachmentObservation, error) {
	if p.conflict {
		return biz.AttachmentObservation{}, &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	o, err := p.provider.ObserveAttachment(ctx, w)
	if p.stale {
		o.Proof.CollectedAt = w.Now.Add(-time.Hour)
		o.Proof.Hash = w.Requirement.Hash
	}
	return o, err
}

func TestVMBlockedAttachmentPGClosesWithFrozenHistoricalNADAndFreshAbsence(t *testing.T) {
	f := newAttachmentFixture(t)
	f.r.WorkloadMode = "vm_managed"
	f.r.ConsumerType = biz.ConsumerVirtualMachine
	const historical = `{"cniVersion":"0.3.1","name":"kc-networking","plugins":[{"type":"kc-networking","server_socket":"/run/kcn.sock"}]}`
	digest, err := data.VMNADConfigDigest(historical)
	if err != nil {
		t.Fatal(err)
	}
	f.r.VMExpectation = &biz.VMNetworkExpectation{NADName: "main", NADUID: "nad-uid", NADConfigDigest: digest}
	// This external resolver seeds a pre-upgrade accepted plan. New admission
	// separately rejects this config; the real KC provider must still verify
	// the historical frozen UID/digest while observing its normal deletion.
	if err = f.p.SetVMNetworkResolver(&vmNADBoundary{}, time.Minute); err != nil {
		t.Fatal(err)
	}
	a := f.prepare(t)
	f.api.Mu.Lock()
	namespaceUID := f.api.Objects["namespaces//"+a.Namespace]["metadata"].(map[string]any)["uid"].(string)
	f.api.Mu.Unlock()
	currentUID, currentConfig := "nad-uid", historical
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/network-attachment-definitions/main"):
			_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": "k8s.cni.cncf.io/v1", "kind": "NetworkAttachmentDefinition", "metadata": map[string]any{"name": "main", "namespace": a.Namespace, "uid": currentUID}, "spec": map[string]any{"config": currentConfig}})
		case strings.HasPrefix(r.URL.Path, "/apis/kubevirt.io/"):
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": "v1", "kind": "Status", "reason": "NotFound", "code": 404})
		default:
			f.api.ServeHTTP(w, r)
		}
	}))
	defer server.Close()
	kc, err := data.NewKCProvider(f.p, &rest.Config{Host: server.URL, QPS: 1000, Burst: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if err = kc.ConfigureVMNetworks([]data.ManagedVMNAD{{TenantID: a.TenantID, VPCID: a.VPCID, SubnetID: a.SubnetID, ClusterID: a.ClusterID, Namespace: a.Namespace, NamespaceUID: namespaceUID, NADName: "main", NADUID: "nad-uid", NADConfigDigest: digest, KCNSubnetRef: a.VMPrimary.KCNSubnetRef}}); err != nil {
		t.Fatal(err)
	}
	c := vmConsumerFor(a)
	setManagedConsumer(f, c)
	provider := &vmClosureProvider{provider: kc, conflict: true}
	policy := biz.DefaultWorkerPolicy()
	policy.ObserveEvery = 5 * time.Millisecond
	policy.RetryMin = time.Millisecond
	f.worker, err = biz.NewAttachmentWorker(f.p, provider, f.consumer, uuid.NewString(), policy)
	if err != nil {
		t.Fatal(err)
	}
	a = f.step(t, a.ID)
	if !attachmentProtocolBlocked(t, f, a.ID) {
		t.Fatal("historical provider conflict did not fence attachment")
	}
	closed := time.Now()
	c.SnapshotRevision++
	c.State = "closed"
	c.DesiredRunState = "deleted"
	c.FinalizationID = uuid.NewString()
	c.ClosedAt = &closed
	c.RetiredPods, c.CurrentPods = c.CurrentPods, nil
	setManagedConsumer(f, c)
	provider.conflict = false
	a = f.step(t, a.ID)
	if a.State != biz.Releasing || a.SnapshotRevision != c.SnapshotRevision || !attachmentProtocolBlocked(t, f, a.ID) {
		t.Fatal("closed high water was not persisted before recovery", a)
	}
	time.Sleep(6 * time.Millisecond)
	staleWork, found, err := f.p.ClaimAttachment(context.Background(), uuid.NewString(), time.Millisecond)
	if err != nil || !found {
		t.Fatal("claim historical closed attachment", found, err)
	}
	time.Sleep(2 * time.Millisecond)
	for _, cause := range []string{"wrong-uid", "changed-config", "stale-proof"} {
		switch cause {
		case "wrong-uid":
			currentUID = "replacement"
		case "changed-config":
			currentConfig = `{"changed":true}`
		case "stale-proof":
			provider.stale = true
		}
		a = f.step(t, a.ID)
		if a.State == biz.Released || !attachmentProtocolBlocked(t, f, a.ID) {
			t.Fatal("closure cleared without exact frozen NAD and fresh collection", cause, a)
		}
		currentUID, currentConfig, provider.stale = "nad-uid", historical, false
	}
	staleProgress := biz.AttachmentProgress{State: biz.Released, FinalizationID: c.FinalizationID, ConsumerSnapshot: &c, Observed: true, ProtocolRecovered: true, Relations: []byte("[]"), Proof: biz.ObservationProof{CollectedAt: time.Now(), CoveredGeneration: staleWork.Requirement.RequestedGeneration}}
	if err = f.p.FinishAttachment(context.Background(), staleWork, staleProgress); !errors.Is(err, biz.ErrLeaseLost) {
		t.Fatal("stale attachment version/lease could clear protocol fence", err)
	}
	if !attachmentProtocolBlocked(t, f, a.ID) {
		t.Fatal("stale completion changed the durable fence")
	}
	a = f.step(t, a.ID)
	if a.State != biz.Released || a.Reason != "" || attachmentProtocolBlocked(t, f, a.ID) {
		t.Fatal("accepted closed VM with unchanged historical NAD and fresh absence remained fenced", a)
	}
}
