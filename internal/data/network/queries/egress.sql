-- Tenant commands share permanent idempotency, operations and reconciliation.
-- name: LockEgressKey :exec
SELECT pg_advisory_xact_lock(hashtextextended(sqlc.arg(tenant_id)::text||':'||sqlc.arg(operation_kind)::text||':'||sqlc.arg(idempotency_key)::text,0));

-- name: GetEgressIdempotency :one
SELECT * FROM network_idempotency WHERE tenant_id=$1 AND operation_kind=$2 AND idempotency_key=$3;

-- name: InsertEgressIdempotency :exec
INSERT INTO network_idempotency(tenant_id,operation_kind,idempotency_key,fingerprint,fingerprint_version,eip_id,snat_id,operation_id,response,created_at)
VALUES(sqlc.arg(tenant_id),sqlc.arg(operation_kind),sqlc.arg(idempotency_key),sqlc.arg(fingerprint),1,NULLIF(sqlc.arg(eip_id)::text,''),NULLIF(sqlc.arg(snat_id)::text,''),sqlc.arg(operation_id),sqlc.arg(response),sqlc.arg(created_at));

-- name: InsertEIP :one
INSERT INTO network_eips(tenant_id,eip_id,cluster_id,namespace,name,description,pool_id,pool_revision,scope,managed_by,state,created_at,updated_at,last_operation_id)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,'public','tenant','provisioning',$9,$9,$10) RETURNING *;

-- name: LockEIP :one
SELECT * FROM network_eips WHERE tenant_id=$1 AND eip_id=$2 FOR UPDATE;

-- name: GetEIP :one
SELECT sqlc.embed(e),coalesce(c.snat_id,'')::text AS binding_id,
 coalesce(c.state,'unbound')::text AS binding_state,
 coalesce(c.target_kind,'')::text AS binding_target_kind,
 coalesce(c.snat_id,c.lb_id,'')::text AS binding_target_id,
 coalesce(sn.vpc_id,lb.vpc_id,'')::text AS binding_vpc_id
FROM network_eips e LEFT JOIN network_eip_claims c
 ON c.tenant_id=e.tenant_id AND c.eip_id=e.eip_id AND c.released_at IS NULL
LEFT JOIN network_snat_bindings sn ON sn.tenant_id=c.tenant_id AND sn.snat_id=c.snat_id
LEFT JOIN network_load_balancers lb ON lb.tenant_id=c.tenant_id AND lb.lb_id=c.lb_id
WHERE e.tenant_id=$1 AND e.eip_id=$2 AND e.scope='public' AND e.managed_by='tenant';

-- Worker-only lookup includes system addresses and uses the same claim projection.
-- name: GetEIPClaim :one
SELECT sqlc.embed(e),coalesce(c.snat_id,'')::text AS binding_id,
 coalesce(c.state,'unbound')::text AS binding_state,
 coalesce(c.target_kind,'')::text AS binding_target_kind,
 coalesce(c.snat_id,c.lb_id,'')::text AS binding_target_id,
 coalesce(sn.vpc_id,lb.vpc_id,'')::text AS binding_vpc_id
FROM network_eips e LEFT JOIN network_eip_claims c
 ON c.tenant_id=e.tenant_id AND c.eip_id=e.eip_id AND c.released_at IS NULL
LEFT JOIN network_snat_bindings sn ON sn.tenant_id=c.tenant_id AND sn.snat_id=c.snat_id
LEFT JOIN network_load_balancers lb ON lb.tenant_id=c.tenant_id AND lb.lb_id=c.lb_id
WHERE e.tenant_id=$1 AND e.eip_id=$2;

-- name: ListEIPs :many
SELECT sqlc.embed(e),coalesce(c.snat_id,'')::text AS binding_id,
 coalesce(c.state,'unbound')::text AS binding_state,
 coalesce(c.target_kind,'')::text AS binding_target_kind,
 coalesce(c.snat_id,c.lb_id,'')::text AS binding_target_id,
 coalesce(sn.vpc_id,lb.vpc_id,'')::text AS binding_vpc_id
FROM network_eips e LEFT JOIN network_eip_claims c
 ON c.tenant_id=e.tenant_id AND c.eip_id=e.eip_id AND c.released_at IS NULL
