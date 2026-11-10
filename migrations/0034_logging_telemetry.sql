-- 0034: independent logging metadata for network operations.
-- Adds one nullable JSONB column carrying the persisted W3C trace snapshot that
-- correlates an accepted network action with its background worker. It is
-- logging metadata only: it never enters idempotency fingerprints, intent
-- hashes, resource specs or any business decision. Existing rows keep NULL and
-- are treated as "missing metadata"; there is no historical backfill.
ALTER TABLE network_operations ADD COLUMN IF NOT EXISTS telemetry_context jsonb;
