-- name: IsPlatformNetwork :one
SELECT EXISTS(SELECT 1 FROM network_platform_vpcs WHERE vpc_id=sqlc.arg(resource_id)::text
 UNION ALL SELECT 1 FROM network_platform_subnets WHERE subnet_id=sqlc.arg(resource_id)::text)::boolean;

-- name: PlatformProductIDCollision :one
SELECT EXISTS(SELECT 1 FROM network_vpcs WHERE vpc_id=sqlc.arg(resource_id)::text
 UNION ALL SELECT 1 FROM network_subnets WHERE subnet_id=sqlc.arg(resource_id)::text)::boolean;

-- name: UpsertPlatformVPC :one
INSERT INTO network_platform_vpcs(vpc_id,cluster_id,cluster_uid,namespace,provider_name,provider_uid,name,description,published,accept_new)
VALUES(sqlc.arg(vpc_id),sqlc.arg(cluster_id),sqlc.arg(cluster_uid),sqlc.arg(namespace),sqlc.arg(provider_name),sqlc.arg(provider_uid),sqlc.arg(name),sqlc.arg(description),sqlc.arg(published),sqlc.arg(accept_new))
ON CONFLICT(vpc_id) DO UPDATE SET name=excluded.name,description=excluded.description,published=excluded.published,accept_new=excluded.accept_new,
 version=network_platform_vpcs.version+1,updated_at=clock_timestamp()
WHERE network_platform_vpcs.cluster_id=excluded.cluster_id AND network_platform_vpcs.cluster_uid=excluded.cluster_uid
 AND network_platform_vpcs.namespace=excluded.namespace AND network_platform_vpcs.provider_name=excluded.provider_name AND network_platform_vpcs.provider_uid=excluded.provider_uid
RETURNING *;

-- name: UpsertPlatformSubnet :one
INSERT INTO network_platform_subnets(subnet_id,vpc_id,cluster_id,namespace,provider_name,provider_uid,binding_id,name,description,published,accept_new)
VALUES(sqlc.arg(subnet_id),sqlc.arg(vpc_id),sqlc.arg(cluster_id),sqlc.arg(namespace),sqlc.arg(provider_name),sqlc.arg(provider_uid),sqlc.arg(binding_id),sqlc.arg(name),sqlc.arg(description),sqlc.arg(published),sqlc.arg(accept_new))
ON CONFLICT(subnet_id) DO UPDATE SET name=excluded.name,description=excluded.description,published=excluded.published,accept_new=excluded.accept_new,
 version=network_platform_subnets.version+1,updated_at=clock_timestamp()
WHERE network_platform_subnets.vpc_id=excluded.vpc_id AND network_platform_subnets.cluster_id=excluded.cluster_id
 AND network_platform_subnets.namespace=excluded.namespace AND network_platform_subnets.provider_name=excluded.provider_name AND network_platform_subnets.provider_uid=excluded.provider_uid
RETURNING *;

-- name: CloseRemovedPlatformVPCs :exec
UPDATE network_platform_vpcs SET published=false,accept_new=false,version=version+1,updated_at=clock_timestamp()
WHERE cluster_id=sqlc.arg(cluster_id) AND vpc_id<>ALL(sqlc.arg(keep_ids)::text[]) AND (published OR accept_new);

-- name: CloseRemovedPlatformSubnets :exec
UPDATE network_platform_subnets SET published=false,accept_new=false,version=version+1,updated_at=clock_timestamp()
WHERE cluster_id=sqlc.arg(cluster_id) AND subnet_id<>ALL(sqlc.arg(keep_ids)::text[]) AND (published OR accept_new);

-- name: EnsurePlatformNetworkObservation :exec
INSERT INTO network_platform_network_observations(resource_id,vpc_id,subnet_id)
VALUES(sqlc.arg(resource_id),sqlc.narg(vpc_id),sqlc.narg(subnet_id)) ON CONFLICT(resource_id) DO UPDATE
SET requested_generation=network_platform_network_observations.requested_generation+1,
 next_run_at=greatest(network_platform_network_observations.retry_not_before,least(network_platform_network_observations.next_run_at,clock_timestamp()));

