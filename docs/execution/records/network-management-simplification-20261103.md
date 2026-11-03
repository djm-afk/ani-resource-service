# 2026-11-03 网络管理简化实施证据

范围：Resource 与 Console；不修改 KCN。完整主链为网络创建、消费者接入/流量、消费者退出与网络真实清理。方案见[实施方案](../../plans/network-management-simplification-20261103.md)，当前结果仅在[执行状态](../status.md)汇总。

## 固定来源与环境

- Resource 基线 `b35365d7e459aad55edefde4270723157c29ea26`，独立分支 `codex/network-management-simplification-20261103`。
- Console 基线 `2e6f278111bc169ed2df7f480304093a11528e0b`，独立分支 `codex/network-observation-ui-20261103`。
- Fedora 任务根 `/home/chabking/tasks/network-management-20261103`；原仓库及其他任务工作树保留。Go 1.26.10、sqlc 1.31.1、buf 1.60.0；PostgreSQL 测试使用脚本独占临时容器。
- 10 集群 kube-system UID `5277649d-e28d-4a0a-ae76-8354365c63bc`，12 集群 `3eeb5379-8748-4bf8-8f6d-d7a48ce3bc24`。12 集群经本机 SSH 转发，驱动/测试仍在 Fedora；登录使用正常租户/平台 API，凭据只存任务 private 目录。

## 修复前事实

本次诊断的 300 秒固定窗口：12 集群 GET/LIST 6000 次（3691/2309），10 集群 4728 次（2241/2487）；两环境分别比较，不相互当作基线。12 集群 11 条 Deleted 仍调度，另 6 条未发送且封闭的取消已退休；还存在 4 条平台 Deleted 和 24 条 released Attachment 周期调度。两个历史 VPC 缺少 Provider UID；另一个 SNAT 有残余 RESOURCE_IN_USE，不能直接清 reason 冒充完成。

10 集群捕获同一公网 LB 原 operation 的 Observe deadline 后退化，随后原身份恢复；采样时原生 Ready/HA 保持。此证据说明 Res 观察路径可产生退化，不证明所有历史 NOT_READY 都有同一原因。#47 原 EIP 容量恢复缺少 KCN 重试，本次不修改或关闭它。

原始窗口：12 集群 `/home/kubercloud/network-issues-readonly-20261103-0145/window-48-02`；Fedora `/home/chabking/tasks/network-issues-readonly-20261103-0145/window-lab-02`。

## 可复现行为验证

- RED：`red-lb.log`，读取超时/临时失败错误地把 Available 改成 Degraded。
- PASS：`fifth-pg.log`，真实 PostgreSQL + 真实业务/Provider adapter，Kubernetes HTTP/实例 owner 外部边界受控。覆盖 VM 旧投影当前准入、同名替换 UID 拒绝、VM Attachment 退出、终态退休和原对象事件恢复、通知租约围栏、普通 LB 不启动全量采集、关键操作两次 410 relist 后仍完成当前证明、安装检查迁出及原生 False 条件仍退化。
- Console 格式化、lint、类型检查及 GitNexus 结果位于同任务根；图谱 UNKNOWN 调用点通过实际 import/路由确认，没有把零边当作无影响。
- `make verify` 第一轮因旧故障注入脚本锚点失效而 FAIL；同步更新真实审计入口的注入点后重新执行。不得将此前 PASS 冒充最终源码通过。

## 保留的必要边界

消费者关闭与实际网卡/IP 释放、SNAT 删除后的原 EIP 解绑、LB 生成对象规格/HA/身份仍保留当前确认。安装诊断独立执行 `go run ./scripts/lb-api installation --verify ...`。没有通过调整 QPS、Burst、展示 TTL 或修改 KCN 来制造收益。

真实环境业务验收、发布镜像与 300 秒后窗口在完成后追加到本记录；受控测试通过不代表真实验收完成。

## 发布前回归补充

