# LB 审计恢复及 CI 修正（2026-09-18）

2026-11-09 引用维护：本记录保留原时点的实施与验收结论。标为“历史定位”或“仓外定位”的原路径在当前 checkout 无法解析，仅保留追溯信息，证据可用性为 not_verified；不表示归档已恢复或旧规则仍适用。处理范围见[文档修复记录](../../res-doc-fix-20261109.md)。

用户在[具体失败分支诊断](../lb-branch-20260917/README.md)后要求正式修复，并确认两组 CI 失败是否需要改代码。本轮以 `83a4346d05069fb957f9ff6096c00dd586504e62` 为产品输入，沿用此前直接提交、推送原分支的授权。本地仅编辑、检查与 Git；编译、测试、生成检查均在 Fedora 隔离目录执行。

## 修改与原因

### LB 产品代码

连续 Watch 续接使完整审计的来源代次失效，旧 LB adapter 只允许一次刷新，仍有约 27 秒请求预算时便返回 `ProviderTemporary`。worker 将实际观察失败落库为 degraded/unknown，并非 scheduler 没有运行，也不证明业务流量中断。

ObserveLoadBalancer（历史定位：`../../../../../internal/data/kc_load_balancer.go`）现在用迭代循环处理关系核验前、核验后的两处审计失效。在原调用截止时间内重新解析依赖、推进数据库采集边界、读取完整审计。只有内部审计失效信号触发重采集；读取错误与身份冲突保持原处理。取消/超时返回空证明；无截止时间的调用只允许一次刷新回退。没有延长 worker 超时、修改持久状态规则或依赖旧证据维持 configured。

权威观察规格（旧锚点 `../../../../specs/cr-observation.md#4-事实时效与真实校验` 已被替代；现行规则见[网络状态消费规格](../../../../specs/cr-observation.md)）同步替换了 LB 单次刷新规则。身份、通知代次、时效、生成对象归属与释放证明保持。

### 两组 CI 失败

- **健康检查端口：测试代码遗漏。** 进程恢复测试的 Create、Update、删除期间拒绝 Update 均补 `health_check.port=8080`，与已有 backend port 一致。产品必填校验正确，未放宽。
- **更新回执重放：测试比较错误。** 旧代码在 Fedora 默认时区通过，`TZ=UTC` 则三个入口全部复现。定点逐字段检查发现所有差异都是同一时刻的 `time.Local` 与 JSON 重放 `time.UTC` 内部表示；所有 `Time.Equal` 均 true，没有业务字段差异。测试改为比较完整 JSON 回执，仍覆盖所有持久回执字段，不忽略时间、健康端口或状态。产品幂等实现未改。

临时逐字段日志仅存在于 `receipt-source` 隔离诊断副本，产品源及提交测试没有此探针。

## 验证

最终结果见 [门禁与清理](gates.json)。正式回归 lb_audit_retry_integration_test.go（历史定位：`../../../../../internal/data/lb_audit_retry_integration_test.go`）在真实 PG、实际 adapter、worker 和查询链上完成；controlled Kubernetes/controller 只提供 Provider 侧事实。

- 原代码：private 与 public_private 都在同一 worker 调用内返回 unknown/PROVIDER_UNAVAILABLE；分别仍余 27.275/27.350 秒。故障窗口前后 Provider 对象摘要相同，实测两次 HTTP Watch 续接。
- 修正后：private / public_private 分别在 5.519 / 5.454 秒内通过，同一 worker 调用后 configured/fresh，Provider 对象摘要不变；持续失效到 deadline、无 deadline 的有限回退、403 不重试、取消无证明全部通过。
- **pass**：27 项顶层测试、54 项子测试，0 fail，实际耗时 400.282 秒。回归选集包含所有 `TestLB*`，以及 Egress/Attachment 连续 Watch 重采集回归，使用 race detector 和 UTC 环境。既有 UID 替换、外来生成资源、后端撤回、删除释放和持久恢复验证继续执行。
- **pass**：`make verify` 检查固定生成物、分层、Go 依赖、测试、vet、build 与 diff。

