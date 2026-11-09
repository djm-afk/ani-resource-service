# NET-02–04 完成要求核对

2026-11-09 引用维护：本记录保留原时点的实施与验收结论。标为“历史定位”或“仓外定位”的原路径在当前 checkout 无法解析，仅保留追溯信息，证据可用性为 not_verified；不表示归档已恢复或旧规则仍适用。处理范围见[文档修复记录](../res-doc-fix-20261109.md)。

依据原始 `.work/net-02-04-goal.md` 全文、固定规格及恢复 Goal 时的用户决定逐项核对。唯一当前进度仍在 [status](../../status.md)；本文件是交付时点审计，不是另一份实施计划。完整命令、退出码和快照摘要见 [运行清单](runs.json)，结论与限制见 [组合记录](../NET-02-04-implementation.md)。

## 要求与实际证据

| 原始要求 | 实现与验收证据 | 结论 |
|---|---|---|
| 固定输入、首次清单、ANI 独立 worktree、保护未知输入 | [初始 Network](network-initial.json)、[初始 ANI](ani-initial.json)、[最终清单](final-sources.json)；HEAD/tree 与 Goal 相同，暂存区为空；[并发输入归属](concurrent-inputs.md) 单列 KC-KIND | `pass` |
| 规范、NET-04 接口范围、closed DTO、owner 协议、目标和锁顺序先明确 | [规格](../../../specs/vpc-subnet.md)、[计划](../../../plans/vpc-subnet.md) 与 [Proto](../../../../api/network/v1/network.proto)；Console 明确排除，无平行总状态或通用流程引擎 | `pass` |
| Subnet 四 RPC、Operation、真实同快照 subnet_count、description/状态/原因/时间/分页 | 业务（历史定位：`../../../../internal/biz/subnet.go`）、SQL（历史定位：`../../../../internal/data/queries/subnets.sql`）、VPC 查询（历史定位：`../../../../internal/data/queries/vpcs.sql`）；`TestSubnetTargetConstraintsAndPagination` 及九路由 HTTP 实际链 | `pass` |
| CIDR/RFC1918/前缀、gateway 缺省/null/非法值、父状态与静态意图 | `TestSubnetGatewayNormalizationAndFingerprint`、`TestSubnetAddressRaceAndParentDeletionRace`、`TestSubnetStaleParentRejectsNewIntentButNotPersistentReplay` | `pass` |
| 永久幂等、同父并发、跨 VPC 重叠、父删除竞争 | `TestSubnetConcurrentPermanentIdempotencyAndDeletion` 与地址/父删除竞争测试；真实 PG 事务先锁 VPC，再 Subnet，再 Attachment/Operation，无进程 mutex 代替约束 | `pass` |
| 后续 migration、0001 不变、空库/NET-01 升级、目标恰一/一致、租户 FK、runtime 无 DDL/提权、Network 无 RLS | [0002](../../../../migrations/0002_subnet.sql)、[0003](../../../../migrations/0003_attachment.sql)、升级测试（历史定位：`../../../../internal/data/subnet_upgrade_integration_test.go`）、真实数据库 fixture/目标负例及六项 SQL 租户谓词变异；0001 SHA-256 保持 `c9c99dd3cfba59f5a6fcf8025e7d0d433a8bd322df1088c58912a44d1e32736a` | `pass` |
| 复用 NET-01 worker/T1–T4、实际 kc adapter、父引用/UID/RV/readiness/残留关系 | 资源 worker（历史定位：`../../../../internal/data/resource_work.go`）、kc adapter（历史定位：`../../../../internal/data/kc.go`）；`TestSubnetKCContractAndResidualReferencesBlockCleanup`、`TestSubnetProcessesRecoverT1ProviderSuccessAndUnknownDeletion` | `pass`（受控 Provider） |
| 未知 POST 不凭404重发、未知删除保留、lease/epoch/旧回写、纯读/stale/墓碑 | Subnet unknown-create/lease 测试、独立进程测试及全量 VPC 回归；`TestAttachmentUnknownConsumerDoesNotExpireAndEpochRejectsLateWrite`；停 worker 后查询不推进，恢复后继续 | `pass` |
| Attachment 永久稳定身份、active slot、父子删除互斥、DTO placement 与最终注解白名单 | Attachment（历史定位：`../../../../internal/biz/attachment.go`）；`TestAttachmentPermanentIdentityTenantScopeAndParentDeletion`、`TestAttachmentPrepareRacesDeleteSubnet`；ANI 实际 renderer/plan 测试拒绝网络覆盖与默认网络回落 | `pass` |
| 实例 owner 先原子保存身份、唯一工作负载 writer、实际 renderer/审计/dry-run、pending/恢复、按 Attachment namespace 观测 | ANI 适配记录（仓外定位：`../../../../../.worktrees/ani-net-02-04/repo/development-records/net-02-04-network-integration.md`） 链接实际迁移和装配；`TestNetworkSubmissionAtomicStableIdentityAndTenantIsolation`，受限 ani_app 迁移权限/RLS；实际 owner/Network/HTTP 独立进程 | `pass` |
| Confirm/Release 同身份先于 version、不可变 Pod UID、owner 稳定封闭纯读、未知提交保留、Network 自核验释放 | `TestAttachmentConfirmRecoveryReleaseAndResidualUIDs`；十场景 wire 的 Confirm/Release 丢失、未知/晚到 POST、分别和同时重启、VNic/IP 分阶段残留；同一数据库/Provider 保留，无手动改业务状态求通过 | `pass` |
| reserved/attached/releasing 删除保护、released 重放不复活、迟到对象协议墓碑和地址保护、四状态后台恢复 | Attachment 集成测试、`TestAttachmentLateAfterSubnetDeletedProtectsCIDRAndVPC`；wire 的多 Pod/错 UID/released 迟到对象跨重启继续阻止删除 | `pass` |
| Network 唯一 schema writer、ANI descriptor/pins/固定生成、无兄弟运行依赖、双进程避免重复注册 | [契约来源清单](network-contract-source.json)、ANI contract_pins、`make validate-network-integration`；生成实际来自 descriptor，来源明确为未提交源码 | `pass` |
| 九 REST 路由、严格当前租户/actor、缺身份零调用、无旧 fallback、201/202/200/404/错误映射 | 实际 Gateway `network_lifecycle.go`、`network_explicit_instance_test.go`、`network_lifecycle_process_test.go`；十场景 wire 的九路由、实际两页/绑定游标、null/旧字段/跨租户/断连恢复 | `pass`（接口范围） |
| 精确 breaking changes、OpenAPI/SDK/API docs/authz 固定生成、保留其他约束 | [组合记录](../NET-02-04-implementation.md) 的 breaking 清单；原生成入口与生成漂移检查；[兼容逐项比对](compatibility-audit.json) 证明 230 operation、313 schema 无新增回归，302 旧策略不变 | `pass`；原全量 compatibility 仍 `fail` |
| 隔离远程快照/权限/hash/实际命令、固定工具、真实两库/受限角色、清理和生成物回传防并发覆盖 | [运行清单](runs.json) 与每轮 snapshot/log；SSH ubuntu/Go1.26.7/Buf1.60/sqlc1.31.1/PG18.6固定digest；回传先比对源 hash；本任务 PG 容器退出清理，源快照/工具/镜像缓存保留 | `pass` |
| 五项 Network、十二项 ANI 门禁及专项测试实际执行 | [组合记录门禁表](../NET-02-04-implementation.md#已执行门禁与故障矩阵) 对应真实日志；所有要求保留，唯一例外是用户接受单独记录的八项固定兼容差异 | 其余 `pass`；compatibility `fail` |
| 最终未提交源码、未跟踪/范围/链接/diff/生成/SBOM、四份 ANI feature 记录 | [最终清单](final-sources.json)；最终远程运行复验文档入口、生成与 SBOM，并两次生成逐字比较；完整交付归档和实际执行结果保存在最终 pair 目录，由交付回复定位 | `pass` 以最终运行 exit0 为准 |

## 验收编号与限制

V-01 对应上表门禁；V-02/03/04 为真实 PG 的资源、租户、FK、幂等及变异；V-05 为地址/父子并发；V-06/07/08/09/11 为受控 Provider、实际进程恢复/竞争/纯读；V-10 为两端持久接入/封闭/释放；V-13 仅 Gateway 接口。进程 fixture 的入口身份是受控输入，kc HTTP fixture 合成 controller 的 readiness、子对象和 GC，不外推真实权限配置、controller 或数据面连通。

V-12 真实 kc/OVN/kind 普通容器数据面、V-14 VM、V-15 IAM、Console/前端和生产部署均 `not_verified`，原 Goal 明确排除。ANI 全历史 Atlas 目录存在固定基线 checksum/重复版本失败；本包新迁移实际 SQL/角色测试通过，未改写旧 SQL 或 atlas.sum，不宣称全目录重放完成。

原全量 Core compatibility 的 `fail` 与 [用户接受的既有失败分离决定](baseline-gate-drift.md#恢复-goal-时的用户决定2026-09-09) 均保留。认证补丁撤回且从未应用；本决定不减少 Network 实现或验收范围，不授权后续认证修复。

交付后停止。NET-05 的固定源码/契约、历史迁移整理、隔离环境、kc/OVN/RBAC/placement/两端地址及真实连通/隔离/清理条件见组合记录；本 Goal 不启动 NET-05/06/NET-AUTH，不提交、推送、发布或部署。