- 并发准入曾 FAIL：12 个 Attachment 请求占满 8 连接池，持父行锁的事务再次借连接读取 Namespace 身份。事务内 Provider 读取现复用当前查询连接；`TestAttachmentPermanentIdentityTenantScopeAndParentDeletion` PASS。
- WatchList 的 `sendInitialEvents=true` 与 LIST 同属重建边界，普通 Watch 续接不失效。`watchlist-fix.log` 3 项 PASS（8.426s）；`egress-relist-final.log` PASS（4.790s）。普通观察等待共享视图，关键清理保留重连后的完整证明。
- `admission-final.log` 的当前原 CR UID/Ready/代次/未决操作拒绝、旧投影不改时间准入、离线保存/激活均 PASS；其中一项父删除并发测试在夹具创建第二个 Subnet 时因父基础连接投影未收敛 FAIL，夹具改为每次创建前驱动真实父 worker 到 Available，未修改业务父删除保护。
- `attachment-lb-final.log` PASS（105.399s）：父删除并发、后端证据年龄/未来时间/UID 围栏，以及真实双进程创建结果不明、更新响应丢失、删除响应丢失、未知创建期间删除四场景。Attachment 读取暂时失败只标记 ProviderUnavailable；LB 仍要求未过期的已登记身份与当前原生对象匹配。
- 扩大的 `network-regression-final.log` 不是 PASS：旧投影预期失败已定向修正复核，但整组在 6 分钟限时退出，未完成部分明确未验证。后续不把此日志作为完整网络集成套件通过。
- Console 提交 `939d643`，PR https://github.com/bykenkk/ani-console/pull/11；代码验证通过，未生产构建/部署。
- 12 集群 `ani_resource` 已完成 PostgreSQL custom-format 备份及服务端 TOC 检查；迁移尚未执行。这里的 network schema 25→27 属于 Res 数据库，非 KCN。

- 最终 `verify-release.log`：PASS，含生成一致性、边界、故障构建、Go 单元测试/vet/build/module 校验。重任务均在 Fedora 执行。

## R1 真实验收失败与回退

2026-11-03，`2fc544f` 镜像在 10 集群滚动更新完成，原三 LB 四入口首次 HTTP 通过，但随后 Attachment 观测时间持续老化，LB 出现 BackendIdentityMismatch 并退化。已只回退 Res 镜像至原 `sha256:4c651e6e...`，原三 LB 同 operation/版本恢复 Available；未修改 KCN，12 集群尚未迁移或更新。

受控复现发现父/平台通知可提高 Attachment 的 requested_generation，但其 Pod/NIC/IP 事实并未变化；普通路径因此一直拒绝未改变的共享视图。修正仅让关系未变、来源 epoch 有效的普通视图覆盖这种通知，不改变 collected_at；初始化、消费者关闭、相关对象变化、UID 替换仍保留当前收集与 fencing。`hint-coverage.log` 通过 11.910s，包含旧时间不续签、真实关系变更拒绝、relist 与领取/观察/完成各阶段通知围栏。

用户追加要求：12 集群的最终镜像改在 `ubuntu@100.64.128.15` 从固定源码重新构建并推送，再更新 12；Fedora 保留测试和 10 集群发布职责。

## R2 真实回归及后续收敛

`b9bd56e` 在 Fedora 和 15 分别构建，源码快照均为 `sha256:2482896381b041384b80f0b8174eacb2569b6dd408e834cde4864057cd487522`。15 已直接推送仓库，但未更新 12。10 的 300 秒窗口仍 FAIL：原 LB 发生状态退化；GET/LIST 为 5179 次，虽 LIST 降至每类约 33–34 次，普通身份读取仍过多。Attachment 能续新，但 Service 的共享 owner UID 通知仍反复唤醒它；LB/其他普通观察仍将这种未改变事实的持久通知当作需要重新采集。10 已只回退 Resource 镜像，原 LB 身份保留。原件 `live/lab/window-r2-failed/`、`r2-rollback.json`。

确定性 LB 回归 `lb-hint-red.log` FAIL、`lb-hint-green.log` PASS（29.708s）：相同对象与时间的共享证明覆盖普通通知，同时真实 Pod 变更仍拒绝旧视图、关键路径仍通过两次 relist。`attachment-hint-filter.log` PASS（9.410s）：共享 owner UID 的 Service 事件不再制造 Attachment 工作，Pod/NIC/IP/EIP 变化及租约各阶段通知保护保留。后续将同一普通观测规则用于 VPC/Subnet/Egress 和已确认删除记录；停用终态的无关平台唤醒，LB 仅绑定直接 VPC/Subnet/EIP 及自身生成对象/成员事件，去掉已不用于运行判断的底层平台依赖通知。

