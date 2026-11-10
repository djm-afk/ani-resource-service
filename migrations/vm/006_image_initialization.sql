-- 005: tenant-declared initialization capability for uploaded images.
-- Applied only by the explicit Migrate() entry point; nothing here rewrites 001-004.
-- The declaration is attested by the tenant at create time and frozen afterwards;
-- legacy rows keep the empty default, which every projection reads as NONE.

ALTER TABLE vm.images ADD COLUMN initialization_capability text NOT NULL DEFAULT ''
 CHECK(initialization_capability IN ('','NONE','CLOUD_INIT_NOCLOUD'));
