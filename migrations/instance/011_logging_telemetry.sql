-- 011: independent logging metadata for the instance lane.
-- Adds one nullable JSONB column to each existing record that a background
-- worker claims. These carry the persisted W3C trace snapshot used to correlate
-- an accepted action with its worker pass.
-- They are logging metadata only: they never enter request hashes, ACK
-- identities, execution specs, refund authority, unique keys or any business
-- decision. Existing rows keep NULL and are treated as "missing metadata";
-- there is no historical backfill, and no new table is created.
ALTER TABLE instance.instance_operations ADD COLUMN IF NOT EXISTS telemetry_context jsonb;
ALTER TABLE instance.borrowed_pvc_guards ADD COLUMN IF NOT EXISTS telemetry_context jsonb;
ALTER TABLE instance.instance_quota_release_outbox ADD COLUMN IF NOT EXISTS telemetry_context jsonb;
