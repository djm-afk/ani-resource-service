package data

import (
	"context"
	"fmt"
	"time"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func sharedTombstone(t biz.ProviderTarget) bool {
	return t.Tombstone && t.Deleting && !t.Direct && t.KnownIdentity != ""
}

// observeTombstone reuses a complete, still valid audit after a prior deletion
// was already independently proved. It never completes an active deletion or
// an uncertain mutation from cache absence. Retained identities and complete
// dependencies continue to discover late objects and protect parent reuse.
func (p *KCProvider) observeTombstone(ctx context.Context, t biz.ProviderTarget, b sqlcgen.NetworkProviderBinding) (biz.ProviderObservation, error) {
	after := time.Time{}
	for {
		if err := ctx.Err(); err != nil {
			return biz.ProviderObservation{}, readFailure(err)
		}
		v, err := p.observation.snapshot(ctx, after)
		if err != nil {
			return biz.ProviderObservation{}, readFailure(err)
		}
		kind, field := "VPC", "gateway"
		resources := []schema.GroupVersionResource{kcSubnets}
		if b.ResourceKind == "subnet" {
			kind, field = "Subnet", "subnet"
			resources = observationGVRs[2:]
		}
		keys := []string{"object:" + kind + "/" + b.Namespace + "/" + b.ProviderName, field + ":" + b.Namespace + "/" + b.ProviderName}
		index := v.indices[kcResource(b)]
		if index == nil {
			return biz.ProviderObservation{}, readFailure(fmt.Errorf("tombstone object collection incomplete"))
		}
		var object *unstructured.Unstructured
		raw, exists, err := index.GetByKey(b.Namespace + "/" + b.ProviderName)
		if err != nil {
			return biz.ProviderObservation{}, readFailure(err)
		}
		if exists {
			object = raw.(*unstructured.Unstructured)
		}
		dependencies := map[schema.GroupVersionResource][]unstructured.Unstructured{}
		hasDependencies := false
		for _, gvr := range resources {
			if v.indices[gvr] == nil {
				return biz.ProviderObservation{}, readFailure(fmt.Errorf("tombstone dependency collection incomplete"))
			}
			dependencies[gvr] = indexedObjects(v.indices[gvr], keys[1:])
			hasDependencies = hasDependencies || len(dependencies[gvr]) > 0
		}
		proof := biz.ObservationProof{CollectedAt: v.collected, CoveredGeneration: v.generations[observationTarget{t.TenantID, t.ResourceID, t.Kind}.key()], Hash: contentHash(struct {
			Object       *unstructured.Unstructured
			Dependencies map[string][]unstructured.Unstructured
		}{object, candidatesForHash(dependencies)})}
		if p.observation.valid(v, keys) {
			proof.CoveredGeneration = t.Requirement.RequestedGeneration
		}
		if proof.Covers(t.Requirement) && p.observation.valid(v, keys) {
			var count int64
			if b.ResourceKind == "subnet" {
				count, err = p.repository.queries.CountAttachments(ctx, sqlcgen.CountAttachmentsParams{TenantID: b.TenantID, SubnetID: textValue(b.SubnetID)})
			} else {
				count, err = p.repository.queries.CountBlockingSubnets(ctx, sqlcgen.CountBlockingSubnetsParams{TenantID: b.TenantID, VpcID: textValue(b.VpcID)})
			}
			if err != nil {
				return biz.ProviderObservation{}, readFailure(err)
			}
			value := biz.ProviderObservation{}
			if object != nil {
				value, err = p.inspect(ctx, object, b, t)
				if err != nil {
					return biz.ProviderObservation{}, err
				}
			}
			value.HasDependencies = value.HasDependencies || hasDependencies || count > 0
			value.Proof = proof
			if p.observation.valid(v, keys) {
				return value, nil
			}
		}
		// A changed relation, uncovered notification, reconnect or older unequal
		// fact needs a new full collection. Stable equal facts keep their original
		// timestamp and do not advance this boundary on every tombstone call.
		if !after.IsZero() {
			if _, bounded := ctx.Deadline(); !bounded {
				return biz.ProviderObservation{}, readFailure(fmt.Errorf("tombstone collection changed"))
			}
		}
		after, err = p.repository.queries.DatabaseTime(ctx)
		if err != nil {
			return biz.ProviderObservation{}, readFailure(err)
		}
	}
}
