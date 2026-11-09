# 租户 VPC / Subnet 首片规格

2026-11-04 平台基础网络增量统一见本文[平台基础网络租户接入](#平台基础网络租户接入)。下文租户创建、CIDR、父行锁和配额规则适用于私有分支；平台目录使用明确的共享引用分支。

> 2026-11-04 网络合同更新：本文件中的网络状态、Ready/时效准入、Attachment 释放、SG 应用及 LB 生成物验收条款以[网络状态消费规格 v3](cr-observation.md)为准；方法/字段变更见[实施方案](../plans/network-management-simplification.md)。实际验证只见执行状态。

> 2026-10-19：Notebook 实例接入新增[受控工作负载合同](instance-configuration-network.md#6-notebook-消费者与网络管理关系)。下文不同 Pod UID 冲突和 Consumer v1 描述继续适用于原 `single_pod` 模式；`controller_managed` 使用持久版本、独立 owner 链核验和当前/历史/待核验关系，不能据新模式放松旧模式的身份保护。实际交付只见[执行状态](../execution/status.md#instance-configuration-and-network-2026-10-19)。

> 2026-09-14 的[统一方案](vpc-connectivity-lb.md)增加 VPC 基础内网创建/删除/观察、既有 VPC 补齐和 LB 占用。这些目标调整接替本文件相应的创建完成和删除准入限制，不改写首片历史验收结果；其余 Subnet/Attachment 协议继续适用。

日期：2026-09-09；更新：2026-09-10。文档版本：2。新增 NET-05A 观察目标及后续依赖，未改变当前实施状态。

本规格是本轮形成的实施设计；用户已接受的架构方向见 [ADR](../START-HERE.md#架构决定)。
字段限制、状态枚举、分页和接入协议等细节是本轮补齐的工程设计，不宣称已经逐项获得人工验收。
实现状态与验证结果只维护在 [执行状态](../execution/status.md)；本文描述应有行为，不证明功能已实现。
领域名称以 [CONTEXT](../../CONTEXT.md) 为准；输入来源见 [源码核对记录](../execution/records/2026-09-09-source-assessment.md)。

## 1. 目标与范围

公网 EIP/SNAT 作为后续独立增量在 [租户 VPC SNAT 方案](vpc-snat.md)定义；不改变本首片的历史范围或已完成验收，也不将 kc 手工出网测试计入本首片产品 API 证明。

首片交付：租户创建 VPC、创建 Subnet，现有普通容器通过 Network 接入子网，并验证基本连通和租户隔离。
VPC/Subnet 的持久操作、后台推进、状态判定、失败恢复、占用保护和真实删除必须一起完成。
普通容器先闭环，VM / KubeVirt 随后独立验收；容器通过不代表 VM 通过。

- 使用全新测试环境及空的 Network 数据库，没有遗留 VPC/Subnet；不做旧表兼容、旧资源迁移或双写。
- PostgreSQL 由 Network 独占业务所有权，sqlc/pgx 数据访问，不启用 RLS。
- 一份部署配置管理一个固定集群；标准租户 Namespace 身份由[统一合同](../contracts/tenant-namespace.md)规定，租户不选择集群、Namespace、CR 或 NAD。
- 首片是 IPv4 私有 VPC 网络：同子网、同 VPC 跨子网连通；不同 VPC 默认隔离，跨租户隔离。
- 自定义路由、SG、LB、NAT/EIP、公网访问、Storage 挂载、IPv6、多集群调度不在本片；同 VPC 的系统连接路由由 Provider 实现。
- 服务间身份验证延期到 IAM 契约就绪；这不阻断本期本地功能及指定测试环境联调，不得将其记为认证/授权验收通过。
- 配额接入延期到 Core 重构及治理契约就绪之后独立编排，不作为 NET-05A 或 NET-06 的前置条件；现有引用占用和删除保护继续生效。
- 用户在设计理清后提供虚拟机，再准备 kind；本轮文档工作不创建集群或访问 live 环境。

## 2. 所有权与模块接口

| Owner | 权威职责 | 不能代管的状态 |
|---|---|---|
| Network | VPC/Subnet、Network Attachment、Network Operation、业务状态、幂等、恢复、删除、操作历史 | Tenant 生命周期、IAM 成员/授权、Pod/VM 生命周期、全局配额账本 |
| Gateway | REST 入口、已有入口身份处理、请求/响应及错误转换、调用 Network | Network DB、Provider 执行器、网络状态解释、网络任务完成判定 |
| 实例 owner | 创建/调度/删除 Pod，实例操作恢复，消费接入结果并封闭提交尝试 | VPC/Subnet 状态、Network operation、Provider 网络命名 |
| kc-networking | 网络 CR 到 OVN/设备/IPAM 的实现与观测事实 | Network 产品身份、受理幂等、业务 operation |
| 治理模块 | Tenant 生命周期、权限、配额/预留等各自权威事实 | 通过观察网络状态替 Network 执行或补偿 |
| 中央任务/审计查询 | 可消费 Network 发布的事实形成投影 | 反写 Network 资源或推进 Network operation |

首片不写现有 `async_tasks`，不依赖 Core reconcile-worker；自身 operation 即最小持久工作来源。
无需为内部调度增加 NATS、通用工作流引擎或中央 task-service。Network 对 Pod 的接入核验是只读的，Pod 的写入仍只有实例 owner。

### 2.1 仓库内职责

- `cmd/ani-resource-service`：显式装配数据连接、Provider、worker、服务和传输生命周期。
- `internal/biz`：领域对象、规则、用例及所需 ports；不得依赖 Kratos/protobuf/pgx/Kubernetes 类型。
- `internal/data`：sqlc/pgx 事务、kc Provider、接入观测、时钟与 ID 等实际外部适配。
- `internal/service`：生成的入站契约与用例之间的转换，不直接调用数据库或 Provider client。
- `internal/server`：传输、跨请求中间件、注册、可观测性与运行接线。
- Network 与 ANI/IAM 通过版本化契约连接，不导入彼此 `internal` 或旧 ANI runtime。

### 2.2 身份与租户输入

业务入口从[受信 mTLS 委托](governance-calls.md)建立当前租户/平台范围、Actor 与 Direct Caller；公开 body 不能自由选择租户或伪造内部上下文。资源 tenant、调用服务和行为人分别表达，不因非空 tenant 字符串就认定已认证。

领域访问显式限定可信 tenant，平台对象使用其实际平台作用域；不复制 IAM Membership/Role/Token 或跨服务 FK。后台执行使用已持久接受的意图，不保存用户 token 作为重试凭据。测试替身的上下文只说明对应测试层，不成为真实鉴权通过或生产旁路。

## 3. 资源规则

### 3.1 身份与输入规范化

| 字段 | 规则 |
|---|---|
| 资源 ID | Network 生成、不可变、不重用；外部只作 opaque string，保留 `vpc_` / `subnet_` 前缀。首片生成格式为前缀加 UUID v4 的 32 位小写十六进制，不由名称或 Provider 名推导 |
| operation / attachment ID | Network 生成 UUID，不是 `request_id` 或幂等键 |
| `name` | 首尾去空白后 1–128 个 Unicode 字符；拒绝控制字符；允许同租户重名，引用只使用 ID |
| `description` | 可选，最多 1024 个 Unicode 字符；缺省/null 规范化为空串；Subnet 也支持此字段 |
| `cidr` | 必填 IPv4 CIDR，必须为规范网络地址，拒绝带 host bits 的输入；范围完整落于 RFC1918 私网之一；前缀不长于 /30 |
| `gateway` | Subnet 可选 IPv4 地址；缺省/null 取该子网第一个可用地址，响应始终给出最终网关；拒绝空串、网段地址、广播地址及子网外地址 |
| `exclude_ips` | Subnet 可选 IPv4 地址/范围列表，单地址或 `startIP..endIP`；端点须属于该子网且起止有序；规范化为排序、去重及合并后的排除集合 |
| `enable_dhcp` | Subnet 可选布尔值，缺省为 false；控制 KCN DHCP 配置，IPAM 地址分配仍由 KCN 负责 |
| `dns_servers` | Subnet 可选 IPv4 DNS 列表，规范化并保持首选顺序；DHCP 关闭时仍可保存，开启后通过 DHCP 下发，不直接修改 Pod/guest DNS |
| `idempotency_key` | 创建必填，1–128 个 ASCII 可打印非空白字符；按原值区分大小写，不与 request/correlation ID 混用 |

VPC CIDR 是租户的地址规划范围。Subnet CIDR 必须属于同租户父 VPC；同 VPC 中所有尚未 `deleted` 的子网不得重叠。
2026-10-10 后端合同：新 VPC 首次受理由 Resource 启动 YAML `network.vpc_cidr_presets` 精确限制。管理员配置少量规范 RFC1918 网段；非法、非规范或重复项使配置校验失败，空配置返回空候选并拒绝新受理。修改配置后重启/滚动 Resource 生效；不新增热更新、管理表或写接口。两个 Resource 运行入口注入同一策略，Governance 只转发读取结果。

租户通过 VPC CIDR 候选接口读取配置白名单中的可用选项；首次受理再次核验相同白名单及 Res 已登记的平台地址池冲突。管理员负责避开未纳入产品台账的物理、管理、Pod/Service 等外部网段，不通过全群基础设施扫描构造另一套运行准入。

受理与平台池写入共用 cluster 锁，Provider 首次创建前再检查平台事实和池冲突；外部 Kubernetes 变更与 PostgreSQL 不具有原子提交保证。白名单只约束首次 VPC 受理，旧已受理同意图的幂等重放仍返回原快照，Subnet CIDR 校验不应用这份白名单。最终部署预设必须据实际环境冻结，仓库示例为空，测试 CIDR 不作为部署值。

不同 VPC 可以重复使用相同 CIDR。网关地址由 Network 确定后交 Provider 预留；具体工作负载 IP 分配始终由 kc IPAM 负责。
首次创建 Subnet 保留父产品归属、CIDR 包含/重叠、幂等、占用和管理状态检查；不追加父 VPC Ready 或观测年龄门槛。VM/容器和手动 VNIC 的管理受理按[网络状态消费](cr-observation.md)办理。
已受理请求重放按第 4.3 / 7.1 节先识别持久记录，不重新应用这些动态准入条件。
CIDR、父 VPC 和网关首片不可修改；不提供 PATCH、在线换网或 VPC 迁移。
2026-11-03 用户指定开放子网创建的网关、排除 IP、DHCP 和 DNS。新增三项随创建事务、幂等快照和 Provider 目标持久化，并在 Get/List 回显；默认空列表/false 保持已受理旧请求的指纹。本期只开放创建，未增加已有子网的编辑接口。配置值必须通过正常 KCN 下发及原身份观察后才可用。

并发创建 Subnet：数据层先锁定同租户父 VPC，再检查父状态及所有未删除子网的 CIDR，插入资源和 operation 后提交。
所有入口及恢复路径都遵守这套串行规则。不能用进程 mutex 或“查完再独立 INSERT”代替事务保护。

## 4. 外部 REST 与内部 RPC

REST 前缀继续为 `/api/v1`。下面路径保留现有产品形式；新增/变化行为在 NET-04 同步 ANI OpenAPI、Gateway 和接口测试；Console 后续独立接线，不在 NET-02–04 Goal。
本文是设计来源；本轮不生成 Proto、OpenAPI 或客户端。实现时按仓库固定生成流程建立 `network.v1`，生成文件不手写。

### 4.1 命令与查询

| REST 路径 | 内部 RPC | 输入 / 结果 |
|---|---|---|
| `POST /networks/vpcs` | `CreateVPC` | `name,cidr,idempotency_key,description?`；201 + VPC |
| `GET /networks/vpcs/{vpc_id}` | `GetVPC` | 200 + VPC；始终按租户查询 |
| `GET /networks/vpcs` | `ListVPCs` | 200 + 分页；`name?,state?,page?,pageSize?`（Gov），旧 RPC 见下文 |
| `DELETE /networks/vpcs/{vpc_id}` | `DeleteVPC` | 接受异步删除时 202 + VPC；已 deleted 时 200 + VPC |
| `POST /networks/subnets` | `CreateSubnet` | `name,vpc_id,cidr,idempotency_key,description?,gateway?,exclude_ips?,enable_dhcp?,dns_servers?`；201 + Subnet |
| `GET /networks/subnets/{subnet_id}` | `GetSubnet` | 200 + Subnet |
| `GET /networks/subnets` | `ListSubnets` | 200 + 分页；`vpc_id?,name?,state?,limit?,cursor?` |
| `DELETE /networks/subnets/{subnet_id}` | `DeleteSubnet` | 接受异步删除时 202 + Subnet；已 deleted 时 200 + Subnet |
| `GET /networks/operations/{operation_id}` | `GetOperation` | 200 + Network Operation；不经过 Core task 懒同步 |

201 表示资源意图和 operation 已持久提交，首次响应固定为该次受理快照 `provisioning`，不等待 Provider 完成。
响应 `Location` 指向资源查询 URL；资源携带 `last_operation_id`，可查询执行结果。
Delete 天然按资源目标幂等，无需 body 或额外幂等键；进行中重复 Delete 返回同一删除 operation。
跨租户目标与不存在的目标统一返回 404，不能泄露资源信息。

### 4.2 响应与分页

VPC/Subnet 公共字段：`id,tenant_id,name,description,cidr,state,reason,created_at,updated_at,version,observed_at,observation_stale,last_operation_id`。
其中 `reason` 为可空的稳定原因码，RPC 以 `reason_message` 给出固定的脱敏说明（无原因时两者为空字符串），`observed_at` 尚未观察时为 null，时间均为 UTC RFC3339。
Subnet 增加 `vpc_id,gateway,available_ip_range,used_ip_range`。VPC 增加 `subnet_count`，由同一查询快照统计未 deleted 子网。
`available_ip_range`/`used_ip_range` 分别直接来自匹配 KCN Subnet.status 的 `v4availableIPrange`/`v4usingIPrange`，原样保留逗号分隔的单 IP 与起止范围字符串，例如 `10.42.1.2,10.42.1.4-10.42.1.9`。通过现有观测链和受保护事务持久化为可空 text，Get/List 读取数据库，不同步访问 KCN。明确空字符串表示确定的空范围，字段缺失或类型错误表示未知；有效新观测清除对应旧值。读取失败保留上次成功范围与原 observed_at，以 `observation_stale` 表达最近读取错误或尚无成功观察，不按时间自动失效；租户、binding、namespace、原 UID、配置版本与 lease/epoch 围栏继续生效。范围单独变化也刷新，不从计数、CIDR 或 Attachment 推算。
2026-10-12 按用户纠正用 range 替换公共 count 字段：旧字段名称与 Proto 编号保留为 reserved，新字符串使用新编号；历史迁移 0011 和计数列保留，运行链不再更新或公开这些列。迁移 0013 新增可空范围列，旧行保持未知，不做推算回填。KCN 原有 `v4usingIPs` 删除依赖护栏继续保留。`zone`、`dev_profile`、Provider 对象和命名空间均不公开。
Fedora 上 `scripts/integration -timeout 5m -v -run '^(TestSubnetKCIPRangesDetailsAndFences|TestSubnetIPRangesMigrationPreservesUnknownOldRows)$' ./internal/data/network` 通过，详情围栏和旧行迁移分别为 0.86s/0.47s；两仓 `TestNetworkJointHTTP/subnet_ip_ranges` 通过（范围子场景 0.31s）。KCN 为外部 HTTP fixture，业务、鉴权和 PostgreSQL 使用真实实现；真实集群验收仍为 `not_verified`。
`version` 是业务记录并发版本，不是 Kubernetes resourceVersion；客户端不解释 Provider generation。

Operation 字段：`id,tenant_id,kind,resource_type,resource_id,state,reason,created_at,updated_at,completed_at,next_attempt_at`。
kind 首片为 `create_vpc,delete_vpc,create_subnet,delete_subnet`；内部接入事务有独立 Attachment 状态，不伪装成中央 AsyncTask。

Gov VPC 列表采用 `page/pageSize`，独立默认 1/20，page 为 uint32 正整数，pageSize 范围 1–100。显式非法、重复、溢出及旧窗口参数返回 400。返回 `items/total`；空页 items 为数组，HTTP total 为十进制字符串。Resource 新增有 presence 的 RPC 字段（6/7），任一存在时按 `(page-1)*pageSize` 直接执行 LIMIT/OFFSET，无需预读前页。超界页仍返回完整 total。
VPC 稳定排序为非 NULL 的 `(created_at DESC,vpc_id DESC)`；`name` 是规范化后区分大小写精确匹配，`state` 为单值，不传时排除 deleted。Count/List 使用相同 tenant/name/state 范围及同一个只读 Repeatable Read 快照。
Resource 旧 RPC limit/cursor 字段标记 deprecated，未传页码时保留原 keyset 协议；页码与非零 limit/非空 cursor 混用拒绝。Gov VPC 已明确退出旧协议，不返回 next_cursor。Subnet 及其他资源仍保留既有 limit/cursor 行为及共享 codec，不随本片迁移。
默认排除 deleted；显式 `state=deleted` 可查墓碑。GET 保留 deleted 资源，直到另行设计保留/清理策略。
cursor 是服务生成的带完整性校验的 opaque 值，绑定租户、资源种类、筛选条件和上页边界；跨租户/改筛选/损坏均 400，不改变租户授权。
分页不是跨多个请求的数据库快照；并发插入的新资源从新一轮列表读取。
`subnet_count` 在单次查询事务内一致，不能要求多页间总数不变。

### 4.3 幂等与错误

创建唯一域为 `(tenant_id,operation_kind,idempotency_key)`；不同租户独立，同租户不同 Actor 不另开空间。
在未来接入 IAM 后，每次重放仍先进行当次授权。幂等键不授予读取权限。
规范化业务字段计算带版本的请求指纹，包含目标父资源和显式/默认网关的最终值，不含 trace/request/correlation、Actor 或 Direct Caller。
顺序固定为：租户范围和无状态输入规范化、适用的当次入口授权 → 查询持久幂等记录 → 只有未受理的新意图才执行动态父状态/CIDR/占用检查。
同键同意图返回首次 201 响应快照及同一资源/operation，不覆盖最初归因；同键不同意图返回 409。
已受理同意图不因父资源后来 degraded/deleted 或当前 CIDR 被原资源占用而拒绝；规范化指纹不依赖父资源的当前状态。
首片不自动过期或复用幂等键，删除后的重放也返回原受理身份；当前结果通过 GET 查询。
这有意替换旧“24 小时去重”承诺；保留与清理策略是未来独立决定，不能偷偷加 TTL。
验证失败且未受理时不占用键；事务提交结果未知时客户端使用同键重试，不换新键猜测。

| 原因 | HTTP / gRPC | 语义 |
|---|---|---|
| `INVALID_ARGUMENT`, `TENANT_REQUIRED`, `INVALID_CURSOR` | 400 / InvalidArgument | 输入或必需租户范围无效 |
| `RESOURCE_NOT_FOUND` | 404 / NotFound | 不存在或不属于请求租户 |
| `IDEMPOTENCY_CONFLICT`, `CIDR_OVERLAP` | 409 / AlreadyExists | 重放冲突或已占用地址范围，ErrorInfo reason 区分 |
| `RESOURCE_IN_USE`, `RESOURCE_BUSY` | 409 / FailedPrecondition | 占用或活动操作互斥 |
| `PARENT_NOT_READY`, `NETWORK_NOT_READY`, `PLACEMENT_MISMATCH` | 422 / FailedPrecondition | 父资源、可用性或实例放置不满足 |
| `DEPENDENCY_UNAVAILABLE` | 503 / Unavailable | 无法完成数据库受理/查询，不能返回成功 |
| `DEADLINE_EXCEEDED` | 504 / DeadlineExceeded | 响应超时不表示已提交 operation 被取消 |

Gateway 延续 ANI 统一错误 envelope，将上述稳定 reason 放入对应错误字段，不透传 SQL、Provider 对象、证书或内部地址。
认证相关 401/403 由入口及未来 IAM 接线处理，本期不虚构认证成功。Provider 在受理后失败通过资源/operation 返回，不逆转已返回的 HTTP 状态。

### 4.4 与旧契约的显式差异

保留路径、产品 ID 前缀、name/cidr/gateway、创建 201、资源查询形式；新增 operation 查询和可恢复异步语义。
破坏性差异：CIDR 必填、严格输入规范化、移除 zone/dev_profile（地址范围按上述 available_ip_range/used_ip_range 观测合同接入）、永久保留首片幂等键、删除 202、`pending` 改 `provisioning` 并新增 degraded。
Subnet description、可靠分页、subnet_count 是明确实现项。未知/已移除的请求字段返回 400，不通过接受并忽略来兼容旧客户端。
Console 需更新生成类型与状态展示；对进行中的资源/operation 定时查询（建议 2 秒起、上限 10 秒退避），终态停止，恢复页面时重新查询。
POST 的幂等键在一次逻辑提交及网络重试中保持稳定；新意图才生成新键。

## 5. 状态、事务与后台恢复

### 5.1 状态定义

公开资源枚举、operation 枚举及字段号按生成契约保留。当前资源状态按[网络状态消费 §1](cr-observation.md#1-状态权威与管理职责)直接映射原对象的 Ready 三态；资源的当前投影与历史 operation 结果分别保存。

Ready=True 可更新当前资源并完成尚未结束的对应操作；Ready=False 在首次创建继续等待，旧稳定资源按该合同表达 Degraded；未报告状态保留稳定事实。读取超时、403 或连接错误保留最后成功的状态、原因、地址及时间，仅登记读取错误和重试，不按年龄转为不可用。

已结束 operation 的 state/reason/attempt/completed_at 不被后续观察改写。明确声明提交失败可以记录失败，旧资源失败状态不形成永远不能恢复的锁。deleted/released 是管理终态，不作物理清场或连通性承诺；端到端连通验收单独记录。

### 5.2 事务边界

T1 受理：无状态校验及当次适用授权 → 先查持久幂等记录 → 新意图锁资源/父资源并在锁内重查幂等 → 动态准入/并发检查 → 资源意图 + operation + 幂等受理快照 + 资源历史 → 原子提交。
首次创建资源在同一事务建立 reconciliation 行；后续 mutation 更新该行的调度信息，不创建另一套执行租约。
唯一键争抢的失败方读取胜出方已提交结果并比较指纹；不能因为先看到资源占用就把同意图重放误判为新建冲突。
事务中不调用 Provider。未提交不应存在可执行 operation，提交失败不能留下半份资源。

T2 领取：短事务从 Network operation 找到到期工作，并领取其目标的持久 `network_reconciliations` 资源执行租约，写入 lease owner/expiry/单调递增 lease epoch；operation 记录所绑定的执行 epoch。
正常业务查询不使用跨租户扫描；worker 专用数据入口只能领取本服务已持久化的工作，领取后所有变更仍约束 tenant、资源和 lease epoch。
无活动 mutation 时，精确对象通知与本服务未完成工作唤醒原 reconciliation；稳定任务停放，已管理终态任务退休，不重开已终态 operation 或周期审计墓碑。
mutation 与普通观察共享每资源执行租约；活动 mutation 优先，不允许两个路径并行改变同一声明意图。

T3 外部执行：Network worker 经 Provider adapter 使用稳定产品/操作身份执行 Ensure/Observe/Delete。
对象映射在调用前已持久化；已存在对象必须核对归属，不能盲目认领同名 CR。

T4 记录：校验租户、资源 version、资源执行租约 epoch，以及适用的 operation 执行 epoch；原子保存观测、资源状态、operation 结果及资源历史。
没有活动 operation 的观察仅更新资源/reconciliation 与必要历史，不改写已终态 operation。
如果要发布领域事件，同事务写 Network outbox，由自身 dispatcher 可靠重试；中央消费者停机不得丢事件或接管状态。
本片不要求实现未定义的全局审计/计量/Task Index 消费者，也不将日志写成功冒充事务审计。

### 5.3 恢复与并发

- 使用数据库时钟领取和更新租约；过期持有者的 DB 回写由 version/epoch 条件拒绝。
- 临时失败采用有上限的指数退避及抖动，记录 attempt、next_attempt_at 和脱敏原因；不能仅存在内存队列。
- 进程启动仅恢复本集群已持久化且尚未完成的管理工作，不依赖 Gateway 的下一次请求。已完成管理生命周期的 Deleted/Released 任务退休，不因重启、旧事件或迟到观察重新激活，不恢复墓碑观察或清理。
- Provider 已成功但 T4 未完成：先观察同一稳定映射，确认当前配置/UID 后续记，不能生成第二个产品 ID。
- DB fencing 只保护 DB，不能自动阻止已经发出的 Provider 请求；adapter 必须用条件写入、对象 UID/版本校验及每资源串行执行实现外部并发约束，并用故障测试证明。
- 首片同一资源最多一个未完成 mutation operation。创建尚未终态时 DELETE 返回 `RESOURCE_BUSY`；不实现取消创建或并行 create/delete。
- 结果未知时保留工作与映射，持续观察或 blocked；不得为了释放锁/地址范围伪造终态。
- 终态历史保留原名称、UID、幂等回执和操作结果；取消墓碑审计不删除业务历史。尚未查明的未知提交仍是未完成责任，沿原意图、原名称、已登记身份和原持久任务恢复，不能先伪造完成再以“终态不重开”为由忽略在途写。
- 关闭 Gateway、Core 网络逻辑或中央任务消费者不影响这些流程；DB/kc 故障则影响依赖能力并有明确状态与恢复路径。

### 5.4 删除与资源占用

父产品删除、Subnet/Attachment 新受理与已有引用使用同一管理台账和既定锁顺序；父资源 closing/deleting/deleted 拒绝新子引用。未释放的业务引用仍保护 VPC/Subnet，不能以原生 Ready 代替这些约束。

对自己声明的原对象按 UID/resourceVersion 正常删除。NotFound 或同名不同 UID 结束该原身份的删除，不碰替换对象；不枚举或等待 KCN 自动子资源清场。自己的子产品、未完成管理步骤和引用分别继续收尾，不因根对象消失丢弃责任，也不把原生内部对象添加为新责任。未知创建沿原意图和确定名称恢复，不能换名猜测成功。

### 5.5 观察与持久排程

观察通知、requested/processed generation、Claim/Finish、lease/epoch、退避、启动恢复和终态退休统一见[网络状态消费 §2–3](cr-observation.md)。普通 informer 通知进入原有任务，执行者 GET 目标；不维护关系索引、Covers、TTL 或墓碑审计。查询不产生写入或唤醒，observed_at 不是准入凭据。

## 6. 持久化设计

关系型业务身份、意图、引用、版本及租约为有类型字段；有限 JSON 用于冻结请求/回执和适用 checkpoint，不维护可独立修改的第二份 spec/status 权威。租户资源、operation、幂等、binding 与历史通过同租户目标关系约束；平台资源不伪造 tenant。

产品状态、引用和对应 operation/永久幂等回执在本地事务提交；Provider 在事务外执行，Finish 校验当前版本、租约 owner/epoch 和有效期。历史身份、幂等、operation 和必要业务历史保留，已结束任务退休。不能清表、重写旧回执或复制 proof 字段恢复已废止机制。

实际表、索引、约束与升级步骤以[迁移源码](../../migrations)和[查询源码](../../internal/data/network/queries)为准，状态消费的升级责任见[网络 v3 §8](cr-observation.md#8-升级与验证)。本文不复制已经被迁移删除的证据列、生成物表或调度 schema。

## 7. Network Attachment 与实例接入

该接口只面向实例 owner 的内部适配，不加入租户 VPC/Subnet CRUD body。
Network 自己提供接入登记/查询/释放能力；实例 owner 必须持久化对应调用步骤，不能只做一次 GET 子网检查。
本期同一实例只支持一个 `primary` 网络 slot，接入目标不能在同一 attachment 内变更。

### 7.1 接入协议

Prepare 先验证当前业务授权和规范化意图，再永久重放原 tenant/consumer/slot/request_key；首次受理在父资源与 slot 锁下验证产品引用、CIDR/位置及管理状态，登记 reserved。相同 key/意图复用原 Attachment，异意图冲突；结束关系不能因重放重新占用或交付可再次提交的旧计划。

交付的 pod_primary / vm_primary 是受限版本化内部格式，只含相应适配所需的 namespace、网络投影、受管 NAD 或关联标签；不是任意 Pod patch。owner 保存冻结计划、submission/generation、原对象身份和后续步骤，不从产品 ID 猜原生位置，也不允许用户覆盖受管字段。

Confirm 登记当前消费者绑定并沿已有工作推进。owner 负责自己创建的根/运行对象及适用 UID/owner 核验，Network 消费其当前管理快照；不再反向发现 Pod 或逐跳核验控制器/网卡链。具体字段以[network.proto](../../api/network/v1/network.proto)及[网络 v3 §4](cr-observation.md#4-attachment-与计算侧)为准。

### 7.2 释放与竞争

owner 先封闭未来提交并查明未知写，完成自己负责的运行对象退出，再发布同一 finalization 的 closed。Network 按该当前管理关系释放；open/stopped 保留预留，closing 等待真实关闭，closed 释放，不等待 KCN 网卡/IP 自动子资源清场。consumer closed 不等待 Network released。

准备、删除准入和释放遵守同一产品台账和父资源锁顺序。消费者读取错误、身份不符或未知状态不能当 closed，也不能按 TTL 解除占用。实例/VM owner 不跨域写 Network SQL，Network 不接管计算对象清理。

### 7.3 接入后台推进

Prepare/Confirm/Release 的管理意图、引用和通知持久提交；当前快照变化沿 owner 现有工作通知 Network，跨库投递可恢复，不引入第二个证明通道。

Claim/Finish 保留 tenant、version、当前 revision、lease owner/epoch 与数据库时钟保护；低代和晚到结果不覆盖新关系。稳定任务停放，读取错误按原退避重试；Released 永久退休，旧事件和进程启动不重新激活。四列当前地址独立读写，错误保留与明确撤回只见网络 v3，不参与释放判定。

### 7.4 关联实例查询与消费者类型

`network_attachments.consumer_type` 记录消费者属于哪个产品（`container_instance`/`virtual_machine`/`sandbox`，空值只表示该列之前写入的历史行），
与 `plan->>'WorkloadMode'` 的接入协议模式分离：模式不是类型，用模式反推类型会把实现细节当成领域事实。
类型也不进入 `PrepareAttachment` 的意图指纹（Go 侧 `json:"-"`），否则新增字段会改变哈希，使存量 attachment 的重放全部变成 `IDEMPOTENCY_CONFLICT`。
Prepare 校验类型与模式一致：`controller_managed`→`container_instance`、`vm_managed`→`virtual_machine`；
外部 `single_pod` 意图不拥有 ANI 资源记录，继续不带类型，不从它恰好使用的 ID 猜一个类型。
迁移 0028 只回填 `plan->>'WorkloadMode'` 能确证的两类，其余保持空值并在查询里返回 `unknown`。

`ListNetworkInstances(tenant_id,vpc_id|subnet_id,page,page_size)` 是治理面只读查询，二选一必填，同时提供时校验子网属于该 VPC。
先确认当前租户可读的私有或平台网络行存在，否则 `RESOURCE_NOT_FOUND`，使“网络不存在”与“存在但没有关联”可区分；已知平台 ID 的查询不受发布开关影响。
Count 与 List 在同一 Repeatable Read 只读事务内，以当前真实租户和 `state<>'released'` 为相同谓词；VPC/Subnet 同时提供时按 AND 过滤。
`subnet_name` 由私有和平台子网分别 LEFT JOIN 后按该行的有效引用选择，不加 `state<>'deleted'` 过滤；
排序固定 `created_at DESC,attachment_id DESC`，不用 `updated_at`，避免观察刷新让翻页漏行或重复。
名称与运行状态由 `ConsumerDirectory` 端口跨库批量补齐（Instance/VM 与 Network 是分离的库，不存在可用的 join），
一次请求每种类型只读一次；查不到资源的行仍然返回，消费者字段为 `unknown`，不删行也不改判类型。
地址只读 Network 已持久化的当前输出，不追加逐行 KCN 调用或时效/Ready 门槛；读错保留、消费者更换及 stopped/closing/closed 撤回规则统一见[网络状态消费规格](cr-observation.md#4-attachment-与计算侧)。`observation_stale` 与“地址为空”分别表达。
本查询不合成“已选网但尚未 Prepare 成功”的行（那没有 attachment 记录），也不聚合子网/LB/EIP，见[治理调用登记](governance-calls.md)。

## 8. Provider 契约与接线

Network 拥有需要的业务端口，data adapter 实现原生声明、精确观察和原 UID 删除；不建设通用 provider 注册中心。稳定名称、cluster/namespace、管理者、tenant、产品/binding 和原 UID 用于本服务写入身份与并发，不扩展成网络正确性的证明。

VPC/Subnet 的状态直接使用各自 Ready；不额外检查 Valid/Initialized、router、父资源健康、generation 或 KCN 内部对象。Subnet 的产品 vpc_id 映射到原生 gateway 引用，产品 gateway IP 与该引用分开；缺少产品引用不能静默接入默认网络。未知 CREATE 沿确定名称认回，原 UID 缺失不自动新建另一代对象。

删除只对登记的原声明使用原生 UID/resourceVersion 前提，正常 finalizer 由组件处理；原对象缺失或已被替换时不删除新对象，不等待自动子资源。原生请求取消、超时和数据库租约不混作请求未执行的证据；当前处理规则以[状态消费合同](cr-observation.md)为准。

### NET-01 的落地边界

首片只交付 VPC 的历史表、schema 和恢复限制见[原 NET-01 记录](../execution/records/NET-01-implementation.md)。它们说明原版本，不覆盖后续 Network v3，也不要求恢复 Orphan 删除、墓碑审计、旧共享观察错误或内部证明字段。

### 8.1 调用适配与 Console

Network 的入站 RPC、可信服务身份、租户/平台委托和公开 HTTP 适配按[服务调用合同](governance-calls.md)及生成 API 定义。Governance 适配传输和当前权限，owner 保持计算生命周期；各服务不导入彼此 internal 或旧 ANI runtime。Console 展示产品状态、历史 operation 和观察错误，不把地址非空或 HTTP 成功等同于网络连通。

## 9. 可观测性与验收

保留 Kratos 日志、trace、metrics、数据库及执行器 readiness。请求、读错、通知重试、排程与积压分别报告，tenant/resource ID 不作无界 metrics label。遥测和日志不产生可用性证明或运行准入。已完成 operation、当前资源与观察错误分别解释。

| ID | 行为与证据要求 |
| --- | --- |
| V-01 | 固定生成、分层、格式及实现变更的仓库必要检查；文档变更只查链接与格式 |
| V-02 | 迁移/checksum、受限 owner/runtime 和租户 CRUD/幂等/关系边界 |
| V-03 | 租户谓词与复合关系负例，不以 superuser 行为代替 runtime |
| V-04 | 永久同键重放、异意图冲突、删除后不重建、历史回执不改写 |
| V-05 | CIDR/父产品管理关系、同 VPC 重叠互斥，不追加父 Ready/TTL 准入 |
| V-06 | 持久受理独立于入站请求存活，其他服务不兜底写 Network 状态 |
| V-07 | 原数据库中断点恢复，不丢未知意图或认错原生身份 |
| V-08 | 版本/lease/epoch、通知和迟到结果的真实并发保护 |
| V-09 | Get/List/GetOperation 只读；旧时间或 Watch 健康不构造 TTL 门槛 |
| V-10 | Prepare/删除互斥、owner closing/closed、管理释放，不做网络子资源清场 |
| V-11 | 读错保留、NotFound/身份替换区分、原 UID 删除不触碰新对象 |
| V-12 | 真实普通容器网络连通/隔离，保留源码、端点与清理范围 |
| V-13 | 正式产品接口、分页、权限及无查询仍可推进的持久执行 |
| V-14 | VM 路径与 guest 网络独立验收，不由普通 Pod 结果推导 |
| V-15 | 当前 mTLS、可信委托和跨租户负例，不沿用历史未认证上下文 |
| V-16 | 原生状态直接映射与读取失败保留，见[观察验证](cr-observation.md#8-升级与验证) |
| V-17 | 通知 Claim/Finish 交错、退避与旧 lease 回写拒绝 |
| V-18 | 已结束任务退休、旧事件/启动不复活，升级保留业务台账和回执 |
| V-19 | 对应版本真实 API/计算消费验收，单独记载未验范围 |

STATIC、真实 PostgreSQL、受控 Provider、真实集群、来宾与浏览器分别记录 pass/fail/not_verified；历史容量或关系审计结果保留原日期，不要求恢复旧机制以重现它们。只读材料和模拟结果不能替代真实连通，kind 多节点也不外推物理多机或生产 HA。

## 10. 后续依赖与设计变更

已确定方向无需重复确认；实现中常规细节可以在本规格内补齐，并同步测试与相关 ADR。
不得把本轮工程选择冒充已实测能力；遇到与产品规则或外部契约冲突，记录具体差异再调整设计。

| 依赖 | 何时必须解决 | 当前设计处理 |
|---|---|---|
| kc 固定 API/镜像、就绪/删除/接入行为 | NET-03 接入 DTO 和 NET-05 真实验证前 | 从外部团队取得可验证契约，实际缺口由其修复；业务 adapter 测试可先推进 |
| 实例提交封闭/恢复协议、放置与 Pod 绑定 | NET-03 | 同步改实例接入适配；没有释放证据时保留占用，不猜测 |
| Gateway OpenAPI/错误 envelope/生成门禁的准确实现 | NET-04 | 按该仓库当前源码固定基线并更新消费者，不照搬旧 drift |
| 用户提供 VM / kind 环境参数 | NET-05 | 后续提供，不阻塞文档/本地实现 |
| 共享观察及持久调度实现、真实 Watch 权限和新版本复验 | NET-05A，NET-06 前 | [观察方案](../plans/cr-observation.md)；不复用旧 NET-05 结果冒充新实现验收 |
| KubeVirt 可用接入方式 | NET-06 | NET-05A 通过后，单独契约和真实验收 |
| IAM Workload 调用契约 | NET-AUTH | 暂缓 S2S 验证；保留入站适配，不在 Network 实现临时 IAM |
| Core 重构后的配额治理契约 | 后续独立工作包，尚未编排 | 明确延期；不预建配额表/RPC/结算，不阻断 NET-05A/06 |

生产切流、存量迁移、全平台配额/计量/任务中心改造不作为本片隐含交付。

## 手动 VNIC 与实例选择

普通 VM/Notebook 仍以 Subnet/安全组声明由 KCN 自动分配 VNIC，不创建手动产品记录。用户显式申请的 VNIC 由 Network 保存产品身份、原 Provider UID、分配事实、operation 和绑定关系；VNicIP/IPAM 继续归 KCN。

创建可选 `requested_ip`、`requested_mac`，留空分别自动分配，创建后不可修改。请求值与实际 `primary_ip` / `mac_address` 分开持久化和展示；只有 KCN 确认实际值匹配才可用。IP 必须为所选子网内的 IPv4 主机地址，不能是网段、广播或网关；KCN 负责分配池和占用冲突，异步拒绝如实终止创建操作，地址占用以 `ADDRESS_CONFLICT` 表达。MAC 规范化为小写六段、非零单播地址，同子网未删除的手动产品网卡之间以数据库唯一约束保护（包含已自动分配的手动网卡）；不扫描或接管 KCN 自动网卡。实例停止、解绑或删除不释放手动网卡 MAC，显式删除网卡完成后才释放。

手动 VNIC 使用独立 bindings 集合，本期仅开放 primary 槽位与单消费者独占；运行中当前 A 和目标 B 可同时保留。实例 owner 通过持久命令预留、同步最新安全组并保存目标，保存不改在线 Pod 或运行模板。平台重建端点时才应用模板；停止态展示下次启动配置。实例删除只释放其关系，VNIC/IP 保留，只有显式 DeleteVNic 才删除原 UID 对象。留存网卡继续保护 Subnet 与实际安全组引用。自动模式保留原 Pod-owner 核验，手动模式核验持久身份、有效消费者绑定及原 Pod 引用。

创建和预留保留真实租户、placement、原 Namespace/绑定身份、自己的管理状态和消费者独占。Ready、地址是否分配、Reason 和观测时间不作为入口条件，遵循[网络状态消费规格](cr-observation.md#4-attachment-与计算侧)。平台分支的首次受理和已受理引用规则见下节。

## 平台基础网络租户接入

### 目录、所有权与可见查询

受信配置 `network.platform_base_network` 登记原集群 UID、固定 `kcn-system/kcn-cluster` VPC 及显式列出的 Subnet。每项具有稳定产品 ID、原 kind/namespace/name/UID、显示名、描述、`published`、`accept_new`；配置和原身份不符时拒绝同步，不从 KCN 自动发现或接管对象。移除配置项只关闭两个开关，保留登记、引用和观察队列。

VPC/Subnet 返回 `owner_scope=tenant|platform`、`can_manage`、`can_use`。平台网络不伪造 tenant，VM、Instance、VNIC 和 Attachment 始终保存实际租户。平台 `can_manage=false`；`can_use` 只取父子目录发布/新接入开关及已记录的原 UID 冲突，不取 Ready、投影时间或地址。私有网络能力沿其管理状态和归属计算。

List 在数据库合并私有和已发布平台目录后统一过滤、计数及分页：VPC 保持 page，Subnet 保持 cursor，count 和本页处于同一只读快照。两个可选 bool 保留未传、false、true 三态；Subnet cursor 绑定它们和原筛选条件。平台 Subnet List 同时要求父与自身 published；平台 VPC 的 subnet_count 只数已发布子网。已知 ID Get 使用原读权限，不受 published/accept_new 影响。

平台 DeleteVPC、CreateSubnet、DeleteSubnet、租户 SNAT/LB 管理请求在受理和 Provider 写入前返回 PermissionDenied。未知 ID/其他租户私有 ID 仍 NotFound。平台基础出口在界面显示为平台托管，不并入租户基础连接分配或租户 VPC/Subnet 配额。

### 首次受理与持久引用

`ResolveNewNetworkSelection` 供只读 Resolve 和真正新建分支使用；VM、Instance、VNIC 在各自持久重放检查之后、首次插入之前重新判断新接入；Instance 保留既有 DELETE-first 墓碑分支并跳过新接入检查。平台父子开关和身份冲突由一条 SQL 读取，其返回时点为本次判定点；允许已通过读取的在途请求完成，不增加跨库预约或分布式锁。

owner 内部快照固定 `NetworkReference{OwnerScope,VPCID,SubnetID,SubnetBindingID}`；旧快照无 scope 时按私有租户引用解释。内部 `PrepareAccepted` 及后续 Start/Restart、Confirm/Release/Delete、已有 VNIC Reservation 读取固定引用，不重查目录发布资格。目录拒绝的新请求返回 `NETWORK_NEW_ADMISSION_CLOSED` / FailedPrecondition；Gov Network 返回 412，VM/Instance 同步入口沿其依赖错误转换为 400，异步投递保留既有 blocked/charge 语义。

### 自有声明与数据分支

首次接入不要求租户曾创建私有网络；登记规范租户 Namespace 原 UID后，声明集群级 `BasicNetworkIsolation`，固定名 `ani-tenant-<完整租户 UUID>`，仅设 `spec.enabled=true`。自己的标签和已存 UID 必须一致，原 UID 已存但对象消失不重建；不等待 Ready/成员集合或读取 OVN。组名写入手动 VNIC 的 `spec.isolationGroup`、VM template annotation 和 Notebook metadata annotation。最后一个消费者删除后保留 BNI、NAD、SA 及平台网络。

迁移 `0033_platform_base_networks.sql` 增加目录与只读观察队列。VNIC/Attachment 保留原带 tenant 的私有父 FK，新增 `(platform_vpc_id,platform_subnet_id)` 平台父 FK；Attachment 的平台 FK 还包含固定 `binding_id`。CHECK 保证两套父引用只能选一支。私有 FK/父锁保持；平台分支只锁自己的消费者行，不让不同租户因平台父排他锁串行。共享 Subnet 的未删除手动 VNIC 显式请求 MAC 跨租户唯一；请求 IP 做格式校验，地址范围、池和冲突分配由 KCN 负责，不等待目录 CIDR 投影。

导入目录的观察和调度见[只读观察分支](cr-observation.md#9-平台基础网络目录的只读观察)。本功能以已合入 main 的 #57（schema 0032）为基线，只需协调本次 schema 0033、认识共享 Reference 的 Res、对应 Gov SDK 与 Console 发布。已有共享消费者后关闭新接入可暂停申请，不能回退到不认识共享引用的旧二进制。

正式 RPC 以 `api/network/v1/network.proto` 为字段合同；VM/Instance 各自拥有 GetNetworkInterfaces / SetNetworkInterface，网络配置读模型以 `api/network/v1/vnic.proto` 为准。普通读只消费数据库投影，分配、预留、模板和在线接入分别表达。实施顺序和验收范围见[实施计划](../plans/kcn-vnic-management-20261103.md)，实际结果见[执行状态](../execution/status.md#kcn-vnic-management-20261103)。
