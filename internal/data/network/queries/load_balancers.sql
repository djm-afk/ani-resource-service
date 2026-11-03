-- name: GetLBInternal :one
SELECT * FROM network_load_balancers WHERE tenant_id=$1 AND lb_id=$2;
-- name: LockLB :one
SELECT * FROM network_load_balancers WHERE tenant_id=$1 AND lb_id=$2 FOR UPDATE;
-- name: GetLBConfiguration :one
SELECT * FROM network_lb_configurations WHERE tenant_id=$1 AND lb_id=$2 AND config_version=$3;
-- name: GetLBListener :one
SELECT * FROM network_lb_listeners WHERE tenant_id=$1 AND lb_id=$2 ORDER BY name LIMIT 1;
-- name: GetLBMember :one
SELECT * FROM network_lb_members WHERE tenant_id=$1 AND lb_id=$2 AND member_id=$3;
-- name: ListLBMembers :many
SELECT sqlc.embed(m),cm.weight FROM network_lb_members m JOIN network_lb_configuration_members cm
ON (m.tenant_id,m.lb_id,m.member_id)=(cm.tenant_id,cm.lb_id,cm.member_id)
WHERE cm.tenant_id=$1 AND cm.lb_id=$2 AND cm.config_version=$3 ORDER BY m.member_id;
-- name: ListLBs :many
SELECT * FROM network_load_balancers l WHERE tenant_id=sqlc.arg(tenant_id) AND last_operation_id IS NOT NULL
AND (sqlc.arg(name_filter)::text='' OR name=sqlc.arg(name_filter))
AND (sqlc.arg(vpc_filter)::text='' OR vpc_id=sqlc.arg(vpc_filter))
AND (sqlc.arg(subnet_filter)::text='' OR subnet_id=sqlc.arg(subnet_filter))
AND (sqlc.arg(exposure_filter)::text='' OR exposure=sqlc.arg(exposure_filter))
AND ((sqlc.arg(state_filter)::text='' AND state<>'deleted') OR state=sqlc.arg(state_filter))
AND (sqlc.arg(after_id)::text='' OR (created_at,lb_id)<(sqlc.arg(after_created_at)::timestamptz,sqlc.arg(after_id)::text))
ORDER BY created_at DESC,lb_id DESC LIMIT sqlc.arg(max_results)::integer;
-- name: InsertLB :one
INSERT INTO network_load_balancers(tenant_id,lb_id,cluster_id,namespace,vpc_id,subnet_id,exposure,flavor,public_eip_id,private_ip,state,name,description,created_at,updated_at,last_operation_id)
VALUES(sqlc.arg(tenant_id),sqlc.arg(lb_id),sqlc.arg(cluster_id),sqlc.arg(namespace),sqlc.arg(vpc_id),sqlc.arg(subnet_id),sqlc.arg(exposure),sqlc.arg(flavor),NULLIF(sqlc.arg(public_eip_id)::text,''),NULLIF(sqlc.arg(private_ip)::text,''),'provisioning',sqlc.arg(name),sqlc.arg(description),sqlc.arg(created_at),sqlc.arg(created_at),sqlc.arg(operation_id)) RETURNING *;
-- name: InsertLBListener :exec
INSERT INTO network_lb_listeners(tenant_id,cluster_id,namespace,lb_id,listener_id,port,name) VALUES($1,$2,$3,$4,$5,$6,$7);
-- name: InsertLBConfiguration :exec
INSERT INTO network_lb_configurations(tenant_id,cluster_id,namespace,lb_id,config_version,name,description,interval_seconds,timeout_seconds,unhealthy_threshold,healthy_threshold,health_check_port)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12);
-- name: InsertLBMember :exec
INSERT INTO network_lb_members(tenant_id,cluster_id,namespace,lb_id,member_id,vpc_id,subnet_id,attachment_id,address,port,pod_uid,vnic_name,vnic_uid,vnicip_name,vnicip_uid,state,observed_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,'available',$16);
-- name: InsertLBConfigurationMember :exec
INSERT INTO network_lb_configuration_members(tenant_id,cluster_id,namespace,lb_id,config_version,member_id,weight) VALUES($1,$2,$3,$4,$5,$6,$7);
-- name: ListLBBackendAttachments :many
SELECT * FROM network_attachments WHERE tenant_id=$1 AND subnet_id=$2 AND state='attached' AND NOT protocol_blocked AND reason IN ('','PROVIDER_UNAVAILABLE') ORDER BY attachment_id;
-- name: ReserveLBSubnet :exec
INSERT INTO network_lb_subnet_refs(tenant_id,cluster_id,namespace,lb_id,vpc_id,subnet_id) VALUES($1,$2,$3,$4,$5,$6)
ON CONFLICT (tenant_id,lb_id,subnet_id) DO UPDATE SET released_at=NULL;
-- name: ListLBSubnetRefs :many
SELECT * FROM network_lb_subnet_refs WHERE tenant_id=$1 AND lb_id=$2 AND released_at IS NULL ORDER BY subnet_id;
-- name: ClaimEIPForLB :execrows
INSERT INTO network_eip_claims(tenant_id,cluster_id,namespace,eip_id,target_kind,lb_id,created_at)
VALUES($1,$2,$3,$4,'load_balancer',$5,$6) ON CONFLICT DO NOTHING;
-- name: ReserveLBVIP :execrows
INSERT INTO network_lb_vip_intents(tenant_id,cluster_id,namespace,vpc_id,subnet_id,lb_id,address)
VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT DO NOTHING;
-- name: InsertLBComponent :exec
INSERT INTO network_lb_components(tenant_id,cluster_id,namespace,lb_id,component_id,kind,member_id,listener_id,provider_name)
VALUES($1,$2,$3,$4,$5,$6,sqlc.narg(member_id),sqlc.narg(listener_id),$7);
-- name: InsertLBIdempotency :exec
INSERT INTO network_idempotency(tenant_id,operation_kind,idempotency_key,fingerprint,fingerprint_version,lb_id,operation_id,response,created_at)
VALUES($1,$2,$3,$4,1,$5,$6,$7,$8);
-- name: UpdateLBIntent :one
UPDATE network_load_balancers SET name=sqlc.arg(name),description=sqlc.arg(description),desired_version=desired_version+1,
configuration_state=CASE WHEN sqlc.arg(projection_changed)::boolean THEN 'pending' ELSE configuration_state END,
data_plane_state=CASE WHEN sqlc.arg(projection_changed)::boolean THEN 'unknown' ELSE data_plane_state END,
data_plane_observed_at=CASE WHEN sqlc.arg(projection_changed)::boolean THEN NULL ELSE data_plane_observed_at END,
version=version+1,updated_at=clock_timestamp(),last_operation_id=sqlc.arg(operation_id)
WHERE tenant_id=sqlc.arg(tenant_id) AND lb_id=sqlc.arg(lb_id) AND version=sqlc.arg(version) AND state NOT IN ('deleting','deleted') RETURNING *;
-- name: AdmitLBDeletion :one
UPDATE network_load_balancers SET state='deleting',reason='',version=version+1,updated_at=clock_timestamp(),last_operation_id=sqlc.arg(operation_id),data_plane_state='unknown',data_plane_observed_at=NULL
WHERE tenant_id=sqlc.arg(tenant_id) AND lb_id=sqlc.arg(lb_id) AND version=sqlc.arg(version) RETURNING *;
-- name: GetLBCapability :one
SELECT * FROM network_lb_capabilities WHERE cluster_id=$1;
-- name: SaveLBCapability :exec
INSERT INTO network_lb_capabilities(cluster_id,ready,reason,observed_at,fingerprint,provider_images) VALUES($1,$2,$3,$4,$5,$6)
ON CONFLICT(cluster_id) DO UPDATE SET ready=EXCLUDED.ready,reason=EXCLUDED.reason,observed_at=EXCLUDED.observed_at,fingerprint=EXCLUDED.fingerprint,provider_images=EXCLUDED.provider_images
WHERE network_lb_capabilities.observed_at<=EXCLUDED.observed_at;
-- name: RetireLBMutation :exec
UPDATE network_operations SET state='failed',reason='LOAD_BALANCER_MUTATION_TERMINATED',completed_at=clock_timestamp(),updated_at=clock_timestamp(),next_attempt_at=NULL
WHERE tenant_id=sqlc.arg(tenant_id) AND lb_id=sqlc.arg(lb_id)::text AND operation_id=sqlc.arg(operation_id) AND completed_at IS NULL AND kind IN ('create_load_balancer','update_load_balancer');

