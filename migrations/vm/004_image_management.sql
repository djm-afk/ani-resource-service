-- 004: tenant self-service VM image catalog, upload lifecycle and consumption pins.
-- Applied only by the explicit Migrate() entry point; nothing here rewrites 001-003.

CREATE TABLE vm.images (
 tenant_id uuid NOT NULL, id uuid NOT NULL,
 media_key text NOT NULL,
 kind text NOT NULL CHECK(kind IN ('DISK_IMAGE','INSTALL_ISO')),
 format text NOT NULL CHECK(format IN ('qcow2','iso')),
 name text NOT NULL, description text NOT NULL DEFAULT '',
 state text NOT NULL CHECK(state IN ('PREPARING','IMPORTING','VERIFYING','READY','FAILED','UNAVAILABLE','DELETING','DELETED')),
 reason text NOT NULL DEFAULT '',
 cleanup_state text NOT NULL CHECK(cleanup_state IN ('NONE','PENDING','RUNNING','COMPLETE','BLOCKED')),
 source_capacity_gb bigint NOT NULL CHECK(source_capacity_gb>0),
 source_requested_bytes bigint NOT NULL DEFAULT 0 CHECK(source_requested_bytes>=0),
 observed_source_capacity_bytes bigint NOT NULL DEFAULT 0 CHECK(observed_source_capacity_bytes>=0),
 source_checksum_bytes bigint NOT NULL DEFAULT 0 CHECK(source_checksum_bytes>=0),
 source_sha256 text NOT NULL DEFAULT '',
 os jsonb NOT NULL DEFAULT '{}'::jsonb,
 native jsonb NOT NULL DEFAULT '{}'::jsonb,
 cleanup jsonb NOT NULL DEFAULT '[]'::jsonb,
 version bigint NOT NULL DEFAULT 1 CHECK(version>0),
 lease_token uuid, lease_until timestamptz,
 next_attempt_at timestamptz NOT NULL DEFAULT now(),
 upload_deadline timestamptz,
 -- Consecutive background failures on this record. The worker's backoff grows from it and
 -- a successful transition resets it, so a stuck dependency cannot spin the lane and the
 -- observed version numbers keep meaning "business changes" instead of retry noise.
 attempt_count integer NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
 created_by text NOT NULL DEFAULT '',
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(tenant_id,id)
);
-- The media key is namespaced by the image UUID, so it is unique across tenants.
CREATE UNIQUE INDEX vm_image_media_key ON vm.images(media_key);
CREATE INDEX vm_image_list ON vm.images(tenant_id,created_at DESC,id DESC);
CREATE INDEX vm_image_due ON vm.images(next_attempt_at);
CREATE INDEX vm_image_source ON vm.images((native->>'dv_namespace'),(native->>'dv_name'));

CREATE TABLE vm.image_requests (
 tenant_id uuid NOT NULL, action text NOT NULL, idempotency_key text NOT NULL,
 image_id uuid NOT NULL, request_digest text NOT NULL,
 record jsonb NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(tenant_id,action,idempotency_key),
 FOREIGN KEY(tenant_id,image_id) REFERENCES vm.images(tenant_id,id)
);
CREATE INDEX vm_image_request_image ON vm.image_requests(tenant_id,image_id);

-- One pin per logical consumer volume. Uniqueness is by consumer identity, not
-- by operation id, because a retained copy is re-attached without a new pin.
CREATE TABLE vm.image_source_references (
 tenant_id uuid NOT NULL, vm_id uuid NOT NULL,
 consumer_kind text NOT NULL CHECK(consumer_kind IN ('SYSTEM_DISK','MEDIA_COPY')),
 consumer_id uuid NOT NULL,
 image_id uuid NOT NULL,
 source_fingerprint text NOT NULL,
 state text NOT NULL CHECK(state IN ('HELD','RELEASED')),
 reason text NOT NULL DEFAULT '',
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(tenant_id,vm_id,consumer_kind,consumer_id),
 FOREIGN KEY(tenant_id,image_id) REFERENCES vm.images(tenant_id,id),
 FOREIGN KEY(tenant_id,vm_id) REFERENCES vm.virtual_machines(tenant_id,id)
);
CREATE INDEX vm_image_reference_image ON vm.image_source_references(tenant_id,image_id) WHERE state='HELD';