LEFT JOIN network_snat_bindings sn ON sn.tenant_id=c.tenant_id AND sn.snat_id=c.snat_id
LEFT JOIN network_load_balancers lb ON lb.tenant_id=c.tenant_id AND lb.lb_id=c.lb_id
WHERE e.tenant_id=sqlc.arg(tenant_id) AND e.scope='public' AND e.managed_by='tenant'
 AND (sqlc.arg(name_filter)::text='' OR e.name=sqlc.arg(name_filter))
 AND ((sqlc.arg(state_filter)::text='' AND e.state<>'deleted') OR e.state=sqlc.arg(state_filter))
 AND (sqlc.arg(after_id)::text='' OR (e.created_at,e.eip_id)<(sqlc.arg(after_created_at)::timestamptz,sqlc.arg(after_id)::text))
ORDER BY e.created_at DESC,e.eip_id DESC LIMIT sqlc.arg(max_results)::integer;

-- name: AdvanceEIP :one
UPDATE network_eips SET state=sqlc.arg(state),reason=sqlc.arg(reason),version=version+1,updated_at=clock_timestamp(),
 address=CASE WHEN sqlc.arg(address)::text<>'' THEN sqlc.arg(address) ELSE address END,
 observed_at=CASE WHEN sqlc.arg(observed)::boolean THEN sqlc.narg(observed_at)::timestamptz ELSE observed_at END
WHERE tenant_id=sqlc.arg(tenant_id) AND eip_id=sqlc.arg(eip_id) AND version=sqlc.arg(version) RETURNING *;

-- name: AdmitEIPDeletion :one
UPDATE network_eips SET state='deleting',reason='',last_operation_id=sqlc.arg(operation_id),version=version+1,updated_at=clock_timestamp()
WHERE tenant_id=sqlc.arg(tenant_id) AND eip_id=sqlc.arg(eip_id) AND version=sqlc.arg(version) RETURNING *;

-- name: InsertSnat :one
INSERT INTO network_snat_bindings(tenant_id,snat_id,cluster_id,namespace,name,description,vpc_id,eip_id,purpose,desired_enabled,state,created_at,updated_at,last_operation_id)
VALUES($1,$2,$3,$4,'VPC SNAT','',$5,$6,'public',true,'provisioning',$7,$7,$8) RETURNING *;

-- name: GetSnat :one
SELECT sqlc.embed(s),e.address FROM network_snat_bindings s
JOIN network_eips e ON e.tenant_id=s.tenant_id AND e.eip_id=s.eip_id
WHERE s.tenant_id=sqlc.arg(tenant_id) AND s.purpose='public' AND e.scope='public' AND e.managed_by='tenant' AND
 ((NOT sqlc.arg(by_vpc)::boolean AND s.snat_id=sqlc.arg(id)::text) OR
 (sqlc.arg(by_vpc)::boolean AND s.vpc_id=sqlc.arg(id)::text AND s.state<>'deleted'));

-- name: LockSnat :one
SELECT * FROM network_snat_bindings WHERE tenant_id=$1 AND snat_id=$2 FOR UPDATE;

-- name: BlockingSnatForVPC :one
SELECT count(*)::bigint FROM network_snat_bindings WHERE tenant_id=$1 AND vpc_id=$2 AND purpose='public' AND state<>'deleted';

-- name: BlockingSnatForEIP :one
SELECT count(*)::bigint FROM network_eip_claims WHERE tenant_id=$1 AND eip_id=$2 AND released_at IS NULL;

-- The target foreign key and the active EIP index arbitrate SNAT/LB admission.
-- name: ClaimEIPForSnat :execrows
INSERT INTO network_eip_claims(tenant_id,eip_id,cluster_id,namespace,target_kind,snat_id,state,created_at)
VALUES(sqlc.arg(tenant_id),sqlc.arg(eip_id),sqlc.arg(cluster_id),sqlc.arg(namespace),'vpc_snat',sqlc.arg(snat_id)::text,'reserved',sqlc.arg(created_at))
ON CONFLICT DO NOTHING;

-- name: SetSnatIntent :one
UPDATE network_snat_bindings SET desired_enabled=sqlc.arg(enabled),applied_enabled=NULL,state='provisioning',reason='',
 last_operation_id=sqlc.arg(operation_id),version=version+1,updated_at=clock_timestamp()
WHERE tenant_id=sqlc.arg(tenant_id) AND snat_id=sqlc.arg(snat_id) AND version=sqlc.arg(version) RETURNING *;