-- name: ListLBComponents :many
SELECT * FROM network_lb_components WHERE tenant_id=$1 AND lb_id=$2 ORDER BY kind,component_id;
-- name: ListLBMemberIdentities :many
SELECT sqlc.embed(m),a.pod_name FROM network_lb_members m JOIN network_attachments a
ON a.tenant_id=m.tenant_id AND a.attachment_id=m.attachment_id
WHERE m.tenant_id=$1 AND m.lb_id=$2 ORDER BY m.member_id;
-- name: BeginLBComponentMutation :execrows
UPDATE network_lb_components SET pending_action=sqlc.arg(action),pending_since=clock_timestamp(),
create_dispatched=create_dispatched OR sqlc.arg(action)::text='create',target_version=sqlc.arg(target_version),
provider_uid=CASE WHEN provider_uid='' THEN sqlc.arg(identity)::text ELSE provider_uid END
WHERE tenant_id=sqlc.arg(tenant_id) AND lb_id=sqlc.arg(lb_id) AND component_id=sqlc.arg(component_id) AND deleted_at IS NULL
AND (provider_uid='' OR provider_uid=sqlc.arg(identity))
AND ((sqlc.arg(action)::text='create' AND pending_action='' AND provider_uid='')
 OR (sqlc.arg(action)::text='update' AND provider_uid<>'' AND pending_action IN ('','update'))
 OR (sqlc.arg(action)::text='delete' AND sqlc.arg(identity)::text<>''));
