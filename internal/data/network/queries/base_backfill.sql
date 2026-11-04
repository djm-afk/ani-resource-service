-- Administrative cross-tenant inventory, bounded to the explicit cluster. Every
-- candidate retains its tenant and subsequent mutations use that tenant scope.
-- name: ListBaseBackfillInventory :many
SELECT v.tenant_id,v.vpc_id,v.version,v.state,
 b.binding_id,b.namespace,coalesce(b.provider_name,'')::text AS provider_name,
 coalesce(b.provider_uid,'')::text AS provider_uid,coalesce(b.pending_action,'')::text AS pending_action,
 coalesce(o.state,'')::text AS operation_state,
 (bc.vpc_id IS NOT NULL)::boolean AS has_base
FROM network_vpcs v
JOIN network_tenant_namespaces ns ON ns.tenant_id=v.tenant_id AND ns.cluster_id=sqlc.arg(cluster_id)
LEFT JOIN network_provider_bindings b ON b.tenant_id=v.tenant_id AND b.vpc_id=v.vpc_id
LEFT JOIN network_operations o ON o.tenant_id=v.tenant_id AND o.operation_id=v.last_operation_id
LEFT JOIN network_vpc_base_connectivity bc ON bc.tenant_id=v.tenant_id AND bc.vpc_id=v.vpc_id
WHERE b.cluster_id=sqlc.arg(cluster_id) OR b.binding_id IS NULL
ORDER BY v.tenant_id,v.vpc_id LIMIT sqlc.arg(max_results)::integer;

-- name: GetBaseBackfillRun :one
SELECT * FROM network_base_backfill_runs WHERE run_id=$1 AND cluster_id=$2;

-- name: LockBaseBackfillRun :one
SELECT * FROM network_base_backfill_runs WHERE run_id=$1 AND cluster_id=$2 FOR UPDATE;

-- name: LockBaseBackfillPlanKey :exec
SELECT pg_advisory_xact_lock(hashtextextended('base-backfill-plan:'||sqlc.arg(run_id)::text,0));

-- name: InsertBaseBackfillRun :exec
INSERT INTO network_base_backfill_runs(run_id,cluster_id,pool_id,pool_revision,interval_ms,plan_sha256)
VALUES($1,$2,$3,$4,$5,$6);

-- name: InsertBaseBackfillCandidate :exec
INSERT INTO network_base_backfill_candidates
(tenant_id,run_id,cluster_id,vpc_id,accepted_vpc_version,namespace,binding_id,provider_name,provider_uid,initial_state,initial_reason,state,reason)
VALUES(sqlc.arg(tenant_id),sqlc.arg(run_id),sqlc.arg(cluster_id),sqlc.arg(vpc_id),sqlc.arg(accepted_vpc_version),
 sqlc.narg(namespace),sqlc.narg(binding_id),sqlc.arg(provider_name),sqlc.arg(provider_uid),
 sqlc.arg(initial_state),sqlc.arg(initial_reason),sqlc.arg(initial_state),sqlc.arg(initial_reason));

-- Read-only evidence includes live execution facts without rewriting the reviewed
-- candidate snapshot. The run is platform metadata; candidate joins preserve tenant.
-- name: ListBaseBackfillCandidates :many
SELECT c.*,coalesce(o.state,'')::text AS operation_state,coalesce(b.state,'missing')::text AS base_state
FROM network_base_backfill_candidates c
LEFT JOIN network_operations o ON o.tenant_id=c.tenant_id AND o.operation_id=c.operation_id
LEFT JOIN network_vpc_base_connectivity b ON b.tenant_id=c.tenant_id AND b.vpc_id=c.vpc_id
WHERE c.run_id=$1 AND c.cluster_id=$2 ORDER BY c.tenant_id,c.vpc_id;

-- name: NextBaseBackfillCandidate :one
SELECT * FROM network_base_backfill_candidates
WHERE run_id=$1 AND cluster_id=$2 AND state='pending'
ORDER BY tenant_id,vpc_id LIMIT 1;

