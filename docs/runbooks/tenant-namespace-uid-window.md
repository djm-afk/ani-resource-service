# One-time Network Namespace UID window

This is task 09's temporary database barrier, not a runtime mode. It changes no
ACL, migration checksum, API, Namespace, VM or Instance record. The final schema
is exactly migration 0025 with the temporary constraint removed. Cluster writes
are authorized only after the fixed five identities from the implementation
specification are freshly verified through `kubercloud@100.64.128.12`.

The reviewed manifest contains exactly the two existing standard Namespace
names/original UIDs, a fresh window UUID, database and runtime role, and the
original Resource Deployment UID/container/Pod name+UID+IP set. The CLI checks
those live Namespaces and cluster identity through that unique SSH entry. Owner
and runtime DSN files remain private, mode 0600. The maintenance owner must
already own/can use the table owner and be able to inspect original backend
identities (`pg_read_all_stats` or existing administrator); the tool never grants
that privilege. A runtime role able to SET owner, DELETE, TRUNCATE or TRIGGER the
identity table is refused. The owner connection is distinct from the runtime. Freeze refuses any runtime
connection outside the exact original Pod IP set (including local sockets or
unproven NAT/poolers); it never invents an address mapping.

All commands default to read-only plans. Save their complete JSON, including
`Plan.SHA256`; apply requires that exact reviewed digest and repeats live checks
under a bounded table lock. The row set, database/table OIDs, owner, checksums,
UID column, owned constraint expression/comment and original backend identities
are re-read. A changed row set or identity is refused, never adopted.

1. On schema 24, `-stage freeze` installs only
   `CHECK (cluster_id <> '<exact cluster>') NOT VALID`, with this window's comment.
   Its ACCESS EXCLUSIVE lock waits in-flight identity transactions (5-second
   lock timeout, 15-second statement timeout). The new constraint rejects target
   INSERT and UPDATE, including INSERT of an existing row with ON CONFLICT DO
   NOTHING. Existing rows remain readable; other tables and clusters continue.
   A failed initial freeze leaves schema 24 and installs nothing: do not migrate.
2. Use the existing owner-only `ani-resource-service -migrate` entry, with the
   existing owner DSN and runtime role. It adds nullable 0025 and leaves the
   committed freeze in place. Runtime composition never migrates.
3. `-stage require-uid -freeze-plan-file <original freeze JSON>` atomically drops
   that exact freeze and replaces it with
   `CHECK (cluster_id <> '<exact cluster>' OR namespace_uid IS NOT NULL) NOT VALID`.
   DROP, ADD and COMMENT are one transaction. Existing NULL rows can be filled;
   old INSERT omitting UID is rejected before ON CONFLICT even for an existing row.
4. Run the existing `scripts/tenant-namespace-backfill` plan against the runtime
   DSN and approved host kubeconfig. Its apply now also requires
   `-window-plan-file <applied require-uid JSON>`. Under a SHARE ROW EXCLUSIVE lock
   it verifies the owned guard, original database/runtime identity and exact two
   rows again, then fills only NULL UIDs using its existing live UID checks. It
   never overwrites a UID. Record the exact before/after rows and zero target NULL.
5. Update Resource only through the specified Workbench. Freeze the actual build
   source→image digest mapping. Do not remove the guard while any old writer
   remains. The release manifest adds that new image reference, immutable digest
   and source commit. `-stage release -freeze-plan-file <original freeze JSON>`
   requires schema25, the owned require-UID guard, the exact two non-NULL original
   UIDs, all original Pod UIDs gone, current Deployment observed/available with
   every matching Resource container on the reviewed imageID and ready, and all
   original `(pid, backend_start)` connections gone, and no runtime connection from
   any original Pod IP (including connections opened after freeze). Reused Pod
   IPs therefore fail closed while a connection is active. Only then can it drop the
   temporary constraint. Other-cluster NULL rows are outside this window.

Example calls (fill current writer identities first; placeholders are not valid
apply input). Build only on Fedora; transfer individual tools/source with rsync,
not a Fedora archive or image package. Run operational commands through the
approved entry, or with DB access tunneled only through that entry:

```sh
go run ./scripts/tenant-namespace-window \
  -manifest-file /private/res-window.json -dsn-file /private/owner-dsn \
  -stage freeze > /private/res-freeze-plan.json
# Review Plan.SHA256, then add -apply -reviewed-sha256 <that exact digest>.
# Save the applied JSON separately; retain the original freeze plan for all stages.

go run ./scripts/tenant-namespace-window \
  -manifest-file /private/res-window.json -dsn-file /private/owner-dsn \
  -stage require-uid -freeze-plan-file /private/res-freeze-plan.json

go run ./scripts/tenant-namespace-backfill \
  -dsn-file /private/runtime-dsn -cluster-id <exact cluster> \
  -kubeconfig /private/approved-kubeconfig
# Apply additionally needs both -reviewed-sha256 <backfill-plan digest> and
# -window-plan-file /private/applied-require-uid.json.

go run ./scripts/tenant-namespace-window \
  -manifest-file /private/res-window-with-new-image.json -dsn-file /private/owner-dsn \
  -stage release -freeze-plan-file /private/res-freeze-plan.json
```

`-stage inspect` is always read-only and reports current schema, rows and
constraint after interruption. It cannot repair or remove an unknown guard.
Errors before commit preserve the last committed guard; a lost commit response
is uncertain and requires inspection, never automatic removal/retry. Missing SSH,
identity drift, nonstandard/missing Namespace, extra/changed target row, schema
or checksum drift, foreign/validated constraint, unreviewed SHA, table-lock
contention, nonzero target NULL, active original writer or unproven new image
stops the dependent step. A successful component test or plan is not a live
migration, zero-NULL result, deployment or A/B acceptance.