-- name: SaveLBComponentObservation :execrows
UPDATE network_lb_components SET provider_uid=CASE WHEN provider_uid='' THEN sqlc.arg(identity)::text ELSE provider_uid END,
pending_action=CASE WHEN sqlc.arg(clear_pending)::boolean THEN '' ELSE pending_action END,
pending_since=CASE WHEN sqlc.arg(clear_pending)::boolean THEN NULL ELSE pending_since END,
applied_version=greatest(applied_version,sqlc.arg(applied_version)::bigint),
target_version=greatest(target_version,sqlc.arg(applied_version)::bigint),
deleted_at=CASE WHEN sqlc.arg(deleted)::boolean THEN coalesce(deleted_at,clock_timestamp()) ELSE deleted_at END
WHERE tenant_id=sqlc.arg(tenant_id) AND lb_id=sqlc.arg(lb_id) AND component_id=sqlc.arg(component_id)
AND (provider_uid='' OR provider_uid=sqlc.arg(identity));
-- name: AdvanceLB :one
UPDATE network_load_balancers SET state=sqlc.arg(state),reason=sqlc.arg(reason),version=version+1,updated_at=clock_timestamp(),
vip_occupied_revision=CASE WHEN sqlc.arg(vip_occupied_revision)::text<>'' THEN sqlc.arg(vip_occupied_revision) ELSE vip_occupied_revision END,
vip_absence_revision=CASE WHEN vip_absence_revision='' THEN sqlc.arg(vip_absence_revision)::text ELSE vip_absence_revision END,
observed_at=CASE WHEN sqlc.arg(observed)::boolean THEN sqlc.narg(observed_at)::timestamptz ELSE observed_at END,
configuration_state=CASE WHEN sqlc.arg(observed)::boolean THEN sqlc.arg(configuration_state) ELSE configuration_state END,applied_version=greatest(applied_version,sqlc.arg(applied_version)::bigint),
data_plane_state='unknown',data_plane_observed_at=NULL
WHERE tenant_id=sqlc.arg(tenant_id) AND lb_id=sqlc.arg(lb_id) AND version=sqlc.arg(version) RETURNING *;
-- name: SaveLBMemberObservation :execrows
UPDATE network_lb_members SET state=CASE WHEN sqlc.arg(eligible)::boolean THEN 'available' ELSE 'unavailable' END,
reason=sqlc.arg(reason),observed_at=sqlc.arg(observed_at)
WHERE tenant_id=sqlc.arg(tenant_id) AND lb_id=sqlc.arg(lb_id) AND member_id=sqlc.arg(member_id)
AND (observed_at IS NULL OR observed_at<=sqlc.arg(observed_at));
-- name: SaveLBGenerated :execrows
INSERT INTO network_lb_generated_resources(tenant_id,cluster_id,namespace,lb_id,kind,provider_name,provider_uid,gateway_uid,observed_at,released_at)
VALUES(sqlc.arg(tenant_id),sqlc.arg(cluster_id),sqlc.arg(namespace),sqlc.arg(lb_id),sqlc.arg(kind),sqlc.arg(provider_name),sqlc.arg(provider_uid),sqlc.arg(gateway_uid),sqlc.arg(observed_at),CASE WHEN sqlc.arg(released)::boolean THEN sqlc.arg(observed_at)::timestamptz ELSE NULL END)
ON CONFLICT(tenant_id,lb_id,kind,provider_uid) DO UPDATE SET observed_at=EXCLUDED.observed_at,released_at=EXCLUDED.released_at
WHERE network_lb_generated_resources.gateway_uid=EXCLUDED.gateway_uid AND network_lb_generated_resources.namespace=EXCLUDED.namespace
AND network_lb_generated_resources.provider_name=EXCLUDED.provider_name AND network_lb_generated_resources.observed_at<=EXCLUDED.observed_at;
-- name: ListLBGenerated :many
SELECT * FROM network_lb_generated_resources WHERE tenant_id=$1 AND lb_id=$2 ORDER BY kind,provider_name;
-- name: ReleaseLBClaim :exec
UPDATE network_eip_claims SET released_at=clock_timestamp() WHERE tenant_id=$1 AND lb_id=$2 AND released_at IS NULL;
-- name: BoundLBClaim :exec
UPDATE network_eip_claims SET state='bound' WHERE tenant_id=$1 AND lb_id=$2 AND released_at IS NULL;
-- name: ReleaseLBVIP :exec
UPDATE network_lb_vip_intents SET released_at=clock_timestamp() WHERE tenant_id=$1 AND lb_id=$2 AND released_at IS NULL;
-- name: ReleaseLBSubnetRefs :exec
UPDATE network_lb_subnet_refs r SET released_at=clock_timestamp() WHERE r.tenant_id=sqlc.arg(tenant_id) AND r.lb_id=sqlc.arg(lb_id) AND released_at IS NULL
AND (sqlc.arg(deleted)::boolean OR (r.subnet_id<>sqlc.arg(entry_subnet)::text
 AND NOT EXISTS(SELECT 1 FROM network_lb_members m JOIN network_lb_configuration_members cm ON cm.tenant_id=m.tenant_id AND cm.lb_id=m.lb_id AND cm.member_id=m.member_id
 WHERE m.tenant_id=r.tenant_id AND m.lb_id=r.lb_id AND m.subnet_id=r.subnet_id AND cm.config_version=sqlc.arg(desired_version))
 AND NOT EXISTS(SELECT 1 FROM network_lb_members m JOIN network_lb_components c ON c.tenant_id=m.tenant_id AND c.lb_id=m.lb_id AND c.member_id=m.member_id
 WHERE m.tenant_id=r.tenant_id AND m.lb_id=r.lb_id AND m.subnet_id=r.subnet_id AND c.deleted_at IS NULL)));

