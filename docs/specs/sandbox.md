# Sandbox 管理与生命周期规格

本规格是 Sandbox 的正式业务入口，职责边界遵循[全域原则](res-responsibility-and-simplification.md)。实现顺序、历史来源与未来切片见[计划](../plans/sandbox-runtime-design-20261025.md)；实际代码、验证、部署和已知偏差仅见[执行状态](../execution/status.md)。目标合同的修订不代表代码已经同步整改。

## 1. 管理对象与事实权威

Sandbox 拥有自己的业务身份、意图/快照、操作、在途 mutation、工作 Pod/PVC、适用直接声明、数据面目标及租户开通职责。Kubernetes/运行时负责原生执行，GC/CSI 负责内部回收；不把其他实例当父资源，不接管相邻服务的配额或生命周期。

当前动作只读取其必要身份与公开结果：原 Pod 的归属与状态、原 PVC 的存在和自身 phase、真实业务引用及未知提交。PV/CSI 等底层诊断不成为另一层运行或删除裁决，不扫描全群补找物理绑定，不改变第三方 finalizer/reclaimPolicy。权限、原 UID、内容、并发和在途输出责任仍须完整保留。

## 2. 产品范围与兼容目标

### 2.1 一期范围

一期只实现 Kata CPU Sandbox 的持久生命周期和专属工作盘：

- 创建、观察、暂停、恢复、删除；
- 持久化 Sandbox 意图、冻结资源、Provider 引用、operation、幂等和观察状态；
- 使用集群中受控的 Kata RuntimeClass；
- 专属 Workspace PVC，明确 StorageClass、容量和回收策略；
- 可恢复 worker、持久化单步 Provider 写入意图和回执；
- 面向本组件 file/exec/port adapter 的目标校验合同。

文件、Exec/code-run、端口、token、egress、checkpoint 都属于 Sandbox 的完整目标能力，但分别作为独立 action 设计和验收；创建、暂停、恢复、删除等生命周期接口不自动授予这些能力。对应门禁未通过时，接口必须明确返回 `UNSUPPORTED` 或 `FAILED_PRECONDITION`，不能静默成功或伪造已生效。

交付阶段与未来能力按[实施计划](../plans/sandbox-runtime-design-20261025.md)安排；规格描述目标合同，不表示全部动作已经实现或开放。

### 2.2 目标架构

```text
Governance BFF
  -> /api/v1/sandbox/...
  -> Resource sandbox.v1 RPC (TLS/mTLS + exact allowlist)
       internal/server/sandbox
  -> internal/service/sandbox
       request validation / operation admission
  -> internal/biz/sandbox
       intent / frozen snapshot / state machine / ports
  -> internal/data/sandbox
       PostgreSQL repository + worker lease
       Kubernetes Provider + file/exec/port/checkpoint adapters
  -> Deployment/Pod + Kata RuntimeClass + Workspace PVC + optional Service/NetworkPolicy
```

Gov 只负责入口治理和协议适配，不承载 Sandbox 状态机；Resource 的业务事实源是 PostgreSQL，Kubernetes adapter 是 Provider 事实源，Sandbox 自己负责数据面 action。Sandbox 不新增 Gov client、Inference client、SG client、Network client 或 `ani-workload-runtime-service`。

### 2.2.1 一期装配与迁移合同

P0～P2 的 Resource 纵切包含：`api/sandbox/v1` 自有 RPC、Gov 精确方法白名单、`SANDBOX` compute usage、`internal/biz/service/data/server` Sandbox 分层、PostgreSQL schema/operation/idempotency/lease、Kubernetes PVC/Pod Provider，以及 Kata CPU 的 create/observe/pause/resume/delete worker。Sandbox 与其他 Resource 模块共用主入口，不需要 `ANI_SANDBOX_CONFIG_FILE` 或专用 kubeconfig。组合根使用 Compute 的受管数据库和 cluster、Resource Kubernetes 配置及固定标准租户 Namespace；缺少共享依赖时，Sandbox RPC 返回依赖不可用，不影响其他 Resource 模块启动。

创建的具体来源：

