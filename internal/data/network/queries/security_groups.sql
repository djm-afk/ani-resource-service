-- name: GetSecurityGroupRow :one
SELECT * FROM network_security_groups WHERE tenant_id=$1 AND security_group_id=$2;
-- name: LockSecurityGroupRow :one
SELECT * FROM network_security_groups WHERE tenant_id=$1 AND security_group_id=$2 FOR UPDATE;
-- name: InsertSecurityGroupRow :exec
INSERT INTO network_security_groups(tenant_id,security_group_id,version,state,payload) VALUES($1,$2,$3,$4,$5);
-- name: UpdateSecurityGroupRow :execrows
UPDATE network_security_groups SET version=sqlc.arg(new_version),state=sqlc.arg(state),payload=sqlc.arg(payload)
WHERE tenant_id=sqlc.arg(tenant_id) AND security_group_id=sqlc.arg(security_group_id) AND version=sqlc.arg(expected_version);
-- name: ListSecurityGroupRows :many
SELECT * FROM network_security_groups WHERE tenant_id=sqlc.arg(tenant_id) AND state<>'deleted'
AND (sqlc.arg(name_filter)::text='' OR payload->>'Name' ILIKE '%'||sqlc.arg(name_filter)||'%')
AND (sqlc.arg(state_filter)::text='' OR state=sqlc.arg(state_filter)) ORDER BY security_group_id
LIMIT sqlc.arg(max_results)::integer OFFSET sqlc.arg(page_offset)::bigint;
-- name: CountSecurityGroupRows :one
SELECT count(*) FROM network_security_groups WHERE tenant_id=sqlc.arg(tenant_id) AND state<>'deleted'
AND (sqlc.arg(name_filter)::text='' OR payload->>'Name' ILIKE '%'||sqlc.arg(name_filter)||'%')
AND (sqlc.arg(state_filter)::text='' OR state=sqlc.arg(state_filter));
-- name: LockSecurityGroupRequest :exec
SELECT pg_advisory_xact_lock(hashtextextended(sqlc.arg(tenant_id)::text||':'||sqlc.arg(action)::text||':'||sqlc.arg(request_key)::text,84013023));
-- name: GetSecurityGroupReplay :one
SELECT fingerprint,response FROM network_security_group_idempotency WHERE tenant_id=$1 AND action=$2 AND idempotency_key=$3;
-- name: InsertSecurityGroupReplay :exec
INSERT INTO network_security_group_idempotency(tenant_id,action,idempotency_key,fingerprint,response) VALUES($1,$2,$3,$4,$5);
-- name: InsertSecurityGroupOperation :exec
INSERT INTO network_security_group_operations(tenant_id,operation_id,payload,actor_ref,caller_ref,intent) VALUES($1,$2,$3,$4,$5,$6);
-- name: UpdateSecurityGroupOperation :execrows
UPDATE network_security_group_operations SET payload=$3 WHERE tenant_id=$1 AND operation_id=$2 AND (payload->>'CompletedAt') IS NULL;
-- name: GetSecurityGroupOperation :one
SELECT payload FROM network_security_group_operations WHERE tenant_id=$1 AND operation_id=$2;
-- name: GetSecurityGroupTarget :one
SELECT * FROM network_security_group_targets WHERE tenant_id=$1 AND target_type=$2 AND target_id=$3;
-- name: LockSecurityGroupTarget :one
SELECT * FROM network_security_group_targets WHERE tenant_id=$1 AND target_type=$2 AND target_id=$3 FOR UPDATE;
-- name: UpsertSecurityGroupTarget :exec
INSERT INTO network_security_group_targets(tenant_id,target_type,target_id,version,payload) VALUES($1,$2,$3,$4,$5)
ON CONFLICT(tenant_id,target_type,target_id) DO UPDATE SET version=excluded.version,payload=excluded.payload;
-- name: ReserveSecurityGroupReference :exec
INSERT INTO network_security_group_references(tenant_id,target_type,target_id,security_group_id) VALUES($1,$2,$3,$4)
ON CONFLICT(tenant_id,target_type,target_id,security_group_id) DO UPDATE SET released=false;
-- name: ReleaseSecurityGroupReferences :exec
UPDATE network_security_group_references SET released=true WHERE tenant_id=$1 AND target_type=$2 AND target_id=$3 AND security_group_id<>ALL(coalesce(sqlc.arg(keep_ids)::text[],ARRAY[]::text[]));
-- name: SecurityGroupReferenceCount :one
SELECT count(*) FROM network_security_group_references WHERE tenant_id=$1 AND security_group_id=$2 AND NOT released;
-- name: ListSecurityGroupTargetRows :many
SELECT t.* FROM network_security_group_targets t JOIN network_security_group_references r USING(tenant_id,target_type,target_id)
WHERE r.tenant_id=sqlc.arg(tenant_id) AND r.security_group_id=sqlc.arg(security_group_id) AND NOT r.released
AND (sqlc.arg(target_filter)::text='' OR t.target_type=sqlc.arg(target_filter)) ORDER BY t.target_type,t.target_id;
-- name: ScheduleSecurityGroup :exec
INSERT INTO network_security_group_reconciliations(tenant_id,target_type,target_id,requested_version) VALUES($1,$2,$3,$4)
ON CONFLICT(tenant_id,target_type,target_id) DO UPDATE SET requested_version=excluded.requested_version,requested_generation=network_security_group_reconciliations.requested_generation+1,next_run_at=clock_timestamp();
-- name: LockSecurityGroupTask :one
SELECT * FROM network_security_group_reconciliations WHERE tenant_id=$1 AND target_type=$2 AND target_id=$3 FOR UPDATE;
-- name: ClaimSecurityGroupTask :one
SELECT * FROM network_security_group_reconciliations WHERE next_run_at<=clock_timestamp() AND (lease_until IS NULL OR lease_until<=clock_timestamp())
ORDER BY next_run_at LIMIT 1 FOR UPDATE SKIP LOCKED;
-- name: LeaseSecurityGroupTask :one
UPDATE network_security_group_reconciliations SET lease_owner=sqlc.arg(owner)::uuid,lease_epoch=lease_epoch+1,
lease_until=clock_timestamp()+sqlc.arg(lease_micros)::bigint*interval '1 microsecond'
WHERE tenant_id=sqlc.arg(tenant_id) AND target_type=sqlc.arg(target_type) AND target_id=sqlc.arg(target_id) RETURNING *;
-- name: FinishSecurityGroupTask :execrows
UPDATE network_security_group_reconciliations SET lease_owner=NULL,lease_until=NULL,
 processed_generation=greatest(processed_generation,sqlc.arg(claimed_generation)::bigint),
 observation_error=CASE WHEN sqlc.arg(observed)::boolean THEN '' WHEN sqlc.arg(observation_error)::text<>'' THEN sqlc.arg(observation_error)::text ELSE observation_error END,
 retry_not_before=CASE WHEN sqlc.arg(backoff)::boolean THEN sqlc.arg(now)::timestamptz+sqlc.arg(delay_micros)::bigint*interval '1 microsecond' ELSE '1970-01-01 UTC'::timestamptz END,
 next_run_at=CASE WHEN sqlc.arg(terminal)::boolean THEN 'infinity'::timestamptz
   WHEN sqlc.arg(backoff)::boolean THEN sqlc.arg(now)::timestamptz+sqlc.arg(delay_micros)::bigint*interval '1 microsecond'
   WHEN requested_generation>sqlc.arg(claimed_generation)::bigint THEN sqlc.arg(now)::timestamptz
   WHEN sqlc.arg(park)::boolean THEN 'infinity'::timestamptz
   ELSE sqlc.arg(now)::timestamptz+sqlc.arg(delay_micros)::bigint*interval '1 microsecond' END
