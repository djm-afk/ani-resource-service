package data

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
)

// The only database inputs to this adapter test are the DB clock and an empty
// set of registered wake-up targets. No resource admission/persistence is faked.
type nodeAuditReadDB struct{}
type nodeAuditNoRows struct{ pgx.Rows }
type nodeAuditTimeRow struct{}

func (nodeAuditReadDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	panic("unexpected write in node observation")
}
func (nodeAuditReadDB) Query(_ context.Context, query string, _ ...any) (pgx.Rows, error) {
	if !strings.HasPrefix(query, "-- name: Observation") {
		return nil, fmt.Errorf("unexpected node audit query: %s", query)
	}
	return nodeAuditNoRows{}, nil
}
func (nodeAuditReadDB) QueryRow(_ context.Context, query string, _ ...any) pgx.Row {
	if !strings.HasPrefix(query, "-- name: DatabaseTime") {
		panic("unexpected node audit row query")
	}
	return nodeAuditTimeRow{}
}
func (nodeAuditNoRows) Close()                  {}
func (nodeAuditNoRows) Next() bool              { return false }
func (nodeAuditNoRows) Err() error              { return nil }
func (nodeAuditTimeRow) Scan(dest ...any) error { *dest[0].(*time.Time) = time.Now(); return nil }

func nodeAuditRetryProvider(t *testing.T, afterList func(*KCObservation, int32) error) (*KCProvider, *atomic.Int32) {
	t.Helper()
	listKinds := map[schema.GroupVersionResource]string{}
	for _, gvr := range observationGVRs {
		listKinds[gvr] = strings.ToUpper(gvr.Resource[:1]) + gvr.Resource[1:] + "List"
	}
	client := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds)
	p := &KCProvider{repository: &Postgres{queries: sqlcgen.New(nodeAuditReadDB{})}, client: client}
	o := &KCObservation{provider: p, options: DefaultObservationOptions(), pending: map[string]struct{}{}, changes: map[string]uint64{}}
	p.observation = o
	collections := &atomic.Int32{}
	client.PrependReactor("list", "*", func(action ktesting.Action) (bool, runtime.Object, error) {
		gvr := action.GetResource()
		if gvr == observationGVRs[0] {
			collections.Add(1)
		}
		list := &unstructured.UnstructuredList{}
		list.SetResourceVersion("collection-rv")
		if gvr == kcNodes {
			list.Items = []unstructured.Unstructured{*networkObservationNode()}
		}
		if gvr == observationGVRs[len(observationGVRs)-1] && afterList != nil {
			if err := afterList(o, collections.Load()); err != nil {
				return true, nil, err
			}
		}
		return true, list, nil
	})
	return p, collections
}

func invalidateNodeAudit(o *KCObservation, sequence int32) {
	before := networkObservationNode()
	after := before.DeepCopy()
	after.SetResourceVersion(fmt.Sprint(sequence + 1))
	annotations := after.GetAnnotations()
	annotations["networking.kubercloud.com/chassis"] = fmt.Sprintf("changed-%d", sequence)
	after.SetAnnotations(annotations)
	o.changed(before, after)
}