- **算力和租户命名空间：** 通过本进程 Compute 目录解析 `SANDBOX` 用途。租户空间开通流程按 标准 Namespace 合同和受信租户 UUID 准备命名空间，不依赖创建 VPC。Sandbox 解析规格时只读取并核验其名称、UID、标签和删除状态，不创建或接管命名空间；规格冻结 UID，受理和 worker 不因命名空间消失而重新创建它。开通链路及上线状态见[执行状态](../execution/status.md)。
- **镜像：** 创建时可以使用 `image_id` 和 `image_scope`（`tenant` 或 `platform`），也可以填写已登记的租户或平台镜像 `image_reference`；两种输入不能同时提供。本进程 Image 目录按租户授权解析登记镜像，冻结不可变 digest 引用，并在受理新命令前再次校验。使用 ID 时还冻结 ID 和 scope；未登记镜像不能直接创建。
- **工作盘：** 请求可提供正数 `workspace_size`；省略时当前采用旧 Sandbox 工作盘的 `5Gi` 默认值。StorageClass 由 Storage 目录选择唯一平台默认类，且实时对象必须未删除、明确使用 `Delete` 回收策略；没有唯一、安全的默认类时创建失败，不自行猜测类名。
- **运行时：** Resource 使用共享 Kubernetes 客户端按 `sandbox-kata`、`kata` 的顺序读取 RuntimeClass，仅在前一项不存在时尝试下一项；选择项必须使用 `kata` 或 `kata-` 开头的 handler，且不得正在删除。不存在、权限不足或对象不符合要求时拒绝新的创建，不回退到普通容器。解析规格时冻结选中的名称，新建受理时独立核对冻结的名称仍在许可名单且有效，不因新出现优先级更高的 RuntimeClass 改写该选择；已受理命令按持久快照重放。当前新创建接口不提供由请求任意选择 RuntimeClass 的能力。目标集群运行时和 StorageClass 的实际可用性仍需真实环境验证。

这些内容定义接线和目标约束，实际实现、发布与验收只按执行状态记录。镜像权限、拉取凭据、默认存储类、租户命名空间和 Kata RuntimeClass 均属于实际验收前置；详见[执行状态](../execution/status.md)。

从旧版本升级必须先停止所有旧 Sandbox worker（包括独立候选），再以 owner 应用对应当前版本的增量迁移、配置合法直接对象权限并启动匹配 worker；禁止旧 worker 与新 journal 写入者共用同库并行运行。旧版本不理解 pending 栅栏，不能直接滚动混用；回退前必须独立评估未决写入和在途请求，不能丢弃 journal。

Sandbox schema 由独立的 `-sandbox-migrate` 运维模式创建，使用 `ANI_SANDBOX_OWNER_DSN_FILE` 与 `ANI_SANDBOX_RUNTIME_ROLE`。`-instance-migrate` 不再隐式创建 Sandbox 表；这样 Instance 和 Sandbox 可以分别迁移、启停和回滚，运行时仍只使用受限角色。

二期 P3～P5 的 file、Exec/code-run、端口、token、egress 和 checkpoint/restore/clone RPC 分别随对应切片引入，不预留默认开放的空实现。实现及验收状态仅见[执行状态](../execution/status.md)。

### 2.2.2 共享租户空间开通目标

租户由平台管理员在 Gov 创建，Sandbox 不创建租户、不依赖父 Instance。Gov 在租户创建事务中保存稳定的空间开通任务身份与待处理状态，提交后通过现有受信服务间入口投递；投递失败或进程重启后使用同一任务身份恢复。应能区分开通中、就绪和需要处理的失败，不能以租户行已创建冒充 Kubernetes 空间已就绪。

Resource 在现有进程内按受信租户 UUID 和固定标准名称准备命名空间及该命名空间内 Sandbox 必需的 Resource 身份权限。命名空间、角色及绑定须核验归属、身份和内容；同名异主对象不得接管。具备 RBAC 创建能力时必须同时由准入约束限定目标、授权对象和规则，并保护命名空间代际，不能依靠应用代码自行声称最小权限。平台部署须先核验策略与既有对象再开放此能力；业务用户的 Sandbox 动作与套餐仍由 Gov 授权。

开通、租户停用/清理和删除需要持久化的跨服务栅栏：不能因 RPC 超时、单次未查到对象或租约到期就认定 Kubernetes 写入未发生。关闭后应阻止迟到开通重新创建或授权，未确认的写入保持待处理，只有可信的完成或关闭回执才能推进对应状态。此目标不新增独立 Sandbox 部署或前端创建参数；实现、迁移、部署与验收状态只见[执行状态](../execution/status.md)。

### 2.3 旧 ANI 行为到新版实现的映射

