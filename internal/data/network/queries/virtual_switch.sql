-- name: LockVirtualSwitchKey :exec
SELECT pg_advisory_xact_lock(hashtextextended('virtual_switch:'||sqlc.arg(idempotency_key)::text,0));

-- name: GetVirtualSwitchReplay :one
SELECT * FROM network_virtual_switch_idempotency WHERE operation_kind='create_virtual_switch' AND idempotency_key=sqlc.arg(idempotency_key);

-- name: InsertVirtualSwitch :one
INSERT INTO network_virtual_switches(resource_id,name,created_at,updated_at,last_operation_id)
 VALUES(sqlc.arg(resource_id),sqlc.arg(name),sqlc.arg(created_at),sqlc.arg(created_at),sqlc.arg(operation_id)) RETURNING *;

-- name: InsertVirtualSwitchOperation :exec
INSERT INTO network_virtual_switch_operations(operation_id,resource_id,kind,state,created_at,updated_at,completed_at)
 VALUES(sqlc.arg(operation_id),sqlc.arg(resource_id),sqlc.arg(kind),sqlc.arg(state),sqlc.arg(created_at),sqlc.arg(created_at),sqlc.narg(completed_at));

-- name: InsertVirtualSwitchReplay :exec
INSERT INTO network_virtual_switch_idempotency(operation_kind,idempotency_key,fingerprint,resource_id,operation_id,response,created_at)
 VALUES('create_virtual_switch',sqlc.arg(idempotency_key),sqlc.arg(fingerprint),sqlc.arg(resource_id),sqlc.arg(operation_id),sqlc.arg(response),sqlc.arg(created_at));

-- name: InsertVirtualSwitchHistory :exec
INSERT INTO network_virtual_switch_history(history_id,resource_id,operation_id,event,actor_ref,caller_ref,detail,created_at)
 VALUES(sqlc.arg(history_id),sqlc.arg(resource_id),sqlc.narg(operation_id),sqlc.arg(event),sqlc.arg(actor_ref),sqlc.arg(caller_ref),sqlc.arg(detail),clock_timestamp());

-- name: GetVirtualSwitch :one
SELECT sqlc.embed(s),coalesce(r.observation_error,'')::text AS observation_error FROM network_virtual_switches s LEFT JOIN network_virtual_switch_reconciliations r USING(resource_id) WHERE s.resource_id=sqlc.arg(resource_id);

-- name: GetVirtualSwitchOperation :one
SELECT * FROM network_virtual_switch_operations WHERE operation_id=sqlc.arg(operation_id);

-- name: LockVirtualSwitch :one
SELECT * FROM network_virtual_switches WHERE resource_id=sqlc.arg(resource_id) FOR UPDATE;

-- name: CountVirtualSwitches :one
SELECT count(*) FROM network_virtual_switches WHERE
 (sqlc.arg(name_filter)::text='' OR position(lower(sqlc.arg(name_filter)) in lower(name))>0)
 AND ((sqlc.arg(state_filter)::text='' AND state<>'deleted') OR state=sqlc.arg(state_filter));

-- name: ListVirtualSwitches :many
SELECT sqlc.embed(s),coalesce(r.observation_error,'')::text AS observation_error FROM network_virtual_switches s LEFT JOIN network_virtual_switch_reconciliations r USING(resource_id) WHERE
 (sqlc.arg(name_filter)::text='' OR position(lower(sqlc.arg(name_filter)) in lower(s.name))>0)
 AND ((sqlc.arg(state_filter)::text='' AND s.state<>'deleted') OR s.state=sqlc.arg(state_filter))
 ORDER BY s.created_at DESC,s.resource_id DESC LIMIT sqlc.arg(max_results)::integer OFFSET sqlc.arg(page_offset)::bigint;

-- name: BindVirtualSwitch :one
UPDATE network_virtual_switches SET cluster_id=sqlc.arg(cluster_id),binding_id=sqlc.arg(binding_id),binding_version=1,
 binding_state='pending',membership_fingerprint=sqlc.arg(fingerprint),nodes=sqlc.arg(nodes),version=version+1,updated_at=clock_timestamp()
 WHERE resource_id=sqlc.arg(resource_id) AND version=sqlc.arg(version) AND state='available' AND binding_id IS NULL RETURNING *;

-- name: ScheduleVirtualSwitch :exec
INSERT INTO network_virtual_switch_reconciliations(resource_id,next_run_at) VALUES(sqlc.arg(resource_id),clock_timestamp())
 ON CONFLICT(resource_id) DO UPDATE SET requested_generation=network_virtual_switch_reconciliations.requested_generation+1,next_run_at=least(network_virtual_switch_reconciliations.next_run_at,EXCLUDED.next_run_at);

