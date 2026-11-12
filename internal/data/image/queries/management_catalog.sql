-- Offset pagination for the management surfaces. The filter conditions are the
-- same as the legacy cursor queries; COUNT and SELECT are always executed in the
-- same REPEATABLE READ snapshot by the adapter.

-- name: ListTenantRegistrationsOffset :many
SELECT * FROM image.registrations
WHERE owner_scope='tenant' AND tenant_id=sqlc.arg(tenant_id) AND unregistered_at IS NULL
 AND (sqlc.arg(search_text)::text='' OR display_name ILIKE sqlc.arg(search_pattern)::text ESCAPE E'\\'
  OR repository ILIKE sqlc.arg(search_pattern)::text ESCAPE E'\\')
 AND (cardinality(sqlc.arg(purposes)::text[])=0 OR purposes && sqlc.arg(purposes)::text[])
 AND (sqlc.arg(accelerator)::text='' OR accelerator=sqlc.arg(accelerator))
ORDER BY created_at DESC,image_id DESC LIMIT sqlc.arg(page_limit)::bigint OFFSET sqlc.arg(page_offset)::bigint;

-- name: ListPlatformRegistrationsOffset :many
SELECT * FROM image.registrations
WHERE owner_scope='platform' AND tenant_id IS NULL AND unregistered_at IS NULL
 AND (sqlc.arg(search_text)::text='' OR display_name ILIKE sqlc.arg(search_pattern)::text ESCAPE E'\\'
  OR repository ILIKE sqlc.arg(search_pattern)::text ESCAPE E'\\')
 AND (cardinality(sqlc.arg(purposes)::text[])=0 OR purposes && sqlc.arg(purposes)::text[])
 AND (sqlc.arg(accelerator)::text='' OR accelerator=sqlc.arg(accelerator))
ORDER BY created_at DESC,image_id DESC LIMIT sqlc.arg(page_limit)::bigint OFFSET sqlc.arg(page_offset)::bigint;
