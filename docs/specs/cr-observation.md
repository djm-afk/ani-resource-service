# 网络状态消费与持久执行规格

日期：2026-11-04。版本：3。对应[实施方案](../plans/network-management-simplification.md)与[执行状态](../execution/status.md#network-management-simplification-20261104)。

本规格取代 v1/v2 中的共享证据、覆盖代次、时效、物理关系闭包和周期审计合同。实施、生成、验证和部署是独立事实，实际进度只记录在执行状态中。

## 1. 状态权威与管理职责

KCN 提供网络资源的运行状态。Res 将主要状态和输出映射、持久化并展示；KCN 的内部状态实现不作为 Res 的第二套控制器。Res 自己负责租户范围、资源账本、用户配置、产品引用、操作幂等和并发，以及自己提交的声明对象。

VPC、Subnet、VNic、EIP、地址池直接采用当前对象的 Ready 三态。SNAT 直接采用自身 phase 及 applied 值。KCN 未报告状态时，Res 保留已有稳定状态；读取失败时保留上次成功读取的资源状态、原因、地址、applied 值和 observed_at。读取错误写入任务的 observation_error，活动 operation 可以记录本次办理错误。读取成功才清除该错误。

| 当前情况 | 资源投影 | operation |
| --- | --- | --- |
| Ready=True | Available，清当前状态原因；历史 Failed 可恢复 | 当前尚未完成的操作可完成 |
| Ready=False | 创建中继续 Provisioning；原稳定资源为 Degraded；原因取原生 condition | 继续等待控制器收敛 |
| 状态未报告 | 保留已有稳定状态；首次创建继续 Provisioning | 继续等待 |
| GET 连接错误、超时或拒绝 | 保留全部成功投影 | 记录读取错误并按本服务策略重试 |
| 自己已登记的原 UID 消失 | 普通观察记目标缺失；不按旧 UID 重建 | 删除可直接完成 |
| 首次声明明确提交失败且没有原对象 | Res 的声明操作可失败 | 保存实际提交失败 |

已完成 operation 的 state、reason、attempt 和 completed_at 保持历史含义。新的 KCN 状态改变当前资源，不改写过去的操作结论。

## 2. 通知与任务

普通 client-go informer 只通知 namespace/name。NotifyObservedObject 将精确对象键映射到已有 Res binding、已声明 LB component、当前 Attachment Pod/VNic、冻结的 VirtualSwitch Node、默认出口自己的 EIP/SNAT 和当前安全组目标。

工作执行者直接 GET 自己正在处理的对象。KCN 子对象、地址分配过程、父网络 Ready、Node 在线、控制器镜像和生成工作负载均不进入资源 Ready 合取。普通 informer 的 LIST/WATCH 恢复由 client-go 负责。

六类任务使用相同的管理排程规则：租户资源、平台资源、Attachment、VirtualSwitch、默认 VPC 出口、安全组。

- requested_generation 和 processed_generation 只计数 Res 的任务通知；Claim 记录 ClaimedGeneration。
- Notify 增加待处理计数并提前 due time，保留当前 lease、epoch、产品 version 和失败退避下限。
- Finish 在事务内验证 tenant、产品版本、lease owner/epoch 和有效期，只消费本次领取的通知。
- Claim 之后的新通知保留下一次执行机会。执行失败遵守 retry_not_before。稳定任务停放为 infinity。
- 已结束的 Deleted/Released 任务退休，事件和启动恢复均不重新激活。历史 binding、UID、幂等回执和 operation 继续保留。
- 每次进程启动只重排本集群已持久的非终态任务一次，覆盖停机期间的删除。未完成操作和失败重试仍有自己的持久 deadline。

任务不保存 evidence_hash、evidence_applied_at 或 provider proof。observed_at 是本服务成功读取的事务时间，不是 TTL 或业务准入条件。

## 3. 声明对象和删除

对象身份来自 Res 接受操作时保存的 cluster、namespace、固定 name、binding 与原 UID。PATCH/DELETE 保留 UID/resourceVersion 并发前提和本服务租约校验。它们只约束本服务写哪个声明对象。

未知结果的首次 CREATE 沿用原名称重试；一旦保存原 UID，缺失不能自动变为新一代资源。删除读到原对象 NotFound 或同名不同 UID 即结束原对象的删除，不删除替代对象，不等待 KCN 自动子资源清场。

资源引用和释放顺序来自 Res 自己的账本。自动基础内网出口按自己的 SNAT、EIP 产品顺序删除；它们的状态展示不覆盖 VPC 的 Ready。VPC 本体消失可以完成 VPC 删除，子产品自身的完成事务继续推进后续清理。

## 4. Attachment 与计算侧

Attachment 记录消费者对网络的管理占用。Prepare 预留；Confirm 登记当前消费者绑定；Release 使用同一 finalization 的计算侧关闭快照完成释放。open/stopped 保留预留，closing 按管理关系推进，closed 释放。当前地址是否存在不决定这些状态。

当前消费者快照只包含 scope、当前 generation/revision、state、desired run state、finalization、closed_at、current pods 和 manual VNics。历史控制器链、退休 Pod 和关系摘要退出 Network 合同。计算侧在自己的持久快照变化后通知 Attachment；计算库和网络库可分开部署，通知投递确认只保存在现有计算侧 JSON 中，并复用原有持久工作重试，不影响网络状态。

地址展示采用四列 current_vnic_name、current_vnic_uid、current_mac_address、current_ips：

1. 精确读取当前登记的 Pod；有原 Pod UID 时只认该消费者实例。
2. 容器采用 Pod.status.podIPs/podIP；VM 采用当前 Pod 的 network-status 默认网络 ips/mac。
3. 当前 VNic 名采用 Pod 的显式主网注解；没有名字就没有 VNic 回退查询。
4. MAC/IP 尚未报告时，精确 GET 该 VNic 补充其已报告输出，不检查 Ready 或 VNicIP。
5. 同一当前消费者读错保留旧地址；明确 stopped/closing/closed 或消费者更换时撤回旧地址。

VM/Instance 的启动、停止、删除、安全组配置，以及手工 VNic 的使用，依据自己的管理状态推进。Network Ready、地址非空、Reason 为空和观测年龄均不再成为动作条件。

## 5. Egress、平台配置与 LB

地址池的分配开关、默认池、CIDR/用户配置冲突、租户引用和版本属于 Res 的管理规则。RecordPublicPoolVerification/RecordIntranetPoolVerification 只真实存档管理员材料。材料不触发 provider 操作，不替换正在运行的 last_operation_id，不重新证明镜像、拓扑或过期时间。

GetIntranetPoolDefaults 仅在显式管理员请求中选择 kcn-system VPC 的 status.default=true；intranet_networks 为可选管理员记录，不通过基础设施扫描推导。

默认 VPC 出口只管理自己声明的 EIP 与 SNAT，状态取这两个原对象。VirtualSwitch 只对管理员选择并冻结的节点登记本服务拥有的配置和注解，不以节点在线或旧观测时间决定配置有效性。

LB 只读取自己声明的 EnvoyProxy、Gateway、Backend、Route 和 Policy。条件选择属于对应 Gateway/listener/controller 的原生状态。原生 observedGeneration 用于确认自己的这次配置版本被处理，不会把旧稳定状态改成 Failed。EnvoyProxy 没有 Ready 合同，其声明提交完成即可。

LB 保存 desired_version/provider_target_version/applied_version、后端引用、配置、占用和声明 UID。组件删除完成后释放自己的账本，不等待生成 Deployment/Pod/Service/EndpointSlice、转发健康或 VIP 分配释放证明。data_plane_state 保留旧字段编号并返回 UNKNOWN，data_plane_observed_at 为空。

## 6. 安全组

现有安全组 worker 处理 security_group、eip、container、vm、vnic 五类任务。安全组自身采用原 CR Ready；目标应用结果直接读取精确 EIP/VNic 的 appliedSecurityGroups，再通过 Res 的租户/namespace/名称映射为产品 ID。

计算侧或手工 VNic 的配置 PATCH 成功后只记 configured 并通知现有任务，不能把 desired IDs 复制成 applied IDs。没有当前 VNic 时保留 configured，后续当前消费者变化唤醒；没有反向 LIST 或成员闭包。目标 released/deleted 时同一事务退休任务。

## 7. API、配置与运维

公开资源枚举和 protobuf tag 保持兼容。observation_stale 仅表示最近读取错误或尚无成功观察，永不按时间使业务状态失效。废弃的控制器链、拓扑/镜像证明、目标 KCN 代次及数据面健康字段保留原编号，不再提供运行验收结果。

旧 audit_interval/audit_jitter/audit_timeout/flush_interval/queue_capacity、Worker.stale_after 和 LB 安装指纹/镜像配置继续可解析并标记 deprecated，运行时忽略。保留 request QPS/burst、worker 并发、lease/request timeout/retry 和未完成工作排程。安装诊断是显式管理员操作。

遥测记录 API 请求、读错、通知重试、任务排程和数据库运行情况，不生成可用性证明。运行角色只保留当前声明、明确地址读取和本服务管理写入所需权限。

## 8. 升级与验证

0032 为协调升级：停止旧 schema 的写入者，搬运已保存的当前地址输出，删除旧证明/关系列和 generated 表，收敛约束，清旧 lease 并增加执行 epoch；保留产品状态、历史 operation、原 UID、用户意图和幂等回执。生成源码、构建、必要数据库回归按 [远程执行约定](../remote-execution.md)在 Fedora 运行。

验证重点是原生 Ready 的直接映射、读错保留、终态历史不可变、通知与 Finish 并发、未知 CREATE、原 UID 删除、管理生命周期与地址分离、安全组真实 applied 字段，以及旧 schema 升级后的账本/回执保留。详细用例及实际结果分别见实施方案和执行状态。

## 9. 平台基础网络目录的只读观察

导入的 VPC/Subnet 使用独立 `network_platform_network_observations` 队列，接入现有 Worker 的显式只读分支。任务固定 cluster、kind、namespace、name 和原 UID，只精确 Observe/Finish；不生成虚构 operation，不调用 Ensure/Delete、添加所有权标签或分配基础连接。

原 UID 匹配的成功读取更新资源投影和 observed_at；Ready False/Unknown 同样是成功读取，完成后停放至下一条精确 Watch 通知。读取失败保留最后成功状态、原因和地址，记录 observation_error 并沿原重试策略退避。UID 冲突保留为新接入阻断，之后的普通读错不能清除它，只有成功读取原 UID 才清除。

配置同步、进程恢复和精确 Watch 沿原持久 generation/lease/epoch 机制唤醒目录项；目录变化不扫描或扇出消费者。目录 published/accept_new 不参与已受理消费者的释放和删除。所有权、查询与配置合同见 [VPC/Subnet](vpc-subnet.md#平台基础网络租户接入)。
