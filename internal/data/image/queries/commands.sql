-- name: GetTenantCommand :one
SELECT * FROM image.commands WHERE owner_scope='tenant' AND tenant_id=sqlc.arg(tenant_id)
 AND space_id=sqlc.arg(space_id) AND idempotency_key=sqlc.arg(idempotency_key);

-- name: GetPlatformCommand :one
SELECT * FROM image.commands WHERE owner_scope='platform' AND tenant_id IS NULL
 AND space_id=sqlc.arg(space_id) AND idempotency_key=sqlc.arg(idempotency_key);

-- name: InsertTenantCommand :one
INSERT INTO image.commands(command_id,space_id,owner_scope,tenant_id,idempotency_key,kind,actor,fingerprint,state)
VALUES(sqlc.arg(command_id),sqlc.arg(space_id),'tenant',sqlc.arg(tenant_id),sqlc.arg(idempotency_key),sqlc.arg(kind),sqlc.arg(actor),sqlc.arg(fingerprint),'pending')
RETURNING *;

-- name: InsertPlatformCommand :one
INSERT INTO image.commands(command_id,space_id,owner_scope,tenant_id,idempotency_key,kind,actor,fingerprint,state)
VALUES(sqlc.arg(command_id),sqlc.arg(space_id),'platform',NULL,sqlc.arg(idempotency_key),sqlc.arg(kind),sqlc.arg(actor),sqlc.arg(fingerprint),'pending')
RETURNING *;

-- name: UpdateTenantCommandPhase :one
UPDATE image.commands SET phase=sqlc.arg(phase),candidate=sqlc.arg(candidate),state=sqlc.arg(state),reason=sqlc.arg(reason),
 secret_ciphertext=sqlc.narg(secret_ciphertext),secret_key_id=sqlc.narg(secret_key_id),version=version+1,updated_at=clock_timestamp()
WHERE owner_scope='tenant' AND tenant_id=sqlc.arg(tenant_id) AND space_id=sqlc.arg(space_id) AND command_id=sqlc.arg(command_id)
 AND version=sqlc.arg(expected_version) AND state IN ('pending','running','retryable','blocked')
RETURNING *;

-- name: UpdatePlatformCommandPhase :one
UPDATE image.commands SET phase=sqlc.arg(phase),candidate=sqlc.arg(candidate),state=sqlc.arg(state),reason=sqlc.arg(reason),
 secret_ciphertext=sqlc.narg(secret_ciphertext),secret_key_id=sqlc.narg(secret_key_id),version=version+1,updated_at=clock_timestamp()
WHERE owner_scope='platform' AND tenant_id IS NULL AND space_id=sqlc.arg(space_id) AND command_id=sqlc.arg(command_id)
 AND version=sqlc.arg(expected_version) AND state IN ('pending','running','retryable','blocked')
RETURNING *;

-- name: CompleteTenantCommand :one
UPDATE image.commands SET state='succeeded',phase='completed',result=sqlc.arg(result),reason='',
 secret_ciphertext=CASE WHEN sqlc.narg(secret_replay_until)::timestamptz IS NULL THEN NULL ELSE secret_ciphertext END,
 secret_key_id=CASE WHEN sqlc.narg(secret_replay_until)::timestamptz IS NULL THEN NULL ELSE secret_key_id END,
 secret_replay_until=sqlc.narg(secret_replay_until),completed_at=clock_timestamp(),updated_at=clock_timestamp(),version=version+1
WHERE owner_scope='tenant' AND tenant_id=sqlc.arg(tenant_id) AND space_id=sqlc.arg(space_id) AND command_id=sqlc.arg(command_id)
 AND version=sqlc.arg(expected_version) AND state IN ('pending','running','retryable','blocked')
RETURNING *;

-- name: CompletePlatformCommand :one
UPDATE image.commands SET state='succeeded',phase='completed',result=sqlc.arg(result),reason='',
 secret_ciphertext=CASE WHEN sqlc.narg(secret_replay_until)::timestamptz IS NULL THEN NULL ELSE secret_ciphertext END,
 secret_key_id=CASE WHEN sqlc.narg(secret_replay_until)::timestamptz IS NULL THEN NULL ELSE secret_key_id END,
 secret_replay_until=sqlc.narg(secret_replay_until),completed_at=clock_timestamp(),updated_at=clock_timestamp(),version=version+1
WHERE owner_scope='platform' AND tenant_id IS NULL AND space_id=sqlc.arg(space_id) AND command_id=sqlc.arg(command_id)
 AND version=sqlc.arg(expected_version) AND state IN ('pending','running','retryable','blocked')
RETURNING *;

-- name: FailTenantCommand :one
UPDATE image.commands SET state='failed',reason=sqlc.arg(reason),secret_ciphertext=NULL,secret_key_id=NULL,secret_replay_until=NULL,
 completed_at=clock_timestamp(),updated_at=clock_timestamp(),version=version+1
WHERE owner_scope='tenant' AND tenant_id=sqlc.arg(tenant_id) AND space_id=sqlc.arg(space_id) AND command_id=sqlc.arg(command_id)
 AND version=sqlc.arg(expected_version) AND state IN ('pending','running','retryable','blocked')
RETURNING *;

-- name: FailPlatformCommand :one
UPDATE image.commands SET state='failed',reason=sqlc.arg(reason),secret_ciphertext=NULL,secret_key_id=NULL,secret_replay_until=NULL,
 completed_at=clock_timestamp(),updated_at=clock_timestamp(),version=version+1
WHERE owner_scope='platform' AND tenant_id IS NULL AND space_id=sqlc.arg(space_id) AND command_id=sqlc.arg(command_id)
 AND version=sqlc.arg(expected_version) AND state IN ('pending','running','retryable','blocked')
RETURNING *;

-- name: GetOpenTenantExternalCommand :one
SELECT * FROM image.commands WHERE owner_scope='tenant' AND tenant_id=sqlc.arg(tenant_id) AND space_id=sqlc.arg(space_id)
 AND kind IN ('enable_space','issue_publisher','reset_publisher','disable_publisher')
 AND state IN ('pending','running','retryable','blocked');

-- name: GetOpenPlatformExternalCommand :one
SELECT * FROM image.commands WHERE owner_scope='platform' AND tenant_id IS NULL AND space_id=sqlc.arg(space_id)
 AND kind IN ('enable_space','issue_publisher','reset_publisher','disable_publisher')
 AND state IN ('pending','running','retryable','blocked');

-- name: ScrubExpiredDeliverySecrets :execrows
UPDATE image.commands SET secret_ciphertext=NULL,secret_key_id=NULL,updated_at=clock_timestamp(),version=version+1
WHERE state='succeeded' AND secret_replay_until IS NOT NULL AND secret_replay_until<=sqlc.arg(now_at)
 AND secret_ciphertext IS NOT NULL;

-- name: ScrubExpiredSpaceDeliverySecrets :execrows
UPDATE image.commands SET secret_ciphertext=NULL,secret_key_id=NULL,updated_at=clock_timestamp(),version=version+1
WHERE owner_scope='platform' AND tenant_id IS NULL AND space_id=sqlc.arg(space_id)
 AND kind IN ('issue_publisher','reset_publisher')
 AND state='succeeded' AND secret_ciphertext IS NOT NULL
 AND secret_replay_until IS NOT NULL AND secret_replay_until<=sqlc.arg(now_at);
