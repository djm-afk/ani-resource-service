# LB 监听器延迟下发方案

> 2026-11-04 网络合同更新：本文件中的网络状态、Ready/时效准入、Attachment 释放、SG 应用及 LB 生成物验收条款以[网络状态消费规格 v3](cr-observation.md)为准；方法/字段变更见[实施方案](../plans/network-management-simplification.md)。实际验证只见执行状态。

日期：2026-10-13。本文件是 LB-DEFERRED-01 的业务合同。实施范围按本次 LB-DEFERRED-01 目标文件授权；实际实现、验证、阻塞与发布状态只见 [唯一执行状态](../execution/status.md#lb-deferred-01)。现有合同与历史证据仍见 [LB 现行规格](vpc-connectivity-lb.md)。

## 1. 业务目标与依据

主链：用户创建没有后端的监听器 → 数据库保存成功、详情显示待添加后端 → 用户在详情添加后端 → 真实授权、持久化、worker 和 Provider 完成下发 → 入口转发到业务后端 → 清空后端后撤流并返回待配置 → 再次添加后端恢复 → 产品删除释放占用。

采用用户提供的 KCN 使用参考：**普通监听器没有后端时只保存数据库，不向 Kubernetes apply 该监听器的 CR；添加后端后才下发。**这是用户给出的流程依据，本轮没有核验 KCN 团队实现源码。

本方案变更 LB 的空后端准入、下发集合、版本、状态、恢复和回收规则；不改变 Network 生命周期所有权、租户隔离、既有身份链或实际业务实例的所有权。首次交付限定 Resource 与 Governance 后端，接口允许详情页完成操作，不把前端实现列为本轮任务。

分析快照：

| 仓库 | 2026-10-13 核对的远端 main |
| --- | --- |
| ani-resource-service | `e7b5a8a3435996181bff62e025a21224c97d2464` |
| ani-governance | `44baecb7bc6d874b859ed556610dd5fa70bbe709` |

它们是分析来源，执行时应刷新 main、检查相关差异，再冻结实际实施 SHA/tree。原 Image 工作区与无关修改保留。

当前具体断点：

- [业务校验](../../internal/biz/network/load_balancer.go)拒绝普通空后端；[监听器组件需求](../../internal/biz/network/load_balancer_parameters.go)无条件包含转发组件。
- [LB worker](../../internal/biz/network/load_balancer_worker.go)以至少一个 member 为创建及完成前提，没有区分正常空配置和后端身份失效。
- [Gateway 生成](../../internal/data/network/kc_load_balancer.go)与 [Service 端口核验](../../internal/data/network/kc_load_balancer_generated.go)使用全部已保存监听器；[工作快照](../../internal/data/network/load_balancer_worker.go)以完整 desired_version 为 Provider 目标。
- 数据库可保存零 member，但 [配置表与状态约束](../../migrations/0007_load_balancers.sql)没有独立 Provider 目标版本或待后端状态；最后一个后端清空后的清理及 Gateway 再创建也需要明确路径。

## 2. 用户配置与下发集合

数据库保存全部监听器，仍要求一个 LB 有 1～64 个监听器。新流程使用 `listeners.items`，普通监听器的 `backends` 省略与空数组含义相同。协议、端口、名称唯一性、证书引用格式、健康检查与算法参数继续校验；不会因后端为空而放行无效参数。

下发集合是完整用户配置的确定性投影，所有 Gateway、Route、Policy、生成 Service 端口、组件需求与完成检查共用此规则：

| 配置 | 下发规则 |
| --- | --- |
| 普通监听器，配置的 backend 集合为空 | 仅保存数据库，没有该监听器的任何专属 CR，也不加入 Gateway listeners/Service 端口期望 |
| 普通监听器，配置的 backend 集合非空 | 属于下发集合；成员即使身份失效也仍是需要处理的配置，按现有安全撤流与故障规则处理 |
| backend 权重全部为 0 | 集合非空，仍属于下发集合；不能将暂停流量误判为未配置后端 |
| HTTP→HTTPS 重定向 | 本身不要求 backend；先在完整用户配置中按既有 443 优先/唯一目标规则确定 HTTPS 目标，再判断该目标是否属于下发集合。目标待后端时，重定向配置保留在数据库并等待，不能过滤空监听器后悄悄换目标 |

仅新增、删除或修改未激活监听器，若下发投影不变，不修改已有 Gateway/Route/Policy，也不增加 Service 端口。混合 LB 中其他监听器继续正常运行。首次所有监听器均待配置时，不创建共享 Gateway，因而也不创建 LB 生成的 Service/Deployment/Pod/EndpointSlice。

这里的“只存数据库”针对本次新增 LB 运行配置，不删除或重建已有 VPC、Subnet、独立 EIP 及其 CR。

## 3. 三个版本与对外状态

### 3.1 版本

单一 desired_version 无法同时表示纯数据库编辑与 Kubernetes 目标；仅放宽空后端校验会使混合 LB 被新版本标记卡住。因此增加 `provider_target_version`，其余版本保留：

| 字段 | 定义 |
| --- | --- |
| `version` / `expected_version` | LB 乐观并发控制，继续沿用现有合同 |
| `desired_version` | 最新已受理的完整用户配置版本，每次配置更新按现有规则递增 |
| `provider_target_version` | 最近一次改变下发投影所对应的完整配置版本；首次纯数据库配置为 0 |
| `applied_version` | 最近一次有真实外部证据确认的 Provider 目标版本；也可表示已确认执行完的空投影清理，不代表当前一定存在运行实例 |

数据库将 provider_target_version 保存为可空 bigint，NULL 在 API 中输出为 0；正值通过 `(tenant_id, lb_id, provider_target_version)` 复合外键引用本 LB 配置，采用可延迟外键以支持同一受理事务内写目标版本和配置。约束：`0 <= applied_version <= COALESCE(provider_target_version,0) <= desired_version`。不要用 SQL 表达式直接充当外键列。

Provider 观察、对象版本标记、组件 target_version、Gateway CAS 和生成资源期望使用 provider_target_version 对应的下发投影。纯数据库编辑不会要求旧 Gateway 改成新的 desired_version。激活时对最新用户配置重新产生完整投影。

| 主链步骤 | desired | Provider target | applied | operation 完成条件 |
| --- | ---: | ---: | ---: | --- |
| v1 新建全部空监听器 | 1 | 0 | 0 | 配置、占用、幂等回执在同一事务保存 |
| v2 添加首个后端 | 2 | 2 | 0→2 | 真实下发与身份、端口、地址观察完成 |
| v3 仅新增一个空监听器 | 3 | 2 | 2 | 数据库保存，下发投影不变 |
| v4 清空最后一个运行监听器的后端 | 4 | 4 | 2→4 | 旧转发、共享数据面与实际地址绑定清理确认 |
| v5 再添加后端 | 5 | 5 | 4→5 | 新运行身份下完整创建与观察完成 |

provider_target_version 在一次运行生命周期内单调前进；清零后不重置为 0。0 只能表达从未产生下发目标，不能单独作为“从未发送 CREATE”的证明。

### 3.2 状态与 operation

Resource/Governance 的 LB ConfigurationState 追加 `WAITING_FOR_BACKENDS`，不改旧枚举数值；LB 响应追加 provider_target_version。监听器响应追加只读 deployment_state/reason，状态为 waiting_for_backends、applying、configured、removing、degraded、unknown；重定向等待目标时使用 waiting_for_backends 并返回 `WAITING_FOR_REDIRECT_TARGET` 原因。状态由已保存配置、目标投影、组件事实和清理阶段推导，不新增监听器独立任务队列或状态表。

| 情况 | LB 生命周期/配置状态 | 语义 |
| --- | --- | --- |
| 全部等待后端、从未下发或已完成清理 | provisioning / waiting_for_backends | 有可管理记录，暂无运行入口；不是持续重试的创建故障 |
| 首次激活或修改下发集合 | provisioning 或原有生命周期 / applying | operation 等待真实外部配置完成 |
| 下发集合已确认，其中可有待配置监听器 | available / configured | 需要下发的集合已配置；空监听器仍单独显示待后端 |
| 期望有后端但身份/Provider 失效 | 沿用 degraded/unknown 等故障规则 | 不能转成正常等待后端 |
| 撤最后一个下发监听器期间 | 原有生命周期 / applying | listener 为 removing，清理完成之前不报告 waiting |

data_plane_state 继续基于真实健康证据；缺少证据为 unknown。configured、operation succeeded 或 listener configured 均不意味着流量健康。

纯数据库创建/不改变投影的更新，在受理事务内完成对应 operation，回执表达“配置已保存”；不制造 ProviderObservation，首次 observed_at 保持 NULL。混合 LB 的原 Provider 故障及观察时效保持，不能因一次数据库编辑被清除。runtime 更新和撤流操作在真实观察完成后才成功。

GET/List 保持纯读取。等待后端的记录不进入无休止 apply/退避循环；存在共享运行配置时，原持续观察与安全恢复仍工作。

## 4. 权限、父资源与地址

所有操作继续经 Governance JWT 或 AK/SK、NETWORK 套餐、既有权限 → 可信租户映射 → mTLS → Resource 租户过滤与事务。添加后端时校验 Attachment、Pod/VNic/VNicIP 身份，不只检查 CIDR；实例 owner 仍拥有业务实例生命周期。

保存阶段校验持久资源归属、冻结的 cluster/namespace、父资源未封闭/删除、VIP 格式/子网范围和 EIP 产品归属、占用及有效参数。纯数据库监听器无需因 K8s 平台当前离线、安装观察过期或 TLS Secret 尚未可读而失败；运行前提检查放到激活路径。EIP 本身必须是已经产品分配的合法记录，不能用此流程生成虚构 EIP。

激活时重新核验 VPC/Subnet 自身 Provider 身份、代次及新鲜度、公共 LB 配置与固定镜像、证书 Secret 权限/租户标签、后端身份和地址实际可用性；不依赖基础 Intranet EIP/SNAT 或 VPC 聚合 available。专属 EnvoyProxy 在首次有运行投影时与 Gateway 一起下发，全空配置不提前创建专属 CR。失败保留已保存配置、原占用和可查原因；不绕过前提，也不偷偷换 VIP/EIP。

入口 Subnet、VIP intent、EIP claim 继续在数据库受理时预留。产品预留不等于 kc IPAM 实际分配或 Service 实际 EIP 绑定；首次激活仍可能因外部地址冲突失败。

退回仅数据库状态时保留父资源、入口子网、VIP intent、EIP 产品预留。EIP claim 在实际解绑确认后由 bound 改回 reserved，`released_at` 保持 NULL。只有整个 LB 产品删除才释放这些预留。后端子网在最后成员引用和对应未删 Backend 组件都消失后释放；跨监听器共享成员依照引用联合集合保护。

## 5. 撤流、再激活与删除

### 5.1 清空部分监听器

受理新配置 → 撤该监听器 Route/策略 → 从共享 Gateway 移除该监听器 → 确认 Service/EndpointSlice 对应端口撤除 → 删除不再被其他监听器使用的 Backend → 释放对应后端子网引用。其他监听器的 Gateway UID、组件和流量保持。

### 5.2 清空最后一个下发监听器

受理空投影 → 撤全部自有转发/策略 → 删除本 LB 共享 Gateway → 观察控制器生成的 Service/Deployment/Pod/EndpointSlice 释放与 VIP/EIP 实际解绑 → 清理本 LB 专属 EnvoyProxy 和剩余自有 Backend → 完成 operation、推进 applied_version 到该清理目标 → waiting_for_backends。公共 GatewayClass/EnvoyProxy 保留。各依赖步骤继续按既有实际释放门禁推进；不得为了形式顺序越过未知请求、finalizer 或地址释放条件。

全部空投影时，不要求一条仍存在的 ready Route。需要的是针对退休组件、端口、生成资源和绑定的真实删除/释放证据。

### 5.3 再激活与未知 CREATE

LB/listener 产品 ID、租户、placement 和地址选择保持。已退休 member 重新添加时传空 member ID，重新核验并分配新身份；旧 Route/Policy/Backend 组件保留墓碑，新增组件不能复活旧 provider UID。

整个 Gateway 被删除后，必须在确认旧 Gateway、生成资源、实际地址绑定已清理的前提下，在持锁事务中分配新的 binding_id/provider_name，重置当前运行实例的 dispatch/pending/UID，同时通过资源版本及 lease fence 防止旧 work 回写。旧生成资源记录保留 released_at 与原 gateway_uid；新运行观察只使用当前 binding/incarnation，不能将旧记录算作新实例已运行或已释放。

追加 migration 增加专用 `network_lb_retired_bindings` 表，保存已确认退出的运行身份：tenant_id、cluster_id、namespace、lb_id、旧 binding_id/provider_name/provider_uid、cleanup_target_version、完成 operation_id、VIP occupied/absence 修订及 cleanup_completed_at。以 tenant/LB/binding 为唯一键，配置、操作和 LB 引用使用保持租户/placement 的复合约束。此表只存已完成清理的不可变事实，不承担第二套任务调度。当前 binding 仍是唯一正在运行/恢复的写入入口。

清理完成时在受 lease/version fence 保护的 Finish 事务中写该退出事实；新增身份前必须读到与当前旧 binding、UID、清理目标一致的完成行。相关语句放在现有 LB queries，并随 sqlc 固定工作流生成。不能仅写普通 history 文案或仓库日志作为重建身份的数据库门禁。

VIP 占用/释放修订与 Provider absence proof 必须绑定旧 binding_id、Gateway UID 和对应清理目标版本。准备新实例的持锁事务先归档已确认的旧实例退出证据，再清空当前实例的 VIP occupied/absence 修订，等待新实例的实际观察；新实例不能复用上一次的释放证明来判定本次已解绑。只有旧实例确认清理完成时允许该重置，未知 CREATE 不适用。

CREATE 已发送但结果未知时，不能因当前 desired 已空、applied=0 或目标版本变化就走未下发捷径、重置身份或再创建。继续使用现有归属核验和 unknown-CREATE 保护；无法证明不存在则明确阻塞并保留恢复记录。

### 5.4 产品删除

从未下发的 LB，只有数据库证明全部相关 binding/component 从未 dispatch、pending 为空且 UID 为空，才能在受 fence 保护的事务中完成数据库逻辑删除（state=deleted）并释放产品预留，不访问 Provider；保留 LB、配置、operation、idempotency 与 history 行用于回执重放和恢复。存在任何已发送/未知请求则走正常清理。曾下发但已退回仅数据库状态的记录，使用对应清理目标的持久完成证据，不把“曾下发”误判为“从未下发”。

业务实例、VPC 基础连接与独立 EIP 本体保留。清理失败继续可查询，不能强删 finalizer、先删库或 namespace 制造成功。

## 6. 数据与接口兼容

零 member 本身无需迁移，但独立 Provider 目标版本、新状态与已退出运行身份的持久事实表需要追加 migration。分析基线下一编号可为 `0013_lb_deferred_activation.sql`；执行时以最新 main 的下一编号为准，旧 migration bytes/checksum 保持。

旧完整配置记录的 provider_target_version 回填为 desired_version；旧无完整已受理配置的 reservation-only 记录使用 NULL。以明确持久配置区分，不能凭当前观察或 applied=0 猜测。旧既有 LB 不因迁移自动撤流、重建或改身份。

RPC/HTTP 继续复用 Create/Get/List/Update/Delete/GetOperation，PATCH 使用 data + update_mask 与完整监听器集合。客户端从 GET 组装更新时只提取输入 DTO 的可写字段，不把 deployment_state/reason 等只读响应字段回填到请求。增加只读字段与枚举，Resource 生成契约、Governance DTO/mapper/OpenAPI 一起接线；不引入新的 backend CRUD 权限或通用配置服务。旧单监听器请求保持可用，新分步操作使用 listeners 集合。

旧幂等 fingerprint 版本及已保存受理回执不重写、不补新字段。新派生的 Provider 目标/运行状态不能参与客户端输入 fingerprint。旧回执 replay 保持原语义，新 GET 可返回新增观察字段。

## 7. 实施位置与验收

本地用于读取/编辑源码和文档、Git 差异审阅、SSH/SCP 转运。**依赖下载、生成、格式化、构建、所有测试、PG/Redis、镜像、运行与 API 驱动统一 `ssh fedora`；Fedora 不可用时保持断点，不回退本地或 Ubuntu。**按 [远程约定](../remote-execution.md)建立任务独占源码、缓存、数据库和日志，用户本轮 Fedora 限制优先于旧回退条款。

真实 Kubernetes 只使用 `172.16.101.10`、`.11`、`.12` 对应的 10～12 集群。实施前读取当前 kubeconfig/context/API Server/CA、kube-system UID、节点与 Provider 版本，核对三节点后冻结；历史 context、地址池与 UID 仅是历史证据。使用隔离租户/namespace、独占 Resource/Governance/数据库和任务资源，不更新共享 ani-system 服务或 KCN/Envoy 安装。

| 验收 | 必须观察到的结果 |
| --- | --- |
| AC01 首次全空创建及未激活更新 | 真授权与 PG 保存，operation 完成，target/applied=0；没有 LB CR mutation，API 为 waiting |
| AC02 首次激活 | 添加归属正确后端，完整 Provider 目标下发，操作完成，直接 VIP:port 命中业务后端 |
| AC03 混合监听器 | 新空 listener 只改数据库；旧 UID/spec/端口/流量保持，target/applied 不推进；激活后新增正确端口 |
| AC04 清空/再激活 | 清空部分仅撤相应转发；清空全部确认共享数据面和实际地址释放、claim reserved；恢复时产品 ID 保持、Provider 新身份 |
| AC05 幂等与并发 | 同键重放、异意图冲突、expected_version、跨租户与共享 backend 最后引用保护有效 |
| AC06 恢复与清理 | 撤流/删除/再创建中进程重启、响应丢失和旧 lease 回写受控；未知 CREATE 不走未发送捷径 |
| AC07 兼容与协议 | 旧回执/旧数据保持；HTTP/HTTPS/TCP/UDP/SSL、redirect、权重零与证书延后检查定向覆盖；HTTPS:443 空、8443 有后端时 redirect 等待443，443激活/清空时同步激活/撤除 |
| AC08 产品删除 | 从未下发及曾下发两条路径正确释放产品占用；业务后端和独立 EIP/VPC 保持，任务资源清理可复核 |

首先在 Fedora 用真实业务代码、鉴权、PG、worker 和 adapter 跑通上述最小纵向链；只有 K8s/控制器/owner 外部协议边界可用替身。替身验证与真实结果分开。

Provider 下发与共享 Gateway 回收是本方案关键，因此需在 10～12 做一条 small/private/HTTP 的真实完整链，包含全空零新增 CR、首次转发、混合监听器、清空、再激活与删除。公网/双入口只在既有平台前提可用且相关绑定变更需要核验时做定点验收；缺少 Class、地址池、证书或 owner 输入则记录 not_verified，不补装共享平台。HTTP 真实链不代表所有协议、公网或生产均已验收。

改动对应定向测试先行，提交前保留仓库要求的 `make verify`。只因受影响逻辑、失败或明确门禁扩大测试；不重复已通过检查。实施结果只进入唯一执行状态与一份工作包记录索引，原始日志/大工件留任务私有目录。

## 8. 完成边界

完成需要主链代码与 Resource→Governance 接线可运行、AC01～AC08 对应的软件检查通过、10～12 的私网 HTTP 主链真实通过、相关恢复与产品清理通过，并保留准确源码/命令/host/结果。必需真实链受阻时，交付已完成代码和具体恢复输入，整体保留未完成；计划、生成、CI、Ready 或 HTTP 200 不能替代业务结果。

本规格不单独授权执行或发布；LB-DEFERRED-01 启动目标文件授权本轮实现与隔离验证，提交、推送、PR、合并、SDK 发布和共享部署按启动会话的明确授权分别处理。
