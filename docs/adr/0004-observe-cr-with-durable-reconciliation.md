---
status: accepted
date: 2026-09-10
---

# 共享观察 CR，由 Network 持久推进业务状态

当前逐资源轮询与逐 Attachment 扫描同一批 Pod/网卡/IP，使观察成本随资源数和关系集合重复增长。根据本轮选型讨论，Network 采用 client-go dynamic SharedInformer 共享观察及关系索引，变化提示与周期真实校验共同驱动 PG 中的持久工作，由现有领域执行路径统一解释事实、推进状态和恢复。用户要求将此改造作为独立工作包 NET-05A 插在 NET-06 前；本决定记录设计方向，不表示实施或验收完成。

PG 继续保存产品意图、operation、幂等及恢复义务，Kubernetes 保存 Provider 事实。观察通知可以重复、合并或重建，不能承担可靠业务事件的职责；尚无 CR 的已受理意图也必须能够执行。保留来源时效、未知请求、UID 归属、消费者提交封闭和真实删除约束。具体目标行为以[持续观察规格](../specs/cr-observation.md)为准，实施顺序见[工作包方案](../plans/cr-observation.md)。

对当前 Network，直接使用已有 client-go 和 Kratos 装配，避免再建立一套与 PG worker 竞争的执行机制。批量共享轮询也能消除逐 Attachment 重复扫描，但仍需定期全量校验，降低变化感知延迟需要更频繁扫描；因此选择 Informer 提供增量通知并保留真实校验。仅用内存队列则不能恢复尚未生成 CR 的产品意图。比较与主源依据保留在[选型记录](../execution/records/2026-09-10-cr-observation-selection.md)。

推广到兄弟仓库的是所有权、幂等、时效及持久恢复规则，具体框架由各服务选择，controller-runtime 是正式可选方案，也可与 PG 产品意图共存。多个服务可以观察同一对象，但各自只推进自己拥有的产品状态、字段及动作；不同数据库的租约不提供跨服务互斥。不建设中央观察服务、跨仓共享业务 runtime 或统一技术开关，本 ADR 不代替兄弟仓库自己的设计决定。

配额接入明确推迟到 Core 重构及治理契约就绪之后，后续独立编排。NET-05A 不实现配额表、预留/确认/释放 RPC、配额 outbox/dispatcher 或临时账本，也不以这些能力作为退出门槛。现有 Attachment、地址范围和删除保护继续有效，不因配额延期而放松。

## 2026-11-03：缩小运行期职责（已由下节替代）

普通观察仅消费共享事实，采集异常不覆写最后确认的可用/配置结果；动作核验当前直接依赖。已完成清理的任务退休，历史身份保留用于相关事件恢复。镜像、CRD、安装指纹和控制器权限检查迁至显式安装验收，不再决定已有 LB 的运行状态。KCN 负责分配重试和底层清理；没有等价完成合同的消费者释放、LB 规格/HA 及 SNAT 原 EIP 解绑检查仍保留。详见[持续观察规格](../specs/cr-observation.md)与[实施方案](../plans/network-management-simplification-20261103.md)。

## 2026-11-04：以 KCN 状态为权威

用户明确授权完全采信 KCN 当前状态，Res 专注管理端职责。当前实现使用普通 informer 提示、已有任务行、直接目标 GET 和本服务事务提交；删除关系索引、全量审计、Proof/Covers、TTL、父子 Ready 合取以及物理清场验收。原对象 UID 和自身租约只用于声明身份与并发。终态任务永久退休，计算侧当前管理快照决定 Attachment 关系，地址展示独立。

本决定替代本 ADR 旧条款中的全链核验和已退休任务事件恢复要求。方法/字段清单见[实施方案](../plans/network-management-simplification.md)，当前规范见[网络状态消费规格 v3](../specs/cr-observation.md)，实现与验证只见[执行状态](../execution/status.md#network-management-simplification-20261104)。不以 KCN 团队先修改代码为实施前提。