-- name: GetPlatformVPC :one
SELECT * FROM network_platform_vpcs WHERE vpc_id=sqlc.arg(vpc_id);
-- name: GetPlatformSubnet :one
SELECT * FROM network_platform_subnets WHERE subnet_id=sqlc.arg(subnet_id);
-- name: GetPlatformNetworkObservation :one
SELECT * FROM network_platform_network_observations WHERE resource_id=sqlc.arg(resource_id);
-- name: PlatformVPCVisibleSubnetCount :one
SELECT count(*)::bigint FROM network_platform_subnets s JOIN network_platform_vpcs v ON v.vpc_id=s.vpc_id WHERE s.vpc_id=sqlc.arg(vpc_id) AND v.published AND s.published;
-- name: PlatformVPCCanUse :one
SELECT EXISTS(SELECT 1 FROM network_platform_vpcs v JOIN network_platform_network_observations vo ON vo.vpc_id=v.vpc_id
 WHERE v.vpc_id=sqlc.arg(vpc_id) AND v.published AND v.accept_new AND vo.observation_error<>'PROVIDER_OWNERSHIP_CONFLICT'
 AND EXISTS(SELECT 1 FROM network_platform_subnets s JOIN network_platform_network_observations so ON so.subnet_id=s.subnet_id
 WHERE s.vpc_id=v.vpc_id AND s.published AND s.accept_new AND so.observation_error<>'PROVIDER_OWNERSHIP_CONFLICT'))::boolean;

-- One statement is the admission linearization point, even when the caller
-- holds only an owner-domain lock or a READ COMMITTED VNIC transaction.
-- name: PlatformSubnetAdmission :one
SELECT (v.published AND v.accept_new AND s.published AND s.accept_new
 AND vo.observation_error<>'PROVIDER_OWNERSHIP_CONFLICT' AND so.observation_error<>'PROVIDER_OWNERSHIP_CONFLICT')::boolean AS can_use,
 (vo.observation_error='PROVIDER_OWNERSHIP_CONFLICT' OR so.observation_error='PROVIDER_OWNERSHIP_CONFLICT')::boolean AS ownership_conflict
FROM network_platform_subnets s JOIN network_platform_vpcs v ON v.vpc_id=s.vpc_id
JOIN network_platform_network_observations vo ON vo.vpc_id=v.vpc_id JOIN network_platform_network_observations so ON so.subnet_id=s.subnet_id
WHERE s.subnet_id=sqlc.arg(subnet_id);

-- name: LockDuePlatformNetwork :one
SELECT o.* FROM network_platform_network_observations o
LEFT JOIN network_platform_vpcs v ON v.vpc_id=o.vpc_id
LEFT JOIN network_platform_subnets s ON s.subnet_id=o.subnet_id
WHERE coalesce(v.cluster_id,s.cluster_id)=sqlc.arg(cluster_id)::text AND o.next_run_at<=clock_timestamp()
 AND (o.lease_until IS NULL OR o.lease_until<=clock_timestamp())
ORDER BY o.next_run_at,o.resource_id LIMIT 1 FOR UPDATE OF o SKIP LOCKED;

-- name: AcquirePlatformNetworkLease :one
UPDATE network_platform_network_observations SET lease_owner=sqlc.arg(owner)::uuid,
 lease_until=clock_timestamp()+sqlc.arg(lease_micros)::bigint*interval '1 microsecond',lease_epoch=lease_epoch+1
WHERE resource_id=sqlc.arg(resource_id) RETURNING *;

-- name: LockPlatformVPC :one
SELECT * FROM network_platform_vpcs WHERE vpc_id=sqlc.arg(vpc_id) FOR UPDATE;
-- name: LockPlatformSubnet :one
SELECT * FROM network_platform_subnets WHERE subnet_id=sqlc.arg(subnet_id) FOR UPDATE;
-- name: CheckPlatformNetworkLease :one
SELECT * FROM network_platform_network_observations WHERE resource_id=sqlc.arg(resource_id)
 AND lease_owner=sqlc.arg(owner)::uuid AND lease_epoch=sqlc.arg(lease_epoch) AND lease_until>clock_timestamp() FOR UPDATE;

