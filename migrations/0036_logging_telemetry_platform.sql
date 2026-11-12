-- 0036: independent logging metadata for platform operations.
-- The platform (cluster-scoped) lane claims from network_platform_operations,
-- which 0034 and 0035 do not cover, so its accepted actions and their background
-- recovery had no shared trace. This column carries the persisted W3C trace
-- snapshot for those operations.
--
-- Logging metadata only: it never enters idempotency fingerprints, intent hashes,
-- resource specs or any business decision. Existing rows keep NULL and are
-- treated as "missing metadata"; there is no historical backfill.
ALTER TABLE network_platform_operations ADD COLUMN IF NOT EXISTS telemetry_context jsonb;
