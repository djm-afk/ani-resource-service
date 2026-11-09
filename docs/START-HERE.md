# ANI Resource 文档导航

[Res 全域管理职责与简化原则](specs/res-responsibility-and-simplification.md)：适用于 Res 全部现有领域、存量实现和未来能力；领域合同在这些职责边界内规定具体对象、权威事实、状态和完成条件，旧文档或旧实现不能豁免越权逻辑。按用户更新的 2026-11-09 附件原位替换；Network/VM 是已完成试点，不代表全域推广完成，实际结果继续以执行状态和对应记录为准。

[平台基础网络租户接入](specs/vpc-subnet.md#平台基础网络租户接入)：同一平台目录、真实租户消费者、首次受理开关和只读观察；VM 两轮准备见 [VM 合同](contracts/vm-v1.md#c05-平台基础网络与两轮准备)，容器重放见 [Instance 合同](specs/instance-configuration-network.md#平台基础网络接入)。编码交接见 [PR #62](https://github.com/zhangzhe-ctrl/ani-resource-service/pull/62)，实际发布与真实验收见[执行状态](execution/status.md#platform-base-network-tenant-access-20261105)。

[2026-11-04 网络状态消费简化](plans/network-management-simplification.md)：完全采信 KCN 原生状态，移除 Res 网络证明与审计逻辑；正式合同为[规格 v3](specs/cr-observation.md)，实现/验证/部署分别见[执行状态](execution/status.md#network-management-simplification-20261104)。

[2026-11-03 网络管理简化](plans/network-management-simplification-20261103.md)：Res 管理意图与结果，KCN 负责底层收敛；该历史方案的观察条款现已由[网络状态消费规格 v3](specs/cr-observation.md)替代，实际结果见执行状态。

[LB 使用方式对齐（2026-11-02）](specs/vpc-connectivity-lb.md)：公共 `lb-common`、每 LB 专属 Proxy 和独立于基础 Intranet 的运行准入；实际迁移、前后端和业务验证见[执行状态](execution/status.md#lb-usage-alignment-20261102)。

[公网 EIP 独立绑定与关联展示](specs/vpc-connectivity-lb.md#5-public-eip-与-vpc-公网出站调整)：无基础网络准入、VPC/EIP 同快照关系和 EIP 创建选组；实际验证见[执行状态](execution/status.md#public-eip-workflow-20261030)。

[租户安全组](specs/security-groups.md)：容器/VM/EIP 接入、默认仅入方向拒绝、持久当前配置及真实应用判定；实际结果见[执行状态](execution/status.md#security-groups-20261030)。

GPU 容器实例 v1.5：[规格](specs/instance-mvp.md)、[实施计划](plans/instance-mvp-v1.5.md)、[运行与恢复](runbooks/instance-mvp-fedora.md)、[当前执行状态](execution/status.md#gpu-container-instance-mvp-v15)。前后端接线与实际验证范围按执行状态分别记录。

容器最终删除与退额：[2026-10-27 整改方案](plans/container-instance-release-without-acc-20261027.md)移除 Resource 对 ACC 释放确认的依赖，以 owner 自身管理责任完成事实和持久 outbox 复用 Gov 现有退额接口；正式行为见[实例规格 §10](specs/instance-mvp.md#10-完整释放及-gov-原账退额)，实际实施与验证只见[执行状态](execution/status.md#container-instance-owner-refund-2026-10-27)。

容器实例配置与 VPC 接入：[增量设计](specs/instance-configuration-network.md)、[IC-01～IC-05 计划](plans/instance-configuration-network.md)；实施与真实验收只看[本任务状态](execution/status.md#instance-configuration-and-network-2026-10-19)。

这是本仓库唯一正式文档导航。项目入口为 [README](../README.md)，工程约定为 [AGENTS](../AGENTS.md)。
当前实施与验证进度只在 [执行状态](execution/status.md) 维护，本文不复制进度表。

[算力目录与公共解析](specs/compute-presets.md)定义 compute 管理、列表、用途绑定的只读候选及与各消费者的组合边界；[原地迁移与恢复](runbooks/compute-presets-migration.md)定义规格表/账本与匹配 Gov 权限维护步骤。实例配置使用唯一格式，生命周期合同继续有效；开发环境清理后通过009切换，见[配置合同](specs/instance-configuration-network.md#7-当前配置合同与原账)。

[Resource 改名历史计划](plans/resource-service-modularization.md)：R0–R5 的原来源、兼容检查和执行范围；当前 Compute、Storage 等领域从下方正式合同进入，不将原批次的“后续切片”作为当前实现状态。

容器重启与已有 PVC 挂卸：[生命周期](specs/instance-mvp.md#84-restart-与挂卸操作)、[五个 RPC 与动作权限](specs/instance-mvp.md#111-用户-api)、[当前配置/引用代际/Storage 删除保护](specs/instance-configuration-network.md#43-持久化和投影)、[008 切换与向前恢复](runbooks/instance-mvp-fedora.md)、[精确服务调用登记](specs/governance-calls.md)。实施输入仍为[已审 v2.0 方案](plans/container-instance-restart-pvc-20261028.md)，来源 SHA-256 `b6dfb17aef74f3d99cc969ecd2ef51b19c77b0a984f82fbe6afd28aa94d41f7a`；部署、A35/A36 边界和实际验收只见[唯一执行状态](execution/status.md#container-restart-pvc-20261028)。

[标准租户 Namespace 与镜像授权合同](contracts/tenant-namespace.md)：固定 canonical UUID/name/归属标签/原 UID，区分 Sandbox 开通与其他 owner 只读复用；ModelDev/Inference 标准租户授权和既有静态 caller 的实际边界以该合同为准。

## 阅读顺序

1. [领域词汇](../CONTEXT.md)与[全域职责原则](specs/res-responsibility-and-simplification.md)。
2. 选择本次所涉领域的正式合同，并查阅当前[执行状态](execution/status.md)中的实现、验证和已知偏差。
3. 查阅适用[架构决定](#架构决定)，将其历史范围与后续替代决定区分。
4. 仅在任务需要时引用相关实施计划和历史证据；旧 Goal 不授权当前执行。

## 领域合同

| 领域 | 正式入口 |
| --- | --- |
| Network | [VPC/Subnet](specs/vpc-subnet.md)、[状态消费与管理生命周期](specs/cr-observation.md)、[Public/SNAT](specs/vpc-snat.md)、[VPC/LB](specs/vpc-connectivity-lb.md)、[安全组](specs/security-groups.md)、[Underlay/交换机](specs/kcn-v09-underlay-flow.md) |
| Compute | [目录与只读解析](specs/compute-presets.md) |
| Image | [规格](specs/image-mvp.md)、[API/凭据交接](specs/image-api.md)、[方法](specs/image-methods.md) |
| Storage | [直接 PVC 管理](specs/storage-volumes.md)、[S3 对象管理](specs/object-storage.md)、[部署与手工绑定](../deployments/storage/README.md) |
| Instance | [生命周期/原账](specs/instance-mvp.md)、[当前配置/借用卷/网络](specs/instance-configuration-network.md) |
| Sandbox | [管理与生命周期](specs/sandbox.md)、[实施顺序与未来能力](plans/sandbox-runtime-design-20261025.md) |
| VM | [规格](specs/vm-mvp.md)、[字段和跨域合同](contracts/vm-v1.md)、[验收层次](verification/vm-mvp-acceptance.md) |
| ClusterOps | [平台集群运维规范](specs/cluster-operations.md)、[v1 调用合同](contracts/cluster-operations-v1.md) |
| 共通入口 | [标准 Namespace](contracts/tenant-namespace.md)、[受信服务调用](specs/governance-calls.md)、[运行装配](runtime.md) |

公网出网增量：[租户 VPC SNAT 方案](specs/vpc-snat.md)定义 Overlay/Underlay、平台与租户接口及生命周期；[实施计划](plans/vpc-snat.md)安排依赖和后续 Underlay 验收；[操作手册](kc-public-egress-manual.md)提供产品 RPC 顺序及 kc CR/YAML 核对参考；[出网部署前置](../deployments/egress/README.md)说明额外 RBAC、冻结节点身份和本服务拥有的组标签配置。

2026-09-14 新统一设计：[VPC 基础内网、公网出站与 LB 方案](specs/vpc-connectivity-lb.md)定义新规则及对旧流程的调整；[13 个可执行任务](plans/vpc-connectivity-lb.md)给出依赖、修改范围与验收条件；[方案交付与来源核对](execution/records/2026-09-14-vpc-lb-plan.md)记录固定代码和手工实测的证据边界。本文档交付不代表新流程已实现。

[LB 监听器延迟下发](specs/lb-deferred-activation.md)定义空监听器保存、运行投影、清空与再激活合同；实际进度只见 [LB-DEFERRED-01](execution/status.md#lb-deferred-01)，证据入口为 [工作包索引](execution/records/LB-DEFERRED-01/README.md)。

[历史未知创建恢复提案](plans/provider-unknown-create-recovery.md)区分退出结果落库修正与旧回执缺失的恢复前提；本次实验例外已按用户授权恢复并复测；通用管理接口未实施，实际结果见执行状态。

[Sandbox 未知创建只读诊断](runbooks/sandbox-unknown-create-review.md)输出目标、凭据身份及 Provider 观察，保留未知屏障；报告本身不是持久审计或恢复证明。

[Sandbox 正式规格](specs/sandbox.md)定义当前目标合同；[实施计划与历史来源](plans/sandbox-runtime-design-20261025.md)只保留阶段顺序、未来目标与设计输入。现有实现偏差、实际部署和验收只见[执行状态](execution/status.md)，不把规范更新当作代码已经整改。

## 架构决定

| 记录 | 决定 |
|---|---|
| [ADR-0001](adr/0001-own-network-lifecycle.md) | Network 自己拥有资源、operation、worker、状态、恢复和真实删除，Core 不兜底；实例 owner 仍拥有 Pod/VM。 |
| [ADR-0002](adr/0002-use-tenant-owned-data-without-rls.md) | 独立 PostgreSQL + sqlc/pgx；租户资源显式 tenant 约束，无 RLS、共享写表或跨服务 FK。 |
| [ADR-0003](adr/0003-defer-workload-authentication.md) | 历史身份延期决定已被当前 mTLS/可信委托合同替代，不能作为认证旁路。 |
| [ADR-0004](adr/0004-observe-cr-with-durable-reconciliation.md) | 最新 11-04 决定采用原生状态、精确通知和已有持久任务，旧审计/闭包/TTL 已替代；兄弟领域按实际契约选择机制。 |
| [ADR-0005](adr/0005-separate-vpc-connectivity-and-exclusive-eip-bindings.md) | 用户确认 EIP 目标独占，VPC 内网/公网 SNAT 分用途限制，基础内网资源随 VPC 管理。 |
| [ADR-0006](adr/0006-evolve-resource-service-preserving-network.md) | 原历史上演进 Resource 单进程，保留 Network 数据、契约、资源身份与回退边界。 |
| [ADR-0007](adr/0007-image-domain-and-private-registry.md) | Image 领域自治、登记 Digest 与私有仓库；运行身份通过统一 Secret writer 交接。 |

ADR 记录已确认的方向及理由；规格中本轮补齐的数值、字段和协议细节是工程设计，不冒充已经逐项人工批准或实际验收。
当前用户明确决定优先。规格、ADR、代码或证据出现差异时，标明是待实现目标、过期材料还是需要变更的决定，并更新对应权威材料，不能静默挑选有利版本。

当前观察、调度与验收合同统一见[持续观察规格](specs/cr-observation.md)；既有资源与 Attachment 协议仍以首片规格为准。实际源码、分层验收和容量边界见 [NET-05A 实施记录](execution/records/NET-05A-implementation.md)，当前结果只看执行状态。

管理员逐条手工复现三项 Public 转发问题：[命令与独立 YAML](runbooks/public-forwarding-manual-20260917/README.md)。此入口是手工 Provider 诊断，准备时未执行新资源创建或流量复验，不替代产品验收。

## 文档职责

| 位置 | 唯一职责 |
|---|---|
| 根 `CONTEXT.md` | 领域词汇定义，保持简短，不放 SQL、传输字段或执行步骤 |
| `docs/specs/` | 应有行为、契约、数据和验收条件；不维护实施进度 |
| `docs/adr/` | 重要取舍的上下文、决定和理由；不另复制整份规格 |
| `docs/plans/` | 工作包、依赖、实施顺序和退出证据 |
| `docs/execution/status.md` | 唯一当前状态及下一步 |
| `docs/execution/records/` | 带日期/工作包的来源与执行证据，保留时点与限制 |

正式材料集中在 docs；不新增另一个根 evidence 目录或 .scratch 当前规格。
源码契约、SQL、migration 和测试保留在其实际源码位置，由规格链接而非复制。
目录只在有实际文件时创建。过期规范注明替代关系；历史执行记录不随当前状态变化而重写成新的证据。

## 运行与来源

- [2026-11-09 文档清理修复](execution/records/res-doc-fix-20261109.md)：全域复扫清单的文档处理、历史引用维护和静态校验；D18/D19 代码整改仍未完成，当前进度见唯一执行状态。

- [2026-11-09 全域原则复扫与待清理清单](execution/records/res-doc-review-20261109-global.md)：按最新版原则复核全部现有领域，列出保留、升级及新增项，并区分文档错误、存量职责整改和证据位置问题；初审记录仅保留原时点结果，后续处理进度仍只见执行状态。

- [Governance VPC 只读接入](specs/governance-vpc-read.md)：用户/API Key 的受信 mTLS 查询入口；原始验证见 [2026-09-22 记录](execution/records/governance-aksk-vpc-20260922/README.md)。
- [Governance → Resource 调用登记](specs/governance-calls.md)：现有 Network/Image 方法、唯一证书身份、租户范围、超时和幂等，以及各装配模式共用业务 mTLS 的边界。

- [NET-VPC-LB-02 实现与分层验收](execution/records/NET-VPC-LB-02/README.md)：固定输入、LB 持久生命周期及受控/真实数据面边界；[隔离产品 API 操作](../deployments/load-balancer/README.md)；[Public 转发失败排查交接](execution/records/NET-VPC-LB-02/device-registration-20260915/public-forwarding-followup.md)。

- [NET-VPC-BASE-01 实施与受控验收](execution/records/NET-VPC-BASE-01/README.md)：六卡实现、固定源码、分层验收、补齐工具及下一批输入；[Intranet 平台运行前提](../deployments/egress/intranet.md)。

- [VPC SNAT 本仓实现与验证](execution/records/VPC-SNAT-IMPLEMENTATION/README.md)：固定设计输入、接口/事务/worker 落点、远端必要门禁、kc 外部阻塞与清理证据。

- [2026-09-10 kind Overlay EIP/Snat 实测](execution/records/KC-OVERLAY-20260910T114200Z/README.md)：原始流程空下一跳失败、测试路由对照、实际流量与清理差异。
- [NET-05 普通容器真实网络验收](execution/records/NET-05-implementation.md)：实际 main、数据面、故障、权限与清理证据。
- [运行说明](runtime.md)
- [运行验证](runtime-verification.md)：已有通用骨架门禁的范围。
- [执行与源码隔离约定](remote-execution.md)：当前环境先按本次指令和个人清单确认；历史 NET-05A/SNAT/R0–R5 参数只供追溯。
- [生成溯源](scaffold/provenance.md) 与 [运行依赖 SBOM](scaffold/bom.cdx.json)
- [2026-09-09 源码评估](execution/records/2026-09-09-source-assessment.md)：ANI、IAM、Notification、kc 的设计输入与快照。
- [2026-09-09 文档交付检查](execution/records/2026-09-09-design-verification.md)
- [2026-09-10 CR 持续观察选型](execution/records/2026-09-10-cr-observation-selection.md)：选型研究与社区主源；已由 ADR-0004 和持续观察规格承接设计，配额讨论仅保留为后续输入。
- [2026-09-10 NET-05A 方案检查](execution/records/2026-09-10-cr-observation-plan.md)：文档交付、编排与静态检查，不代表实施验收。

本仓库独立拥有生成源码，不在构建或运行时依赖 layout 或 ANI 内部目录。
旧版 START-HERE 中的初始化交接及待讨论问题由上述词汇、ADR、规格和来源记录取代；历史版本仍可从 Git 查询。

## VM 一阶段职责整改

R01–R09 的已归档整改范围与对应行为见 [VM 规格](specs/vm-mvp.md)，CDI 内部故障的限定入口见 [运维接替](runbooks/vm-image-cdi-handoff.md)。实际代码、测试、部署及未完成事项只见 [执行状态](execution/status.md#vm-phase1-20261106)。该记录不表示整机克隆二阶段已实现，也不限制后续任务的实际授权范围。

## VM MVP

- [虚拟机一期规格 v2](specs/vm-mvp.md)：Linux/Windows、ISO 与 qcow2/raw、Block/RWO 自有盘、多盘、VNC、原生热插拔、在线/离线快照恢复及租户主网的唯一业务规则。
- [VM v1 正式合同](contracts/vm-v1.md)：API、action/usage/codec、compute、Network v3、可信 console binding 及 Gov 删除持久受理栅栏的唯一字段/接线规则。
- [纵向实施计划 v2](plans/vm-mvp.md)：VM-00 至 VM-05B 共16卡；先贯通真实Linux主链，再补齐一期范围。
- [VM MVP 历史执行 Goal](plans/vm-mvp-complete-goal-20261023.md)：原 16 卡范围、来源与历史授权；其机器、集群、连续执行和维护窗口不自动适用于新任务。
- [验收矩阵 V01–V25](verification/vm-mvp-acceptance.md)：CODE/PG/BOUNDARY/LIVE/GUEST/BROWSER及双系统必需格子，含Gov删除投递暂停期间的受理栅栏；本文件不维护执行结果。
- [制包来源](references/vm-mvp-sources.md)、[设计复核历史](execution/records/VM-MVP/design-review-20261023.md)、[制包校验历史](execution/records/VM-MVP/package-validation-20261023.md)：固定输入与历史证据，不作当前环境或实施状态。
- [首卡 Goal](plans/vm-mvp-goals/VM-00.md)、[维护窗口 Goal](plans/vm-mvp-goals/VM-00R.md)；其余16卡文件在同目录。它们是原批次的实施输入，当前任务不得从中自动取得连续执行或共享环境权限。
- 空白模板：[环境输入](plans/vm-mvp-templates/environment-inputs.yaml)、[runtime变更准备单](plans/vm-mvp-templates/runtime-change-plan.md)、[任务报告](plans/vm-mvp-templates/task-report.md)。填写后的输入、变更单、报告统一放入docs/execution/records/VM-MVP/。
- [VM候选部署交接](execution/records/VM-MVP/vm-candidate-deployment-handoff.md)：实际候选来源、依赖顺序、迁移/密钥和部署/回退边界；Resource/Gov/SG实现任务ref及三个Harbor镜像已发布并核对digest，实际引用和未验/未部署边界以交接回执为准，不复制验收矩阵。
- 当前结果仅见[唯一执行状态](execution/status.md#vm-mvp)。

### 文档接入与唯一状态

不可变输入包为ani-vm-mvp-v2-20261023.zip，SHA-256为ef781d20e31f02ecdd6e3e0f98dcd3bd77ee38a3b790b0a69b0ef86ecd49a10d。下表保留当时 VM-00 的接入映射，仅作来源追溯；新任务不据此重新导入旧规范、覆盖后续修订或限制已获授权的文档维护。迁移后的Markdown链接及Goal文字路径均指向仓内目标；包README引用统一指本VM入口。

| 包内文件 | Resource目标位置 |
|---|---|
| docs/specs/vm-mvp.md | [docs/specs/vm-mvp.md](specs/vm-mvp.md) |
| docs/contracts/vm-v1.md | [docs/contracts/vm-v1.md](contracts/vm-v1.md) |
| docs/plans/vm-mvp.md | [docs/plans/vm-mvp.md](plans/vm-mvp.md) |
| docs/verification/acceptance.md | [docs/verification/vm-mvp-acceptance.md](verification/vm-mvp-acceptance.md) |
| docs/references/sources.md | [docs/references/vm-mvp-sources.md](references/vm-mvp-sources.md) |
| docs/review/consistency-review.md | [docs/execution/records/VM-MVP/design-review-20261023.md](execution/records/VM-MVP/design-review-20261023.md) |
| docs/review/package-validation.md | [docs/execution/records/VM-MVP/package-validation-20261023.md](execution/records/VM-MVP/package-validation-20261023.md) |
| goals/*.md | docs/plans/vm-mvp-goals/，保留16个原文件名 |
| templates/* | docs/plans/vm-mvp-templates/，保留3个原文件名，模板保持空白 |
| README.md | 不覆盖仓库README；由本VM入口承接映射及接入规则 |

唯一当前状态始终为 docs/execution/status.md；VM-MVP/ 为原批次证据，VM 一阶段及后续工作使用同一状态文件中的各自日期/记录索引。Gov、SG和前端记录链接到该索引，不再建立第二套总状态。两份历史复核仅保留制包时事实，不重写为实施通过声明；目标行为和计划也不作为产品验收证据。

## Image MVP

- [2026-10-13 tenant-a 前端演示手动操作](execution/records/IMAGE-MVP/frontend-demo-tenant-a-20261013/README.md)：真实租户空间、两类镜像推送/登记和恢复记录。
- [2026-10-08 后端交接](execution/records/IMAGE-MVP/HANDOFF-20261008.md)：业务主链断点、代码入口、验证范围与后续接入条件。
- [2026-09-30 节点缓存补充清理](execution/records/IMAGE-MVP/img-cleanup-20260930T0344Z/README.md)：历史测试资源复查、三节点精确镜像删除和校验和。
- [规格](specs/image-mvp.md)、[API](specs/image-api.md)、[数据](specs/image-data.md)、[方法](specs/image-methods.md)、[验收](specs/image-acceptance.md)
- [逐批计划](plans/image-mvp.md)、[执行授权](plans/image-mvp-goal.md)、[来源](plans/image-mvp-sources.md)、[设计包](plans/image-mvp-package.md)
- [ADR-0007](adr/0007-image-domain-and-private-registry.md)、[Fedora 手册](runbooks/image-mvp-fedora.md)
- 当前结果仍以[唯一执行状态](execution/status.md#image-mvp)为准。

手动 VNIC：[实施计划](plans/kcn-vnic-management-20261103.md)、[KCN 固定源码](references/kcn-vnic-source-20261103.md)、[正式资源规则](specs/vpc-subnet.md#手动-vnic-与实例选择)、[当前结果](execution/status.md#kcn-vnic-management-20261103)。


## 管理员集群运维一期

[业务规范](specs/cluster-operations.md)定义固定资源动作与双登记 Namespace 保护；[v1 调用合同](contracts/cluster-operations-v1.md)定义 Proto、原生操作、Secret、日志和预算；[CO-01–CO-06/A01–A19 计划](plans/cluster-operations-phase1.md)安排验收；[治理平台委托](specs/governance-calls.md#clusterops-platform)登记精确方法。实际实现与验证只见[执行状态](execution/status.md#cluster-operations-phase1)。
