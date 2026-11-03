-- name: GetSubnetIdempotency :one
SELECT * FROM network_idempotency WHERE tenant_id=$1 AND operation_kind='create_subnet' AND idempotency_key=$2;

-- name: LockSubnetCreationKey :exec
SELECT pg_advisory_xact_lock(hashtextextended(sqlc.arg(tenant_id)::text || ':create_subnet:' || sqlc.arg(idempotency_key)::text,0));

-- name: InsertSubnet :one
INSERT INTO network_subnets (tenant_id,subnet_id,vpc_id,name,description,cidr,gateway,state,created_at,updated_at,last_operation_id,exclude_ips,enable_dhcp,dns_servers)
VALUES ($1,$2,$3,$4,$5,$6,$7,'provisioning',$8,$8,$9,$10,$11,$12) RETURNING *;

-- name: InsertSubnetIdempotency :exec
INSERT INTO network_idempotency (tenant_id,operation_kind,idempotency_key,fingerprint,fingerprint_version,subnet_id,operation_id,response,created_at)
VALUES (sqlc.arg(tenant_id),'create_subnet',sqlc.arg(idempotency_key),sqlc.arg(fingerprint),1,sqlc.arg(subnet_id)::text,sqlc.arg(operation_id),sqlc.arg(response),sqlc.arg(created_at));

-- name: GetSubnet :one
SELECT * FROM network_subnets WHERE tenant_id=$1 AND subnet_id=$2;

-- name: LockSubnet :one
SELECT * FROM network_subnets WHERE tenant_id=$1 AND subnet_id=$2 FOR UPDATE;

-- name: CountSubnets :one
SELECT count(*)::bigint FROM network_subnets WHERE tenant_id=$1 AND vpc_id=$2 AND state<>'deleted';

-- name: CountBlockingSubnets :one
SELECT count(*)::bigint FROM network_subnets s WHERE s.tenant_id=$1 AND s.vpc_id=$2
 AND (s.state<>'deleted' OR EXISTS(SELECT 1 FROM network_attachments a WHERE a.tenant_id=s.tenant_id AND a.subnet_id=s.subnet_id AND (a.state<>'released' OR a.protocol_blocked)));

-- name: SubnetOverlaps :one
SELECT EXISTS(SELECT 1 FROM network_subnets s WHERE s.tenant_id=sqlc.arg(tenant_id) AND s.vpc_id=sqlc.arg(vpc_id)
 AND (s.state<>'deleted' OR EXISTS(SELECT 1 FROM network_attachments a WHERE a.tenant_id=s.tenant_id AND a.subnet_id=s.subnet_id AND (a.state<>'released' OR a.protocol_blocked)))
 AND s.cidr::cidr && sqlc.arg(cidr)::cidr)::boolean;

-- name: ListSubnets :many
SELECT * FROM network_subnets WHERE tenant_id=sqlc.arg(tenant_id)
 AND (sqlc.arg(vpc_filter)::text='' OR vpc_id=sqlc.arg(vpc_filter))
 AND (sqlc.arg(name_filter)::text='' OR name=sqlc.arg(name_filter))
 AND ((sqlc.arg(state_filter)::text='' AND state<>'deleted') OR state=sqlc.arg(state_filter))
 AND (sqlc.arg(after_id)::text='' OR (created_at,subnet_id)<(sqlc.arg(after_created_at)::timestamptz,sqlc.arg(after_id)::text))
ORDER BY created_at DESC,subnet_id DESC LIMIT sqlc.arg(max_results)::integer;

-- name: LockDueSubnet :one
SELECT s.* FROM network_subnets s JOIN network_reconciliations r ON r.tenant_id=s.tenant_id AND r.subnet_id=s.subnet_id
WHERE s.tenant_id=$1 AND s.vpc_id=$2 AND r.next_run_at<=clock_timestamp()
 AND (r.lease_until IS NULL OR r.lease_until<=clock_timestamp())
ORDER BY r.next_run_at,s.subnet_id LIMIT 1 FOR UPDATE OF s SKIP LOCKED;

-- name: AdvanceSubnet :one
UPDATE network_subnets SET state=sqlc.arg(state),reason=sqlc.arg(reason),version=version+1,updated_at=clock_timestamp(),
 observed_at=CASE WHEN sqlc.arg(observed)::boolean THEN sqlc.narg(observed_at)::timestamptz ELSE observed_at END,
 available_ip_range=CASE WHEN sqlc.arg(observed)::boolean THEN sqlc.narg(available_ip_range)::text ELSE available_ip_range END,
 used_ip_range=CASE WHEN sqlc.arg(observed)::boolean THEN sqlc.narg(used_ip_range)::text ELSE used_ip_range END
WHERE tenant_id=sqlc.arg(tenant_id) AND subnet_id=sqlc.arg(subnet_id) AND version=sqlc.arg(version) RETURNING *;

-- name: AdmitSubnetDeletion :one
UPDATE network_subnets SET state='deleting',reason='',last_operation_id=sqlc.arg(operation_id),version=version+1,updated_at=clock_timestamp()
WHERE tenant_id=sqlc.arg(tenant_id) AND subnet_id=sqlc.arg(subnet_id) AND version=sqlc.arg(version) RETURNING *;

-- name: CountListedSubnets :one
SELECT count(*) FROM network_subnets WHERE tenant_id=sqlc.arg(tenant_id)
 AND (sqlc.arg(vpc_filter)::text='' OR vpc_id=sqlc.arg(vpc_filter))
 AND (sqlc.arg(name_filter)::text='' OR name=sqlc.arg(name_filter))
 AND ((sqlc.arg(state_filter)::text='' AND state<>'deleted') OR state=sqlc.arg(state_filter));
