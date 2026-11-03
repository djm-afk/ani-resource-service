package data

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func (p *KCProvider) Observe(ctx context.Context, t biz.ProviderTarget) (biz.ProviderObservation, error) {
	if egressKind(t.Kind) {
		return p.observeEgress(ctx, t)
	}
	if p.observation != nil && sharedTombstone(t) {
		b, err := p.binding(ctx, t)
		if err != nil {
			return biz.ProviderObservation{}, err
		}
		return p.observeTombstone(ctx, t, b)
	}
	if p.observation != nil && !t.Direct && t.KnownIdentity != "" {
		b, err := p.binding(ctx, t)
		if err != nil {
			return biz.ProviderObservation{}, err
		}
		p.observation.mu.Lock()
		v := p.observation.view
		p.observation.mu.Unlock()
		if v != nil && time.Since(v.collected) <= p.observation.options.AuditInterval+p.observation.options.AuditJitter {
			kind := "VPC"
			if b.ResourceKind == "vnic" {
				kind = "VNic"
			}
			if b.ResourceKind == "subnet" {
				kind = "Subnet"
			}
			keys := []string{"object:" + kind + "/" + b.Namespace + "/" + b.ProviderName}
			objects := indexedObjects(v.indices[kcResource(b)], keys)
			if len(objects) == 1 && p.observation.valid(v, keys) {
				proof := biz.ObservationProof{CollectedAt: v.collected, Hash: contentHash(objects[0].Object), CoveredGeneration: t.Requirement.RequestedGeneration}
				if proof.Covers(t.Requirement) {
					value, err := p.inspect(ctx, &objects[0], b, t)
					value.Proof = proof
					return value, err
				}
			}
		}
	}
	// Absence, unknown POST, UID conflict and uncovered/older cache observations
	// are verified by a current API request after the durable Claim.
	now, err := p.repository.queries.DatabaseTime(ctx)
	if err != nil {
		return biz.ProviderObservation{}, readFailure(err)
	}
	value, err := p.observeDirect(ctx, t)
	value.Proof = biz.ObservationProof{CollectedAt: now, Hash: contentHash(value), CoveredGeneration: t.Requirement.RequestedGeneration}
	return value, err
}