-- name: VirtualSwitchDependencies :one
SELECT count(*)::bigint FROM (
 SELECT 1 FROM network_vlan_networks v JOIN network_platform_resources p USING(resource_id)
 WHERE v.virtual_switch_id=sqlc.arg(resource_id)::text AND (NOT v.retired OR p.state<>'deleted' OR p.pending_action<>'')
 UNION ALL SELECT 1 FROM network_device_adoptions d JOIN network_platform_resources p USING(resource_id)
 JOIN network_platform_reconciliations r USING(resource_id)
 WHERE d.virtual_switch_id=sqlc.arg(resource_id)::text AND (p.pending_action<>'' OR (r.lease_until IS NOT NULL AND r.lease_until>clock_timestamp()))
) AS active_references;

-- name: DeleteVirtualSwitchMetadata :one
UPDATE network_virtual_switches SET state=sqlc.arg(state),binding_state=sqlc.arg(binding_state),version=version+1,
 last_operation_id=sqlc.arg(operation_id),updated_at=clock_timestamp()
 WHERE resource_id=sqlc.arg(resource_id) AND version=sqlc.arg(version) AND state NOT IN ('deleting','deleted') RETURNING *;

-- name: ClaimVirtualSwitch :one
SELECT s.* FROM network_virtual_switches s JOIN network_virtual_switch_reconciliations r USING(resource_id)
 WHERE r.next_run_at<=clock_timestamp() AND (r.lease_until IS NULL OR r.lease_until<=clock_timestamp())
 AND s.state<>'deleted' ORDER BY r.next_run_at,s.resource_id LIMIT 1 FOR UPDATE OF s SKIP LOCKED;

-- name: LeaseVirtualSwitch :one
UPDATE network_virtual_switch_reconciliations SET lease_owner=sqlc.arg(owner)::uuid,lease_until=clock_timestamp()+sqlc.arg(lease_micros)::bigint*interval '1 microsecond',lease_epoch=lease_epoch+1
 WHERE resource_id=sqlc.arg(resource_id) AND (lease_until IS NULL OR lease_until<=clock_timestamp()) RETURNING *;

-- name: CheckVirtualSwitchLease :one
SELECT * FROM network_virtual_switch_reconciliations WHERE resource_id=sqlc.arg(resource_id)
 AND lease_owner=sqlc.arg(owner)::uuid AND lease_epoch=sqlc.arg(epoch) AND lease_until>clock_timestamp() FOR UPDATE;

-- name: SaveVirtualSwitchNodes :execrows
UPDATE network_virtual_switches SET nodes=sqlc.arg(nodes),updated_at=clock_timestamp()
 WHERE resource_id=sqlc.arg(resource_id) AND version=sqlc.arg(version) AND binding_id=sqlc.arg(binding_id);

-- name: AdvanceVirtualSwitch :execrows
UPDATE network_virtual_switches SET binding_state=sqlc.arg(binding_state),reason=sqlc.arg(reason),
 state=CASE WHEN sqlc.arg(binding_state)::text='cleaned' THEN 'deleted' WHEN state='deleting' THEN state
 WHEN sqlc.arg(binding_state)::text='degraded' THEN 'degraded' ELSE 'available' END,updated_at=clock_timestamp()
 WHERE resource_id=sqlc.arg(resource_id) AND version=sqlc.arg(version);

-- name: CompleteVirtualSwitchDeletion :exec
UPDATE network_virtual_switch_operations SET state='succeeded',reason='',updated_at=clock_timestamp(),completed_at=clock_timestamp()
 WHERE resource_id=sqlc.arg(resource_id) AND kind='delete_virtual_switch' AND completed_at IS NULL;

-- name: ReleaseVirtualSwitchLease :execrows
UPDATE network_virtual_switch_reconciliations SET lease_owner=NULL,lease_until=NULL,
 processed_generation=greatest(processed_generation,sqlc.arg(claimed_generation)::bigint),
 observation_error=CASE WHEN sqlc.arg(observed)::boolean THEN '' WHEN sqlc.arg(observation_error)::text<>'' THEN sqlc.arg(observation_error)::text ELSE observation_error END,
 retry_not_before=CASE WHEN sqlc.arg(backoff)::boolean THEN sqlc.arg(now)::timestamptz+sqlc.arg(delay_micros)::bigint*interval '1 microsecond' ELSE '1970-01-01 UTC'::timestamptz END,
 next_run_at=CASE WHEN sqlc.arg(terminal)::boolean THEN 'infinity'::timestamptz
   WHEN sqlc.arg(backoff)::boolean THEN sqlc.arg(now)::timestamptz+sqlc.arg(delay_micros)::bigint*interval '1 microsecond'
   WHEN requested_generation>sqlc.arg(claimed_generation)::bigint THEN sqlc.arg(now)::timestamptz
   WHEN sqlc.arg(park)::boolean THEN 'infinity'::timestamptz ELSE sqlc.arg(now)::timestamptz+sqlc.arg(delay_micros)::bigint*interval '1 microsecond' END
 WHERE resource_id=sqlc.arg(resource_id) AND lease_owner=sqlc.arg(owner)::uuid AND lease_epoch=sqlc.arg(epoch) AND lease_until>clock_timestamp();