-- name: AdvancePlatformVPC :execrows
UPDATE network_platform_vpcs SET state=sqlc.arg(state),reason=sqlc.arg(reason),cidr=sqlc.arg(cidr),
 observed_at=clock_timestamp(),updated_at=clock_timestamp(),version=version+1
WHERE vpc_id=sqlc.arg(vpc_id) AND version=sqlc.arg(version);
-- name: AdvancePlatformSubnet :execrows
UPDATE network_platform_subnets SET state=sqlc.arg(state),reason=sqlc.arg(reason),cidr=sqlc.arg(cidr),gateway=sqlc.arg(gateway),
 exclude_ips=sqlc.arg(exclude_ips)::text[],dns_servers=sqlc.arg(dns_servers)::text[],enable_dhcp=sqlc.arg(enable_dhcp),
 available_ip_range=sqlc.narg(available_ip_range),used_ip_range=sqlc.narg(used_ip_range),observed_at=clock_timestamp(),updated_at=clock_timestamp(),version=version+1
WHERE subnet_id=sqlc.arg(subnet_id) AND version=sqlc.arg(version);

-- name: FinishPlatformNetworkObservation :execrows
UPDATE network_platform_network_observations SET processed_generation=greatest(processed_generation,sqlc.arg(claimed_generation)::bigint),
 observation_error=CASE WHEN sqlc.arg(observed)::boolean THEN ''
 WHEN observation_error='PROVIDER_OWNERSHIP_CONFLICT' THEN observation_error ELSE sqlc.arg(observation_error)::text END,
 retry_not_before=CASE WHEN sqlc.arg(backoff)::boolean THEN clock_timestamp()+sqlc.arg(delay_micros)::bigint*interval '1 microsecond' ELSE '1970-01-01 UTC'::timestamptz END,
 retry_attempt=CASE WHEN sqlc.arg(backoff)::boolean THEN retry_attempt+1 ELSE 0 END,
 next_run_at=CASE WHEN sqlc.arg(backoff)::boolean THEN clock_timestamp()+sqlc.arg(delay_micros)::bigint*interval '1 microsecond'
 WHEN requested_generation>sqlc.arg(claimed_generation)::bigint THEN clock_timestamp() ELSE 'infinity'::timestamptz END,
 lease_owner=NULL,lease_until=NULL
WHERE resource_id=sqlc.arg(resource_id) AND lease_owner=sqlc.arg(owner)::uuid AND lease_epoch=sqlc.arg(lease_epoch) AND lease_until>clock_timestamp();

-- name: NotifyPlatformNetwork :execrows
UPDATE network_platform_network_observations SET requested_generation=requested_generation+1,
 next_run_at=greatest(retry_not_before,least(next_run_at,clock_timestamp())) WHERE resource_id=sqlc.arg(resource_id);

-- name: GetTenantBasicIsolation :one
SELECT basic_isolation_name,basic_isolation_uid FROM network_tenant_namespaces WHERE tenant_id=sqlc.arg(tenant_id) AND cluster_id=sqlc.arg(cluster_id);
-- name: ReserveTenantBasicIsolation :execrows
UPDATE network_tenant_namespaces SET basic_isolation_name=sqlc.arg(name)
WHERE tenant_id=sqlc.arg(tenant_id) AND cluster_id=sqlc.arg(cluster_id) AND namespace_uid=sqlc.arg(namespace_uid)::text
 AND (basic_isolation_name='' OR basic_isolation_name=sqlc.arg(name));
-- name: SaveTenantBasicIsolationUID :execrows
UPDATE network_tenant_namespaces SET basic_isolation_uid=sqlc.arg(uid)
WHERE tenant_id=sqlc.arg(tenant_id) AND cluster_id=sqlc.arg(cluster_id) AND basic_isolation_name=sqlc.arg(name)
 AND (basic_isolation_uid='' OR basic_isolation_uid=sqlc.arg(uid));

