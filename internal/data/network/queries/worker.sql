-- Schedule both resource kinds by due time. Always lock the parent VPC first,
-- including for Subnet work, preserving the common parent -> child lock order.
-- This worker-only query crosses tenants; subsequent operations use its scope.
-- name: LockDueResourceParent :one
SELECT v.tenant_id, v.vpc_id, r.subnet_id
FROM network_reconciliations r
LEFT JOIN network_subnets s ON s.tenant_id=r.tenant_id AND s.subnet_id=r.subnet_id
JOIN network_vpcs v ON v.tenant_id=r.tenant_id AND v.vpc_id=coalesce(r.vpc_id,s.vpc_id)
WHERE r.next_run_at <= clock_timestamp()
  AND (r.lease_until IS NULL OR r.lease_until <= clock_timestamp())
ORDER BY r.next_run_at, coalesce(r.vpc_id,r.subnet_id,r.eip_id,r.snat_id,r.lb_id,r.vnic_id)
LIMIT 1 FOR UPDATE OF v SKIP LOCKED;

-- name: LockVPC :one
SELECT * FROM network_vpcs WHERE tenant_id=$1 AND vpc_id=$2 FOR UPDATE;

-- name: AcquireLease :one
UPDATE network_reconciliations
SET lease_owner=sqlc.arg(owner)::uuid, lease_epoch=lease_epoch+1,
    lease_until=clock_timestamp()+sqlc.arg(lease_micros)::bigint*interval '1 microsecond'
WHERE tenant_id=sqlc.arg(tenant_id) AND coalesce(vpc_id,subnet_id,eip_id,snat_id,lb_id,vnic_id)=sqlc.arg(resource_id)::text
  AND (lease_until IS NULL OR lease_until <= clock_timestamp())
RETURNING *;

-- name: CheckLease :one
SELECT * FROM network_reconciliations
WHERE tenant_id=$1 AND coalesce(vpc_id,subnet_id,eip_id,snat_id,lb_id,vnic_id)=sqlc.arg(resource_id)::text AND lease_owner=sqlc.narg(lease_owner) AND lease_epoch=sqlc.arg(lease_epoch)
  AND lease_until > clock_timestamp()
FOR UPDATE;

-- name: RunOperation :one
UPDATE network_operations SET state='running', attempt=attempt+1,
    execution_epoch=sqlc.arg(epoch), updated_at=clock_timestamp()
WHERE tenant_id=sqlc.arg(tenant_id) AND coalesce(vpc_id,subnet_id,eip_id,snat_id,lb_id,vnic_id)=sqlc.arg(resource_id)::text
  AND operation_id=sqlc.arg(operation_id) AND state NOT IN ('succeeded','failed')
RETURNING *;

-- name: GetBinding :one
SELECT * FROM network_provider_bindings WHERE tenant_id=sqlc.arg(tenant_id) AND coalesce(vpc_id,subnet_id,eip_id,snat_id,lb_id,vnic_id)=sqlc.arg(resource_id)::text;

-- name: BeginProviderMutation :execrows
UPDATE network_provider_bindings
SET pending_action=sqlc.arg(action), pending_since=clock_timestamp(),
    create_dispatched=create_dispatched OR sqlc.arg(action)::text='create',
    provider_uid=CASE WHEN provider_uid='' THEN sqlc.arg(identity)::text ELSE provider_uid END
WHERE tenant_id=sqlc.arg(tenant_id) AND coalesce(vpc_id,subnet_id,eip_id,snat_id,lb_id,vnic_id)=sqlc.arg(resource_id)::text AND binding_id=sqlc.arg(binding_id)
  AND (provider_uid='' OR provider_uid=sqlc.arg(identity))
  AND ((sqlc.arg(action)::text='create' AND (pending_action='' OR (pending_action='create' AND provider_uid='')))
       OR (sqlc.arg(action)='update' AND provider_uid<>'' AND pending_action IN ('','update'))
       OR (sqlc.arg(action)='delete' AND (pending_action IN ('','delete') OR sqlc.arg(identity)::text<>'')));

-- name: SaveBindingObservation :execrows
UPDATE network_provider_bindings
SET provider_uid=CASE WHEN provider_uid='' THEN sqlc.arg(identity)::text ELSE provider_uid END,
    pending_action=CASE WHEN sqlc.arg(clear_pending)::boolean THEN '' ELSE pending_action END,
    pending_since=CASE WHEN sqlc.arg(clear_pending)::boolean THEN NULL ELSE pending_since END
WHERE tenant_id=sqlc.arg(tenant_id) AND coalesce(vpc_id,subnet_id,eip_id,snat_id,lb_id,vnic_id)=sqlc.arg(resource_id)::text AND binding_id=sqlc.arg(binding_id)
  AND (provider_uid='' OR provider_uid=sqlc.arg(identity));

