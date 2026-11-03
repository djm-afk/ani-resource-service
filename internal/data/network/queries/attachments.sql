-- name: GetAttachment :one
SELECT * FROM network_attachments WHERE tenant_id=$1 AND attachment_id=$2;
-- name: LockAttachment :one
SELECT * FROM network_attachments WHERE tenant_id=$1 AND attachment_id=$2 FOR UPDATE;
-- name: GetAttachmentReplay :one
SELECT * FROM network_attachments WHERE tenant_id=$1 AND instance_id=$2 AND slot=$3 AND request_key=$4;
-- name: LockAttachmentSlot :exec
SELECT pg_advisory_xact_lock(hashtextextended(sqlc.arg(tenant_id)::text||':attachment:'||sqlc.arg(instance_id)::text||':'||sqlc.arg(slot)::text,0));
-- name: HasAttachmentSlot :one
SELECT EXISTS(SELECT 1 FROM network_attachments WHERE tenant_id=$1 AND instance_id=$2 AND slot=$3 AND (state<>'released' OR protocol_blocked));
-- name: CountAttachments :one
SELECT count(*)::bigint FROM network_attachments WHERE tenant_id=$1 AND subnet_id=$2 AND (state<>'released' OR protocol_blocked);
-- name: InsertAttachment :one
INSERT INTO network_attachments(tenant_id,attachment_id,vpc_id,subnet_id,binding_id,instance_id,slot,request_key,submission_id,generation,fingerprint,cluster_id,namespace,binding_revision,plan,consumer_type,state)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,'reserved') RETURNING *;
-- name: ConfirmAttachment :one
UPDATE network_attachments SET pod_name=$3,pod_uid=$4,confirm_uid=$4,version=version+1,updated_at=clock_timestamp(),next_check_at=clock_timestamp(),lease_owner=NULL,lease_until=NULL
WHERE tenant_id=$1 AND attachment_id=$2 RETURNING *;
-- name: ReleaseAttachment :one
UPDATE network_attachments SET finalization_id=$3,state='releasing',version=version+1,updated_at=clock_timestamp(),next_check_at=clock_timestamp(),lease_owner=NULL,lease_until=NULL
WHERE tenant_id=$1 AND attachment_id=$2 RETURNING *;
-- name: InsertAttachmentHistory :exec
INSERT INTO network_attachment_history(tenant_id,history_id,attachment_id,version,event,state,reason,pod_uid,finalization_id)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9);
-- name: LockDueAttachmentParent :one
SELECT v.* FROM network_vpcs v WHERE EXISTS(SELECT 1 FROM network_attachments a WHERE a.tenant_id=v.tenant_id AND a.vpc_id=v.vpc_id AND a.next_check_at<=clock_timestamp() AND (a.lease_until IS NULL OR a.lease_until<=clock_timestamp()))
ORDER BY (SELECT min(a.next_check_at) FROM network_attachments a WHERE a.tenant_id=v.tenant_id AND a.vpc_id=v.vpc_id AND (a.lease_until IS NULL OR a.lease_until<=clock_timestamp())),v.vpc_id LIMIT 1 FOR UPDATE OF v SKIP LOCKED;
-- name: LockDueAttachmentSubnet :one
SELECT s.* FROM network_subnets s WHERE s.tenant_id=$1 AND s.vpc_id=$2 AND EXISTS(SELECT 1 FROM network_attachments a WHERE a.tenant_id=s.tenant_id AND a.subnet_id=s.subnet_id AND a.next_check_at<=clock_timestamp() AND (a.lease_until IS NULL OR a.lease_until<=clock_timestamp()))
ORDER BY (SELECT min(a.next_check_at) FROM network_attachments a WHERE a.tenant_id=s.tenant_id AND a.subnet_id=s.subnet_id AND (a.lease_until IS NULL OR a.lease_until<=clock_timestamp())),s.subnet_id LIMIT 1 FOR UPDATE OF s SKIP LOCKED;
-- name: LockDueAttachment :one
SELECT * FROM network_attachments WHERE tenant_id=$1 AND subnet_id=$2 AND next_check_at<=clock_timestamp() AND (lease_until IS NULL OR lease_until<=clock_timestamp())
ORDER BY next_check_at,attachment_id LIMIT 1 FOR UPDATE SKIP LOCKED;
-- name: ClaimAttachment :one
UPDATE network_attachments SET lease_owner=sqlc.arg(owner)::uuid,lease_until=clock_timestamp()+sqlc.arg(lease_micros)::bigint*interval '1 microsecond',epoch=epoch+1
WHERE tenant_id=sqlc.arg(tenant_id) AND attachment_id=sqlc.arg(attachment_id) RETURNING *;
-- name: FinishAttachment :one
UPDATE network_attachments SET state=sqlc.arg(state),reason=sqlc.arg(reason),protocol_blocked=sqlc.arg(protocol_blocked),pod_name=sqlc.arg(pod_name),pod_uid=sqlc.arg(pod_uid),finalization_id=sqlc.narg(finalization_id),
 provider_relations=sqlc.arg(provider_relations),version=version+1,updated_at=clock_timestamp(),
 observed_at=CASE WHEN sqlc.arg(observed)::boolean THEN sqlc.narg(observed_at)::timestamptz ELSE observed_at END,
 released_at=CASE WHEN sqlc.arg(state)::text='released' THEN coalesce(released_at,clock_timestamp()) ELSE NULL END,
 processed_generation=greatest(processed_generation,sqlc.arg(covered_generation)::bigint),
 evidence_hash=CASE WHEN sqlc.arg(observed)::boolean THEN sqlc.arg(evidence_hash)::text ELSE evidence_hash END,
 evidence_applied_at=CASE WHEN sqlc.arg(observed)::boolean THEN clock_timestamp() ELSE evidence_applied_at END,
 retry_not_before=CASE WHEN sqlc.arg(backoff)::boolean THEN clock_timestamp()+sqlc.arg(delay_micros)::bigint*interval '1 microsecond' ELSE '1970-01-01 UTC'::timestamptz END,
 next_check_at=CASE WHEN requested_generation>sqlc.arg(covered_generation)::bigint AND NOT sqlc.arg(backoff)::boolean THEN clock_timestamp() WHEN sqlc.arg(park)::boolean THEN 'infinity'::timestamptz ELSE clock_timestamp()+sqlc.arg(delay_micros)::bigint*interval '1 microsecond' END,lease_owner=NULL,lease_until=NULL
