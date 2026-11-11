-- Existing hashes, snapshots, operations, ACKs and release facts stay intact.
-- Only the explicit offline manifest tool marks closed old GPU rows archived.
ALTER TABLE instance.instances ADD COLUMN archived_at timestamptz;
ALTER TABLE instance.instances ADD CONSTRAINT instance_archive_terminal
 CHECK(archived_at IS NULL OR desired='deleted');
CREATE INDEX instances_active_tenant_created
 ON instance.instances(tenant_id,created_at DESC,instance_id DESC)
 WHERE archived_at IS NULL;
