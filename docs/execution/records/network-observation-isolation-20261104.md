# 网络观测故障隔离与终态恢复（2026-11-04）

基线为本次实际拉取的 main `d23828c95a634bab4cd979eb14d896e84830f283`，分支 `codex/network-observation-isolation-20261104`。修复和发布范围仅 Resource；补丁不增加迁移文件，后续授权的 10–12 部署、既有迁移前置及真实业务验收见下文。

## 业务结果

真实 PG/worker 回归覆盖：正常创建 VPC/EIP/SNAT/Subnet → 暂停共享观察 → EIP 读取失败、VPC 聚合 degraded 且自身证据新鲜 → VNIC 创建仍按当前原 VPC/Subnet 受理，且不增加全量 LIST。VM 预检/Prepare、已有手动 VNIC 预留也在相同聚合异常下受理。UID 替换、删除、未决写入、配置未就绪继续拒绝；定向核验不刷新投影。

Released 协议阻塞记录消费晚于持久异常的共享证据，原封闭身份、revision、摘要、finalization 和实际无残留均满足才恢复；初次释放、迟到孤儿和活动删除的独立证明保留。Deleted LB 复用有效完整审计。SNAT 失败读取/门控保留已确认 applied 事实，基础父子证据恢复只在新鲜度跃迁时唤醒。可选 LB 读取和来源断点不再丢弃/失效完整基础证据，不完整 LB 索引不能证明清理完成。

## 验证

重任务均在 Fedora 独占目录 `/home/chabking/tasks/network-observation-fix-20261104/candidate`，沿用 `net05a-heavy.lock`，Go 1.26.10、sqlc v1.31.1、Buf 1.60.0。源文件清单和 SHA-256 校验后传输；sqlc 生成物和 gofmt 结果回传前检查本地文件未漂移。

| 检查 | 结果 |
|---|---|
| main + 新增回归的基线运行 | FAIL：VNIC 创建/预留被聚合阻塞，SNAT 事实被清空，原 VPC 就绪证据被丢弃，子项恢复未通知父级，可选 LB 超时丢弃基础证据 |
| `make sql` | PASS，远程固定 sqlc 生成，无 migration |
| `make verify` | PASS，exit 0，448.48s；生成一致性、分层、故障构建、tidy、全仓单测、vet、构建、模块校验均执行 |
| 相关 PG/领域回归 | PASS，36 个顶层测试、75 条含子项 PASS；network 包业务运行 47.04s，完整命令 141.54s |
| 对应 race 回归 | PASS，同组 36 个顶层测试，exit 0，193.86s，无 DATA RACE；临时容器已移除 |
| 10–12 集群部署/真实 VM 与 VNIC 验收、请求量验证 | 见下文后续发布验收；长期稳定性未验证 |

定向 PG 和 race 复用同一组选择器，涵盖新用例及既有 Namespace/UID、墓碑、迟到依赖、owner 关闭后采集、VM/VMI 证明和手动 VNIC 留存/删除用例。核心可复现命令：

```bash
scripts/integration ./internal/data/network ./internal/biz/network \
  -run '^(TestVNic(AdmissionPG|ReservationPG)|TestVMNetworkPreflightPG|TestBaseObservation|TestOptionalLB|TestReleasedProtocol|TestDeletedLoadBalancerReuses)' \
  -timeout 240s -v
```

完整命令与原始回执在 Fedora 任务目录的 `baseline-result.json`、`generate-result.json`、`verify-result.json`、`targeted-result.json`、`race-result.json` 和对应日志。基线/中间失败原样保留；测试夹具的 consumer_type、pending_since 和 VMPrimary 按当前合同补齐，未删除负向断言。`make verify` 有非致命 shell 初始化和旧模块元数据缓存权限警告，最终退出码为 0；没有更改全局环境掩盖它们。

`make verify` 完成于最后两处回归夹具修正及纵向用例补充前，所有生产源码及生成物与最终验证输入相同；随后修改的测试由最终 PG 和 race 命令重新编译、执行。未因测试记录/文档补充重复全仓构建。

数据库使用 `scripts/integration` 的独占临时 PG 容器和逐测试数据库，仅 KCN 外部 HTTP 边界受控。已完成命令的清理回执记录临时容器移除。受控回归不等于现场 KCN 连通、guest 启动或线上预算已验证，也不认定此前 API Server/etcd 读超时已解决。

## 10–12 Resource 发布及业务验收

