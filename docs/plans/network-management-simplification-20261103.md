# Res 网络管理简化方案

> 历史方案：网络状态消费、准入与释放条款已由[2026-11-04 实施方案](network-management-simplification.md)及[规格 v3](../specs/cr-observation.md)替代。历史执行事实继续按原记录阅读。

日期：2026-11-03。本文为用户已授权的实施方案；用户已明确职责方向：Res 做好管理侧功能，底层网络问题交由 KCN 团队解决。实施与验证结果仍只记入[执行状态](../execution/status.md)。

目标是减少 Res 必须理解、推导和恢复的底层行为。Res 保留产品意图、租户归属、资源关系、持久操作和结果展示；KCN 负责网络资源实现、IPAM、收敛重试与底层清理。重试发送管理命令与重试底层网络收敛必须分清。

完整业务链为：创建 VPC/Subnet → 按业务需要申请并绑定 EIP/SNAT → VM 或 LB 使用网络 → 正确展示结果 → 释放消费者 → 删除网络并释放管理侧占用。第一批先贯通 VPC/Subnet → VM 内网使用与退出，其余能力按同一职责逐项收敛。

当前断点与成本见[本次现场诊断](../execution/records/network-management-simplification-20261103.md)：#47 是 KCN 原 EIP 分配失败后缺少可靠重试；#48 存在重复采集和历史 Deleted 持续调度；#49 已捕获 Res Observe 超时及状态退化，部分 NOT_READY 原因仍未确定。简化方案不将 #49 整体归因于 KCN，也不把历史验收当成新方案已通过。

## 1. 职责划分

| 工作 | Res 保留 | 下层负责 |
| --- | --- | --- |
| 资源管理 | 产品 ID、tenant/cluster/namespace/UID、参数约束、持久幂等、版本、operation | KCN CR 与底层对象的实现 |
| VPC/Subnet/EIP/SNAT | 创建、修改、删除意图；父子关系；EIP 业务独占；默认基础 EIP/SNAT 的产品编排 | KCN 分配地址、配置 OVN/路由/NAT、处理暂时失败和容量恢复 |
| 运行状态 | 保存并解释权威对象的状态与原因，区分配置结果和观测异常 | KCN 对其 Ready、绑定结果和清理结果负责；需要时由平台监控提供流量健康 |
| 消费者接入 | VM/容器身份、Attachment 登记、提交封闭、管理侧删除保护 | 实例 owner 负责实例退出；KCN 负责其派生网卡、地址和网络规则清理 |
| 交换机与地址池 | 平台输入、节点/网卡选择、逻辑占用、原身份与资源关联 | KCN 执行网卡接管、chassis/路由收敛及健康反馈；安装方负责部署兼容性 |
| LB | Listener/Backend/VIP/规格管理，Gateway/Route/EnvoyProxy 意图编排与归属 | Gateway/Envoy 控制器负责生成对象及代理运行；KCN 负责 Service 网络与 EIP 绑定 |
| 删除完成 | 关闭自身写入与引用、请求下层删除、等待权威完成结果、释放 Res 占用 | KCN/对应控制器完成自身底层对象清理并提供可靠的完成语义 |

Res 不增加通过改 annotation/spec 唤醒 KCN、换 ID 重建故障资源、直接清理 OVN/IPAM、强制去 finalizer 的补偿分支。既有业务编排继续存在，例如用户启用默认基础连接后创建 EIP/SNAT；它与替代 KCN 的故障恢复有不同职责。

## 2. 要删减的具体逻辑

