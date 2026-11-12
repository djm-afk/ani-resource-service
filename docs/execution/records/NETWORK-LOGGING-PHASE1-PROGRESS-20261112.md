# Res Network 日志整改一期 — 实施进度

更新：2026-11-13。分支 `feat/network-logging-phase1`（基于 `origin/main = 71c2e8eb`）。PR [#77](https://github.com/zhangzhe-ctrl/ani-resource-service/pull/77)。
**当前状态：代码完成 + 离线门禁通过 + 真机验收（覆盖部署 + 独立库）已完成并已还原。**

## 一、一句话结论

执行包 `docs/00-实施/ANI-Res-Network-Logging-Main-v1.0/` 的 N00–N08 已全部落地（§2 十处偏差 + §3 业务事件 + 平台第五张表 + NG29/NG30 + 真机验收）。产出 20 类事件经合同校验器 `verify_logs.py` 实测 **PASS（0 issue）**；真机以「覆盖主 Deployment + 独立库」方式跑通 11 项 NG，累计 65 条真机 JSONL 过校验器 **0 issue**，验收后已完整还原。唯一红项 `tests/runtime::TestRuntimeLifecycle` 在干净 `origin/main` 上同样失败，属既有基线。

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
| N08 | 真机验收 | 完成（11 项 PASS，其余 NOT_RUN 已分栏） | 覆盖部署 + 独立库，验收后已还原；见 §10 |

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

## 六、CI 归因（PR #77）

结论：**没有本 PR 新引入的失败**。逐项与 `main@71c2e8eb`（run 43209632269）比对：

| 检查 | PR #77 | main | 判定 |
|---|---|---|---|
| `source` | `TestRuntimeLifecycle` | 同 | 既有 |
| `network (lb)` | 6 个 LB 用例 | 6 个同名用例 | 既有（集合完全一致） |
| `supply-chain` | govulncheck 65 条标准库告警 | 同 65 条、首条同 | 既有 |
| `network (core)` | 上述两项 + 2 个 `internal/server` race | main CI 仅 1 项 | **需说明，见下** |
| `image` | PASS | PASS | — |

`network (core)` 多出的两个失败：

- `TestClusterOpsRequestLoggingRedactsBody`、`TestImageRuntimeMTLSListener`
- 均为 **data race**：测试主 goroutine 读 `bytes.Buffer.String()`，与 gRPC 服务端 goroutine 写同一 buffer 竞争；两个测试文件（`clusterops_test.go`、`image_runtime_test.go`）本 PR 未改
- 在干净 `main@71c2e8eb` 独立树（`git archive`）上以 `-race` 复跑：**3/3 稳定失败**；本分支同样 **3/3 失败**。即确定性既有缺陷，与改动无关
- main CI 未报，是它跑全包集合时该用例失败前先被其他因素中止；单跑必现

## 七、本次补齐的三个自检缺陷
进度自查时发现并修复，均已加回归测试或复验：

1. **重复 JSON 键**：事件 helper 自带 `action`/`operation_id`/`resource_*`，与 lane 身份字段重复，会让 `verify_logs.py` 判 `duplicate JSON key`。已在 `networkEvent` 内按“首见为准”去重。
2. **`operation.failed` 缺 `result=failed`**：合同 `fixed` 要求，已补。
3. **五个 step 漏了成功侧**：`ensure_vpc`/`ensure_subnet`/`ensure_egress`/`update_egress`/`delete_provider` 此前只在观察者当失败 step 名使用，`Step` 内没有 `step.started`/`step.succeeded`，不符合 §6.1 强制规则。已在 create/update/delete 三条 provider 变更分支补齐，`ProviderConflict`/`ProviderInUse` 时另记 `operation.conflict`。

补完第 3 项后重跑：合同校验器仍 PASS（20 条、0 issue），`go build`/`vet`/业务单测全绿。

## 八、明确未做

- 未部署、未在 10.10.1.66 或任何集群运行；NG01–NG28 真机项全部 NOT_RUN。
- 未改 Proto / 状态机 / 租约 / 重试 / 幂等 / 授权；未新增 K8s 查询；未恢复退役观察循环与 node-facts；未给 `network_attachments` 加列。
- 未向远端推送分支、未开 PR。

## 九、详细记录

逐条证据、行号锚点、集成失败对照表见 [2026-11-12-network-logging-phase1.md](2026-11-12-network-logging-phase1.md)。

## 十、真机验收（2026-11-13）

### 10.1 方式与还原

采用**覆盖主 Deployment + 独立库**（与 Gov-Res 包第二轮同一路径），不是旁挂副本：只有这样才能让 Gov 流量与真实 Service 端口进入验证。

| 项 | 值 |
|---|---|
| 实测镜像 | `ani-resource-service:netlog-20261112`，digest `sha256:5258e5b1…` |
| 镜像自证 | `crane export` 取出的二进制 sha256 `bf28e3bf…` 与本地构建一致；`strings` 命中 `operation.lease_lost`/`notify_observed_object`/`network-default-egress-worker` |
| 数据库 | 独立库 `ani_resource_netlog`，network **36** / instance 11 / sandbox 9 / vm 9 / compute 2 / image 1 |
| 覆盖前备份 | `.tmp/deploy-backup-20261112/`（Deployment + Secret + `rollback.sh`） |
| 还原 | 镜像回 `15f2d94c…`；Secret 15 键与备份逐键一致；独立库与 4 个角色已 DROP；迁移 Job/Secret/客户端 Pod 已删；1/1 Ready |

切换顺序为「先 patch DSN、后换镜像」，中间态必然不可用（旧镜像迁移集合到 network 0033，独立库是 36），因此一步内完成。

### 10.2 结果

| ID | 结果 | 关键证据 |
|---|---|---|
| NG01 | **PASS** | 单行 JSON，非 JSON 行 0；`process.starting`（`log_format=json`、`log_level=INFO`）+ `process.ready` |
| NG02 | **PASS** | `ANI_LOG_LEVEL=debug` 下健康路径 DEBUG，INFO/WARN 等级与内容不变 |
| NG03 | **PASS** | `ANI_LOG_LEVEL=warn` → `process.start_failed` ERROR、`step=logger`、真实原因，进程退出 |
| NG05 | **PASS** | CreateVPC 返回 VPC/operation；VPC 实际落到 K8s（`vpc-5619a80a…`，10.128.0.0/16，Ready）；同 trace `operation.accepted` |
| NG06 | **PASS** | 非法参数 → `rpc.code=InvalidArgument` WARN/rejected；`operation.rejected` WARN 带 `reason=INVALID_ARGUMENT`；无 accepted |
| NG07 | **PASS** | 未知方法 → `rpc.server.completed` `PermissionDenied`/WARN/rejected，恰一条；`UnknownServiceHandler` 原行为保留 |
| NG15 | **PASS** | RPC 与后台同 trace `76978bff…`、span 不同；`operation_id` 贯穿 |
| NG17 | **PASS** | 经 `ProviderError` 包装的真实 403：`http.status_code=403`、`error_code=Forbidden`、`error` 为完整原始 message（含 verb/resource/namespace/身份） |
| NG20 | **PASS** | watch 403 → **ERROR** `dependency.failed`（未降 WARN），`error_source=kubernetes`，保留 GVR 与原错 |
| NG25 | **PASS** | `/healthz` `/readyz` `/metrics` 成功 DEBUG；404 → WARN/真实 code 并带 `http.status_code`、`duration_ms` |
| NG30 | **PASS** | 五张 operation 表 `telemetry_context` 均为可空 jsonb（真库） |
| 合同校验 | **PASS** | 累计 65 条真机 JSONL，`verify_logs.py` `issue_count=0` |

**附带强证据（原 §2#5/#12 的直接验证）**：VPC 操作完成后，终态周期观察轮次的 `trace_id` 从受理 trace `76978bff…` 切换为新 trace `9995060b…`，证明「终态不继承创建 trace」在真机生效，且该轮未报 `telemetry.context_missing`。

### 10.3 NOT_RUN 与原因

| 项 | 原因 |
|---|---|
| NG10 / NG11 / NG12 | 独立库 `network_attachments` 为空，无 workload 产生附件；构造需先建 instance/sandbox，超出 Network 域 |
| NG21 | 通知失败需精确命中 `NotifyObservedObject` 执行瞬间的 DB 断连；两次以 `pg_terminate_backend` 尝试均在连接池自动重连后才轮到通知，未命中 |
| NG04 / NG08 / NG09 / NG14 / NG16 / NG18 / NG19 / NG22 / NG23 / NG24 / NG26 / NG27 / NG28 | 需专门故障注入（panic、解码失败、重启恢复、并发临界）或未构造的场景，本轮未做 |

其中 NG18 已由定向单测覆盖（四条路径 + 终态不继承），但**单测不等同真机**，仍记 NOT_RUN。

### 10.4 故障注入与还原

| 注入 | 还原校验 |
|---|---|
| `ClusterRole/ani-network-egress` 移除 `vpcs` 的 `create`（NG17） | rules 指纹回到 `5a6b213dab43b9d7`；`auth can-i create` = yes |
| 同上移除 `vpcs` 的 `watch`（NG20） | 同上；watch/create/delete 全 yes |
| `ANI_LOG_LEVEL=debug`（NG02）/ `=warn`（NG03） | env 已移除，回到默认 INFO |

注入前备份与还原脚本在 `.tmp/inject-backup-20261113/`（含 rules 指纹与 `restore.sh`）。还原时 `kubectl apply` 会因 `resourceVersion` 冲突失败，改用 `kubectl replace --force` 并以「rules 指纹一致 + `auth can-i` 恢复」作为验收判据。

### 10.5 与历史做法的差异

Gov-Res 第二轮（2026-11-11）同样是覆盖部署 + 独立库。本轮额外确认：现网旧镜像的迁移集合止于 network 0033 且不含任何 `logging_telemetry` 迁移，因此**切库后旧镜像必然启动失败**，不能把「先切库、让旧镜像跑一会儿」当作可行中间态。
