# Res Network 日志整改一期 — 实施进度

更新：2026-11-12。分支 `feat/network-logging-phase1`（基于 `origin/main = 71c2e8eb`）。
**当前状态：代码完成 + 离线门禁通过；未部署、未真机验收。**

## 一、一句话结论

执行包 `docs/00-实施/ANI-Res-Network-Logging-Main-v1.0/` 的 N00–N07 已全部落地（§2 十处偏差 + §3 业务事件 + 平台第五张表 + NG29/NG30），N08 的真机部分因未部署全部 NOT_RUN。产出 20 类事件经合同校验器 `verify_logs.py` 实测 **PASS（0 issue）**。唯一红项 `tests/runtime::TestRuntimeLifecycle` 在干净 `origin/main` 上同样失败，属既有基线。

## 二、进度总表

| 阶段 | 内容 | 状态 | 证据 |
|---|---|---|---|
| N00 | 基线/分支/工作区 | 完成 | `origin/main=71c2e8eb`；PR#76 只动 image，Network 相关路径 `git diff` 为 0 |
| N01 | §2 未修偏差 10 处 + #7 lease WARN | 完成 | 见 §3 逐条 |
| N02 | 受理三态 + 平台第五张表 | 完成 | `admission_log.go`、迁移 `0036`、`claimPlatform` 读回 |
| N03 | 主 Worker（含平台/LB 提前分支） | 完成 | `worker.go` L252–434 |
| N04 | Attachment（自建 run trace、三模式分派） | 完成 | `attachment_worker.go` L96–225 |
| N05 | Switch / SG / DefaultEgress | 完成 | 三个 `*_worker.go` |
| N06 | 观察链（仅 watch + notify） | 完成 | `kc_observation.go` |
| N07 | 门禁 + NG29/NG30 | 完成 | 见 §5；只有既有基线红 |
| N08 | 真机验收 | **未开始（NOT_RUN）** | 未部署、无集群授权 |

## 三、关闭的偏差（§2）

| # | 原问题 | 现在 |
|---|---|---|
| 1 | `ObserveWork` 输出 `resource_state`/`attempt_result` 非合同字段 | 删除；改 `step.succeeded`/`step.waiting`/`operation.succeeded`/`operation.failed` |
| 2 | `ObserveVirtualSwitchWork` 输出 `provider_failure/node_name/node_uid/binding_state` 等 | 删除；节点身份改 `target{kind:"Node"}` |
| 3 | 上述 `step.failed` 缺 `step` | 已补（按固定 step 表） |
| 4 | `ErrorFields` 无数字状态码 | 补 `http.status_code`（403 等） |
| 6 | `controller_managed` 原错被吞 | 转换前记 `dependency.failed`（`error_source=local`）；`vm_managed` 同规则；两分支无 `rpc.client.completed` |
| 7 | 附件 lease 竞争无任何记录 | 补 `operation.conflict` WARN，不伪造 `operation_id` |
| 8 | watch 真实故障被降 WARN | 升 ERROR `dependency.failed`，保留 GVR 原错 |
| 9 | `processNextNotification` 无 span/事件 | 每 key 独立 span；失败 `dependency.failed` + `worker.retry_scheduled` |
| 10 | 四处 `ErrLeaseLost` 静默 | 三条 lane 记 `operation.lease_lost`；附件记 `operation.conflict`；返回值不变 |
| 11 | `error` 取外层包装文本 | 改为解包链，`ProviderError`/`biz.Error` 的底层 message 不再丢 |
| 12 | 三条 lane 无条件 `Resume` | 仅活动操作恢复来源 trace，终态开新 trace |

## 四、新增能力（§3）

- **受理三态**：新受理 `operation.accepted` / 幂等 `operation.replayed`（事务提交后）；`operation.rejected` 唯一落在 `stats.End`（只写方法、WARN 类 code、`Domain=="network.ani.io"`）。
- **第五张表**：`migrations/0036` 给 `network_platform_operations` 加可空 `telemetry_context`；平台受理写入、`claimPlatform` 读回，平台资源受理与后台恢复同 trace。
- **五 lane 事件**：`worker.run_*`、`operation.*`、`step.*`、`state.persist_failed`、`dependency.failed`、`resource.mutated/replayed`，`step.started/succeeded` 按 §6.1 全覆盖。

## 五、门禁（构建机 192.168.18.35，Go 1.26.10）

| 项 | 结果 |
|---|---|
| `verify-source`（含 `sqlc generate` 幂等 + gofmt + 生成物一致） | PASS |
| `verify-boundaries` | PASS |
| 故障注入 `net05a-build-faults --network-only` | PASS |
| `go mod tidy -diff` / `vet` / `build -trimpath` / `mod verify` / `git diff --check` | PASS |
| `go test ./...` | 仅 `tests/runtime::TestRuntimeLifecycle` FAIL —— **干净 `origin/main` 同样失败**（既有基线） |
| **合同校验器**（20 条真实产出 JSONL 过 `verify_logs.py`） | **PASS，0 issue** |
| NG29 trace 中立性单测 | PASS |
| NG30 真库迁移往返（五表） | PASS |

集成套件 `internal/data/network` 全量在 25m 超时；抽查的 DefaultEgress/VirtualSwitch/LB 用例在干净基线树上同样失败，判定为既有环境问题，**未记为 PASS**。

## 六、本次补齐的三个自检缺陷

进度自查时发现并修复，均已加回归测试或复验：

1. **重复 JSON 键**：事件 helper 自带 `action`/`operation_id`/`resource_*`，与 lane 身份字段重复，会让 `verify_logs.py` 判 `duplicate JSON key`。已在 `networkEvent` 内按“首见为准”去重。
2. **`operation.failed` 缺 `result=failed`**：合同 `fixed` 要求，已补。
3. **五个 step 漏了成功侧**：`ensure_vpc`/`ensure_subnet`/`ensure_egress`/`update_egress`/`delete_provider` 此前只在观察者当失败 step 名使用，`Step` 内没有 `step.started`/`step.succeeded`，不符合 §6.1 强制规则。已在 create/update/delete 三条 provider 变更分支补齐，`ProviderConflict`/`ProviderInUse` 时另记 `operation.conflict`。

补完第 3 项后重跑：合同校验器仍 PASS（20 条、0 issue），`go build`/`vet`/业务单测全绿。

## 七、明确未做

- 未部署、未在 10.10.1.66 或任何集群运行；NG01–NG28 真机项全部 NOT_RUN。
- 未改 Proto / 状态机 / 租约 / 重试 / 幂等 / 授权；未新增 K8s 查询；未恢复退役观察循环与 node-facts；未给 `network_attachments` 加列。
- 未向远端推送分支、未开 PR。

## 八、详细记录

逐条证据、行号锚点、集成失败对照表见 [2026-11-12-network-logging-phase1.md](2026-11-12-network-logging-phase1.md)。
