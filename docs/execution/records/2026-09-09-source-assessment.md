# 2026-09-09：VPC / Subnet 首片设计的来源评估

2026-11-09 引用维护：本记录保留原时点的实施与验收结论。标为“历史定位”或“仓外定位”的原路径在当前 checkout 无法解析，仅保留追溯信息，证据可用性为 not_verified；不表示归档已恢复或旧规则仍适用。处理范围见[文档修复记录](res-doc-fix-20261109.md)。

本记录说明设计依据和证据边界，不是实施结果或部署批准。源码、契约和设计文档的静态检查已完成；新 Network 业务、数据库、真实网络连通性、租户隔离及服务间验证均为 `not_verified`。没有访问集群、运行 live gate 或修改其他仓库。

## 本地源码快照

核对日期为 2026-09-09；本次文档写入前以 `git rev-parse HEAD` 和 `git status --short` 重新核对。时间锚点：2026-09-09 09:18:41 UTC（Asia/Shanghai 17:18:41）。`clean` 只描述该次本地工作区，不表示已检查远端、CI 或部署。

| 仓库 | 本地 HEAD | 工作区事实与用途 |
|---|---|---|
| `/home/chabking/workspace/ani-network-service` | `f8d44daaff6dc1bbe2ed9960ce43b99045550583` | 本轮文档写入起点为 clean；已有独立运行骨架，没有 VPC/Subnet 业务实现。 |
| `/home/chabking/workspace/ANI` | `50aa9fe2099b7ff4c276f883939a8d26c9d9eff8` | clean；用于核对旧 OpenAPI、Network 同步执行、Gateway 装配及实例接入耦合。 |
| `/home/chabking/workspace/kc-networking` | `a2245883eb2b46a998f041feb3ad0ed3f6cf7c60` | clean；仅作为外部网络 Provider 的契约输入，不把该仓库实现纳入本次文档实施范围。 |
| `/home/chabking/workspace/ani-notification-service` | `0e3f0a2b47fcc1fa96fa926cae2b9ab55bd25d84` | clean；用于核对已接受的领域重构理由、边界及 schema。 |
| `/home/chabking/workspace/ani-iam` | `cd38cd90bca3e9d83af09a381051b895d82ae94f` | dirty；有已修改及未跟踪的设计、事项和文档。本记录引用的当前设计不等于该 HEAD 已提交内容，也不等于身份接入已实现。 |

IAM 未提交文档只作为当前设计快照。为避免以后把移动文件误认为固定提交，记录本次实际读取文件的 SHA-256；不复制凭据、运行配置或原始调试证据：

| IAM 文档 | SHA-256 |
|---|---|
| `CONTEXT.md` | `c432722fb4391a7b6c2410ded532ec4ef11933dbfb0f693046173a91e2c2275d` |
| `docs/adr/0022-unify-software-actors-as-workload-principals.md` | `7c7ae7ee9703aa83242b600965fcf17554b716ef9160feb059e37e1f58cff9f1` |
| `docs/plans/plan-workload-principal-refoundation.md` | `bf26159e7ff8c72177df7b219ce0d077355671663573d6cabacb0dd4cab18073` |

以下绝对路径链接指向本地文件，行号对应本次读取。复查时应先对照上述提交或文件指纹，不能假设链接中的内容一直不变。

## 已核对的 ANI 事实

### 产品 API 可以复用，执行方式需要重建

现有产品契约包含 VPC/Subnet 创建、查询和列表入口，提供复用产品名称、ID、CIDR 和父子关系的起点：VPC OpenAPI（仓外定位：`/home/chabking/workspace/ANI/repo/api/openapi/v1.yaml:8891`）、Subnet OpenAPI（仓外定位：`/home/chabking/workspace/ANI/repo/api/openapi/v1.yaml:9018`）、NetworkVPC schema（仓外定位：`/home/chabking/workspace/ANI/repo/api/openapi/v1.yaml:2158`）。复用不表示逐字段照搬；用户已允许为简洁和隐藏实现细节进行破坏性调整。

