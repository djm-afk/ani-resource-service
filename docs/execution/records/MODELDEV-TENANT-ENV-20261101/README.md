# Resource implementation: dynamic tenant environment prerequisites

Base: `23e25b33ca02890231cd273ef7246b0fc415476c` (main read through
`ssh://git@ssh.github.com:443/zhangzhe-ctrl/ani-resource-service.git`).
Implementation branch: `codex/modeldev-dynamic-tenant-environment`.

The Resource component accepts only an existing standard tenant Namespace and
its original UID. Network acceptance, provider operations and VM NAD reads
reject missing/changed/NULL identity; Sandbox no longer initializes Namespace
or bypasses the guard. Storage validates Namespace identity on replay and
checks the live UID before writes. VM/Instance selection uses the fixed name.
ModelDev pull Secret authorization is dynamic under tenant/cluster/name/UID
checks; Inference retains its static branch and no SA writer was added.

Migration 0025 and `scripts/tenant-namespace-backfill` prepare the task 09
maintenance window. The command defaults to a read-only plan, requires a reviewed
SHA256 for apply, conditionally updates only exact existing NULL rows, preserves
existing UIDs and unrelated clusters, and reports before/after identities/counts.
No production migration, backfill, deployment or cluster write was run here.

| Check | Result | Actual evidence |
|---|---|---|
| NS01 canonical/public positive-negative vector | PASS (component) | Pure helper test; vector SHA256 `1e7eb5561043b5fe172e8bcf18a173cd3b2deb90804ded802a55ed06d1bc882a`; [verify](verify.log) |
| NS02 existing name/owner/phase/UID and no Namespace Create/Patch | PASS (component) | Network/Sandbox/VM/Instance fake Kubernetes actions and ModelDev Secret writer boundary tests; [verify](verify.log) |
| Storage replay still validates identity | PASS (component) | `TestCreateVolumeReplayStillValidatesNamespaceIdentity`; [verify](verify.log) |
| RS01 concurrent different UID admission conflict | PASS (real isolated PostgreSQL) | `TestTenantNamespaceConcurrentAdmissionRejectsUIDConflict`; [PG](namespace-pg.log) |
| RS01 NULL runtime refusal, exact NULL backfill, existing UID and unrelated cluster preservation | PASS (real isolated PostgreSQL) | `TestTenantNamespaceBackfillIsExactAndNeverOverwritesUID`; [PG](namespace-pg.log) |
| IM01 dynamic ModelDev Secret target / cross tenant-cluster-UID-registry rejection; Inference original caller regression | PASS (component) | Real use case and adapter tests; [verify](verify.log) |
| Pinned config/protobuf/sqlc generation | PASS | Buf 1.60.0, sqlc 1.31.1; [generation](generation.log), deterministic generation in [verify](verify.log) |
| Repository required `make verify` | PASS, exit 0 | Fedora, `GOTOOLCHAIN=auto GOMAXPROCS=2 GOFLAGS=-p=2 make verify`; generation, boundaries, fault builds, tidy diff, default tests, vet/build, module verification and diff checks in [verify](verify.log) |
| Runtime source matches verified Fedora source | PASS, exit 0 | 880 input files in [manifest](source-files.sha256), manifest SHA256 `f26ffcafb6a4d5c11acb152d8e2b0044d26441e9d4fb5ff3a9c3135b9a3a3239` |
| Live UID backfill/zero NULL, deployment and A/B business acceptance | NOT_RUN | Owned by task 09/10; this record makes no live claim |

All generation, builds, tests and isolated PostgreSQL ran only on `ssh fedora`
in `/home/chabking/workspace/ani-resource-service-runs/modeldev-tenant-env-20261101/source`.
Source transferred with rsync, with no Fedora packaging or archive transfer.
The PostgreSQL driver used pinned image
`docker.io/library/postgres@sha256:4ef4dbc939d61acea57712655ddb4b4ab27419c913f94cca0cd57cb3ea3c2280`
and removed its task-owned container. Default `make verify` skips opt-in PG/live
cases; only the two named PG cases above were separately selected. No full
integration or live suite was run.

The first mandatory verification identified two config-field references and a
real historical read regression: Instance metric scope had used the create
namespace resolver merely for DNS syntax validation. Tightening creation would
reject unchanged persisted historical scopes. The minimum correction separates
read-only name syntax validation from new target authorization, preserving
original tenant-scoped Instance/VM metric evidence as required by spec 2.2/12.3.
The final required verification passed after that correction. Startup rejects
old nonempty namespace-prefix configuration; ModelDev caller's old namespace/
owner-label configuration must also be removed at task 09 switch. Existing
Inference configuration is retained.


## Temporary UID write barrier (2026-11-01)

Base for this increment: `71b3ad7a2a88ae531e5e7b80abde2f494ff28d9d`.
The [one-time owner tool](../../../runbooks/tenant-namespace-uid-window.md)
defaults to read-only exact plans. It freezes target-cluster identity INSERT/UPDATE
on schema24, preserves that guard through official migration0025, atomically
requires non-NULL UIDs for backfill, and removes only its own temporary constraint
after the exact two UIDs and original writer retirement are proven. It changes no
ACL, migration, RPC or normal runtime composition. Backfill CLI apply now requires
the applied require-UID window plan and checks DB/runtime/row identity under lock.

Real isolated PostgreSQL selected exactly
`^(TestNamespaceUIDWindow|TestTenantNamespaceBackfillIsExactAndNeverOverwritesUID)`
with `scripts/integration -race -timeout=3m -v -run <pattern> ./internal/data/network`:
**PASS, exit0**, 2026-10-31T22:37:43.474373Z–22:38:06.715224Z.
[Full PG output](uid-window-pg-final.log) and [receipt](uid-window-pg-final-receipt.json)
prove existing-row ON CONFLICT rejection (23514), target UPDATE rejection,
read/original Delete admission and unrelated-cluster preservation, official25
migration, transaction-race waiting and changed-row refusal, replacement ADD
failure rolling back DROP, exact two-row fill, unknown writer source refusal,
original-address late connections and captured backend retirement, cancellation,
wrong digest and NULL release refusal. External Namespace verification is a
controlled boundary in these tests; this is not live cluster acceptance.

Required `make verify`: **PASS, exit0**, 22:38:06.717633Z–22:41:40.721999Z
on the same nine-file source hashes; [full output](uid-window-verify-final.log),
[terminal receipt](uid-window-verify-final-receipt.json). This includes deterministic
pinned generation, ownership boundaries, Network fault builds, tidy diff, default
package tests (including embedded Python syntax), vet/build/module verification
and diff checks. The SSH observation handle later ended255; a single bounded
read retrieved the already-completed authoritative receipt, without restarting
the check. Existing module-cache stat-write warnings did not fail the check.
Only these execution records/status were appended after tests.

An earlier PG attempt failed because the new `has_table_privilege` overload
inferred table OID as text; explicit `$3::oid` corrected it. Its actual failure
and earlier PG outputs remain in coordinated task09 `res-uid-window-pg-oid-fail*`
evidence, rather than being reported as final PASS.

**Live freeze/migration/guard swap/backfill/zero-NULL/deployment/release: NOT_RUN.**
The two historical live Namespace UIDs are available, but current Deployment,
Pod and runtime-role identity must be freshly read before the template can become
an apply-ready manifest. No live Plan.SHA256 exists yet. The coordinated task09
manifest template file hash is only a template hash, not an executable plan digest.