-- name: CountVisibleVPCs :one
WITH visible AS (
 SELECT v.vpc_id AS product_id,v.name,v.state,v.created_at,'tenant'::text AS owner_scope,
  (v.state NOT IN ('deleting','deleted'))::boolean AS can_manage,
  (v.state NOT IN ('deleting','deleted'))::boolean AS can_use
 FROM network_vpcs v WHERE v.tenant_id=sqlc.arg(tenant_id)::uuid
 UNION ALL
 SELECT pv.vpc_id,pv.name,pv.state,pv.created_at,'platform'::text,false,
  (pv.accept_new AND o.observation_error<>'PROVIDER_OWNERSHIP_CONFLICT' AND EXISTS(
    SELECT 1 FROM network_platform_subnets s JOIN network_platform_network_observations so ON so.subnet_id=s.subnet_id
    WHERE s.vpc_id=pv.vpc_id AND s.published AND s.accept_new AND so.observation_error<>'PROVIDER_OWNERSHIP_CONFLICT'))::boolean
 FROM network_platform_vpcs pv JOIN network_platform_network_observations o ON o.vpc_id=pv.vpc_id
 WHERE pv.cluster_id=sqlc.arg(cluster_id)::text AND pv.published
)
SELECT count(*)::bigint FROM visible
WHERE (sqlc.arg(name_filter)::text='' OR name=sqlc.arg(name_filter)::text)
 AND ((sqlc.arg(state_filter)::text='' AND state<>'deleted') OR state=sqlc.arg(state_filter)::text)
 AND (sqlc.narg(can_manage)::boolean IS NULL OR can_manage=sqlc.narg(can_manage)::boolean)
 AND (sqlc.narg(can_use)::boolean IS NULL OR can_use=sqlc.narg(can_use)::boolean)
;

-- name: ListVisibleVPCs :many
WITH visible AS (
 SELECT v.vpc_id AS product_id,v.name,v.state,v.created_at,'tenant'::text AS owner_scope,
  (v.state NOT IN ('deleting','deleted'))::boolean AS can_manage,
  (v.state NOT IN ('deleting','deleted'))::boolean AS can_use
 FROM network_vpcs v WHERE v.tenant_id=sqlc.arg(tenant_id)::uuid
 UNION ALL
 SELECT pv.vpc_id,pv.name,pv.state,pv.created_at,'platform'::text,false,
  (pv.accept_new AND o.observation_error<>'PROVIDER_OWNERSHIP_CONFLICT' AND EXISTS(
    SELECT 1 FROM network_platform_subnets s JOIN network_platform_network_observations so ON so.subnet_id=s.subnet_id
    WHERE s.vpc_id=pv.vpc_id AND s.published AND s.accept_new AND so.observation_error<>'PROVIDER_OWNERSHIP_CONFLICT'))::boolean
 FROM network_platform_vpcs pv JOIN network_platform_network_observations o ON o.vpc_id=pv.vpc_id
 WHERE pv.cluster_id=sqlc.arg(cluster_id)::text AND pv.published
)
SELECT product_id,owner_scope,can_manage,can_use FROM visible
WHERE (sqlc.arg(name_filter)::text='' OR name=sqlc.arg(name_filter)::text)
 AND ((sqlc.arg(state_filter)::text='' AND state<>'deleted') OR state=sqlc.arg(state_filter)::text)
 AND (sqlc.narg(can_manage)::boolean IS NULL OR can_manage=sqlc.narg(can_manage)::boolean)
 AND (sqlc.narg(can_use)::boolean IS NULL OR can_use=sqlc.narg(can_use)::boolean)
 AND (sqlc.arg(after_id)::text='' OR (created_at,product_id)<(sqlc.arg(after_created_at)::timestamptz,sqlc.arg(after_id)::text))
ORDER BY created_at DESC,product_id DESC LIMIT sqlc.arg(max_results)::integer OFFSET sqlc.arg(page_offset)::bigint;