该现场还明确了另一条误判：`lbMemberIdentity` 将 Attachment 证据过期与实际 UID/归属不匹配都折叠成 `false`，触发 backend 撤出及 LB degraded。现区分这两种结果：当前管理身份/原生 Pod→VNic→VNicIP 地址链不符仍拒绝；身份链吻合但证明时间不足或 DB 读取失败时返回观测异常，保持最后结果与原时间，操作仍等待确认。没有延长 TTL、把旧时间改成当前时间或放行未知 backend。

R3 发布前 Fedora：`r3-related.log` PASS（38.863s），`r3-backend-observation.log` PASS（31.895s，含真实 Worker/PG 保留配置与观测时间、future/expired 证据不可采用、过期情况下真实 UID 替换仍拒绝），最后 `r3-final-verify.log` 完整 `make verify` PASS。后续真实环境结果见下节。

## R3 发布与真实环境结果

业务源码固定为 `dbe3cd514a3cedb4f6ab1474ddc6906bb0be8440`，两个构建主机使用相同源快照 `sha256:7a7c37a9a9a22e601300775fe8b6a6339900d20b2ebe8cbb7f5406ea99a24f1b`。

- 10 集群在 Fedora 构建并发布：`172.16.101.10:30003/icn-01a15337-release/ani-resource-service@sha256:fd46eaed0213d21e504d5fd4dd7c6fdb20f4ddf54316a0f1588ed0c54ace7abf`。
- 12 集群按用户要求在 `ubuntu@100.64.128.15` 重建并推送：`10.10.1.66:30003/ani-gpu-platform/ani-resource-service@sha256:bdd19ba3317f2b39859b72b9894c2513d514cc89dfb1dc2d6fae37a4e0986e93`。两个 Deployment 均只替换镜像，原 UID 保留，滚动更新完成；QPS 20、Burst 40、30 秒 audit 和 60 秒 stale 参数未调整。
- 12 的 Resource `ani_resource` Network schema 从 25 正常迁移至 27，使用正式 `-migrate` 入口；迁移文件与 R3 一致。迁移前 custom-format 备份与 TOC 已核验，备份 SHA-256 `c4229acd454f2498c80150e509ab320213bdd210361114a40d16b015683d5a69`。这是 **Resource 数据库迁移，不是 KCN schema**。回执为 `live/full/migration.json`；原 25 版镜像不能直接回滚到 27 库。
- KCN 源码仍为 `6132623baabee50f17e9e179c0f5e9c1f380db04`，工作树干净；没有修改 KCN 代码、镜像、配置或 CRD。

10 集群 R3 的 `live/lab/window-r3-stable/`：连续 300 秒、21 次指标采样、11 次投影采样，同一 Pod/container，采样全部成功。GET/LIST **1767 次，5.89 次/秒**，相对原同集群 4728 次下降 **62.6%**；基础 GVR 各 23 次 LIST。无传输错误计数增量、audit failure、watch error、queue overflow，也无窗口内 limiter/deadline 错误。此窗口仍有两个既存活动 VPC 的真实 ownership conflict 和平台 egress 观测待确认，不能称所有网络资源已健康，也不把 23 次 LIST 全部归为 30 秒定时器。

4 个 LB 的 44 个投影样本全部 Available，同 operation/配置版本；启动恢复阶段短暂 ProviderUnavailable，保留旧时间与配置，最大年龄 71.77 秒，随后正常续新。最终三条主样本 fresh/available/configured；原 **15 个 Gateway/Deployment/Pod/Service UID 全部保留**。观测前后私网、公网、双模式的 **四入口 HTTP 200 + 正确 marker 均 PASS**，没有重建 LB 或修改 backend/operation。回执 `r3-final-invariants.json`、`r3-final-lb-snapshot.json`、`r3-final-*-private-http.json` 和 `external-curl/r3-final-*.json`。本结论覆盖已复现的读失败/证明滞后误判，不追认所有历史 NOT_READY 的原因。

12 集群使用正式 tenant-a API 新建 VPC `vpc_9913cae07ff54ce5a15a26263dfbb843`、Subnet `subnet_d3f07993e4f945ea939c5d69b5fa18ac` 和 VM `7aef1e31-0cc3-44a1-a30d-0a53007a0b51`：