目标为 `172.16.101.10–12`，kube-system UID `5277649d-e28d-4a0a-ae76-8354365c63bc`。首次发布 `669a4cd`；修复自动网卡 VM 删除收尾后发布 `86f88a0`；该版稳态验收又捕获共享观察等待的错误退避，随后发布 `99a03a172d14cfc69af1a6ce776461f26337f218`。最终版本 `network-obs-20261104-99a03a1`，镜像：

```text
172.16.101.10:30003/icn-01a15337-release/ani-resource-service@sha256:10ae12e3151dfeb2198663497e79aa22defef0267f7b804bff28ef1930c0a800
```

462 个生产输入与提交逐项一致，快照 `sha256:a99cd65fd15b05a81a8cbbc616a0527d12cb747eb105f7cc5af3dcd299d01ae7`；固定 builder/runtime 镜像，Fedora 构建 exit 0 / 62.50s，Harbor CA 校验的推送及 digest 回读通过。Deployment 原 UID 保持，generation/observed 为 92，1/1 Ready、restart 0；三次 JSON Patch 均只替换 server 镜像，并校验其余 spec 完全相同。没有发布 Governance、Console、Sandbox 或 KCN。

### 既有 main 的数据库前置

现场 Network 原迁移账本为旧 28–30 编号，直接发布最新 main 会被严格 schema 检查拒绝。先在数据库节点执行独立 `pg_dump -Fc`，以 `pg_restore --list` 核验并取回私密目录，备份 7,316,980 字节、SHA-256 `ba09e11141b9aa7e068ef25dcf445bac94570fb0a072d300292fafe0bcebd579`。仅执行 main 已有的 Network owner 迁移入口：受保护地识别旧账本、补 consumer_type，并规范为 1–31；所有 31 条 checksum 与本次源码核对通过。首个 Job 在 ani-03 的 ContainerCreating 阶段超时，未运行 SQL；复用 Resource 原有 ani-02 调度及拉取配置后 Job 一次成功，exit 0。两个任务自有 Job 均已删除。

未迁移其他模块、未修改运行配置或 RBAC。该前置没有新增业务表回填或人工清除围栏；原始备份和 owner/runtime 身份仅保存在 Fedora 私密目录。镜像回滚需选择兼容 Network 31 的版本，发布前的旧 `85534215…` 镜像不能直接使用当前账本。

### 实际主流程及补充修复

通过现有 Governance 正式接口提交幂等请求，观测原操作收敛，并核对 KubeVirt/KCN 的原 UID 与实际清理。专用 VM 使用已登记 Ubuntu BIOS 磁盘和现有规格，不修改用户 VM 或安全组。以下先保留前两版的实际结果，最终版结果独立记录。

| 检查 | 实际结果 |
|---|---|
| 初版 `669a4cd` 的 VM 两轮启动/普通关机 | PASS；启动 22.411s / 21.690s，关机 38.321s / 51.185s；两次 VMI UID 不同，关机后 launcher、自动 VNIC/IP 退出 |
| 手动 VNIC 创建/实际分配/删除 | PASS；`vnic_f4e54e8d8cf9431d8b00a24fa5cedeb6` 原 UID `349adc38-d989-46ab-a6df-e1d783267d8f`，实际 IP `10.75.5.1`、MAC `fa:00:fe:60:49:a6`；原 UID 及子 IP 删除确认 |
| 原 VM 删除收尾 | 初次 FAIL：原请求运行但反复 `INVALID_ARGUMENT: invalid applied VNIC revision`；修复后同一操作 `d4ac38b9-a4a7-428c-b00b-cff8a4239f0b` 自然 succeeded，没有重发 DELETE |
| 第二版 `86f88a0` 的独立 VM 启动→普通关机→删除 | PASS；VM `8b237e58-320e-4490-b334-2fafe37159d4`，原始单次操作分别 29.505s / 48.041s / 19.053s，VMI Running 及原生/产品地址 `10.75.0.19` 一致 |
| 物理清理与保护对象 | PASS；两个测试 VM 及原 UID 的 VMI/launcher/DV/PVC/PV/VNIC/IP 均不存在；最终 attachment released、reason 空、protocol_blocked=false、next_check_at=infinity；用户原 VM 始终 Running/version 4，VM/VMI/launcher UID 保持 |
| guest 启动/登录 | NOT_VERIFIED；QGA NOT_CONNECTED，未安装串口工具或修改客体 |

