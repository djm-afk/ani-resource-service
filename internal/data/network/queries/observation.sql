-- Observer hints may only wake already registered, tenant-scoped identities.
-- Hints never revoke a live lease or mutate a public version. A failure's
-- persisted backoff remains a lower bound even under an event storm.
-- name: NotifyResource :execrows
UPDATE network_reconciliations
SET requested_generation=requested_generation+1,
    next_run_at=greatest(retry_not_before,least(next_run_at,clock_timestamp()))
WHERE NOT retired AND tenant_id=sqlc.arg(tenant_id) AND coalesce(vpc_id,subnet_id,eip_id,snat_id,lb_id,vnic_id)=sqlc.arg(resource_id)::text;

-- name: NotifyAttachment :execrows
UPDATE network_attachments
SET requested_generation=requested_generation+1,
    next_check_at=greatest(retry_not_before,least(next_check_at,clock_timestamp()))
WHERE tenant_id=sqlc.arg(tenant_id) AND attachment_id=sqlc.arg(attachment_id);

-- Complete range inventory for observer recovery only; not a product query.
-- name: ObservationResources :many
SELECT r.tenant_id,coalesce(r.vpc_id,r.subnet_id,r.eip_id,r.snat_id,r.lb_id,r.vnic_id)::text AS resource_id,
 r.requested_generation,(coalesce(v.state,s.state,e.state,n.state,l.state,vn.state)<>'deleted' OR r.next_run_at<>'infinity'::timestamptz)::boolean AS recovery,b.resource_kind,b.namespace,b.provider_name,b.provider_uid
FROM network_reconciliations r JOIN network_provider_bindings b
 ON b.tenant_id=r.tenant_id AND coalesce(b.vpc_id,b.subnet_id,b.eip_id,b.snat_id,b.lb_id,b.vnic_id)=coalesce(r.vpc_id,r.subnet_id,r.eip_id,r.snat_id,r.lb_id,r.vnic_id)
LEFT JOIN network_vpcs v ON v.tenant_id=r.tenant_id AND v.vpc_id=r.vpc_id
LEFT JOIN network_subnets s ON s.tenant_id=r.tenant_id AND s.subnet_id=r.subnet_id
LEFT JOIN network_eips e ON e.tenant_id=r.tenant_id AND e.eip_id=r.eip_id
LEFT JOIN network_snat_bindings n ON n.tenant_id=r.tenant_id AND n.snat_id=r.snat_id
LEFT JOIN network_load_balancers l ON l.tenant_id=r.tenant_id AND l.lb_id=r.lb_id
LEFT JOIN network_vnics vn ON vn.tenant_id=r.tenant_id AND vn.vnic_id=r.vnic_id
WHERE b.cluster_id=sqlc.arg(cluster_id) AND NOT r.retired;

-- name: ObservationAttachments :many
SELECT a.tenant_id,a.attachment_id,a.subnet_id,a.vpc_id,a.namespace,a.pod_name,a.pod_uid,
 coalesce(a.plan->>'WorkloadMode','single_pod')::text AS workload_mode,a.provider_relations,a.requested_generation,
 (a.state<>'released' OR a.protocol_blocked OR a.next_check_at<>'infinity'::timestamptz)::boolean AS recovery,coalesce(c.snapshot,'null'::jsonb) AS consumer_snapshot
FROM network_attachments a LEFT JOIN network_attachment_consumer_snapshots c
 ON c.tenant_id=a.tenant_id AND c.attachment_id=a.attachment_id AND coalesce(a.plan->>'WorkloadMode','single_pod')='vm_managed'
WHERE a.cluster_id=sqlc.arg(cluster_id);

-- Observer-only bulk relationship inventory. No per-tenant/per-resource query
-- loop on a platform event; all fanout joins preserve tenant and cluster scope.
-- name: ObservationEgressEdges :many
WITH targets AS (
 SELECT e.tenant_id::text AS tenant_id,e.eip_id AS resource_id,'eip'::text AS kind,e.pool_id,
 s.vpc_id,e.eip_id,s.snat_id FROM network_eips e
 LEFT JOIN network_snat_bindings s ON s.tenant_id=e.tenant_id AND s.eip_id=e.eip_id AND s.state<>'deleted'
 WHERE e.cluster_id=sqlc.arg(cluster_id)
 UNION ALL
 SELECT s.tenant_id::text,s.snat_id,'snat'::text,e.pool_id,s.vpc_id,s.eip_id,s.snat_id
 FROM network_snat_bindings s JOIN network_eips e ON e.tenant_id=s.tenant_id AND e.eip_id=s.eip_id
 WHERE s.cluster_id=sqlc.arg(cluster_id)
 UNION ALL
 SELECT b.tenant_id::text,b.vpc_id,'vpc'::text,b.pool_id,b.vpc_id,b.eip_id,b.snat_id
 FROM network_vpc_base_connectivity b WHERE b.cluster_id=sqlc.arg(cluster_id)
 UNION ALL
 SELECT ''::text,p.resource_id,p.kind,p.resource_id,NULL::text,NULL::text,NULL::text
 FROM network_platform_resources p WHERE p.cluster_id=sqlc.arg(cluster_id) AND p.kind='public_pool'
)
SELECT t.tenant_id,t.resource_id,t.kind,b.resource_kind AS ref_kind,b.namespace,b.provider_name,b.provider_uid
FROM targets t JOIN network_provider_bindings b ON b.tenant_id::text=t.tenant_id AND b.cluster_id=sqlc.arg(cluster_id)
 AND (b.eip_id=t.eip_id OR b.snat_id=t.snat_id OR b.vpc_id=t.vpc_id)