- VPC/Subnet Available，基础出口 Ready；VM 创建 Running，客机取得 `10.131.1.2/24`、默认网关 `10.131.1.1`，串口内真实 ping 网关 3/3 成功。
- 客机内执行正常关机，串口记录 `reboot: Power down`，Resource 独立观察为 Stopped；desiredState=Running、version=1、原创建 operation 保持。随后正式平台 Start→Running/version2、Stop→Stopped/version3、Delete→Deleted 完成。没有网络过期前置拒绝。
- 原 VM UID `1327ac91-dfdb-43d9-a688-34f264605eb2` 保留至删除。两个启动周期共捕获 12 个原生 UID：VM×1、VMI/launcher Pod/VNic/VNicIP 各×2、DV/PVC/PV 各×1；删除后逐项 GET 均 absent。
- 正常删除 Subnet 和 VPC 完成；原 VPC/Subnet、基础 EIP `eip_2e8b9ec814be40d291baaf4f82e906bb`、SNAT `snat_42542a137e724234b98473b4f0f89907` 全部 absent，对应网卡/IP/NAT/Service 引用为空。共享 NAD `ani-vm-primary-v1` 原 UID 保留。该 VM Attachment `att_126daeb7b45c4f23819d50461e0106a8` 为 released、reason 为空、`next_check_at=infinity`、无 protocol block。

R3 业务回执位于 `live/full/r3-business/`：`vm-flow.json`、`vm-first-running.json`、`guest-console-probe.json`、`guest-console-shutdown-final.json`、`vm-guest-stopped.json`、`vm-after-{start,stop,delete}.json`、`vm-native-cleanup.json`、`network-native-cleanup.json`。HTTP 400 的原始驱动失败保留：VM 幂等键要求 UUID、SSH 公钥不能带换行、Delete 不接受 force-stop 专用确认字段；修正验收请求后复用有效幂等键，没有改产品代码。串口首轮提示/ANSI 解析错误也保留，客机 ping 的原始完成输出已重新解析，没有重复业务操作冒充首次通过。辅助 SSH 端口转发曾报 no route to host，未修复该通道；本次内网与客机关机依据实际串口结果，不把 SSH 记为 PASS。

## R4：停止 VM 的预留连接

R3 的 12 集群完整 300 秒窗口为 1606 次 GET/LIST，较基线减少 73.2%，每类基础 LIST 19 次；无传输错误/limiter/deadline。25 条 Deleted 和 25 条 released Attachment 均不再周期调度，两个缺 UID 的历史 VPC 保留 ownership conflict 且 epoch 不增长，原残留 reason 的 SNAT 经真实观察完成确认。7 个现存 VPC 均 Available、观察及时；原 5 台 VM 的 11 组采样状态/版本/operation 不变。窗口保留于 `live/full/window-r3-before-stopped-fix/`。

该窗口尚未满足成本收敛目标：既存停止 VM `e739fb1a-4921-472d-baa3-743616aa7c42` 的 `att_f07dd6c001e24641a7b7e261433c9f57` 为 Reserved、reason 为空，却仍使用独立定时器和首次接入采集分支。本轮没有操作该受保护 VM；修复仅让已接受 open/stopped 快照的 VM 预留连接按普通共享观察运行，保留占用。活动首次接入、封闭后释放和协议恢复继续当前证明。

真实 PG + Provider/Worker 回归 `r4-stopped-red2.log` 确定性 FAIL（Stopped Reserved 仍设置 timer），`r4-stopped-green.log` PASS（9.220s）：停止后停放、父通知不重采或续签旧时间、最终释放仍新采集；另覆盖 VM 重新接入/原生 UID、未登记 launcher 拒绝、封闭释放及两次 Watch relist 清理。首个 RED 夹具遇启动观察待同步，补入真实初始 audit 后才得到上述目标失败，没有更改业务断言。

R4 最终 `r4-final-verify.log` 完整 `make verify` PASS，生成一致性、边界、故障构建、单元测试/vet/build/module/diff 检查全部完成；三份 Go 文件与本地交付源的 SHA-256 一致。随后只补充文档和现场回执，不重新扩大测试范围。

