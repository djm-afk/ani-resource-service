# VPC 基础连接观察失败分支诊断（2026-09-17）

2026-11-09 引用维护：本记录保留原时点的实施与验收结论。标为“历史定位”或“仓外定位”的原路径在当前 checkout 无法解析，仅保留追溯信息，证据可用性为 not_verified；不表示归档已恢复或旧规则仍适用。处理范围见[文档修复记录](../../res-doc-fix-20261109.md)。

承接[独立 Watch 对照](../base-watch-20260917/README.md)，用户授权继续定位具体失败分支，并在原 Goal 范围内推进直接缺陷修正。

本轮先在 `f07178a6bd2100ae6fb9583a25de06703e586c94` 上添加[临时定点日志](instrumentation.patch.gz)，不改变观察或重试规则。构建、数据库及服务均在 Fedora；实际集群仍为 ani-test-1，已有 Public Subnet 不参与创建或删除。

运行目录：`/home/chabking/workspace/ani-network-service-runs/base-diag-20260917T1437`。隔离实例 `basediag-0917`，管理 namespace 同名，租户 namespace `bd-ba2860dc-d656-43e5-9dae-effd084419ac`。只有新 VPC 和自动基础 EIP/SNAT；Intranet 池资格来自本轮实际检查，非复用历史 ready。

诊断二进制 SHA-256：`ce47f75578b230b94cf8e3777e701c528b4f4bd9dd9327a329dd2833f6878c8f`；调用驱动保持 `694b5f80db05672e463ebc6c4322517d5e98a8441f8792c53aef1c3cda37eb50`。原始源码包 SHA-256：`048dbf724fa8462ad637bc1d6fa684fdfed88d6196a6910822b49f592abc0751`；远端 gofmt 后的有效源码另存 manifest。诊断构建只通过编译，不继承原发布构建的验证资格。

日志标记 `[DEBUG-basewatch]` 记录失败分支、资源身份、来源代次/相关键变化、请求覆盖代次和剩余调用预算，不输出对象正文、配置或凭据。临时日志已从产品源码撤回，诊断构建及其独立源码快照仅作为证据保留。

## 已确认的具体原因

诊断窗口首次 ready 后采集 600 秒，PG 为 ready 487、degraded 114 个样本，4 段退化约 25/30/30/29 秒。五条独立 Watch 无断流，目标 VPC/EIP/Snat 在就绪后的 UID、版本和状态保持；产品 Watch 错误及审计采集失败计数均为 0。

首次失败完整链路（UTC）：

1. 14:42:15.243，ServiceCIDR 的正常 Watch 续接推进全局来源代次到 25。
2. SNAT 进入关键路径重新采集，采集边界为 14:42:15.882，使用代次 25。
3. 14:42:20.242，VNic 的 Watch 又续接，来源代次推进到 26。
4. 14:42:23.881，校验明确记录 `invalid_epoch`：view=25/current=26；对象通知 sequence 两侧均为 657，说明这次失效由来源边界触发，而非相关对象变化。
5. 同次 SNAT 关键采集返回 `invalid_view`/ProviderTemporary，已经耗时约 8 秒，原调用仍剩 **21.518 秒**；覆盖代次已满足（12/12），没有读取错误 cause。
6. 14:42:24.509，PG 基础连接变为 degraded；Provider/EIP/SNAT 事实年龄分别约 0.62/9.13/11.04 秒。SNAT `applied_enabled` 被保守写为 NULL，随后 worker 退避与成功观察使状态恢复。

就绪后捕获的 EIP/SNAT 关键路径失败共 6 次，全部为 `invalid_view`，失败时仍剩约 21.5–25.3 秒预算。根因是 Egress 审计恢复只允许一次完整刷新，在正常连续续接下过早耗尽次数；worker 正常执行了错误结果的持久化和重试，不是没有调度，也不是已证实的业务断网。

## 修正范围

Egress 采集（历史定位：`../../../../../internal/data/kc_egress.go`）仅对缓存缺项、审计失效、覆盖不足进行原期限内的重新采集，每次推进数据库采集边界并直接核验对象。实际读失败仍返回；取消/超时不返回可用证明；没有截止时间的调用保留原一次回退限制。原 60 秒新鲜度、UID/归属、并发围栏、PG 未知事实规则及 worker 退避均保持。

权威[观察规格](../../../../specs/cr-observation.md)同步更新。此修正针对基础连接所依赖的 Egress 路径，没有顺带改变 LB 自身的审计恢复规则。

