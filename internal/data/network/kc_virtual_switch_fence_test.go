package data

import (
	"context"
	"encoding/json"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	jsonpatch "gopkg.in/evanphx/json-patch.v4"
	"io"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"
)

// Only the Kubernetes HTTP boundary is replaced. It applies actual JSON Patch
// tests atomically, increments RVs, and can delay a request past client timeout.
type switchFenceAPI struct {
	mu          sync.Mutex
	node        *unstructured.Unstructured
	writes      int
	loseWrite   int
	holdCreate  bool
	entered     chan struct{}
	release     chan struct{}
	lateDone    chan error
	releaseOnce sync.Once
}

func (a *switchFenceAPI) snapshot() *unstructured.Unstructured {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.node.DeepCopy()
}
func (a *switchFenceAPI) apply(raw []byte) error {
	patch, err := jsonpatch.DecodePatch(raw)
	if err != nil {
		return err
	}
	old, _ := json.Marshal(a.node.Object)
	body, err := patch.Apply(old)
	if err != nil {
		return err
	}
	var object map[string]any
	if err = json.Unmarshal(body, &object); err != nil {
		return err
	}
	next := &unstructured.Unstructured{Object: object}
	rv, _ := strconv.Atoi(a.node.GetResourceVersion())
	next.SetResourceVersion(strconv.Itoa(rv + 1))
	a.node = next
	return nil
}
func (a *switchFenceAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodGet {
		_ = json.NewEncoder(w).Encode(a.snapshot().Object)
		return
	}
	if r.Method != http.MethodPatch {
		http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
		return
	}
	raw, _ := io.ReadAll(r.Body)
	a.mu.Lock()
	hold := a.holdCreate
	if hold {
		a.holdCreate = false
	}
	a.mu.Unlock()
	if hold {
		close(a.entered)
		<-a.release
		a.mu.Lock()
		err := a.apply(raw)
		a.mu.Unlock()
		a.lateDone <- err
		w.WriteHeader(http.StatusConflict)
		return
	}
	a.mu.Lock()
	a.writes++
	err := a.apply(raw)
	lose := a.writes == a.loseWrite
	node := a.node.DeepCopy()
	a.mu.Unlock()
	if err != nil {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{"kind": "Status", "apiVersion": "v1", "status": "Failure", "reason": "Conflict", "code": 409})
		return
	}
	if lose {
		conn, _, _ := w.(http.Hijacker).Hijack()
		_ = conn.Close()
		return
	}
	_ = json.NewEncoder(w).Encode(node.Object)
}
func newSwitchFenceAPI(t *testing.T, node *unstructured.Unstructured, loseWrite int, hold bool) (*KCProvider, *switchFenceAPI) {
	t.Helper()
	api := &switchFenceAPI{node: node.DeepCopy(), loseWrite: loseWrite, holdCreate: hold, entered: make(chan struct{}), release: make(chan struct{}), lateDone: make(chan error, 1)}
	host := httptest.NewServer(api)
	t.Cleanup(host.Close)
	t.Cleanup(func() { api.releaseOnce.Do(func() { close(api.release) }) })
	client, err := dynamic.NewForConfig(&rest.Config{Host: host.URL, QPS: 1000, Burst: 1000})
	if err != nil {
		t.Fatal(err)
	}
	return &KCProvider{client: client}, api
}
func fenceFixture(t *testing.T) (biz.VirtualSwitch, biz.VirtualSwitchNode, *unstructured.Unstructured) {
	t.Helper()
	sw, nodes, _, _, _, _ := switchFixture(t)
	sw.BindingID = "binding1"
	sw.Version = 3
	sw.State = biz.Deleting
	member := biz.VirtualSwitchNode{NodeName: "n1", NodeUID: "uid1", Priority: 100, Allocated: true, Prepared: true, Unknown: true, ResourceVersion: "1"}
	nodes[0].SetAPIVersion("v1")
	nodes[0].SetKind("Node")
	labels := nodes[0].GetLabels()
	labels[virtualSwitchGroupPrefix+"protected-other"] = "50"
	nodes[0].SetLabels(labels)
	annotations := nodes[0].GetAnnotations()
	annotations["protected.example/keep"] = "unchanged"
	nodes[0].SetAnnotations(annotations)
	return sw, member, &nodes[0]
}