| 旧行为要求 | 新版保留的语义 | 新版实现要求 | 开放门禁 |
|---|---|---|---|
| Kata create/ready/pause/resume/delete | 生命周期动作和状态可观察 | Sandbox 持久意图 + Kubernetes Provider Apply/Observe；不使用本地 map | P2 真实集群 |
| 重启后查询任务、重复请求不重复创建 | 幂等和恢复 | DB operation/idempotency + lease + Provider UID/名称观察 | P1 fake，P2 live |
| `/workspace` 专属 PVC | 工作盘隔离、pause 后保留 | 冻结原 PVC 与 StorageClass；删除完成自身管理回执，不等待 PV/CSI 清场 | P2 |
| 路径、symlink、hard-link、rename-out 防护 | 工作区边界和文件安全性质 | Sandbox 自己的 file adapter 做 fd/容器边界测试，不能复制旧脚本 | P3 |
| Python/JavaScript code-run、输出上限 | 明确语言、超时、工作目录、结果状态 | Sandbox 自己的 exec adapter + operation；不依赖 Gateway 或其他服务 | P3 |
| 预览端口 | 受控 endpoint、TTL、撤销和清理 | Sandbox 自己创建受控 Service/endpoint；NodePort 只能实验开关 | P4 |
| HMAC token 绑定 tenant/instance/scope/expiry/jti | scope 和目标绑定 | Sandbox 自己的 signer 签发，稳定密钥和撤销/版本检查 | P3 |
| CSI filesystem checkpoint | 文件系统 crash-consistent 快照 | 独立 action，记录 Snapshot/PVC UID；不承诺内存/CRIU | P5 |
| 30 秒过期扫描和 session/idle 判定 | 到期语义 | 持久 `expires_at`/`last_activity_at` + lease/条件更新 worker | P4 |
| egress allowlist | 策略意图 | Sandbox 自己渲染和观察 NetworkPolicy；回显不算成功 | P4 |
| `/api/v1/sandbox/...` | Gov 的独立 Sandbox 入口 | 以租户和 Sandbox ID 确定范围，不依赖其他 Instance | P0 |

## 3. 资源解析与推理资源语义对齐

### 3.1 资源来源

Sandbox 创建请求必须携带 `preset_id`。Sandbox 在受理新请求时通过同一 `ani-resource-service` 进程内的 `SelectionResolver` port 调用算力目录的只读解析，不发起跨服务 RPC：

```text
tenant_id + preset_id + usage=SANDBOX
        -> ComputeSelection
        -> Sandbox frozen resource snapshot
        -> Runtime render
```

请求不能同时把任意 `cpu`/`memory` 数量作为另一套权威来源。若产品需要“直接输入资源”模式，必须另立版本化合同和准入规则，不能在 Sandbox 内绕过 compute 目录。

与当前代码的对应关系如下：

```text
internal/biz/compute.NewResolver(presetRepository, clusterID)
        -> *compute.Resolver
        -> Resolve(ctx, tenantID, presetID, usage)
        -> compute.Selection{CPUMillis, MemoryMiB, GPU, ...}
        -> Sandbox local selection + canonical selection_digest
```

`SelectionResolver` 是 Sandbox 内部 port，不是新服务；进程内 `*compute.Resolver` 直接实现该 port，返回 `compute.Selection`。入站 service adapter 按 `compute-selection-v1` 生成和校验 canonical digest，再冻结 Sandbox spec；protobuf 不进入 `internal/biz/sandbox`。`internal/service/compute` 暴露的 gRPC 仍是现有 Gov 入口，Sandbox 不通过该 gRPC 回调自己。可信 scope 来自 Gov 入站身份链，usage 按[算力目录合同](../specs/compute-presets.md)校验。


### 3.2 `SANDBOX` 用途

Sandbox 固定使用[算力目录合同](../specs/compute-presets.md)登记的 `SANDBOX` usage，一期只接受 CPU 选择；不得发送 `UNSPECIFIED`、静默复用 `CONTAINER_INSTANCE` 或把未知目标默认当作容器。候选资格、停用和绑定规则由该合同定义。

### 3.3 ComputeSelection 到 Runtime 资源

`ComputeSelection` 是一次只读候选，不创建资源、不占额、不产生受理 ACK。Resource 必须：