-- name: CountVisibleSubnets :one
WITH visible AS (
 SELECT s.subnet_id AS product_id,s.vpc_id,s.name,s.state,s.created_at,'tenant'::text AS owner_scope,
  (s.state NOT IN ('deleting','deleted'))::boolean AS can_manage,
  (s.state NOT IN ('deleting','deleted') AND v.state NOT IN ('deleting','deleted'))::boolean AS can_use
 FROM network_subnets s JOIN network_vpcs v ON v.tenant_id=s.tenant_id AND v.vpc_id=s.vpc_id WHERE s.tenant_id=sqlc.arg(tenant_id)::uuid
 UNION ALL
 SELECT ps.subnet_id,ps.vpc_id,ps.name,ps.state,ps.created_at,'platform'::text,false,
  (pv.accept_new AND ps.accept_new AND o.observation_error<>'PROVIDER_OWNERSHIP_CONFLICT' AND so.observation_error<>'PROVIDER_OWNERSHIP_CONFLICT')::boolean
 FROM network_platform_subnets ps JOIN network_platform_vpcs pv ON pv.vpc_id=ps.vpc_id
 JOIN network_platform_network_observations o ON o.vpc_id=pv.vpc_id JOIN network_platform_network_observations so ON so.subnet_id=ps.subnet_id
 WHERE pv.cluster_id=sqlc.arg(cluster_id)::text AND pv.published AND ps.published
)
SELECT count(*)::bigint FROM visible
WHERE (sqlc.arg(name_filter)::text='' OR name=sqlc.arg(name_filter)::text)
 AND ((sqlc.arg(state_filter)::text='' AND state<>'deleted') OR state=sqlc.arg(state_filter)::text)
 AND (sqlc.narg(can_manage)::boolean IS NULL OR can_manage=sqlc.narg(can_manage)::boolean)
 AND (sqlc.narg(can_use)::boolean IS NULL OR can_use=sqlc.narg(can_use)::boolean)
 AND (sqlc.arg(vpc_filter)::text='' OR vpc_id=sqlc.arg(vpc_filter)::text)
;

-- name: ListVisibleSubnets :many
WITH visible AS (
 SELECT s.subnet_id AS product_id,s.vpc_id,s.name,s.state,s.created_at,'tenant'::text AS owner_scope,
  (s.state NOT IN ('deleting','deleted'))::boolean AS can_manage,
  (s.state NOT IN ('deleting','deleted') AND v.state NOT IN ('deleting','deleted'))::boolean AS can_use
 FROM network_subnets s JOIN network_vpcs v ON v.tenant_id=s.tenant_id AND v.vpc_id=s.vpc_id WHERE s.tenant_id=sqlc.arg(tenant_id)::uuid
 UNION ALL
 SELECT ps.subnet_id,ps.vpc_id,ps.name,ps.state,ps.created_at,'platform'::text,false,
  (pv.accept_new AND ps.accept_new AND o.observation_error<>'PROVIDER_OWNERSHIP_CONFLICT' AND so.observation_error<>'PROVIDER_OWNERSHIP_CONFLICT')::boolean
 FROM network_platform_subnets ps JOIN network_platform_vpcs pv ON pv.vpc_id=ps.vpc_id
 JOIN network_platform_network_observations o ON o.vpc_id=pv.vpc_id JOIN network_platform_network_observations so ON so.subnet_id=ps.subnet_id
 WHERE pv.cluster_id=sqlc.arg(cluster_id)::text AND pv.published AND ps.published
)
SELECT product_id,owner_scope,can_manage,can_use FROM visible
WHERE (sqlc.arg(name_filter)::text='' OR name=sqlc.arg(name_filter)::text)
 AND ((sqlc.arg(state_filter)::text='' AND state<>'deleted') OR state=sqlc.arg(state_filter)::text)
 AND (sqlc.narg(can_manage)::boolean IS NULL OR can_manage=sqlc.narg(can_manage)::boolean)
 AND (sqlc.narg(can_use)::boolean IS NULL OR can_use=sqlc.narg(can_use)::boolean)
 AND (sqlc.arg(vpc_filter)::text='' OR vpc_id=sqlc.arg(vpc_filter)::text)
 AND (sqlc.arg(after_id)::text='' OR (created_at,product_id)<(sqlc.arg(after_created_at)::timestamptz,sqlc.arg(after_id)::text))
ORDER BY created_at DESC,product_id DESC LIMIT sqlc.arg(max_results)::integer OFFSET sqlc.arg(page_offset)::bigint;