R4 业务源码 `018bda711fa4114ac8b3fe761803dbf04c9465c1`，两主机源快照均为 `sha256:b6d084991511efe58b6a43177d4a3d169f3de60474326c0a97bd23804ad8d6bb`。10 镜像为 `172.16.101.10:30003/icn-01a15337-release/ani-resource-service@sha256:d55f4b9363ad807238221c037b133e16216b0764d6b8507d7b3007cae2e9cada`；12 镜像仍由 15 主机重新构建并推送，为 `10.10.1.66:30003/ani-gpu-platform/ani-resource-service@sha256:1d34ea5d18f34bc7be06934cc84c34dc4ce6d939bb5698d3ed49bb22003395e2`。两个集群均在 2026-11-03 05:19 UTC 完成仅镜像更新，无新迁移、预算/TTL 调整或 KCN 变更；`live/{lab,full}/rollout.json` 为 R4，前版回执保留为 `r3-rollout.json`。

R4 的 10 集群窗口为 1804 次 GET/LIST（较原 4728 次减少 61.84%），每类基础 LIST 26 次；4 个 LB 的 44 个样本全为 Available，窗口结束后原 15 个原生 UID、配置版本及 operation 保留，四入口 HTTP 与 marker 再次通过。启动恢复期间最大证明年龄 94.91 秒，观察异常明确显示，最终恢复 fresh；没有伪造时间。此结果不把额外 LIST 全部算作周期采集。

## R5：真实 VM 验收发现的提交边界

R4 新 VM `8e7937b1-8111-4d7c-a6e5-fac7b0e67073` 以停止态创建，其 Reserved Attachment 已停放；首次平台启动和客机网关 ping 3/3 成功，客机内关机后自动观察为 Stopped。但随后 Start `8b98ca51-04b4-49b0-b367-4cb8597f606f` 超过 360 秒仍为未知，不能把 R4 完整 VM 主链记为通过。

只读核对确认原终态 VMI/launcher 已退出且未更换；两个 virt-api 的对应时间日志只记录首次 Start，没有第二次原生请求。Resource 在 Native.Start 的前置检查前持久写入 Submitted，adapter 的启动前失败未撤销该标记，后续因旧终态 VMI 仍存在而正确触发未知重发围栏，却无法继续原操作。确定性 PG 回归 `r5-start-pg-red.log` 复现“前置 Busy 后仍有 Pending”。

修复只增加 adapter 明确的“未发送原生命令”结果，worker 收到后持久清除本次提交标记，沿原 operation/版本继续；实际 PUT 的错误/丢失响应和进程中断仍保留未知围栏，不从现状反推历史未发送。旧失败样本通过正常业务 Delete 终止并检查原生清理，不修改 DB、KubeVirt 或 KCN 来恢复其启动。

`r5-start-green.log` PASS（VM data 包 47.702s）：真实 PG 关闭/重开后原 Start 继续，已发送丢失响应/保留旧终态 VMI 仍禁止重发，原生 HTTP 失败不误报未发送；UID/namespace/template 前置拒绝及终态启动失败、未知 Restart 恢复回归通过。`r5-final-verify.log` 完整 `make verify` PASS；6 个 Go 文件的 SHA-256 在本地与 Fedora 一致。R4 失败业务记录保存于 `live/full/r4-business/`，其中 VM、盘、网卡/IP、VPC/Subnet/EIP/SNAT 均已真实删除，共享 NAD 保留、Attachment released 且停放。

R5 业务源码 `54d1a40a513001ca76eea5035c7754f8a8237e83`，两个构建主机快照一致为 `sha256:09c3b31c1a63f4e6f49fef384d9f89e36369d681c8ed0cd3c5bc3bf53c0db755`。Fedora 构建/推送 10 镜像 `172.16.101.10:30003/icn-01a15337-release/ani-resource-service@sha256:effbfd4ab96ae58fc98ca9f2a054f6d79d1f434c347272679ea17edb0a5299e9`；`ubuntu@100.64.128.15` 独立重建/推送 12 镜像 `10.10.1.66:30003/ani-gpu-platform/ani-resource-service@sha256:d7476d4b5f486b28734ed201798e235a062a6c52bbfbe7952aa466e4af3a5d50`。两处仅更新 Resource 镜像并完成 rollout；没有修改 KCN，未调整 QPS/Burst/audit/stale，未增加数据库迁移。当前回执为 `live/{lab,full}/rollout.json`，上一版为 `r4-rollout.json`。