WHERE tenant_id=sqlc.arg(tenant_id) AND target_type=sqlc.arg(target_type) AND target_id=sqlc.arg(target_id)
AND lease_owner=sqlc.arg(owner)::uuid AND lease_epoch=sqlc.arg(epoch) AND requested_version=sqlc.arg(version) AND lease_until>clock_timestamp();

-- name: GetSecurityGroupConsumerAttachment :one
SELECT * FROM network_attachments WHERE tenant_id=sqlc.arg(tenant_id) AND cluster_id=sqlc.arg(cluster_id)
 AND namespace=sqlc.arg(namespace) AND instance_id=sqlc.arg(instance_id) AND consumer_type=sqlc.arg(consumer_type)
 AND slot='primary' AND state='attached' ORDER BY updated_at DESC,attachment_id DESC LIMIT 1;

-- name: MapSecurityGroupProviderNames :many
SELECT security_group_id FROM network_security_groups WHERE tenant_id=sqlc.arg(tenant_id)
 AND payload->>'ClusterID'=sqlc.arg(cluster_id)::text AND payload->>'Namespace'=sqlc.arg(namespace)::text
 AND payload->>'ProviderName'=ANY(sqlc.arg(provider_names)::text[]) ORDER BY security_group_id;

-- name: RetireSecurityGroupTask :exec
UPDATE network_security_group_reconciliations SET next_run_at='infinity'::timestamptz,
 lease_owner=NULL,lease_until=NULL,lease_epoch=lease_epoch+1,processed_generation=requested_generation
WHERE tenant_id=sqlc.arg(tenant_id) AND target_type=sqlc.arg(target_type) AND target_id=sqlc.arg(target_id);
