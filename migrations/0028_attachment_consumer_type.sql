-- Record which product resource consumes an attachment. workload_mode stays an
-- attachment protocol fact; it is not a substitute for the consumer identity.
ALTER TABLE network_attachments ADD COLUMN consumer_type text NOT NULL DEFAULT ''
    CHECK (consumer_type IN ('','container_instance','virtual_machine','sandbox'));

-- Historical rows only carry a protocol mode. Rows without a managed mode keep
-- an empty type and are reported as unknown instead of guessed.
UPDATE network_attachments SET consumer_type = CASE plan->>'WorkloadMode'
    WHEN 'vm_managed' THEN 'virtual_machine'
    WHEN 'controller_managed' THEN 'container_instance'
    ELSE '' END
WHERE plan->>'WorkloadMode' IN ('vm_managed','controller_managed');

-- VPC-scoped consumer lookup. The subnet index already exists and the released
-- predicate mirrors the occupancy rule used by attachment admission.
CREATE INDEX network_attachment_vpc_active ON network_attachments (tenant_id,vpc_id)
    WHERE state<>'released' OR protocol_blocked;
