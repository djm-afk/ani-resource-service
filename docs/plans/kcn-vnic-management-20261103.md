# KCN 手动 VNIC 管理与实例绑定方案

日期：2026-11-03。本次按用户提出的方向收敛：**普通实例继续由 KCN 自动创建网卡，只新增用户可见的手动 VNIC 管理和可选绑定能力。** 本文替换早先“所有实例由 Res 预建 VNIC”的提案，不要求存量迁移，也不增加默认实例的隐藏 VNIC 生命周期。

完整主链分为两条：

- 默认创建保持：实例 Pod 携带 subnet/security_groups → KCN 自动创建 VNic/VNicIP → 回填 VNIC 名称 → CNI 接网；Pod 退出后的自动网卡回收仍由 KCN 负责。
- 新增手动路径：用户在 VNIC 页面申请 → Res 创建 VNic CR → KCN 分配 MAC/IP → 用户选择实例及目标网卡 → Res 预留目标、同步实例安全组并保存期望配置 → 运行中的实例显示“待重启生效”，停止态实例在下次启动应用 → 新 Pod 引用该 VNIC 并确认接网 → 删除实例仅解除绑定，手动 VNIC 和地址保留 → 用户另行删除 VNIC。

可观察结果是：页面中存在稳定 VNIC 身份及 MAC/IP，当前网卡、目标网卡和生效阶段可查，平台重启后实例实际使用目标网卡，实例退出后网卡仍可再次绑定。当前缺口集中在手动资源 CRUD、可选绑定/解绑、期望与生效配置、重启时模板应用、安全组跟随、身份及释放判断；已有自动分配链路不重写。

本文是实施方案；源码研究见 [KCN VNIC 源码记录](../references/kcn-vnic-source-20261103.md)，实施与验收结果仍归 [执行状态](../execution/status.md)。

## 1. 来源和实施边界