-- name: CountLBs :one
SELECT count(*) FROM network_load_balancers l WHERE tenant_id=sqlc.arg(tenant_id) AND last_operation_id IS NOT NULL
AND (sqlc.arg(name_filter)::text='' OR name=sqlc.arg(name_filter))
AND (sqlc.arg(vpc_filter)::text='' OR vpc_id=sqlc.arg(vpc_filter))
AND (sqlc.arg(subnet_filter)::text='' OR subnet_id=sqlc.arg(subnet_filter))
AND (sqlc.arg(exposure_filter)::text='' OR exposure=sqlc.arg(exposure_filter))
AND ((sqlc.arg(state_filter)::text='' AND state<>'deleted') OR state=sqlc.arg(state_filter));

-- name: AdvanceLBProviderTarget :one
UPDATE network_load_balancers SET provider_target_version=desired_version,configuration_state='pending'
WHERE tenant_id=sqlc.arg(tenant_id) AND lb_id=sqlc.arg(lb_id) AND version=sqlc.arg(version) RETURNING *;
-- name: CompleteLBDatabaseOperation :execrows
UPDATE network_operations SET state='succeeded',reason='',updated_at=clock_timestamp(),completed_at=clock_timestamp(),next_attempt_at=NULL
WHERE tenant_id=sqlc.arg(tenant_id) AND lb_id=sqlc.arg(lb_id)::text AND operation_id=sqlc.arg(operation_id) AND completed_at IS NULL;
-- name: MarkLBWaitingForBackends :one
UPDATE network_load_balancers SET configuration_state='waiting_for_backends'
WHERE tenant_id=sqlc.arg(tenant_id) AND lb_id=sqlc.arg(lb_id) AND version=sqlc.arg(version) AND provider_target_version IS NULL RETURNING *;
-- name: CanDeleteLBWithoutProvider :one
SELECT NOT EXISTS(SELECT 1 FROM network_provider_bindings b WHERE b.tenant_id=sqlc.arg(tenant_id) AND b.lb_id=sqlc.arg(lb_id)::text
 AND (b.create_dispatched OR b.pending_action<>'' OR b.provider_uid<>''))
