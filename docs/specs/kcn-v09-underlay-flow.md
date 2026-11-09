# 虚拟交换机与二层网络管理合同

本文件维护 Res 平台交换机、节点配置和 VLAN 的产品合同。原生状态、通知、删除和引用遵循[网络状态消费](cr-observation.md)及[全域原则](res-responsibility-and-simplification.md)。固定版本来源和历史对象仅用于追溯，当前实现/验证只见[执行状态](../execution/status.md)。

## 1. 产品流程与管理边界

管理员准备接口及上游 VLAN/路由，先创建虚拟交换机，再一次提交所选集群、交换机、手填 device_name 和 vlan_id。Res 保存自己的配置、成员身份、逻辑占用、原生声明及并发责任；KCN 负责实际接口/OVS 接管与内部控制。没有网卡库存、独立设备登记前置或第二次物理审批。

集群只使用当前部署实际支持的配置，不增加区域/AZ 或调度平台。租户 Namespace 按[标准合同](../contracts/tenant-namespace.md)处理，不使用可配置 namespace_prefix；平台/集群级对象不机械增加租户 Namespace。

## 2. 固定来源与历史边界

| 来源 | 本文核对的固定版本与边界 |
|---|---|
| Resource 远端 main | `6bff99f1747a495fe4b278844c20ee72b1ce2ffc`，2026-10-21 已确认；旧草案/本地 checkout 基线为 `d32c4f438b43b8cb7534486c73e116e4d26c890a`，不把本地旧文件当最新源码 |
| Governance 远端 main | `323eb749b8a3d607b26687797b372919052895c7`，2026-10-21 已确认；仍固定 Res API 模块 `v0.0.0-20261019174951-733f625995f8`，不等于最新 Res main |
| Governance 先前部署来源 | `codex/gov-model-inference-latest-20261020`，`05bdb9d890b7e89645dd67ed1c697af2ad7c3ed0`；历史来源，不代表本轮确认运行镜像 |
| KCN 缓存源码 | 2026-10-30 本地 HEAD `a1e271c0162fb73ab84e0ca6618e5a7302442bd6`；GitLab 远端最新未确认，也不代表当前运行版本；新 monitor 不设置 VLAN Ready/Healthy |
| KCN 旧运行回执 | v0.9.0，`ca06a4e06c902b9f9cdc6a6a6a1879792e390fd3`；本轮未查询当前 imageID/CRD，不以缓存 HEAD 代替实际部署 |
| installer 远端 main | `78d8418307b6156954feb10aee8d6972d2ff1bbd`，2026-10-21 已确认；components.lock 仍锁 dev/fix2 |
| installer 远端 release | `8df08cd5c13f0ce5156a2df3f1ce0ed8cf56f166`，2026-10-21 已确认；锁定 KCN source `2b9467c`、index digest `494432d2…`、amd64 digest `26470989…` 与 17 CRD；完整物料引用见审查记录 |
| 已有原生出网证据 | 2026-10-20 跨仓回执，原定位 `../../../ani-model-service/docs/deploy/kcn-cluster-public-eip-20261020/README.md` 当前 checkout 不可验证，保留为 not_verified；本文未重新查询集群 |

以上是原设计在 2026-10 的取证版本，不等于当前安装或部署。具体来源见[审查记录](../execution/records/kcn-network-flow-review-20261021.md)。原 profile 的内部组识别、Ready/Healthy 缺项和恢复缺陷用于理解组件行为，不构成每次 Res 动作的完成证明或要求先修改 KCN 才能消费其公开结果。

## 3. 产品对象与标签协议

### 3.1 虚拟交换机由平台管理，用户只填名称

平台保存虚拟交换机的稳定产品 ID 和名称，提供创建、查询、列表、无引用删除；不新增区域/AZ 或描述字段。创建成功表示该配置已持久化，可以创建二层网络，不表示已创建物理桥或出口可达。