1. 校验 `schema_version=1`、`preset_id`、`compute_type`、正数 `cpu_millis`/`memory_mib` 和 `selection_digest`；
2. 把 `preset_id`、code、name、compute_type、CPU、内存、GPU（一期必须为 null）和 digest 原样写入 Sandbox snapshot；
3. 通过本地 canonical digest 校验候选，不能只相信展示字段；
4. 将 CPU 转为 `<cpu_millis>m`，内存转为 `<memory_mib>Mi`；
5. 一期渲染 `requests.cpu/memory` 与 `limits.cpu/memory` 为相同值，避免出现“目录显示值”和实际调度值不一致；
6. 不用 GPU 显存、profile 展示字段或推理模型参数替代主机内存；GPU 未来扩展必须有独立的 ACC/计费/调度合同。

例如 `cpu_millis=1500`、`memory_mib=2048` 只生成：

```yaml
resources:
  requests:
    cpu: "1500m"
    memory: "2048Mi"
  limits:
    cpu: "1500m"
    memory: "2048Mi"
```

渲染器不接受前端传入的原始 Pod resources，也不从默认值补齐缺失 CPU/内存。解析失败、digest 不匹配、usage 不合法、preset 不存在或已禁用都必须返回明确错误，不能回退默认规格。

### 3.4 资源语义的本地实现

推理服务的 `ResourceSpec` 只作为历史参考：CPU/内存使用 Kubernetes Quantity，managed 资源要求 requests==limits，GPU 是独立资源。Sandbox 在本地实现同样的语义，但所有类型、校验、快照和渲染代码都属于本仓库：

- 不导入推理 protobuf，不依赖推理服务运行、数据库或部署状态；
- `cpu_millis`、`memory_mib` 和 `selection_digest` 由 res 内部 `SelectionResolver` 提供；
- Sandbox snapshot 直接转换为自身 Kubernetes adapter 所需的 Quantity 或 typed fields；
- 模型、引擎、拓扑、副本、推理 API 和推理调度均不进入 Sandbox。

## 4. 对外调用与内部边界

### 4.1 Gov 入站与 Resource Sandbox RPC

公网入口由 Gov 提供，直接创建和管理 Sandbox：

```text
/api/v1/sandbox
/api/v1/sandbox/{sandbox_id}
/api/v1/sandbox/operations/{operation_id}
```

Gov 完成公网认证、产品授权、租户映射和 DTO 校验，再通过 Resource 已登记的 `sandbox.v1.SandboxService` RPC 传入 `tenant_id`、`actor` 和业务请求；`request_id` 由受信 metadata/context 校验并用于请求链路关联。Resource 的 `internal/server` 只做 TLS/mTLS、精确方法 allowlist、metadata 校验、deadline 和传输适配，不承载 Sandbox 状态机。具体 HTTP DTO、版本号和 RPC 名称在 P0 冻结。

| 方法 | 语义 | 幂等/权限 |
|---|---|---|
| `ResolveCreateSpec` | 校验 Gov 传入的 tenant，以及 Sandbox 自身的 image digest、preset、工作盘、策略和运行时参数，生成未受理的候选规格 | 只读核验已开通的共享租户 Namespace；不创建 Sandbox Pod/PVC、不占额 |
| `AcceptCreate` | 持久化 Sandbox 意图、冻结 snapshot、创建 operation | 原 `idempotency_key`；新请求才解析和冻结 |
| `GetSandbox`/`ListSandboxes` | 读取 DB 快照和观察状态 | 租户 + Sandbox ID 范围查询 |
| `GetOperation` | 读取持久 operation | 租户 + Sandbox ID 范围查询 |
| `Pause`/`Resume`/`Delete` | 产生可恢复 action | 租户 + Sandbox ID + 原键 + expected_version；不以请求返回代表 Provider 完成 |
| `ResolveDataPlaneTarget` | 向本组件自身的 file/exec/port adapter 返回当前有效 target、版本和 Pod UID | 进程内 port；不依赖 SG 或其他服务 |

Resource RPC 的身份由本服务的 transport middleware 注入并校验；Sandbox 不调用认证、授权或租户服务。请求中的 tenant/actor/request_id 只能来自 Gov 受信委托和本服务认可的 transport context，不能由业务字段覆盖。Gov 路径不经 Higress；Sandbox RPC 必须登记在 Resource 的 Gov 调用清单中。

### 4.1.1 创建请求和响应的最小字段

最终 HTTP/Proto 字段在 P0 冻结，但语义必须保持以下边界：

