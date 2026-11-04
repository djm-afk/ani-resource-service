-- Events only schedule existing Res tasks; they never apply provider state,
-- revoke a lease or advance a product version. Backoff remains a lower bound.
-- name: NotifyResource :execrows
UPDATE network_reconciliations SET requested_generation=requested_generation+1,
 next_run_at=greatest(retry_not_before,least(next_run_at,clock_timestamp()))
WHERE NOT retired AND tenant_id=sqlc.arg(tenant_id) AND coalesce(vpc_id,subnet_id,eip_id,snat_id,lb_id,vnic_id)=sqlc.arg(resource_id)::text;

-- name: NotifyAttachment :execrows
UPDATE network_attachments SET requested_generation=requested_generation+1,
 next_check_at=greatest(retry_not_before,least(next_check_at,clock_timestamp()))
WHERE state<>'released' AND tenant_id=sqlc.arg(tenant_id) AND attachment_id=sqlc.arg(attachment_id);

-- name: NotifyPlatform :execrows
UPDATE network_platform_reconciliations q SET requested_generation=q.requested_generation+1,
 next_run_at=greatest(q.retry_not_before,least(q.next_run_at,clock_timestamp()))
FROM network_platform_resources p WHERE p.resource_id=q.resource_id AND p.state<>'deleted' AND q.resource_id=sqlc.arg(resource_id);

-- name: NotifyVirtualSwitch :execrows
UPDATE network_virtual_switch_reconciliations q SET requested_generation=q.requested_generation+1,
 next_run_at=greatest(q.retry_not_before,least(q.next_run_at,clock_timestamp()))
FROM network_virtual_switches s WHERE s.resource_id=q.resource_id AND s.state<>'deleted' AND q.resource_id=sqlc.arg(resource_id);

-- name: NotifyDefaultEgress :execrows
UPDATE network_default_vpc_egresses SET requested_generation=requested_generation+1,
 next_run_at=greatest(retry_not_before,least(next_run_at,clock_timestamp()))
WHERE retired_at IS NULL AND state<>'deleted' AND cluster_id=sqlc.arg(cluster_id)::text AND resource_id=sqlc.arg(resource_id);

-- name: NotifySecurityGroupTarget :execrows
UPDATE network_security_group_reconciliations q SET requested_generation=q.requested_generation+1,
 next_run_at=greatest(q.retry_not_before,least(q.next_run_at,clock_timestamp()))
WHERE q.tenant_id=sqlc.arg(tenant_id) AND q.target_type=sqlc.arg(target_type) AND q.target_id=sqlc.arg(target_id)
 AND (EXISTS(SELECT 1 FROM network_security_groups g WHERE q.target_type='security_group' AND g.tenant_id=q.tenant_id AND g.security_group_id=q.target_id AND g.state<>'deleted')
 OR EXISTS(SELECT 1 FROM network_security_group_targets t WHERE t.tenant_id=q.tenant_id AND t.target_type=q.target_type AND t.target_id=q.target_id AND t.payload->>'State' NOT IN ('released','deleted')));

-- Match only the current names registered by Res. No dependency graph,
-- historical controller chain, child ownership inventory or health fanout.
-- name: LookupObservationTargets :many
SELECT 'resource'::text AS target_type,coalesce(b.vpc_id,b.subnet_id,b.eip_id,b.snat_id,b.lb_id,b.vnic_id)::text AS target_id,b.tenant_id::text AS tenant_id
FROM network_provider_bindings b JOIN network_reconciliations q ON q.tenant_id=b.tenant_id
 AND coalesce(q.vpc_id,q.subnet_id,q.eip_id,q.snat_id,q.lb_id,q.vnic_id)=coalesce(b.vpc_id,b.subnet_id,b.eip_id,b.snat_id,b.lb_id,b.vnic_id)