AND NOT EXISTS(SELECT 1 FROM network_lb_components c WHERE c.tenant_id=sqlc.arg(tenant_id) AND c.lb_id=sqlc.arg(lb_id)
 AND (c.create_dispatched OR c.pending_action<>'' OR c.provider_uid<>''))
AND NOT EXISTS(SELECT 1 FROM network_lb_generated_resources g WHERE g.tenant_id=sqlc.arg(tenant_id) AND g.lb_id=sqlc.arg(lb_id)) AS safe;
-- name: CompleteLBDatabaseDeletion :one
UPDATE network_load_balancers l SET state='deleted',reason='',version=version+1,updated_at=clock_timestamp(),data_plane_state='unknown',data_plane_observed_at=NULL
WHERE l.tenant_id=sqlc.arg(tenant_id) AND l.lb_id=sqlc.arg(lb_id) AND l.version=sqlc.arg(version) AND l.state='deleting'
AND NOT EXISTS(SELECT 1 FROM network_provider_bindings b WHERE b.tenant_id=l.tenant_id AND b.lb_id=l.lb_id
 AND (b.create_dispatched OR b.pending_action<>'' OR b.provider_uid<>''))
AND NOT EXISTS(SELECT 1 FROM network_lb_components c WHERE c.tenant_id=l.tenant_id AND c.lb_id=l.lb_id
 AND (c.create_dispatched OR c.pending_action<>'' OR c.provider_uid<>''))
