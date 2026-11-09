# VPC 基础连接：独立 Watch 对照（2026-09-17）

2026-11-09 引用维护：本记录保留原时点的实施与验收结论。标为“历史定位”或“仓外定位”的原路径在当前 checkout 无法解析，仅保留追溯信息，证据可用性为 not_verified；不表示归档已恢复或旧规则仍适用。处理范围见[文档修复记录](../../res-doc-fix-20261109.md)。

用户要求持续 Watch CR，并与 Network 的状态判定对照。本次采用 `diagnosing-bugs` 的先捕获、再定位流程。仅诊断，不改变产品状态规则、放宽时效检查或直接改数据库。

## 固定输入与范围

- 产品源码为已发布 `f07178a6bd2100ae6fb9583a25de06703e586c94`；复用健康检查端口闭环已经验证的二进制，Network SHA-256 `17564f57def31e305b1951be3d21c17babdeaec7e82ff8de12be0d1b7f8a5993`，API 驱动 SHA-256 `694b5f80db05672e463ebc6c4322517d5e98a8441f8792c53aef1c3cda37eb50`。
- 执行主机 `fedora`，目录 `/home/chabking/workspace/ani-network-service-runs/base-watch-20260917T1405`；真实集群 `ani-test-1`，UID `be57b911-892c-4e75-aa9d-4a05d819c59e`。
- 独立数据库 `net_vpc_lb_02_b91701`、管理 namespace `basewatch-0917`、租户 namespace `bw-d01b8dee-3371-4053-b0df-bdad82fba4bc`。新 Intranet 池 `10.242.250.0/24`，VPC `10.236.0.0/16`。已有 Public Subnet 与手工 Public/双入口/SNAT 现场保持。
- 初始仅创建 VPC 及产品自动管理的基础 EIP/SNAT；若最小现场稳定，再加入上次场景的子网操作，每次记录发生时间。
- Watch 在目标 VPC 创建前启动。首轮在第一次 ready 后连续采集 600 秒，最长 1800 秒、150 MB；结束后保留原始结果。没有把“持续”实现为无人管理的永久后台进程。

## 独立采集与判定

[采集器](collect.py)不复用产品缓存，不写产品状态：

1. 使用五条独立 `kubectl get --watch --output-watch-events -o json`，覆盖租户 VPC/EIP/Snat，以及 kcn-system 的默认 VPC、Subnet。记录事件、对象 UID/resourceVersion/generation/spec/status、本机 UTC 和 monotonic 时间。
2. 每秒只读采集隔离 PG 的 VPC/base/EIP/SNAT、binding、租约/调度、operation 与最近历史，并调用真实 `GetVPC`。
3. 数据库状态转换时，对 VPC/EIP/Snat 发起一次独立直接 GET；每 5 秒采集既有观察/worker 指标。工作进程原有日志保留。
4. 记录 Watch 开始/结束/缺口。存在缺口的区间不能当作连续 CR 证据；resourceVersion 仅标识变化，不比较其数值顺序。

[对照程序](compare.py)的命令为 `python3 compare.py watch-capture`。在曾经 ready 后出现 degraded，且同期独立 Watch 中三种 CR 基础就绪条件成立、绑定 UID 相符、没有采集断流时返回 exit 2，写入具体数据库时间、CR 状态及三类观测年龄。没有捕获时记录 `not_captured`，不代表问题已修复。该信号只证明三类 CR 与产品判定有差异，不代替全部适配器依赖检查或实际流量证明。

## 准备阶段记录

首次 Intranet 资格检查早于探测 Pod Running，明确失败且未写入验证回执。随后保存失败记录，待 Pod 就绪重新执行真实资格检查并通过。一次串联命令的前置失败未阻止后续 VPC 创建，形成一个未启用基础连接的预备 VPC；它已通过产品 Delete 删除。正式采集窗口重启于启用基础连接之后、目标 VPC 创建之前；预备调用与采集分别保存在远端 `product-driver-before-rollout`、`watch-preparation`，不混入正式结果。

正式目标 VPC 为 `vpc_f1b4df0c3bac4ba1b5f33467ff3ff383`，14:12:17 UTC 创建，14:13:01 UTC 首次在 PG 采样中 ready。14:15:49 尝试进入子网阶段时，驱动读取基础连接已经 degraded，提前退出 3，**没有发出 CreateSubnet**；阶段标记表示尝试，不表示创建成功。正式窗口只有 VPC 和它自动管理的 EIP/SNAT。

## 实际结果：捕获五次状态差异

窗口为 14:12:15–14:23:01 UTC，其中首次 ready 后连续 600 秒。五条独立 Watch 各建立一次，无断流、无自动重连；停止时的 exit 1 是采集器主动结束子进程，终止回执保留每条流尾部未解析的 1 byte。采集错误与 API 调用错误均为 0。

- PG：pending 43、ready 442、degraded 159 个样本。
- GetVPC：pending 43、ready 441、degraded 160 个样本。API 与 PG 是先后读取，不是原子快照；边界相差一个样本。
- 首次 ready 后，VPC/EIP/Snat 三个 CR 的 UID、resourceVersion、generation 与就绪状态保持；转换时的独立 GET 也一致。
- 结果是 `divergence_captured`，对照程序 exit 2；这是预期的故障捕获信号，不是采集失败。观察稳定性 `fail`，本次 Watch 对照采集 `pass`，实际流量与修复效果 `not_verified`。