WHERE b.cluster_id=sqlc.arg(cluster_id)::text AND b.namespace=sqlc.arg(namespace)::text AND b.provider_name=sqlc.arg(name) AND NOT q.retired
 AND ((sqlc.arg(api_group)::text='networking.kubercloud.com' AND sqlc.arg(resource)::text=CASE b.resource_kind WHEN 'vpc' THEN 'vpcs' WHEN 'subnet' THEN 'subnets' WHEN 'eip' THEN 'eips' WHEN 'snat' THEN 'snats' WHEN 'vnic' THEN 'vnics' END)
 OR (b.resource_kind='load_balancer' AND sqlc.arg(api_group)::text='gateway.networking.k8s.io' AND sqlc.arg(resource)::text='gateways')
 OR (b.resource_kind='vnic' AND sqlc.arg(api_group)::text='networking.kubercloud.com' AND sqlc.arg(resource)::text='vnicips'
  AND EXISTS(SELECT 1 FROM network_vnics v WHERE v.tenant_id=b.tenant_id AND v.vnic_id=b.vnic_id AND v.requested_ip<>'')))
UNION
SELECT 'resource'::text,c.lb_id,c.tenant_id::text FROM network_lb_components c JOIN network_reconciliations q ON q.tenant_id=c.tenant_id AND q.lb_id=c.lb_id
WHERE c.cluster_id=sqlc.arg(cluster_id)::text AND c.namespace=sqlc.arg(namespace)::text AND c.provider_name=sqlc.arg(name) AND c.deleted_at IS NULL AND NOT q.retired
 AND sqlc.arg(api_group)::text=CASE WHEN c.kind IN('gateway','route','tcp_route','udp_route') THEN 'gateway.networking.k8s.io' ELSE 'gateway.envoyproxy.io' END
 AND sqlc.arg(resource)::text=CASE c.kind WHEN 'gateway' THEN 'gateways' WHEN 'route' THEN 'httproutes' WHEN 'tcp_route' THEN 'tcproutes' WHEN 'udp_route' THEN 'udproutes'
  WHEN 'envoy_proxy' THEN 'envoyproxies' WHEN 'backend' THEN 'backends' WHEN 'policy' THEN 'backendtrafficpolicies' WHEN 'client_policy' THEN 'clienttrafficpolicies' WHEN 'patch_policy' THEN 'envoypatchpolicies' END
UNION
SELECT 'platform'::text,p.resource_id,''::text FROM network_platform_resources p JOIN network_platform_reconciliations q USING(resource_id)
WHERE p.cluster_id=sqlc.arg(cluster_id)::text AND p.state<>'deleted' AND p.provider_name=sqlc.arg(name)
 AND sqlc.arg(api_group)::text='networking.kubercloud.com'
 AND sqlc.arg(namespace)::text=CASE WHEN p.kind='public_pool' THEN 'kcn-system' ELSE '' END
 AND sqlc.arg(resource)::text=CASE p.kind WHEN 'public_pool' THEN 'subnets' WHEN 'egress_gateway' THEN 'eipgateways' WHEN 'vlan_network' THEN 'vlannetworks' END
UNION
SELECT 'platform_network'::text,v.vpc_id,''::text FROM network_platform_vpcs v
WHERE v.cluster_id=sqlc.arg(cluster_id)::text AND v.namespace=sqlc.arg(namespace)::text AND v.provider_name=sqlc.arg(name)
 AND sqlc.arg(api_group)::text='networking.kubercloud.com' AND sqlc.arg(resource)::text='vpcs'
UNION
SELECT 'platform_network'::text,s.subnet_id,''::text FROM network_platform_subnets s
WHERE s.cluster_id=sqlc.arg(cluster_id)::text AND s.namespace=sqlc.arg(namespace)::text AND s.provider_name=sqlc.arg(name)
 AND sqlc.arg(api_group)::text='networking.kubercloud.com' AND sqlc.arg(resource)::text='subnets'
UNION
SELECT 'attachment'::text,a.attachment_id,a.tenant_id::text FROM network_attachments a
WHERE a.cluster_id=sqlc.arg(cluster_id)::text AND a.state<>'released'
 AND ((sqlc.arg(api_group)::text='' AND sqlc.arg(resource)::text='pods' AND a.namespace=sqlc.arg(namespace)::text AND a.pod_name=sqlc.arg(name))
 OR (sqlc.arg(api_group)::text='networking.kubercloud.com' AND sqlc.arg(resource)::text='vnics'
  AND (a.namespace=sqlc.arg(namespace)::text AND a.current_vnic_name=sqlc.arg(name) OR a.current_vnic_name=sqlc.arg(namespace)::text||'/'||sqlc.arg(name)::text)))