UNION ALL
SELECT t.tenant_id,t.resource_id,t.kind,p.kind AS ref_kind,
 CASE WHEN p.kind='public_pool' THEN 'kcn-system' ELSE '' END::text AS namespace,p.provider_name,p.provider_uid
FROM targets t JOIN network_public_pools cfg ON cfg.cluster_id=sqlc.arg(cluster_id) AND cfg.resource_id=t.pool_id
LEFT JOIN network_vlan_networks vlan ON vlan.cluster_id=cfg.cluster_id AND vlan.resource_id=cfg.vlan_network_id
JOIN network_platform_resources p ON p.cluster_id=cfg.cluster_id AND p.resource_id IN (cfg.resource_id,cfg.gateway_id,cfg.vlan_network_id,vlan.device_id)
UNION ALL
SELECT t.tenant_id,t.resource_id,t.kind,'vpc'::text,'kcn-system'::text,cfg.default_vpc_name,cfg.default_vpc_uid
FROM targets t JOIN network_public_pools cfg ON cfg.cluster_id=sqlc.arg(cluster_id) AND cfg.resource_id=t.pool_id WHERE cfg.scope='intranet';

-- Complete worker-only fanout inventory, preserving tenant/cluster placement.
-- name: ObservationLBEdges :many
SELECT l.tenant_id,l.lb_id,('load-balancer:'||l.namespace||'/'||l.lb_id)::text AS relation_key
FROM network_load_balancers l WHERE l.cluster_id=sqlc.arg(cluster_id) AND l.last_operation_id IS NOT NULL
UNION ALL
SELECT l.tenant_id,l.lb_id,('object:'||CASE b.resource_kind WHEN 'vpc' THEN 'VPC' WHEN 'subnet' THEN 'Subnet' ELSE 'EIP' END||'/'||b.namespace||'/'||b.provider_name)::text
FROM network_load_balancers l JOIN network_provider_bindings b ON b.tenant_id=l.tenant_id AND b.cluster_id=l.cluster_id
 AND (b.vpc_id=l.vpc_id OR b.subnet_id=l.subnet_id OR b.eip_id=l.public_eip_id)
WHERE l.cluster_id=sqlc.arg(cluster_id) AND l.last_operation_id IS NOT NULL
UNION ALL
SELECT l.tenant_id,l.lb_id,('eip:'||b.namespace||'/'||b.provider_name)::text
FROM network_load_balancers l JOIN network_provider_bindings b ON b.tenant_id=l.tenant_id AND b.cluster_id=l.cluster_id AND b.eip_id=l.public_eip_id
WHERE l.cluster_id=sqlc.arg(cluster_id) AND l.last_operation_id IS NOT NULL
UNION ALL
SELECT c.tenant_id,c.lb_id,('object:'||CASE c.kind WHEN 'route' THEN 'HTTPRoute' WHEN 'policy' THEN 'BackendTrafficPolicy' ELSE 'Backend' END||'/'||c.namespace||'/'||c.provider_name)::text
FROM network_lb_components c WHERE c.cluster_id=sqlc.arg(cluster_id)
UNION ALL
SELECT m.tenant_id,m.lb_id,('uid:'||x.uid)::text FROM network_lb_members m CROSS JOIN LATERAL (VALUES(m.pod_uid),(m.vnic_uid),(m.vnicip_uid)) x(uid) WHERE m.cluster_id=sqlc.arg(cluster_id)
UNION ALL
SELECT m.tenant_id,m.lb_id,('attachment:'||m.attachment_id)::text FROM network_lb_members m WHERE m.cluster_id=sqlc.arg(cluster_id)
UNION ALL
SELECT r.tenant_id,r.lb_id,('subnet:'||b.namespace||'/'||b.provider_name)::text FROM network_lb_subnet_refs r JOIN network_provider_bindings b ON b.tenant_id=r.tenant_id AND b.subnet_id=r.subnet_id AND b.cluster_id=r.cluster_id WHERE r.cluster_id=sqlc.arg(cluster_id) AND r.released_at IS NULL
UNION ALL
SELECT g.tenant_id,g.lb_id,('uid:'||g.provider_uid)::text FROM network_lb_generated_resources g WHERE g.cluster_id=sqlc.arg(cluster_id);

-- Successful shared collection wakes application without inventing a new
-- event generation. Notifications and fault backoff retain precedence.
-- name: WakeAuditedResource :exec
UPDATE network_reconciliations SET next_run_at=greatest(retry_not_before,least(next_run_at,clock_timestamp()))
WHERE NOT retired AND tenant_id=sqlc.arg(tenant_id) AND coalesce(vpc_id,subnet_id,eip_id,snat_id,lb_id,vnic_id)=sqlc.arg(resource_id)::text;
-- name: WakeAuditedPlatform :exec
UPDATE network_platform_reconciliations SET next_run_at=greatest(retry_not_before,least(next_run_at,clock_timestamp())) WHERE resource_id=sqlc.arg(resource_id);
-- name: WakeAuditedAttachment :exec
UPDATE network_attachments SET next_check_at=greatest(retry_not_before,least(next_check_at,clock_timestamp())) WHERE tenant_id=sqlc.arg(tenant_id) AND attachment_id=sqlc.arg(attachment_id);
