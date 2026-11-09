---
status: accepted
date: 2026-09-09
---

# Network 独立拥有网络资源生命周期

服务命名与单进程部署边界后续演进见 [ADR-0006](0006-evolve-resource-service-preserving-network.md)；本记录的 Network 生命周期所有权继续有效，以下保留原决定语境。

当前认证按[服务调用合同](../specs/governance-calls.md)执行，网络状态与删除按[状态消费规格](../specs/cr-observation.md)执行。下文首片的身份延期、fresh 环境和旧命令路径仅为历史语境，不覆盖后续明确合同。

ANI 原有网络能力将业务记录、Provider 调用和实例接入知识分散在 Gateway 与共享运行代码中，难以独立迭代。首个拆分试点由 `ani-network-service` 独立拥有租户 VPC、Subnet 及其网络接入关系的生命周期，使用 `kc-networking` 实现底层网络。Gateway 保留产品入口，实例所属服务保留实例创建职责；业务状态、恢复和删除不再依赖 Core 兜底。

本决定记录本轮已确认的架构方向。本轮落实范围为设计文档，不代表业务实现、数据库迁移、跨仓库修改、部署或切流已经开始。术语以 [领域词汇](../../CONTEXT.md) 为准，具体契约以 [VPC/Subnet 规格](../specs/vpc-subnet.md) 为准，当前执行状态只在 [执行状态](../execution/status.md) 维护。

## 所有权决定

| 责任 | 唯一所有者 | 其他参与者的职责 |
|---|---|---|
| 手动 VNIC 的产品身份、原 Provider UID、分配事实与绑定预留 | Network | 实例 owner 保存选择并控制停止、模板应用和启动；KCN 负责 IPAM/CNI。实例退出不级联删除手动 VNIC。 |
| VPC、Subnet 的产品身份、租户归属、约束、期望状态与对外状态 | Network | Gateway 转交命令与查询；实例所属服务按产品 ID 使用资源。 |
| 创建与删除操作、持久幂等、重试、状态推进、失败恢复 | Network | Core 不建立第二份任务、状态机或修复链路。 |
| Network 自有数据、迁移和数据库事务 | Network | 其他服务通过契约访问，不直接读写 Network 表。 |
| Network Attachment 的登记、接入结果、释放与删除保护 | Network | 实例所属服务申请并消费接入结果，在实例结束或创建失败后释放。 |
| kc 网络资源到 OVN 的实现、IPAM 和底层网络收敛 | kc-networking | Network 通过 Provider Adapter 提交意图、观测结果与请求删除，不直接操作 OVN 或分配端点 IP。 |
| Pod/实例的创建、更新、终止及实例状态 | 实例所属服务 | Network 提供接入结果，不代替实例所属服务创建 Pod。 |
| 外部产品路由及面向调用方的传输适配 | ANI Gateway | 使用 Network 契约和客户端；不持有网络数据库、业务状态机或 Provider 控制代码。 |

Network 的深 Module 隐藏持久化、操作执行和 Provider 细节。`internal/biz` 定义用例与所需端口；`internal/data` 实现 PostgreSQL 与 kc Provider Adapter；`internal/service` 适配入站契约；`internal/server` 负责传输和运行装配；`cmd/ani-network-service` 为唯一 composition root。独立服务不依赖 ANI 内部包或生成它的 layout。

## 生命周期与一致性

Network 数据库保存产品事实和待执行意图。资源、对应操作和幂等记录按规格在 Network 本地事务内提交，worker 按持久记录执行；请求进程退出不能丢失后续处理。worker 负责管理命令投递重试、观测、状态推进和未决请求恢复；KCN 负责其分配/网络收敛重试，Res 不通过改 annotation/spec、换 ID、OVN/IPAM 写入或强制去 finalizer 兜底，Get/List 是纯查询，不靠读请求触发修复。

Network 是产品状态的唯一写入者，kc 是底层网络状态的权威来源。Provider 请求成功不能直接等同于网络可用；Network 根据规格要求的最新观测推进状态，并保护并发执行和迟到结果。删除必须驱动 Provider 释放资源、确认相应对象已不存在，再完成 Network 的删除状态；只改数据库记录不构成删除完成。

Network Attachment 将实例消费与网络删除串联起来。接入登记、删除准入和释放必须形成可恢复协议，不能仅靠一次远程查询判断“当前没有实例”后删除子网。实例所在服务仍拥有实例生命周期，其操作与 Network 的本地事务之间通过契约和恢复流程协调，不建立跨服务数据库事务。

自有持久化采用 PostgreSQL、sqlc/pgx，不使用 RLS。租户资源及其操作、幂等和关联记录携带 `tenant_id`，业务读写显式限定租户，资源关系使用保持租户归属的复合约束。这里的租户是资源所有者，不是创建者的身份；不为 VPC/Subnet 引入 Notification 的 HumanPrincipal Scope，也不把 `tenant_id` 强加给不属于租户的全局配置或数据库版本记录。

## 首片范围与取舍

首片面向 fresh 环境中的新资源，验证 VPC/Subnet 的完整生命周期，以及现有普通容器消费者接入后的基本连通和租户隔离。沿用合理的 ANI 产品 OpenAPI 表达，对不合理字段及状态语义按规格进行明确变更；不保留旧 Network 数据、Provider、客户端或存量资源的兼容读写，不建立新旧双权威。旧 Storage/LB/Route 的关联不进入本片验证资源。

替代方案是仅把旧代码搬到新进程，继续由 Core 维护任务、恢复、Provider 接入与实例网络判断。该方案减少初期迁移工作，却保留跨服务状态所有权和修复依赖，无法检验独立 Network 是否真正可迭代，因此不采用。另一种方案是让 Network 直接创建 Pod，这会取得实例生命周期所有权，也不采用。

Gateway 的最终使用方式可以表现为接线变化，但交付需要同时完成 Network 客户端、实例网络解析与接入 seam、Console 状态展示；修改目标地址本身不足以替换旧 Kube-OVN 绑定知识。首片不以全平台 Core、Task 或 Quota 重构为前置条件，也不顺带迁移全部网络能力。

验收顺序是普通容器先闭环，VM 后续验证。用户之后提供的是运行 kind 的宿主虚拟机；KubeVirt 所需能力和业务 VM fixture 届时另行核定，不能从普通容器结果推导 VM 已可用。kc Provider 缺口由 kc 团队处理，不在本仓库计划中增加 kc 实现工作包。IAM 服务间验证暂不加入本期，待 IAM 准备好后独立接入；该后续接线不改变租户资源所有权与数据库隔离规则。

工作包划分见 [实施计划](../plans/vpc-subnet.md)，本次静态检查依据见 [源码评估记录](../execution/records/2026-09-09-source-assessment.md)。