func TestVirtualSwitchDeleteFenceRejectsDelayedOldPatch(t *testing.T) {
	for _, loseWrite := range []int{0, 1, 2} {
		t.Run(strconv.Itoa(loseWrite), func(t *testing.T) {
			sw, member, node := fenceFixture(t)
			provider, api := newSwitchFenceAPI(t, node, loseWrite, true)
			oldPatch, _ := json.Marshal([]map[string]any{{"op": "test", "path": "/metadata/uid", "value": member.NodeUID}, {"op": "test", "path": "/metadata/resourceVersion", "value": "1"}, {"op": "add", "path": "/metadata/labels/" + jsonPointerKey(virtualSwitchGroupPrefix+sw.GroupName), "value": "100"}, {"op": "add", "path": "/metadata/annotations/" + jsonPointerKey(VirtualSwitchOwnerKey(sw.GroupName)), "value": sw.BindingID}})
			oldResult := make(chan error, 1)
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
				defer cancel()
				_, err := provider.client.Resource(kcNodes).Patch(ctx, member.NodeName, types.JSONPatchType, oldPatch, metav1.PatchOptions{})
				oldResult <- err
			}()
			<-api.entered
			if err := <-oldResult; err == nil {
				t.Fatal("delayed create did not become uncertain")
			}
			current, err := inspectSwitchNodeObject(sw, member, api.snapshot())
			if err != nil || current.State == "cleaned" || current.DeleteFenced || !current.Unknown {
				t.Fatal("absence cleared an unfenced unknown", current, err)
			}
			prepared := current
			prepared.Prepared = true
			prepared.Unknown = true
			current, err = provider.patchVirtualSwitchDeletionNode(context.Background(), sw, prepared, api.snapshot())
			if loseWrite == 1 {
				if err == nil {
					t.Fatal("lost fence response accepted")
				}
				current = prepared
			} else if err != nil {
				t.Fatal(err)
			}
			current, err = inspectSwitchNodeObject(sw, current, api.snapshot())
			if err != nil || !current.DeleteFenced || current.Unknown || current.State != "unfencing" {
				t.Fatal("exact fence did not recover the barrier", current, err)
			}
			// This is the exact value saved by the worker before the next dispatch.
			body, _ := json.Marshal(current)
			var persisted biz.VirtualSwitchNode
			_ = json.Unmarshal(body, &persisted)
			prepared = persisted
			prepared.Unknown = true
			current, err = provider.patchVirtualSwitchDeletionNode(context.Background(), sw, prepared, api.snapshot())
			if loseWrite == 2 {
				if err == nil {
					t.Fatal("lost unfence response accepted")
				}
				current = prepared
			} else if err != nil {
				t.Fatal(err)
			}
			current, err = inspectSwitchNodeObject(sw, current, api.snapshot())
			if err != nil || current.State != "cleaned" || !current.DeleteFenced || current.Unknown {
				t.Fatal("persisted barrier did not recover unfence", current, err)
			}
			api.releaseOnce.Do(func() { close(api.release) })
			if err := <-api.lateDone; err == nil {
				t.Fatal("old prepared RV resurrected the deleted group")
			}
			final := api.snapshot()
			if _, ok := final.GetLabels()[virtualSwitchGroupPrefix+sw.GroupName]; ok {
				t.Fatal("old label resurrected")
			}
			if _, ok := final.GetAnnotations()[virtualSwitchDeleteFenceKey(sw.GroupName)]; ok {
				t.Fatal("temporary fence leaked")
			}
			if final.GetLabels()[virtualSwitchGroupPrefix+"protected-other"] != "50" || final.GetAnnotations()["protected.example/keep"] != "unchanged" {
				t.Fatal("unrelated metadata changed")
			}
			// A new binding of the same name has no owner/fence left to adopt.
			sw.BindingID = "replacement"
			sw.State = biz.Available
			member.Prepared = false
			member.Unknown = false
			member.DeleteFenced = false
			if _, err = inspectSwitchNodeObject(sw, member, final); err != nil {
				t.Fatal("same-name recreation blocked by fence residue", err)
			}
		})
	}
}
