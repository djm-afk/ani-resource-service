# Network 日志整改一期 — 执行与验收记录

基线：`origin/main = 71c2e8eb`（2026-11-12，PR#76 image-only）。PR#76 未改动 `internal/biz/network`、`internal/data/network`、`internal/service/network`、`internal/telemetry`、`internal/data/instance`、`migrations/` 与 `internal/server/{observability,worker,rpc_observability}.go`（`git diff --name-only 735e40c4 71c2e8eb --` 上述路径 = 0 个文件），故 `RECONCILIATION.md` 对 `735e40c4` 的行号与结论仍然有效。

分支：`feat/network-logging-phase1`（worktree `.tmp/wt/res-net-log`）。编译/测试在 `192.168.18.35`（Go 1.26.10）执行，独占 PostgreSQL 用仓库 pin 镜像。

**本记录区分三种完成状态**：代码完成（编译 + 单测/集成）／部署完成（未做）／实测完成（未做）。

## 1 关闭的 §2 未修偏差

| # | 位置 | 处理 |
|---|---|---|
| 1 | `server/observability.go::ObserveWork` | 删除 `resource_state`/`attempt_result`；reconciliation 改 `step.succeeded`/`step.waiting`/`operation.succeeded`/`operation.failed`，仅保留合同字段 |
| 2 | `server/observability.go::ObserveVirtualSwitchWork` | 删除 `provider_failure/node_name/node_uid/binding_state/observed/attempt_result`；节点身份改 `target{kind:"Node",...}`，状态用 `reason`/`step` |
| 3 | 同上 `step.failed` | 补必填 `step`（按固定 step 表） |
| 4 | `telemetry/logfields/error.go::ErrorFields` | 新增 `http.status_code`（`k8sErr.ErrStatus.Code`）；Reason 仍 `error_code`，message 仍 `error` |
| 6 | `data/instance/network.go::InstanceNetworkConsumer` | `controller_managed` 在读失败处、转 `ErrNetworkUnavailable` **之前**记 `dependency.failed`（`error_source=local`），`SetLogger` 经 `biz.SetConsumerLogger` 注入；`vm_managed` 同规则落在 `attachment_vm.go::vmConsumerRouter`；两分支不产生 `rpc.client.completed` |
| 7 | `server/observability.go::logAttachmentFailure` | 补 `ErrLeaseLost` → `operation.conflict` WARN（无 operation，不伪造 id）；DB 写失败仍 `state.persist_failed` ERROR；两路互斥 |
| 8 | `data/network/kc_observation.go` watch | 真实依赖故障升 ERROR `dependency.failed`（`error_source=kubernetes`，保留 GVR 原错），不自造事件名 |
| 9 | `data/network/kc_observation.go::processNextNotification` | 每 key `StartRun` 独立 span；失败 `dependency.failed` ERROR（重试前）+ `worker.retry_scheduled` WARN |
| 10 | 四处 `ErrLeaseLost` 静默 | `virtual_switch_worker.go`、`security_groups.go`、`default_vpc_egress_worker.go` 记 `operation.lease_lost` WARN；附件记 `operation.conflict` WARN；返回值不变 |
| 11 | `logfields::safeErrorMessage` | `error` 改为解包链（外层动作 + 最内层 cause），`ProviderError`/`biz.Error` 的 driver message 不再丢；类型判定不变 |
| 12 | 三条 lane 无条件 `Resume` | 新增 `biz/network/run.go::activeRun`：仅 Claim 产物标记“操作未完成”时 `Resume`，否则 `StartRun` 新 trace；终态缺元数据不报 `context_missing` |

## 2 §3 未实现项的落地