UNION
SELECT 'virtual_switch'::text,s.resource_id,''::text FROM network_virtual_switches s JOIN network_virtual_switch_reconciliations q USING(resource_id)
WHERE s.cluster_id=sqlc.arg(cluster_id)::text AND s.state<>'deleted' AND sqlc.arg(api_group)::text='' AND sqlc.arg(resource)::text='nodes' AND sqlc.arg(namespace)::text=''
 AND EXISTS(SELECT 1 FROM jsonb_array_elements(s.nodes) n WHERE n->>'NodeName'=sqlc.arg(name))
UNION
SELECT 'default_egress'::text,d.resource_id,''::text FROM network_default_vpc_egresses d
WHERE d.cluster_id=sqlc.arg(cluster_id)::text AND d.default_vpc_namespace=sqlc.arg(namespace)::text AND d.retired_at IS NULL AND d.state<>'deleted' AND sqlc.arg(api_group)::text='networking.kubercloud.com'
 AND ((sqlc.arg(resource)::text='eips' AND d.eip_provider_name=sqlc.arg(name)) OR (sqlc.arg(resource)::text='snats' AND d.snat_provider_name=sqlc.arg(name)))
UNION
SELECT 'sg_security_group'::text,g.security_group_id,g.tenant_id::text FROM network_security_groups g
JOIN network_security_group_reconciliations q ON q.tenant_id=g.tenant_id AND q.target_type='security_group' AND q.target_id=g.security_group_id
WHERE g.state<>'deleted' AND g.payload->>'ClusterID'=sqlc.arg(cluster_id)::text AND g.payload->>'Namespace'=sqlc.arg(namespace)::text AND g.payload->>'ProviderName'=sqlc.arg(name)
 AND sqlc.arg(api_group)::text='networking.kubercloud.com' AND sqlc.arg(resource)::text='securitygroups'
UNION
SELECT ('sg_'||q.target_type)::text,q.target_id,q.tenant_id::text FROM network_security_group_reconciliations q
JOIN network_provider_bindings b ON b.tenant_id=q.tenant_id AND ((q.target_type='eip' AND b.eip_id=q.target_id) OR (q.target_type='vnic' AND b.vnic_id=q.target_id))
JOIN network_security_group_targets t ON t.tenant_id=q.tenant_id AND t.target_type=q.target_type AND t.target_id=q.target_id
WHERE b.cluster_id=sqlc.arg(cluster_id)::text AND b.namespace=sqlc.arg(namespace)::text AND b.provider_name=sqlc.arg(name) AND t.payload->>'State' NOT IN ('released','deleted')
 AND sqlc.arg(api_group)::text='networking.kubercloud.com' AND sqlc.arg(resource)::text=CASE q.target_type WHEN 'eip' THEN 'eips' WHEN 'vnic' THEN 'vnics' END
UNION
SELECT ('sg_'||q.target_type)::text,q.target_id,q.tenant_id::text FROM network_security_group_reconciliations q
JOIN network_attachments a ON a.tenant_id=q.tenant_id AND a.instance_id=q.target_id AND q.target_type=CASE a.consumer_type WHEN 'container_instance' THEN 'container' WHEN 'virtual_machine' THEN 'vm' END
JOIN network_security_group_targets t ON t.tenant_id=q.tenant_id AND t.target_type=q.target_type AND t.target_id=q.target_id
WHERE a.cluster_id=sqlc.arg(cluster_id)::text AND a.state<>'released' AND t.payload->>'State' NOT IN ('released','deleted') AND sqlc.arg(api_group)::text='networking.kubercloud.com' AND sqlc.arg(resource)::text='vnics'
 AND ((a.namespace=sqlc.arg(namespace)::text AND a.current_vnic_name=sqlc.arg(name)) OR a.current_vnic_name=sqlc.arg(namespace)::text||'/'||sqlc.arg(name)::text);