| 对象 | 必填字段 | 规则 |
|---|---|---|
| `ResolveCreateSpecRequest` | `tenant_id`、`preset_id`、`image_id` 和 `image_scope` 或 `image_reference`；`workspace_size` 可选 | tenant 来自 Gov 受信委托；Resource 不查询其他 Instance。镜像须先在对应租户或平台 Image 目录登记，经本地 Image 能力授权并解析为不可变 digest；`image_reference` 不可与 ID 混用。工作盘大小须为正数，省略时采用当前默认值。不接受原始 Pod YAML、CPU/内存 Quantity 或任意 Namespace；当前不支持通过请求选择 RuntimeClass |
| `CreateSpec` | tenant、Sandbox、image digest、`ComputeSelection`、workspace、egress、runtime、timeout、spec digest | 未受理候选；解析只读核验共享租户 Namespace，不创建 Sandbox Pod/PVC、不占额、不产生长期 token |
| `AcceptCreateRequest` | `create_spec`、`spec_digest`、`idempotency_key`、`expected_version`（如适用） | `request_digest` 必须是 `sandbox-create-v1` 对 tenant/resource/operation/空历史 instance 字段/spec 的 SHA-256；校验冻结规格摘要、原始请求摘要与候选一致；新键才冻结和持久化，原键返回原 operation/快照 |
| `Sandbox` | `sandbox_id`、tenant、generation、desired/observed、snapshot、operation 状态、Provider refs | 历史 `instance_id` 仅为兼容保留，不用于新资源归属；所有读取带显式 tenant 条件；Provider 状态和业务状态分开表达 |
| Action 请求 | `sandbox_id`、`idempotency_key`、`expected_version` | Resource 按 `sandbox-action-v1` 对空历史 instance 字段/expected_version/action 计算并持久化 request digest；同键不同请求返回冲突。Pause/Resume/Delete 都是异步 action；调用返回 accepted operation，不代表 Runtime 已完成 |
| `ResolveDataPlaneTarget` | tenant、sandbox、generation、action/scope | 只给本组件 adapter 当前有效目标、Pod UID、容器和短时授权信息；pause/delete/version 变化后旧目标失效 |

业务错误至少区分：`INVALID_ARGUMENT`（结构/单位/digest）、`UNAUTHENTICATED`（transport identity/metadata）、`PERMISSION_DENIED`（租户或 action）、`NOT_FOUND`（资源/镜像/preset）、`FAILED_PRECONDITION`（能力或状态不满足）、`ALREADY_EXISTS`（幂等冲突）、`UNAVAILABLE`/`DEADLINE_EXCEEDED`（基础设施故障）、`UNKNOWN`（Provider 结果不确定）。不能把 Provider 超时直接改写成成功或已删除。

### 4.1.2 摘要计算

Sandbox 不接受任意字符串作为 `request_digest`。新创建和动作的摘要使用原有版本和字段格式，但历史 `instance_id` 字段固定为空；请求按租户、Sandbox ID、operation、原始创建意图和冻结规格校验，同一幂等键的不同请求返回冲突。历史记录的摘要和引用不被改写：已受理的旧创建仅在原命令完全匹配时重放；旧动作按保存的历史摘要验证。尚未被 Resource 受理的旧父实例创建不能当作成功或自动改写，应保留待确认记录并单独核查。旧客户端带非空 `instance_id` 的普通查询和动作必须拒绝，避免按旧父资源授权的客户端误读整个租户的列表。

列表采用一基页码，默认 `page=1/pageSize=20`，pageSize 只允许 1–100；HTTP 显式 0、重复或非规范数字为 400，不做静默夹取。同次 count/items 必须使用一致数据库快照，按 created_at DESC、sandbox_id DESC 排序。已完成的 operation 结果保持 succeeded/superseded，后续健康漂移只更新资源观察。公共错误保持稳定 reason：缺失 404，幂等/版本/前置条件冲突 409，依赖不可用 503；RPC 原因需映射到同样语义。

### 4.2 Sandbox -> Kubernetes Provider

Kubernetes adapter 只接受 Sandbox 已冻结的业务 snapshot 和 opaque refs：

```text
PrepareMutation(work lease, mutation ID/kind/operation/version/target UID/attempt) -> durable pending
Apply(snapshot, one mutation) -> applied/rejected/not_sent/unknown receipt + opaque refs
Observe(refs) -> provider facts / UNKNOWN
能力范围按实际已注册动作及领域配置确定，不为此新建通用能力接口
```

