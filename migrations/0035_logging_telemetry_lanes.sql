-- 0035: independent logging metadata for the lane-specific network operations.
-- The virtual-switch, security-group and default-VPC-egress workers claim from
-- their own operation tables rather than network_operations, so 0034 does not
-- cover them. Each column carries the persisted W3C trace snapshot that
-- correlates the accepted action with its background worker.
--
-- Logging metadata only: it never enters idempotency fingerprints, intent
-- hashes, resource specs or any business decision. Existing rows keep NULL and
-- are treated as "missing metadata"; there is no historical backfill.
ALTER TABLE network_virtual_switch_operations ADD COLUMN IF NOT EXISTS telemetry_context jsonb;
ALTER TABLE network_security_group_operations ADD COLUMN IF NOT EXISTS telemetry_context jsonb;
ALTER TABLE network_default_vpc_egress_operations ADD COLUMN IF NOT EXISTS telemetry_context jsonb;
