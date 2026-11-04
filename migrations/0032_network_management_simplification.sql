-- Network status is provided by the object's controller. These columns retain
-- only Res task errors and current consumer output, never provider proofs.
ALTER TABLE network_reconciliations
 ADD COLUMN observation_error text NOT NULL DEFAULT '',
 DROP COLUMN evidence_hash,
 DROP COLUMN evidence_applied_at;
ALTER TABLE network_platform_reconciliations
 ADD COLUMN observation_error text NOT NULL DEFAULT '',
 DROP COLUMN evidence_hash,
 DROP COLUMN evidence_applied_at;
ALTER TABLE network_attachments
 ADD COLUMN observation_error text NOT NULL DEFAULT '',
 ADD COLUMN current_vnic_name text NOT NULL DEFAULT '',
 ADD COLUMN current_vnic_uid text NOT NULL DEFAULT '',
 ADD COLUMN current_mac_address text NOT NULL DEFAULT '',
 ADD COLUMN current_ips text[] NOT NULL DEFAULT '{}';

-- Preserve the output already exposed by GetAttachment before dropping its old
-- representation. This copies saved values; it does not certify their freshness
-- or issue any provider requests, and does not change resource/operation state.
WITH current_nics AS (
 SELECT a.tenant_id,a.attachment_id,r.value,r.ordinality
 FROM network_attachments a
 LEFT JOIN network_attachment_consumer_snapshots s USING(tenant_id,attachment_id)
 CROSS JOIN LATERAL jsonb_array_elements(a.provider_relations) WITH ORDINALITY r(value,ordinality)
 WHERE a.state='attached' AND a.pod_uid<>''
 AND r.value->>'Kind'='VNic' AND r.value->>'Current'='true'
 AND (r.value->>'OwnerUID'=a.pod_uid OR EXISTS (
  SELECT 1 FROM jsonb_array_elements(coalesce(nullif(s.snapshot->'ManualVNics','null'::jsonb),'[]'::jsonb)) m
  WHERE m->>'ProviderName'=r.value->>'Name'
   AND coalesce(m->>'ProviderUID','')<>'' AND m->>'ProviderUID'=r.value->>'UID'
 ))
), last_nic AS (
 SELECT DISTINCT ON(tenant_id,attachment_id) tenant_id,attachment_id,value
 FROM current_nics ORDER BY tenant_id,attachment_id,ordinality DESC
), current_addresses AS (
 SELECT a.tenant_id,a.attachment_id,array_agg(r.value->>'Address' ORDER BY r.ordinality) AS addresses
 FROM network_attachments a
 CROSS JOIN LATERAL jsonb_array_elements(a.provider_relations) WITH ORDINALITY r(value,ordinality)
 WHERE a.state='attached' AND a.pod_uid<>'' AND r.value->>'Kind'='VNicIP'
  AND r.value->>'Current'='true' AND r.value->>'AddressEligible'='true'
  AND coalesce(r.value->>'Address','')<>'' AND EXISTS (
   SELECT 1 FROM current_nics n WHERE n.tenant_id=a.tenant_id AND n.attachment_id=a.attachment_id
    AND n.value->>'UID'=r.value->>'OwnerUID'
  )
 GROUP BY a.tenant_id,a.attachment_id
)
UPDATE network_attachments a SET
 current_vnic_name=coalesce(n.value->>'Name',''),
 current_vnic_uid=coalesce(n.value->>'UID',''),
 current_mac_address=coalesce(n.value->>'MACAddress',''),
 current_ips=coalesce(i.addresses,'{}'::text[])
FROM last_nic n LEFT JOIN current_addresses i USING(tenant_id,attachment_id)
WHERE a.tenant_id=n.tenant_id AND a.attachment_id=n.attachment_id;

DROP INDEX network_attachment_vpc_active;
ALTER TABLE network_attachments
 DROP COLUMN provider_relations,
 DROP COLUMN protocol_blocked,
 DROP COLUMN evidence_hash,
 DROP COLUMN evidence_applied_at;
CREATE INDEX network_attachment_vpc_active ON network_attachments(tenant_id,vpc_id) WHERE state<>'released';
CREATE INDEX network_attachment_current_pod ON network_attachments(cluster_id,namespace,pod_name)
 WHERE state<>'released' AND pod_name<>'';