| degraded 开始 UTC | 恢复 UTC | 按每秒采样估计的持续时间 |
| --- | --- | --- |
| 14:14:51 | 14:15:22 | 31 秒 |
| 14:15:29 | 14:15:59 | 30 秒 |
| 14:16:51 | 14:17:21 | 30 秒 |
| 14:17:36 | 14:18:14 | 38 秒 |
| 14:21:19 | 14:21:49 | 30 秒 |

原始事件、逐秒 PG/API 和指标保存在 [capture.tar.gz](capture.tar.gz)；[机器判定](assessment.json)、[首次/末次差异](comparison.json)、[转换时直接 GET](direct-summary.json)和[指标时间线](metrics-summary.json)可直接阅读。原始包及来源文件哈希见 [capture-manifest.json](capture-manifest.json)。解压后在该目录运行 `python3 compare.py watch-capture`，即可离线重放判定，预期 exit 2；不需要集群权限。

## 已确认的触发链与尚未确认的分支

第一段退化的直接原因已经定位：

1. 14:14:50.624 UTC，SNAT worker 记录 `PROVIDER_UNAVAILABLE`；随后 EIP 也记录同类结果。CR 同期没有更新。
2. 观察失败没有新的应用事实。资源写入（历史定位：`../../../../../internal/data/resource_work.go`）生成空 `AppliedEnabled`，AdvanceSnat（历史定位：`../../../../../internal/data/queries/egress.sql`）将 `applied_enabled` 写为 NULL，同时保留旧 `observed_at`。
3. 14:14:51 的数据库样本中，VPC Provider 观测年龄约 0.42 秒，EIP/SNAT 约 13.11 秒，均未超过 60 秒；EIP/SNAT 自身状态仍为 available，但 SNAT 的 `applied_enabled=NULL`。
4. 基础连接聚合（历史定位：`../../../../../internal/data/base_connectivity.go`）要求 SNAT 应用事实明确为 true，因此将原 ready 改为 degraded。后台退避约 30 秒后，成功观察恢复 true，父 VPC 随后恢复 ready。这里的 NULL 表示未知，不等于 Provider 中 SNAT 被禁用。

这证明本次退化发生在 Network 的观察/状态投影链路；不能解释为“三个 CR 变为未就绪”，也不能由此声称业务流量曾中断。失败时清除应用事实本身是一种保守处理，不能仅凭 NULL 就断定应当保留旧值或忽略观察失败。

排查前列出的三类假设与当前证据：

| 假设 | 当前证据与结论 |
| --- | --- |
| Watch 来源变化使审计视图失效 | 产品 `source_boundaries_total` 从 24 增至 53；`watch_errors_total` 与 `audit_failures_total` 始终为 0。源码在每次新 Watch 请求时推进全局 epoch，旧审计视图会失效。与失败时间相邻，是优先方向；尚未捕获失败调用的具体返回分支。 |
| 实际读取 CR/依赖失败 | 独立 Watch/转换时 GET 均正常，三个目标 CR 未变；但独立客户端不能排除产品连接或其他依赖读失败。现有 worker 日志未保留原错误，不能完全排除。 |
| 状态写入意外丢字段 | 空应用事实到 NULL 的转换与源码一致，未见存储随机丢失。已确认的是临时观察失败触发保守状态更新，而非已证实数据库错误。 |

Egress 采集（历史定位：`../../../../../internal/data/kc_egress.go`）发现旧视图失效后只重新采集一次；关键路径再失效会返回 `ProviderTemporary`。观察源（历史定位：`../../../../../internal/data/kc_observation.go`）使用全局来源代次。下一步有辨识度的探针应只记录失败调用的 `invalid_view` / `proof_not_covering` / 读错误分支、前后 epoch 与调用期限，再验证修正；无须继续反复创建 LB 或排查本次未使用的 Public 网络。

另一个日志限制：worker 回调接收原始 progress，基础连接聚合在持久化内部修改其副本，因此 VPC 日志可仍写 `resource_state=available`，而真实 PG/API 已为 degraded。本次以 PG、API 和直接 CR 三方证据为准。

## 收尾

采集器及五条 Watch 已按窗口自动停止；VPC、基础 EIP/SNAT、实验 Intranet 池均经产品流程删除，两个资格检查 Pod、支持 namespace/RBAC、运行进程和本任务 PG 容器已清理。私有数据库备份留在 Fedora，不进入证据包。[清理回执](cleanup.json)保存结果和备份哈希。已有 Public Subnet UID `39619c25-703e-4db2-a35a-d44ee2acb780`、generation 1 保持。

证据导出经过 Gitleaks 扫描；只忽略了一个精确匹配的二进制 SHA-256 误报，见[说明](export-scan-note.json)，凭据、kubeconfig、私有配置及 PG dump 均未导出。本采集阶段没有产品代码变更或提交/推送；后续已完成[失败分支确认、修正及验证](../base-branch-20260917/README.md)，交付随该修正归档。本页保留诊断当时的判断。保留的 Public/LB/SNAT 现场不参与本次清理。
