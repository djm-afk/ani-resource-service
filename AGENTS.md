# Service repository guidance

This service owns its generated runtime source. It has no runtime dependency on
the layout that created it.

Read `docs/START-HERE.md` for the current document index, then `CONTEXT.md` and
the applicable spec/ADRs before changing business behavior. Current execution
state lives only in `docs/execution/status.md`. A design or plan is not evidence
that its behavior has been implemented or verified; follow the user's actual
task scope and authorization.

For design, implementation, fixes, configuration, tests, and PR review in every
existing and future Res domain, read [Res global management responsibilities and simplification principles](docs/specs/res-responsibility-and-simplification.md).
These principles govern responsibility boundaries, including implementations
awaiting migration. Domain contracts define their objects, authoritative facts,
states, and completion conditions within those boundaries; conflicting older
documents, tests, or implementations do not grant exemptions. Apply the rules
by object ownership and action, without imposing VM/Network-specific fields,
resource-type bans, or uniform execution frameworks on other domains. Align
conflicts within the actual task scope. Historical plans, evidence, and
future-phase designs do not authorize new implementation, deployment, or
cross-repository changes.

## Boundaries

- `cmd/ani-resource-service` is the explicit composition root.
- `internal/service` adapts inbound transport contracts to use cases.
- `internal/biz` owns domain concepts, behavior, use cases, and required ports;
  it must not import Kratos, protobuf, transports, or storage drivers.
- `internal/data` implements outbound ports and owns infrastructure-specific
  code. Add an adapter only when a vertical slice needs it.
- `internal/server` owns transport and cross-cutting runtime wiring, not
  business rules.

Prefer framework capabilities over local replacements. Do not introduce a
shared ANI runtime module, generalized YAML exception registry, or technology
switches unrelated to a real slice. Generate protobuf output through the pinned
workflow. Implementation changes require the applicable checks and `make verify`
before committing; documentation-only changes require link and format checks.

## Execution location

- Read the personal environment inventory before environment access. Confirm the
  execution host and connection entry from the current task; do not infer SSH
  configuration, usernames, clusters, or fallback permission from old records.
- Local source inspection, editing, and lightweight document checks do not need
  a build or deployment environment. Do not run business tests for a document-only task.
- For implementation verification, use the environment confirmed for that task
  and record its actual host, source snapshot, commands, and results.
- Follow `docs/remote-execution.md` for source isolation and artifact handling.
  Its R0–R5, NET-05A, and SNAT host restrictions belong to those historical tasks.
  Never overwrite another working tree or include credentials in a snapshot.

## Documentation

- Root `README.md` is the project/quick-start entry, `AGENTS.md` contains brief
  engineering rules, and `CONTEXT.md` contains domain vocabulary only.
- `docs/START-HERE.md` is the single documentation index. Keep formal specs in
  `docs/specs/`, decisions and rationale in `docs/adr/`, implementation order in
  `docs/plans/`, and current progress plus evidence in `docs/execution/`.
- Each rule has one authoritative home; link to it instead of copying contract,
  state-machine, schema, or progress tables into multiple files. Do not place a
  second current spec or formal execution ledger under `.scratch/`.
- Use canonical domain names in contracts, implementation, tests, and docs.
  Keep provider terminology in infrastructure adapters and their contracts.
- Mark superseded documents with a replacement link. Record source snapshots,
  actual commands, and `pass` / `fail` / `not_verified` separately from plans.
- Keep local document links relative and verify targets/anchors after changes.
  Cross-repository source links are point-in-time evidence, not build/runtime
  dependencies or authorization to change those repositories.