每次 Apply 只允许一个子操作：create workspace、create pod、delete pod 或 delete workspace。worker 必须先持久化 pending，再调用 Provider，随后保存匹配回执。超时或传输结果不确定时保留 pending；创建对象暂时不存在不能关闭未知创建。只能通过归属和规格匹配的对象观察恢复该创建，再按最新 desired intent 回收。迟到回执只能合并匹配 journal 的身份事实，不能覆盖新意图、快照、operation 或接管者 lease。

UID 受限删除可重试，但每次发送前必须递增并持久化 `Attempt`，同时保留 `PreviousUncertain`。回执必须匹配当前完整 attempt；旧 attempt 的回执不能清除后继屏障。即使当前 attempt 明确未发送或被拒绝，只要仍有前次不确定性，就继续保留 pending；当前匹配的 Applied 或原目标 Gone 观察才可解除。已被清除或推进的 journal 不得由旧 Work 复活。成功 Apply 也不得复用写入前的 Ready/Gone 完成当前 operation：写入后进入等待观察，下一次 Observe 确认当前目标才可成功。

Provider 在构造时绑定受管配置的非零规范 ClusterID，每次 Observe/Apply 必须先与冻结 snapshot 的 ClusterID 相等；不匹配时返回 `SANDBOX_PROVIDER_CLUSTER_MISMATCH`，不发任何 Kubernetes 请求，不生成回收回执。kubeconfig 与逻辑 ClusterID 的物理映射仍由受信部署配置负责；此检查不提供物理集群指纹认证，也不宣称已验证误配 endpoint 的所有情形。

Provider adapter 不重新解析 preset、不读取其他服务数据库、不接受用户 Pod YAML、不决定外部租户配额。`Apply` 的成功只表示 Provider mutation receipt 已取得；最终状态按当前动作需要的原 Pod/PVC 身份及原生公开状态登记；自己实际创建的 Service/NetworkPolicy 等对象按其动作合同处理，不扩成无关诊断合取。

普通生命周期只需要其直接管理的 Pod/PVC、标准 Namespace 与适用镜像 Secret 权限。租户开通者与只读消费者的 Namespace 权限分开；首次新建使用 StorageClass/RuntimeClass 的必要配置读取。不得为运行状态或删除证明要求 PV/CSI 读取/写入；部署权限按实际合法用途配置，不借文档新增授权。

### 4.3 本组件内部 owner

| 组件 | 负责 | 不负责 |
|---|---|---|
| `internal/service/sandbox` | 入站请求适配、字段校验、use case 调用和响应映射 | 业务规则、Kubernetes、数据库细节 |
| `internal/biz/sandbox` | 租户边界、compute 解析端口、冻结 snapshot、状态机、operation、token/file/exec/port 端口 | Kratos、protobuf、HTTP、Kubernetes、SQL |
| `internal/data/sandbox` | PostgreSQL facts、worker lease、Kubernetes Apply/Observe、workspace、file/exec/port/checkpoint、token signer | 对外 API、业务授权决策 |
| `internal/server` | Sandbox HTTP/gRPC、transport identity、deadline、日志和健康检查 | Sandbox 状态机和 Provider 规则 |
| `cmd/ani-resource-service` | 装配本组件所有依赖和 worker | 跨服务编排或远程业务调用 |

### 4.4 res 仓库内的实现布局

```text
cmd/ani-resource-service/
  sandbox composition root
internal/biz/sandbox/
  types.go          # Sandbox、Snapshot、Operation、Provider facts 与 ports
  worker.go         # journal、恢复、单步生命周期推进
internal/service/sandbox/
  service.go        # transport DTO -> use case -> response
internal/data/sandbox/
  postgres.go       # facts、operation、idempotency、lease、cleanup receipt
  provider.go       # Kata/原 PVC/Pod 的 Apply/Observe
  # 以下 adapter 随二期切片引入：
  filesystem.go     # /workspace 文件边界和 upload action
  exec.go           # Pod exec/command adapter 和输出限制
  endpoint.go       # Service/endpoint/TTL 以及撤销
  token.go          # 稳定密钥 signer 和 claims 校验
internal/server/
  sandbox_identity.go / sandbox_worker.go
  # 入站 gRPC 注册复用 governance.go，公共 HTTP 属于 Gov
```

`internal/biz/sandbox` 不得导入 Kratos、protobuf、Kubernetes client、SQL driver 或其他服务包；`internal/service/sandbox` 不得把业务规则写进 handler；`internal/data/sandbox` 不得从请求重新解释资源或绕过 use case；`internal/server/governance.go` 与 `sandbox_identity.go` 只处理 Gov 入站传输合同，不把 Gov DTO 带入 biz；`cmd` 只负责构造依赖、启动 worker 和关闭资源。这样 Sandbox 可以在同一 res 进程内独立启停、测试和恢复，而不会形成跨服务业务耦合。

