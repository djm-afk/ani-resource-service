package data

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
	"strings"
	"time"
)

func attachment(row sqlcgen.NetworkAttachment) (biz.Attachment, error) {
	var plan biz.PodPrimaryPlan
	if err := json.Unmarshal(row.Plan, &plan); err != nil {
		return biz.Attachment{}, databaseFailure(err)
	}
	a := biz.Attachment{ID: row.AttachmentID, TenantID: row.TenantID, VPCID: row.VpcID, SubnetID: row.SubnetID, InstanceID: row.InstanceID, ConsumerType: row.ConsumerType, Slot: row.Slot, SubmissionID: row.SubmissionID, Generation: row.Generation, ClusterID: row.ClusterID, Namespace: row.Namespace, BindingRevision: row.BindingRevision, State: biz.AttachmentState(row.State), Reason: biz.Reason(row.Reason), Version: row.Version, PodName: row.PodName, PodUID: row.PodUid, ConfirmUID: row.ConfirmUid, FinalizationID: textValue(row.FinalizationID), CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt, ObservedAt: row.ObservedAt, ReleasedAt: row.ReleasedAt}
	a.WorkloadMode = plan.WorkloadMode
	if a.WorkloadMode == "" {
		a.WorkloadMode = "single_pod"
	}
	if a.State == biz.Reserved || a.State == biz.Attached {
		if a.WorkloadMode == "vm_managed" {
			a.VMPrimary = plan.VMPrimary
		} else {
			a.Plan = &plan
		}
	}
	return a, nil
}
func attachmentReplay(ctx context.Context, q *sqlcgen.Queries, db attachmentSnapshotReader, r biz.PrepareAttachment) (biz.Attachment, bool, error) {
	row, e := q.GetAttachmentReplay(ctx, sqlcgen.GetAttachmentReplayParams{TenantID: r.TenantID, InstanceID: r.InstanceID, Slot: r.Slot, RequestKey: r.RequestKey})
	if errors.Is(e, pgx.ErrNoRows) {
		return biz.Attachment{}, false, nil
	}
	if e != nil {
		return biz.Attachment{}, false, databaseFailure(e)
	}
	if row.Fingerprint != r.Fingerprint() {
		return biz.Attachment{}, true, biz.Fail(biz.IdempotencyConflict, "attachment key already accepted another intent")
	}
	a, e := managedAttachment(ctx, db, row)
	return a, true, e
}
func attachmentHistory(ctx context.Context, q *sqlcgen.Queries, row sqlcgen.NetworkAttachment, event string) error {
	return q.InsertAttachmentHistory(ctx, sqlcgen.InsertAttachmentHistoryParams{TenantID: row.TenantID, HistoryID: uuid.NewString(), AttachmentID: row.AttachmentID, Version: row.Version, Event: event, State: row.State, Reason: row.Reason, PodUid: row.PodUid, FinalizationID: row.FinalizationID})
}
func (p *Postgres) PrepareAttachment(ctx context.Context, r biz.PrepareAttachment, freshness time.Duration) (biz.Attachment, error) {
	if a, found, e := attachmentReplay(ctx, p.queries, p.pool, r); found || e != nil {
		return a, e
	}
	tx, e := p.pool.Begin(ctx)
	if e != nil {
		return biz.Attachment{}, databaseFailure(e)
	}
	defer tx.Rollback(ctx)
	q := p.queries.WithTx(tx)
	ctx = context.WithValue(ctx, admissionQueriesKey{}, q)
	initial, e := q.GetSubnet(ctx, sqlcgen.GetSubnetParams{TenantID: r.TenantID, SubnetID: r.SubnetID})
	if e != nil {
		return biz.Attachment{}, databaseFailure(e)
	}
	v, e := q.LockVPC(ctx, sqlcgen.LockVPCParams{TenantID: r.TenantID, VpcID: initial.VpcID})
	if e != nil {
		return biz.Attachment{}, databaseFailure(e)
	}
	s, e := q.LockSubnet(ctx, sqlcgen.LockSubnetParams{TenantID: r.TenantID, SubnetID: r.SubnetID})
	if e != nil {
		return biz.Attachment{}, databaseFailure(e)
	}
	if e = q.LockAttachmentSlot(ctx, sqlcgen.LockAttachmentSlotParams{TenantID: r.TenantID, InstanceID: r.InstanceID, Slot: r.Slot}); e != nil {
		return biz.Attachment{}, databaseFailure(e)
	}
	if a, found, e := attachmentReplay(ctx, q, tx, r); found || e != nil {
		return a, e
	}
	occupied, e := q.HasAttachmentSlot(ctx, sqlcgen.HasAttachmentSlotParams{TenantID: r.TenantID, InstanceID: r.InstanceID, Slot: r.Slot})
	if e != nil {
		return biz.Attachment{}, databaseFailure(e)
	}
	if occupied {
		return biz.Attachment{}, biz.Fail(biz.AttachmentConflict, "instance primary slot is occupied")
	}
	now, e := q.DatabaseTime(ctx)
	if e != nil {
		return biz.Attachment{}, databaseFailure(e)
	}
	if ((r.WorkloadMode == "vm_managed" || p.networkProvider != nil) && (!networkManagementOpen(v.State, v.Reason) || !networkManagementOpen(s.State, s.Reason))) || (r.WorkloadMode != "vm_managed" && p.networkProvider == nil && (v.State != "available" || s.State != "available" || v.ObservedAt == nil || s.ObservedAt == nil || now.Sub(*v.ObservedAt) > freshness || now.Sub(*s.ObservedAt) > freshness)) {
		return biz.Attachment{}, biz.Fail(biz.NetworkNotReady, "fresh available VPC and subnet required")
	}
	if r.VPCID != "" && r.VPCID != s.VpcID {
		return biz.Attachment{}, biz.Fail(biz.InvalidArgument, "subnet does not belong to requested VPC")
	}
	b, e := q.GetBinding(ctx, sqlcgen.GetBindingParams{TenantID: r.TenantID, ResourceID: r.SubnetID})
	if e != nil {
		return biz.Attachment{}, databaseFailure(e)
	}
	if b.ClusterID != r.ClusterID || b.Namespace != r.Namespace {
		return biz.Attachment{}, biz.Fail(biz.PlacementMismatch, "consumer placement differs from network binding")
	}
	if b.ProviderUid == "" {
		return biz.Attachment{}, biz.Fail(biz.NetworkNotReady, "subnet binding identity is unavailable")
	}
	if r.WorkloadMode != "vm_managed" && p.networkProvider != nil {
		currentCtx, err := p.networkProvider.namespaceContext(ctx, b)
		if err != nil {
			return biz.Attachment{}, err
		}
		for _, id := range []string{s.VpcID, s.SubnetID} {
			if _, err = p.networkProvider.currentNetworkResource(currentCtx, q, r.TenantID, id); err != nil {
				return biz.Attachment{}, err
			}
		}
	}
	id := "att_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	delivery := biz.PodPrimaryPlan{FormatVersion: 1, WorkloadMode: r.WorkloadMode, Namespace: b.Namespace, PrimaryNetworkRef: b.Namespace + "/" + b.ProviderName, TenantID: r.TenantID, InstanceID: r.InstanceID, AttachmentID: id, SubmissionID: r.SubmissionID, Generation: r.Generation}
	if r.WorkloadMode == "vm_managed" {
		if p.vmNetworks == nil || r.VMExpectation == nil {
			return biz.Attachment{}, biz.Fail(biz.DependencyUnavailable, "managed VM network is not configured")
		}
		vmPlan, err := p.vmNetworks.ResolveVMNetwork(ctx, biz.VMNetworkSelection{TenantID: r.TenantID, VPCID: s.VpcID, SubnetID: s.SubnetID, ClusterID: b.ClusterID, Namespace: b.Namespace, BindingRevision: b.BindingID + ":" + b.ProviderUid, KCNSubnetRef: delivery.PrimaryNetworkRef}, *r.VMExpectation)
		if err != nil {
			return biz.Attachment{}, err
		}
		if vmPlan.Namespace != b.Namespace || vmPlan.KCNSubnetRef != delivery.PrimaryNetworkRef || vmPlan.NADName != r.VMExpectation.NADName || vmPlan.NADUID != r.VMExpectation.NADUID || vmPlan.NADConfigDigest != r.VMExpectation.NADConfigDigest {
			return biz.Attachment{}, biz.Fail(biz.AttachmentConflict, "VM network resolver returned a different frozen dependency")
		}
		vmPlan.FormatVersion = 1
		vmPlan.TenantID = r.TenantID
		vmPlan.InstanceID = r.InstanceID
		vmPlan.AttachmentID = id
		vmPlan.SubmissionID = r.SubmissionID
		vmPlan.Generation = r.Generation
		delivery.VMPrimary = &vmPlan
	}
	plan, e := json.Marshal(delivery)
	if e != nil {
		return biz.Attachment{}, databaseFailure(e)
	}
	row, e := q.InsertAttachment(ctx, sqlcgen.InsertAttachmentParams{TenantID: r.TenantID, AttachmentID: id, VpcID: s.VpcID, SubnetID: s.SubnetID, BindingID: b.BindingID, InstanceID: r.InstanceID, ConsumerType: r.ConsumerType, Slot: r.Slot, RequestKey: r.RequestKey, SubmissionID: r.SubmissionID, Generation: r.Generation, Fingerprint: r.Fingerprint(), ClusterID: b.ClusterID, Namespace: b.Namespace, BindingRevision: b.BindingID + ":" + b.ProviderUid, Plan: plan})
	if e != nil {
		return biz.Attachment{}, databaseFailure(e)
	}
	if r.WorkloadMode == "controller_managed" || r.WorkloadMode == "vm_managed" {
		if _, e = tx.Exec(ctx, `INSERT INTO network_attachment_consumer_snapshots(tenant_id,attachment_id) VALUES($1,$2)`, r.TenantID, id); e != nil {
			return biz.Attachment{}, databaseFailure(e)
		}
	}
	if e = attachmentHistory(ctx, q, row, "prepared"); e != nil {
		return biz.Attachment{}, databaseFailure(e)
	}
	if e = tx.Commit(ctx); e != nil {
		return biz.Attachment{}, databaseFailure(e)
	}
	return managedAttachment(ctx, p.pool, row)
}
func (p *Postgres) GetAttachment(ctx context.Context, t, id string) (biz.Attachment, error) {
	row, e := p.queries.GetAttachment(ctx, sqlcgen.GetAttachmentParams{TenantID: t, AttachmentID: id})
	if e != nil {
		return biz.Attachment{}, databaseFailure(e)
	}
	a, err := managedAttachment(ctx, p.pool, row)
	if err != nil {
		return biz.Attachment{}, err
	}
	if a.WorkloadMode == "vm_managed" {
		a.VMCurrentAddresses = p.vmCurrentAddresses(ctx, a, row.ProviderRelations)
		if a.State == biz.Attached && a.Reason == "" && len(a.VMCurrentAddresses) == 0 {
			a.Reason = biz.Reason("VM_NETWORK_ADDRESS_NOT_CURRENT")
		}
	}
	return a, nil
}
func lockAttachmentParents(ctx context.Context, q *sqlcgen.Queries, t, id string) (sqlcgen.NetworkAttachment, error) {
	initial, e := q.GetAttachment(ctx, sqlcgen.GetAttachmentParams{TenantID: t, AttachmentID: id})
	if e != nil {
		return initial, e
	}
	if _, e = q.LockVPC(ctx, sqlcgen.LockVPCParams{TenantID: t, VpcID: initial.VpcID}); e != nil {
		return initial, e
	}
	if _, e = q.LockSubnet(ctx, sqlcgen.LockSubnetParams{TenantID: t, SubnetID: initial.SubnetID}); e != nil {
		return initial, e
	}
	return q.LockAttachment(ctx, sqlcgen.LockAttachmentParams{TenantID: t, AttachmentID: id})
}
func (p *Postgres) ConfirmAttachment(ctx context.Context, r biz.ConfirmAttachment) (biz.Attachment, error) {
	tx, e := p.pool.Begin(ctx)
	if e != nil {
		return biz.Attachment{}, databaseFailure(e)
	}
	defer tx.Rollback(ctx)
	q := p.queries.WithTx(tx)
	row, e := lockAttachmentParents(ctx, q, r.TenantID, r.AttachmentID)
	if e != nil {
		return biz.Attachment{}, databaseFailure(e)
	}
	if r.ClusterID != row.ClusterID || r.Namespace != row.Namespace {
		return biz.Attachment{}, biz.Fail(biz.PlacementMismatch, "Pod placement differs from attachment")
	}
	mode, e := attachment(row)
	if e != nil {
		return biz.Attachment{}, e
	}
	if mode.WorkloadMode == "controller_managed" || mode.WorkloadMode == "vm_managed" {
		return p.confirmManagedAttachment(ctx, tx, q, row, r)
	}
	// Immutable identity replay precedes mutable version and state checks, also
	// when the worker recovered the same Pod before Confirm arrived.
	if row.PodUid == r.PodUID && row.PodName == r.PodName {
		return attachment(row)
	}
	if row.PodUid != "" || row.ConfirmUid != "" {
		return biz.Attachment{}, biz.Fail(biz.AttachmentConflict, "attachment already registered another Pod")
	}
	if row.State != "reserved" {
		return biz.Attachment{}, biz.Fail(biz.AttachmentConflict, "attachment no longer accepts submission")
	}
	if row.Version != r.ExpectedVersion {
		return biz.Attachment{}, biz.Fail(biz.VersionConflict, "attachment version changed")
	}
	row, e = q.ConfirmAttachment(ctx, sqlcgen.ConfirmAttachmentParams{TenantID: r.TenantID, AttachmentID: r.AttachmentID, PodName: r.PodName, PodUid: r.PodUID})
	if e != nil {
		return biz.Attachment{}, databaseFailure(e)
	}
	if e = attachmentHistory(ctx, q, row, "confirm_requested"); e != nil {
		return biz.Attachment{}, databaseFailure(e)
	}
	if e = tx.Commit(ctx); e != nil {
		return biz.Attachment{}, databaseFailure(e)
	}
	return attachment(row)
}
func (p *Postgres) ReleaseAttachment(ctx context.Context, r biz.ReleaseAttachment) (biz.Attachment, error) {
	tx, e := p.pool.Begin(ctx)
	if e != nil {
		return biz.Attachment{}, databaseFailure(e)
	}
	defer tx.Rollback(ctx)
	q := p.queries.WithTx(tx)
	row, e := lockAttachmentParents(ctx, q, r.TenantID, r.AttachmentID)
	if e != nil {
		return biz.Attachment{}, databaseFailure(e)
	}
	if row.FinalizationID != nil {
		if *row.FinalizationID != r.FinalizationID {
			return biz.Attachment{}, biz.Fail(biz.AttachmentConflict, "attachment has another finalization identity")
		}
		return managedAttachment(ctx, tx, row)
	}
	if row.Version != r.ExpectedVersion {
		return biz.Attachment{}, biz.Fail(biz.VersionConflict, "attachment version changed")
	}
	row, e = q.ReleaseAttachment(ctx, sqlcgen.ReleaseAttachmentParams{TenantID: r.TenantID, AttachmentID: r.AttachmentID, FinalizationID: &r.FinalizationID})
	if e != nil {
		return biz.Attachment{}, databaseFailure(e)
	}
	if e = attachmentHistory(ctx, q, row, "release_requested"); e != nil {
		return biz.Attachment{}, databaseFailure(e)
	}
	if e = tx.Commit(ctx); e != nil {
		return biz.Attachment{}, databaseFailure(e)
	}
	return managedAttachment(ctx, p.pool, row)
}