- KCN 固定为本地干净的 `6132623baabee50f17e9e179c0f5e9c1f380db04`（2026-10-30）。此前远端 SSH 查询失败，未确认远端最新或部署版本。
- 2026-11-03 重新读取 GitHub 分支并更新远端跟踪引用：Resource `main` 为 `46a2b8e0e9e85bd8438815fcd3d407ac68f4315d`，已合入 [PR #50](https://github.com/zhangzhe-ctrl/ani-resource-service/pull/50)。原网络简化工作区 `4ab138a4e7d792a9f677484f5e553bd25bd18575` 与该 main 的 tree 均为 `391ec980ba4e81c60d653c763650a6a5b190d4f4`，本次源码核对内容与合入结果一致。
- Governance `main` 为 `b6262869d841121a56911fde244e2f336da4eafa`；Console 当前开发基线为 `main@87b75acd102916380fc901c579bf56ee4901e7a3`，不再使用本地主目录的旧 `feat/governance-api` 工作副本作实现基线。Console [PR #11](https://github.com/bykenkk/ani-console/pull/11) 在核对时仍 open/unmerged，不能把后端合入等同于前端观测提示已合入。
- 当前文档所在主目录仍是旧 Image 分支。后续实现从上述 main 建立独立 `codex/` worktree；不需要再 cherry-pick 或等待网络简化分支，不改动三个仓库主目录中的无关工作。
- 遵循已合入的[网络管理简化方案](network-management-simplification-20261103.md)及实际代码：共享对象观察、动作核验、观测异常与资源失败分开、已完成终态停止周期调度。第 7 节给出具体复用点；本方案不复制其观察实现，也不宣称所有关键动作的完整关系核验已被删除。
- 用户明确由其与 KCN 协调清理缺陷。Res 不为 KCN 忽略 OVN 删除错误的问题增加防御分支、额外释放门禁、版本判断、OVN 检查或补偿清理；继续消费正常资源状态。

### 1.1 为什么不能只改正在运行的 Pod 注解

KCN `pod_controller.go:123–140` 仅在 `PodIP == ""` 时执行网卡初始化；其 update predicate 在已有 PodIP 后也跳过普通网络注解变更（658–660）。CNI ADD 从最终 Pod annotation 读取 VNIC（`cniserver/handler.go:177–234`）。因此，给已接网的 Pod PATCH 一个新的 VNIC 名称，不构成换网卡，可能只改变声明而运行网卡未变。

一期允许运行中保存主网卡绑定、替换、解绑配置，返回 **PendingRestart=true / EffectiveNow=false**；停止态保存后显示“下次启动使用”。真正的网络切换仍在旧运行端点退出、新 Pod 创建的边界执行。保存动作不修改当前 Pod 的 VNIC 注解，也不提前更新会触发控制器自动滚动的模板；期望配置先持久保存在 Res，由实例 owner 在平台 Restart 或下一次 Stop → Start 时应用。

页面同时展示“当前网卡 A / 目标网卡 B / 待重启生效”，提供“重启并应用”以及取消待生效变更。这里的重启要求重建承载网络的 Pod；VM guest 内部 reboot、容器进程重启、VM 暂停后继续均不构成这个生效边界。仅保存配置不能暗中触发重启。

当前 VM worker 的 Restart 已按新 VMI UID 判断重建完成，但仍直接调用原生 restart；容器 lifecycle worker 已有停止确认和新 Pod 核验。新增待生效网络需要接入这些既有生命周期：VM 的带网络变更重启按停止、应用模板、启动推进，容器在既有停止确认与启动之间应用。完成条件还需核验目标网络版本及接入结果，不能仅凭新 VMI/Pod Running 宣告网卡生效。上述是待实现扩展，不表示当前代码已有此能力。

源码定位：[KCN Pod controller](../../../kc-networking/internal/controller/core/pod_controller.go)、[KCN CNI handler](../../../kc-networking/internal/daemon/cniserver/handler.go)。完整固定提交及行号见源码记录 S05/S06/S22。

## 2. 两种模式的明确职责

| 项目 | 默认自动网卡 | 手动 VNIC |
| --- | --- | --- |
| 申请入口 | 现有实例创建，继续传 Subnet/安全组 | 新 VNIC 页面或正式 CreateVNic API |
| 谁写 VNic CR | KCN | Res Network 模块 |
| IPAM、CNI、底层网络配置 | KCN | KCN |
| 生命周期 | 随 Pod，由 KCN 回收 | 用户显式创建、显式删除，实例删除不级联 |
| 实例停止后 | 沿用现有行为，重建 Pod 不保证原地址 | 保留网卡、地址和逻辑绑定 |
| 安全组同步 | 现有 Pod security_groups → KCN 自动 VNIC | 绑定后 Res 将实例安全组写到原 VNIC spec |
| Res 新增管理记录 | 不为每个自动网卡创建新 CRUD/operation 账本 | 保存页面上可见的手动资源和绑定操作 |

两种模式是明确的产品选择，不是失败回退。没有手动绑定的实例不发起 Res CreateVNic；已绑定手动 VNIC 失效时不能静默改回自动模式。自动网卡若在页面展示，应标明来源并只读，不据此接管其生命周期。

## 3. 手动 VNIC 管理页面与合同

页面提供列表、详情、申请、绑定、解绑、删除。展示名称、VPC/Subnet、MAC/IP、分配结果、绑定实例及其类型、当前/目标配置、待重启提示、运行接入情况、安全组、观测时间和原因。绑定目标以列表表达，本期按单主网卡规则受理；未适配的实例类型不出现在可绑定目标中。

下表列出最小业务动作，使用现有授权、分页、幂等、operation 和 PG worker 设施。正式 HTTP/RPC 的归属和落位见第 10 节，绑定操作最终进入实例 owner 的命令入口：

| 接口 | 行为 |
| --- | --- |
| `CreateVNic` | 用户提交名称、subnet_id 和幂等键；服务端解析可信 tenant/cluster/namespace，持久原名称后创建 CR，返回 VNIC ID 和 operation |
| `GetVNic / ListVNics` | 纯读投影，不靠刷新触发创建或修复 |
| `BindVNic` | 指定 VNIC ID、实例类型/ID、网卡槽位及配置版本；本期只接受主网卡槽位。支持首次绑定或直接将 A 替换为 B；运行中保存为待重启配置 |
| `UnbindVNic` | 将对应槽位的期望配置恢复为原 Subnet 自动分配方式；运行中待重启生效，停止态更新下一次启动配置；旧运行占用结束后才解除旧绑定，手动 VNIC 保留 |
| `DeleteVNic` | 仅无逻辑绑定、无未决绑定操作、无运行占用时受理；按原 UID 请求 KCN 正常删除 |

只为手动资源新增必要持久记录：产品 ID、tenant、原 VPC/Subnet/binding UID、可信 cluster/namespace/namespace UID、Provider name/UID、版本、操作和观测结果。实例网络配置按槽位保存期望选择、已应用配置及版本，运行观察关联实际端点；保留现有 Subnet 选择，不回写初始创建审计快照。绑定关系独立保存实例身份、VNIC 身份、槽位与配置版本，不能由原生 annotation 反推一条已授权绑定。具体字段与既有 current configuration、operation 设施整合，不另建通用配置执行引擎。

不增加 `delete_with_instance/retain` 通用策略：本功能创建的所有手动 VNIC 都由用户显式删除。也不复制 IPAM、底层清理或另一套执行引擎；但用户可见的创建/删除及绑定操作仍须持久、幂等、可恢复。

### 3.1 正常申请如何映射到 KCN

VPC、Subnet、手动 VNIC 和目标实例在同一租户 namespace。Res 以稳定产品 ID 派生普通名称，避开 KCN 的 `auto-...-vnic-xxxxx` 自动回收命名模式，不给手动 VNIC 添加 Pod/VMI/实例 ownerReference。

```yaml
apiVersion: networking.kubercloud.com/v1
kind: VNic
metadata:
  name: ani-vnic-0123456789abcdef
  namespace: tenant-example
  labels:
    network.ani.io/tenant-id: tenant-example-id
    network.ani.io/vnic-id: vnic-example-id
  annotations:
    networking.kubercloud.com/allocate_ip_with_dhcp: "false"
spec:
  type: VETH
  subnet: tenant-example/subnet-main
```

标签、名称和 namespace 均由服务端解析。该分配 annotation 的存在触发自动 VNicIP 创建；`"false"` 仍分配 IP，只关闭该 VNicIP 的 OVN DHCP。VM 的 launcher Pod 通过 CNI 接入同样使用 VETH，不能套用类型注释中的 UNMANAGED。VNicIP/IPAM 由 KCN 管理。

申请完成读取原 VNIC 的 Valid/Initialized/Ready、`spec.macAddress` 和所需地址族的 `status.primaryIP/ips` 等结果，不等待 Pod/Nic。VNIC Ready 不等于有地址或已有实例流量；VNIC 没有通用 observedGeneration 回执，不能套用 VPC 的统一 generation 门槛。分配、绑定和在线接入分别展示。

### 3.2 多网卡与多个实例的扩展边界

用户已确认 KCN 支持多网卡绑定。当前源码的 `getPodNetConfs` 也按 Multus 网络/接口遍历，并读取接口对应的 VNIC 注解；Res 不应把“单网卡”写成 KCN 的永久能力限制。本期仍只开放主网卡，后续多网卡/多个实例的管理入口不在本次交付范围。

模型需要分别容纳以下关系，不能将它们混作同一种能力：

| 后续能力 | 本期留出的结构 | 本期不开启的行为 |
| --- | --- | --- |
| 一个实例使用多个 VNIC | 实例网络配置为按槽位标识的集合；每条关联有稳定 ID，原生接口名由 adapter 映射 | 多槽位配置、附加网络/NAD 编排和多网卡页面 |
| 给多个实例分配手动网卡 | VNIC 与实例通过绑定关系关联；查询返回 `bindings[]`，不在 VNIC 上只放一个不可扩展的 owner_instance_id | 批量选择、批量绑定等管理入口 |
| 同一个 VNIC 关联多个实例 | 关系模型不依赖 VNIC ID 的永久全局唯一绑定约束；释放按具体关联和运行端点处理 | 同一 VNIC 的多实例同时占用、主备切换或共享接网语义 |

最后一种能力与“一个实例多网卡”不是同一结论。本地 VNIC controller 同时记录 `nic/dstNic`，但当前选择一个主 Nic；这不足以证明多个独立实例能同时使用同一 MAC/IP 收发流量。后续按 KCN 已确认的具体绑定/转发语义开放，不把本期限制固化为永久拒绝，也不在本期承诺共享数据面已获验证。

本期仍由业务准入限制：每个实例一个主网卡槽位，每个手动 VNIC 至多属于一个实例的有效或待应用配置。这个限制在事务内按 VNIC 锁定并检查全部相关绑定，不能因预留集合结构就放松并发保护；不使用一个永久 `UNIQUE(vnic_id)` 将未来多绑定封死。运行中 A → B 时，A 的当前占用与 B 的目标预留可以同时存在，不能把它们误判为已启用两个网卡。

## 4. 绑定和解绑的最小业务链

绑定或替换按同一命令处理，不要求用户先解绑 A 再绑定 B：

1. 按原 tenant、cluster、namespace/UID、VNIC UID 验证身份及本期占用规则。为控制改动，VM/容器首批仅接受与当前网络选择**相同 Subnet**的手动 VNIC；不顺带实现跨子网迁移，VM 现有 NAD 继续沿用。
2. 通过实例 owner 的命令入口，与 Start/Stop/Restart/Delete/网络变更互斥并检查配置版本。持久受理目标网卡及预留关系；确认本期未被其他实例占用后再修改目标 VNIC，不能在 Network 中绕过实例生命周期直接改 Pod。
3. 将实例的安全组产品 ID 解析为同 namespace 的 KCN 名称，写入目标 VNIC 的 `spec.securityGroups`，并消费相应应用结果。记录实例配置和安全组版本；同步未完成显示“配置处理中”，不能提前宣告已具备重启应用条件。
4. 保存实例期望配置。运行中完成“配置已保存、待重启生效”，保留当前 Pod、当前有效网卡及其占用；目标 VNIC 显示“已预留、待应用”。该配置操作完成后释放操作互斥，不能一直占着运行中 operation 阻止用户发起 Restart。
5. 用户平台 Restart 或下一次 Stop → Start 时，重新核验目标身份、占用和最新安全组，锁定本次应用版本；旧运行端点退出后，在既有生命周期中应用模板再创建新 Pod。VM 写 `spec.template.metadata.annotations`，Notebook 写根注解；其 v1.10 controller 不更新已有 StatefulSet 的模板 metadata，故仅在原 Notebook UID、受控 StatefulSet UID/RV、安全停止、零副本及无受控 Pod 再次确认后退休旧 StatefulSet，沿原 operation 等待 controller 重建并确认目标模板，不直接修改 StatefulSet/Pod。停止态保存也可在确认无旧端点后先完成模板应用。仅更新期望配置期间，控制器自行重建 Pod 仍使用原有效模板。
6. 新 Pod 从创建时就携带 `networking.kubercloud.com/vnic: <裸名称>`；KCN 的 VNIC 引用优先于 Subnet。保留一致的 Subnet 声明，核验新 Pod/VMI/launcher 身份、目标配置版本和实际接网结果后更新生效结果。观察不明时保留最后确认信息和原因；不能把新 Pod Running 或 annotation 正确单独当成网络已生效。

旧自动 VNIC 随旧 Pod 退出由 KCN 回收；Res 不把它改名、接管、移除 ownerReference，或自行删除其底层对象。旧手动 VNIC 在旧端点退出、不再被期望配置引用、且仍可能创建 Pod 的旧模板引用已撤下后解除本实例占用，资源和地址保留。目标 VNIC 在待应用期间禁止被其他实例抢占或删除；切换后 IP 可能不同，页面应在保存前展示目标地址。

解绑是把期望主网卡改回自动模式：运行中仍显示当前手动网卡，待下一次重建 Pod 后才使用原 Subnet 自动分配；停止态则更新下一次启动配置。不能在保存解绑意图时立即把仍在使用的 VNIC 显示为空闲。初始自动网卡的实际名称来自运行观察，不能把 KCN 回填名称误存成用户手动绑定意图。

待重启配置可以通过同一配置命令取消或替换：恢复当前选择即取消待应用变更，释放已不被引用的目标预留；若有应用动作已经发出，则先按原操作确认结果，不能释放可能被新 Pod 使用的网卡。Restart 已受理时，后续配置变更按现有互斥/版本检查处理。启动失败保留期望配置、占用和失败原因，实际状态按已确认端点展示，不自动解绑、删除网卡或回落到自动模式。

一次操作中断或结果未知，沿原操作与原身份恢复。持久配置、目标预留、模板应用及实际接入阶段必须可查；模板完成和运行接网完成是不同结果。停止态没有当前在线网卡，页面显示下一次启动配置，不冒充已经产生网络流量。

## 5. 安全组跟随实例

用户提出的“把手动 VNIC 安全组改为实例的”应同时覆盖**首次绑定和后续变更**，不能只复制一次：

- 自动模式继续由现有 Pod annotation 驱动 KCN。
- 手动绑定期间，以实例的安全组配置为唯一权威。Res 的现有安全组命令将其写入原手动 VNIC；保留实例未来模板中的组声明，使解绑后的自动模式仍有正确输入。
- 运行中存在待重启变更时，实例安全组变更要覆盖当前仍有效的手动 VNIC 和待应用目标；当前自动网卡仍走既有同步路径。新网卡应用前核对最新安全组版本，不能因目标是旧快照而回滚实例策略。
- KCN `handlePodSg` 在发现非自动 VNIC 名称时直接返回（`pod_controller.go:170–172`），所以手动 VNIC 的同步必须由 Res 承接，不能只更新 Pod 的 security_groups 注解。
- 管理页面在绑定期间只读展示继承结果，避免实例页面和 VNIC 页面分别修改两套绑定配置。实例没有组时，明确同步空集合，不能保留上一个实例的组。
- 解绑或删除实例后，建议保留最后已应用的组配置并显示“未绑定”；再次绑定时完整替换为新实例的组，包括清空。它仍是实际 SG 引用，安全组删除保护不能因实例删除就漏掉该 VNIC。

应用回执以相关 SG 的 `status.appliedVNics` 成员关系为充分条件：新组包含目标 VNIC 即绑定成功，旧组不再包含目标 VNIC 即移除成功；不再额外等待 SG Ready、observedGeneration 或 VNIC 的 `status.appliedSecurityGroups`。既有资源引用准入、原 UID 与租户归属核验、owner 安全组选择版本围栏保持。只观察本次相关对象，不能把 PATCH 成功或 VNIC Ready 当成策略已应用。

“跟随实例”是本期单消费者策略。未来若同一 VNIC 关联多个实例，不能由各实例轮流覆盖同一个 `spec.securityGroups`；届时需明确共用同一安全组配置的准入条件，或改为 VNIC 统一管理策略。本期只集中这一规则的入口，不新增策略合并引擎，也不开放共享绑定。

首条真实验收使用现有 IPv4 主网。KCN 自动分配可以生成双栈子对象，但当前 SG 索引的 IPv4 条件不能证明纯 IPv6 同等隔离；其他地址族按实际能力另行接入，不在界面静默降级。

## 6. 实例退出和手动 VNIC 删除必须分开

- **停止实例**：运行 Pod/Nic 退出，保留期望配置中的逻辑绑定和手动 VNIC/IP，未来启动复用。若有换绑/解绑配置，旧网卡占用按第 4 节结束，目标预留保留；所有手动 VNIC 本身均不因此删除。
- **删除实例**：实例 owner 持久封闭新提交，处理本实例的待应用配置并确认原运行对象退出；Network 解除属于本实例的绑定和目标预留，保留手动 VNIC/IP。解除以绑定 ID 和实例身份为范围，不能将 VNIC 的全部关联一并清空。清理状态区分“运行端点仍在占用”和“用户保留的独立网卡仍存在”。
- **删除手动 VNIC**：用户从管理页面明确操作，服务按原 UID 请求 KCN 正常删除，并按正常 CR/子对象删除语义完成。不会随实例删除自动发起。
- **删除 Subnet**：未绑定但未删除的手动 VNIC 仍占用 Subnet；删除保护必须计入这些独立资源，不能只统计活动 Attachment。

现有 Res `kc_attachment.go` 和 `kc_security_groups.go` 都要求 VNIC owner 是 Pod，手动模式要改为“持久原 VNIC UID + 租户/网络归属 + 原实例绑定 + 当前 Pod 引用”验证，自动模式保留原校验。不能简单取消所有权检查。

现有 Attachment Release 把 VNIC/VNicIP 的存在计入依赖（`attachment_managed.go` 的 HasDependencies 判断）。手动模式必须允许独立资源保留而释放实例关联；否则会永久等待一个按产品要求不能删除的网卡。旧自动网卡、旧运行 Nic、未知提交和消费者封闭仍按对应正常合同处理，不能一律忽略依赖。

这些是两种资源所有权的正常业务区分，不是针对 KCN 缺陷增加防御。KCN 清理缺陷由用户与 KCN 协调，Res 不增加 OVN 扫描、强制 finalizer 或故障补偿。

## 7. 采用现有简化观测

### 7.1 main 合入对本方案的影响

总体业务方向不变，实施基线和接线位置已确定。以下落点均位于 Resource main 的 `internal/data/network/`，除明确标出的 biz 文件外：

| 已合入行为 | 本次必须沿用的接法 |
| --- | --- |
| `kc_observation.go:cachedSnapshot` 供普通投影消费共享恢复快照；`kc_observed_provider.go:ObserveAttachment` 区分普通与关键动作 | 手动 VNIC 的正常状态读共享快照；不为每张网卡启动全量采集、独立 informer 或周期探测 |
| `kc_index.go:observationGVRs/relationshipKeys` 已包含 VNic、VNicIP、Pod 及对象、UID、vNic 关系键 | 增加持久手动 VNIC 到现有目标索引的映射；不新增一套网络观察服务。`queries/observation.sql` 的资源清单、通知及共享恢复唤醒必须识别新 kind |
| `worker.go:parkResource` 停放确认稳定的 Available 和已清理 Deleted；`attachment_worker.go` 停放 Attached 及 open/stopped 的 VM Reserved | 空闲/绑定中的稳定手动 VNIC 都使用共享事件唤醒；“用户尚未重启”不构成后台重试任务。保留原身份索引，使真实后续事件仍能唤醒；不能删除索引来冒充终态退休 |
| `biz/network/worker.go` 与 Attachment worker 在读失败时保留业务状态，记录原因，不刷新证据时间 | 页面分别展示分配/绑定结果与观察异常；不能将一次超时显示为网卡已损坏，也不能用配置保存时间冒充运行观察时间 |
| `attachment_vm.go:verifyVMNetworkResources` 对原 VPC/Subnet 做命令核验，取消单凭旧投影年龄拦截 | Bind/Start/Restart 加入原 VNIC 的当前身份和配置核验；不恢复统一 stale 秒数门禁，不引入无关 EIP/SNAT 前置条件 |
| `ObserveAttachment` 对首次接入、`RequireFreshRelations`、封闭后的最终释放仍使用当前共享完整采集 | 本次保留这些已有完成条件。手动资源保留通过调整“谁的依赖”表达，不通过跳过消费者封闭、未知发送或旧端点确认解决 |
| VM 增加 `NativeNotSubmittedError`，区分确定未发送与未知结果 | 带网络配置的重启阶段复用该发送边界；前置检查失败可沿原 operation 重试，实际发送后丢失响应仍按未知处理 |

### 7.2 需要新增的最小观察逻辑

常态投影复用共享 VNic/VNicIP/Pod 来源，以持久 VNIC ID/name/UID、绑定 ID 和当前端点索引定位。列表/详情读数据库投影，不逐行请求 K8s；不为每个自动 VNIC 增加持久 CRUD 调度。VNIC 创建、删除及其安全组应用采用原对象的定向检查，首次接网/最终退出沿用已有 Attachment 关键路径，避免复制一次额外采集。

运行观察按已应用配置和端点身份解释事实，期望配置单独展示。存在待重启变更不能把仍按旧配置正常运行的实例标为网络异常，也不能用目标 VNIC 的 IP 覆盖当前实际地址。重启应用仅增加本次目标 VNIC、相关安全组和新旧端点的定向核验，不新增周期深扫。

保存 B 为目标时，当前使用 A 的 Attachment 继续表示 A 的有效接入，不能为了目标预留把它退回首次 Reserved 状态，造成重复的首次接网采集。目标预留属于 VNIC 绑定关系；到重启应用边界再更新实例网络配置与提交快照。

观察超时保留最后确认状态和时间；共享来源未覆盖变更时，普通投影等待既有共享恢复，动作只做相应当前核验。用户保留的手动 VNIC 是仍存续资源，不与已释放实例 Attachment 混成永久清理任务。安全组的版本与应用确认复用既有 SG 动作路径和相关对象读取；不为本功能增加全量 SG 周期扫描。

Console PR #11 尚未合入，实施前再次核对其状态：已合入则直接复用，未合入则只纳入本功能需要的资源状态/观测提示组件改动并避免重复实现。旧缓存展示可以继续，最终资格由后端动作核验；来源延迟本身不能让前端永久禁用候选网卡。

## 8. 实例适配范围与交付顺序

| 对象 | 必要变化 | 不新增的默认工作 |
| --- | --- | --- |
| Network | 手动 VNIC CRUD、可扩展绑定关系、当前占用和目标预留、两类身份与释放语义、SG 目标投影、独立 VNIC 的 Subnet/SG 引用保护 | 不替所有自动网卡建产品生命周期 |
| VM | 按槽位保存期望/已应用配置、运行中保存与取消、停止后的模板应用、重启/删除和 SG 接点 | 未绑定实例仍走已有 Prepare 与 KCN 自动分配 |
| 容器/Notebook | 同样的可选接点，延迟到重启边界应用模板，保留既有控制器、提交封闭和启停流程 | 不重写正常容器创建 |
| 沙箱等后续类型 | 暴露其停止/恢复语义，保存可选网络引用、注入 Pod 配置并交付退出事实后接入 | 不因新增页面就宣称它已支持绑定，也不要求先改完所有类型 |
| Gov/Console | 手动 VNIC 页面、正式 API、实例选择、当前/目标网卡、“待重启生效”、“重启并应用”和取消入口 | 不要求普通创建页面增加“先申请网卡”步骤；本期不开放多网卡/共享/批量绑定界面 |

各实例 owner 只适配本域生命周期，VNIC CR CRUD 集中在 Network。现有 CPU Kata Sandbox 目前直接渲染 Pod、没有完整 Network/Attachment 接入；其 `provider.go:898` 还会整体设置 annotations，后续接线须合并网络投影与 mutation marker，不能被覆盖。

按真实纵向主链分批：

1. **VM 手动 VNIC 主链与页面**：申请 → 对运行中 VM 保存绑定并继承 SG，确认当前网络未变 → 平台重启应用/guest 通信 → 再次停止启动复用地址 → 保存换绑及取消/应用 → 删除 VM 后 VNIC 保留 → 将空闲网卡重新绑定给另一 VM → 解绑及显式删除网卡。包含停止态配置与自动模式回归，沿用 VM 已有网络/启停合同，不等全部实例适配才交付；先后复用不等于同一网卡同时绑定多个实例。
2. **容器接入**：复用同一管理合同，验证保存不会提前触发 Notebook/StatefulSet 滚动、重启边界的模板传播、Stop/Start/Restart、后续 SG 变更、删除后保留，以及普通自动创建回归。
3. **沙箱等后续适配**：按各自实际网络与生命周期能力逐类增加绑定目标。没有 adapter 的类型明确不可绑定，不做原生 Pod PATCH 的绕过入口。

第一批同一发布必须包括新 API 的正式消费和可操作页面；可以先以受控 Provider 边界打通真实业务代码，但最终须以实际 KCN/VM 数据面证明交付。普通自动模式与新增手动模式都保留，这是产品能力，不是等待全量迁移的临时兼容分支。

## 9. 验证与文档收敛

验证聚焦本次增加的行为：

- 默认 VM/容器仍由 KCN 自动创建、回收，Res 不额外创建隐藏 VNIC。
- 手动 VNIC 同名请求重放、创建结果未知恢复、跨租户/namespace/UID 冲突、重复绑定和并发删除正确。
- 运行中绑定/替换/解绑可保存并准确显示待重启；当前 Pod、实际网卡和地址保持原值，控制器不因保存配置而滚动重启。停止态配置在下一次启动使用。
- 平台重启按原版本应用目标模板，只有新端点使用目标网卡并确认接入后才显示生效；guest reboot、进程重启或暂停/继续不误消除待重启状态。中断恢复不误用旧配置或释放仍可能被消费的预留。
- 当前 A 与目标 B 同时受到占用保护；取消/替换待应用配置、并发 Restart/Delete、启动失败和待重启期间的 SG 变更均保持正确关系。配置保存完成不阻塞后续 Restart。
- 数据和读合同保留槽位与绑定集合，本期服务端明确拒绝超出单主网卡/单消费者范围的请求；不为尚未交付的多网卡或共享接网制造验证结论。
- 不以 Pod annotation PATCH 成功冒充换网；新 Pod 确实使用手动网卡，MAC/IP 与分配结果对应。
- SG 初次继承、后续增加/替换/清空、换绑另一实例均通过实际策略验证；用户保留的独立网卡仍保护其 SG/Subnet 引用。
- 删除实例可完成且手动 VNIC/IP 保留，不被旧 HasDependencies 条件永久卡住；显式删除网卡才释放地址。
- 原自动网卡正常回收，手动 VNIC 不带 Pod/实例 ownerReference，也不使用 KCN 自动回收命名。
- 普通观察无逐资源全集关系 LIST，终态不周期领取；沿网络简化方案区分观测异常与资源失败。
- 本次不新增针对 KCN 已知清理缺陷的 Res 防御或补偿测试分支；下层修复及其故障验收归 KCN。

生成、构建、行为测试、PG 和真实 API 驱动在独立 Fedora 快照执行；本地只编辑、阅读和做轻量检查。测试聚焦新增行为及 VM/容器/Network 关联路径，提交前执行仓库要求的 `make verify`。API 受理、受控边界、真实接网和显式回收分别留证。

正式实现同批更新 [VPC/Subnet 规格](../specs/vpc-subnet.md)、[VM C05 合同](../contracts/vm-v1.md#c05-network-v3-合同)、[VM S07](../specs/vm-mvp.md#s07-network-与动态地址)及 [ADR-0001](../adr/0001-own-network-lifecycle.md)；保留自动模式现有语义，只追加手动资源的例外所有权与正常释放规则。观察职责沿 [ADR-0004](../adr/0004-observe-cr-with-durable-reconciliation.md)和简化方案集成。当前方案不修改业务代码、迁移、部署或 KCN。

## 10. 可执行的后端改造清单

以下字段、路径和文件为本次拟新增内容，不是当前已存在的接口。实施时在对应现有规格/Proto 落成唯一合同，本文保留任务顺序和验收入口。

### 10.1 数据与模块归属

| 落点 | 必须实现的内容 | 必须保持的边界 |
| --- | --- | --- |
| Network `network_vnics` | `tenant_id/vnic_id/name/description/vpc_id/subnet_id`、可信 placement、原 Provider binding、配置版本、生命周期、MAC/IP 投影、观察时间/原因、最近 operation | 产品 ID 使用 `vnic_<32 hex>`；父网归属和原 UID 不可通过更新接口换掉；不以实例 ownerReference 管理生命周期 |
| Network `network_vnic_bindings` | 独立 `binding_id`、tenant、VNIC、`consumer_kind/consumer_id/slot`、期望/已应用配置版本、释放时间；按相关端点解释当前占用 | 同租户组合外键；实例删除只释放本实例关系。一个槽位在切换期间允许当前 A 与目标 B 两条记录；不加永久 `UNIQUE(vnic_id)` |
| 既有 Network 管理表 | 为 `network_operations/reconciliations/idempotency/provider_bindings/resource_history` 增加 VNIC 资源引用和合法 kind，扩展封闭的 CHECK、外键、列表/claim/finish/通知 SQL | 复用现有 operation、租约、未知请求恢复和共享 worker；VNIC 不另建独立调度器 |
| VM 当前配置、Instance 当前配置 | `network_interfaces[]` 按 `slot` 保存 `desired/applied` 选择和配置版本，运行投影独立关联 Pod/VMI UID；本期只有 `primary` | 初始创建快照不改；缺省自然代表既有自动模式，不批量回填隐藏网卡；观察更新不覆盖较新的期望配置 |
| SG targets/references | 允许独立 `vnic` 目标，保存 VNIC 实际引用的 SG 集合；沿用实例 SG 命令驱动当前和目标网卡 | VNIC 未绑定但仍保留组配置时，SG 删除保护继续有效。实例 SG 引用释放不等于独立 VNIC 引用释放 |

Network 合并主干后保留已发布的 `0028_attachment_consumer_type.sql`，本任务三条迁移原字节顺移为 `0029_manual_vnics.sql`、`0030_vnic_requested_addresses.sql`、`0031_subnet_creation_options.sql`。显式 owner 迁移在同一事务与锁内，仅对此前已发布的本任务 28–30 精确校验和连续前缀补执行主干 28，再顺移既有账本版本并保留校验和及执行时间；未知、混合或空洞历史拒绝。运行时只接受统一后的 canonical 历史。VM/Instance 的新增配置优先沿用既有 JSON 当前配置与 operation execution；只有确需持久列/查询索引时才追加各域迁移，不能仅为新 JSON 字段复制整套状态表。

新类型落在 `internal/biz/network/vnic.go`，Provider 实现在 `internal/data/network/kc_vnic.go`，PG 命令/查询在 `internal/data/network/vnics.go`、`queries/vnics.sql`。VNIC create/delete 接入既有 Network worker 的 kind 分支，分配就绪使用本方案第 3.1 节的 VNIC 特定事实；不能强套 VPC 的 CIDR/generation 判断。sqlc 和 protobuf 从输入生成，不手改生成文件。

### 10.2 写入与恢复协议

1. **申请**：Network 在一个事务内保存产品记录、父网引用、稳定 Provider 名称、幂等摘要和 operation；worker 按原身份创建 VNic。响应丢失先读原对象并验证 UID/归属，不能换名称重复申请。空闲网卡确认分配后停放周期任务。
2. **保存配置**：VM/Instance 先按自身配置版本受理持久命令，worker 调用 Network 的窄端口预留目标。预留事务锁定 VPC 与目标 VNIC/绑定行，防止父网删除、另一实例绑定和 VNIC 删除并发穿透。本期独占校验在此完成。
3. **跨模块中断**：使用实例 operation ID、槽位和配置版本作为稳定请求身份；Network 预留调用幂等。预留成功、实例配置尚未提交时重启 worker 继续原步骤。明确取消才撤销对应预留；超时未知不撤销、不换幂等键。每次调用单独提交本域事务，不跨域持有数据库锁等待 Provider。
4. **安全组与完成**：实例 owner 解析最新组配置，经 Network 更新目标 VNIC；准备完成后提交期望配置，结束“保存配置”operation。待重启状态留在资源配置中，不保留一个永久运行的 operation。失败保存原阶段和原因，可从原 operation 恢复。
5. **应用**：平台 Restart 对本次目标版本建立执行快照，先核验目标再停止；在旧端点退出后应用模板、启动并确认网络。VM/容器各用自己的 worker 阶段，Network 只提供预留、应用查询和释放端口。没有手动变更的普通 Restart 继续原行为。
6. **删除**：实例 Delete 封闭本实例新提交，处理待应用目标和运行占用后释放关系；VNIC 留存。VNIC Delete 只对无关系/无未决发送/无运行占用的对象受理，再沿 KCN 正常删除事实完成。保留 Subnet 和 SG 引用直至 VNIC 自己的删除完成。

本次不要让 VM/Instance adapter 直接读写 Network SQL，也不要让 Network worker 接管 VM/Notebook 的启停。`cmd/ani-resource-service` 在现有组合根注入端口；`internal/service` 只做运输合同到用例的转换。所有网络 annotation 由可信 adapter 生成，继续使用既有保留键校验，用户通用 annotations 不能绕过绑定接口指定任意 VNIC。

### 10.3 Resource RPC 与 Gov HTTP

| 公共入口（拟新增） | Resource 归属 | 请求与结果 |
| --- | --- | --- |
| `POST /api/v1/networks/vnics` | Network `CreateVNic` | `data` 包含名称、可选描述、subnet_id、idempotency_key；返回 VNIC 详情及 last_operation_id |
| `GET /api/v1/networks/vnics` | Network `ListVNics` | `page/pageSize`；声明 `name/subnet_id/binding_state/state` 筛选，返回 `items/total` |
| `GET /api/v1/networks/vnics/{vnic_id}` | Network `GetVNic` | 返回详情、分配事实、`bindings[]`、SG 与观察信息；普通读不访问 K8s |
| `DELETE /api/v1/networks/vnics/{vnic_id}` | Network `DeleteVNic` | 原 ID 的幂等删除，按现有 Network DELETE 约定，不自行附加请求体；返回持久删除结果/operation 引用 |
| `GET /api/v1/networks/vnics/{vnic_id}/binding-targets` | Resource 受信查询适配器调用各 owner 的租户读端口 | `consumer_kind=vm/container`、name、page/pageSize；服务端筛出同租户/cluster/namespace/Subnet 且本期支持的目标 |
| `GET /api/v1/virtual-machines/{vm_id}/network-interfaces` | VM 网络配置读用例 | 返回按槽位组织的期望、已应用配置和运行事实 |
| `PUT /api/v1/virtual-machines/{vm_id}/network-interfaces/{slot}` | VM `SetNetworkInterface` | `data` 含 mode、可选 vnic_id、expected_version、idempotency_key；返回现有 VM operation 类型 |
| `GET /api/v1/instances/{instance_id}/network-interfaces` | Instance 网络配置读用例 | 与 VM 相同含义的网络配置视图，实例身份和版本仍由 Instance 管理 |
| `PUT /api/v1/instances/{instance_id}/network-interfaces/{slot}` | Instance `SetNetworkInterface` | 同样表达自动/手动选择，使用 Instance 自身命令与 operation 返回合同 |

网卡页和实例详情都调用上述实例配置命令，不再增加一个会绕过实例互斥的 Network 绑定写入口。解绑就是将相应槽位设为 `automatic`；取消待重启变更就是以当前已应用选择覆盖期望选择，仍带最新实例 expected_version。服务端在启动/应用已受理时拒绝并发覆盖。

从实例详情选择网卡时，`ListVNics` 另声明成对的 `bindable_to_kind/bindable_to_id` 条件，服务端根据实例归属筛选同位置/同 Subnet 的空闲网卡及该实例自己的目标网卡。反向候选查询按实例类型调用现有 owner 读端口，不能为了组合分页在 Gov 或浏览器拉齐所有 VM/容器；候选按类型分别分页。运行中实例可以被选中，删除中、未适配类型及不匹配网络不能选。

新列表统一采用 page 默认 1、pageSize 默认 20/最大 100、真实 total 十进制字符串；列表和计数共享同一租户/筛选快照，排序 `created_at DESC, id DESC`，name 按去空白后不区分大小写的子串匹配。未声明参数或非法分页返回 400。只有服务端定义的过滤可出现在页面上，不复制旧 cursor 接口或由前端二次筛选。

运行中切换请求示意（字段在 Proto 显式定义 JSON 名称，不批量重命名既有 VM 字段）：

```http
PUT /api/v1/virtual-machines/{vm_id}/network-interfaces/primary
Content-Type: application/json

{
  "data": {
    "mode": "manual",
    "vnic_id": "vnic_0123456789abcdef0123456789abcdef",
    "expected_version": "9",
    "idempotency_key": "8db24b28-faf2-46ca-a838-6084a76b71c5"
  }
}
```

operation 受理与完成分别使用已有语义；网络配置读结果至少包含 `slot`、`desired`、`applied`、`requested_revision`、`applied_revision`、`pending_restart`、`effective_now`、当前地址及观察字段。停止态表达“下次启动使用”，不能因为 desired 与模板一致就展示为在线。实际地址来自当前端点与 KCN 事实，不能从目标 VNIC 或容器 PodIP 猜出。

“重启并应用”使用既有 VM `/restart` 和容器 `:restart` 接口，读取最新实例版本后提交；业务中断说明和用户确认放在页面。保存配置请求不附带自动重启副作用，Gov 不在一次保存成功后偷偷追加 Restart。

### 10.4 授权、装配和发布依赖

- Resource 增加对应 gRPC 注册及 mTLS 方法白名单；Gov 依赖正式生成的 Resource API 版本，不通过 local replace、跨仓 internal 导入或拷贝生成类绕开发布依赖。
- Gov VNIC 管理走 `NetworkService`、可信租户映射、NETWORK 套餐和 `network:vnic:create/list/get/delete` 等精确权限；原 Network operation 查询继续复用。候选读取同时检查目标类型的读取权限。
- VM/容器网络写入增加各 owner 的网络配置权限，并核验使用手动网卡所需的 Network 权限；平台 Restart 仍要求实例原有重启权限。不能仅因用户能查看 VNIC 就允许其改变任意实例。
- 更新 `pkg/middleware/auth/network_policy.go` 的方法、请求类型和 ID 校验，VM/Instance 的动作登记、BFF service/client、RPC allowlist、API 目录/角色/套餐关系，以及 Gov 接口集成登记。API 目录同步不等于自动授予权限。
- Resource 的 `deployments/egress/network-rbac.yaml` 当前对 vnics/vnicips 只有 get/list/watch。单独为 **vnics** 增加 create/patch/delete；VNicIP 和底层 Nic 的创建、删除仍归 KCN。核对实际安装方清单中的同一角色，不能仅换服务镜像后假定 RBAC 已生效。
- 保留现有版本/归属/占用/Provider 错误原因；只有 VNIC 特有情况才加新 reason。前端收到版本冲突先刷新并保留用户选择，不自动拿新版本重提；Provider 观察失败也不能触发新身份重建。

## 11. 前端页面与交互交付

这是本次新增功能设计。页面复用 Console 当前正式路由、Arco、DataTable、DetailPageFrame、TanStack Query 和反馈设施；不把开发参考页面或未核对的产品原型当成交付。租户 Console 是本期前端范围，不新增 Boss 的跨租户网卡管理。

### 11.1 页面入口和必要内容

| 页面/入口 | 必须交付的内容 | 主要动作与反馈 |
| --- | --- | --- |
| 网络管理 → **虚拟网卡**，路由 `/vnics` | 手动 VNIC 列表；名称/ID、VPC/Subnet、MAC/IP、分配状态、使用状态、绑定对象、创建时间、观察提示；筛选和分页与第 10 节一致 | 申请、进入详情、绑定/换绑入口、删除；自动生成的 VNIC 不加入这一手动管理清单 |
| `/vnics/$vnicId` | 基本信息、地址、安全组只读信息、绑定关系列表；关系显示实例类型/名称、槽位、当前使用或待应用；观察异常与业务结果分开 | 资源级动作统一放“更多操作”；被使用或预留时不能删除，展示具体占用实例入口 |
| **申请虚拟网卡**弹窗 | 单页表单：名称、可选描述、VPC、联动 Subnet；VPC 仅帮助选择，服务端按 subnet_id 解析归属；展示将自动分配地址 | 提交后提示“创建已提交”，关闭弹窗并刷新列表；分配未完成前不能绑定。页面不暴露 namespace、CR 名称、UID 或 DHCP annotation 等实现参数 |
| **绑定实例**弹窗（从 VNIC 进入） | 先选 VM/容器类型，再按服务端候选结果选实例；显示当前网卡、目标网卡/IP、将继承的实例安全组、运行状态 | 运行中提示“保存后需重启生效”；停止态提示“下次启动使用”；提交保存，不隐式重启 |
| VM 详情的现有网络内容 | 在既有 VPC/Subnet、地址、运行网卡、DNS、安全组显示上增加网卡配置表：当前网卡、目标网卡、使用模式、生效状态 | “切换网卡”“恢复自动分配”“取消待生效变更”“重启并应用”；目标 IP 不覆盖当前地址，保留现有来宾观察来源 |
| 容器详情的现有网络内容 | 同样增加配置表与动作；通过新网络视图补齐当前网卡与地址，保留已知来源 | 只对已接入 Notebook/Network 的受支持实例开放；其他类型显示实际能力，不借原生 Pod PATCH 实现 |
| Subnet、SG 关联资源 | 显示留存手动 VNIC，并可跳转详情；删除被阻止时能定位原因 | 实例删除后仍能找到占用 Subnet/SG 的独立网卡，用户可完成后续显式删除 |

两侧入口使用同一份绑定请求和服务端资格判断。名称、地址和安全组对照来自后端正式视图，不能在浏览器遍历全部资源拼接；只展示当前用户有权查看的实例。安全组本期在 VNIC 页面始终只读，解绑后显示“保留上次配置”。

### 11.2 页面状态与刷新规则

| 后端事实 | 页面必须显示 | 可执行动作 |
| --- | --- | --- |
| 创建已受理、地址尚未分配 | 创建/分配中，MAC/IP 缺项显示 `-` | 查看、刷新；按后端能力控制删除，不启用绑定 |
| 分配完成，无绑定/预留 | 空闲，显示已分配 MAC/IP | 绑定、删除 |
| 配置已保存，实例仍使用 A，目标为 B | 当前 A 与其实际地址；目标 B 与分配地址；**待重启生效** | 取消或替换目标、重启并应用；A/B 均不显示为空闲 |
| 实例已停止 | 无当前在线端点；显示下次启动配置 | 启动、修改配置；不显示“已在线生效” |
| 重启应用已受理 | 正在应用，保留本次目标和真实进度/失败原因 | 禁止并发配置覆盖；恢复与重试按已有 operation 语义 |
| 新端点使用目标且接网已确认 | 已生效，清除待重启提示 | 后续正常操作 |
| 观察暂不可确认 | 保留最后确认事实和时间，显示观察待确认 | 刷新；命令由后端当前核验决定，不单凭缓存年龄永久禁用 |

写入成功回调只负责缓存失效、关闭弹窗和必要导航；新查询结果决定生效状态，不把 mutation 响应直接写成“当前网卡”。VNIC 列表首进、手动和操作后刷新，不增加每行/全列表永久轮询；实例详情复用已有刷新机制。创建/删除沿 Console 当前规范不新建 task 轮询和任务弹窗。

跨页面刷新范围为 VNIC 列表/对应详情、实例网络视图/详情、受影响 Subnet 和 SG 关联查询。删除提交后返回列表，详情缓存仅标记失效。查询错误走统一 errorNotification、写操作走 meta.feedback；“观察待确认”属于业务观察信息，不能被当作请求错误弹窗替代。

### 11.3 前端代码落位

| 范围 | 文件/目录 | 改动 |
| --- | --- | --- |
| 导航/路由 | `src/layouts/AppLayout/navigation.tsx`；新增 `src/routes/_authenticated/_app/vnics/index.tsx`、`vnics/$vnicId.tsx` | 网络管理菜单、路径展开映射；路由保持薄入口 |
| VNIC API | 新增 `src/api/network/vnics.ts`，扩展同目录 `types.ts/index.ts` | 正式 Gov 请求、分页、类型；幂等作用域在 API 层，页面不生成 key |
| VNIC 页面 | 新增 `src/components/vnics/VnicsPage/index.tsx`、`VnicDetailPage/index.tsx` | 列表/详情与关联导航，使用公共表格/详情框架 |
| 弹窗/选择器 | 新增 `CreateVnicModal`、`BindVnicModal`、`VnicSelect`，均位于 `src/components/vnics/<组件名>/index.tsx` | 联动表单、后端选项查询、当前/目标对照；按现有 withFirst100 选项约定加载，不遍历分页或本地筛选 |
| 实例配置交互 | 新增 `src/components/instances/InstanceNetworkInterfaceModal/index.tsx`；扩展 `src/api/vms/`、`src/api/instances/` | mode/目标选择、expected_version；VM 和容器分别调用自己的命令 API，弹窗不绑定路由 |
| 实例展示 | `src/components/vms/VmDetailPage/VmNetwork/index.tsx`、`src/components/instances/InstanceNetwork/index.tsx` 与实际 ContainerDetailPage 消费处 | 当前/目标/待重启展示及动作；复用已有 Restart，不另造生命周期 Hook |
| 关联资源 | `src/components/subnets/SubnetDetailPage/SubnetRelatedResources/`、`src/components/security-groups/SecurityGroupDetailPage/SecurityGroupRelatedResources/` | 接入 VNIC 类型和详情链接；按服务端分页返回引用 |

选项数据使用服务端 `bindable_to_*` 或 `binding-targets` 条件，关闭组件的本地选项过滤。表格显式列宽、长 ID 截断、空值 `-`、删除/重启确认和图标可访问名称沿现有 UI 规范。共享绑定 mutation/刷新有实际多入口复用时才提取业务 Hook，不提前搭通用网卡表单框架。

当前 Console `subnetIpUnavailableReason` 仍因 `observation_stale` 拒绝选择，这是待合入 PR #11 涉及的旧逻辑。本功能的自动分配表单不能复制这个地址范围时效门禁，也不需要前端指定 IP；由后端申请命令核验 Subnet/IPAM。独立 VNIC 的正常缺地址结果必须明确展示，不能以忽略观察提示换取“可用”。

## 12. 按顺序实施的工作包

工作包按业务闭环排列。后端和前端属于同一工作包的退出条件，不把“新增了 API wrapper”当作页面完成；不等待沙箱、多网卡或批量绑定才交付本期 VM/容器能力。

| 工作包 | 实施内容和主要落点 | 退出条件 |
| --- | --- | --- |
| VNIC-00 基线与合同 | 三仓从第 1 节 main 建独立 `codex/` worktree；冻结本期 DTO、owner 命令、权限、分页、JSON 字段及迁移顺序；确认 Console PR #11 依赖 | 能明确列出每个页面调用的正式端点、每个写入的领域 owner；不新增运行时能力声明 |
| VNIC-01 申请到页面 | Resource migration、`biz/network/vnic*.go`、`data/network/vnic*.go/kc_vnic.go`、sqlc/观察映射/worker、RPC/RBAC；Gov `catalog/.../vnic.proto`、`i_network.proto`、network service/client/policy；Console 导航、申请、列表、详情和删除 | 经 Console → Gov → Res → KCN 创建真实空闲 VNIC并看到 MAC/IP；刷新页面不触发创建；删除完成后地址释放；同键重放无第二张网卡 |
| VNIC-02 VM 完整主链 | `biz/vm/types.go/worker.go` 与新增网络配置命令；`data/vm/network.go/native.go/postgres.go/security_groups.go`；Network 预留与手动身份/SG/释放适配；Gov VM 合同、service/client/授权；VmNetwork 与绑定弹窗 | 对运行 VM 保存、取消/替换目标；旧网不变；平台重启后使用手动 VNIC；后续 SG 变更/清空有效；停启复用；删除 VM 留存 VNIC；再解绑/删除网卡可完成 |
| VNIC-03 容器完整主链 | `biz/instance/network.go/lifecycle*.go/security_groups.go`、`data/instance/network.go/notebook.go/security_groups.go/postgres.go`，对应 RPC/Gov Instance 接线与 InstanceNetwork 组件 | 同一页面能力贯通受支持容器；保存不触发 StatefulSet 滚动；重启更换 Pod 后生效；SG、取消、删除保留及自动模式回归全部成立 |
| VNIC-04 关联展示与故障收口 | Subnet/SG 关系保护与页面跳转、观察提示、取消/版本冲突反馈；未知发送、并发绑定删除、服务重启恢复的定向回归 | 用户能从占用提示找到资源并完成清理；无错误“已生效”、无悬空预留；不引入周期逐网卡全集查询 |
| VNIC-05 发布与真实验收 | 同批发布 Resource API/镜像、迁移/RBAC、Gov 客户端/API/权限、Console 正式页面；完成第 13 节流程与当前文档链接 | VM 和容器各一条从页面操作到实际网络结果的证据，源码/受控边界/真实环境结果分别标记 |

VNIC-02 的 SG、身份、互斥及删除保留是主链的一部分，不推迟到 VNIC-04。VNIC-04 只集中补齐跨页面一致性和已接通流程的必要异常分支。没有端口适配的沙箱不加入候选列表；新增多槽位、批量关联、同网卡多实例共享属于后续工作包。

## 13. 验收、执行位置与发布顺序

### 13.1 每步需要的验证

- **Resource**：新增命令/worker 行为测试、真实 PG 的身份/预留/版本/幂等/恢复测试、KCN adapter 的字段与 UID 读写测试；范围限定 Network、VM、Instance 的受影响路径。新增 SG `vnic` 引用验证留存和删除保护。提交前按仓库要求在 Fedora 执行 `make verify`。
- **Gov**：正式 HTTP 请求/响应、严格分页、JWT/AK 租户隔离、NETWORK 与实例动作权限、受信 RPC 元数据、错误映射和同键重放；生成使用既有 `make api` 流程。真实客户端依赖必须指向已生成并发布的 Resource API 版本。
- **Console**：先对修改文件执行 Oxlint，再按工程规范执行 `pnpm lint`、`pnpm fmt`、`pnpm fmt:check`、`pnpm typecheck`、`git diff --check` 和 GitNexus 影响/变更检测。按当前仓库约定不新增自动化测试资产、不运行 production build、不干预用户 dev 服务；使用正式页面完成手工交互验收。
- **观察成本**：固定一组空闲 VNIC、运行实例和停止实例；在无操作窗口核对没有逐 VNIC 全量 LIST、稳定目标停放、真实事件可唤醒、结束操作无重复领取。再执行一次申请/重启/删除，区分关键动作必要核验与普通观察；不要求重跑无关全网压测。

重任务在 `ssh fedora` 的独立源快照执行；本地只编辑、检查和传输。检查命令、源 commit、实际主机和结果记录到现有执行状态/证据入口。新测试名随实现确定，不在方案中声称当前仓库已有可运行的 VNIC 测试。

### 13.2 必须完成的页面到数据面流程

| 用例 | 用户操作 | 要看到的真实结果 |
| --- | --- | --- |
| 默认模式回归 | 原方式创建 VM、容器，再停止/删除 | 仍由 KCN 自动创建/回收，没有 Res 隐藏 VNIC 产品记录 |
| 申请及重复请求 | 页面申请手动 VNIC；同键同参重放 | 仅一条产品记录、一个原 CR 身份，MAC/IP 可见，空闲时不依赖实例存在 |
| 运行中配置 | 运行实例从 A 选择 B，随后刷新/取消/再次选择 | 当前 Pod UID 和真实地址不因保存变化；B 被预留；取消释放 B 的无效预留；两侧页面一致 |
| 重启应用 | 点击“重启并应用”，再验证实际通信 | 新 Pod/VMI 身份、目标 VNIC UID、MAC/IP 和 guest/容器网络一致；旧端点退出；页面才显示生效 |
| 自动模式恢复 | 对手动模式选择恢复自动，平台重启 | 新 Pod 走 Subnet 自动申请；原手动 VNIC/IP 留存且可再次绑定，页面提示 IP 可能变化 |
| 安全组 | 绑定继承、增删/清空组、待重启时修改组 | 当前和目标按本期规则使用最新策略，实际允许/拒绝流量符合配置；不存在旧目标覆盖新组 |
| 实例删除 | 含当前手动绑定和待应用目标时删除实例 | 实例删除完成、原运行对象退出，相关手动 VNIC 均保留；无关联再占用该实例；SG/Subnet 引用仍准确 |
| 再次使用与显式删除 | 将空闲网卡绑定另一实例，随后解绑并删除网卡 | 使用稳定原网卡身份/地址；只有显式删除 VNIC 才走地址释放；无 KCN 缺陷补偿逻辑 |
| 冲突与恢复 | 两实例同时抢同一网卡、绑定与删除竞争、保存/重启中服务重启、观察暂时不可用 | 至多一个有效预留；按原 operation 恢复；未知结果不重复创建；不以过期绿色状态冒充当前已确认 |

VM 和容器分别执行适用用例。API 200/202、Pod Running、页面成功提示均不是网络验收替代品；真实流量、保留资源和最终显式释放分别留证。未执行的地址族、实例类型、多网卡和共享接网明确不在本期验收声明中。

### 13.3 发布顺序与回退约束

1. 固定三仓提交与生成 API 版本，先在独立环境验证增量迁移、Resource 组合模式、vnics RBAC 和回归；部署时显式应用迁移及 RBAC，再更新支持新 schema 的 Resource。
2. 更新 Gov 的正式 Resource API 依赖、HTTP/RPC 注册和权限目录/角色/套餐关联，再发布 Gov。新端点未就绪前不发布可提交的前端入口。
3. 发布 Console 正式导航和页面，执行上述真实主链。不存在要求用户先迁移存量自动 VNIC 的步骤。
4. 本期迁移保持增量，不删除旧表/初始配置。回退前检查是否已有手动绑定/待生效配置；不支持这些字段的旧 Resource 镜像不能直接接管。必要时先通过受支持版本解除新能力占用或使用兼容回退版本，不能靠删 CR/清数据库实现回退。

本次方案修订的完成范围为 main 影响核对、后端/前端工作包和验收设计；新 VNIC 功能的生成、测试、部署、浏览器与数据面验收均尚未执行。
