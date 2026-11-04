-- name: GetBaseConnectivity :one
SELECT * FROM network_vpc_base_connectivity WHERE tenant_id=$1 AND vpc_id=$2;

-- name: GetConnectivityRollout :one
SELECT * FROM network_connectivity_rollout WHERE cluster_id=$1;

-- name: InsertBaseEIP :one
INSERT INTO network_eips(tenant_id,eip_id,cluster_id,namespace,name,description,pool_id,pool_revision,scope,managed_by,system_owner_vpc,state,created_at,updated_at,last_operation_id)
VALUES(sqlc.arg(tenant_id),sqlc.arg(eip_id),sqlc.arg(cluster_id),sqlc.arg(namespace),'VPC base connectivity','',sqlc.arg(pool_id),sqlc.arg(pool_revision),'intranet','system',sqlc.arg(vpc_id),'provisioning',sqlc.arg(created_at),sqlc.arg(created_at),sqlc.arg(operation_id)) RETURNING *;

-- name: InsertBaseSnat :one
INSERT INTO network_snat_bindings(tenant_id,snat_id,cluster_id,namespace,name,description,vpc_id,eip_id,purpose,desired_enabled,state,created_at,updated_at,last_operation_id)
VALUES(sqlc.arg(tenant_id),sqlc.arg(snat_id),sqlc.arg(cluster_id),sqlc.arg(namespace),'VPC base connectivity','',sqlc.arg(vpc_id),sqlc.arg(eip_id),'intranet',true,'provisioning',sqlc.arg(created_at),sqlc.arg(created_at),sqlc.arg(operation_id)) RETURNING *;

-- name: InsertBaseConnectivity :exec
INSERT INTO network_vpc_base_connectivity(tenant_id,vpc_id,cluster_id,namespace,pool_id,pool_revision,eip_id,snat_id,operation_id)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9);

-- name: RequireVPCBase :exec
UPDATE network_vpcs SET base_connectivity_required=true WHERE tenant_id=$1 AND vpc_id=$2;

-- name: SetBaseObservation :exec
UPDATE network_vpc_base_connectivity SET state=sqlc.arg(state),reason=sqlc.arg(reason),
 observed_at=sqlc.narg(observed_at),version=version+1
WHERE tenant_id=sqlc.arg(tenant_id) AND vpc_id=sqlc.arg(vpc_id);

-- name: TerminateBase :exec
UPDATE network_vpc_base_connectivity SET terminating=true,state='deleting',reason='',version=version+1 WHERE tenant_id=$1 AND vpc_id=$2;

-- name: RetireActiveOperation :exec
UPDATE network_operations SET state='failed',reason='CREATE_TERMINATED',completed_at=clock_timestamp(),updated_at=clock_timestamp(),next_attempt_at=NULL
WHERE tenant_id=$1 AND operation_id=$2 AND completed_at IS NULL AND kind NOT LIKE 'delete_%';

-- name: GetBaseForEIP :one
SELECT * FROM network_vpc_base_connectivity WHERE tenant_id=$1 AND eip_id=$2;

-- name: GetBaseForSnat :one
SELECT * FROM network_vpc_base_connectivity WHERE tenant_id=$1 AND snat_id=$2;

-- name: ReleaseSnatClaim :exec
UPDATE network_eip_claims SET released_at=clock_timestamp() WHERE tenant_id=$1 AND snat_id=$2 AND released_at IS NULL;

-- name: ConfirmSnatClaim :exec
UPDATE network_eip_claims SET state='bound' WHERE tenant_id=$1 AND snat_id=$2 AND released_at IS NULL;

-- name: BlockingLBForVPC :one
SELECT count(*)::bigint FROM network_load_balancers WHERE tenant_id=$1 AND vpc_id=$2 AND state<>'deleted';

-- name: BlockingLBForSubnet :one
SELECT count(*)::bigint FROM network_load_balancers l WHERE l.tenant_id=$1 AND l.state<>'deleted'
AND (l.subnet_id=$2 OR EXISTS(SELECT 1 FROM network_lb_subnet_refs r WHERE r.tenant_id=l.tenant_id AND r.lb_id=l.lb_id AND r.subnet_id=$2 AND r.released_at IS NULL));

-- name: NotifyBaseParent :exec
UPDATE network_reconciliations r SET requested_generation=r.requested_generation+1,
 next_run_at=greatest(r.retry_not_before,least(r.next_run_at,clock_timestamp()))
FROM network_vpc_base_connectivity b WHERE NOT r.retired AND b.tenant_id=sqlc.arg(tenant_id) AND r.tenant_id=b.tenant_id AND r.vpc_id=b.vpc_id
 AND (b.eip_id=sqlc.arg(resource_id) OR b.snat_id=sqlc.arg(resource_id));

-- name: NotifyBaseChildren :exec
UPDATE network_reconciliations r SET requested_generation=r.requested_generation+1,
 next_run_at=greatest(r.retry_not_before,least(r.next_run_at,clock_timestamp()))
FROM network_vpc_base_connectivity b WHERE NOT r.retired AND b.tenant_id=sqlc.arg(tenant_id) AND b.vpc_id=sqlc.arg(vpc_id) AND r.tenant_id=b.tenant_id
 AND (r.eip_id=b.eip_id OR r.snat_id=b.snat_id);

-- name: RetireCancelledResource :execrows
UPDATE network_reconciliations SET retired=true,next_run_at='infinity',lease_owner=NULL,lease_until=NULL,
 processed_generation=requested_generation
WHERE tenant_id=sqlc.arg(tenant_id) AND coalesce(eip_id,snat_id)=sqlc.arg(resource_id)
 AND lease_owner=sqlc.arg(owner)::uuid AND lease_epoch=sqlc.arg(epoch) AND lease_until>clock_timestamp();
