# Cluster identity PR #78 review fixes — 2026-11-13

Scope: review comment `issuecomment-6842057102`. Development and verification ran in the task's local WSL worktree. No 47 resource or deployment was changed.

- Image's empty `pull_secret_kubeconfig` again selects its own in-cluster ServiceAccount. Network's resolved connection is reused only when both configured paths are identical. A distinct Image connection reads its own kube-system UID and keeps its own credentials. Regression covers same UID/different bearer credentials and a different UID.
- `configs/config.yaml` defaults `network.cluster_id` to empty. A test executes the real configuration-loading command entry: omitted UID reaches discovery; an explicitly wrong UID fails before database composition. The raw configuration validation fixture supplies its own explicit test UID.
- The source `ClaimVirtualSwitch` query selects telemetry only for an unfinished operation. Removed its unused operation_id subquery, whose null result could otherwise fail pgx scanning when no unfinished operation exists. Regenerated with pinned sqlc v1.31.1.

Validation: `make verify TOOLS_DIR=/home/ubuntu/workspace/ani-resource-service/.tools/bin` passed (generation/source boundaries, default tests, vet, build and module verification). `scripts/integration -run TestVirtualSwitchClaimExcludesCompletedCreateTelemetry -count=1 ./internal/data/network` passed against a fresh task-owned PostgreSQL container: completed CREATE → first VLAN binding claims work without the old trace; an unfinished operation still propagates its trace. The integration runner removed its container on exit.

These are local/isolated regression results, not 47 live acceptance. PR #78 retains the earlier deployment and A01–A10 evidence; this revision requires a later release to 47.


CI follow-up: the process-start fixture now serves a canonical kube-system UID while unrelated provider calls remain unavailable. Harbor transport deadline errors retain the deadline reason even when the parent context has not yet observed cancellation. The two asynchronous gRPC logging fixtures use synchronized buffers. Ten repeated race runs of the affected Harbor/ClusterOps/Image runtime tests passed; the process signal/listener test passed with a fresh task-owned PostgreSQL instance. The build/toolchain is Go 1.26.11. Existing first-party provenance records now match the actual published module pins, including VM console; licenses remain UNDECLARED and all other dependencies retain SPDX checks.