AND NOT EXISTS(SELECT 1 FROM network_lb_generated_resources g WHERE g.tenant_id=l.tenant_id AND g.lb_id=l.lb_id)
RETURNING *;
-- name: RetireLBUndispatchedComponents :exec
UPDATE network_lb_components SET deleted_at=coalesce(deleted_at,clock_timestamp())
WHERE tenant_id=sqlc.arg(tenant_id) AND lb_id=sqlc.arg(lb_id) AND NOT create_dispatched AND pending_action='' AND provider_uid='';
-- name: GetLBRetiredBinding :one
SELECT * FROM network_lb_retired_bindings WHERE tenant_id=sqlc.arg(tenant_id) AND lb_id=sqlc.arg(lb_id) AND binding_id=sqlc.arg(binding_id);
-- name: InsertLBRetiredBinding :execrows
INSERT INTO network_lb_retired_bindings(tenant_id,cluster_id,namespace,lb_id,binding_id,provider_name,provider_uid,cleanup_target_version,operation_id,vip_occupied_revision,vip_absence_revision,cleanup_completed_at)
SELECT l.tenant_id,l.cluster_id,l.namespace,l.lb_id,b.binding_id,b.provider_name,b.provider_uid,l.provider_target_version,sqlc.arg(operation_id),l.vip_occupied_revision,l.vip_absence_revision,clock_timestamp()
FROM network_load_balancers l JOIN network_provider_bindings b USING(tenant_id,cluster_id,namespace,lb_id)
WHERE l.tenant_id=sqlc.arg(tenant_id) AND l.lb_id=sqlc.arg(lb_id) AND l.version=sqlc.arg(version)
AND l.configuration_state='waiting_for_backends' AND l.applied_version=l.provider_target_version
AND b.binding_id=sqlc.arg(binding_id) AND b.provider_uid=sqlc.arg(provider_uid) AND b.provider_uid<>'' AND b.pending_action=''
AND (l.private_ip IS NULL OR l.vip_absence_revision<>'')
AND NOT EXISTS(SELECT 1 FROM network_lb_components c WHERE c.tenant_id=l.tenant_id AND c.lb_id=l.lb_id AND c.deleted_at IS NULL)
AND NOT EXISTS(SELECT 1 FROM network_lb_generated_resources g WHERE g.tenant_id=l.tenant_id AND g.lb_id=l.lb_id AND g.gateway_uid=b.provider_uid AND g.released_at IS NULL)
ON CONFLICT (tenant_id,lb_id,binding_id) DO NOTHING;
-- name: ResetLBRetiredVIPProof :execrows
UPDATE network_load_balancers l SET vip_occupied_revision='',vip_absence_revision=''
WHERE l.tenant_id=sqlc.arg(tenant_id) AND l.lb_id=sqlc.arg(lb_id) AND l.version=sqlc.arg(version)
AND EXISTS(SELECT 1 FROM network_lb_retired_bindings r WHERE r.tenant_id=l.tenant_id AND r.lb_id=l.lb_id
 AND r.binding_id=sqlc.arg(binding_id) AND r.provider_uid=sqlc.arg(provider_uid) AND r.cleanup_target_version=l.applied_version
 AND r.vip_occupied_revision=l.vip_occupied_revision AND r.vip_absence_revision=l.vip_absence_revision);
-- name: ReserveLBClaim :exec
UPDATE network_eip_claims SET state='reserved' WHERE tenant_id=sqlc.arg(tenant_id) AND lb_id=sqlc.arg(lb_id) AND released_at IS NULL;
-- name: ParkLBReconciliation :execrows
UPDATE network_reconciliations SET next_run_at='infinity'::timestamptz,lease_owner=NULL,lease_until=NULL,lease_epoch=lease_epoch+1
WHERE tenant_id=sqlc.arg(tenant_id) AND lb_id=sqlc.arg(lb_id)::text;

