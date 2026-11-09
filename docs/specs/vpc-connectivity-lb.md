# VPC 基础内网、公网出站与租户 LB 统一方案

2026-11-04 [平台基础网络目录](vpc-subnet.md#平台基础网络租户接入)是既有共享网络的只读登记，独立于本文的租户 VPC 和地址池生命周期。目录不调用 BaseConnectivity 分配、不创建租户 EIP/SNAT，不允许租户在平台 VPC/Subnet 创建 LB 或配置 SNAT；管理命令按所有权返回 PermissionDenied，平台出口在 Console 显示为平台托管。既有私有网络流程继续按本文执行。

> 2026-11-04 网络合同更新：本文件中的网络状态、Ready/时效准入、Attachment 释放、SG 应用及 LB 生成物验收条款以[网络状态消费规格 v3](cr-observation.md)为准；方法/字段变更见[实施方案](../plans/network-management-simplification.md)。实际验证只见执行状态。

初始日期：2026-09-14；后端合同更新：2026-10-30。状态：实现范围与实际验收分别见执行状态；用户已确认的约束见 [ADR-0005](../adr/0005-separate-vpc-connectivity-and-exclusive-eip-bindings.md)。本文定义目标行为，实施任务见[执行计划](../plans/vpc-connectivity-lb.md)，当前进度只看[执行状态](../execution/status.md)。历史实施结果保留各自时点；本次固定三规格、多监听器和 VPC 预设只改后端，未修改前端或共享平台预置。

## 1. 固定输入和变化范围

Network 实施基础为 `d8835a22d905e358b7f60756d3113baa97d7c762`（既有 EIP/SNAT 分支），不是只有 VPC/Subnet 的 main `e481e968...`。保留其租户隔离、操作、幂等、UID、fencing 和 SharedInformer→持久唤醒→worker 模型。不能把手工 kc 实验直接并入产品数据库。

本方案覆盖：平台基础网络准备、已有 VPC 创建/删除/观察的调整、既有 Public EIP/SNAT 的调整、三种类型租户 LB 的新生命周期，以及 API/界面接入任务。Network 保持唯一网络产品写入者；ANI Gateway/Console 只调用契约和展示结果；实例 owner 仍创建 Pod/VM；kc 与 Envoy Gateway 各自负责其数据面资源。

本批 LB 交付范围为 IPv4、单集群、三种入口类型、1～64 个独立 HTTP/HTTPS/TCP/UDP/SSL 监听器、固定 small/medium/large、同 VPC 的 IP 后端、权重与监听参数。旧单监听器默认 HTTP/8080 输入继续兼容。复杂路由、证书管理、在线切换 LB 类型/规格、HA/容量和 Underlay 物理验收仍属后续单独任务。

空监听器保存、独立 Provider 目标版本、下发集合与清空/再激活的增量合同由 [LB 延迟下发规格](lb-deferred-activation.md) §§2～6 定义；实际验证范围只见 [LB-DEFERRED-01](../execution/status.md#lb-deferred-01)。

已有 VPC/Subnet/Attachment 的未冲突规则、Public EIP 的独立申请/保留/释放语义、平台 Public 网络模式与当前准入规则继续适用。本文替代旧方案中“每 VPC 总共一个 SNAT”“VPC Ready 即整个创建完成”“任何 SNAT 都直接阻塞 VPC 删除”“EIP 只能用于 SNAT”的相关限制。

### 1.1 已有证据与边界

- 独立 Network EIP/SNAT 代码已有受控 Provider、真实 PostgreSQL、恢复及观察验证；其历史公网原生 Overlay 验收存在外部 kc 阻塞，不能因新 LB 成功而自动关闭。
- 2026-09-14 手工 CR 实验补齐 Intranet Subnet→EIP→Snat，并由用户重启 kc 控制器后绑定成功；三种 HTTP LB 的 kind 私网/外部 EIP 入口成功，配套 30 项检查 pass。
- 手工 CR、kind 节点作为外部客户端、实验 Public 地址不等于产品 API 创建链路、真实互联网、生产可用性或高可用证明。
- 控制器初始化缓存问题的修复由 kc 团队承担；Network 不自动重启控制器、改 OVN 路由或用 NodePort 代替 EIP。

## 2. 产品模型和固定规则

术语统一见 [CONTEXT](../../CONTEXT.md)。本节是规则权威来源，任务文档只引用。

| 对象 | 管理者 | 创建及释放方式 |
|---|---|---|
| Intranet / Public Address Pool、出口网关、必要的二层网络 | 平台管理员 | 管理员独立初始化，租户只消费能力 |
| VPC | 租户 | 创建同时编排基础内网连接；删除时清理系统子资源 |
| 基础 Intranet EIP/SNAT | 系统，资源归属仍为对应租户 | 随 VPC 管理意图创建、观察和释放；不提供租户独立解绑 |
| Public EIP | 租户 | 默认从平台 Public 池申请；未绑定时可保留或释放 |
| Public VPC SNAT Binding | 租户 | 将独立 Public EIP 用于整个 VPC 出站；可启停/解绑 |
| LoadBalancer | 租户 | 属于单一 VPC/Subnet；可有私网入口、公网入口或两者 |
| LB 的 EIP Binding | 租户资源操作产生，系统维护占用 | 占用一个已申请 Public EIP；LB 删除后归还 EIP，不自动释放地址 |

规则：

1. 同 VPC 最多一个未释放的 Intranet SNAT 和一个未释放的 Public SNAT；停用、退化、删除中、结果未知均继续占用自己的位置。
2. 一个 EIP 同时只有一个有效目标：`vpc_snat` 或 `load_balancer`，从受理预留开始即排他。目标修改必须先完成解绑；不得两份资源各自声明唯一却一起占用。
3. Intranet EIP 是 system-managed，只能用于本 VPC 的基础 SNAT；Public EIP 可以用于 Public SNAT 或一个有公网入口的 LB。地址池类型、管理归属与用途必须匹配。
4. 申请租户 EIP API 默认且只申请 Public；基础地址不混入租户公网 EIP 列表，猜测内部 ID 也不能 GET/启停/解绑/删除它。VPC 只读显示 `base_connectivity` 摘要，管理员诊断视图可查基础资源。
5. 纯私网 LB 不占用 Public EIP；三种 LB 均校验 VPC/Subnet 的产品归属、引用和管理状态，不依赖基础 Intranet EIP/SNAT，也不以受基础故障影响的 VPC 聚合 available 作为准入。创建公网 LB 不要求该 VPC 已有 Public SNAT。
6. Public SNAT 关闭、解绑、换绑只作用于公网绑定，Intranet EIP/Snat 的身份、地址和期望保持不变。删除 LB 不解绑 VPC 的任一 SNAT。
7. 跨租户绑定统一按不存在处理；所有关系验证 tenant、cluster、namespace、产品 ID、Provider 名称及 UID。Provider 命名用稳定产品 ID，不使用租户显示名。
8. 资源状态、当前配置应用状态与数据面探测证据分开。`available` 表示本节后续定义的配置完成条件，不能独自证明所有流量健康；读取错误单独显示，旧观察时间不构成准入或状态失效。

## 3. 平台初始化流程

管理员准备所需二层网络、Public EIPGateway/池、Intranet 池与 LB 组件。Res 维护其平台产品配置、原身份、分配/默认开关及引用；物理接口和外部路由按实际部署支持条件准备，不建立网卡库存、自动接管审批或运行期镜像/拓扑证明。

GetIntranetPoolDefaults 只在显式管理员请求中读取默认 VPC 已公开身份，intranet_networks 是可选管理员记录。SetIntranetPoolAutoAllocationEnabled 按池版本/幂等原子设置默认源、开放分配及新 VPC 自动分配开关；关闭/切换只作用后续首次受理，不迁移既有原身份。人工 verification 只是材料存档。

Public 与 Intranet 保持独立 scope/默认池，EIP 固定接受时的目的池。LB 使用已配置公共 lb-common Class/Proxy 和自己专属的声明对象；安装缺项由部署方处理，worker 不补装或覆盖公共组件。运行管理不重复检查固定镜像、安装 fingerprint、节点健康或观测年龄；显式安装/流量诊断另行记录。

## 4. 租户 VPC 流程调整

### 4.1 首次受理与分步创建

用户提交名称、CIDR、描述和幂等键，不选择内部基础 EIP/SNAT。受理保存 VPC 意图、operation、固定产品身份和适用自动分配配置；本地事务与 Kubernetes 声明分开。

VPC 运行结果来自它自己的原生 Ready。自动基础出口按已受理配置管理自己固定的 EIP/SNAT 子产品步骤，不把 EIP 地址、SNAT 绑定、父 Ready、时效或物理路由合成为第二套 VPC Ready。内部步骤不调用要求 VPC 聚合状态的旧 Public 绑定路径，避免循环等待。

### 4.2 就绪与持续观察

VPC 当前状态、历史 create operation 和 base_connectivity 摘要分别表达。基础摘要使用自己原 EIP/SNAT 的公开结果，不覆盖 VPC 的 Ready 或改写已结束 operation。

首次受理校验实际自动分配开关、默认源、产品引用和配置；关闭只影响新请求，原键回执优先。读错保留最后成功状态/输出/时间，任务记录错误重试；结果未知时保留原池、名称和原意图，不换名、切池或伪造地址。

### 4.3 删除与创建失败终止

VPC 删除仍受 Res 自己未释放的用户 Subnet/Attachment、Public 绑定和 LB 等产品引用保护。内部基础出口按自己的 SNAT→EIP 管理顺序正常删除，不要求租户解绑系统子产品，不等待 EIP boundResource/物理地址清场证明。

原 VPC 声明消失可结束该原对象删除；子产品的完成事务继续推进各自责任，不因根对象消失丢弃身份或步骤。对确定未发送的步骤按本服务事务取消；未知创建沿原意图和确定名称恢复，不能清空责任或批量标完成。具体边界只见[状态消费合同](cr-observation.md)。

## 5. Public EIP 与 VPC 公网出站调整

租户接口只申请和操作 Public EIP/绑定，不暴露系统基础出口为可自由解绑资源。每 VPC 内网/公网用途分别限制一个当前 SNAT；EIP 从预留开始独占一个 vpc_snat 或 load_balancer 产品目标，SNAT 与 LB 共用占用事务。

受理绑定/启用检查租户、placement、当前产品管理状态、配置、引用与唯一占用，不依赖基础 Intranet、VPC 聚合 available、Provider Ready、观测年龄或内部关系证明。停用保留 EIP 和绑定；解绑结束自己的原 Snat 后释放管理引用，EIP 本体保留。

EIP 查询、默认池与分配开关按[Public/SNAT 合同](vpc-snat.md)处理。原 EIP 地址/状态由该对象公开结果登记；不为修内部失败直接改 OVN、重启 controller 或追删生成物。

## 6. 新增租户 LB 生命周期

### 6.1 资源和租户输入

LB 归属于一个租户/VPC/Subnet，包含 1～64 个 HTTP/HTTPS/TCP/UDP/SSL Listener，每个监听器拥有稳定 ID/name、端口、独立 Backend Members 和健康检查配置；HTTP/HTTPS 使用默认 `/` 前缀规则。一期不开放任意 Provider YAML。建议创建输入：

| 字段 | 规则 |
|---|---|
| name、description、idempotency_key | 沿用现有校验与持久重放规则 |
| vpc_id、subnet_id | 同 tenant/cluster/namespace，Subnet 属于 VPC；保留当前管理引用和身份，不追加 Provider Ready/时效或基础 Intranet EIP/SNAT 前置 |
| exposure | `private` / `public` / `public_private`，创建后一期不可改 |
| flavor | 固定 small/medium/large；空值兼容 small，未知值拒绝，不能直接传 Class 名 |
| public_eip_id | public/public_private 必填，private 禁止；已分配且未占用的本租户 Public EIP |
| private_ip | private/public_private 必填，属于所选 Subnet 且非网关/保留地址；public 禁止 |
| listeners | `{items:[...]}`；1～64 个监听器，protocol 为 HTTP/HTTPS/TCP/UDP/SSL；name 遵守安装 CRD 的 253 字符 DNS 名称限制，name/port 不重复，端口范围 1–65535 |
| listener | 旧单监听器输入仍兼容，规范化为 `http`；不得与 listeners 同时提供 |
| backends | 普通监听器允许空集合，等待添加后端时仅保存数据库；HTTP 重定向监听器必须为空，其激活依赖固定 HTTPS 目标。下发规则见 [延迟下发 §2](lb-deferred-activation.md#2-用户配置与下发集合)。非空成员含 subnet_id、IPv4、port、weight，须验证归属本 VPC 的已分配业务地址，禁止直接指定平台/节点/跨租户地址；顶层字段仅用于旧输入 |
| health_check | TCP/HTTP；UDP 仅 TCP。port 缺失或 0 使用后端业务端口，非零 1–65535 可以独立于业务端口；HTTP 要求 path，可选 DNS hostname。默认 interval=5s、timeout=3s、unhealthy=3、healthy=1，时间与阈值分别为正数；panicThreshold=0 |
| algorithm | 每监听器 RoundRobin（旧请求默认）、LeastRequest、SourceIP ConsistentHash，详情返回保存的实际算法 |
| certificate_ref | HTTPS/SSL 必填 TLS Secret name，可选 namespace 必须等于资源绑定 namespace；Secret 需具有相同租户标签。只引用，不签发或上传 |
| session_persistence | HTTP/HTTPS 可停用、COOKIE_TIMEOUT（正 cookie_expiry_seconds）、COOKIE_SESSION；关闭删除旧字段 |
| max_connections | 可选正数，0/省略关闭；HTTP/HTTPS/TCP/SSL 映射 CTP；UDP 不接受该字段 |
| gzip_enabled | HTTP/HTTPS 开启 Gzip；false 清除 compressor |
| https_redirect_status_code | HTTP 的 301/302，0 关闭；与 backends 互斥，HTTPS 目标优先 443，否则必须唯一，重定向包含目标端口 |

Backend 地址的业务归属使用 Res 已持久化的当前消费者/Attachment 与地址映射，不仅按 CIDR 判断，也不反向读取 VNicIP/控制器链补证。Network 不创建或删除业务 Pod/VM；实例 owner 返回的接入信息用于地址确认。后端删除/地址变化触发成员退化与配置更新，固定 IP 成员不会静默转发到复用同一 IP 的另一身份。需要未纳管静态后端时另行定义管理员准入，不默认为任意 IP 放行。

LB 的成员依据当前产品消费者身份、地址映射与业务引用处理；消费者读取错误不当作空集合，已退出或身份更换不得静默转发到复用同一 IP 的新资源。没有 TTL、provider audit 或反向 owner/网卡链。

私网 VIP 是 Res 本 VPC 的地址意图和唯一占用；实际分配由 KCN 负责。用户不能指定任意平台/节点/跨租户地址，也不由本地表重新竞争原生 IPAM。

### 6.2 Provider 映射

| LB exposure | 公共 GatewayClass | lb_vip_address | lb_eips | 专属 Proxy 的 Service 类型 |
|---|---|---|---|---|
| private | lb-common | 指定 VIP | 不填 | ClusterIP |
| public | lb-common | disable | Public EIP CR短名 | LoadBalancer |
| public_private | lb-common | 指定 VIP | Public EIP CR短名 | LoadBalancer |

固定规格的声明参数使用以下常量；运行时不复核生成数据面规格：

| flavor | 副本 | 每副本 requests | 每副本 limits |
| --- | ---: | --- | --- |
| small | 2 | 1 CPU / 1Gi | 2 CPU / 2Gi |
| medium | 4 | 2 CPU / 2Gi | 4 CPU / 4Gi |
| large | 6 | 4 CPU / 4Gi | 8 CPU / 8Gi |

Gateway `spec.infrastructure.annotations` 的 `lb_vpc`/`subnet` 使用 namespace/name，`spec.gatewayClassName` 固定为 `lb-common`，`spec.infrastructure.parametersRef` 引用同 namespace 的专属 EnvoyProxy。专属 Proxy 显式填写 `mergeType: StrategicMerge`、副本、Envoy 容器 requests/limits 和 Service 类型；公共 Proxy 提供镜像、调度、`preserveRouteOrder: true`、`mergeGateways: false`、Deployment patch 及 telemetry/bootstrap，不设置容量或 Service 类型。运行时完成以自己声明的原生配置结果为准；生成 Deployment/Pod/Service 的安装或故障诊断由显式运维处理。

专属 EnvoyProxy、Gateway、各协议 Route、Backend、BackendTrafficPolicy 及所需 ClientTrafficPolicy/EnvoyPatchPolicy 放在固定租户 namespace；GatewayNamespace 模式由 Envoy Gateway 在同 namespace 生成 Service/Deployment/Pod。Network 只写自己拥有的专属 Proxy/Gateway/Route/Backend/Policy，不直接成为生成 Service/Deployment 的第二个 spec 写入者。专属 Proxy 使用现有 LB component、租约、pending mutation 和未知创建屏障；写权限限定租户 namespace，公共 Class/Proxy 只读。已存在的旧规格 Class 不自动切换，涉及旧运行实例时先核验身份并执行受控迁移。

监听参数随配置版本持久化，旧行参数默认为空对象，旧回执与 fingerprint 不重写。健康端口缺失/0 时省略 `overrides.port`，非零独立端口显式写入；不再要求与后端端口相同或 timeout 小于 interval。

| 协议/参数 | 本服务声明对象及字段 |
|---|---|
| HTTP / HTTPS | Gateway HTTP / HTTPS（TLS Terminate）+ HTTPRoute |
| TCP / UDP / SSL | Gateway TCP / UDP / TLS（TLS Terminate）+ TCPRoute / UDPRoute / TCPRoute |
| algorithm | BackendTrafficPolicy `loadBalancer`；SourceIP 使用 `ConsistentHash` + `consistentHash.type=SourceIP` |
| Cookie | HTTPRoute `rules[].sessionPersistence`，Permanent + absoluteTimeout 或 Session |
| 健康检查 | 非 UDP：BackendTrafficPolicy `healthCheck.active`；UDP：EnvoyPatchPolicy TCP health_checks、可选 endpoint 检查端口及 panic=0 |
| 最大连接数 | ClientTrafficPolicy 指向 Gateway 与 listener sectionName，`connection.connectionLimit.value` |
| Gzip / HTTPS 重定向 | BackendTrafficPolicy compressor / HTTPRoute RequestRedirect，无 backendRefs |

HTTPRoute rule 名固定 `default-rule`，HTTP BackendTrafficPolicy 的 sectionName 指向该 rule；Gateway parentRef 与 ClientTrafficPolicy 的 sectionName 指向 listener name。TCP/UDP policy 不填写不存在的 Route rule sectionName。每个 listener 继续拥有独立 Route/Policy 组件，协议切换与关闭参数只清理对应自有组件。生成 Service/Deployment 由底层组件管理，Res 不沿 Gateway owner 链重新证明其归属或完成，不直接写这些对象。

最大连接数是 Envoy 每 Pod 的限制，不是全局协调额度。ClientTrafficPolicy 只写所需 connectionLimit，保留平台已有可信代理/clientIPDetection 以及 HTTP2/WebSocket 默认行为。TLS 引用需要资源绑定 namespace 内对选定 Secret 的 `get` 权限，可通过带 resourceNames 的 Role/RoleBinding 授予，不扩大为全局 Secret list/watch。UDP 健康策略要求底层开启 `extensionApis.enableEnvoyPatchPolicy`；缺失时保留失败/未就绪证据，由底层处理，应用不更改安装配置。


历史实现记录中的 Fedora 具名验证：`TestLBMultipleListenersMainFlow`、`TestLBListenerParametersMainFlow`，以及两仓 `TestNetworkJointHTTP/multiple_listeners` 均通过。测试使用真实 admission、PostgreSQL、Worker、RPC/HTTP/auth；Kubernetes/控制器边界为 fixture。未运行全量测试。真实集群/数据面验收仍为 `not_verified`，不以 fixture Accepted/Programmed 代替。

### 6.3 创建事务和执行顺序

受理在同一管理事务保存 LB、完整监听配置、后端引用、地址意图、EIP claim、operation/幂等与确定声明名称。空后端的保存和运行投影按[延迟激活合同](lb-deferred-activation.md)分别办理，不凭 API 接受声明流量已健康。

Worker 只声明自己拥有的专属 EnvoyProxy、Gateway、Backend、Route 和 Policy。原生 Gateway/listener/controller 的 Accepted/Programmed、Route/Policy 的对应条件及本次配置 observedGeneration 按对象合同消费；EnvoyProxy 没有 Ready 合同，声明提交即完成该步骤。生成 Service/Deployment/Pod/EndpointSlice、地址分配或健康探针不作为配置合取。

### 6.4 更新与删除

desired_version/provider_target_version/applied_version 分别表达保存、确定下发目标和本次原生配置回执；不能混用，也不由后续观察改写历史 operation。成员和后端引用按当前意图及已声明组件保护，少量失败保留原配置、身份与恢复责任。

删除按 Route/Policy → 原 Gateway → Backend/专属 EnvoyProxy 的声明顺序推进，逐个正常删除原 UID；未知请求继续认回原尝试。自己的组件退出和 pending/引用责任完成后释放 EIP/VIP/父产品占用，不等待生成对象、物理 VIP 或 EIP 解绑证明。公共 Class/Proxy、独立 EIP、本业务后端及 VPC 基础出口保留。

## 7. 持久化、并发与观察

### 7.1 目标数据变化

LB 保存配置、产品引用、三类版本、稳定 binding/component 名和原 UID、lease/epoch、操作与幂等。退休 binding 保留原声明身份与删除记录，不代表物理地址回收证明。data_plane_state 保留原字段号并返回 UNKNOWN，data_plane_observed_at 为空。

实际表和约束以[迁移源码](../../migrations)与当前 queries 为准，不恢复 generated_resources 表、absence/occupied proof 修订、安装 fingerprint 或其他已由 0032 删除的证明字段。历史回执、旧字段号和必要 checkpoint 按确定方案保留。

### 7.2 观察与恢复

普通 informer 将精确对象通知到已登记任务，执行者读取自己的声明；不订阅生成工作负载做完整性证明。Finish 保留 tenant/version/lease/epoch 和通知代次，稳定任务停放，读错保留并退避，管理终态退休。通知与启动恢复不反向发现新的产品对象。

未知 CREATE/DELETE 继续原 binding/component 名、UID 与意图，不换名猜测完成，不提前释放产品占用。后端业务归属通过公开窄端口和持久管理数据确认，不跨域写计算表或接管 Pod。

## 8. 存量升级与契约接入

### 8.1 已有记录和旧接口

数据迁移将已知从Public池创建的EIP/SNAT标为public/tenant-managed，保留资源ID、Provider UID、历史operation、幂等指纹和已返回快照；不能按“VPC唯一SNAT”猜用途。发现不符合旧约束/来源不明数据输出冲突清单，由独立处置解决，不能静默收养外部CR。

租户原Public EIP/SNAT API语义保持；增加绑定目标类型和VPC基础连接摘要。平台新增Intranet池/默认池管理。LB新增 Create/Get/List/Update/Delete 和operation查询，公开租户body不允许覆盖tenant/namespace/Provider字段。

### 8.2 既有 VPC 补齐

数据库升级不自动批量写集群。先生成可审核的VPC补齐清单和固定默认Intranet池版本，再通过独立的系统 `ensure_vpc_base_connectivity` 持久操作逐个推进，幂等、限速、可暂停和恢复；不复用或重写历史create_vpc成功operation。

旧 VPC 在补齐前返回基础连接 missing/unknown，既有工作负载保持；不因数据库加字段删除、重建或自动停用旧资源。补齐只完成本服务基础出口产品管理，不切换为聚合 Ready。VPC 直接采用自身原生 Ready，基础连接单独展示自己的 EIP/SNAT 状态；新增 LB/Public 绑定按归属、配置、引用与管理状态办理，不以补齐进度或父网络 Ready 为门槛。实际补齐范围和结果只在执行状态记账。

删除/补齐/新绑定竞争通过同VPC封闭标记与锁串行；默认池切换不改已受理补齐意图。补齐失败保留已分配地址与步骤，用户可按既有删除流程终止。

### 8.3 ANI 接入与界面

Network 定义并生成契约，ANI Gateway以可信租户上下文调用；不恢复Core的网络DB/worker/Provider写入。界面在VPC创建页不增加基础EIP输入，VPC详情显示基础连接摘要；公网EIP列表只显示tenant-managed Public地址及SNAT/LB绑定目标；绑定弹窗区分“VPC出站”和“LB入口”，过滤已有claim的EIP；LB表单按exposure显示VIP/EIP字段，提交后展示 operation、配置状态和已读取的原生结果；端到端流量诊断另行记录。

可信身份适配沿用既有服务边界，不能把未验证header当管理员权限。当前身份/配额/Core大规模重构各有独立任务，本方案只接入已有可用授权能力；未完成的部分在集成任务明确标记，不以临时dev身份冒充上线验收。

## 9. 验收要求

| 编号 | 应验证行为 |
| --- | --- |
| U-V01 | 平台/Public/Intranet 范围、真实配置、默认源与分配开关，不伪造 tenant |
| U-V02 | 一个 EIP 的唯一产品目标及 VPC 内网/公网各自约束，并发不双占 |
| U-V03 | VPC、基础摘要及历史 operation 分开；读错保留，不拼父子 Ready |
| U-V04 | 不同默认源/开关变更仅影响后续首次受理，原键和既有身份不漂移 |
| U-V05 | Public 启停/解绑保留正确业务占用和独立 EIP，不依赖基础出口健康 |
| U-V06 | 三种 exposure 的真实入口流量单独实测，不以配置完成替代 |
| U-V07 | 只观察自己声明的组件完成，不等待生成 Service/Pod/EndpointSlice |
| U-V08 | 配置版本、部分失败、后端更换与迟到回执保护 |
| U-V09 | 自有组件原 UID 退出后释放管理占用；不等物理地址清场 |
| U-V10 | 旧回执、原身份和明确升级责任保留，不收养手工 CR |
| U-V11 | 精确通知、读错退避与终态退休，GET/List 没有修复副作用 |
| U-V12 | API/Console 区分配置、原生结果和流量健康，废弃数据面字段不伪造 |

必要实现检查在当前任务确认的环境执行；STATIC、数据库、真实 API 和流量证据分开。历史 Ubuntu/Fedora/10–12 记录不授权新部署，本文也不承诺未实测的 HA 或上游网络能力。

## 列表总数

负载均衡列表返回 `items,next_cursor,total`。`total` 统计当前租户、name/state/vpc/subnet/exposure 筛选下已受理的产品资源，不包含尚未受理的占位记录，不应用 cursor/limit；总数与本页数据来自同一只读数据库快照。跨页不承诺总数固定。

显式安装核验与真实流量验收分别记录，不成为后台运行期的镜像/生成物门槛；Gateway Programmed 也不构成流量健康承诺。
