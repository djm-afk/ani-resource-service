# LB 监听器延迟下发方案

> 2026-11-04 网络合同更新：本文件中的网络状态、Ready/时效准入、Attachment 释放、SG 应用及 LB 生成物验收条款以[网络状态消费规格 v3](cr-observation.md)为准；方法/字段变更见[实施方案](../plans/network-management-simplification.md)。实际验证只见执行状态。

日期：2026-10-13。本文件是 LB-DEFERRED-01 的业务合同。实施范围按本次 LB-DEFERRED-01 目标文件授权；实际实现、验证、阻塞与发布状态只见 [唯一执行状态](../execution/status.md#lb-deferred-01)。现有合同与历史证据仍见 [LB 现行规格](vpc-connectivity-lb.md)。

## 1. 业务目标与依据

普通监听器无后端时保存数据库和产品预留；添加后端后形成运行投影。局部后端变更只更新受影响配置或撤除不再需要的本服务声明；只有最后一个可下发监听器被清空、整套自身管理责任完成后，LB 才恢复待配置。再次激活保留产品身份并使用正确的新声明身份。

业务规则按本文件和[LB 产品合同](vpc-connectivity-lb.md)，原生状态、调度和删除边界统一见[网络状态消费](cr-observation.md)。2026-10-13 的旧校验/生成资源断点属于原分析快照，现有能力与验证范围只见[执行状态](../execution/status.md#lb-deferred-01)，不通过旧说明恢复已删除 adapter。

## 2. 用户配置与下发集合

数据库保存全部监听器，仍要求一个 LB 有 1～64 个监听器。新流程使用 `listeners.items`，普通监听器的 `backends` 省略与空数组含义相同。协议、端口、名称唯一性、证书引用格式、健康检查与算法参数继续校验；不会因后端为空而放行无效参数。

下发集合是完整用户配置的确定性投影，所有自己声明的 Gateway、Route、Policy 与 Backend 组件需求共用此规则：

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

本服务声明对象的原生配置观察、版本标记、组件 target_version 和 Gateway CAS 使用 provider_target_version 对应的下发投影，不核验生成工作负载或物理地址回收。纯数据库编辑不会要求旧 Gateway 改成新的 desired_version。激活时对最新用户配置重新产生完整投影。

| 主链步骤 | desired | Provider target | applied | operation 完成条件 |
| --- | ---: | ---: | ---: | --- |
| v1 新建全部空监听器 | 1 | 0 | 0 | 配置、占用、幂等回执在同一事务保存 |
| v2 添加首个后端 | 2 | 2 | 0→2 | 本服务声明下发、本次原生配置回执及身份登记完成 |
| v3 仅新增一个空监听器 | 3 | 2 | 2 | 数据库保存，下发投影不变 |
| v4 清空最后一个运行监听器的后端 | 4 | 4 | 2→4 | 不再需要的原声明退出，pending 与自身引用责任完成 |
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

保留既有字段编号，data_plane_state 固定返回 UNKNOWN，data_plane_observed_at 为空。configured、operation succeeded、listener configured 和原生配置回执仅表达各自配置结果，不代表实际流量健康。显式流量诊断可单独执行并记录，但不恢复这些旧字段背后的运行健康证明、后台探测或业务完成门槛。

纯数据库创建/不改变投影的更新，在受理事务内完成对应 operation，回执表达“配置已保存”；不制造 ProviderObservation，首次 observed_at 保持 NULL。混合 LB 的原读取错误与历史操作结果保持，不能因一次数据库编辑被清除；观察年龄不构成业务门槛。runtime 更新和撤流操作在真实观察完成后才成功。

GET/List 保持纯读取。等待后端的记录不进入无休止 apply/退避循环；存在共享运行配置时，原持续观察与安全恢复仍工作。

## 4. 权限、父资源与地址

保留当前业务授权、同租户 placement、父产品引用、地址格式/范围、EIP 产品归属和唯一预留；后端身份使用当前消费者与持久地址映射，不能仅凭 CIDR 放行。空投影保存不访问无关 provider，也不因平台暂时离线或旧安装材料失败。

实际下发核验本次自己需要的配置，例如合法的证书 Secret 引用与租户范围；不追加父网络 Ready、观测时效、固定镜像或生成 Service/EndpointSlice 门槛。专属 EnvoyProxy 只在有运行投影时声明，完整空配置不提前创建。

退回数据库状态保留父资源、VIP intent 和 EIP 产品预留；只有产品删除才释放产品预留。后端引用在最后成员及本服务相应 Backend 声明退出后收尾，不等待下游数据面证明。

## 5. 撤流、再激活与删除

### 5.1 清空部分监听器

仅减少部分 Backend 且相关监听器仍有合法运行投影时，更新受影响的配置，保留仍需要的 Route、Gateway 和专属 EnvoyProxy。清空某个监听器但其他监听器仍可运行时，只撤不再需要的相关 Route/Policy，更新并保留共享 Gateway，消费本次原生配置回执后再删除不再引用的 Backend 并释放相应业务引用。其他监听器和专属 Proxy 保留，不等待生成端口或 EndpointSlice 集合；全部运行投影清空才进入 §5.2。

### 5.2 清空最后一个下发监听器

撤除本服务 Route/Policy，正常删除原 Gateway，再清理专属 EnvoyProxy/Backend；自己声明与 pending 责任完成后推进 applied_version 并进入 waiting_for_backends。不要求不存在的 ready Route，也不等待生成工作负载、VIP 或 EIP 物理解绑。

### 5.3 再激活与未知 CREATE

LB/listener 产品身份、placement 和地址选择保留；旧原生声明退出后，在原事务与 version/lease 保护下分配新的 binding/provider 名，旧 work 不得写回当前运行身份。退休记录只保存自己旧 Gateway 的身份和删除完成，不作为地址回收证书。

CREATE 结果未知不能因 desired 为空、applied=0 或目标版本变化走未发送捷径，不清空原身份猜测成功。继续认回原尝试并完成自己的声明/输出责任；不能恢复 generated 表、VIP absence/occupied proof 或全 namespace 扫描。

### 5.4 产品删除

从未下发的 LB，只有数据库证明全部相关 binding/component 从未 dispatch、pending 为空且 UID 为空，才能在受 fence 保护的事务中完成数据库逻辑删除（state=deleted）并释放产品预留，不访问 Provider；保留 LB、配置、operation、idempotency 与 history 行用于回执重放和恢复。存在任何已发送/未知请求则走正常清理。曾下发但已退回仅数据库状态的记录，使用对应清理目标的持久完成证据，不把“曾下发”误判为“从未下发”。

业务实例、VPC 基础连接与独立 EIP 本体保留。清理失败继续可查询，不能强删 finalizer、先删库或 namespace 制造成功。

## 6. 数据与接口兼容

desired_version、provider_target_version 和 applied_version 分别维护，不由空投影混成同一数字。旧已接受配置和永久幂等回执保留原 bytes/语义，派生的运行状态不加入客户端输入指纹。新增或废弃字段遵守原 protobuf tag 与迁移 checksum，不恢复已删除 proof 列。

实际 schema 与字段以当前迁移、生成 API 和查询为准；历史 0013 增量不是当前唯一部署版本，0032 删除生成物/物理释放证明后的约束继续有效。退休 binding 是原声明身份历史，不能复用旧 UID 作为新运行对象。

RPC/HTTP 继续复用现有 Create/Get/List/Update/Delete/GetOperation。PATCH 只从 GET 提取可写 data/update_mask 与监听器集合，不回传部署 reason 等只读字段；Resource、Governance、Console 各按自己的接线范围处理，不增加通用配置或审批服务。

## 7. 实施位置与验收

实施环境依据当前任务确认的连接入口与[远程约定](../remote-execution.md)。旧 LB-DEFERRED-01 的 Fedora 和 10～12 集群要求属于该工作包的历史执行条件，不能作为当前连接配置、共享设施权限或新一轮部署授权。实现检查与真实流量诊断分别记录；文档修复只需文档静态检查。

| 验收 | 必须观察到的结果 |
| --- | --- |
| AC01 首次全空创建及未激活更新 | 授权与持久保存成功，operation 完成，target/applied=0；无 LB CR mutation，API 为 waiting |
| AC02 首次激活 | 添加归属正确后端，本服务完整声明下发并消费原生配置结果；显式流量验收另记 VIP:port 实际结果 |
| AC03 混合监听器 | 新空 listener 只改数据库；旧声明身份和配置保持，target/applied 不推进；激活后提交所需配置 |
| AC04 清空/再激活 | 清空部分只撤对应声明；清空全部完成自身组件退出与产品管理责任，claim reserved；再激活保留产品 ID 并使用新运行身份，不等待物理地址或生成数据面退出 |
| AC05 幂等与并发 | 同键重放、异意图冲突、expected_version、跨租户与共享 backend 最后引用保护有效 |
| AC06 恢复与清理 | 撤流/删除/再创建中进程重启、响应丢失和旧 lease 回写受控；未知 CREATE 不走未发送捷径 |
| AC07 兼容与协议 | 旧回执/旧数据保持；HTTP/HTTPS/TCP/UDP/SSL、redirect、权重零与证书延后检查定向覆盖；HTTPS:443 空、8443 有后端时 redirect 等待443，443激活/清空时同步激活/撤除 |
| AC08 产品删除 | 从未下发及曾下发两条路径正确完成自己的声明与产品占用；业务后端和独立 EIP/VPC 保持 |

真实流量检查以任务授权和具备的平台条件为前提；缺少 Class、地址池、证书或 owner 输入时记录 not_verified，不擅自补装共享平台。替身边界和真实结果分开，单条 HTTP 链不代表所有协议、公网或生产已验收。实现改动运行适用检查及提交前 make verify，实际命令、源码和结果仅写入执行状态及对应记录。

## 8. 完成边界

实现完成需要相关接线、AC01～AC08 对应管理行为、恢复和兼容检查有实际证据；部署与真实数据面验收按独立授权及范围记录。计划、生成、CI、Ready 或 HTTP 200 各自只证明对应事实。受阻时明确已完成部分、未验证范围和恢复输入，不能用历史工作包的环境或验收结论代替当前结果。

本规格规定行为，不授予实施、发布或共享设施变更权限；实际状态只见[执行状态](../execution/status.md)。