-- name: CanParkLBWaitingWithoutProvider :one
SELECT EXISTS (
 SELECT 1 FROM network_load_balancers l
 JOIN network_provider_bindings b USING(tenant_id,cluster_id,namespace,lb_id)
 JOIN network_operations o ON o.tenant_id=l.tenant_id AND o.lb_id=l.lb_id AND o.operation_id=l.last_operation_id
 WHERE l.tenant_id=sqlc.arg(tenant_id) AND l.lb_id=sqlc.arg(lb_id)
 AND l.state='provisioning' AND l.configuration_state='waiting_for_backends' AND o.completed_at IS NOT NULL AND o.state='succeeded'
 AND b.pending_action=''
 AND (
  (NOT b.create_dispatched AND b.provider_uid=''
   AND NOT EXISTS(SELECT 1 FROM network_lb_components c WHERE c.tenant_id=l.tenant_id AND c.lb_id=l.lb_id
    AND (c.create_dispatched OR c.pending_action<>'' OR c.provider_uid<>''))
   AND NOT EXISTS(SELECT 1 FROM network_lb_generated_resources g WHERE g.tenant_id=l.tenant_id AND g.lb_id=l.lb_id))
  OR
  (l.applied_version=l.provider_target_version
   AND EXISTS(SELECT 1 FROM network_lb_retired_bindings r WHERE r.tenant_id=l.tenant_id AND r.cluster_id=l.cluster_id AND r.namespace=l.namespace AND r.lb_id=l.lb_id
    AND r.binding_id=b.binding_id AND r.provider_name=b.provider_name AND r.provider_uid=b.provider_uid
    AND r.cleanup_target_version=l.provider_target_version
    AND r.vip_occupied_revision=l.vip_occupied_revision AND r.vip_absence_revision=l.vip_absence_revision)
   AND NOT EXISTS(SELECT 1 FROM network_lb_components c WHERE c.tenant_id=l.tenant_id AND c.lb_id=l.lb_id AND c.deleted_at IS NULL)
   AND NOT EXISTS(SELECT 1 FROM network_lb_generated_resources g WHERE g.tenant_id=l.tenant_id AND g.lb_id=l.lb_id AND g.gateway_uid=b.provider_uid AND g.released_at IS NULL))
 )
) AS safe;

-- A confirmed retired incarnation can release product reservations without
-- rechecking whether an unrelated consumer has since allocated its freed VIP.
-- Keep this separate from never-dispatched deletion: the latter has no need to
-- read the retired fact table and must retain its existing privilege contract.
-- name: CompleteLBRetiredDatabaseDeletion :one
UPDATE network_load_balancers l SET state='deleted',reason='',version=version+1,updated_at=clock_timestamp(),data_plane_state='unknown',data_plane_observed_at=NULL
WHERE l.tenant_id=sqlc.arg(tenant_id) AND l.lb_id=sqlc.arg(lb_id) AND l.version=sqlc.arg(version) AND l.state='deleting'
AND l.configuration_state='waiting_for_backends' AND l.provider_target_version>0 AND l.applied_version=l.provider_target_version
AND EXISTS (
 SELECT 1 FROM network_provider_bindings b
 JOIN network_lb_retired_bindings r ON (r.tenant_id,r.cluster_id,r.namespace,r.lb_id,r.binding_id)=(b.tenant_id,b.cluster_id,b.namespace,b.lb_id,b.binding_id)
 JOIN network_operations o ON o.tenant_id=r.tenant_id AND o.lb_id=r.lb_id AND o.operation_id=r.operation_id
 WHERE (b.tenant_id,b.cluster_id,b.namespace,b.lb_id)=(l.tenant_id,l.cluster_id,l.namespace,l.lb_id)
 AND b.provider_uid<>'' AND b.pending_action='' AND b.pending_since IS NULL
 AND r.provider_uid=b.provider_uid AND r.provider_name=b.provider_name AND r.cleanup_target_version=l.provider_target_version
 AND r.vip_occupied_revision=l.vip_occupied_revision AND r.vip_absence_revision=l.vip_absence_revision
 AND (l.private_ip IS NULL OR r.vip_absence_revision<>'')
 AND o.state='succeeded' AND o.completed_at IS NOT NULL
)
AND NOT EXISTS(SELECT 1 FROM network_lb_components c WHERE c.tenant_id=l.tenant_id AND c.lb_id=l.lb_id AND (c.deleted_at IS NULL OR c.pending_action<>'' OR c.pending_since IS NOT NULL))
AND NOT EXISTS(SELECT 1 FROM network_lb_generated_resources g WHERE g.tenant_id=l.tenant_id AND g.lb_id=l.lb_id AND g.released_at IS NULL)
RETURNING *;
