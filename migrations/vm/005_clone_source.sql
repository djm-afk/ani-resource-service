-- Phase 2: managed VM clone source references.
-- The table is the only authority for HELD/RELEASED. source_version is platform
-- concurrency control over the source business row, not a native generation.
CREATE TABLE vm.clone_source_references (
  tenant_id uuid NOT NULL,
  source_vm_id uuid NOT NULL,
  target_vm_id uuid NOT NULL,
  create_operation_id uuid NOT NULL,
  source_version bigint NOT NULL,
  source_native_uid text NOT NULL,
  state text NOT NULL CHECK (state IN ('HELD','RELEASED')),
  release_kind text NOT NULL DEFAULT '' CHECK (release_kind IN ('','REGISTERED','FAILED_NO_OUTPUT','GONE_NO_OUTPUT','ADMIN_RELEASED','CANCELLED')),
  release_reason text NOT NULL DEFAULT '',
  released_by text NOT NULL DEFAULT '',
  release_evidence text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now(),
  released_at timestamptz,
  PRIMARY KEY (tenant_id, target_vm_id),
  FOREIGN KEY (tenant_id, target_vm_id) REFERENCES vm.virtual_machines(tenant_id, id),
  CONSTRAINT clone_source_release_consistent CHECK (
    (state = 'HELD' AND released_at IS NULL AND release_kind = '' AND released_by = '' AND release_reason = '' AND release_evidence = '')
    OR
    (state = 'RELEASED' AND released_at IS NOT NULL AND release_kind <> '' AND released_by <> '' AND release_evidence <> '')
  )
);
-- One source can be held by at most one target at a time.
CREATE UNIQUE INDEX clone_source_one_held
  ON vm.clone_source_references (tenant_id, source_vm_id)
  WHERE state = 'HELD';
-- Patrol query for references that should have been handled but were not.
CREATE INDEX clone_source_by_state
  ON vm.clone_source_references (state, created_at);
