-- Local operator expired-candidate recovery. Every statement keeps explicit
-- scope/tenant/space predicates and a version CAS; there is no generic UPDATE.

-- name: GetTenantCandidateRecoveryCommand :one
SELECT * FROM image.commands
WHERE owner_scope='tenant' AND tenant_id=sqlc.arg(tenant_id) AND space_id=sqlc.arg(space_id)
 AND command_id=sqlc.arg(command_id);

-- name: GetPlatformCandidateRecoveryCommand :one
SELECT * FROM image.commands
WHERE owner_scope='platform' AND tenant_id IS NULL AND space_id=sqlc.arg(space_id)
 AND command_id=sqlc.arg(command_id);

-- name: FailTenantCandidateCommand :one
UPDATE image.commands SET state='failed',phase=sqlc.arg(phase),reason=sqlc.arg(reason),
 candidate=sqlc.arg(candidate),secret_ciphertext=NULL,secret_key_id=NULL,secret_replay_until=NULL,
 completed_at=clock_timestamp(),updated_at=clock_timestamp(),version=version+1
WHERE owner_scope='tenant' AND tenant_id=sqlc.arg(tenant_id) AND space_id=sqlc.arg(space_id)
 AND command_id=sqlc.arg(command_id) AND version=sqlc.arg(expected_version)
 AND state IN ('pending','running','retryable','blocked')
RETURNING *;

-- name: FailPlatformCandidateCommand :one
UPDATE image.commands SET state='failed',phase=sqlc.arg(phase),reason=sqlc.arg(reason),
 candidate=sqlc.arg(candidate),secret_ciphertext=NULL,secret_key_id=NULL,secret_replay_until=NULL,
 completed_at=clock_timestamp(),updated_at=clock_timestamp(),version=version+1
WHERE owner_scope='platform' AND tenant_id IS NULL AND space_id=sqlc.arg(space_id)
 AND command_id=sqlc.arg(command_id) AND version=sqlc.arg(expected_version)
 AND state IN ('pending','running','retryable','blocked')
RETURNING *;

-- name: BlockTenantRecoveredCredential :one
UPDATE image.credentials SET state='blocked',version=version+1,updated_at=clock_timestamp()
WHERE owner_scope='tenant' AND tenant_id=sqlc.arg(tenant_id) AND space_id=sqlc.arg(space_id)
 AND purpose=sqlc.arg(purpose) AND version=sqlc.arg(expected_version)
 AND generation=sqlc.arg(generation)
RETURNING *;

-- name: BlockPlatformRecoveredCredential :one
UPDATE image.credentials SET state='blocked',version=version+1,updated_at=clock_timestamp()
WHERE owner_scope='platform' AND tenant_id IS NULL AND space_id=sqlc.arg(space_id)
 AND purpose='publisher' AND version=sqlc.arg(expected_version) AND generation=sqlc.arg(generation)
RETURNING *;