名称直接作为 KCN vClusterName：按 Node label-key 名称规则校验，额外禁止点号，平台内活动名称唯一；本期名称不可修改。删除进行中或自有节点配置尚未完成管理收尾时继续保留名称占用，不能因同名重建把旧组归给新产品 ID。

创建交换机时不要求选择集群或网卡，也不写 Node 标签。首次创建二层网络时确定目标集群并建立实际节点绑定。本期一个虚拟交换机只绑定一个集群；后续二层网络复用该绑定，不支持跨集群复用、原地换集群或用户编辑成员/优先级。集群选择只列出当前 Res 部署已配置可访问的目标；单集群部署只提供该集群，不借本次改造建设跨集群调度系统。

产品交换机有 ID，不代表 KCN 有同名 CR 或 UID。KCN vCluster 仍由 Node 标签派生；平台的 SQL 身份和后端节点绑定事实分别管理，不伪造 group Provider UID。

### 3.2 vcluster.networking.kubercloud.com 的标签协议

同一个完整键放在 Node labels 和 annotations 中时语义不同：

| 所在位置 | 示例 | 语义及写入方 |
|---|---|---|
| Node label | `vcluster.networking.kubercloud.com/ani-public: "100"` | 声明节点属于该组及组内优先级；由 Res 平台绑定操作管理 |
| Node label | `networking.kubercloud.com/allocated: "true"` | KCN 节点已分配状态；Res 不为凑齐成员强制修改 |
| Node annotation | `vcluster.networking.kubercloud.com/ani-public: "ens35"` | KCN 发布该组已就绪的设备列表，多个设备逗号分隔；Res 只读 |
| VlanNetwork spec | `vClusterName: ani-public`、`devName: ens35`、`vlanID: 0` | 指定目标组、接口及 VLAN；VLAN 0 表示不带 tag 的 Underlay |
| OVS mapping | `ani-public.ens35:br-ens35` | 对应组及设备的实际 local network |
| Public Subnet spec | `underlayConfig.vlanNetwork: <VLAN 名>` | 通过 VLAN 关联组，另用 gateway 字段引用 EIPGateway |

```yaml
# 结构示例，不是本轮执行清单。
metadata:
  name: ani-01
  labels:
    vcluster.networking.kubercloud.com/ani-public: "100"  # Res 配置
  annotations:
    vcluster.networking.kubercloud.com/ani-public: "ens35" # KCN 接管后反馈
```

标签不能证明 ens35 已接管，组设备注解是 KCN 配置步骤的反馈，不能证明持续链路健康或上游业务连通。Res 不代写接管成功注解，不直改 OVS。vCluster 是物理网络节点分组，VPC 是工作负载逻辑网络；ani-public 与默认 VPC kcn-cluster 是不同对象。

一个节点可加入多个组；本期后台仍保留 cluster/device 唯一占用保护，不据此允许同一物理口跨交换机重复接管。标签优先级默认 100，只用于 Underlay Subnet 的 GatewayChassis 新候选选择；高值优先、同级随机、目标最多两台，既有有效选择保留。它不是网卡接管顺序或 EIP/Snat nodeName 优先级，也不构成 HA 验收。

Public/Host EIPGateway 没有 vClusterName 字段；Subnet 通过 VLAN 的组为该 Subnet 的 router port 选 chassis。KCN topoVersion 只包含排序节点名，不能替代平台包含 Node UID、allocated 和优先级的成员绑定。


### 3.3 后台确定与保存节点范围

首次绑定根据所选集群的既定成员选择规则读取必要 Node 名称/UID，冻结成员、配置优先级和组归属；不因网卡库存或诊断子集缩小范围。后续二层网络复用原绑定，不自动加入新节点或接管替换 UID。

Node 身份和 Res 自己声明的配置用于正确写入与并发，不以 Node 在线、CNI 健康、观测年龄、KCN 设备反馈或拓扑 fingerprint 反复证明 VLAN 可用。管理员负责实际接口和上游条件，组件自行执行其原生控制。

## 4. 用户操作与后台流程

### 4.1 租户网络流程

