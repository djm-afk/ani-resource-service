-- 009: independent logging metadata for the VM image lane.
-- Adds one nullable JSONB column carrying the persisted W3C trace snapshot
-- used to correlate an accepted image action with its background worker.
-- It is logging metadata only: it never participates in the image request
-- digest, idempotency key, version bumps or any business decision.
-- Existing rows keep NULL and are treated as "missing metadata", not as an
-- empty trace; there is no historical backfill.
ALTER TABLE vm.images ADD COLUMN IF NOT EXISTS telemetry_context jsonb;