R5 的 12 集群 VM `16fd4897-e90a-4c40-901d-e24c8c43817a`：停止态创建→首次 Start→客机网关 ping 3/3→客机正常关机→原 VM 再次 Start→平台 Stop→Delete 全部完成。创建、首次/再次启动、关机、删除 5 个 operation 均 succeeded、reason 为空，版本依次 1–5；客机关机不制造新 operation 或业务版本。原 VM UID `e2cda88c-c6b9-4190-ab25-a7801f4b2ee4` 保留至删除，两个运行周期共 12 个原生 VM/VMI/launcher/VNic/VNicIP/DV/PVC/PV UID 均逐项确认 absent。停止态创建及平台关机后的 Reserved Attachment 均 reason 为空、`next_check_at=infinity`，没有释放预留关系。

随后正常删除 VPC `vpc_19afd89a4e334bef8fafba35aa474856` 和 Subnet `subnet_01894b7907f34293b1ba0ffed8a2d45a`；它们及基础 EIP/SNAT 原对象均 absent，无残留 Subnet/VNic/VNicIP/SNAT/NAT/Service 引用，Attachment `att_9bf282df8e0e48e58a3ec23d643c9310` released 且停放；共享 NAD 原 UID 保留。对应 `live/full/r5-all-operations-final.json`、`parked-{initial,after-stop}.json`、`vm-native-cleanup.json`、`network-native-cleanup.json`，均为真实环境结果。

R5 的 10 集群最终窗口 `live/lab/window-after/`（05:36:59–05:42:00 UTC；与集群日志按数据库时钟对齐）：21 个指标/11 个投影样本、同 Pod/container、无缺样。GET/LIST **1792 次，较原 4728 次减少 62.10%**；每类基础 LIST 25 次。4 个 LB 的 44 个样本全部 Available，最大观测年龄 34.03 秒，原 operation/配置版本保留；最终三条主样本 fresh/configured，原 15 个 Gateway/Deployment/Pod/Service UID 保留、每个 Deployment 2 Ready，私网/公网/双模式四入口 HTTP 200+正确 marker 均通过。一次 `LB audit needs a fresh collection` 只留下观测异常，没有错误退化资源或撤出后端。窗口没有传输错误、audit/watch failure 或队列溢出；既存网络 ownership conflict 和平台 egress 待确认仍如实保留，不能把 25 次 LIST 都称为周期采集。最终流量/身份回执为 `r5-final-*`。

## R6：稳定交换机的普通节点读取

R5 的 12 集群 300 秒窗口为 1577 GET/LIST（较 6000 次减少 73.72%），每类基础 LIST 19 次，尚未达到主要由 30 秒共享周期驱动的要求，故 #48 未关闭。只读检查时没有活动租户网络操作，也没有有限定时器的 Attachment；现存交换机为 Available/configured。其 `InspectVirtualSwitchNode` 仍调用节点完整实时采集，节点探测 ConfigMap 更新也会失效该采集，属于普通观察遗留放大。`r6-switch-red.log` 确定性复现：仅 ConfigMap 变化便发生第二次全量 LIST，并改用新的观测时间。

只对已有 Prepared/Applied、非 Unknown 的 configured 交换机成员复用共享节点集合；节点成员/UID/归属相关变化使用独立的现有关系键失效，ConfigMap 探测更新不使这份节点身份事实失效。没有新缓存或后台任务；保留原 collected_at。新增/替换 Node、来源断点、过期视图拒绝旧证明并等待共享 audit；首次配置、未知变更和删除继续当前完整采集及原生 UID/RV fence。#49 已在 R5 的身份/实际流量复核后关闭；#47 仍开放且 KCN 未修改。

`r6-switch-green.log` PASS（data 22.077s、biz 0.008s）：普通节点事实不重采/不续签，成员新增/替换、断线、过期仍拒绝，未知节点变更使用当前完整采集；真实 PG 两阶段受理/恢复、并发 VLAN 和删除等待旧租约退出通过，原生删除 fence 拒绝延迟旧 PATCH。`r6-final-verify.log` 完整 `make verify` PASS，3 个 Go 文件本地/Fedora SHA-256 一致。R5 VM 业务原件归档至 `live/full/r5-business/`，两环境窗口归档至 `window-r5-before-switch-fix/`；10 的 R5 窗口包含启动后的终态一次性恢复，不能作为终态完全静止窗口，12 的 33 条终态则全部停放且 epoch 不变。R6 最终窗口将先等待启动恢复完成，再统计固定 300 秒。