租户只按产品接口管理 VPC/Subnet、Public EIP 和 Public SNAT 或 LB；不通过任意注解、原生名称或 platform scope 修改物理网络。产品引用、地址独占和管理状态按[统一合同](vpc-connectivity-lb.md)。

### 4.2 平台管理员网络流程

1. 创建交换机只提交 name/幂等键，本地保存产品身份；没有原生 vCluster CR，不伪造 Provider UID。
2. 创建 VLAN 一次提交所选交换机/集群/接口/VLAN 参数，在同一管理事务冻结绑定与逻辑槽位，防止并发重复占用。
3. Worker 以原 Node UID/resourceVersion 条件合并自己拥有的组配置，保存逐节点进度；外部同名配置不自动收养，部分成功和未知写继续原尝试。
4. 使用稳定名称声明 VlanNetwork，并直接消费其 Initialized。KCN 发布的组设备注解可供诊断，但不是另一个完成门槛；不要求组识别证书、全成员反馈、notReadyNodes 为空或节点健康合取。
5. 网关/池使用自己的产品身份和 Ready，SNAT 使用自己的 phase。开放分配/默认源按本服务权限、开关、引用、配置与版本处理；人工 verification 和真实流量分别记录，不作为准入凭证。

Res 不改 OVS、接口 IP、bridge、共享 managedDevices/kcn-config，不通过 sleep、重启 controller、隐藏删除重建或改 annotation 补偿组件内部错误。

## 5. 接口、状态与持久化

### 5.1 产品状态

| 对象/阶段 | 成功含义 | 不表示 |
| --- | --- | --- |
| 交换机创建 | 产品配置已持久化 | 已有物理桥或出口 |
| 节点绑定配置 | 自己声明的原身份配置完成 | KCN 内部链路被 Res 证明正确 |
| VlanNetwork | 原声明的 Initialized 结果 | 全成员/物理链路或出口健康 |
| 网关/池/EIP | 自己对象的 Ready | 所有下游资源均健康 |
| Snat | 自己的 phase 与 applied 输出 | 真实流量、DNS 或回程已通过 |

读错保留最后成功投影；管理通知/lease/epoch 和终态退休见网络 v3。不同对象不机械套用同一状态字段，原生 observedGeneration 仅在确认自己的特定配置时按合同使用。

### 5.2 API 合同

| 能力 | 用户输入/行为 |
|---|---|
| CreateVirtualSwitch | name、idempotency_key；不接收 region/AZ/description；返回系统 ID、名称与管理状态 |
| Get/List/DeleteVirtualSwitch | 按稳定 ID 操作；新列表使用 page/pageSize，直接返回 items/total，HTTP JSON 的 total 为十进制字符串；只删除无引用且绑定已安全清理的交换机 |
| 网卡输入 | 管理员手填已准备接口名，不依赖接口库存列表作为受理前置 |
| CreateVlanNetwork v2 | 保留名称/既有可选简介/VLAN，增加 cluster_id、virtual_switch_id、device_name；内部登记不成为用户前置操作 |
| Get/List/DeleteVlanNetwork | 保留产品入口，补交换机引用和逐节点进度；删除不删除交换机 |
| 旧独立设备接口 | 发现、登记、查询和退出 RPC/HTTP 全部移除；旧方法调用不再注册，交换机内部逻辑设备继续随二层网络生命周期管理 |


实际字段号和允许方法以生成 Proto 与[调用登记](governance-calls.md)为准，不重用旧 device_id tag。交换机未绑定状态不填假 cluster/Provider UID。平台请求保持平台 scope/actor/request 身份，不夹带虚构 tenant。旧永久幂等回执按原输入版本重放，不能给旧请求结构添加字段后重算 hash。

### 5.3 持久化、幂等与所有权

交换机、绑定、成员身份、设备/VLAN 槽位、引用、operation 和幂等在 Res 本地管理事务内保存；外部写入在事务外。保留原 UID、写入前提、version/lease/epoch 和已有在途 checkpoint，未知结果不换名、不提前释放或批量改成功。