Fedora run：`/home/chabking/workspace/ani-network-service-runs/lb-fix-20260918T0011`。固定 Go 实际版本随日志留存；重任务共用 flock，`GOMAXPROCS=2`、`GOFLAGS=-p=2`、CPUQuota=200%、MemoryMax=2300M、MemorySwapMax=0；每次 PG 768 MiB/1 CPU、随机 loopback 端口，测试数据库独立。完整输入文件按哈希验证后解包，gofmt 仅在 Fedora 执行并回传；有效源码清单与最终本地源核对。修复记录及执行状态在结果形成后追加，运行清单描述实际受测快照，不将后续文档变更冒充受测源码。

```sh
# 资源受限的 Fedora scope 内执行；test.sh 内部调用 scripts/integration。
bash test.sh receipt-red receipt-source '^TestLBActualAdapterThreeExposuresUpdateAndDelete$/private$'
TZ=UTC bash test.sh receipt-utc-red receipt-source '^TestLBActualAdapterThreeExposuresUpdateAndDelete$'
bash test.sh retry-red red-source '^TestLBFinishesFreshAuditAfterConsecutiveWatchBoundaries$'
TZ=UTC bash test.sh green-race green-source '^Test(LB|EgressFinishesFreshAudit|AttachmentFinishesFreshAudit)' -race
bash verify.sh
```

第一条 Go 子测试选择也匹配了 public_private，实际执行 private/public_private 两项，以日志为准。第二条三类入口 red，第三条两类入口 red 都是命中预期问题，不能记为产品通过。

## 边界与交付

本轮没有操作 Kubernetes 集群，没有删除/重建已有 Public Subnet，没有重启共享控制器。测试只清理自己创建的 PG 容器/数据库，诊断源码、日志和源哈希保留。

这里证明的是已定位失效路径的正式修复与回归；没有重新执行真实双入口 LB 的产品观察稳定窗口，也没有将历史所有 `PROVIDER_NOT_READY`/stale 追认为同因。手工 Public 场景不被收养为产品资源，kcn 流量问题仍按用户决定独立跟进。整个 NET-VPC-LB-02 的范围与验收状态继续看[当前执行状态](../../../status.md)。

原始日志、诊断差异与哈希清单放在 [证据包](evidence.tar.gz)，未压缩内容先经过 Gitleaks 扫描；[归档清单](evidence-manifest.json)记录哈希。提交、远端 ref 与 CI 结论以最终发布回执为准。

## CI 整包预算补正

首次修复提交 `a7ff14462cc12f6712c670884db4d0a5fbe824a2` 的 [CI 35246326927](https://github.com/zhangzhe-ctrl/ani-network-service/actions/runs/35246326927)中 Verify、完整 PostgreSQL/进程恢复均 pass；旧的两组失败已消失。全仓 race 在 `internal/data` 包累计 600.063 秒时被 Go 默认 10 分钟 alarm 终止，正在执行的 `TestSubnetStaleParentRejectsNewIntentButNotPersistentReplay` 仅运行 0 秒、处于新数据库初始化。日志没有数据竞争报告或测试断言失败；这是整包预算耗尽，不能记录为 race pass。

[Makefile](../../../../../Makefile)为 `make integration` / `make race` 显式增加 `INTEGRATION_TIMEOUT ?= 20m`。保持全部测试、race 检测、每个业务操作的超时及断言，预算仍有上界并允许调用者覆盖。仅该测试配置变化，产品代码保持已验证版本。Fedora 独立 `budget-source` 再执行 `make verify`；`make -n integration race`确认参数传入。相关 [CI 预算证据](ci-budget.json)固定失败日志、有效 Makefile 哈希和检查结果；后续精确提交 CI 结论以最终发布回执为准。