func TestNodeNetworkRetriesConsecutiveInvalidAuditsWithinCallerBudget(t *testing.T) {
	p, collections := nodeAuditRetryProvider(t, func(o *KCObservation, attempt int32) error {
		if attempt <= 2 {
			invalidateNodeAudit(o, attempt)
		}
		return nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	nodes, _, _, at, err := p.nodeNetworkObjects(ctx)
	if err != nil || len(nodes) != 1 || at.IsZero() || collections.Load() != 3 {
		t.Fatalf("stable third collection not consumed: collections=%d nodes=%d at=%v err=%v", collections.Load(), len(nodes), at, err)
	}
}

func TestNodeNetworkAuditRetriesStayBoundedAndDiagnostic(t *testing.T) {
	t.Run("persistent_invalidation", func(t *testing.T) {
		p, collections := nodeAuditRetryProvider(t, func(o *KCObservation, attempt int32) error { invalidateNodeAudit(o, attempt); return nil })
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		started := time.Now()
		nodes, _, _, at, err := p.nodeNetworkObjects(ctx)
		var failure *biz.ProviderError
		if !errors.As(err, &failure) || failure.Kind != biz.ProviderTemporary || failure.Cause == nil || !errors.Is(err, context.DeadlineExceeded) || len(nodes) != 0 || !at.IsZero() || collections.Load() < 2 || time.Since(started) > time.Second {
			t.Fatalf("unbounded or opaque invalidation: collections=%d at=%v err=%#v", collections.Load(), at, err)
		}
	})
	t.Run("without_deadline_one_refresh_only", func(t *testing.T) {
		p, collections := nodeAuditRetryProvider(t, func(o *KCObservation, attempt int32) error { invalidateNodeAudit(o, attempt); return nil })
		_, _, _, at, err := p.nodeNetworkObjects(context.Background())
		var failure *biz.ProviderError
		if !errors.As(err, &failure) || failure.Kind != biz.ProviderTemporary || failure.Cause == nil || !strings.Contains(failure.Cause.Error(), "node network audit") || !at.IsZero() || collections.Load() != 2 {
			t.Fatalf("unbounded caller did not stop after one fresh retry: collections=%d at=%v err=%#v", collections.Load(), at, err)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		p, collections := nodeAuditRetryProvider(t, func(o *KCObservation, attempt int32) error { invalidateNodeAudit(o, attempt); cancel(); return nil })
		_, _, _, at, err := p.nodeNetworkObjects(ctx)
		if !errors.Is(err, context.Canceled) || !at.IsZero() || collections.Load() != 1 {
			t.Fatalf("cancellation lost: collections=%d at=%v err=%v", collections.Load(), at, err)
		}
	})
	t.Run("read_error", func(t *testing.T) {
		readErr := errors.New("API LIST unavailable")
		p, collections := nodeAuditRetryProvider(t, func(*KCObservation, int32) error { return readErr })
		_, _, _, at, err := p.nodeNetworkObjects(context.Background())
		if !errors.Is(err, readErr) || !at.IsZero() || collections.Load() != 1 {
			t.Fatalf("real LIST failure retried or lost: collections=%d at=%v err=%v", collections.Load(), at, err)
		}
	})
}

func TestStableVirtualSwitchUsesSharedNodeFacts(t *testing.T) {
	p, collections := nodeAuditRetryProvider(t, nil)
	p.repository.placement.ClusterID = "cluster"
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, _, _, at, err := p.nodeNetworkObjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	member := biz.VirtualSwitchNode{NodeName: "node2", NodeUID: "node-uid", Priority: 100, Allocated: true, Prepared: true, Applied: true, LastObservedAt: at}
	sw := biz.VirtualSwitch{EgressMetadata: biz.EgressMetadata{State: biz.Available}, ClusterID: "cluster", GroupName: "public", BindingID: "binding", BindingState: "configured", Nodes: []biz.VirtualSwitchNode{member}}
	// Node-probe ConfigMaps change independently of the node ownership facts
	// needed by an already configured switch. They cannot force a full LIST.
	cm := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "node-probe", "namespace": kcSystemNamespace, "labels": map[string]any{ownerLabel: "ani-network-node-network"}}, "data": map[string]any{"observed_at": "new-probe"}}}
	p.observation.changed(nil, cm)
	current, err := p.InspectVirtualSwitchNode(ctx, sw, member)
	if err != nil || !current.Applied || !current.LastObservedAt.Equal(at) || collections.Load() != 1 {
		t.Fatalf("ordinary switch re-collected or renewed old evidence: %+v collections=%d err=%v", current, collections.Load(), err)
	}
	for _, change := range []string{"replaced", "added", "source-boundary", "stale"} {
		t.Run(change, func(t *testing.T) {
			o := p.observation
			o.mu.Lock()
			o.changes = map[string]uint64{}
			o.epoch = o.view.epoch
			o.view.collected = at
			o.mu.Unlock()
			node := networkObservationNode()
			switch change {
			case "replaced":
				replacement := node.DeepCopy()
				replacement.SetUID("replacement")
				o.changed(node, replacement)
			case "added":
				node.SetName("new-allocated-node")
				node.SetUID("new-uid")
				o.changed(nil, node)
			case "source-boundary":
				o.sourceBoundary()
			case "stale":
				o.mu.Lock()
				o.view.collected = at.Add(-time.Minute)
				o.mu.Unlock()
			}
			_, err := p.InspectVirtualSwitchNode(ctx, sw, member)
			var failure *biz.ProviderError
			if !errors.As(err, &failure) || failure.Kind != biz.ProviderTemporary || collections.Load() != 1 {
				t.Fatalf("invalid ordinary facts were accepted or forced a collection: collections=%d err=%v", collections.Load(), err)
			}
		})
	}
	// An unresolved node mutation retains the current collection/recovery path.
	member.Unknown = true
	current, err = p.InspectVirtualSwitchNode(ctx, sw, member)
	if err != nil || !current.Applied || current.Unknown || collections.Load() != 2 {
		t.Fatalf("unknown node mutation did not use current proof: %+v collections=%d err=%v", current, collections.Load(), err)
	}
}