当前 schema/查询为权威；历史 fingerprint_version 用于业务输入重放，不恢复拓扑/镜像/时效 proof。必要的历史设备登记可读，不授权继续运行旧 adoption worker。

## 6. 删除与失败恢复

VLAN 删除先处理 Res 自己的产品引用，再正常删除原声明；原 UID 不存在或同名已替换时结束原对象，不等待 KCN 自动子资源、物理网卡或 bridge 回收。单 VLAN 删除不移除其他 VLAN 仍使用的交换机组配置。

交换机无产品引用后，按原 Node UID/resourceVersion 和登记原值清理自己拥有的组键，保留其他组及 KCN 内部反馈；完成自己的管理配置后结束，不等待设备注解/物理端口消失。错误或身份冲突保留实际原因，不接管同名新对象、不剥第三方 finalizer、不直接改数据库完成状态。

## 7. 系统默认 VPC 与已有原生资源

### 7.1 kcn-cluster 采用独立平台用例

系统默认 VPC 不伪造租户 VPC/EIP 行。平台用例使用当前明确配置的 cluster/namespace/name/原 UID、平台授权、产品 EIP/SNAT 占用、operation/幂等及原声明提交；状态消费仍按对象公开结果，不联合证明组件内部执行。

原生现存资源的只读登记不带来写入/删除权；完整移交须有具体对象与任务范围，不能从本合同取得共享环境操作许可。

### 7.2 已有原生出口默认保留

2026-10-20 原生流程恢复出口的时点记录如下，不代表当前集群仍有全部对象。用户于 2026-10-30 说明 10～12 上旧版独立网卡登记已清理，本次不再次清理或把它作为移除 NodeFacts 的前置。其他原生网络仍按现有授权边界保护；历史受控拆建恢复范围见[可执行 Goal](../plans/kcn-network-platform-goal-20261021.md)。

| 记录 | 已有身份/含义 |
|---|---|
| 节点组 | ani-public，ani-01/02/03，priority=100 |
| VLAN | cluster-public-ens35-20261020，ens35/VLAN 0 |
| Gateway | cluster-public-egress-20261020 |
| Public Subnet | kcn-system/cluster-public-102-20261020 |
| EIP/Snat | kcn-system/cluster-public-egress-20261020，EIP 172.16.102.192，目标 kcn-cluster |
| 旧平台 Device | device_94d6ebc1e5bb466d9effad72e9fb055d，当时 provisioning/PROVIDER_UNAVAILABLE，保留 ConfigMap/adoption 历史 |
| 旧平台 Gateway | egw_d6d865fccf9946bb90a8a171975be566，当时 available、未被上述原生池使用 |

新建同名产品交换机不能收养上述组或网络。平台显示外部管理并拒绝重叠占用，不能把 ens35 已就绪事实直接写回旧 Device 成功，也不自动合并两个 Gateway。

其他环境若仍有历史 Device，占用和未知写入须按原身份核清，不能因删除原生公网 CR 就推定旧记录已释放，也不能重新启用旧失败 worker。本次不新增旧 adoption 写入或退役流程；不得 raw SQL 删行、直接改 available、盲清标记或以新产品收养原生组。


## 8. 实施来源与范围

历史工作包见[原平台化计划](../plans/kcn-network-platform-goal-20261021.md)。其集群、接口、地址段、拆建恢复和连续执行许可只属于原任务，不授权当前或后续操作。实现变更、部署、迁移与共享设施处理分别按实际用户任务执行；本文不维护实施进度。

## 9. 验收条件

按当前变更验证配置/输入、权限与作用域、固定身份、组键所有权、唯一占用、永久重放、并发/迟到写、读错保留、原生结果及自己的删除收尾。不为本领域恢复全节点、CNI、镜像、拓扑或物理清场证明。

显式安装、接口接管和真实 DNS/HTTPS/源地址/回程诊断按各自环境保存真实结果；它们不是每次管理操作的新门槛。STATIC、数据库、真实 API 与流量分别记录；文档修改只检查链接和格式，不运行上述业务验收。
