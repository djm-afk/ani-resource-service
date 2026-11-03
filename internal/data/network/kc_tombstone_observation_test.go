package data

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
)

// This adapter-only seam supplies the DB clock, empty registered audit targets,
// and dependency counts. Resource admission and state persistence are not mocked.
type tombstoneAuditReadDB struct{ nodeAuditReadDB }
type tombstoneCountRow int64

func (db tombstoneAuditReadDB) QueryRow(ctx context.Context, query string, args ...any) pgx.Row {
	if strings.HasPrefix(query, "-- name: CountAttachments") || strings.HasPrefix(query, "-- name: CountBlockingSubnets") {
		return tombstoneCountRow(0)
	}
	return db.nodeAuditReadDB.QueryRow(ctx, query, args...)
}
func (r tombstoneCountRow) Scan(dest ...any) error { *dest[0].(*int64) = int64(r); return nil }

func TestCleanVPCTombstonesShareCompleteAbsentAudit(t *testing.T) {
	for _, kind := range []string{"vpc", "subnet"} {
		t.Run(kind, func(t *testing.T) {
			p, collections := nodeAuditRetryProvider(t, nil)
			p.repository.queries = sqlcgen.New(tombstoneAuditReadDB{})
			id := "retired"
			b := sqlcgen.NetworkProviderBinding{TenantID: "tenant", ResourceKind: kind, Namespace: "tenant", ProviderName: id, ProviderUid: "original-uid", VpcID: &id, SubnetID: &id}
			target := biz.ProviderTarget{Kind: kind, TenantID: b.TenantID, ResourceID: id, KnownIdentity: b.ProviderUid, Deleting: true, Tombstone: true}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			var collected time.Time
			for i := 0; i < 3; i++ {
				o, err := p.observeTombstone(ctx, target, b)
				if err != nil || o.Exists || o.HasDependencies || o.Proof.CollectedAt.IsZero() {
					t.Fatal("completed tombstone failed to reuse complete absence", o, err)
				}
				if i == 0 {
					collected = o.Proof.CollectedAt
				} else if !o.Proof.CollectedAt.Equal(collected) {
					t.Fatal("shared tombstone observation renewed its source timestamp")
				}
				if o.Proof.CoveredGeneration != target.Requirement.RequestedGeneration {
					t.Fatal("unchanged tombstone failed to cover a parent hint")
				}
				target.Requirement = biz.ObservationRequirement{AppliedAt: time.Now(), Hash: o.Proof.Hash, RequestedGeneration: int64(i + 1)}
			}
			if collections.Load() != 1 {
				t.Fatal("completed VPC/Subnet tombstones repeatedly recollected the network", collections.Load())
			}
		})
	}
}

func tombstoneGatewayResolved() egressResolved {
	return egressResolved{
		binding: sqlcgen.NetworkProviderBinding{ResourceKind: "egress_gateway", ProviderName: "retired-gateway", ProviderUid: "original-uid", BindingID: "binding"},
		target:  biz.ProviderTarget{Kind: "egress_gateway", ResourceID: "retired", BindingID: "binding", KnownIdentity: "original-uid", Deleting: true, Tombstone: true},
		refs:    map[string]egressReference{}, spec: map[string]any{},
	}
}

func TestCleanEgressTombstonesShareCompleteAbsentAudit(t *testing.T) {
	p, collections := nodeAuditRetryProvider(t, nil)
	r := tombstoneGatewayResolved()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := p.observation.snapshot(ctx, time.Time{}); err != nil {
		t.Fatal(err)
	}
	var originalCollected time.Time
	for i := 0; i < 3; i++ {
		set, err := p.readEgressSet(ctx, r, false)
		if err != nil {
			t.Fatal(err)
		}
		value, err := inspectEgressSet(r, set)
		if err != nil || value.Exists || value.HasDependencies || set.proof.CollectedAt.IsZero() {
			t.Fatal("shared complete absence was not usable", value, set.proof, err)
		}
		if i == 0 {
			originalCollected = set.proof.CollectedAt
		} else if !set.proof.CollectedAt.Equal(originalCollected) {
			t.Fatal("reading identical tombstones renewed the collection timestamp")
		}
		if set.proof.CoveredGeneration != r.target.Requirement.RequestedGeneration {
			t.Fatal("unchanged egress tombstone failed to cover a parent hint")
		}
		r.target.Requirement = biz.ObservationRequirement{AppliedAt: time.Now(), Hash: set.proof.Hash, RequestedGeneration: int64(i + 1)}
	}
	if got := collections.Load(); got != 1 {
		t.Fatalf("clean tombstones recollected the entire network instead of sharing: collections=%d", got)
	}
	for _, action := range p.client.(*fake.FakeDynamicClient).Actions() {
		if action.GetVerb() == "get" && action.GetResource() == kcEgressGateways {
			t.Fatal("clean tombstone performed a per-resource GET instead of using complete audit absence")
		}
	}
}