Gateway 当前在进程内装配 `LocalNetworkService`、数据库和 KubeOVN Provider：Network runtime（仓外定位：`/home/chabking/workspace/ANI/repo/services/ani-gateway/network_runtime.go:51`）。创建 handler 同步调用该 service 并返回 HTTP 201：VPC handler（仓外定位：`/home/chabking/workspace/ANI/repo/services/ani-gateway/internal/router/network_resources.go:294`）、Subnet handler（仓外定位：`/home/chabking/workspace/ANI/repo/services/ani-gateway/internal/router/network_resources.go:348`）。

Network 创建在请求内写入 pending，随后执行 render、dry-run、apply、observe，再持久化结果：CreateVPC（仓外定位：`/home/chabking/workspace/ANI/repo/pkg/adapters/runtime/network_service.go:165`）、CreateSubnet（仓外定位：`/home/chabking/workspace/ANI/repo/pkg/adapters/runtime/network_service.go:329`）、Provider 调用链（仓外定位：`/home/chabking/workspace/ANI/repo/pkg/adapters/runtime/network_service.go:1071`）。这不是请求受理后由独立 Network worker 保证完成的持久执行链。

`LocalNetworkStatusReconciler` 接受调用方提供的 observation，执行一次 `UpdateResourceState`；源码引用检查只发现 bootstrap 装配及测试，没有持续 Network 调度调用：Network status reconciler（仓外定位：`/home/chabking/workspace/ANI/repo/pkg/adapters/runtime/network_status_reconciler.go:38`）。现有独立 `reconcile-worker` 启动的是 workload controller：worker 启动（仓外定位：`/home/chabking/workspace/ANI/repo/pkg/bootstrap/server.go:546`）。旧 Network 创建路径没有接入通用 `AsyncTaskStore`，因此首片要建立 Network 自己的 operation 与恢复机制，不能描述为迁移一个已经存在的 Network 持久 worker。

### 查询已经纯读，进程内权威仍然影响恢复

Network GET/LIST 当前读数据库或内存记录，不触发 Provider 刷新：ListVPCs（仓外定位：`/home/chabking/workspace/ANI/repo/pkg/adapters/runtime/network_service.go:220`）、GetVPC（仓外定位：`/home/chabking/workspace/ANI/repo/pkg/adapters/runtime/network_service.go:266`）、GetSubnet（仓外定位：`/home/chabking/workspace/ANI/repo/pkg/adapters/runtime/network_service.go:408`）。不能把实例/task 查询中的状态推进问题误报成 Network GET 的现状。

与此同时，幂等键、创建父资源检查及删除仍依赖进程 map：字段定义（仓外定位：`/home/chabking/workspace/ANI/repo/pkg/adapters/runtime/network_service.go:15`）、父 VPC 检查（仓外定位：`/home/chabking/workspace/ANI/repo/pkg/adapters/runtime/network_service.go:329`）。静态可见，数据库 GET 能返回资源，并不保证另一个副本或重启后的 CreateSubnet 能通过该 map 检查。

旧 VPC/Subnet 删除只把业务记录改为 deleted：DeleteVPC（仓外定位：`/home/chabking/workspace/ANI/repo/pkg/adapters/runtime/network_service.go:279`）、DeleteSubnet（仓外定位：`/home/chabking/workspace/ANI/repo/pkg/adapters/runtime/network_service.go:421`）。旧 Provider apply 只接受 create：Provider 操作限制（仓外定位：`/home/chabking/workspace/ANI/repo/pkg/adapters/runtime/kubeovn_network_provider.go:146`）。新服务必须自行定义真实删除、失败清理、重试和重启恢复，不能沿用“业务记录已删除即完成”。

### 实例接入是明确的跨领域接点

Gateway 把同一个进程内 Network service 注入实例 runtime：SharedNetworkService（仓外定位：`/home/chabking/workspace/ANI/repo/services/ani-gateway/main.go:142`）。实例 resolver 已按租户查询 VPC/Subnet，并验证可用状态和父子关系：实例网络解析（仓外定位：`/home/chabking/workspace/ANI/repo/pkg/adapters/runtime/instance_resource_resolver.go:316`）。

