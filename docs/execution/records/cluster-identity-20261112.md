# Single target identity implementation (2026-11-12)

Source baseline: `71c2e8eb60fbfabbb7f374d538c8c6c3a5c18289` (main; retains
Image #76). Implementation/verification host: local WSL Ubuntu in the task-owned
`ani-resource-service-cluster-identity-20261112` worktree.

Full and governance now resolve kube-system UID before database access and
share the resolved connection across Kubernetes modules. Rendered owner IDs are
checked against that UID. ClusterOps optionally displays the configured target
name. Offline compositions and existing namespace protection remain independent
of live identity reads. See [runtime](../../runtime.md#single-target-identity).

Local regression covers valid/invalid UID, explicit mismatch, 403/unavailable,
exactly one identity read, both online composition roots rejecting drift before
other dependencies, private client config copies, and disabled Image. Existing
ClusterOps/owner tests remain in their domains. These are local fixtures, not
47 acceptance evidence.

Commands:

```bash
make config TOOLS_DIR=/home/ubuntu/workspace/ani-resource-service/.tools/bin
GOMAXPROCS=2 GOFLAGS=-p=2 make verify \
  TOOLS_DIR=/home/ubuntu/workspace/ani-resource-service/.tools/bin
```

The required pinned-generation check found an upstream sqlc output drift in
`virtual_switch.sql.go`; it has been regenerated from the existing SQL using
sqlc v1.31.1. The lifecycle fixture still expected old Kratos request logging,
which current production wiring replaced with native gRPC stats. Its assertion
now covers its installed recovery middleware; real RPC completion/span behavior
continues under `internal/server/rpc_observability_test.go`. The added
`TestNativeServerStatsHandlerRetainsSingleTraceAndCompletion` exercises the
production factory with its real OTel delegate and checks exactly one completed
span, one completion event, and matching trace/span identifiers. No duplicate logging
or tracing middleware was restored.

Complete local output: ignored `.work/cluster-identity-verify.log`. Integration
fixtures requiring a task-owned PostgreSQL instance are skipped when that
fixture environment is absent. Deployment and live A08/A09 results are recorded
in the Governance cross-service release record.

Final result: `make verify` PASS, including pinned generators, ownership boundaries,
build-fault checks, all default Go tests, vet/build/module verification and diff
checks. `go test -count=1 ./internal/server ./tests/runtime` also PASS.
