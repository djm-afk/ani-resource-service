# LB 观察退化的具体分支诊断（2026-09-17）

2026-11-09 引用维护：本记录保留原时点的实施与验收结论。标为“历史定位”或“仓外定位”的原路径在当前 checkout 无法解析，仅保留追溯信息，证据可用性为 not_verified；不表示归档已恢复或旧规则仍适用。处理范围见[文档修复记录](../../res-doc-fix-20261109.md)。

本轮按用户要求定位“CR 正常但 Network 的 LB 观察状态退化”的具体原因。输入为已推送提交 `83a4346d05069fb957f9ff6096c00dd586504e62`，包括上一轮 Egress 修正；本轮没有修改产品源码，没有部署或发布实验补丁。

后续正式产品修正与 CI 问题结论见 [2026-09-18 修复记录](../lb-fix-20260918/README.md)；下文保留诊断时点事实。

## 结论

**LB 自身仍使用“缓存不合格后只完整刷新一次”的规则。刷新期间 Watch 续接使来源代次变化，LB 就在调用仍剩约 27 秒时返回临时失败。worker 将这次失败正常落库为 degraded / unknown / PROVIDER_UNAVAILABLE。**

这是在当前源码、真实 PostgreSQL、实际 adapter/worker/查询链上重复复现并经单变量对照确认的具体失败路径，不依赖 Public 流量失败。上一轮改的是 Egress 入口（历史定位：`../../../../../internal/data/kc_egress.go`），没有覆盖 LB 独立入口（历史定位：`../../../../../internal/data/kc_load_balancer.go`）。

本结论覆盖受控复现的 `PROVIDER_UNAVAILABLE` 路径，不把历史 `ProviderNotReady`、所有 stale 或“当时只有双入口退化”的每个样本都追认为同一原因。两种入口都能触发，历史入口差异是否仅由采集时序造成仍无逐次定点证据。

## 明确的因果链

以带探针的双入口用例为例（原始时间见 [结构化记录](probe-structured.json)）：

1. 产品生命周期已完成，LB 为 configured；调用前变更一个后端 Pod 的诊断 annotation，使旧审计需要刷新。随后冻结 Provider 对象集合，故障窗口前后 SHA-256 完全一致。
2. 新完整采集从来源代次 34 开始，两个真实 HTTP Pod Watch 在该采集期间续接，将代次推进为 36；没有修改 CR 状态、UID、版本、归属或就绪条件。
3. 进入 `observeLoadBalancer` 的 `coverage_or_view` 分支：`refresh=true`，`Proof.Covers=true`，`DependenciesReady=true`；view/current sequence 都是 41，相关键没有变化。唯一失效项为来源 epoch 34/36。
4. LB 采集（历史定位：`../../../../../internal/data/kc_load_balancer.go`）返回 `LB audit does not cover current dependency facts`。这个错误文本合并了“覆盖不足”和“视图失效”两种情况；本次实际是后者，并非通知覆盖不足。失败时还剩 **27.245 秒**。
5. worker（历史定位：`../../../../../internal/biz/load_balancer_worker.go`）把临时 Provider 错误转换成 `PROVIDER_UNAVAILABLE`，资源从 available 变成 degraded；持久化（历史定位：`../../../../../internal/data/resource_work.go`）因没有成功观察而将 configuration_state 设为 unknown，并保留旧 observed_at。这是保守处理失败的正常结果，不是 worker 未运行或数据库随机丢字段。
6. 实验副本仅在这一失效分支允许原 deadline 内继续完整采集，其他时限、配置、身份和新鲜度检查均不变。双入口观察约 **5.520 秒**完成，最终 configured、非 stale、无错误；仍余约 24.48 秒。CR 内容前后哈希保持。

对应[汇总](controlled-summary.json)：

| 实验 | private | public_private | 含义 |
| --- | --- | --- | --- |
| 当前源码 + 失败断言 | unknown / PROVIDER_UNAVAILABLE | unknown / PROVIDER_UNAVAILABLE | red，原始场景复现 |
| 相同源码 + 定点日志 | 同样失败 | 同样失败；明确 epoch 失效 | 排除读取超时、依赖不就绪及覆盖不足作为本次失败原因 |
| 只改变失效后的刷新次数 | configured / fresh | configured / fresh | 单变量反事实通过，修正方向有因果证据 |

每次使用新独立测试数据库，Provider 为受控 HTTP API；对象由实际 Network 生命周期产生，控制器侧生成资源由既有 fixture 模拟。受控测试不是真实 kcn 数据面验收。为选择目标 LB，诊断 repository 包装只跳过无关 fixture 的真实 PG Claim，未处理的 fixture 租约随隔离数据库删除；不据此验证 scheduler 公平性或饥饿问题。

运行命令（Fedora，资源受限 scope 内）：