-- name: SetBaseBackfillPaused :execrows
UPDATE network_base_backfill_runs SET paused=sqlc.arg(paused),
 reviewed_at=CASE WHEN NOT sqlc.arg(paused)::boolean THEN clock_timestamp() ELSE reviewed_at END
WHERE run_id=sqlc.arg(run_id) AND cluster_id=sqlc.arg(cluster_id)
 AND (sqlc.arg(paused)::boolean OR plan_sha256=sqlc.arg(reviewed_sha256));

-- name: AdvanceBaseBackfillAdmission :execrows
UPDATE network_base_backfill_runs
SET next_admission_at=clock_timestamp()+interval_ms*interval '1 millisecond',
 paused=paused OR sqlc.arg(blocked)::boolean
WHERE run_id=sqlc.arg(run_id) AND cluster_id=sqlc.arg(cluster_id);

-- name: SetBaseBackfillCandidateResult :execrows
UPDATE network_base_backfill_candidates
SET state=sqlc.arg(state),reason=sqlc.arg(reason),operation_id=sqlc.narg(operation_id)
WHERE tenant_id=sqlc.arg(tenant_id) AND run_id=sqlc.arg(run_id) AND vpc_id=sqlc.arg(vpc_id) AND state='pending';

-- name: AdmitBaseBackfillVPC :execrows
UPDATE network_vpcs SET last_operation_id=sqlc.arg(operation_id),version=version+1,updated_at=clock_timestamp()
WHERE tenant_id=sqlc.arg(tenant_id) AND vpc_id=sqlc.arg(vpc_id) AND version=sqlc.arg(version)
 AND state NOT IN ('deleting','deleted') AND NOT base_connectivity_required;

-- name: ScheduleBaseBackfill :execrows
UPDATE network_reconciliations SET next_run_at=clock_timestamp(),lease_owner=NULL,
 lease_until=NULL,lease_epoch=lease_epoch+1,requested_generation=requested_generation+1
WHERE tenant_id=$1 AND vpc_id=$2;

-- Shared with first VPC admission, including the missing rollout-row case.
-- name: LockConnectivityRollout :exec
SELECT pg_advisory_xact_lock(hashtextextended('connectivity-rollout:'||sqlc.arg(cluster_id)::text,0));

-- name: EnsureConnectivityRollout :exec
INSERT INTO network_connectivity_rollout(cluster_id) VALUES($1) ON CONFLICT DO NOTHING;

-- name: SetNewVPCConnectivityEnabled :execrows
UPDATE network_connectivity_rollout SET new_vpcs_enabled=sqlc.arg(enabled),updated_at=clock_timestamp()
WHERE cluster_id=sqlc.arg(cluster_id) AND (sqlc.arg(enabled)::boolean OR NOT legacy_aggregation_enabled);

-- The exclusive rollout lock closes concurrent admission while all legacy rows
-- are held in deterministic order. No Provider call is made under these locks.
-- name: LockLegacyBaseAggregationVPCs :many
SELECT v.tenant_id,v.vpc_id,v.base_connectivity_required
FROM network_vpcs v
JOIN network_tenant_namespaces ns ON ns.tenant_id=v.tenant_id AND ns.cluster_id=sqlc.arg(cluster_id)
LEFT JOIN network_provider_bindings b ON b.tenant_id=v.tenant_id AND b.vpc_id=v.vpc_id
WHERE (b.cluster_id=sqlc.arg(cluster_id) OR b.binding_id IS NULL) AND v.state NOT IN ('deleting','deleted')
ORDER BY v.tenant_id,v.vpc_id FOR UPDATE OF v;

-- Activate only the Res rollout setting after its registered product intents
-- have been checked under the same management lock.
-- name: ActivateLegacyBaseAggregation :execrows
UPDATE network_connectivity_rollout SET legacy_aggregation_enabled=true,updated_at=clock_timestamp()
WHERE cluster_id=$1 AND new_vpcs_enabled;