## 验证与证据边界

针对性回归（历史定位：`../../../../../internal/data/egress_audit_retry_integration_test.go`）使用真实 PostgreSQL 和实际 adapter，以两个真实 HTTP Watch 中断在连续完整采集期间推进来源代次。原候选在仍有调用预算时返回 temporary（red）；修正后同一次调用取得稳定事实，并通过持续失效的截止时间、SNAT UID 替换保护（green）。诊断实测已在该回归启动前捕获全部 4 段退化；回归在独立 testenv 数据库运行，不写实际实验库。

相关 PostgreSQL/race 选集的 44 个顶层测试通过，没有数据竞争报告；容量测试按原范围跳过，保持 not_verified。首轮将总测试时限设为 180 秒，在 race 进程构建与恢复测试中超时，已保留原始记录；14 个已通过的顶层测试没有重跑，剩余选集用 6 分钟上限完成。超时遗留的本任务受控子进程按精确路径停止，测试数据库由本任务 PG 收尾清理。没有把这次运行器超时算作产品断言通过，也没有修改断言。

`make verify` 已通过：固定生成器一致性、边界、依赖、非 PG 常规测试、vet、构建及模块校验；真实 PG/race 证据另记。修正运行二进制 SHA-256 为 `fb3db3aa041e921f950f005a0a1c120e532d56ca58245e024cbca6421e95eb6c`，本地运行源码及测试文件已逐项对齐远端有效 manifest。

15:01:59 UTC 切换修正构建；保留原数据库及 CR 身份、原配置文件和平台资格记录，先保存私有 PG 备份。15:02:07 全部基础事实在重启后重新观测为 ready，才启动第二个 600 秒窗口。修正运行进程额外纳入 `CPUQuota=200%/MemoryMax=2300M/MemorySwapMax=0/GOMAXPROCS=2` 保护，诊断运行进程未使用此 scope；因此这是同产品现场的功能对照，不是严格单变量性能基准。代码行为的单变量红绿证据来自前述受控回归。

修正窗口于 15:12:08 UTC 结束。[前后对照](comparison-final.json)：数据库/API 均为 **602/602 ready**，0 段退化、0 次 stale、0 次 SNAT 应用事实未知；经历 24 次产品 Watch 来源续接，Watch 错误和审计采集失败始终为 0，五条独立 Watch 无断流。前后及窗口结束后的直接 GET 确认三个 CR 的 UID/resourceVersion/generation 完全相同。修正窗口没有非空错误 reason 日志；事实最大年龄约 40.55 秒，原新鲜度上限未修改。

这确认本次 Egress 过早放弃采集的问题得到修正。有限窗口不代表永久稳定、实际流量或全部 LB 观察路径已验证；历史双入口 LB 自身的观察问题仍单独记录。当前进度只维护于[执行状态](../../../status.md)。

## 收尾与交付

[清理回执](cleanup.json)确认实验 VPC/EIP/SNAT/Intranet 池均为 deleted、EIP claim 已释放、Provider pending action 为空，产品 CR 和实验 Pod 已消失。支持 namespace/RBAC、运行进程及隔离 PG 容器已清理，最终私有 PG 备份保留在 Fedora。第一次池删除早于依赖释放，被 `RESOURCE_IN_USE` 明确拒绝；保留原请求/计划，确认依赖删除后重新提交正常产品 Delete 完成，没有强删或伪造成功回执。

已有 Public Subnet `pubdebug-20260917-public` 的 UID `39619c25-703e-4db2-a35a-d44ee2acb780`、generation 1 和 spec 与开始时完全一致；保留的 Public/LB/SNAT 现场不参与本次清理。

[完整证据包](evidence.tar.gz)及其[逐文件和归档哈希](evidence-manifest.json)包含前后 Watch、PG/API、定点日志、驱动调用、测试及构建记录。导出前扫描未压缩明文，只排除了三处已核实的调用驱动 SHA-256 误报，见[扫描说明](export-scan-note.json)；凭据、kubeconfig、私有配置和 PG dump 均不进入证据包。

源码基线为 `f07178a6bd2100ae6fb9583a25de06703e586c94`；修正运行源码以[远端有效清单](green-manifest.json)为准，本地 203 个运行/测试/构建输入逐项匹配。交付目标沿用 `codex/net-vpc-lb-02`，用户已授权本地提交及推送；不合并 main、不打 tag、不部署。最终提交与远端一致性以 Git 历史及交付回执为准，CI 未在本记录中认定通过。