R6 业务源码 `4a41fe95c9f15d7d6cc70a3096abbac8075b068e`，两主机源码快照均为 `sha256:3364e0eca70f5de9110566f4c10db148d3c42ac6353b4f1be5fbe66fbc238305`。10 镜像 `172.16.101.10:30003/icn-01a15337-release/ani-resource-service@sha256:92aae5fa29ca33124695fcbd624767ad9b3a632ea42fe8947cb57bd64b590f28` 由 Fedora 构建推送；12 镜像 `10.10.1.66:30003/ani-gpu-platform/ani-resource-service@sha256:6efdfc029fb98b4449c398f0d7735ffae7b2f45813db9aadebf199a1b8fd4364` 由 `ubuntu@100.64.128.15` 独立构建推送。两个集群仅镜像更新并完成 rollout，没有新迁移、预算/TTL 或 KCN 变更。

R6 的真实 VM `8a27c0cf-b38a-4906-87e7-0700b07703e6` 完成停止态创建、两次 Start、客机网关 ping 3/3、客机正常关机、平台 Stop、Delete；5 个 operation 均 succeeded 且 reason 为空。两个停止阶段的 Reserved 关系停放，随后 12 个原生 VM/运行/盘/网卡/IP 对象逐项 absent，VPC/Subnet/EIP/SNAT 正常删除、无孤儿引用，共享 NAD 原 UID 保留；Attachment `att_a38a7ac6483b48b39cdbc7fda8a54b5a` released 且停放。原件为 `live/full/r6-all-operations-final.json` 及当前 VM/网络清理回执，汇总 `r6-acceptance.json`。

两环境 R6 指标请求间隔均为 300 秒，21 个指标/11 个投影样本、同 Pod/container；终态先等待恢复完成再采样。10：05:58:40–06:03:41 UTC，GET/LIST **1699 次（下降 64.07%）**、基础 LIST 17 次，40 条 Deleted 全部停放且 epoch 不变；4 个 LB 的 44 个样本全 Available，原 15 个原生 UID、operation/版本保留，四入口 HTTP 200+marker 通过。12：06:04:26–06:09:43 UTC（最后日志收尾另耗约 16 秒，不计入指标请求间隔），GET/LIST **1661 次（下降 72.32%）**、基础 LIST 15 次，37 条 Deleted 全部停放且 epoch 不变，7 个 VPC/4 个 Subnet 持续 Available、最大观测年龄分别 20.14/22.53 秒，原 5 台 VM 在 11 个样本中状态/原因/版本/operation 不变。两窗口无传输错误、audit/watch failure、队列溢出或持续 limiter/deadline 错误。归档为 `live/{lab,full}/window-r6-before-default-egress-fix/`。

## R7：平台默认出口复用普通观察

R6 12 窗口的默认出口历史确认 5 次 `Available → Degraded/PROVIDER_UNAVAILABLE → Available`，对应周期之外的 5 轮全量读取；原操作已成功且无 pending action。原件 `live/full/r6-default-egress-history.jsonl`。这是 Res 默认出口 worker 尚未采用统一观察语义，不能归为必要的 KCN 恢复采集，故 #48 继续开放。

修复默认出口读取失败时保留最后状态、应用结果及原 observed_at；查询年龄只返回 stale。Available/Degraded 的普通读取缺少共享事实时等待，不主动收集；首次创建、未知发送和删除继续当前证明。持久 Finish 只允许保留原 Available，不能借观察异常从 Degraded 建立 Available 或提前清理。没有新增状态机、缓存、迁移或 KCN 逻辑。

`r7-default-red.log` 在原 R6 业务代码上复现查询年龄误降级和 Degraded 普通读取的额外采集；测试停止夹具中独立的平台观察器，排除其 80ms 定时采集对计数的干扰。`r7-default-green.log` PASS（7.708s）：真实 PG/业务 worker/Provider adapter 验证失败保留、年龄不改结果、当前身份冲突拒绝、普通缺失快照不采集、未完成创建仍采集，以及原平台出口创建、未知响应恢复、未知未送达预留、删除解绑和过期租约。

R7 的 Fedora `r7-final-verify.log` 完整 `make verify` PASS（包含生成一致性、故障构建、Go 测试/vet/build/module 检查）；对应业务源码尚待发布。R7 只改变默认出口观察，VM/LB 生命周期代码与已完成真实验收的 R6 一致；发布后复核原业务身份、实际 LB 流量和请求窗口，不重复创建已验证且本轮未改的 VM 用例。

