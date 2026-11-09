# NET-U08 存量基础连接管理工具

2026-11-09 引用维护：本记录保留原时点的实施与验收结论。标为“历史定位”或“仓外定位”的原路径在当前 checkout 无法解析，仅保留追溯信息，证据可用性为 not_verified；不表示归档已恢复或旧规则仍适用。处理范围见[文档修复记录](../res-doc-fix-20261109.md)。

日期：2026-09-14。本记录描述本批实现入口和隔离验证方法；当前验收结果只汇总到[执行状态](../../status.md)，产品规则见[存量补齐规格](../../../specs/vpc-connectivity-lb.md#82-既有-vpc-补齐)。工具未在真实存量环境执行。

## 实现与权限边界

命令入口（历史定位：`../../../../cmd/ani-network-service/base_connectivity.go`）是同一 `ani-network-service` 二进制的显式 `-base-connectivity` 模式，与正常服务、migration、node-facts 三个入口互斥。它只使用受限运行数据库角色，读取 `ANI_NETWORK_DATABASE_DSN`、`ANI_NETWORK_CLUSTER_ID` 和可选的 `ANI_NETWORK_NAMESPACE_PREFIX`（默认 `tenant-`），不读取 owner DSN，不打印 DSN，不创建 Kubernetes 客户端，不需要 kubeconfig。

持久用例（历史定位：`../../../../internal/data/base_backfill.go`）与 sqlc 查询（历史定位：`../../../../internal/data/queries/base_backfill.sql`）负责审核计划和受理。Provider 执行、观察、重试和清理仍由正常服务的既有持久 worker 负责。运行数据库凭据就是此离线管理入口的能力边界，应仅给获准运行维护命令的操作者；该 CLI 不能作为租户 HTTP/RPC 暴露，也不把命令行参数当成 IAM 管理身份。

执行以下示例前，由本次操作的授权环境提供变量和受限 DSN；示例不包含任何真实地址或凭据。实际上线/正式补齐不是本 Goal 的授权范围。

## 审核计划、限速执行与恢复

1. 分配一次稳定的非零 UUID 作为 `BASE_RUN_ID`。同一 run ID 重复规划会返回原快照；不同池或不同速率不能覆盖旧 run。每个计划最多容纳 10,000 个当前 cluster 的 VPC，超过则拒绝保存，绝不静默截断。
2. 运行 `plan`，只保存 paused=true 的计划及候选/排除/冲突清单，不产生 ensure operation、EIP、SNAT 或任何 Provider 写入。未传 pool 时只在首次计划固定当时的默认 Intranet 池及配置版本；此后默认池切换不改变计划。
3. 审核完整输出，记录 `plan_sha256`。审核值覆盖格式版本、run ID、cluster、固定池/版本、速率及每项 tenant/VPC、审核时 VPC 版本、namespace、binding ID/name/UID、最初分类和原因；后续执行 state/reason/operation 变化不改变该摘要。
4. `resume` 必须提交精确的已审核摘要，才能写 reviewed_at 并解除暂停。`dispatch` 只消费这个已持久审核的计划，使用数据库时间和 run 行锁在多个 dispatcher 间共同限速。
5. `pause` 封闭后续受理。已受理 operation 继续由服务 worker 恢复，不撤销地址或 claim。再次提交同一审核摘要 `resume` 后，`dispatch` 从下一项继续；退出、进程丢失和连接重建不会重复受理已完成的候选。

```bash
./bin/ani-network-service -base-connectivity=plan -base-run="$BASE_RUN_ID" -base-interval=1s
./bin/ani-network-service -base-connectivity=status -base-run="$BASE_RUN_ID"
./bin/ani-network-service -base-connectivity=resume -base-run="$BASE_RUN_ID" -base-reviewed-sha256="$BASE_REVIEWED_SHA256"
./bin/ani-network-service -base-connectivity=dispatch -base-run="$BASE_RUN_ID" -base-max=20
./bin/ani-network-service -base-connectivity=pause -base-run="$BASE_RUN_ID"
```

显式固定某个池时，在首次 `plan` 增加 `-base-pool="$BASE_POOL_ID"`。速率只接受整数毫秒，范围 100ms—1h；`-base-max` 范围 1—10,000，默认一次处理一项。dispatch 输出逐步 JSON，`waiting` 表示共享速率截止时间尚未到；长间隔仍每秒重新检查暂停，不需要长期持有事务。

`accepted` 只证明本地持久受理；`admission_complete` 只表示该计划没有 pending 候选，不表示 ensure 完成或真实流量通过。`status` 是纯读，同时显示独立 ensure operation 状态和基础连接当前状态。存在 conflict/excluded 项时，不能把 admission_complete 当作整个存量范围完成。

## 候选、冲突和失败处置

计划按当前 cluster 的 tenant namespace 及 VPC Provider binding 识别候选；有 tenant namespace 而缺失 binding 的资源纳入冲突。完全失去 cluster/tenant namespace 归属证据的数据不能被猜测到某一集群，须作为独立数据异常核查，工具不收养同名 CR。

| 情况 | 记录和处理 |
|---|---|
| VPC 删除中/已删除 | excluded；不重新建立基础连接 |
| 已有基础连接意图 | excluded；其既有 operation/worker 持续恢复，另一个 run 不重复分配 |
| 非 available/degraded、旧 operation 未成功、binding 缺失/UID未知/pending mutation | conflict；不受理，修正明确前提后创建新的审核 run |
| 计划后只发生正常观察版本增长 | 不使审核失效；重新锁当前 VPC、核验当前状态和固定身份，用当前版本原子更新 |
| 计划后进入删除、基础连接被其他 run 受理、Provider namespace/name/UID/binding ID 改变 | conflict；原审核快照保持，禁止按新身份继续或按名称收养 |
| 固定池关闭、过期或基础配置暂不可用 | 自动 pause，候选保留 pending，reason 明确；修复同一固定池前提后 resume 原摘要，不换池 |
| 数据库中审核快照与 hash 不符 | 拒绝读取/恢复受理；保留现场，不修改摘要掩盖漂移 |
| 受理事务中的任一步失败 | savepoint 回滚新 operation、子资源和占用；成功状态及失败原因在外围 run 事务中落库 |
| ensure 已受理后遇到 Provider 超时/未知结果 | 原稳定子资源、claim 和待执行步骤保留，由同一共享 worker 恢复；CLI 不重试 Provider 写入 |

一个受理事务锁定 run，然后锁 VPC，核验其 tenant/placement/绑定 UID、当前状态及既有 operation；取得固定地址池锁后重新读取数据库时钟，拒绝在锁等待期间已经过期的事实；新建 `ensure_vpc_base_connectivity` operation，固定池和子资源，更新 VPC 的 last_operation 指针并持久唤醒原 reconciliation。旧 `create_vpc` 成功 operation、原幂等响应不被更新。VPC 在 legacy 聚合激活前保持其原状态，只新增基础连接摘要。

并发删除与补齐以同一 VPC 行锁串行。删除先到，候选不受理；ensure 先到，删除走已有系统子资源清理流程，未知 Provider 结果仍保留占用。工具不强删 finalizer、不直接释放 claim、不把数据库行清空当成成功。

## 新 VPC 与 legacy 聚合切换

```bash
./bin/ani-network-service -base-connectivity=rollout
./bin/ani-network-service -base-connectivity=enable-new
./bin/ani-network-service -base-connectivity=activate-legacy
```

`new_vpcs_enabled` 和 `legacy_aggregation_enabled` 分别持久化。初始两个开关均为 false；`enable-new` 使之后的新 VPC 在首次受理时保存基础意图并使用聚合条件，已经受理的意图不受后续开关变化影响。新 Public 绑定按基础连接就绪准入，不以 legacy 聚合尚未激活为由绕过。

`activate-legacy` 要求新建路径已启用，并与首次 VPC 受理共用 cluster 入口锁。它按稳定顺序锁定全部非 deleting/deleted VPC，逐一核验真实持久基础状态、EIP/SNAT 观察时效、UID/pending mutation、启用事实与 bound claim，任何缺失/过期就回滚整个激活事务。通过后统一标记基础依赖必需并开启 legacy 聚合；不能只凭补齐 operation 曾成功或一个计划没有 pending 判为通过。

legacy 聚合激活前，可用 `-base-connectivity=disable-new` 关闭新建路径；它不删除已受理基础意图。激活后本批不提供降低 legacy 聚合语义的开关，disable-new 也会拒绝，避免重新混入无基础意图的新 VPC。后续兼容升级/回退策略需消费本批 migration、claim 及执行器，不可直接回退到不认识 Intranet/LB claim 的旧二进制。

## 验证证据边界

真实 PostgreSQL 回归（历史定位：`../../../../internal/data/base_backfill_integration_test.go`）新增以下检查，均由主任务在 ubuntu 的隔离 run 执行；最终隔离演练结果为 `pass`，精确命令、源码 manifest、退出码和逐项测试见[最终门禁](gates.json)。

| 测试 | 验证层 |
|---|---|
| TestBaseBackfillReviewedSnapshotPoolPinningRatePauseAndConnectionRecovery | 两租户候选、错误审核值、切默认池、观察版本增长、双 dispatcher 限速、暂停/连接恢复、历史快照保持 |
| TestBaseBackfillRechecksFreshnessAfterPlatformLockWait | 真 PG 锁等待跨过事实有效期后拒绝受理，原 plan 暂停且零孤儿 ensure |
| TestBaseBackfillClosedPoolRollsBackAndResumesSameIntent | 固定池关闭时 savepoint 零孤儿、同 run 解除阻塞后受理 |
| TestBaseBackfillRejectsChangedProviderIdentityAndRacesDeletion | UID 漂移拒绝，补齐/删除实际并发，受控 Provider+共享 worker 清理 |
| TestBaseBackfillLegacyAggregationRequiresAllFreshDependencies | 新建开关、缺失/陈旧基础依赖拒绝，完成 ensure 后统一激活，历史 create 保持 |
| TestBaseBackfillCLIProcessRestartKeepsReviewedPlanAndSingleAdmission | 实际生产二进制多个独立进程 plan/resume/dispatch/status，数据库计划恢复，重复 dispatch 无双重受理，不读 kubeconfig |

本地仅编辑和轻量检查；编译、真实 PG 与测试全部在 ubuntu 隔离 run 执行，未操作共享集群。受控 Provider、连接恢复、实际命令进程都不替代 U09 的旧工作负载不中断、真实内网访问和 Public 源地址验证；这些数据面项仍为 `not_verified`。