- 受理三态：`data/network/admission_log.go`（`Postgres.SetLogger` + `logAcceptance`/`logResourceMutation`）。新受理→`operation.accepted`、幂等命中→`operation.replayed`，均在**事务提交后**记录；`operation.rejected` 唯一落点按分册在 `server/rpc_observability.go::stats.End`（只对写方法、WARN 类 code、`Domain=="network.ani.io"` 记 WARN）。覆盖 VPC/Subnet/VPC删除/Subnet删除/EIP/SNAT/Platform/LB/VNic/VirtualSwitch/SG/DefaultEgress 的受理与删除，含 base-connectivity 子操作不单独记 accepted 的豁免。
- 第五张表：新增 `migrations/0036_logging_telemetry_platform.sql`（`network_platform_operations.telemetry_context jsonb`，可空、无回填）；`queries/platform.sql::InsertPlatformOperation` 增列、`GetPlatformOperation` 解码；`claimPlatform` 读回装入 `Work`；`claimPlatformNetwork` 无 operation，自建 run trace。
- 五 lane 事件：`worker.run_started/run_completed/run_failed`、`operation.started/succeeded/failed/retry_scheduled/conflict/lease_lost`、`step.started/succeeded/waiting/failed`、`state.persist_failed`、`dependency.failed`、`resource.mutated/replayed` 按分册 §3.2–§3.7 落点。共用 helper 在 `biz/network/events.go`；`server/worker.go::runLane` 补 `worker.run_failed` 并保留 panic 真值。
- 观察链：只含 watch + notify（§3.7），未恢复退役循环。

## 3 门禁结果

`192.168.18.35`，源码同步自本 worktree（`.git` 不随同步）。

| 项 | 结果 |
|---|---|
| `make check-buf` / `check-sqlc` | PASS（buf 1.60.0 / sqlc v1.31.1，按 pin 现场安装） |
| `scripts/verify-source` | PASS（含 `sqlc generate` 幂等、gofmt、生成物一致） |
| `scripts/verify-boundaries` | PASS |
| `scripts/net05a-build-faults.py --network-only` | PASS（`{"fault_build":"...","status":"pass"}`） |
| `go test ./...`（全仓单测） | 仅 `tests/runtime::TestRuntimeLifecycle` FAIL；**该失败在干净 `origin/main` 上同样复现**（已用 `git archive origin/main` 独立树验证），属既有基线 |
| `go vet ./...` | PASS |
| `go build -trimpath ./...` | PASS |
| `go mod verify` | PASS |
| `git diff --check` | PASS |

**基线缺陷（顺带发现，未在本任务扩大范围）**：`internal/data/network/queries/virtual_switch.sql::ClaimVirtualSwitch` 与已提交的 `sqlcgen/virtual_switch.sql.go` 不一致——生成文件含 `LEFT JOIN ... AND o.completed_at IS NULL`（活动操作判定），源 `.sql` 却是子查询版本。`make verify` 的 `verify-source` 会跑 `sqlc generate` 并因此报「generation changed the repository tree」。本任务把源 `.sql` 对齐到生成文件的既有语义（行为不变），使生成稳定；未改语义、未改索引。

## 4 NG29 / NG30

- **NG29** `internal/biz/network/trace_neutrality_test.go`：改变 trace 前后 `VPCIntent.Fingerprint`/`SubnetIntent.Fingerprint`/`PlatformIntent.VersionedFingerprint` 完全一致，资源 JSON 不含任何 telemetry 字段，`Version` 不变。PASS。
- **NG18** `internal/biz/network/run_test.go`：活动操作 resume 同 trace；终态 `StartRun` 新 trace 且不报 `context_missing`；坏元数据活动轮报一次 `context_invalid`。`TestTerminalDefaultEgressStartsNewTrace` 用真实 `DefaultVPCEgressWorker.Step` 证明终态不继承创建 trace。PASS。
- **NG30** `internal/data/network/telemetry_roundtrip_integration_test.go`（独占 PG）：五张 operation 表均有可空 `telemetry_context`；`network_operations` 与三个 lane 表写读一致、坏值隔离、legacy 读作 missing；新增 `TestPlatformOperationTelemetryRoundTrip` 证明平台受理写入与 `GetPlatformOperation` 读回同 trace、坏值不影响业务；`TestAllFiveOperationTablesCarryTelemetry` 断言五表齐全。PASS。
- `logfields` 新增 `TestErrorFieldsKeepWrappedCause`（解包 + `http.status_code=403`）与 `TestNetworkEventEmitsContractFields`（空串丢弃、target 成组对象、解包 cause）。PASS。
- **合同校验器实测**：`TestEmitContractSample` 用真实 helper 产出 20 条 JSONL（覆盖全部 Network 事件），`tools/verify_logs.py` 结果 `{"status":"PASS","records":20,"issue_count":0}`。这一步只证明事件词汇、字段名、级别、`result` 固定值合合同；它不替代真机故障注入（NG27 的真机部分仍 NOT_RUN）。
- `TestNetworkEventHasNoDuplicateKeys`：断言同一记录内无重复 JSON 键（helper 自带字段与 lane 身份重叠时首见为准），并覆盖 `operation.failed` 必须带 `result=failed`。