func (p *KCProvider) ObserveAttachment(ctx context.Context, w biz.AttachmentWork) (biz.AttachmentObservation, error) {
	if w.Attachment.WorkloadMode == "vm_managed" && p.observation == nil {
		now, err := p.repository.queries.DatabaseTime(ctx)
		if err != nil {
			return biz.AttachmentObservation{}, readFailure(err)
		}
		result, err := p.observeAttachment(ctx, w, func(ctx context.Context, gvr schema.GroupVersionResource) ([]unstructured.Unstructured, error) {
			if gvr == pods {
				return p.listScoped(ctx, gvr, w.Attachment.Namespace, "")
			}
			return p.listAll(ctx, gvr)
		})
		result.Proof = biz.ObservationProof{CollectedAt: now, CoveredGeneration: w.Requirement.RequestedGeneration, Hash: contentHash(result.Relations)}
		return result, err
	}
	if p.observation == nil {
		// observeAttachment validates this Namespace against the saved Subnet
		// binding before invoking the reader. Preserve an unfiltered Pod list
		// within that boundary so missing labels cannot hide historic Pods.
		return p.observeAttachment(ctx, w, func(ctx context.Context, gvr schema.GroupVersionResource) ([]unstructured.Unstructured, error) {
			if gvr == pods {
				return p.listScoped(ctx, gvr, w.Attachment.Namespace, "")
			}
			return p.listAll(ctx, gvr)
		})
	}
	a := w.Attachment
	// A stopped VM retains its reservation, but is no longer awaiting initial
	// network attachment. Its ordinary projection uses the same shared facts
	// as Attached; final closure/protocol recovery still demands a fresh audit.
	stoppedReservation := a.State == biz.Reserved && a.WorkloadMode == "vm_managed" && w.ConsumerSnapshot != nil && w.ConsumerSnapshot.State == "open" && w.ConsumerSnapshot.DesiredRunState == "stopped"
	critical := w.RequireFreshRelations || (a.State != biz.Attached && a.State != biz.Released && !stoppedReservation)
	keys, err := attachmentObservationKeys(w)
	if err != nil {
		return biz.AttachmentObservation{}, readFailure(err)
	}
	after := time.Time{}
	if w.RequireFreshRelations {
		// The owner response is obtained after Claim. A collection started before
		// that response can miss relations created by the owner's last in-flight
		// submission. Require a complete collection begun after durable closure
		// was observed, using the same database clock as the audit timestamp.
		var err error
		after, err = p.repository.queries.DatabaseTime(ctx)
		if err != nil {
			return biz.AttachmentObservation{}, readFailure(err)
		}
	}
	// Each invalidation moves the required collection boundary forward. Keep
	// collecting within the caller's original request budget; consecutive Watch
	// renewals can invalidate more than one audit without changing any identity.
	for {
		if err := ctx.Err(); err != nil {
			return biz.AttachmentObservation{}, readFailure(err)
		}
		var v *auditView
		if critical {
			v, err = p.observation.snapshot(ctx, after)
		} else {
			now, e := p.repository.queries.DatabaseTime(ctx)
			if e != nil {
				return biz.AttachmentObservation{}, readFailure(e)
			}
			v, err = p.observation.cachedSnapshot(now)
		}
		if err != nil {
			return biz.AttachmentObservation{}, readFailure(err)
		}
		candidates := map[schema.GroupVersionResource][]unstructured.Unstructured{}
		allKeys := append([]string{}, keys...)
		// Expand Pod UID -> VNic UID -> VNicIP references in the shared indices.
		// Persisted relations keep orphaned IPs and same-name UID changes attributable.
		// Only these four GVRs are read by observeAttachment or its EIP dependency
		// check. Other objects sharing a controller UID are not address evidence.
		for _, gvr := range observationGVRs[2:6] {
			objects := indexedObjects(v.indices[gvr], allKeys)
			candidates[gvr] = objects
			p.observation.candidates.Add(int64(len(objects)))
			for _, o := range objects {
				allKeys = append(allKeys, "uid:"+string(o.GetUID()))
				if o.GetKind() == "VNic" {
					allKeys = append(allKeys, "vNic:"+o.GetNamespace()+"/"+o.GetName())
				}
			}
		}
		proof := biz.ObservationProof{CollectedAt: v.collected, CoveredGeneration: v.generations[observationTarget{a.TenantID, a.ID, "attachment"}.key()]}
		// Parent/platform hints can advance the durable generation without
		// changing this attachment's Pod/NIC/IP facts. A complete, unchanged
		// relationship view covers such a hint without changing its source time.
		// Closure and initial attachment still require the post-claim audit.
		if !critical && p.observation.validAttachment(v, allKeys) {
			proof.CoveredGeneration = w.Requirement.RequestedGeneration
		}
		if proof.CoveredGeneration < w.Requirement.RequestedGeneration || !p.observation.validAttachment(v, allKeys) {
			if !critical {
				return biz.AttachmentObservation{}, readFailure(fmt.Errorf("attachment shared observation is pending"))
			}
			after, err = p.repository.queries.DatabaseTime(ctx)
			if err != nil {
				return biz.AttachmentObservation{}, readFailure(err)
			}
			continue
		}
		result, err := p.observeAttachment(ctx, w, func(_ context.Context, gvr schema.GroupVersionResource) ([]unstructured.Unstructured, error) {
			return candidates[gvr], nil
		})
		if err != nil {
			return biz.AttachmentObservation{}, err
		}
		// EIP/orphan objects are never hidden by a Network label selector. Any EIP
		// referring to this subnet blocks release until the external owner cleans it.
		eips := candidates[observationGVRs[5]]
		if len(eips) > 0 {
			result.HasDependencies = true
		}
		// VM/VMI facts are verified by observeAttachment's precise reads. Include
		// those facts before applying the same-content exception to an older audit.
		proof.Hash = attachmentObservationHash(w, candidates, result)
		if !proof.Covers(w.Requirement) || !p.observation.validAttachment(v, allKeys) {
			if !critical {
				return biz.AttachmentObservation{}, readFailure(fmt.Errorf("attachment shared observation is pending"))
			}
			after, err = p.repository.queries.DatabaseTime(ctx)
			if err != nil {
				return biz.AttachmentObservation{}, readFailure(err)
			}
			continue
		}
		result.Proof = proof
		return result, nil
	}
}
func candidatesForHash(values map[schema.GroupVersionResource][]unstructured.Unstructured) map[string][]unstructured.Unstructured {
	out := map[string][]unstructured.Unstructured{}
	for gvr, v := range values {
		// Informer List order is not evidence of a changed object set. Keep
		// every field, but canonicalize a copy so shared view slices stay intact.
		objects := slices.Clone(v)
		slices.SortFunc(objects, func(a, b unstructured.Unstructured) int {
			for _, pair := range [][2]string{{a.GetNamespace(), b.GetNamespace()}, {a.GetName(), b.GetName()}, {string(a.GetUID()), string(b.GetUID())}} {
				if order := strings.Compare(pair[0], pair[1]); order != 0 {
					return order
				}
			}
			// Preserve deterministic evidence even for duplicate identities with
			// contradictory contents; do not collapse or hide either candidate.
			return strings.Compare(contentHash(a.Object), contentHash(b.Object))
		})
		out[gvr.String()] = objects
	}
	return out
}

// completeDependencies shares full-range audits for critical cleanup checks.
// It cannot use a cache miss, selector-filtered collection, or partial page.
func (p *KCProvider) completeDependencies(ctx context.Context, b sqlcgen.NetworkProviderBinding) (bool, error) {
	now, err := p.repository.queries.DatabaseTime(ctx)
	if err != nil {
		return false, readFailure(err)
	}
	v, err := p.observation.snapshot(ctx, now)
	if err != nil {
		return false, readFailure(err)
	}
	field := "gateway"
	resources := []schema.GroupVersionResource{kcSubnets}
	if b.ResourceKind == "subnet" {
		field = "subnet"
		resources = observationGVRs[2:]
	}
	keys := []string{field + ":" + b.Namespace + "/" + b.ProviderName}
	if !p.observation.valid(v, keys) {
		return false, readFailure(fmt.Errorf("dependency collection changed"))
	}
	for _, gvr := range resources {
		if len(indexedObjects(v.indices[gvr], keys)) > 0 {
			return true, nil
		}
	}
	return false, nil
}
