-- 009: independent logging metadata for the sandbox lane.
-- Adds one nullable JSONB column to the operation journal and to the tenant
-- space journal, carrying the persisted W3C trace snapshot used to correlate an
-- accepted action with its background worker.
-- These are logging metadata only: they never enter spec digests, idempotency
-- keys, mutation receipts or any business decision, and no new table is added.
-- Existing rows keep NULL and are treated as "missing metadata"; there is no
-- historical backfill.
ALTER TABLE sandbox.operations ADD COLUMN IF NOT EXISTS telemetry_context jsonb;
ALTER TABLE sandbox.tenant_spaces ADD COLUMN IF NOT EXISTS telemetry_context jsonb;