| 现状与源码落点 | 目标行为 | 删除条件 |
| --- | --- | --- |
| `kc_observed_provider.go` 的 `completeDependencies` 按本次数据库时间要求新完整快照；`kc_observation.go` 维护跨类型审计 | 普通状态观察使用共享的对应 CR 事实；命令只核验本次直接依赖。删除普通路径中的全量关系采集和跨类型重采循环 | VPC/Subnet 主链可先做；清理路径按第 4 节合同逐项退出 |
| `attachment_vm.go` 用 VPC/Subnet 的持久状态和统一 60 秒门槛拒绝 VM 预检 | VM 动作针对原 VPC/Subnet、Namespace/NAD 等实际接入依赖核验；旧投影年龄不能成为唯一拒绝原因 | 保持原身份、租户、配置版本和事务并发检查；当前核验失败仍拒绝该动作 |
| `kc_load_balancer_capability.go` 在共享 audit 内检查安装指纹、镜像、CRD、控制器权限 | 安装兼容性归安装/升级验收；运行配置变更或实际错误再触发集中诊断。已有资源的状态不反复依赖整套安装验收 | 运行时仍识别 API/权限错误；平台能力缺失只影响对应动作/能力 |
| `kc_load_balancer_generated.go` 推导 Service、Deployment、Pod、端口、地址范围的复合就绪 | 消费有明确语义的 Gateway/Route、KCN Service/EIP 状态；生成对象明细保留为按需诊断 | 对应控制器必须覆盖已承诺的配置应用与就绪语义，详见第 5 节 |
| `worker.go`、`kc_tombstone_observation.go` 为终态墓碑持续领取和观察 | 清理完成的记录退休；保留历史身份和操作，不再为每条终态记录设置周期任务 | 无活动操作、无未知发送、写入已封闭、下层清理已按现有协议确认即可先做，不必等待 KCN 新增统一回执；不能只凭数据库 `Deleted` |
| `kc_attachment_vm.go` 等路径推导 VM/VMI/launcher 与底层网卡、IP 的完整释放链 | Res 保留消费者身份及封闭协议；消费 KCN 的网络释放事实 | KCN 提供覆盖原消费者 UID 的清理完成合同后，删除对应网卡/IP 全集扫描及补偿逻辑 |
| `kc_egress.go` 等路径重复取 ConfigMap/ServiceCIDR；Namespace 校验重复 GET | 集群配置共享读取；同一动作内复用已验证的同一对象 | 可独立实施，不改变所有权与读取结果语义 |

这些是实际删除或缩小路径的工作，不以把原逻辑搬进一个新封装作为完成。沿用现有 PG worker、Provider adapter 和 shared informer，不新增第二套执行引擎、中央网络观察服务或通用证据框架。

## 3. 状态与执行收敛

### 3.1 保留已有产品状态，调整状态的依据

继续使用已有生命周期、operation 及 `observed_at`、`observation_stale`、reason 字段。资源状态表达管理操作和下层对目标配置的确认；观测字段表达 Res 是否掌握可用的当前信息。先复用这些字段，避免新增一套平行状态机。

| 输入事实 | Res 的处理 |
| --- | --- |
| 对象身份吻合、下层确认当前配置就绪 | 更新最后确认状态；操作满足其完成条件后结束 |
| 下层明确报告当前配置失败或尚未就绪 | 展示对应状态及原因；可重试故障等待下层恢复，参数拒绝与永久失败分别处理 |
| Res 查询超时、来源不同步或投影应用滞后 | 标记观测异常并保留最后确认结果及时间；不生成“底层网络已坏”的结论，不刷新旧证据时间 |
| 同名不同 UID、归属冲突 | 阻止采用该对象与继续变更，保留冲突原因 |
| 下层对象消失 | 根据已有操作与身份解释删除或意外缺失；不自动换身份重建 |

界面应同时表达“最后确认可用”和“当前待确认”，不能在来源异常时只显示绿色可用。无 Watch 事件不等于失联；`HasSynced`、连接建立或重放缓存也不能给对象续鲜。连接错误、重新同步和应用积压使用现有来源遥测表达。

统一的 60 秒规则拆为两个用途：展示的观测延迟提示、业务动作所需的当前核验。取消单凭持久投影超过 60 秒就拒绝动作的规则；命令核验失败时仍返回真实的依赖不可确认原因。显示阈值和后台校验间隔在固定负载下确定，第一批可保留现有参数以单独衡量结构删减收益。

VM 内网动作只要求本次接入所需的网络能力，不连带要求无关公网 SNAT 就绪；Res 中正在删除、已删除、归属冲突或尚未完成必要管理步骤的资源仍不能使用。定向核验获得的事实必须对应原 binding 和本次业务版本，并在现有事务/租约检查后使用。