## 5. Sandbox 冻结快照

下表区分一期与后续目标；创建输入保持冻结，观察、引用和操作事实独立更新。二期专属字段随各自切片增加，不能据此推断一期已提供该能力：

| 类别 | 字段 |
|---|---|
| 归属和版本 | `tenant_id`、`sandbox_id`、`generation`、`operation_id`、`idempotency_key` 摘要、`request_digest`；`request_id` 保留在受信请求上下文 |
| 算力 | `preset_id`、preset code/name、`compute_type`、`cpu_millis`、`memory_mib`、GPU selection（一期 null）、`selection_digest` |
| 镜像和运行时 | image ID/digest、允许的 RuntimeClass、cluster、namespace、service account、Provider spec digest |
| 工作盘 | volume ID、PVC name/UID、容量、StorageClass、volume mode、reclaim policy、mount path `/workspace` |
| 生命周期 | 一期：desired/observed state、created/deleted timestamps；P4：`expires_at`、`last_activity_at`、session/idle timeout、`on_timeout` |
| 网络和数据面 | egress policy 摘要、Sandbox 自己创建的 NetworkPolicy/Service refs、端口声明、data-plane target version；不保存长期 token |
| 安全和环境 | 受控 Secret/env 引用、非特权安全上下文、允许 action/scope、资源和操作版本 |
| Provider 事实 | Deployment/Pod/PVC/Service/Network opaque refs、UID、resourceVersion、phase、Ready、last observed error |
| 回收 | delete intent、各子资源清理状态、snapshot/volume/network cleanup receipt、unknown/retry reason |

快照 digest 应覆盖所有影响执行的字段。重试、恢复、Start/Stop 或 Resume 不重新读取当前 preset、镜像别名、Namespace 或网络默认值。

### 5.1 工作盘与 PVC 的关系

业务 Workspace 通过冻结的原 PVC name/UID 绑定，PVC 的 spec/status 是请求和公开准备状态的权威；PV/CSI/后端是组件内部实现，不成为 Sandbox 直接管理对象。

创建保存工作盘参数与明确原生输出。Pause 停止已知原 Pod，保留同一 PVC；完成取决于原运行对象退出和未知写收敛，不要求保留盘 Bound。Resume 使用原 PVC，不能在原 UID 丢失后创建空盘替代；Pending/WFFC 等真实准备状态如实展示，必要的消费者创建按本动作合同推进。

运行投影消费原 Pod Ready 和原 PVC 自身的必要状态，不以底层 PV 存在、回收策略、额外诊断读取成功或物理链路健康重新裁决。PVC Lost/Missing/Deleting、真实 UID/归属冲突和读取失败仍按其实际影响处理，不能由简化忽略；未读取的诊断不覆盖已登记事实。

Delete 封闭新提交，查明未知 mutation，以 UID 条件正常删除自己负责的 Pod、原 PVC 及实际管理的其他声明，完成自身引用/输出责任后收尾。请求接受不等于对象退出，terminating 仍存在，403/超时不当作 NotFound；完成边界不等待 PV/CSI/物理擦除，不补扫全部 PV 来证明无残留。

已登记 PVC 身份与必要历史清理责任保留；历史 PV/VolumeSnapshot 标识可按实际取得的诊断事实保存，但不要求持续读取、更新或作为收尾凭据。未来 restore/clone 如生成新业务 PVC，须先持久交接具名输出和责任，再结束结果载体；未知输出不能当零产物，具体能力按对应新增合同开放。

### 5.2 持久化对象

数据库表名和迁移编号在实现阶段确定，但至少要有以下逻辑对象，且每个对象都带 `tenant_id` 或通过受保护的 Sandbox 外键归属租户：

