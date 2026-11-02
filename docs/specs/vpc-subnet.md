# 租户 VPC / Subnet 首片规格

> 2026-10-19：Notebook 实例接入新增[受控工作负载合同](instance-configuration-network.md#6-res-network-支持-notebook-的-pod-替换)。下文不同 Pod UID 冲突和 Consumer v1 描述继续适用于原 `single_pod` 模式；`controller_managed` 使用持久版本、独立 owner 链核验和当前/历史/待核验关系，不能据新模式放松旧模式的身份保护。实际交付只见[执行状态](../execution/status.md#instance-configuration-and-network-2026-10-19)。

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

本期内部调用显式提供非空 UUID `tenant_id`，入口将它转换为 Network 自有的单租户执行范围。
这个范围限定数据访问，不证明调用者已经认证。公开创建 body 不增加自由选择租户的字段；Gateway 使用当前请求所处租户，清除客户端伪造的内部上下文。
直接业务测试从测试适配器提供租户；没有租户时返回 `TENANT_REQUIRED`，不得回落 `demo-tenant`、空租户或平台全表权限。

资源 `tenant_id`、Actor、Direct Caller 分开。未知/未验证归因不填虚构 IAM Principal；需要保留的调用归因记录注明未验证。
后续 IAM 在入站适配层建立已验证上下文；其 token、Grant 和 proof 不进入领域 schema。
Network 不复制 Membership/Tenant Access，不创建平行 Tenant，也不向 IAM/Core 表建立外键。
后台操作使用已持久受理事实运行，不保存用户 token 供重试；未来异步执行身份按届时已接受的 IAM 契约接入。

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
| `idempotency_key` | 创建必填，1–128 个 ASCII 可打印非空白字符；按原值区分大小写，不与 request/correlation ID 混用 |

VPC CIDR 是租户的地址规划范围。Subnet CIDR 必须属于同租户父 VPC；同 VPC 中所有尚未 `deleted` 的子网不得重叠。
2026-10-10 后端合同：新 VPC 首次受理由 Resource 启动 YAML `network.vpc_cidr_presets` 精确限制。管理员配置少量规范 RFC1918 网段；非法、非规范或重复项使配置校验失败，空配置返回空候选并拒绝新受理。修改配置后重启/滚动 Resource 生效；不新增热更新、管理表或写接口。两个 Resource 运行入口注入同一策略，Governance 只转发读取结果。

租户经 `GET /api/v1/networks/vpc-cidr-presets` 读取当前无冲突的 `cidrs`；CreateVPC 保留 `cidr` 字段，首次受理再次验证相同白名单。CIDR 候选由平台预规划，管理员负责避开现场物理/管理网段和其他未纳入平台的外部网段。在线冲突事实包括 KCN 默认子网、完整 ServiceCIDR、PodCIDR、平台 System/Public/Intranet 子网与尚未释放的 ANI 地址池，不再依赖宿主网卡采集。`intranetNetworks` 聚合路由不作为已分配地址范围。必需的 Kubernetes/KCN 事实缺失或过期时返回依赖不可用，服务启动不要求在线读取集群。

受理与平台池写入共用 cluster 锁，Provider 首次创建前再检查平台事实和池冲突；外部 Kubernetes 变更与 PostgreSQL 不具有原子提交保证。白名单只约束首次 VPC 受理，旧已受理同意图的幂等重放仍返回原快照，Subnet CIDR 校验不应用这份白名单。最终部署预设必须据实际环境冻结，仓库示例为空，测试 CIDR 不作为部署值。

不同 VPC 可以重复使用相同 CIDR。网关地址由 Network 确定后交 Provider 预留；具体工作负载 IP 分配始终由 kc IPAM 负责。
首次受理创建 Subnet 要求父 VPC `available` 且观测未过期。首次受理接入要求 VPC 与 Subnet 都满足此条件。
已受理请求重放按第 4.3 / 7.1 节先识别持久记录，不重新应用这些动态准入条件。
CIDR、父 VPC 和网关首片不可修改；不提供 PATCH、在线换网或 VPC 迁移。

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
| `POST /networks/subnets` | `CreateSubnet` | `name,vpc_id,cidr,idempotency_key,description?,gateway?`；201 + Subnet |
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
`available_ip_range`/`used_ip_range` 分别直接来自匹配 KCN Subnet.status 的 `v4availableIPrange`/`v4usingIPrange`，原样保留逗号分隔的单 IP 与起止范围字符串，例如 `10.42.1.2,10.42.1.4-10.42.1.9`。通过现有观测链和受保护事务持久化为可空 text，Get/List 读取数据库，不同步访问 KCN。明确空字符串表示确定的空范围，字段缺失或类型错误表示未知；有效新观测清除对应旧值。读取失败不续鲜，历史范围以 `observation_stale` 表达过期；租户、binding、namespace、UID、generation、lease 和观测时间围栏继续生效。范围单独变化也刷新，不从计数、CIDR 或 Attachment 推算。
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

| Resource State | 含义 |
|---|---|
| `provisioning` | 已受理，尚未取得本次配置可用的证据 |
| `available` | 最新有效观测确认配置已应用且满足 Provider 就绪契约 |
| `degraded` | 已经 available 的资源不再满足就绪，或观测超过有效期 |
| `failed` | 创建存在不可恢复错误，相关未知执行结果已澄清；是否仍需清理由记录明确表达 |
| `deleting` | 已接受删除；占用与 Provider 清理尚未全部确认 |
| `deleted` | 相关接入释放、Provider 删除已确认，保留产品墓碑 |

Operation 状态为 `queued,running,retrying,blocked,succeeded,failed`。
queued/running/retrying/blocked 均非终态；blocked 表示必须外部修复/明确处理的原因，仍由 Network 维护重查计划。
succeeded/failed 是不可重写的历史结果。创建 succeeded 后资源发生故障，只更新资源，不把该操作改成 failed。
首片删除 operation 不进入 failed：未确认清理完成时保持 retrying/blocked，重复 DELETE 返回同一持久操作；只有完成才 succeeded。
这避免出现永久 deleting 却没有可恢复工作记录的状态。删除准入失败发生在受理前，不产生 operation。
Provider 超时、失联、执行结果未知不能直接映射为业务 failed 或 deleted。
创建确证失败后可由 DELETE 清理；不增加“重试同一失败创建变成另一意图”的隐式接口。

NET-05A 前的初版轮询默认后台观察间隔 10 秒、观测有效期 60 秒，均为有界运行配置。NET-05A 保留有效期与准入语义，观察方式及各类时钟改由[持续观察规格](cr-observation.md#4-事实时效与真实校验)定义；该增量是待实现目标。
过期状态由 worker 持久推进为 degraded；查询可以纯计算 `observation_stale`，不能触发写入。
接入必须同时满足 state=available 和 observation_stale=false，防止 worker 停止后凭旧快照继续绑定。
available 表示配置就绪，不替代端到端连通验收。

### 5.2 事务边界

T1 受理：无状态校验及当次适用授权 → 先查持久幂等记录 → 新意图锁资源/父资源并在锁内重查幂等 → 动态准入/并发检查 → 资源意图 + operation + 幂等受理快照 + 资源历史 → 原子提交。
首次创建资源在同一事务建立 reconciliation 行；后续 mutation 更新该行的调度信息，不创建另一套执行租约。
唯一键争抢的失败方读取胜出方已提交结果并比较指纹；不能因为先看到资源占用就把同意图重放误判为新建冲突。
事务中不调用 Provider。未提交不应存在可执行 operation，提交失败不能留下半份资源。

T2 领取：短事务从 Network operation 找到到期工作，并领取其目标的持久 `network_reconciliations` 资源执行租约，写入 lease owner/expiry/单调递增 lease epoch；operation 记录所绑定的执行 epoch。
正常业务查询不使用跨租户扫描；worker 专用数据入口只能领取本服务已持久化的工作，领取后所有变更仍约束 tenant、资源和 lease epoch。
无活动 mutation 时，同一 reconciliation 记录根据 next_observe_at 领取持续观察或墓碑检查，不重开已终态 operation。
mutation、正常观察与墓碑清理共享每资源执行租约；活动 mutation 优先，不允许两个路径并行改变同一 Provider 意图。

T3 外部执行：Network worker 经 Provider adapter 使用稳定产品/操作身份执行 Ensure/Observe/Delete。
对象映射在调用前已持久化；已存在对象必须核对归属，不能盲目认领同名 CR。

T4 记录：校验租户、资源 version、资源执行租约 epoch，以及适用的 operation 执行 epoch；原子保存观测、资源状态、operation 结果及资源历史。
没有活动 operation 的观察仅更新资源/reconciliation 与必要历史，不改写已终态 operation。
如果要发布领域事件，同事务写 Network outbox，由自身 dispatcher 可靠重试；中央消费者停机不得丢事件或接管状态。
本片不要求实现未定义的全局审计/计量/Task Index 消费者，也不将日志写成功冒充事务审计。

### 5.3 恢复与并发

- 使用数据库时钟领取和更新租约；过期持有者的 DB 回写由 version/epoch 条件拒绝。
- 临时失败采用有上限的指数退避及抖动，记录 attempt、next_attempt_at 和脱敏原因；不能仅存在内存队列。
- 重启扫描未完成 operation，以及需要重新观察的资源和墓碑；不依赖 Gateway 的下一次请求。
- Provider 已成功但 T4 未完成：先观察同一稳定映射，确认当前配置/UID 后续记，不能生成第二个产品 ID。
- DB fencing 只保护 DB，不能自动阻止已经发出的 Provider 请求；adapter 必须用条件写入、对象 UID/版本校验及每资源串行执行实现外部并发约束，并用故障测试证明。
- 首片同一资源最多一个未完成 mutation operation。创建尚未终态时 DELETE 返回 `RESOURCE_BUSY`；不实现取消创建或并行 create/delete。
- 结果未知时保留工作与映射，持续观察或 blocked；不得为了释放锁/地址范围伪造终态。
- 删除后墓碑仍记录 Provider 身份；若检测到迟到/漂移对象，Network 自行清理并暴露异常，不让 Core 修复。
- 关闭 Gateway、Core 网络逻辑或中央任务消费者不影响这些流程；DB/kc 故障则影响依赖能力并有明确状态与恢复路径。

### 5.4 删除与资源占用

VPC 仅在所有子网已 deleted 时接受删除，不级联删除子网；Subnet 仅在所有 Attachment 已 released 时接受删除。
创建 Subnet 与删除 VPC 使用同一父 VPC 锁；PrepareAttachment 与删除 Subnet 使用同一 Subnet 锁并核对父 VPC 状态。
加锁顺序固定为 VPC → Subnet → Attachment/Operation，避免相反顺序产生死锁。
deleting 资源拒绝新子资源/接入；删除调用采用已记录 Provider UID，禁止删除重建后的同名他人对象。
只有确认 Provider 目标及其由本资源拥有的派生项已清理，且没有未澄清的外部 mutation，才能记 deleted。
明确 NotFound 可作为删除完成的必要观测之一，但不能忽略仍在执行/结果未知的旧请求。
未 deleted 的失败/删除中子网仍占用 CIDR，避免与残留数据面重叠。

### 5.5 NET-05A 持续观察增量

NET-05A 的观察范围、共享索引、持久通知代次、公平调度、真实校验及多副本规则统一见[持续观察规格](cr-observation.md)。上述 T1～T4、状态和删除规则继续适用，通知进入同一执行路径。公开版本与内部观察控制字段的边界见该规格的[查询与版本约束](cr-observation.md#5-查询版本与数据库成本)，不在本文件另建一套调度 schema。

## 6. 持久化设计

以下是表与约束设计，不是已执行 DDL。关系型权威字段使用 typed columns；JSON 只用于不可变响应快照或有限的事件/审计快照。
不引入与核心字段重复且可独立修改的 spec/status JSON 副本。

| 表 | 主键 / 唯一域 | 必需内容与关系 |
|---|---|---|
| `network_vpcs` | PK `(tenant_id,vpc_id)`；全局唯一 `vpc_id` | name/description、IPv4 cidr、state/version、观测时间、错误、时间戳、删除意图 |
| `network_subnets` | PK `(tenant_id,subnet_id)`；全局唯一 `subnet_id` | `(tenant_id,vpc_id)` FK、cidr/gateway、state/version、观测与时间；提供 UNIQUE `(tenant_id,vpc_id,subnet_id)` |
| `network_operations` | PK `(tenant_id,operation_id)` | kind/state、恰好一个 `target_vpc_id` 或 `target_subnet_id`，分别有租户复合 FK；快照指纹/重试/绑定执行 epoch/时间；目标与 kind CHECK；活动 operation 对目标分别部分唯一 |
| `network_reconciliations` | 每租户资源一行，VPC/Subnet 闭合目标分别复合 FK / 唯一 | next_observe_at、lease owner/expiry/epoch、当前工作类型；mutation、持续观察和墓碑检查共用资源租约 |
| `network_idempotency` | PK `(tenant_id,operation_kind,idempotency_key)` | 指纹版本/摘要、受理响应、resource 引用、`(tenant_id,operation_id)` FK；不存在无 tenant 的 GetByKey |
| `network_provider_bindings` | PK `(tenant_id,binding_id)` | 恰好一个 VPC/Subnet 目标及复合 FK；cluster/ref/UID/期望版本；唯一 `(cluster_id,resource_kind,namespace,provider_name)`；Subnet binding 提供 UNIQUE `(tenant_id,subnet_id,binding_id)` |
| `network_attachments` | PK `(tenant_id,attachment_id)` | `(tenant_id,vpc_id,subnet_id)` FK、`(tenant_id,subnet_id,binding_id)` FK、instance_id、slot、state/version、request_key/指纹/首次结果、Confirm 身份、consumer_finalization_id、已观察 workload UID 与时间、next_check_at 与执行 lease owner/expiry/epoch；活动 `(tenant_id,instance_id,slot)` 唯一；永久 UNIQUE `(tenant_id,instance_id,slot,request_key)` |
| `network_resource_history` | PK `(tenant_id,history_id)` | 恰好一个 VPC/Subnet 目标及租户复合 FK；可选 operation 引用须满足同租户、同目标 FK；状态迁移/原因/归因/时间；追加写入，不保存 credentials 或任意请求原文 |
| `network_attachment_history` | PK `(tenant_id,history_id)` | attachment 租户复合 FK；接入迁移/协议异常/原因/归因/时间，随状态变化同事务追加；不制造虚拟 Network Operation |

resource.last_operation 由数据层在同事务维护，并通过同租户 FK/目标一致性保证不会指向另一资源的 operation；不能只保存未约束的字符串。
idempotency 的资源引用采用与 operation 一致的闭合目标结构及约束，不使用任意类型+任意 ID 的无外键映射。
资源历史可记录无活动 operation 的持续观察与墓碑清理；只有关联真实 operation 时才填写 operation 引用，不能为记历史重开已终态操作。
Actor/Direct Caller 仅外部引用和已验证程度，不能建立 IAM 跨库 FK；instance_id 同样是外部引用。
平台级迁移版本/运行配置若需要独立表，保持平台数据，不构造 tenant_id。

每个租户读写、条件更新、幂等、Provider 映射和历史查询都有显式 tenant 谓词；更新带预期 version/lease epoch。
SQL 位于 Network 自有 query/migration 路径，生成代码封装在 data 层，用同一事务绑定 sqlc 查询。
运行角色无 schema owner/DDL 能力，普通调用不走 worker 的领取入口；migration 与运行凭据分离。
无 RLS 是明确选择，应用角色可访问其授权表内多租户行的风险不能被包装成数据库自动隔离；以复合约束、代码入口和真实负向测试验证范围。

## 7. Network Attachment 与实例接入

该接口只面向实例 owner 的内部适配，不加入租户 VPC/Subnet CRUD body。
Network 自己提供接入登记/查询/释放能力；实例 owner 必须持久化对应调用步骤，不能只做一次 GET 子网检查。
本期同一实例只支持一个 `primary` 网络 slot，接入目标不能在同一 attachment 内变更。

### 7.1 接入协议

1. `PrepareAttachment(tenant_id,instance_id,subnet_id,slot,request_key)`：完成无状态规范化及当次适用授权后，先查询永久唯一域 `(tenant_id,instance_id,slot,request_key)`。新意图在同租户事务锁父资源、重查幂等，验证可用性和放置一致性，建立 `reserved` 记录。指纹包括 subnet/instance/slot 等业务意图，相同 key/意图返回同一 attachment，改变意图冲突；同实例 active slot 不能指向另一子网。
2. 返回 `attachment_id,version,binding_revision` 和面向实例基础设施适配的 `pod_primary` 接入方案。Provider adapter 负责产出网络所需配置，领域只管理接入关系与不透明 binding 引用。
3. 实例 owner 持久保存 attachment 身份，生成 Pod 时附上此身份与网络配置，负责唯一 Pod 写入和提交重试；Network 不创建或修改 Pod。
4. `ConfirmAttachment(tenant_id,attachment_id,expected_version,workload_ref)` 持久登记以不可变 workload UID 为身份的确认请求并触发核验，不凭调用者声明直接记 attached。相同确认身份重放先于版本检查返回现有结果，不重复安排工作；不同 UID 冲突。Network 自己读取 Pod/Provider 事实，匹配 tenant/instance/attachment、cluster 与 UID 后从 reserved 推进 attached。
5. 实例提交成功但 Confirm 调用丢失：Network 可以依据已登记的 attachment 身份发现匹配对象并补全；出现多个/错误归属对象进入明确冲突，不能任选一个。Confirm/自动发现都不能把 releasing/released 改回 attached。

Prepare 的 request_key 使用创建幂等键的字符限制。released 后重放 Prepare 仍返回原 attachment 身份及当前 released 状态，不重新占用 slot，也不交付可再次提交的接入方案；新的接入意图使用新 key。
首次交付方案保存为不可变、版本化结果；只在该 attachment 仍可使用时返回，不能通过历史响应复活已释放的接入。
Attachment 的重放语义与资源 Create 的固定 201 快照语义分别定义，不共享一个会误复活接入的通用幂等 helper。

`pod_primary` 是版本化、受限的内部交付格式，只允许网络适配需要的目标 namespace、网络注解和 attachment 关联标签；不是任意 Pod spec patch。
序列化和 Provider 字符串只存在于 Network 的基础设施/传输适配与实例出站适配，不进入产品 API 或领域规则。
实例适配按闭合格式合并网络片段，不自行从产品 ID 拼 CR/NAD，不接受用户覆盖这些字段，也不允许该片段修改容器镜像、命令、权限或其他 owner 字段。
`PodPrimaryPlan` v1 是生成 DTO：`format_version=1,namespace,subnet_annotation,labels`。
`subnet_annotation` 是已持久映射的 `namespace/provider-subnet-name`；其唯一输出键为
`networking.kubercloud.com/subnet`。`labels` 封闭为 tenant/instance/attachment/submission/generation，
输出键分别为 `network.ani.io/{tenant-id,instance-id,attachment-id,submission-id,generation}`。
DTO 不含开放 map。实例 adapter 在 renderer 的最终 Deployment metadata 与 Pod template 写入关联，
校验 namespace、主网及全部关联字段；用户提供 kc 网络选择、Multus 网络、上述关联字段或旧 OVN 网络注解时拒绝。
实际 Pod 上由 kc controller 追加的 vnic/vnicip 选择必须沿 Pod UID 的 ownerReference 验证，不能当作用户输入接受。

Prepare 的规范化意图额外固定 `submission_id,generation,cluster_id,namespace`（以及可选显式 VPC 引用）；
`slot` 首片只允许 `primary`。实例 owner 先在 ANI 本地租户事务建立稳定 instance/operation/submission，
完整保存可恢复的容器意图，再进行 Prepare。相同接受键永久重放这组身份；实例 operation 不再是临时占位 ID。
网络方案、binding revision、部署发送标记、Deployment UID、Pod UID、pending Confirm/Release 与 finalization ID
均属于实例 owner 持久记录。该记录与实例状态在 ANI 本地事务中提交；没有跨服务 FK 或跨库事务。

`InstanceNetworkConsumer.GetSubmission` v1 是 Network 所需的消费者只读协议，由 ANI 实例 owner 实现。
查询固定 tenant/instance/submission/generation/attachment；可携带预期 finalization ID。
响应重复完整身份、cluster/namespace，返回 `open,closing,closed` 枚举、稳定 finalization ID、Pod UID 集合、
controller UID 集合及 closed_at。它不是由 Release 请求携带的布尔声明。
只有 owner 持久禁止未来发送、查明全部已发创建结果，并通过 UID 条件停止/删除 Deployment、ReplicaSet 和 Pod 后，
才能在本地事务写入 closed。未知发送即使 GET 404 仍为 closing；永久保留可恢复的占用。
Network 的后台查询也能发现 owner 已持久化但未成功送达的 Release，不依赖原请求进程。
接口不可用、身份不符、多 Pod、错 UID 或派生关系不符均保留占用；IAM S2S 仍按 ADR-0003 延期。
实例若尚无与目标网络相同的集群/namespace 放置能力，返回 `PLACEMENT_MISMATCH`，由实例适配完成该放置能力后接线，不自动改用默认网络。

### 7.2 释放与竞争

Attachment 状态为 `reserved,attached,releasing,released`，异常原因单独记录；前三者都阻止删除 Subnet。
不凭租约超时、用户不刷新或“暂时没看到 Pod”释放 reserved。

`ReleaseAttachment(tenant_id,attachment_id,expected_version,consumer_finalization_id)` 是幂等的释放请求。
首次 Release 校验 expected_version 并绑定 consumer_finalization_id；相同 finalization_id 重试在版本检查之前返回现有释放结果，不同身份冲突。
消费者先持久封闭本次提交尝试：禁止未来发送/重放，澄清所有已经发出但结果未知的创建请求，并停止可能再建该工作负载的控制器；存在在途/未知结果或可能再建时不得声明 finalization 完成。已创建的工作负载由实例 owner 删除。
Network 保存释放意图，核对消费者的持久封闭结果，自行确认对应工作负载不存在且 Provider 网卡/IP 关系已释放，才标记 released；一次 NotFound 不能替代提交封闭。
如果消费者崩溃，Network 保留释放记录继续观察；若无法确认是否仍可能提交，保留占用及原因，不能仅按 TTL 猜测安全。
这类未完成消费者协议由其操作恢复接口处理，不要求 Core 替 Network 更新状态。

没有工作负载创建成功的失败路径也使用同一协议：消费者封闭提交尝试，Network 核对无残留后释放。
released 后发现关联对象仅登记协议异常并阻止受影响资源被误判清理完成，按 owner 协议处理残留；不得自动复活 attachment 或由 Network 接管 Pod 写入。
NET-03 必须验证晚到提交、重复 Confirm/Release、跨租户引用、删除/Prepare 竞争和未使用 reserved 的恢复，不能只验证 happy path。

### 7.3 接入后台推进

Prepare、Confirm 和 Release 在各自 Network 本地事务内提交接入事实、next_check_at 和必要历史；提交后即使请求进程退出，工作也不会丢失。
worker 从 Attachment 持久记录领取到期核验，以数据库时钟维护该 attachment 的执行 lease/epoch；外部只读观察在事务外进行，回写必须同时匹配 tenant、version、epoch 与允许的状态迁移。
reserved 持续查找匹配消费者，attached 持续核验实际占用，releasing 重试提交封闭及残留释放核验；released 的墓碑核验继续发现迟到对象，不重新激活接入。
状态变化与历史、下一次核验时间同事务保存；两端重启或暂时失联时从持久记录恢复，不能依赖 Confirm/Release 重发、GET 副作用或内存定时器恢复工作。
NET-05A 对本段的调度与共享关系观察增量同样由[持续观察规格](cr-observation.md)约束，不改变本节的接入/封闭/释放协议。
接入核验不使用 VPC/Subnet 的 mutation operation，也不与其共用一个数据库长事务；涉及删除准入的写事务仍遵守第 5.4 节的父资源加锁顺序。
接入表通过 `(tenant_id,vpc_id,subnet_id)` 外键指向唯一父子关系，绑定通过
`(tenant_id,subnet_id,binding_id)` 外键指向同一 Subnet；接入历史与派生关系快照以同租户 Attachment 外键闭合。
Prepare 的 key/active-slot 数据库互斥位于 VPC、Subnet 行锁之后；Confirm、Release、claim、finish 均先锁 VPC，
再锁 Subnet，最后锁 Attachment。调度扫描可跨租户选到期项，之后所有实体读写均以 tenant_id 定界。
版本/lease owner/epoch/数据库时钟有效期共同栅栏回写。released 的迟到对象只登记协议异常并保护父清理，
不重新启用接入方案。Provider 的已见 UID/关联持久保存，避免 VNic 消失后遗留 VNicIP 被误当作无关。

## 8. Provider 契约与接线

仅实现本切片需要的 kc adapter，不建通用插件注册中心或多技术开关。
概念端口为：`EnsureVPC`、`EnsureSubnet`、`Observe`、`Delete`、`ResolvePodBinding`、`ObserveAttachment`；实际类型由 Network 拥有。
Ensure 对稳定映射和相同意图幂等；Observe 为只读；Delete 对已记录身份幂等。
返回的失败至少区分暂时不可用、尚未就绪、明确拒绝、资源冲突和结果未知，Provider reason 需脱敏归一化。

就绪必须关联所请求的配置版本和正确对象，不能以 HTTP 成功、CR 存在、没有 condition 或一次合法性校验代替。
Subnet 的父 VPC 必须显式映射：产品 `vpc_id` → kc `spec.gateway` 引用，产品 `gateway` → kc `spec.gatewayIP`。
不能因引用缺失而落入 Provider default VPC。命名/UID/集群关系由 adapter 保存，不公开给租户。
租户对应 namespace，首片 VPC/Subnet 仅允许相应租户 namespace 使用；实例 owner 不能通过自由注解选择其他租户子网。

kc 是外部团队依赖。使用中若发现契约不满足，在执行记录登记最小请求/结果、预期、版本和复现，交由对方处理；本仓库不承担其 controller 修复。
等待修复时 Network 保留可恢复状态，受影响的 Provider/数据面验收 `not_verified` 或据实际失败记 `fail`；纯业务与数据库工作可继续。
不声称默认 CNI、Multus 或 KubeVirt 接入已经通过。第一真实配置使用用户提供 VM 中的全新 kind，不做与旧 Kube-OVN 同节点共存。

### NET-01 的落地边界

NET-01 的具体 SQL 只建立 VPC 及其 operation/reconciliation/idempotency/provider binding/history 六张租户表；目标以 `vpc_id` 闭合。Subnet/Attachment 表及跨资源扩展由后续迁移加入。该历史切片的 `subnet_count` 为 0；NET-02 通过后续迁移、同快照子网计数和父子锁准入扩展它。检测到非预期 kc 子资源时，已受理的删除保持 blocked，不执行盲目级联。

内部契约见 [network.v1](../../api/network/v1/network.proto)。RPC enum 和字段校验已落地；JSON null、未知/已移除字段和 HTTP 状态码由 NET-04 的 REST 适配落实与验收。当前创建归因字段为未经验证的可选信息，不用于选择租户或鉴权。

实际 kc adapter 固定读取 `networking.kubercloud.com/v1`（基线见实施记录）：`spec.cidrBlock` 对应产品 CIDR，`ipVersion=IPv4`，`allowedNamespaces.from=Same`。以受理时的映射验证 namespace、name、管理者/租户/资源/binding 标签、UID 和不可变意图；不会 PATCH 认领外部对象或修改其路由。就绪要求正确对象、非删除中、正 generation、相等的 `status.observedGeneration`、Valid/Initialized/Ready 均 True 且已报告 router。固定 kc helper 未填 condition 的 observedGeneration，允许其为 0；非零且与对象版本矛盾时拒绝就绪。API 注释中的 Applied 在该版本条件常量中未实现，适配器不虚构这一信号。

对象被明确判定归属冲突时立即 degraded；单纯连接故障保留最近观测并在有效期之后 degraded。控制器未就绪或 Valid=False 本身不推导不可恢复业务失败；明确被 API 拒绝的创建（如 422）才进入 failed。真实 kc Ready 信号和 OVN 保证仍由 NET-05 验收，不能用受控 server 构造的状态代替。

T3 前额外持久化 `pending_action`。VPC POST 超时或结果未知后，只观察原映射；一次 NotFound 不会清除该标记或触发第二个 POST，创建继续 blocked，因此也不准入删除。若对象随后出现并通过归属校验，Network 记录原 UID 并继续。若进程恰好在标记提交后、真正发送 POST 前退出，无法与外部迟到 POST 区分，首版同样保持 blocked；不自动丢弃意图。需要后续以 Provider 的可查询请求结果/持久执行凭据安全解除这种不确定性，不在本包添加不安全的“强制成功”开关。

服务正常退出取消 Provider 调用后，worker 仍在独立、受 `request_timeout` 限制的 context 内尝试保存已返回的执行结果。完成事务继续核对原 lease/epoch、资源版本与观察 fence；不能借此续租或发送新的 Provider 请求。确定的发送前失败可以清除本次 pending；真正未知的发送结果仍保留，进程被强杀或 lease 已失效时也不推断未发送。此规则不追认旧记录中缺失的执行回执。

DELETE 每次重新检查归属、status 中子资源和实际 Subnet 引用列表，用 UID/resourceVersion Preconditions 和 Orphan 策略请求删除，不移除 finalizer。请求被接受仍保持 deleting；之后观察缺失才成功，未知 DELETE 可按原 UID 重查/重试。子资源列表与 VPC DELETE 并非跨对象事务；并发外部写入的 Kubernetes/kc 保证仍需 NET-05 验证，NET-02 将负责服务内父子锁定。namespace 为租户多个资源共享而保留，不作为删除 VPC 的级联目标。

已记录 UID 的对象若意外消失，资源 degraded 并报告 `PROVIDER_OBJECT_MISSING`，不会自动创建另一 UID；明确删除可以完成缺失对象清理。墓碑继续观察原映射，清理迟到的同一归属/UID 对象并报告原因；同名换 UID 只报告冲突、不误删。历史 succeeded operation 在这些观察中保持原终态与完成时间。

### 8.1 ANI 接口适配与后续 Console

- Gateway VPC/Subnet/operation 路由调用 Network gRPC client；移除这些路径的 LocalNetworkService、Network DB/Provider 装配；未知/失败不回退旧 owner。
- 实例 resolver 的网络查询与 Prepare/Confirm/Release 都指向 Network；保留实例自己的生命周期与 Pod 权限。
- renderer 消费上述受限接入方案，移除对应路径的 Kube-OVN 名称推导；未支持的 SG/LB/Route/Storage 关联明确拒绝，不写旧表补齐。
- 当前旧 `NetworkService` 包含多个网络产品；适配只依赖本片的小接口，不迫使新服务实现整个旧接口。
- Console 后续独立更新生成类型、创建参数、异步状态和轮询；不通过用户刷新触发执行。NET-04 本次只验收接口，前端保持 not_verified。
- ANI OpenAPI 的 owner/operation/authz registry 标注在接线包与该仓库生成门禁一起更新；本轮只定义责任，不复制正在重构的 IAM security metadata。

## 9. 可观测性与验收

沿用现有 Kratos 生命周期、日志、trace、metrics；业务增加 tenant/resource/operation/attachment/request/correlation 关联，未知身份明确标记，不输出凭证或完整 Provider 对象。
worker 指标包括待执行数量、最老任务年龄、重试/blocked、租约争抢、最近成功推进、观测过期和清理积压；资源/tenant ID 不作无界 metrics label。
readiness 至少覆盖 DB/schema、必需配置和 worker 已启动；Provider 暂时失联不伪装为可用资源，也不必阻断可持久受理与查询，单独报告依赖退化。
worker 异常退出必须体现为不健康，不能继续只有进程存活就 green。

| ID | 行为与证据要求 |
|---|---|
| V-01 | 固定生成无漂移、`make verify` 通过；Proto/SQL 生成、分层依赖与错误映射通过适用检查 |
| V-02 | 空 PostgreSQL migration 重放，受限运行角色、无 RLS；两个租户的 CRUD/operation/幂等/attachment/provider mapping 正负向通过 |
| V-03 | 跨租户复合 FK 写入失败；故意漏 tenant 谓词的查询变异使测试失败；不以 superuser 行为证明运行角色安全 |
| V-04 | 同键并发单受理、同键异意图 409、不同租户同键独立、删除后重放不重建、关联字段不覆盖 |
| V-05 | 同 VPC 并发重叠 CIDR 仅一个成功，跨 VPC 同 CIDR 合法；错误父 VPC/网关及父未就绪拒绝 |
| V-06 | T1 后停止 Gateway，Network 自己完成；Core/Gateway 无 Network 表及 VPC/Subnet CR 写权限仍闭环，实例 Pod 权限保留 |
| V-07 | T1 后、Provider 成功但 T4 前、删除未确认时终止 Network；重启无重复身份、无丢任务、无伪终态 |
| V-08 | 多副本 lease/version/fencing 竞争，旧回写拒绝；验证外部迟到请求不会破坏当前意图，不能只测 mutex |
| V-09 | 停止相关后台观察/校验/执行后反复 GET/LIST/GetOperation，DB version 与 Provider 调用计数不变，证明查询纯读；另测只暂停状态应用、保留 Watch 健康时，未应用事实不能续鲜，stale 可见且接入拒绝；恢复应用后才推进 |
| V-10 | Prepare/删除互斥，提交后 Confirm 丢失可恢复；未封闭的 reserved 不被 TTL 释放；确认 Pod/网卡释放后才可删子网 |
| V-11 | Provider 失联/明确拒绝/未知结果分别呈现；恢复后继续；对象归属不匹配不认领、不误删；删除不以业务软标记代替 |
| V-12 | 新 kind 中普通容器同子网同节点及跨节点连通、同 VPC 跨子网连通、不同 VPC/跨租户隔离；重叠 CIDR 使用独立带身份响应的端点验证，避免 ping 自己造成假通过 |
| V-13 | 本次只验收 Gateway 接口创建、分页、状态及稳定幂等键；无查询时仍推进；无旧表或旧 Provider fallback。Console 后续独立验收 |
| V-14 | 后续 VM 单独验证实际 KubeVirt 网络路径与网关/地址行为，不从普通 Pod 通过推导 VM 通过 |
| V-15 | 后续 IAM 验证调用身份、目标租户与委托语义；本期未经验证测试输入不可被作为此项 pass |
| V-16 | NET-05A Watch、快照与时效故障，断言见[观察验收合同](cr-observation.md#8-验收合同) |
| V-17 | NET-05A 持久通知竞争、多副本与公平性，同上 |
| V-18 | NET-05A 共享关系索引、容量对照与兼容，同上 |
| V-19 | NET-05A 精确新版本真实观察及普通容器复验，同上 |

每条记录固定源身份、环境、实际命令、结果及限制，使用 `pass / fail / not_verified`。
本地 fake/provider 合同测试只证明对应层；真实 PostgreSQL、真实 kc、容器、VM、IAM 分别记证据。
Kind 多节点跨节点测试是多个 kind node 的验证，不外推物理多机、生产高可用或性能结果。

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