时间使用同一 Fedora 时钟计算请求到最终原生证据读回，包含读回耗时。中间 `VM_EXTERNAL_RESULT_UNKNOWN`、`ATTACHMENT_CONFLICT` 和版本冲突历史保留，均由同一操作自然收敛；不把 HTTP ACK、Ready 或临时状态代替最终业务结果。

收尾缺陷位于 `internal/data/vm/network_interfaces.go`：没有持久化 primary 配置的普通自动 VM，被默认配置代入 `ApplyVNicBinding("", 0)`，真实领域校验必然拒绝。现在只确认实际保存的 primary 配置，仍调用 ReleaseVNicBindings 释放消费者关系；显式自动/手工配置的非法版本继续拒绝。回归使用真实 Network 领域校验，只替换持久化边界：旧源码 9 个子例中 3 个失败，补丁后 data/vm + biz/vm 全部通过；随后 Fedora `make verify` exit 0 / 261.32s，包含新增测试。日志中的既有 shell 初始化及缓存写权限警告未影响退出码，未更改全局环境。

已释放且 protocol_blocked 的记录从发布前 9 条降至 1 条。剩余记录属于另一租户的历史 VM，虽然 owner closed/deleted，但本轮没有取得其当前 Namespace/原 UID、封闭快照摘要及 provider 无残留的完整证明，因此保留围栏和有界重检，不声称已解封。

### 第二版稳态发现的真实残留问题

`86f88a0` 的 304.81s 稳态窗口发出 1,831 次 GET/LIST（6.007 QPS），14 类基础 LIST 各增加 11，观察到 10 次审计证据更新、相邻更新平均约 29.97s，audit/optional audit failure 均为 0，最大证据年龄 37.45s。采样和传输预算恢复，但 **基础连接健康验收 FAIL**：30 个 DB 样本中存在 VPC/base 的 `degraded / BASE_CONNECTIVITY_NOT_READY`，随后恢复，原始失败窗口保留在 `final-86f88a0/`，没有用后续成功覆盖。

目标 EIP 的两次、SNAT 的三次实际 WARN 均为 `shared observation is pending`，不是 KCN 请求报错。缓存门槛是 AuditInterval+AuditJitter，采集时间取 LIST 前，而下一轮周期从上一轮采集、唤醒和遥测结束后计时，因此正常采集也可能短暂越界。普通巡检误装最长约 60s 的指数故障退避，新共享证明不能及时唤醒应用，最后确认的 SNAT 证据超时后触发基础聚合降级。最终原 EIP/SNAT UID 保持、phase=Bound；此前未连续保存 KCN 状态，不能据此断言整个历史窗口都未变化。

`99a03a1` 将本地共享观察等待标成独立错误类型。仅对普通 Available/Degraded、无 active operation/pending mutation 的巡检保留常规 deadline，不安装指数失败退避，使下一轮成功审计可以唤醒应用。没有增大缓存 TTL、改采集频率或 QPS，没有续写证据时间；真实请求失败、创建、活动变更和删除继续原退避与证明。该边界写入 `cr-observation` 规范。

远程行为 red 在旧 `86f88a0` 源码中仅补新 sentinel 声明以编译，未改调度/适配行为：4 个普通 EIP/SNAT 子例出现 53–58s 退避，缓存等待和无效审计分类也失败；原始 red 日志保留。补丁后 biz/network + data/network 全包单测通过，3 个新增顶层用例及 15 个子项覆盖真正请求失败、相同错误字符串、未决写、活动操作、墓碑与证据时间保持；随后 Fedora `make verify` exit 0 / 261.49s。新改动没有新增 PG 测试设施，调度落库和真实业务由后续现场复验确认。

日志 `PROVIDER_UNAVAILABLE` 的 847 次词频不代表 847 次外部失败：对应 801 条记录，其中实际 WARN 707 条，大部分为本地共享视图等待/失效，另有 INFO 重复携带 reason。仅一次 `context deadline exceeded` 位于启动时可选 LB capability 的 500ms DB 持久化，发生于稳态之前，不是 KCN GET/LIST 超时。分类回执为 `validation/resource-log-classification.json`。

### 最终 `99a03a1` 复验与边界