-- Once per observer startup, wake persisted work to cover downtime deletes.
-- No Kubernetes scan, resource status reset or lease invalidation is involved.
-- name: WakeManagedTasks :exec
WITH resources AS (
 UPDATE network_reconciliations q SET requested_generation=q.requested_generation+1,next_run_at=greatest(q.retry_not_before,least(q.next_run_at,clock_timestamp()))
 FROM network_provider_bindings b WHERE NOT q.retired AND b.cluster_id=sqlc.arg(cluster_id)::text AND b.tenant_id=q.tenant_id
 AND coalesce(q.vpc_id,q.subnet_id,q.eip_id,q.snat_id,q.lb_id,q.vnic_id)=coalesce(b.vpc_id,b.subnet_id,b.eip_id,b.snat_id,b.lb_id,b.vnic_id) RETURNING q.tenant_id
), platform AS (
 UPDATE network_platform_reconciliations q SET requested_generation=q.requested_generation+1,next_run_at=greatest(q.retry_not_before,least(q.next_run_at,clock_timestamp()))
 FROM network_platform_resources p WHERE p.resource_id=q.resource_id AND p.cluster_id=sqlc.arg(cluster_id)::text AND p.state<>'deleted' AND p.kind<>'device' RETURNING q.resource_id
), platform_networks AS (
 UPDATE network_platform_network_observations q SET requested_generation=q.requested_generation+1,next_run_at=greatest(q.retry_not_before,least(q.next_run_at,clock_timestamp()))
 WHERE EXISTS(SELECT 1 FROM network_platform_vpcs v WHERE v.vpc_id=q.vpc_id AND v.cluster_id=sqlc.arg(cluster_id)::text)
 OR EXISTS(SELECT 1 FROM network_platform_subnets s WHERE s.subnet_id=q.subnet_id AND s.cluster_id=sqlc.arg(cluster_id)::text) RETURNING q.resource_id
), attachments AS (
 UPDATE network_attachments SET requested_generation=requested_generation+1,next_check_at=greatest(retry_not_before,least(next_check_at,clock_timestamp()))
 WHERE cluster_id=sqlc.arg(cluster_id)::text AND state<>'released' RETURNING attachment_id
), switches AS (
 UPDATE network_virtual_switch_reconciliations q SET requested_generation=q.requested_generation+1,next_run_at=greatest(q.retry_not_before,least(q.next_run_at,clock_timestamp()))
 FROM network_virtual_switches s WHERE s.resource_id=q.resource_id AND s.cluster_id=sqlc.arg(cluster_id)::text AND s.state<>'deleted' RETURNING q.resource_id
), defaults AS (
 UPDATE network_default_vpc_egresses SET requested_generation=requested_generation+1,next_run_at=greatest(retry_not_before,least(next_run_at,clock_timestamp()))
 WHERE cluster_id=sqlc.arg(cluster_id)::text AND retired_at IS NULL AND state<>'deleted' RETURNING resource_id
)
UPDATE network_security_group_reconciliations q SET requested_generation=q.requested_generation+1,next_run_at=greatest(q.retry_not_before,least(q.next_run_at,clock_timestamp()))
WHERE EXISTS(SELECT 1 FROM network_security_groups g WHERE q.target_type='security_group' AND g.tenant_id=q.tenant_id AND g.security_group_id=q.target_id AND g.state<>'deleted' AND g.payload->>'ClusterID'=sqlc.arg(cluster_id)::text)
 OR EXISTS(SELECT 1 FROM network_security_group_targets t JOIN network_tenant_namespaces n ON n.tenant_id=t.tenant_id AND n.namespace=t.payload->>'Namespace'
 WHERE t.tenant_id=q.tenant_id AND t.target_type=q.target_type AND t.target_id=q.target_id AND t.payload->>'State' NOT IN ('released','deleted') AND n.cluster_id=sqlc.arg(cluster_id)::text);