| 对象 | 关键字段 | 作用 |
|---|---|---|
| Sandbox intent | ID、tenant、generation、desired state、create request digest、frozen snapshot、created/deleted time | 保存用户意图和不可变执行输入；不建立 Instance 外键 |
| Sandbox observation | sandbox ID、Provider refs/UID/resourceVersion、phase、ready、runtime/storage/network facts、last error | 保存 Provider 观察，不由请求线程伪造 |
| Operation | operation ID、action、state（queued/running/retrying/succeeded/unknown/superseded）、attempt、lease、deadline、error detail、started/finished time | 支撑异步动作、重试、查询和重启恢复；新动作替代仍在执行的旧动作时，旧 operation 标记 `superseded` |
| Idempotency record | tenant、scope/action、key、request digest、operation ID、response digest、expiry | 原键返回原结果；同键不同请求返回冲突 |
| Workspace binding | volume ID、PVC name/UID、StorageClass、size、reclaim、mount、cleanup receipt | 绑定工作盘和自身管理完成回执 |
| Runtime reference | provider kind/name/UID、cluster、namespace、resourceVersion、owner generation | 通过 Observe 恢复资源身份，避免重复 create |
| Provider mutation journal | mutation ID、单一 kind、operation/version、目标 UID、pending 与回执 | 写前持久化；unknown 保留屏障，防止迟到创建泄漏 |
| Expiry lease（P4） | sandbox、expires_at、last_activity_at、on_timeout、lease owner/version、last decision | 支撑过期 worker 的抢占和幂等处理 |

一期 Provider 资源名称使用受控前缀加 Sandbox UUID（`workspace-<id>`、`sandbox-<id>`），不直接拼接用户名称；Sandbox version/generation 和 Provider UID 仍写入事实，用于动作隔离和迟到对象校验。二期 clone/restore 若引入新 generation，必须为新资源分配新名称。删除和回收 receipt 要保留足够时间支持审计和迟到对象清理。

## 6. 状态、持久化与恢复

### 6.1 状态建议

业务状态和 Provider 观察状态分开保存：

```text
PENDING -> CREATING -> READY -> PAUSED -> READY
                    └-> FAILED
PENDING/CREATING -> PAUSED（无未决 mutation、运行负载已停止；工作盘允许 Pending）
READY/PAUSED -> DELETING -> DELETED
任意非终态 -> UNKNOWN（Provider 或 worker 结果不确定）
```

`expired` 是业务结果或 reason，不替代 Provider 状态；到期动作（pause/kill）完成并核对资源后，才更新最终业务状态。Provider 2xx 不等于已 Ready，删除接受不等于自己直接管理的原对象已经退出。

### 6.2 DB 和 worker

- PostgreSQL 保存 intent、snapshot、desired/observed、operation、idempotency、lease、mutation journal 和 cleanup receipt；expiry 在 P4 引入；
- worker 通过租户范围查询非终态记录，以 lease/条件更新抢占，不使用进程内扫描状态；
- PVC、Pod、Deployment、Service、NetworkPolicy、VolumeSnapshot 的真实状态只来自 Kubernetes Provider 的查询结果，并按 name/UID/resourceVersion 恢复；不能从本地文件、内存对象或上一次请求结果推断 `Bound`、`Ready`、`Deleted` 或 snapshot 完成；
- Provider unknown 时保留原 refs、请求摘要和最后错误，下一次 Observe 先恢复身份，再决定重试或人工介入；
- 同一幂等元组 `tenant_id + sandbox_id + version + action + idempotency_key` 不产生第二个 Sandbox 或第二套 Provider 资源；
- 只要本服务直接负责的 Pod/PVC/其他实际声明、引用、未知提交或输出交接尚未完成，业务状态不能标记 DELETED；不等待其下游物理清场；
- 所有查询和写入带显式 tenant predicate，禁止依靠 RLS、默认租户或进程配置填补归属。

## 7. 安全和数据面约束

1. 目标校验至少比较 tenant、sandbox generation/version、Pod UID、container 和允许 action；pause/delete 后旧 data-plane target 必须失效。
2. 文件操作限制在 `/workspace`，覆盖规范化路径、symlink、hard-link、rename-out、父目录替换、大小上限和上传幂等。
3. code-run 只接受白名单语言、超时、输出上限和工作目录；容器不具备宿主机权限，Sandbox 进程不依赖 `kubectl` 二进制。
4. token 由 Sandbox 自己的 signer 签发，claims 至少包含 tenant、sandbox version、scope、expiry、jti；没有稳定签名密钥时拒绝启用，不生成进程内随机 fallback。
5. 一期 CPU-only；GPU 需要 ACC profile、设备数、cluster/pool、计费和 runtime 调度合同，不能把 `gpu` 字段或显存展示值直接拼到 Sandbox Pod。
6. egress 的“策略已保存/已回显”与“数据面已生效”分开记录；Kubernetes Provider 无法下发时返回明确失败，不伪造 allowlist 生效。