```sh
bash /home/chabking/workspace/ani-network-service-runs/lb-diag-20260917T1538/run.sh red
bash /home/chabking/workspace/ani-network-service-runs/lb-diag-20260917T1538/run.sh probe diagnostic-source
bash /home/chabking/workspace/ani-network-service-runs/lb-diag-20260917T1538/run.sh counterfactual counterfactual-source
```

三者内部均执行 `scripts/integration -timeout 4m -run '^TestLBObservationDiagnosticConsecutiveWatchBoundaries$' -v ./internal/data`。red/probe 的 exit=1 是已命中退化断言，counterfactual exit=0。当前复现 fixture 在历史诊断基础上增加了真实失败断言；历史仅“采集成功”不再被误读为观察通过。

本地只编辑与检查。Fedora 编译、真实 PG 和测试使用 `GOMAXPROCS=2`、`GOFLAGS=-p=2`、CPUQuota=200%、MemoryMax=2300M、MemorySwapMax=0；每次临时 PG 为 768 MiB/1 CPU。输入[清单](source-manifest.json)、各阶段有效源码[清单](source-variants.json)和完整日志保留。

## 真实集群的只读旁证

ani-test-1 保留的 `pubdebug-20260917/dual-lb` 为手工 CR，没有对应 Network 数据库记录。因此没有伪造/收养产品 binding，也没有把该 CR Watch 冒充真实产品状态对照。

[只读 Watch 汇总](live-summary.json)覆盖 12 个 namespace/GVR 流、49 个初始对象，持续 **413.023 秒**，未出现 MODIFIED、DELETED 或 ERROR 事件。所观察范围包括 Gateway、Route、Backend、Policy、Service、Pod、VPC、EIP/Snat、VNic/VNicIP 及平台 Subnet。根因和反事实对照完成后主动结束采样，原 600 秒是上限，不记成完成了 10 分钟稳定性验收。

首次采集器误用当前 kubectl 不支持的 `--resource-version` 参数，所有 Watch 在启动前退出；原错误保留，不计入观察证据。第二次改用标准 `--watch --output-watch-events`，记录初始 ADDED 及之后的事件；12 个进程均由本轮按 PID/父进程验证后正常终止。已有 Public Subnet UID `39619c25-703e-4db2-a35a-d44ee2acb780` 保持，本轮没有任何集群资源写入，没有新增 Public 子网、EIP、LB 或流量探测。

## 修正方向与交付边界

推荐将 LB 的视图失效/覆盖不足恢复改为原调用截止时间内的有界重新采集，与已验证的 Egress 原则一致；每次重新获取数据库边界和完整审计，不复用失效视图，不续写虚假新鲜度。真实读失败、UID 冲突、取消/超时仍立即结束；无 deadline 的内部调用不能变成无界循环。普通 LB 与删除路径的身份/释放证明必须保持。

这里的反事实补丁只用于证明原因，采用递归形式且只处理已捕获分支，**不是待发布实现**。正式修正仍需覆盖前后两处视图失效、终止条件和身份保护，并更新权威观察规格中的 LB 单次刷新规则。本轮没有声称整个 Goal 完成或历史所有 LB 退化已修复。

## 独立发现：上一提交的 CI 失败

诊断期间读取 [83a4346 的 CI](https://github.com/zhangzhe-ctrl/ani-network-service/actions/runs/35239901724)，结果为 failure，失败步骤为 Real PostgreSQL and process recovery，另记为未完成门禁：

- `TestLBServiceProcessesRecoverUnknownMutationsWithTwoWorkers` 的 RPC 创建 fixture 未填写新增必填 `health_check.port`，请求被 InvalidArgument 拒绝；已从测试入参确认。它没有进入预期的故障恢复场景。
- `TestLBActualAdapterThreeExposuresUpdateAndDelete` 在三个入口的“update receipt was not immutable”断言失败。当前只能确认断言失败，未在本轮定位是产品回执还是比较方式的问题。

这两组失败不作为上述观察退化的根因，也不能因 Fedora 前一轮相关选集通过而忽略。完整 CI 尚未通过，当前任务只记录这项新增事实。

## 清理与证据

三个隔离 PG 容器及其数据库已由测试工具清理，并逐个复核不存在，见[回执](controlled-cleanup.json)。集群 Watch 已停止，临时诊断与反事实源码仅保留在明确命名的实验目录，产品工作树没有调试日志。证据归档包含两轮只读采集、三次受控运行、源码差异和 CI 原始失败日志；不包含凭据、kubeconfig、数据库 dump 或二进制。

[完整证据包](evidence.tar.gz)以[逐文件哈希及归档哈希](evidence-manifest.json)核验；未压缩明文经 Gitleaks 扫描通过，[结果](export-scan.json)为空，无检测规则豁免。本轮只有正式记录及当前执行状态新增，未提交/推送；产品 HEAD 保持 `83a4346`。