实例 renderer 当前自己把产品 Subnet ID 转成 KubeOVN 名称和 annotation：实例 annotation（仓外定位：`/home/chabking/workspace/ANI/repo/pkg/adapters/runtime/dryrun_renderer.go:312`）。VM 渲染也明确依赖当前 KubeOVN 主网络语义：VM 网络渲染（仓外定位：`/home/chabking/workspace/ANI/repo/pkg/adapters/runtime/dryrun_renderer.go:938`）。因此更换 Provider 后，“Gateway 转发已接好”不能单独证明实例已接入新子网。

设计影响是：Network 拥有网络资源、使用关系和网络接入结果；实例 owner 继续拥有 Pod/VM 生命周期及其 manifest 写入。Network 不接管 Compute，实例 owner 不写 Network 状态，也不自行推导 Provider 名称。

## IAM 与 Notification 的领域设计输入

### Tenant、调用者与执行者必须分开

IAM 当前工作区把 Tenant 定义为 Core Control 拥有的平台资源，其他领域引用其不可变 ID；Tenant 生命周期与 IAM Tenant Access 不同：IAM 术语（仓外定位：`/home/chabking/workspace/ani-iam/CONTEXT.md:7`）。TenantScope 是验证后的单次操作作用域，不是客户端自报 Tenant ID：TenantScope（仓外定位：`/home/chabking/workspace/ani-iam/CONTEXT.md:39`）。

当前 ADR 将软件主体统一为稳定的 Workload Principal，Owner、Identity、Credential、Authority 与请求执行上下文独立；直接 Workload caller 与所服务的 Human/Tenant Workload 分开：IAM ADR 0022（仓外定位：`/home/chabking/workspace/ani-iam/docs/adr/0022-unify-software-actors-as-workload-principals.md:7`）。该设计明确保留接收端与授权证据的后续检查点，详细计划标记 implementation not started：IAM refoundation 计划（仓外定位：`/home/chabking/workspace/ani-iam/docs/plans/plan-workload-principal-refoundation.md:3`）。

对 Network 的约束：租户资源归属使用 `tenant_id`，请求来源、直接 caller、代办主体与后台执行者独立表达；不把 worker 伪装成用户，不把普通 header、namespace 或空 Tenant ID 当作权限。按用户决定，本期允许显式 Tenant 测试输入推进业务及联调；服务间身份验证仍未完成，不把未验证输入记成可信身份，也不复制 IAM 的一套身份契约。

### Notification 重构说明不能把首例参数推广成通用模型

Notification ADR 0005 记录，最初 Tenant Invitation 首例把 Tenant 要求扩散到全部 wire contract、domain、授权、数据库、幂等指纹、加密 AAD、worker 和查询。全局 Human 的密码操作不成立这一假设，邀请收件人还可能没有 Principal：重构原因（仓外定位：`/home/chabking/workspace/ani-notification-service/docs/adr/0005-keep-notification-native-and-integrate-iam-through-adapters.md:17`）。

替代模型使用封闭的 TenantScope/HumanPrincipalScope，不使用空作用域、特殊 Tenant 或 Boolean 旁路：Scope 决策（仓外定位：`/home/chabking/workspace/ani-notification-service/docs/adr/0005-keep-notification-native-and-integrate-iam-through-adapters.md:163`）。当前 migration 已有 `scope_kind`、`scope_id` 及通知类型/收件人的一致性约束：Notification schema（仓外定位：`/home/chabking/workspace/ani-notification-service/migrations/0001_notification.sql:8`）。

这不是要求 Network 复制 Notification Scope。VPC/Subnet 是租户资源，应保留确定的租户归属；可复用的是先分清领域归属、Actor、执行状态与外部引用的设计方法。Notification 自己拥有投递闭环，Producer 保留通知业务决策；同理 Network 自己拥有网络闭环，Core 不兜底它的状态：Notification 当前边界（仓外定位：`/home/chabking/workspace/ani-notification-service/README.md:21`）。上述其他服务的历史验证不证明 Network 已可用。