R7 发布业务源码 `caefebc2319a57edb0d4e2d9d3bc10b5ec23f6bf`，Fedora 与 15 的构建快照同为 `sha256:e50ff289f7e7bc3b78bb6660b2876f87a5b67f7729bd1a04b5d288583fa9c149`。10 镜像 `172.16.101.10:30003/icn-01a15337-release/ani-resource-service@sha256:bd4490ac023072ca6ea84870000f16e58b0a3fd44037a2026bd2f6bc33216e40`；12 镜像在 `ubuntu@100.64.128.15` 独立构建并直接推送为 `10.10.1.66:30003/ani-gpu-platform/ani-resource-service@sha256:49e5bd2e6fa0bb93ac2f6461233548ff2f5d61474ecd2a419bb7f7655d3f9841`。两集群只替换 Resource 镜像、保留原 Deployment UID，rollout 完成；原 R6 回执保留为 `live/{lab,full}/r6-rollout.json`。

## 最终交付与验收边界

R7 两个窗口的指标请求间隔均为 300 秒，21 个指标/11 个业务投影样本、同 Pod/container，无缺样、传输错误、audit/watch failure、来源重建或队列溢出，也没有窗口内持续 limiter/deadline 错误。

| 环境 | 窗口 UTC（含收尾） | GET/LIST | 相对本环境基线 | 每类基础 LIST |
| --- | --- | ---: | ---: | ---: |
| 12 | 06:25:08–06:30:17 | 1461 | 6000 → 1461，减少 75.65% | 9 |
| 10 | 06:23:23–06:28:24 | 1587 | 4728 → 1587，减少 66.43% | 13 |

12 的 #48 原现场已回到约 30 秒共享周期，9 轮基础 LIST 无额外强制全量读取；7 个 VPC、4 个 Subnet 持续 Available，最大观测年龄为 33.64/32.72 秒；原 5 台 VM 的 11 个样本保留状态、原因、版本和 operation。采样的 37 条租户/平台 Deleted 全部停放且 epoch 不变，28 条 Released Attachment 无定时任务。活动 Attachment 在短暂观察不确定时仍可按既有退避调度，不能声称全窗口所有活动记录均无任务。

10 的 4 个 LB 共 44 个样本全部 Available，最大年龄 48.57 秒；原 operation/配置版本及 15 个 Gateway/Deployment/Pod/Service UID 保留，每个 Deployment 2 Ready，最终私网、公网、双模式四入口 HTTP 200+原 marker 全通过。采样的 40 条租户/平台 Deleted 停放且 epoch 不变。13 次基础 LIST 的额外来源尚未逐次归因，不全部归为周期；既存 ownership conflict、1 条 Released Attachment 观察待确认仍保留，没有手改记录或声明所有历史网络异常已修复。#48 的关闭依据是其 12 集群固定现场及完整业务验收；10 窗口用于 #49 原 LB 状态与流量复核。

两环境默认出口的 11 个样本均 Available；读取不确定时只保留 `PROVIDER_UNAVAILABLE`，观测最大年龄分别为 67.33/57.36 秒（12/10），没有改时间维持假新鲜。默认 VPC/EIP/SNAT 的原 UID、spec 和管理 operation 在发布前后保持。原件为 `live/{lab,full}/default-r7-{before,after}.json`、`window-after/`、`r7-final-*`；汇总为 Fedora 任务根 `final-acceptance.json`。VM 完整生命周期/物理释放证据固定在 R6，R7 的 3 个产品代码改动仅涉及平台默认出口；没有把未重跑的 VM 用例称为 R7 新执行。

Resource [PR #50](https://github.com/zhangzhe-ctrl/ani-resource-service/pull/50) 与 Console [PR #11](https://github.com/bykenkk/ani-console/pull/11) 分仓交付，均未合并。Console 源码检查 PASS，生产构建/部署/浏览器验收 NOT_RUN；后端通过不能替代前端验收。完整网络 PG 集成组曾超时未完成，仍不记为全量通过。#48、#49 依据上述各自现场证据关闭；#47 的 KCN 原 EIP 自动重试未修复，保持开放。KCN `6132623` 工作树仍干净，本轮没有修改其代码、配置、CRD 或镜像。

代码回退需使用兼容 Network schema 27 的 Resource 镜像；12 原 schema 25 镜像不能直接回滚，配套数据库恢复须使用前述已核验备份。本 PR 没有新增 migration。