CREATE INDEX network_attachment_current_vnic ON network_attachments(cluster_id,namespace,current_vnic_name)
 WHERE state<>'released' AND current_vnic_name<>'';
UPDATE network_attachment_consumer_snapshots SET snapshot=snapshot-ARRAY['RootController','Controllers','RetiredPods','ControllerUIDs','SnapshotHash'];
ALTER TABLE network_attachment_consumer_snapshots DROP COLUMN snapshot_hash,DROP COLUMN root_controller;

ALTER TABLE network_virtual_switch_reconciliations
 ADD COLUMN observation_error text NOT NULL DEFAULT '',
 ADD COLUMN requested_generation bigint NOT NULL DEFAULT 1,
 ADD COLUMN processed_generation bigint NOT NULL DEFAULT 0,
 ADD COLUMN retry_not_before timestamptz NOT NULL DEFAULT '1970-01-01 UTC',
 ADD CONSTRAINT virtual_switch_notification_order CHECK(processed_generation>=0 AND requested_generation>=processed_generation);
ALTER TABLE network_default_vpc_egresses
 ADD COLUMN observation_error text NOT NULL DEFAULT '',
 ADD COLUMN requested_generation bigint NOT NULL DEFAULT 1,
 ADD COLUMN processed_generation bigint NOT NULL DEFAULT 0,
 ADD COLUMN retry_not_before timestamptz NOT NULL DEFAULT '1970-01-01 UTC',
 ADD CONSTRAINT default_egress_notification_order CHECK(processed_generation>=0 AND requested_generation>=processed_generation),
 DROP COLUMN pool_provider_uid,DROP COLUMN admission_hash,DROP COLUMN admitted_at,DROP COLUMN target_generation;
ALTER TABLE network_security_group_reconciliations
 ADD COLUMN observation_error text NOT NULL DEFAULT '',
 ADD COLUMN requested_generation bigint NOT NULL DEFAULT 1,
 ADD COLUMN processed_generation bigint NOT NULL DEFAULT 0,
 ADD COLUMN retry_not_before timestamptz NOT NULL DEFAULT '1970-01-01 UTC',
 ADD CONSTRAINT security_group_notification_order CHECK(processed_generation>=0 AND requested_generation>=processed_generation),
 DROP CONSTRAINT network_security_group_reconciliations_target_type_check;
ALTER TABLE network_security_group_reconciliations ADD CONSTRAINT network_security_group_reconciliations_target_type_check
 CHECK(target_type IN('security_group','eip','container','vm','vnic'));
INSERT INTO network_security_group_reconciliations(tenant_id,target_type,target_id,requested_version)
 SELECT tenant_id,target_type,target_id,version FROM network_security_group_targets
 WHERE target_type IN('container','vm','vnic') AND coalesce(payload->>'State','') NOT IN ('released','deleted')
 ON CONFLICT(tenant_id,target_type,target_id) DO NOTHING;

ALTER TABLE network_provider_bindings
 DROP COLUMN provider_ready,DROP COLUMN provider_observed_at,DROP COLUMN provider_observed_uid,
 DROP COLUMN provider_generation,DROP COLUMN provider_observed_generation;
ALTER TABLE network_vpc_base_connectivity DROP COLUMN provider_ready,DROP COLUMN provider_observed_at;
ALTER TABLE network_platform_resources DROP COLUMN provider_images;
ALTER TABLE network_snat_bindings DROP COLUMN target_generation;
DROP TABLE network_lb_generated_resources;
ALTER TABLE network_load_balancers DROP COLUMN vip_occupied_revision,DROP COLUMN vip_absence_revision,
 DROP COLUMN data_plane_state,DROP COLUMN data_plane_observed_at;
ALTER TABLE network_lb_retired_bindings DROP COLUMN vip_occupied_revision,DROP COLUMN vip_absence_revision;
ALTER TABLE network_lb_members DROP COLUMN vnic_name,DROP COLUMN vnic_uid,DROP COLUMN vnicip_name,DROP COLUMN vnicip_uid;
ALTER TABLE network_lb_capabilities DROP COLUMN fingerprint,DROP COLUMN provider_images;

DROP INDEX network_vnics_manual_mac_unique;
CREATE UNIQUE INDEX network_vnics_manual_mac_unique ON network_vnics(tenant_id,subnet_id,lower(requested_mac))
 WHERE state<>'deleted' AND requested_mac<>'';