## kc-networking 的使用边界

本次把 kc-networking 视为外部 Provider 的事实来源，核对输入资源为 namespaced VPC/Subnet、实际 schema 为 IPv4，以及产品资源到 Provider 的映射接点：VPC 类型（仓外定位：`/home/chabking/workspace/kc-networking/api/networking/v1/vpc_types.go:99`）、Subnet 类型（仓外定位：`/home/chabking/workspace/kc-networking/api/networking/v1/subnet_types.go:75`）、实际 IP 类型约束（仓外定位：`/home/chabking/workspace/kc-networking/api/networking/v1/common_types.go:3`）、VPC CRD scope（仓外定位：`/home/chabking/workspace/kc-networking/config/crd/bases/networking.kubercloud.com_vpcs.yaml:15`）。

Provider 的内部对象名称、namespace、OVN/OVS、annotation 等留在适配器和环境接入契约。该快照不是 Provider 生产适用性、Ready、数据面连通性或隔离的验收。本设计记录不展开 Provider 缺陷审计，不批准修改或部署 kc-networking。

## 本轮用户已确定的方向

- 以独立 Network 为拆分试点，首片为租户 VPC/Subnet；Core 不负责兜底 Network command、operation、状态、Provider 收敛、清理或恢复。
- 优先复用 ANI 产品 OpenAPI；允许为删除不合理参数、隐藏底层实现和简化产品语义进行破坏性调整，VPC 产品 CIDR 保留。
- Network 自有持久化采用 sqlc/pgx，去掉 RLS；仍要求显式租户范围、租户保持的父子约束及真实数据库负向验证。
- 验证环境是全新的 VM + kind，由用户后续提供。没有历史 VPC/Subnet，不兼容旧表或旧资源，不为首片建立历史迁移、双读或双写。
- 先完成普通 Container 接入与网络闭环，再验证 VM；实例接入、基本连通与租户隔离属于功能目标，创建 CR 成功本身不足以验收。
- 服务间验证暂缓。身份、Tenant 生命周期等外部接点按各自 owner 的版本化契约处理，真实服务间接线与验收保留为后续检查点。
- 本轮授权落实设计文档，不是业务实现、数据库重建、发布、集群操作或部署授权。功能“首片一起实现”表达后续实施范围，不能把本轮设计标记为已实现。

## 先前讨论的检索来源

本任务协调者已通过 `list_threads` / `read_thread` 读取以下两项置顶讨论，作为用户讨论背景；当前源码事实仍以上述本地复核为准：

| 准确标题 | Task ID | 本次使用方式 |
|---|---|---|
| ANI架构职责重构 | `01a065fc-685d-7641-96ef-abf4cfee15fa` | 以领域 owner、事务和唯一状态写入者界定服务自治，不把 Core 继续作为统一业务执行器。 |
| 评估现有core重构方案 | `01a07f9d-c10b-7720-a4de-072ad0070fed` | 聚焦 Network 首片的可验证独立闭环，不扩大到整个 Core 或全部实例体系重构。 |

## 证据等级

| 检查项 | 状态 | 范围 |
|---|---|---|
| 本地 HEAD / dirty 与所引文件静态检查 | `pass` | 仅本记录时点；IAM 当前设计包含未提交内容。 |
| 用户选择与领域职责记录 | `pass` | 设计记录已形成，不代表协议生成、代码或数据库已实现。 |
| Network 业务、持久操作、重启和多副本恢复 | `not_verified` | 本轮不实现、不执行验证。 |
| fresh VM + kind 环境与 Container/VM 网络验收 | `not_verified` | 环境后续提供，Container 在先、VM 在后。 |
| IAM / Gateway 等真实服务间验证 | `not_verified` | 用户明确暂缓；不存在默认可用或临时可信假设。 |
| 切流、发布、部署、生产可用性 | `not_verified` | 本记录不构成授权或证据。 |