WHERE tenant_id=sqlc.arg(tenant_id) AND attachment_id=sqlc.arg(attachment_id) AND version=sqlc.arg(version) AND epoch=sqlc.arg(epoch) AND lease_owner=sqlc.arg(owner)::uuid AND lease_until>clock_timestamp() RETURNING *;
-- name: AttachmentVPCScopeExists :one
SELECT EXISTS(SELECT 1 FROM network_vpcs WHERE tenant_id=$1 AND vpc_id=$2)::boolean AS exists;
-- name: CountAttachmentConsumers :one
SELECT count(*)::bigint FROM network_attachments a
WHERE a.tenant_id=sqlc.arg(tenant_id)
  AND (a.subnet_id=sqlc.arg(subnet_id)::text OR a.vpc_id=sqlc.arg(vpc_id)::text)
  AND (a.state<>'released' OR a.protocol_blocked);
-- name: ListAttachmentConsumers :many
SELECT a.tenant_id,a.attachment_id,a.vpc_id,a.subnet_id,a.slot,a.state,a.reason,a.protocol_blocked,
 a.instance_id,a.consumer_type,a.pod_uid,a.observed_at,a.created_at,a.provider_relations,
 coalesce(a.plan->>'WorkloadMode','')::text AS workload_mode,
 coalesce(s.name,'')::text AS subnet_name,
 cs.snapshot AS consumer_snapshot
FROM network_attachments a
LEFT JOIN network_subnets s ON s.tenant_id=a.tenant_id AND s.subnet_id=a.subnet_id
LEFT JOIN network_attachment_consumer_snapshots cs ON cs.tenant_id=a.tenant_id AND cs.attachment_id=a.attachment_id
WHERE a.tenant_id=sqlc.arg(tenant_id)
  AND (a.subnet_id=sqlc.arg(subnet_id)::text OR a.vpc_id=sqlc.arg(vpc_id)::text)
  AND (a.state<>'released' OR a.protocol_blocked)
ORDER BY a.created_at DESC,a.attachment_id DESC
LIMIT sqlc.arg(max_results)::integer OFFSET sqlc.arg(page_offset)::bigint;