-- name: AdvanceVPC :one
UPDATE network_vpcs
SET state=sqlc.arg(state), reason=sqlc.arg(reason), version=version+1, updated_at=clock_timestamp(),
    observed_at=CASE WHEN sqlc.arg(observed)::boolean THEN sqlc.narg(observed_at)::timestamptz ELSE observed_at END
WHERE tenant_id=sqlc.arg(tenant_id) AND vpc_id=sqlc.arg(vpc_id) AND version=sqlc.arg(version)
RETURNING *;

-- name: CompleteAttempt :execrows
UPDATE network_operations
SET state=sqlc.arg(state), reason=sqlc.arg(reason), updated_at=clock_timestamp(),
    completed_at=CASE WHEN sqlc.arg(state)::text IN ('succeeded','failed') THEN clock_timestamp() ELSE NULL END,
    next_attempt_at=CASE WHEN sqlc.arg(state)::text IN ('succeeded','failed') THEN NULL
      ELSE clock_timestamp()+sqlc.arg(delay_micros)::bigint*interval '1 microsecond' END
WHERE tenant_id=sqlc.arg(tenant_id) AND coalesce(vpc_id,subnet_id,eip_id,snat_id,lb_id,vnic_id)=sqlc.arg(resource_id)::text
  AND operation_id=sqlc.arg(operation_id) AND execution_epoch=sqlc.arg(epoch)
  AND state NOT IN ('succeeded','failed');

-- name: ReleaseLease :execrows
UPDATE network_reconciliations
SET lease_owner=NULL, lease_until=NULL,
    processed_generation=greatest(processed_generation,sqlc.arg(claimed_generation)::bigint),
    observation_error=CASE WHEN sqlc.arg(observed)::boolean THEN '' WHEN sqlc.arg(observation_error)::text<>'' THEN sqlc.arg(observation_error)::text ELSE observation_error END,
    retired=retired OR sqlc.arg(terminal)::boolean,
    retry_not_before=CASE WHEN sqlc.arg(backoff)::boolean THEN sqlc.arg(now)::timestamptz+sqlc.arg(delay_micros)::bigint*interval '1 microsecond' ELSE '1970-01-01 UTC'::timestamptz END,
    next_run_at=CASE WHEN sqlc.arg(terminal)::boolean THEN 'infinity'::timestamptz
      WHEN sqlc.arg(backoff)::boolean THEN sqlc.arg(now)::timestamptz+sqlc.arg(delay_micros)::bigint*interval '1 microsecond'
      WHEN requested_generation>sqlc.arg(claimed_generation)::bigint THEN sqlc.arg(now)::timestamptz
      WHEN sqlc.arg(park)::boolean THEN 'infinity'::timestamptz
      ELSE sqlc.arg(now)::timestamptz+sqlc.arg(delay_micros)::bigint*interval '1 microsecond' END
WHERE tenant_id=sqlc.arg(tenant_id) AND coalesce(vpc_id,subnet_id,eip_id,snat_id,lb_id,vnic_id)=sqlc.arg(resource_id)::text
  AND lease_owner=sqlc.arg(owner) AND lease_epoch=sqlc.arg(epoch) AND lease_until>clock_timestamp();

-- name: AdmitDeletion :one
UPDATE network_vpcs SET state='deleting', reason='', last_operation_id=sqlc.arg(operation_id),
    version=version+1, updated_at=clock_timestamp()
WHERE tenant_id=sqlc.arg(tenant_id) AND vpc_id=sqlc.arg(vpc_id) AND version=sqlc.arg(version)
RETURNING *;

-- name: ScheduleDeletion :execrows
UPDATE network_reconciliations SET next_run_at=clock_timestamp(),
    lease_owner=NULL, lease_until=NULL, lease_epoch=lease_epoch+1
WHERE tenant_id=sqlc.arg(tenant_id) AND coalesce(vpc_id,subnet_id,eip_id,snat_id,lb_id,vnic_id)=sqlc.arg(resource_id)::text;

-- name: ReincarnateLBBinding :execrows
UPDATE network_provider_bindings b SET binding_id=sqlc.arg(new_binding_id),provider_name=sqlc.arg(new_provider_name),provider_uid='',
 create_dispatched=false,pending_action='',pending_since=NULL
WHERE b.tenant_id=sqlc.arg(tenant_id) AND b.lb_id=sqlc.arg(lb_id)::text AND b.binding_id=sqlc.arg(old_binding_id) AND b.provider_uid=sqlc.arg(old_provider_uid) AND b.pending_action=''
AND EXISTS(SELECT 1 FROM network_lb_retired_bindings r JOIN network_load_balancers l USING(tenant_id,cluster_id,namespace,lb_id)
 WHERE r.tenant_id=b.tenant_id AND r.lb_id=b.lb_id AND r.binding_id=b.binding_id AND r.provider_uid=b.provider_uid
 AND r.provider_name=b.provider_name AND r.cleanup_target_version=l.applied_version AND l.configuration_state='waiting_for_backends');