Console 的 `src/lib/subnets.ts:subnetIpUnavailableReason` 当前会因 `observation_stale` 禁用部分 LB 网络选择。相应页面应将旧投影作为提示，把最终资格交给正式命令的当前核验；明确的删除、归属错误、非法地址仍不能提交。地址可用范围用于辅助选择，实际分配冲突由 KCN/IPAM 返回，Res 继续保护 EIP 的业务独占。Gov 的转译及其他前端门禁在同批检查，避免只改 Res 后页面仍提前阻断。

### 3.2 三类工作足够

1. **管理命令**：复用当前事务、operation、幂等和租约，提交对应 CR 意图并记录结果。超时未知时继续用原身份恢复发送结果；不能以“下层负责”为由丢弃 Res 尚未确定的请求。
2. **状态投影**：共享 informer 按相关对象变化唤醒现有 worker，更新该资源投影。完成创建后不通过定时改 spec 促使 KCN 重新收敛。多副本旧观察不能覆盖新事实，保留必要的版本与租约检查；删除依赖底层拓扑全集的通用证明要求。
3. **恢复检查**：启动恢复、断线重新同步及有界的共享校验处理漏事件与待执行意图。稳定资源不再逐条按 10 秒领取租约；未完成命令按其持久 deadline 和退避继续推进。无需每个对象重开完整 LIST。

事实顺序保护缩小到实际依赖的对象及其观察范围。无法证明缓存覆盖本次变更时，定向读取对应对象；不通过跨 GVR 的全局证明循环恢复。Kubernetes `resourceVersion` 保持不透明，不能把数值排序当作跨对象的新旧判断。

Get/List/GetOperation 继续纯读。关键动作在自己的上下文内完成定向核验，不靠用户刷新列表推进业务，也不因一次动作触发全局全量采集。

## 4. 已有下层合同与待补能力

KCN 改造不是本次 Res 优化的整体前置条件。优先消费现有下层已经提供的等价证据；字段命名与位置不统一，本身不构成等待理由。仅当某个动作的完成结果没有可靠证据时，保留该动作必要的现有确认或明确结果未确认，并将缺口交给 KCN，不新增 Res 底层补偿。

| 合同 | 当前核对结果 | KCN 需提供的保证与验证 |
| --- | --- | --- |
| 状态属于哪版配置 | VPC/Subnet 有顶层 `observedGeneration`；通用 Condition 未设置它，但 EIP 的 `boundResource.observedGeneration` 已记录所应用的 SNAT generation，SNAT 用它产生 Bound/Disabled，Res 的 `bindingApplied` 也已使用 | SNAT 绑定及启停优先复用现有协议，无需等待字段统一。EIP 自身等未覆盖变更仍需相应版本/结果确认，不能把旧 Ready 当作新配置已应用；标准化可独立交付 |
| 暂时失败会自行恢复 | 本地 `6132623` 中 #47 的分配失败 `return nil` 与缺少容量唤醒仍存在 | 原 EIP 容量恢复必须由 KCN 修复；不阻塞 Res 的读取、调度、准入与状态简化。未修复前如实展示未分配/等待/失败，不能通过 Res 改 metadata 或换 ID 掩盖 |
| 删除代表底层清理完成 | EIP/VPC/Subnet 有各自 finalizer 清理路径；SNAT 删除函数只清 finalizer，网络解绑由 EIP 控制器异步处理 | 按资源类型消费完成事实。SNAT CR 缺失后仍核验原 EIP 已解除原 SNAT 绑定及管理占用一致；不能统一采用“一个 CR 不见就完成”。已有清理流程可先验收与缩小读取范围，不强制等待新接口 |
| 消费者网络释放 | `8b860c6` 已修复 Pod 不存在时 CNI DEL 不再清理 Nic 的分支，包含 sandbox 过滤和重复 DEL 测试；不等于消费者所有派生对象的统一释放保证 | 现有消费者封闭、身份和最终释放确认保留在退出动作；普通状态观察先取消反复深扫。删除最后一层完成确认需可靠等价证据，缺口由 KCN 承接 |
| 原因可定位 | #49 部分 NOT_READY 尚无精确责任定位 | 下层状态带 resource identity、原因和相关配置版本。Res 自身采集失败也保留阶段，不把所有失败统一包装为 KCN 故障 |