## 5 `internal/data/network` 集成套件（真实 PG）

用仓库 pin 镜像（`docker.io/library/postgres@sha256:4ef4dbc9…`，实际 18.6）在独占实例上运行。全量 `go test ./internal/data/network/` 在 25m 超时（`TestSubnetProcessesRecoverProviderSuccessAndUnknownDeletion` 挂起），报 40 条失败。**这 40 条是超时前累积的失败，不是全部断言失败**。

抽样复现（我的树 vs 干净 `origin/main` 独立树，同一 PG 实例、同一命令）：

| 用例 | 本树 | origin/main |
|---|---|---|
| `TestCurrentDefaultVPCEgress` | FAIL `DEPENDENCY_UNAVAILABLE: Network database unavailable` | 同 |
| `TestVirtualSwitchDurableTwoStepAcceptanceAndRecovery` | FAIL `VLAN created before group configuration` | 同 |
| `TestVirtualSwitchDeleteWaitsForOldClaimLeaseExpiry` | FAIL `condition did not converge` | 同 |
| `TestPlatformEIPInventoryAndOwnerRelease` | FAIL `unknown fixture tenant` | 同 |
| `TestDefaultVPCEgressKCRealBusinessLifecycle` | FAIL | 同 |
| 宽集（VPC/Subnet/LB/Attachment/SecurityGroup/Egress，baseline） | — | 同样 FAIL 且 20m 超时 |

结论：**这些集成失败是既有环境/基线问题，不是本任务引入**；但按 `ACCEPTANCE.md §4` 口径，它们**不是 PASS**，记为 FAIL（既有）。本任务新增/受影响且在同一实例上能独立跑的用例（NG30 四条、§4 中新增单测、`go test ./...` 中除 `tests/runtime` 外的全部）均为 PASS。

## 6 未执行（NOT_RUN）

以下真机项需要已部署的 Res 进程与授权测试集群，本任务未部署、未在 10.10.1.66 或任何集群运行，故**全部 NOT_RUN**，不以单测或本地推演冒充：NG01–NG05、NG07–NG17、NG19–NG28 中除已单测覆盖分支外的真机观测；特别是 NG10/NG11（远端 consumer 可达性）、NG15/NG16（同 trace 端到端与重启）、NG17（真 RBAC 403 经 ProviderError）、NG20/NG21（真 watch 与通知故障）、NG27（导出真实 JSONL 跑 `tools/verify_logs.py` 并比对 K8s 调用次数）。

## 7 交接

- 迁移：`0036` 只加可空列，不 backfill、不改历史迁移 checksum；部署需先在 owner 角色下执行 `-migrate`（运行时不自动迁移）。
- 平台受理 trace 只对 `AcceptPlatform`/`acceptPoolChange`/`DeletePlatform` 及 `managed_pool_gateway`/`switch_vlan` 的内部 operation 写入；`claimPlatform` 读回后 `ActiveOperation` 仍按 `completed_at` 判定，不影响状态机。
- `ClaimVirtualSwitch`/`DueDefaultEgress`/`SecurityGroupWork` 新增只读投影列（`operation_kind`/`completed_at`/`ActiveOperation`），仅用于日志侧判定「活动操作」，不进入业务判定。
- 未改动 Proto/状态机/租约/重试/幂等/授权；未新增 K8s 查询；未恢复退役观察循环与 node-facts；未给 `network_attachments` 加列。