| 检查 | 最终结果 |
|---|---|
| VM 普通平台启动→关机→删除 | PASS；VM `136aeb2f-70de-4b14-8f72-a002a706c38e`，单次操作分别 18.174s / 28.939s / 15.659s；原生 Running、产品/实际地址 `10.75.0.20` 一致，关机后 VMI/launcher/自动网卡/IP 退出 |
| VNIC 创建→实际分配→删除 | PASS；实际 IP `10.75.5.2`，产品地址/MAC 与原生一致；原 VNIC 及子 IP UID 删除确认 |
| 物理清理与原 VM 保护 | PASS；本轮 VM/DV/PVC/PV 和网络子资源均不存在，原 PV UID `2f3f7c79…` 已消失；attachment released、reason 空、protocol_blocked=false、next_check_at=infinity；用户原 VM 仍 Running/version 4，VM/VMI/launcher UID 不变 |
| 原 EIP/SNAT 精确 Watch | PASS；Fedora UTC 02:04:59.95–02:25:00.21，从初始精确 GET 的 RV 连续 Watch，两个原 UID 均 Bound、0 事件/0 status 变化，正常 exit 0，无遗留；该范围完整覆盖选定稳态窗口 |
| 303.35s 静稳请求量与基础采集 | PASS；3,099 次 GET/LIST，10.216 QPS；14 类基础 LIST 各 +9，观察到 9 次完整证据更新、相邻更新平均约 31.06s；基础 audit failure=0、采样错误=0，最大 audit 年龄 39.30s |
| 聚合状态与应用事实 | 29 个 DB 样本 VPC/base 全为 available/ready，SNAT applied_enabled 全为 true；没有再观察到上一版的状态降级 |
| 所有网络观测健康 | 未全部通过：可选 LB audit failure +1，未重验 LB 业务；base 聚合时间最大年龄 75.49s，而 VPC/EIP/SNAT 各自最大约 45.37s，存在聚合时间应用滞后，见 [Issue #55](https://github.com/zhangzhe-ctrl/ani-resource-service/issues/55) |
| 客体启动/登录、客体内部关机 | NOT_VERIFIED；QGA NOT_CONNECTED，仅做一次读回，不安装工具、不修改 guest/SG |

静稳数据复用实际等待鉴权续期的间隙：Stopped VM 创建回执 Fedora 02:06:19.97；DV 原生 Bound 时间 02:05:20Z，配对 DB.now 的实测时差约 66s，对应 Fedora 02:06:26；唯一 Start 回执 02:12:29.81。选取 Fedora 02:06:42.00–02:11:45.35 的首段连续超过 300s 数据，位于磁盘就绪之后、Start 之前，窗口中没有本任务业务变更。原始样本 phase 仍为 functional，派生 `quiescent-window-result.json` 明确写明选取依据；未篡改原 phase 或用其他版本计数。结束后停止采样，不再额外等待一轮。传输计数合并两个 Network dynamic client，不等同单一 limiter；不同历史窗口不构成相同负载的严格 A/B 对照。

本轮原始失败也保留：启动前 GET 因 access token 过期返回 401，经既有 refresh/csrf 均 200 后确认仍 Stopped，再提交唯一 Start；一次 SSH 包装重定向失败发生于 Stop POST 之前，修正后只提交一次；最终只读 probe 的旧“Watch 必须仍 active”断言在 Watch 正常 20min 结束后失败，移除该验收工具断言后完成必要清理/保护核对。这些是鉴权和验收工具问题，没有当作 Resource 操作失败或重放已受理业务。

最终 Resource generation/observed 92、1/1 Ready、restart 0，镜像/生产源码与 Fedora verify 输入一致。最终日志未发现 limiter Wait 或 context deadline exceeded；仍有本地待覆盖的观察 reason，不能按词频解释为底层断网。最后核对原 VPC/EIP/SNAT UID 保持，EIP/SNAT Bound，均无 pending mutation，VPC available/base ready。

68 个 Deployment 的前后比较中，Resource 由 generation 89→92、仅镜像变化；另观测到 Governance 169→170 的并行 rollout restart（restartedAt=01:51:11Z，镜像 digest 完全相同）。本任务未提交 Governance 的发布或重启操作，也未回退该并行变更；没有将全局差异误记为“只有 Resource 发生过变化”。任务自有迁移 Job、采样和两个临时 Watch 均已结束；镜像/工具缓存、原有工作负载保留。

最终证据根目录为 Fedora `/home/chabking/tasks/network-observation-deploy-20261104/`：`rollout-final.json`、`build/final-99a03a1/result.json`、`pending-*-result.json`、`validation/functional-done.json`、`validation/round3/` 和 `final-99a03a1/`。私密备份、会话和原始完整日志留在任务私密目录，不入仓。