ALTER TABLE network_public_pools DROP CONSTRAINT network_pool_scope_config;
ALTER TABLE network_public_pools ADD CONSTRAINT network_pool_scope_config CHECK (
 (scope='public' AND gateway_id IS NOT NULL AND default_vpc_name='' AND default_vpc_uid='' AND cardinality(intranet_networks)=0) OR
 (scope='intranet' AND gateway_id IS NULL AND mode='overlay' AND default_vpc_name<>'' AND default_vpc_uid<>''));
UPDATE network_virtual_switches s SET nodes=(SELECT coalesce(jsonb_agg(n.value-'LastObservedAt' ORDER BY n.ordinality),'[]'::jsonb)
 FROM jsonb_array_elements(s.nodes) WITH ORDINALITY n(value,ordinality));

-- The coordinated upgrade stops old writers first. Keep every accepted command,
-- original provider identity and historical operation; only retire old leases.
UPDATE network_reconciliations r SET lease_owner=NULL,lease_until=NULL,lease_epoch=lease_epoch+1,
 retired=coalesce(v.state,s.state,e.state,n.state,l.state,vn.state)='deleted',
 next_run_at=CASE WHEN coalesce(v.state,s.state,e.state,n.state,l.state,vn.state)='deleted'
  THEN 'infinity'::timestamptz ELSE clock_timestamp() END
FROM network_provider_bindings b
LEFT JOIN network_vpcs v ON v.tenant_id=b.tenant_id AND v.vpc_id=b.vpc_id
LEFT JOIN network_subnets s ON s.tenant_id=b.tenant_id AND s.subnet_id=b.subnet_id
LEFT JOIN network_eips e ON e.tenant_id=b.tenant_id AND e.eip_id=b.eip_id
LEFT JOIN network_snat_bindings n ON n.tenant_id=b.tenant_id AND n.snat_id=b.snat_id
LEFT JOIN network_load_balancers l ON l.tenant_id=b.tenant_id AND l.lb_id=b.lb_id
LEFT JOIN network_vnics vn ON vn.tenant_id=b.tenant_id AND vn.vnic_id=b.vnic_id
WHERE r.tenant_id=b.tenant_id AND coalesce(r.vpc_id,r.subnet_id,r.eip_id,r.snat_id,r.lb_id,r.vnic_id)=coalesce(b.vpc_id,b.subnet_id,b.eip_id,b.snat_id,b.lb_id,b.vnic_id);
UPDATE network_platform_reconciliations q SET lease_owner=NULL,lease_until=NULL,lease_epoch=lease_epoch+1,
 next_run_at=CASE WHEN p.state='deleted' THEN 'infinity'::timestamptz ELSE clock_timestamp() END
FROM network_platform_resources p WHERE p.resource_id=q.resource_id;
UPDATE network_attachments SET lease_owner=NULL,lease_until=NULL,epoch=epoch+1,
 next_check_at=CASE WHEN state='released' THEN 'infinity'::timestamptz ELSE clock_timestamp() END;
UPDATE network_virtual_switch_reconciliations q SET lease_owner=NULL,lease_until=NULL,lease_epoch=lease_epoch+1,
 next_run_at=CASE WHEN s.state='deleted' THEN 'infinity'::timestamptz ELSE clock_timestamp() END
FROM network_virtual_switches s WHERE s.resource_id=q.resource_id;
UPDATE network_default_vpc_egresses SET lease_owner=NULL,lease_until=NULL,lease_epoch=lease_epoch+1,
 next_run_at=CASE WHEN retired_at IS NOT NULL THEN 'infinity'::timestamptz ELSE clock_timestamp() END;
UPDATE network_security_group_reconciliations q SET lease_owner=NULL,lease_until=NULL,lease_epoch=lease_epoch+1,
 next_run_at=CASE WHEN EXISTS(SELECT 1 FROM network_security_groups g WHERE q.target_type='security_group'
   AND g.tenant_id=q.tenant_id AND g.security_group_id=q.target_id AND g.state='deleted')
  OR EXISTS(SELECT 1 FROM network_security_group_targets t WHERE t.tenant_id=q.tenant_id AND t.target_type=q.target_type
   AND t.target_id=q.target_id AND t.payload->>'State' IN ('released','deleted')) THEN 'infinity'::timestamptz ELSE clock_timestamp() END;
