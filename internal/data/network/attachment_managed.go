package data

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
)

type attachmentSnapshotReader interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}
type attachmentSnapshotWriter interface {
	attachmentSnapshotReader
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

func managedAttachment(ctx context.Context, db attachmentSnapshotReader, row sqlcgen.NetworkAttachment) (biz.Attachment, error) {
	a, err := attachment(row)
	if err != nil || (a.WorkloadMode != "controller_managed" && a.WorkloadMode != "vm_managed") {
		return a, err
	}
	var snapshot []byte
	err = db.QueryRow(ctx, `SELECT snapshot_revision,snapshot,confirmed_snapshot_revision,confirmed_pod_uid FROM network_attachment_consumer_snapshots WHERE tenant_id=$1 AND attachment_id=$2`, a.TenantID, a.ID).Scan(&a.SnapshotRevision, &snapshot, &a.ConfirmedSnapshotRevision, &a.ConfirmedPodUID)
	if err != nil {
		return biz.Attachment{}, databaseFailure(err)
	}
	if len(snapshot) > 0 {
		a.ConsumerSnapshot = &biz.ConsumerSubmission{}
		if err = json.Unmarshal(snapshot, a.ConsumerSnapshot); err != nil {
			return biz.Attachment{}, databaseFailure(err)
		}
	}
	if a.WorkloadMode == "vm_managed" {
		a.VMCurrentAddresses = vmCurrentAddresses(a)
	}
	return a, nil
}
func persistConsumerSnapshot(ctx context.Context, db attachmentSnapshotWriter, a biz.Attachment, c biz.ConsumerSubmission) error {
	if biz.CompareConsumerSnapshot(a, c) != biz.SnapshotAccepted {
		return biz.ErrLeaseLost
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	tag, err := db.Exec(ctx, `UPDATE network_attachment_consumer_snapshots SET snapshot_revision=$3,snapshot=$4 WHERE tenant_id=$1 AND attachment_id=$2 AND snapshot_revision<=$3`, a.TenantID, a.ID, c.SnapshotRevision, b)
	if err != nil {
		return databaseFailure(err)
	}
	if tag.RowsAffected() != 1 {
		return biz.ErrLeaseLost
	}
	return nil
}
func (p *Postgres) confirmManagedAttachment(ctx context.Context, tx pgx.Tx, q *sqlcgen.Queries, row sqlcgen.NetworkAttachment, r biz.ConfirmAttachment) (biz.Attachment, error) {
	a, err := managedAttachment(ctx, tx, row)
	if err != nil {
		return biz.Attachment{}, err
	}
	if a.State == biz.Releasing || a.State == biz.Released {
		return biz.Attachment{}, biz.Fail(biz.AttachmentConflict, "attachment is finalizing")
	}
	if r.ExpectedSnapshotRevision < 1 || r.ExpectedSnapshotRevision != a.SnapshotRevision || a.ConsumerSnapshot == nil {
		return biz.Attachment{}, biz.Fail(biz.VersionConflict, "consumer snapshot revision changed")
	}
	c := a.ConsumerSnapshot
	if c.State != "open" || c.DesiredRunState != "running" || len(c.CurrentPods) != 1 || c.CurrentPods[0].Reference.UID != r.PodUID || c.CurrentPods[0].Reference.Name != r.PodName {
		return biz.Attachment{}, biz.Fail(biz.AttachmentConflict, "Pod is not current in consumer snapshot")
	}
	var confirmedRevision int64
	var confirmedUID string
	if err = tx.QueryRow(ctx, `SELECT confirmed_snapshot_revision,confirmed_pod_uid FROM network_attachment_consumer_snapshots WHERE tenant_id=$1 AND attachment_id=$2`, a.TenantID, a.ID).Scan(&confirmedRevision, &confirmedUID); err != nil {
		return biz.Attachment{}, databaseFailure(err)
	}
	if confirmedRevision == r.ExpectedSnapshotRevision && confirmedUID == r.PodUID && a.State == biz.Attached && a.PodUID == r.PodUID && a.PodName == r.PodName {
		return a, nil
	}
	if row.Version != r.ExpectedVersion && (confirmedRevision != r.ExpectedSnapshotRevision || confirmedUID != r.PodUID) {
		return biz.Attachment{}, biz.Fail(biz.VersionConflict, "attachment version changed")
	}
	_, err = tx.Exec(ctx, `UPDATE network_attachment_consumer_snapshots SET confirmed_snapshot_revision=$3,confirmed_pod_uid=$4 WHERE tenant_id=$1 AND attachment_id=$2`, a.TenantID, a.ID, r.ExpectedSnapshotRevision, r.PodUID)
	if err != nil {
		return biz.Attachment{}, databaseFailure(err)
	}
	_, err = tx.Exec(ctx, `UPDATE network_attachments SET version=version+1,state='attached',pod_name=$3,pod_uid=$4,confirm_uid=$4,reason='',current_vnic_name=CASE WHEN pod_uid=$4 THEN current_vnic_name ELSE '' END,current_vnic_uid=CASE WHEN pod_uid=$4 THEN current_vnic_uid ELSE '' END,current_mac_address=CASE WHEN pod_uid=$4 THEN current_mac_address ELSE '' END,current_ips=CASE WHEN pod_uid=$4 THEN current_ips ELSE '{}'::text[] END,updated_at=clock_timestamp(),next_check_at=clock_timestamp(),lease_owner=NULL,lease_until=NULL WHERE tenant_id=$1 AND attachment_id=$2`, a.TenantID, a.ID, r.PodName, r.PodUID)
	if err != nil {
		return biz.Attachment{}, databaseFailure(err)
	}
	row, err = q.GetAttachment(ctx, sqlcgen.GetAttachmentParams{TenantID: a.TenantID, AttachmentID: a.ID})
	if err != nil {
		return biz.Attachment{}, databaseFailure(err)
	}
	if err = attachmentHistory(ctx, q, row, "confirm_requested"); err != nil {
		return biz.Attachment{}, databaseFailure(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return biz.Attachment{}, databaseFailure(err)
	}
	return managedAttachment(ctx, p.pool, row)
}