-- name: AdvanceSnat :one
UPDATE network_snat_bindings SET state=sqlc.arg(state),reason=sqlc.arg(reason),version=version+1,updated_at=clock_timestamp(),
 applied_enabled=CASE WHEN sqlc.arg(observed)::boolean THEN sqlc.narg(applied_enabled)::boolean ELSE applied_enabled END,
 target_generation=greatest(target_generation,sqlc.arg(target_generation)::bigint),
 observed_at=CASE WHEN sqlc.arg(observed)::boolean THEN sqlc.narg(observed_at)::timestamptz ELSE observed_at END
WHERE tenant_id=sqlc.arg(tenant_id) AND snat_id=sqlc.arg(snat_id) AND version=sqlc.arg(version) RETURNING *;

-- name: AdmitSnatDeletion :one
UPDATE network_snat_bindings SET state='deleting',reason='',applied_enabled=NULL,last_operation_id=sqlc.arg(operation_id),version=version+1,updated_at=clock_timestamp()
WHERE tenant_id=sqlc.arg(tenant_id) AND snat_id=sqlc.arg(snat_id) AND version=sqlc.arg(version) RETURNING *;

-- Global scheduling inventory is worker-only. Admission and completion always
-- recover explicit tenant/parent locks and fence the claimed resource version.
-- name: DueResourceCandidates :many
SELECT r.tenant_id,coalesce(r.vpc_id,r.subnet_id,r.eip_id,r.snat_id,r.lb_id,r.vnic_id)::text AS resource_id,b.resource_kind,
 coalesce(r.vpc_id,s.vpc_id,sn.vpc_id,e.system_owner_vpc,lb.vpc_id,vn.vpc_id,'')::text AS parent_vpc_id,coalesce(r.eip_id,sn.eip_id,'')::text AS parent_eip_id,
 r.next_run_at AS due_at
FROM network_reconciliations r JOIN network_provider_bindings b ON b.tenant_id=r.tenant_id
 AND coalesce(b.vpc_id,b.subnet_id,b.eip_id,b.snat_id,b.lb_id,b.vnic_id)=coalesce(r.vpc_id,r.subnet_id,r.eip_id,r.snat_id,r.lb_id,r.vnic_id)
LEFT JOIN network_subnets s ON s.tenant_id=r.tenant_id AND s.subnet_id=r.subnet_id
LEFT JOIN network_snat_bindings sn ON sn.tenant_id=r.tenant_id AND sn.snat_id=r.snat_id
LEFT JOIN network_eips e ON e.tenant_id=r.tenant_id AND e.eip_id=r.eip_id
LEFT JOIN network_load_balancers lb ON lb.tenant_id=r.tenant_id AND lb.lb_id=r.lb_id
LEFT JOIN network_vnics vn ON vn.tenant_id=r.tenant_id AND vn.vnic_id=r.vnic_id
WHERE NOT r.retired AND r.next_run_at<=clock_timestamp() AND (r.lease_until IS NULL OR r.lease_until<=clock_timestamp())
ORDER BY r.next_run_at,coalesce(r.vpc_id,r.subnet_id,r.eip_id,r.snat_id,r.lb_id,r.vnic_id) LIMIT 32;

-- name: TryLockWorkVPC :one
SELECT * FROM network_vpcs WHERE tenant_id=$1 AND vpc_id=$2 FOR UPDATE SKIP LOCKED;

-- name: TryLockWorkEIP :one
SELECT * FROM network_eips WHERE tenant_id=$1 AND eip_id=$2 FOR UPDATE SKIP LOCKED;

-- name: TryLockWorkSnat :one
SELECT * FROM network_snat_bindings WHERE tenant_id=$1 AND snat_id=$2 FOR UPDATE SKIP LOCKED;

-- name: GetEIPInternal :one
SELECT * FROM network_eips WHERE tenant_id=$1 AND eip_id=$2;

-- name: GetSnatInternal :one
SELECT * FROM network_snat_bindings WHERE tenant_id=$1 AND snat_id=$2;

-- name: CountEIPs :one
SELECT count(*) FROM network_eips e WHERE e.tenant_id=sqlc.arg(tenant_id) AND e.scope='public' AND e.managed_by='tenant'
 AND (sqlc.arg(name_filter)::text='' OR e.name=sqlc.arg(name_filter))
 AND ((sqlc.arg(state_filter)::text='' AND e.state<>'deleted') OR e.state=sqlc.arg(state_filter));