EIP 的 `boundResource.observedGeneration` 指向绑定资源的处理版本，足以作为该协议内 SNAT 版本确认的一部分，同时核验原对象身份、绑定目标和启停值；不能直接当作 EIP 自身全部配置已应用的通用证明。用一次新 GET 的时间也不能补出缺失的下层版本确认。

正常删除流程收敛为：Res 封闭新增引用与未决发送 → 按原 UID 请求删除 → KCN 执行清理 → Res 确认对应资源的删除及必要解绑结果 → 结束操作并退休任务。对于由 finalizer 覆盖全部责任的对象，完成可以表现为原对象消失；SNAT 则还要等待 EIP 的解绑结果。Kubernetes finalizer 机制允许对象保持 terminating，直到控制器完成责任并解除 finalizer；不能将一个控制器的 finalizer 自动理解为其他控制器的异步工作也已结束。[Kubernetes finalizer 说明](https://kubernetes.io/docs/concepts/overview/working-with-objects/finalizers/)

未发送且已持久封闭的取消可直接结束；未知发送不能按未发送处理。同名对象更换 UID、管理员绕过正常删除或历史身份冲突进入明确异常处理，不被当作正常释放。KCN 派生资源残留由 KCN 处理；Res 自身晚到的管理命令与身份记录仍由 Res 负责。

终态记录保留原身份、操作结果和必要清理确认。正常终态不继续逐对象巡检；共享事件若发现原管理对象异常重现，可报告并唤醒相应管理恢复。底层派生对象的全量残留扫描属于 KCN 运维诊断，不继续嵌入每个 Res 资源的恢复路径。

## 5. LB 的单独处理

目前没有核实到能替代现有 Res LB 编排的统一 KCN LB 合同，因此第一批保留 Gateway/Route/EnvoyProxy 的产品编排，不为简化另造一个大型 LB CR。KCN 与 Gateway/Envoy 控制器各自承接自己生成的对象和状态。

LB 常规投影目标是读取自身 Gateway、必要 Listener/Route 的当前配置状态，以及其实际使用的 KCN Service/EIP 绑定。已有 LB 的状态不再要求每轮复验全平台镜像、所有控制器 Pod、安装指纹与权限，也不应受到无关基础内网能力的牵连。当前 `requireLBVPCProviderReady` 已将 VPC Provider 就绪与可选基础连接分开，应复用这一方向。

`Programmed=True` 表示配置已被编程到下层，不能单独承诺此刻已有真实流量。因此，删除 `lbGenerated` 中 Deployment/Pod/端口/资源配置的深层推导前，要明确现有控制器实际保证的范围：已应用规格由对应版本化状态确认，数据面健康由其权威运行状态或平台探测报告；Res 不自行推导整条代理链。[Gateway API 状态语义](https://gateway-api.sigs.k8s.io/guides/implementers-guide/#standard-status-fields-and-conditions)

若当前控制器不能提供某项承诺，将缺口交给对应网络平台实现方，并明确该项未满足；不要把已有的规格、HA 副本或流量验收要求悄悄删除。只需要排障时再采集生成对象明细，诊断读取不自动改变业务资源或修补下层。

## 6. 分批实施与退出条件

| 批次 | 实际修改与交付 | 主链退出条件 |
| --- | --- | --- |
| A：Res 独立完成第一条主链 | 调整普通观察与关键动作的区别；移除 VM 因投影年龄单独失败的门禁；对象内读取去重；集群安装检查退出逐资源就绪链；同步 Gov/Console 展示与门禁 | 真实 VPC/Subnet → VM 内网访问 → 退出仍能完成；后台投影延迟而当前定向核验成功时能执行；当前 API/身份核验失败时能正确拒绝。清理暂沿用已存在且必要的路径 |
| B：复用既有 EIP/SNAT 协议 | Res 用现有绑定 generation、目标和启停回执缩小观察范围，删除重复重采；KCN 的 #47 重试修复独立推进 | 正常绑定、停用/启用和解绑符合业务合同；Res 没有用于唤醒 KCN 的变更。原 EIP 容量恢复单独验收，未修时不得宣称此能力已修复，但不阻塞本批 Res 改动 |
| C：终态退休与清理范围收敛 | 先退休已按现有协议确认完成的记录；在退出动作中保留必要的原对象及解绑确认，取消稳定状态的重复深扫；有等价下层证明后再删除余下派生对象检查 | 消费者退出、地址实际释放、网络删除闭环；下层暂停或清理失败时不提前完成；已确认完成记录的任务 epoch 停止增长。尚无等价释放证据的局部检查不拖住其余退休改动 |
| D：LB 收敛及旧路径移除 | 按第 5 节承接权威状态；删除运行期安装审计、生成对象深度推导及已无调用的观察代码 | 当前支持的私网/公网/双入口真实流量与删除通过；观察异常、下层失败、安装问题原因明确；相关旧代码和权限实际移除 |

即使 KCN 保持当前版本，A、B 的现有协议简化、C 的完成记录退休，以及 D 的安装检查迁出都可以推进。依赖 KCN 的是特定缺陷修复，以及删除尚无等价完成证据的最后一层检查；不把它们扩展为整批等待。过渡时保留的既有路径须限定到相应动作，不新增长期兼容分支。每批完成一条真实业务路径后再扩展，避免先搭公共框架或先统一重写全部 worker。

主要改动在 `internal/biz/network`、`internal/data/network` 及其现有 SQL/测试。安装验收的目标位置是现有 `ani-installer`/部署检查，不能在 Res 新建另一个平台审计服务。KCN、Console、Gov 的修改按各自仓库归属交付；本方案没有替这些仓库执行修改或发送问题。

涉及正式语义变更时，同批更新 [ADR-0001](../adr/0001-own-network-lifecycle.md)、[ADR-0004](../adr/0004-observe-cr-with-durable-reconciliation.md)、[持续观察规格](../specs/cr-observation.md)、[VPC/Subnet 规格](../specs/vpc-subnet.md)及对应 Egress/LB/VM 合同。保留 Res 产品生命周期所有权，明确缩小底层观测责任；本提案不静默覆盖现有正式规格。

## 7. 验证、存量与完成标准

验证聚焦真实业务代码和相应外部合同：

- **管理正确性**：跨租户、同名不同 UID、并发更新、EIP 独占继续被保护；worker 重启和未知发送仍按原操作恢复，不重复创建。
- **状态正确性**：采集超时只产生观测异常；下层明确失败能展示；旧版本状态不能完成新操作；延迟投影不会单独阻止当前核验成功的动作。
- **真实业务**：A 的 VM 内网路径、B 的原 EIP 恢复和出站路径、C 的资源退出、D 的实际 LB 入口分别验收。API 受理、Deployment Ready 与真实流量分开记录。
- **清理正确性**：KCN 暂停、清理失败及迟到写入不产生提前释放；清理恢复后占用退出，终态任务停止；不能以修改 state 或删除数据库记录证明完成。
- **复杂度与成本**：普通观察/单次 VM 动作触发的全集关系 LIST 为 0；终态逐对象周期任务为 0；安装权限/镜像核验退出资源就绪热路径；删除对应旧分支与无用权限。共享读取成本主要随观察范围和分页增长，不随“每个资源 × 完整依赖集合”重复增长。

性能按同一集群、对象规模、镜像与客户端预算比较至少一个 300 秒窗口，同时看请求量、状态延迟、操作完成和 DB 调度写入。既有 #48 的 6000 次 GET/LIST 与 #49 的 4728 次属于不同环境，分别作为时点基线；不承诺未经测量的固定降幅，也不用提高 QPS 或延长 TTL 冒充结构删减。

存量保持产品 ID、Provider UID、幂等与 operation 历史。先分类活动操作/未知发送、已完成清理、历史身份异常；只有清理合同与原管理操作都闭合的记录才能退休。不能批量把历史 `Deleted` 清 reason、换 UID 或标记完成。需要迁移时新增 migration，保留旧数据恢复能力，不改已执行 migration。

按能力切换单一执行路径，避免两个实现同时下发同一意图；可先做只读比较，再切换实际执行。回退也沿用原资源身份与持久操作，不重建业务资源。两套集群的部署基线不同，分别固定版本验收，不要求先把所有最新代码部署到一起。

实现测试、生成、构建与集群验收遵循[远程执行约定](../remote-execution.md)和当前用户要求，在远端执行；按影响范围运行必要行为测试与提交门禁。本轮方案交付只做源码和文档检查，未运行新方案业务验收。

## 8. 固定来源

- Resource 远端 `main` 已核对为 `b35365d7e459aad55edefde4270723157c29ea26`。本地阅读工作树为 `7a8edb866e350204819c960c7efe965177b9ae59`；本方案主要落点的九个文件（以下源码加 `cr-observation.md`）通过 GitHub blob SHA 核对与该 main 一致。Git SSH 获取 main 失败后使用连接器核对，没有切换或覆盖用户工作树。
- [普通观察与完整依赖采集](https://github.com/zhangzhe-ctrl/ani-resource-service/blob/b35365d7e459aad55edefde4270723157c29ea26/internal/data/network/kc_observed_provider.go)、[共享观察](https://github.com/zhangzhe-ctrl/ani-resource-service/blob/b35365d7e459aad55edefde4270723157c29ea26/internal/data/network/kc_observation.go)、[VM 准入](https://github.com/zhangzhe-ctrl/ani-resource-service/blob/b35365d7e459aad55edefde4270723157c29ea26/internal/data/network/attachment_vm.go)。
- [Worker](https://github.com/zhangzhe-ctrl/ani-resource-service/blob/b35365d7e459aad55edefde4270723157c29ea26/internal/biz/network/worker.go)、[墓碑观察](https://github.com/zhangzhe-ctrl/ani-resource-service/blob/b35365d7e459aad55edefde4270723157c29ea26/internal/data/network/kc_tombstone_observation.go)。
- [LB 安装能力](https://github.com/zhangzhe-ctrl/ani-resource-service/blob/b35365d7e459aad55edefde4270723157c29ea26/internal/data/network/kc_load_balancer_capability.go)、[生成对象推导](https://github.com/zhangzhe-ctrl/ani-resource-service/blob/b35365d7e459aad55edefde4270723157c29ea26/internal/data/network/kc_load_balancer_generated.go)、[LB 父资源就绪](https://github.com/zhangzhe-ctrl/ani-resource-service/blob/b35365d7e459aad55edefde4270723157c29ea26/internal/data/network/lb_runtime_dependencies.go)。
- KCN 本地源码核对固定在 `6132623baabee50f17e9e179c0f5e9c1f380db04`：`api/networking/v1/conditions.go`、`api/networking/v1/{eip,snat,vpc,subnet}_types.go`，以及对应 controller；部署版本 `ca06a4e06c902b9f9cdc6a6a6a1879792e390fd3` 的 Condition helper 也核对到未设置 `ObservedGeneration`。这属于具体源码缺口，不代表下层清理合同已完成验收。
- 用户追加核对后，指定 KCN 目录仍为上述干净的本地 `main`，最新本地提交日期为 2026-10-30；GitLab 远端 SSH 关闭连接，HTTPS 查询 TLS EOF，未确认远端有无更新。结论明确针对本地版本。
- 追加核对落点：[EIP 分配失败](../../../kc-networking/internal/controller/networking/eip_controller.go)、[SNAT 阶段与 EIP 版本对应](../../../kc-networking/internal/controller/handlers/eip_bound_res.go)、[SNAT 删除](../../../kc-networking/internal/controller/networking/snat_controller.go)、[Pod 消失后的 CNI DEL 修复](../../../kc-networking/internal/daemon/cniserver/handler.go)。`8b860c6884f89dbc9ee17b4518a11f1e8e31e755` 在当前本地 main 中，但不在此前现场自报的 `ca06a4e` 中；本轮只核对代码和已有测试内容，未构建、运行测试或重验现场。
- Console 只读核对工作树 HEAD 为 `ad70a4f2fe5a04722a28ade2b45c86f2bfb14023`，`src/lib/subnets.ts` 存在 stale 前端门禁；该工作树含其他未提交修改，未改动，不据此声称线上 bundle 已确认。
- 现场与源码版本、计数范围及未完成诊断统一引用[原始诊断记录](../execution/records/network-management-simplification-20261103.md)，本轮没有重复运行现场测量。