func TestUnmarkedDeletionDoesNotUseTombstoneAbsenceShortcut(t *testing.T) {
	p, collections := nodeAuditRetryProvider(t, nil)
	r := tombstoneGatewayResolved()
	r.target.Tombstone = false
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for i := 0; i < 2; i++ {
		if _, err := p.readEgressSet(ctx, r, false); err != nil {
			t.Fatal(err)
		}
	}
	gets := 0
	for _, action := range p.client.(*fake.FakeDynamicClient).Actions() {
		if action.GetVerb() == "get" && action.GetResource() == kcEgressGateways {
			gets++
		}
	}
	if gets != 2 || collections.Load() < 2 {
		t.Fatal("unmarked deletion mistook old complete absence for post-claim cleanup evidence", gets, collections.Load())
	}
}

func TestSharedEgressTombstoneKeepsLateDependencyAndReplacementIdentity(t *testing.T) {
	for _, name := range []string{"late-dependency", "replacement-uid"} {
		t.Run(name, func(t *testing.T) {
			p, _ := nodeAuditRetryProvider(t, nil)
			r := tombstoneGatewayResolved()
			client := p.client.(*fake.FakeDynamicClient)
			client.PrependReactor("list", "*", func(action ktesting.Action) (bool, runtime.Object, error) {
				if name == "late-dependency" && action.GetResource() == kcSubnets {
					child := unstructured.Unstructured{Object: map[string]any{"apiVersion": "networking.kubercloud.com/v1", "kind": "Subnet", "metadata": map[string]any{"name": "late", "namespace": kcSystemNamespace}, "spec": map[string]any{"type": "Public", "gateway": r.binding.ProviderName}}}
					list := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{child}}
					list.SetResourceVersion("collection-rv")
					return true, list, nil
				}
				if name == "replacement-uid" && action.GetResource() == kcEgressGateways {
					o := egressObject(r)
					o.SetUID(types.UID("replacement-uid"))
					o.SetResourceVersion("replacement-rv")
					list := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{*o}}
					list.SetResourceVersion("collection-rv")
					return true, list, nil
				}
				return false, nil, nil
			})
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if _, err := p.observation.snapshot(ctx, time.Time{}); err != nil {
				t.Fatal(err)
			}
			set, err := p.readEgressSet(ctx, r, false)
			if err != nil {
				t.Fatal(err)
			}
			value, err := inspectEgressSet(r, set)
			if name == "late-dependency" && (err != nil || !value.HasDependencies) {
				t.Fatal("shared tombstone hid an unlabelled late dependency", value, err)
			}
			if name == "replacement-uid" {
				failure, ok := err.(*biz.ProviderError)
				if !ok || failure.Kind != biz.ProviderConflict {
					t.Fatal("shared tombstone accepted a replacement UID", value, err)
				}
			}
			// Reading the collection must never issue deletion against a candidate.
			for _, action := range client.Actions() {
				if action.GetVerb() == "delete" {
					t.Fatal("observation deleted a candidate before the mutation fence")
				}
			}
		})
	}
}

func TestOrdinaryEgressObservationDoesNotStartFullCollection(t *testing.T) {
	p, collections := nodeAuditRetryProvider(t, nil)
	r := tombstoneGatewayResolved()
	r.target.Deleting, r.target.Tombstone = false, false
	if _, err := p.readEgressSet(context.Background(), r, false); err == nil {
		t.Fatal("missing shared audit was treated as current evidence")
	}
	if collections.Load() != 0 {
		t.Fatal("ordinary observation started a full collection")
	}
}
