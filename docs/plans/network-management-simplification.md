# Res 网络管理简化实施方案

本方案已获用户授权，于 2026-11-04 开始在 main `5dc539d92e3150a3593bde3668082753aa4fdebe` 上实施。以下保留获批的方法和字段级规格；当前实现、验证和部署结果只见[执行状态](../execution/status.md#network-management-simplification-20261104)。正式状态消费合同见[规格 v3](../specs/cr-observation.md)。


本方案基于 Res 主分支 `5dc539d92e3150a3593bde3668082753aa4fdebe`，核对日期为 2026-11-04。KCN 上传源码 `6132623baabee50f17e9e179c0f5e9c1f380db04` 只用于确认已有状态字段。以下方法和字段级安排保留审批时的设计输入，不作为实现、测试或部署结果；这些结果只记录在上述执行状态中。

**整改目标：Res 直接消费 KCN 对自己管理对象给出的状态，删除自行验证底层网络结果的逻辑。此次实施全部在 Res 完成，没有 KCN 改造前置条件。**

固定执行路径：用户配置进入现有业务事务和任务表；普通 informer 事件只通知任务；worker 精确 GET 自己的目标对象；按对象原生状态保存和展示。删除确认自己的原对象已不存在，即结束该管理生命周期。

Res 保留自己的租户与 placement、资源账本、输入约束、用户引用、幂等记录、命令发送记录、版本与租约、原对象 UID。删除的是用 Ready 以外的字段、关联对象、镜像、节点、生成资源、审计快照、TTL 和清场证明再次判断 KCN 是否可信的逻辑。

## 唯一准入口径

`AcceptSubnet`、`AcceptVNic`、`ReserveVNic`、`PrepareAttachment`、`AcceptEIP`、`AcceptSnat`、`AcceptLoadBalancer` 以及 Instance/VM 网络选择，只检查 Res 记录存在、tenant/placement 匹配、未 deleting/deleted、声明名称确定、用户参数与本地占用。删除 Provider Ready、观察时间、父对象 UID 已返回、MAC/IP 已生成等准入条件。网络状态仍原样展示；受理管理声明不再现场验收网络。

## 权威状态映射

| 产品 | 唯一运行结果来源 | 本次明确删除的额外判据 |
| --- | --- | --- |
| VPC、Subnet、VNic、EIPGateway、地址池、EIP、SecurityGroup | 自己目标 CR 的主要 `Ready` condition | Valid/Initialized 合取、代次通用门槛、boundResources、父资源 Ready、节点/镜像/物理设备 |
| SNAT | 自己 `status.phase`；Bound→applied_enabled=true，Disabled→false，Pending/缺失→nil | EIP 反向绑定、VPC Ready、统一 Ready/generation、gateway/node/Valid 合取 |
| VLAN | 自己 `Initialized` condition，表示配置已被控制器接受 | notReadyNodes、daemon/chassis、设备 wiring |
| Default VPC Egress | 自己的 EIP 与 SNAT；启用结果取 SNAT phase | PoolProof、联合快照、EIPReady/Unbound、admission hash |
| VirtualSwitch | Res 对已登记成员 Node 写入的 label/annotation | 全集群成员一致、链路健康、KCN feedback 清空 |
| Attachment | 当前 Res 消费者的 open/stopped/closing/closed 管理事实 | Pod→VNic→VNicIP→Nic 清场链、历史 controller/Pod 闭包 |
| LB | Res 明确声明的 Gateway/Route/Backend/Policy 的 owner 状态 | 生成 Deployment/Pod/Service/EndpointSlice、EIP/IPAM 回收证明 |

VPC/Subnet/VNic 的 KCN generation 不再成为附加门槛。SG 与 Gateway/Route 在原生 API 已提供配置确认代次时，只用它确认本次更新是否被处理，不能用来推翻已保存的运行状态或阻止其他管理请求。

源码入口：[worker.go](https://github.com/zhangzhe-ctrl/ani-resource-service/blob/5dc539d92e3150a3593bde3668082753aa4fdebe/internal/biz/network/worker.go)、[kc.go](https://github.com/zhangzhe-ctrl/ani-resource-service/blob/5dc539d92e3150a3593bde3668082753aa4fdebe/internal/data/network/kc.go)、[kc_egress.go](https://github.com/zhangzhe-ctrl/ani-resource-service/blob/5dc539d92e3150a3593bde3668082753aa4fdebe/internal/data/network/kc_egress.go)。后文路径均相对这个固定版本的 Res 仓库。

# 2. 共同状态、错误与操作语义

## 状态与历史操作分开

| 本次直接结果 | 当前资源投影 | 尚未完成的 operation | 已完成的 operation |
| --- | --- | --- | --- |
| Ready=True | Available，包括此前 Failed/Degraded | 自己本次声明已提交后成功 | 原样保留 |
| Ready=False | 原 Provisioning/Failed→Provisioning；原 Available/Degraded→Degraded；显示 KCN 原因 | 继续办理/重试 | 原样保留 |
| Ready 缺失或 Unknown | 保留上次状态；初次仍 Provisioning，原因可显示 PROVIDER_STATUS_UNREPORTED | 等目标对象报告 | 原样保留 |
| GET 超时、403、429、连接失败 | 保留 State、Reason、地址、Applied 值和 ObservedAt | OperationReason 记本次请求错误并重试 | 不更新，包括 UpdatedAt |
| API 明确拒绝本次写入 | 既存对象保留旧运行投影；首次创建被明确拒绝且从未生成原对象时，资源 Failed 表达 Res 声明提交失败 | 本次 operation 按现有错误分类失败 | 原样保留 |

删除 `ProviderObservation.RejectionReason`。该字段当前的赋值来自 VNic 的 Valid、地址差异和子对象推导；这些用途一并删除，不再保留一个没有明确契约的“原生终态”分支。写请求错误仍通过现有错误分类及 OperationReason 处理。

## 存在性与删除规则

| 场景 | 固定行为 |
| --- | --- |
| 活跃 create、原 UID 尚未知、直接 GET 404，包括 pending_action=create | 同 binding/namespace/name 重试 POST；AlreadyExists 后 GET 同名目标，按本层归属采用；不另换名称 |
| 已保存原 UID，非删除观察 404 | 记录原对象缺失；不自动 POST 一个新 UID，不接管同名新对象 |
| 非删除 GET 200，但同名对象 UID 与保存的原 UID 不同 | 本层身份冲突；不采用该对象投影、不更换 binding UID、不修改该对象 |
| 无活跃创建命令，GET 404 | 不自动重建；保留历史操作，当前资源表达目标缺失 |
| 删除中 GET 404 | Deleted、删除 operation 成功、清 pending、释放本地引用并退出调度；pending_create 不构成例外 |
| 删除中同名对象 UID 与保存的原 UID 不同 | 原对象已结束，完成原删除；不删除或接管新 UID |
| 原 UID 未知，同名对象属于其他 binding | 本层归属冲突；不能修改别人对象 |
| 原对象仍存在 | 原 UID 条件 DELETE；已有 deletionTimestamp 就等待原对象消失；不拆 finalizer |
| 已 dispatch 但没有做任何 provider 读取 | 不能直接走“从未提交”的 DB-only 取消；安排直接 GET，再按上面结果完成 |
| 已完成 Deleted/Released | 永久退出该生命周期的任务；事件和启动恢复都不复活墓碑检查 |

`Worker.Step` 删除历史 `case Failed` 锁死；`Worker.ensure` 删除 RejectionReason/Proof 分支；`Worker.delete` 删除 HasDependencies、pending_create+404 阻塞和 tombstone 分支。现有 UID 条件写、版本、租约仍有效。

## 成功读取与时间

`observed_at` 只表示 Res 最近一次成功读取并保存该投影的事务时间。`ObservationStale = observed_at IS NULL OR observation_error <> ''`；不比较年龄，不清 applied_enabled，不影响准入。

本次没有尝试读取：既不更新 observed_at，也不清掉旧读取错误。主消费管理事实读成功、可选地址读取失败：可以提交管理生命周期；保留失败字段和 observation_error，不能因主读取成功覆盖同轮地址错误。Egress/Default Egress/LB/VirtualSwitch 的本轮必需目标 GET 任一失败，本轮运行状态与输出整体沿用旧投影；自己的命令发送/回执仍按现有事务记录。Attachment 可以提交成功读取的当前管理事实，地址失败时沿用 work 中旧地址并保留错误。这里不新增字段级部分观测或联合快照框架。

源码入口：[Worker.Step/ensure/delete](https://github.com/zhangzhe-ctrl/ani-resource-service/blob/5dc539d92e3150a3593bde3668082753aa4fdebe/internal/biz/network/worker.go)、[Finish](https://github.com/zhangzhe-ctrl/ani-resource-service/blob/5dc539d92e3150a3593bde3668082753aa4fdebe/internal/data/network/worker.go)。

# 3. 共享接口和字段整改

文件：`internal/biz/network/worker.go`、`observation.go` 以及各 worker 的 Work/Progress 定义。

| 类型 | 删除 | 保留或替换 |
| --- | --- | --- |
| `ObservationProof`、`ObservationRequirement` | 整个类型及 Covers/Hash/CollectedAt/AppliedAt/CoveredGeneration 逻辑 | 无替代证明对象 |
| `ProviderTarget` | Requirement、Direct、Tombstone | Deleting、TenantID、ResourceID、BindingID、KnownIdentity、Kind、VPCID/SubnetID、CIDR/Gateway、RequestedIP/MAC、SubnetOptions、Egress |
| `ProviderObservation` | Proof、HasDependencies、Generation、ObservedGeneration、RejectionReason | Ready: bool→*bool；Exists/Identity、VNic/SubnetIPRanges/Egress、NeedsUpdate；新增 StatusReason Reason，仅投影主要状态原因 |
| `Work` | Requirement、GateReason、DependencyFailed 的运行健康用途 | 新 ClaimedGeneration int64；Owner/Epoch、Resource.Version、ActiveOperation、Operation、原对象身份、PendingAction、CancelUnsent |
| `Progress` | Proof、ProviderGeneration、ProviderObservedGeneration | 保留 Observed、State/Reason、OperationState、Identity、ClearPending、Backoff/NextDelay、ProviderError；新增 ObservationError、OperationReason |
| `WorkerPolicy` | StaleAfter | Lease、RequestTimeout、ObserveEvery、RetryMin/RetryMax |
| 公共业务模型 | 各自基于年龄计算 stale/清状态的逻辑 | 现有 ObservedAt/ObservationStale 字段；由 data 层读取内部任务错误后映射 |

共同增量字段固定如下，其他已有 worker result 使用同名字段；没有 operation 的 Attachment 不增加 OperationReason：

```go
// Work：仅确认本次已经消费的 Res 队列通知
ClaimedGeneration int64

// ProviderObservation：直接原生状态
Ready        *bool
StatusReason Reason

// Progress / 有 operation 的对应结果
Observed         bool
ObservationError Reason
OperationReason  Reason
// 原有 ProviderError error 继续仅供日志
```

`StatusReason` 只搬运主要 condition 的 reason；无值时使用 PROVIDER_NOT_READY/PROVIDER_STATUS_UNREPORTED。它不触发终态分类，不分析 message 文本，不从 Valid 或地址差异生成拒绝。Ready=True 时资源成功原因清空。

`NeedsUpdate` 只表达 Res 自己尚未提交的声明步骤，例如显式 RequestedIP 对象的创建；不允许以 KCN 自动子对象或地址结果不符合预期驱动“修复”。

`Worker.Step` 初始化 Progress 时明确带上旧 `State` 与 `Reason`，不能只复制 State/Identity。读取失败时 observed=false，各 Advance SQL 保留旧输出，禁止把未填 facts 的零值写回；operation 成功清 OperationReason，正常处理中使用当前办理原因，读取错误才写本次错误。

删除 `ErrObservationPending`、`proofTime`、`coveredGeneration`。`advanceResource` 固定为 `advanceResource(ctx, q, resource, progress, observedAt *time.Time)`：Finish 仅在成功读取时传入同一事务的 now，否则 nil。

# 4. observer、任务通知和提交事务

## 逐方法替换

| 文件/方法 | 动作与替代 |
| --- | --- |
| `data/network/kc_observation.go: ObservationOptions / DefaultObservationOptions` | 删除审计参数模型 |
| `EnableObservation` | 改为 `EnableObservation(requestTimeout time.Duration) (*KCObservation,error)`；只组装通知实现 |
| `KCObservation.Start/Stop` | 启停普通 informer、client-go typed rate-limiting queue 和纯指标采集；启动只重排已持久非终态任务 |
| `changed` | 改 `changed(gvr, old, new)`，只取 namespace/name 入队；Update resourceVersion 相同可忽略 |
| `unwrapObject` | 只解开 DeletedFinalStateUnknown 取键；缓存删除不直接完成产品删除 |
| `auditLoop/refresh/collect/sourceBoundary/lbSourceBoundary` | 删除；没有独立全量审计、全局失效或全源同步成功门槛 |
| `valid/validAttachment/validChanges/cachedSnapshot/snapshot/attachmentObservationObject` | 删除共享快照和关系有效性计算 |
| `flush/wakeAudited` | 替为 `processNextNotification`：DB 通知成功 Forget，失败 AddRateLimited，最后 Done |
| `kc_index.go: relationshipKeys/relationshipIndex/indexedObjects` | 删除关系索引；只为这些索引使用的 contentHash 同删 |
| `kc_observed_provider.go: Observe` | 直接派发 observeDirect/observeEgress；删除缓存、Critical、Requirement、Proof 分支 |
| `ObserveAttachment/completeDependencies/candidatesForHash` | 删除；Attachment 改当前管理事实与普通地址投影 |
| `kc_tombstone_observation.go` | 整个运行实现删除 |
| `data/network/observation.go` | 留普通 Notify 包装；新增 NotifyObservedObject/WakeManagedTasks，删 proofTime/coveredGeneration |

`KCObservation` 删除 view、changes、attachmentChanges、sequence、epoch、overflow、refreshing、audit 统计与手工有界队列恢复字段；保留 informer 生命周期和普通计数；新增 `queue workqueue.TypedRateLimitingInterface[observedObjectKey]`。`Postgres.sharedObservation` 删除。业务 worker 不从 informer store 读取状态。

## 精确通知路由

新增 `observedObjectKey{GVR, Namespace, Name}`。`Postgres.NotifyObservedObject(ctx,gvr,namespace,name)` 调用 `LookupObservationTargets :many`，以固定 cluster_id 加精确对象键，在现有 DB 绑定中返回 target_type/target_id/tenant_id，再 Notify 已有任务。

| 事件 | 只匹配这些已有记录 |
| --- | --- |
| VPC/Subnet/VNic/EIP/Snat | network_provider_bindings 的 cluster/kind/namespace/provider_name |
| Res 自建 RequestedIP VNicIP | 有该明确声明的 manual VNic binding；不匹配所有生成 VNicIP |
| EIPGateway/VlanNetwork/平台 Subnet | network_platform_resources 的 kind/provider_name/固定 namespace |
| LB 九类直接声明组件 | network_lb_components；主 Gateway 的 LB binding |
| 当前 Pod | network_attachments 的 cluster/namespace/pod_name，排除 released |
| 当前 VNic | Attachment.current_vnic_name 或 manual binding；唤醒地址投影与当前 SG target |
| 已登记 Node | network_virtual_switches 冻结成员 NodeName；只唤醒这个 switch |
| Default Egress EIP/Snat | 固定 namespace/eip_provider_name/snat_provider_name，排除 retired |
| SecurityGroup | payload 的 ClusterID/Namespace/ProviderName |
| EIP/VNic 的 SG 应用变化 | 当前 EIP/manual VNic/Attachment 管理映射到 network_security_group_targets |

删除为审计订阅的 NAT、Service、EndpointSlice、Deployment/ReplicaSet、ConfigMap、ServiceCIDR、provider daemon、镜像、CRD。Node/Pod 仅用于上表的管理与当前地址用途。GatewayClass 只用于显式 capabilities 查询，不参加后台证明。

watch 初始 LIST/relist 是 informer 的正常通知实现，不构成完整性证明。稳定任务 park=infinity，事件唤醒；未完成操作用 ObserveEvery 重读自己的目标；读请求错误按 retry 调度。启动调用 `WakeManagedTasks` 重排六类非终态已有 DB 任务，不建立集群反向产品发现流程。

## Finish 的具体提交规则

tenant/platform/attachment 保留现有 requested_generation/processed_generation；virtual-switch/default-egress/SG 补同形计数。这只是保证 Res 不丢执行期间的新通知，不传给 provider、不与 KCN generation 比较。

所有 Finish 在现有 version/owner/lease_epoch/lease_until 校验通过后使用同一事务 now，按以下次序写任务：

```sql
processed_generation = greatest(processed_generation, :claimed_generation),
observation_error = CASE
  WHEN :observation_error <> '' THEN :observation_error
  WHEN :observed THEN '' ELSE observation_error END,
retry_not_before = CASE WHEN :backoff THEN :now + :delay
  ELSE '1970-01-01 UTC'::timestamptz END,
next_run_at = CASE
  WHEN :terminal THEN 'infinity'::timestamptz
  WHEN :backoff THEN :now + :delay
  WHEN requested_generation > :claimed_generation THEN :now
  WHEN :park THEN 'infinity'::timestamptz
  ELSE :now + :delay END
```

Attachment 用 next_check_at。API 失败也可确认这个通知已执行一次，后续依持久 retry 重试。失败不能清空业务值；成功才用 now 更新该投影的 observed_at。

`Finish/finishPlatform/FinishAttachment` 删除 Covers 检查；`CompleteAttempt/CompletePlatformAttempt/FinishDefaultEgressOperation` 使用 OperationReason。`FinishSecurityGroup` 仅当 operation.CompletedAt==nil 才更新操作 JSON，已完成操作的 UpdatedAt 也不动。

`BeginProviderMutation/BeginPlatformMutation/BeginDefaultEgressMutation` 允许同一未知 UID 创建命令重试自己的 pending create/create_eip/create_snat；UID 已保存后的目标丢失不走重建。

源码入口：[kc_observation.go](https://github.com/zhangzhe-ctrl/ani-resource-service/blob/5dc539d92e3150a3593bde3668082753aa4fdebe/internal/data/network/kc_observation.go)、[observation.sql](https://github.com/zhangzhe-ctrl/ani-resource-service/blob/5dc539d92e3150a3593bde3668082753aa4fdebe/internal/data/network/queries/observation.sql)、[worker.sql](https://github.com/zhangzhe-ctrl/ani-resource-service/blob/5dc539d92e3150a3593bde3668082753aa4fdebe/internal/data/network/queries/worker.sql)。

# 5. VPC、Subnet、VNic 方法级改动

## VPC / Subnet

| 文件/方法 | 固定修改 |
| --- | --- |
| `data/network/kc.go: binding` | 仅取 Res binding 与管理归属；状态读不再以 Namespace 生命周期检查重算 Ready |
| `observeDirect` | 精确 GET binding 的 GVR/namespace/name；404 直接 Exists=false；删除 hasDependencies |
| `inspectResource / inspect` | 只投影原 UID、Ready 三态、StatusReason、Subnet IP ranges；不查询父对象 |
| `inspectResource` 内 spec 校验 | 移除 cidrBlock/ipVersion/allowedNamespaces/routeTable/policyRoutes 及 Subnet type/gateway/gatewayIP/underlayConfig/excludeIPs/enableDHCP/dnsServers/natOutgoing 的读回全等验收；自己的输入验证与声明提交保留 |
| `inspectResource` 内状态合取 | 删除 metadata.generation>0、observedGeneration 相等、router/switch/gatewayPort 非空、Valid&&Initialized&&Ready；status.subnets/eips/v4usingIPs 不再生成删除阻塞 |
| `subnetIPRange` | 直接输出 v4availableIPrange/v4usingIPrange，不受 generation 二次过滤 |
| `EnsureVPC/EnsureSubnet/ensure` | 固定名创建、AlreadyExists 精确读、管理标签与本次声明步骤保留；不审计完整默认 spec |
| `parentReference` | 仅渲染已保存的父 namespace/name；删除父 ProviderUid 非空门槛；Observe/Delete 不调用 |
| `Delete` | 仅对原 UID 正常 DELETE；删除 HasDependencies/hasDependencies 和 Orphan propagation；不清 finalizer |
| `hasDependencies` | 整体删除跨 CR 读取；Res 自有引用仍在产品删除入口校验 |
| `lb_runtime_dependencies.go: currentNetworkResource / requireLBVPCProviderReady` | 整体删除，所有网络/VM/LB/egress 准入调用同步移除 |

`AcceptSubnet` 保留父产品关联、CIDR 包含/重叠、幂等与本地占用，删除 freshness/Available 前提。`DeleteVPC/DeleteSubnet` 保留 Res 子产品引用，引用释放后不再向 KCN复查。`networkManagementOpen` 仅检查本地未 deleting/deleted，不解释 Reason、Ready 或 ObservedAt。

## VNic

| 方法 | 固定修改 |
| --- | --- |
| `kc_vnic.go: inspectVNic` | 单独投影本 VNic；移除父 Subnet 读回、VETH/allocate_ip_with_dhcp/owner链健康验收 |
| `vnicAddressConditions` | 只读父 VNic Ready；删除 Valid=False 的 addressconflict 字符串推断 |
| `inspectVNicAddressFacts` | 替为纯 `vnicFacts`：spec.macAddress、status.primaryIP、status.ips；不 GET VNicIP，不对父/子 IP map、RequestedIP/MAC 与生成结果作成功裁决 |
| `inspectRequestedVNicIP` | 退出 Observe；仅显式声明提交时核自己的目标身份，不读 child condition |
| `manualVNicObject / ensureRequestedVNicIP` | 保留 Res 接受的显式 RequestedIP 声明创建及 ownerReference；requested-ip-uid 只记录自己声明已提交 |
| `EnsureVNic` | 删除父 ProviderUid 非空、父 Subnet 现场 Ready/终止态预检；已提交自己的声明步骤后直接听父 Ready |
| `deleteVNic` | 只 DELETE 原 VNic，依已有 ownerReference 和正常 GC；原 VNic 404 即完成，不再 GET/DELETE RequestedIP child 或自动后代 |
| `vnics.go: AcceptVNic` | 保留用户 RequestedIP/MAC 格式、Subnet 范围、显式请求唯一性；删 currentNetworkResource、父 UID 和现场 KCN 预检 |
| `ListVNics` | Bindable 只表达未 deleting/deleted、未占用、同租户/Subnet/placement；删除 state=available 与 MAC/IP 非空条件 |
| `ReserveVNic` | 只办 Res 当前消费者排他预约；删除 MAC/IP 非空、现场 Ready、pending/父 UID 预检、现场地址必须等于旧 DB 的判断 |
| `ReleaseVNicBindings / ApplyVNicBinding` | 保留自己的 reservation/revision；不等待自动 VNicIP/Nic |
| `data/network/worker.go: Finish` | 删除二次查询生成 MAC 冲突并改 Failed、丢弃 facts 的整段逻辑 |

新增迁移重建 `network_vnics_manual_mac_unique`：键为 `(tenant_id, subnet_id, lower(requested_mac))`，保留未删除记录的原业务范围，另加 `requested_mac <> ''`；删除键中 `COALESCE(NULLIF(requested_mac,''), mac_address)`。自动 mac_address 是 KCN 输出，直接保存。

`lockVNicSGSelection` 继续保护 Res 当前选择与 revision；`syncVNicSecurityGroups` 只 PATCH spec.securityGroups/管理注解，保存 configured 并通知 SG worker。删除逐 SG appliedVNics 反查和同步 AppliedIDs 伪造。

源码入口：[kc.go](https://github.com/zhangzhe-ctrl/ani-resource-service/blob/5dc539d92e3150a3593bde3668082753aa4fdebe/internal/data/network/kc.go)、[kc_vnic.go](https://github.com/zhangzhe-ctrl/ani-resource-service/blob/5dc539d92e3150a3593bde3668082753aa4fdebe/internal/data/network/kc_vnic.go)、[vnics.go](https://github.com/zhangzhe-ctrl/ani-resource-service/blob/5dc539d92e3150a3593bde3668082753aa4fdebe/internal/data/network/vnics.go)、[0030 MAC 索引](https://github.com/zhangzhe-ctrl/ani-resource-service/blob/5dc539d92e3150a3593bde3668082753aa4fdebe/migrations/0030_vnic_requested_addresses.sql)。

# 6. Attachment 管理关系与地址投影

## 生命周期固定为管理事实映射

| 当前消费者 | Attachment | 地址处理 |
| --- | --- | --- |
| open 尚无当前 Pod，或 stopped | Reserved | 清当前地址，保留选择和预约 |
| open/running，当前 Pod 已登记且属于当前 submission/slot | Attached | 独立读取普通地址；缺地址或读错不撤销 Attached |
| closing | Releasing | 撤回发布地址，等待本次管理关闭 |
| closed 且 FinalizationID 对应本次释放 | Released | 清地址、释放本地引用、结束任务 |
| 当前消费端读取失败 | 保留上次管理状态 | 同一消费者的旧地址保留，记读错并重试 |

## 方法和字段

| 位置 | 固定修改 |
| --- | --- |
| `NewAttachmentWorker` | 删除 AttachmentProvider 依赖；只注入 repository、当前 consumer reader、独立地址 reader |
| `Step/stepManaged` | 消费当前管理事实；删除 ObserveAttachment、VerifiedCurrent、PendingCandidate、HasDependencies、Proof、历史关系累积、closed 后 collection、released 后检查 |
| `validConsumer/validManagedConsumer/validVMConsumer` | 只核 scope、可解码协议、当前 submission/revision/state/FinalizationID；不核祖先链与历史 UID 完整集合 |
| `CompareConsumerSnapshot` | 仅当前 scope 与 revision 应用，旧 revision 不能覆盖新管理事实；不比较历史集合或 hash |
| `ConsumerSnapshotHash/frozenConsumerSnapshotHash/releasedManagedClosure` | 删除 |
| `ManagedProtocolRecoveryMatches/ClosedProtocolRecoveryProof/VMClosedProtocolRecoveryMatches/ClosedProtocolRecoveryMatches/closedProtocolSnapshotMatches` | 删除 |
| `ClaimAttachment/FinishAttachment` | 保留 row version/lease/当前 revision；删除 Covers、protocol recovery、relations 审查；Released 永久退出任务 |
| `managedAttachment/persistConsumerSnapshot` | 仅保存当前轻量快照，不再保存 SnapshotHash/RootController 证明 |
| `confirmManagedAttachment/ConfirmAttachment` | 当前登记成功即 Attached，不先置 reserved 等 provider 二验 |
| `ReleaseAttachment` | 同 FinalizationID 幂等；消费当前 closed 即 Released，否则 Releasing；重放不得在旧 FinalizationID 分支提前返回而错过后来 closed |
| `PrepareAttachment` | 删除 freshness/currentNetworkResource/ProviderUid 非空；根据确定名称构造 plan |
| `GetAttachment` | 直接映射当前管理状态和四个地址字段；不因地址空动态改 Reason=VM_NETWORK_ADDRESS_NOT_CURRENT |

新 Attachment 的 `BindingRevision` 固定为 Res BindingID 不透明令牌，替代 BindingID:ProviderUID；旧字符串在既有幂等重放中原样返回，不解析其中 UID 作新 gate。

| 类型 | 字段整改 |
| --- | --- |
| AttachmentWork | 删 Requirement、RequireFreshRelations、Relations、ConsumerPodUIDs、ProtocolBlocked；留当前 Attachment/ConsumerSnapshot/Owner/Epoch/Now，加 ClaimedGeneration |
| AttachmentProgress | 删 Proof、Relations、ProtocolBlocked、ProtocolRecovered；留当前快照/State/Reason/Pod/FinalizationID/Observed/排程，加 ObservationError |
| AttachmentProvider / AttachmentObservation | 整体删除，不保留改名后的 VerifiedCurrent/HasDependencies |
| ConsumerSubmission | 留 ProtocolVersion、当前 scope/Generation/SnapshotRevision、State/DesiredRunState、FinalizationID/ClosedAt、CurrentPods、ManualVNics；删内部 RootController/Controllers/RetiredPods/ControllerUIDs |
| Attachment | 去 SnapshotHash/RootController 当前合同依赖；保留现有 CurrentVNicName/CurrentVNicUID/CurrentMACAddress/CurrentIPs 和管理 revision |

## 地址来源定死

新增窄端口 `AttachmentAddressReader.ObserveAttachmentAddresses(ctx, Attachment, ConsumerSubmission) (AttachmentAddressObservation,error)`；结果仅四个当前地址字段，不含 Ready、AddressEligible、HasDependencies、Proof。

1. 只 GET 当前消费者登记的 Pod namespace/name；UID 已登记时用于确认仍是这个消费实例。single_pod 使用已 Confirm 的 PodName/PodUID。不 LIST 猜测，不追 controller/Node/历史 Pod。
2. Container 用 Pod.status.podIPs，兼容 podIP；VM 用当前 Pod 的 `k8s.v1.cni.cncf.io/network-status` 默认网络项 ips/mac。
3. 当前 VNic 名取已存在的 `networking.kubercloud.com/vnic` 主网注解；无 namespace 使用当前 namespace。无名字就没有 VNic fallback，不拼接 auto 名。
4. MAC/IP 尚未给出且有明确 VNic 名，才 GET 这一个 VNic，直接补 spec.macAddress/status.primaryIP/status.ips。Pod 输出优先，不比较二者一致性，不查 VNicIP 或 Ready。
5. 同一当前消费实例读错保留旧值；明确 stopped/closing/closed 或当前 Pod 更换时撤回旧地址。地址错误不影响生命周期、VM 操作或现有 LB 后端。

DB 用 `current_vnic_name/current_vnic_uid/current_mac_address/current_ips` 四列替换 provider_relations。`vmCurrentAddresses/vmAddressIdentity/vmCurrentAddressRelations/containerAddressRelations/attachmentRelations` 删除旧 proof/TTL 实现，换成当前消费者与四列的直接映射。VMCurrentAddresses 兼容输出的 LauncherPodUID/VMIUID 取已登记管理引用，不重新 GET VMI。

`kc_attachment.go` 删除 attachmentRelation 运行结构及 observeAttachment；`kc_attachment_managed.go` 删除 verifyManagedPod/managedControllersExist/acceptedManagedControllerReplacement/managedNics；`kc_attachment_vm.go` 删除 vmPrimarySpecMatches/vmLauncherMainNetworkMatches/verifyVMPod。尚用于声明或管理列表的 kcRef、labels、GVR、listScoped 等纯工具迁出保留，不留审计分页证明。

源码入口：[attachment_worker.go](https://github.com/zhangzhe-ctrl/ani-resource-service/blob/5dc539d92e3150a3593bde3668082753aa4fdebe/internal/biz/network/attachment_worker.go)、[attachment_managed.go](https://github.com/zhangzhe-ctrl/ani-resource-service/blob/5dc539d92e3150a3593bde3668082753aa4fdebe/internal/biz/network/attachment_managed.go)、[attachment.go](https://github.com/zhangzhe-ctrl/ani-resource-service/blob/5dc539d92e3150a3593bde3668082753aa4fdebe/internal/data/network/attachment.go)、[attachment_vm.go](https://github.com/zhangzhe-ctrl/ani-resource-service/blob/5dc539d92e3150a3593bde3668082753aa4fdebe/internal/data/network/attachment_vm.go)。

# 7. Instance 与 VM 调用方同步整改

Network 模块删掉 proof 后，Compute 调用方也必须停止等待旧证据，否则实例仍会被旁路逻辑卡住。

## Instance

| 方法/字段 | 改法 |
| --- | --- |
| `data/instance/network.go: ValidateSelection` | 仅 Res 管理选择检查，删除 Available/ObservationStale 判据 |
| `NewInstanceNetwork / InstanceNetwork.freshness` | 删除 TTL 构造参数和私有字段 |
| `InstanceNetwork.Observe` | 返回 Attachment 管理状态及普通地址；删 time.Since 门槛，地址不决定 Confirmed；Mode 取管理选择，不从 auto- 名称猜测 |
| `InstanceNetwork.Confirm` | 登记当前 attachment/submission/revision/Pod；Network ACK 即管理交付完成 |
| `biz/instance.NetworkPort.Release` | `error` 改为 `(bool,error)`；bool 只表示关系已 Released |
| `InstanceNetwork.Release / Worker.releaseNetwork` | 消费 Release 响应，done=true 即 NetworkState=released、Cleaned=true；无物理清场阶段 |
| `NetworkRuntime` | 保留 Cleaned（改为管理释放完成）；删除 ReleaseStarted/ReleaseAcknowledged、历史 Controllers/RetiredPods/RootController 证明字段 |
| `ReconcileSubmission / InstanceNetworkConsumer.GetSubmission` | 当前 Pod 来自 Instance 已有成功观察；只保存/返回当前管理快照，不累计历史链 |
| `initialNetworkSubmission/networkIntent/saveNetworkSubmission/mergeNetworkResult/CloneNetwork` | 同步轻量字段；同 FinalizationID 合并 Cleaned；保持单调 revision 与现有持久化 CAS |
| `Worker.observeNetwork/confirmNetwork`、包级 `confirmNetworkInterface` | 仅等待自己的管理登记与当前配置 revision；无地址、读取地址失败、TTL 不令 Running 回 pending |
| `lifecycle_worker / lifecycle.go / OwnerCleanupReady` | waiting_network 仅管理交付；Cleaned 只确认本地引用释放 |
| `ReserveNetworkInterface / Notebook.SetNetworkInterface` | 删除 ProviderUID 非空前提；确定 ProviderName 即可提交所选网卡声明 |

`biz/instance/network_worker.go`、`biz/instance/network_interfaces.go`、`biz/instance/lifecycle_worker.go`、`data/instance/lifecycle.go` 和 `data/vm/network_interfaces.go` 里的 `Network.Reason == ""` 完成门槛明确删除。只检查当前管理身份、Attached、当前 Pod 与配置 revision，旧诊断 Reason 不阻塞确认。

`ReleaseNetworkInterfaces` 继续释放自己的 manual VNic reservation，不删除用户 VNic，也不等待 KCN 自动子对象。

## VM

将 `biz/vm.NetworkPort.Observe(ctx,VM,Observation)([]Address,error)` 明确拆成：

```go
Confirm(context.Context, VM, Observation) error
CurrentAddresses(context.Context, VM) ([]Address, error)
Release(context.Context, VM) (bool, error) // 原签名保留，实际消费返回值
```

| 方法/字段 | 改法 |
| --- | --- |
| `data/vm.Network.GetVMNetworkSubmission` | 只 repo.Get 并返回已保存当前管理事实；零 K8s 请求、零写库；删除 native.Observe、历史 Controllers/RetiredPods 累积、hash、UpdateNetwork 副作用 |
| 新 `biz/vm.ReconcileNetworkSubmission(v *VM,x Observation) bool` | 纯函数使用 VM 自己本轮成功观察和管理意图更新当前快照；不持 K8s/KCN client |
| VM `Save/Checkpoint/SaveObservation` | 保存上述快照与 revision；变化后通知 Attachment；先持久化，再在后续办理 Confirm/Release；native 读取错误不伪造 closed |
| `NetworkPlan.Consumer` | json.RawMessage→*NetworkConsumerState；仍在原 JSON 位置存储；字段只为 State、DesiredRunState、FinalizationID、ClosedAt、CurrentVMI、CurrentLauncher |
| `NetworkPlan.SnapshotRevision` | 保留；仅当前管理事实改变时增加；scope/ManualVNics 从现有 VM/NetworkPlan/NetworkInterfaces 映射，不复制 |
| `Network` 的 native 字段及 `NewNetwork(...,n NativePort)` | 从 Network adapter 删除；VM Compute 自己的 NativePort 保留 |
| `Network.Confirm/CurrentAddresses` | 管理确认和地址读取独立；无 len(addresses)>0、停止时旧地址非空、NAD 重验的 gate |
| `Network.Release` | closed 时直接返回 Attachment 是否 Released；删除原一律 false 的返回 |
| `CurrentNetworkInterface/Worker.confirmNetworkInterface` | 去地址 VMI/Launcher UID 全链、指定 IP 必须出现在生成结果等验收；保留用户选择 AppliedRevision、当前运行代次和管理 ACK |
| `Worker.power` | start/stop 不受地址读取错误或地址空集合条件影响；VM 自己已停止就撤回本消费者地址 |
| `Worker.remove` | VM 自己发布 closed 后 Release；管理释放完成即继续释放本地 reservation/SG 引用 |
| `ReserveNetworkInterface/Native.SetNetworkInterface` | 确定所选 ProviderName 即可提交，不要求网卡 UID/MAC/IP 先返回 |

`ConfigureVMNetworks/VMNADConfigDigest/validateVMSinglePluginNAD` 留在 Res 自管 NAD 准备与写入阶段。`verifyVMNetworkResources` 删除；`ResolveVMNetworkSelection` 不查 VPC/Subnet Ready/ProviderUid；`SetVMNetworkResolver` 删 vmAddressFreshness；观察、关闭、释放不再复验 NAD 字节与原 UID。

当前 VM JSON 升级从旧 State/DesiredRunState/FinalizationID/ClosedAt/CurrentPods 提取；当前 Pod.Owner 映射 CurrentVMI；未知历史字段忽略，下一次保存新结构。Instance 已有 Cleaned=true、FinalizationID、当前 Pod、确认 revision 原样保留。

源码入口：[Instance network](https://github.com/zhangzhe-ctrl/ani-resource-service/blob/5dc539d92e3150a3593bde3668082753aa4fdebe/internal/data/instance/network.go)、[VM network](https://github.com/zhangzhe-ctrl/ani-resource-service/blob/5dc539d92e3150a3593bde3668082753aa4fdebe/internal/data/vm/network.go)、[VM worker](https://github.com/zhangzhe-ctrl/ani-resource-service/blob/5dc539d92e3150a3593bde3668082753aa4fdebe/internal/biz/vm/worker.go)。

# 8. Egress、地址池、Default Egress 与 VirtualSwitch

## Egress 与平台方法

| 文件/方法 | 动作 |
| --- | --- |
| `kc_egress.go: resolveEgress` | 只用 DB 声明渲染 spec；不展开物理依赖、父 Ready、switch 健康 |
| `egressGVR/egressCRKind/resourceEndpoint/platformBinding/egressBinding/inspectEgressIdentity` | 保留明确目标定位和本层归属 |
| `matchesEgressSpec` | 仅用于本服务写入字段是否还需提交；KCN 默认字段、生成地址不造成冲突 |
| `egressReadSet/readEgressSet/readEgressSetOnce/errEgressAuditInvalid` | 整体删除；无联合快照替代 |
| `observeEgress/inspectEgressSet` | 改为目标 GET + `inspectEgressObject`，按第 1 节直接映射主状态 |
| `egressPrerequisites/publicPoolReady/vlanControlReady/eipAllocated/bindingApplied/eipBindingConflict/eipBindingConflictAllowed` | 删除跨资源运行检查；VLAN Initialized 留为简单字段读取 |
| `providerImages/providerNodesReady/emptyBoundResource` | 删除运行用途及无剩余调用的实现 |
| `EnsureEgress/UpdateEgress/deleteEgress` | 只办理自己的确定声明、UID/RV 和 pending；删除 read-set/physical preparation/反向绑定条件 |
| `AcceptEIP/AcceptSnat/AcceptLoadBalancer` | 删除 poolReady/freshResource/VPCProviderReadiness、EIP 现场复核；保留 tenant/scope/placement、Res claim、输入规则 |
| `AcceptPlatform/checkParent` | 只校验自己的父记录/配置与 CIDR 分配；不调用 ValidatePublicPool/ValidateIntranetPool |
| `poolReady` | 删除；管理策略改用 `poolAllocationAllowed`，只读 scope/allocation_enabled/default/retired/deleting |
| `acceptPoolChange` | Record 分支只存管理员 verification JSON；删分支前 topology 查询及分支内镜像/时间/状态核对 |
| `DeletePlatform/acceptManagedGateway/finishManagedPoolCleanup` | 保留本地 pool/gateway/VLAN 引用与各自声明生命周期；逐个原 CR NotFound 即结束自己的删除 |
| `claimPlatform/finishPlatform/snatWork/AdvanceSnat` | 删除 parent Ready、Proof、SavePlatformImages、TargetGeneration；applied_enabled 取 SNAT phase |
| `kc_node_network.go: ValidatePublicPool` | 删除 KCN Subnet/Node IP/PodCIDR/ServiceCIDR 扫描；输入 CIDR/gateway/excludedIPs 与 Res 自有分配冲突校验保留 |
| `kc_intranet.go` | 留 renderIntranetPool；删 networksCover/intranetRoutesConfigured/controllerDefaultGateway/intranetGatewayReady/intranetInfrastructureReady/addressPoolReady/allocatedEIP/ValidateIntranetPool |

`GetIntranetPoolDefaults` 唯一路径：显式管理员查询只 LIST kcn-system VPC，选择 `status.default=true`，返回 name/UID；0 项无默认引用，多项为选择歧义。不要求 Ready，不读 controller Pod 参数、ConfigMap、ServiceCIDR，不新增部署配置。intranet_networks 返回 []。CreateIntranetPool 可直接提交管理员明确 name/UID，defaults 不是必经调用。

`IntranetNetworks` 保留为可选管理员记录；NormalizeIntranetPool 取消必填与基础设施覆盖校验，现有 DB scope CHECK 取消数组非空要求。RecordPublicPoolVerification/RecordIntranetPoolVerification 继续真实存档，不推断管理员材料已获 Res 实测认证。ValidatePoolVerification/ForScope 只保留材料格式、长度、时间戳与归属，删除未过期才允许操作的判断。

| 结构 | 删除 | 保留 |
| --- | --- | --- |
| egressResolved | lbID、vpcCIDR、virtualSwitch、switchDevice、preparePhysical、运行 refs/旧地址验收值 | binding/target/spec，渲染所需 pool/snat/设备管理配置 |
| EgressWorkSpec / EgressAppliedFacts | TargetGeneration；ProviderImages | EIPID/PoolID/DesiredEnabled/Platform；Address/AppliedEnabled |
| PlatformResource | TopologyFingerprint/ObservedProviderImages | 管理配置/开关/ConfigRevision/Verification |
| PreparedDeviceInventory | CollectedAt 与 TTL 用途 | 管理员选择的 Items、冻结配置 Fingerprint |
| EgressInfrastructure/IntranetPoolInfrastructure | ValidatePublicPool/ValidateIntranetPool 预检接口 | 现有窄查询接口 IntranetPoolDefaultsInfrastructure/VirtualSwitchInfrastructure |

`NewEgress` 收窄为 `NewEgress(repository, defaults IntranetPoolDefaultsInfrastructure, authorization, key, now)`；`UseEgressInfrastructure` 改为 `UseVirtualSwitchInfrastructure`。now 只用于管理员记录/管理操作，不保留 freshness。

## Default VPC Egress

| 方法/字段 | 固定修改 |
| --- | --- |
| `poolSet/ValidateDefaultVPCEgress/DefaultEgressPoolProof` | 删除 |
| `identityObject/readOwned` | 仅固定 default VPC 引用身份与自己 EIP/SNAT 原 UID；无 Ready 复核 |
| `observe` | 精确 GET 自己 EIP/SNAT，投影地址和 phase；无 NAT/Service 扫描、反向 Unbound、Hash |
| `MutateDefaultVPCEgress/Worker.Step` | EIP、SNAT 顺序提交，不等待 EIPReady；删除 SNAT→EIP 各自确认原对象不存在 |
| `CreateDefaultVPCEgress/FinishDefaultVPCEgress` | 留固定名称、pool_revision、原 UID、lease、pending；删 admission hash/时效/完整绑定证明 |
| DefaultEgressObservation | 删 EIPReady、Unbound、Hash、TargetGeneration、ObservedAt；保留 EIPExists/SnatExists/EIPUID/SnatUID/Address/AppliedEnabled；Ready *bool 仅映射 SNAT 对本意图的结果 |
| DefaultVPCEgress / DefaultEgressWork | 删 TargetGeneration/PoolProviderUID；留 PoolRevision 与已接受管理配置 |

platformSystemEIP 继续表达 system 管理范围，租户释放权限不变。

## VirtualSwitch

`VirtualSwitchInventory/virtualSwitchInventoryMode` 只为管理员选择成员提供数据并冻结自己的成员名/UID。`validateSwitchMembers` 只校验已登记成员及自己的声明归属，不要求当前全体节点集合与过去一致。

`InspectVirtualSwitchNode/inspectSwitchNodeObject` 精确 GET 已登记 Node；`PatchVirtualSwitchNode/patchVirtualSwitchDeletionNode` 保留自身 label/annotation、UID/RV、lease 与未知响应恢复。`inspectSwitchNodeCleanup/switchNodeCleanupConfirmed` 只看本服务写入字段是否已移除，不等 KCN feedback 清空。

删除 `virtualSwitchDeviceState/publicPoolChassisReady/inspectVirtualSwitchDeviceState/virtualSwitchObservationProof/switchWiring/switchDaemonReady/virtualSwitchObservationTime/freshVirtualSwitchObservation`。`nodeReady` 删除网络运行用途；`nodeNetworkObjects/nodeNetworkFromView` 删除 audit/续鲜路径。首次选择只读取所需 Node 名称/UID，不夹带 ConfigMap、daemon 或 chassis 验证。`acceptSwitchVlan` 去一分钟时效和健康门槛，保留自己的成员配置、device/group 独占。

`VirtualSwitchNode.LastObservedAt` 删除；NodeName/NodeUID/Priority/Allocated 与 ResourceVersion/OriginalValue/OriginalOwner/OriginalPresent/Prepared/Applied/Unknown/DeleteFenced 留作自己的配置和 PATCH 恢复。BindingID/BindingVersion/MembershipFingerprint 留作管理配置版本。

源码入口：[platform.go](https://github.com/zhangzhe-ctrl/ani-resource-service/blob/5dc539d92e3150a3593bde3668082753aa4fdebe/internal/data/network/platform.go)、[kc_intranet.go](https://github.com/zhangzhe-ctrl/ani-resource-service/blob/5dc539d92e3150a3593bde3668082753aa4fdebe/internal/data/network/kc_intranet.go)、[default_vpc_egress_kc.go](https://github.com/zhangzhe-ctrl/ani-resource-service/blob/5dc539d92e3150a3593bde3668082753aa4fdebe/internal/data/network/default_vpc_egress_kc.go)、[kc_virtual_switch.go](https://github.com/zhangzhe-ctrl/ani-resource-service/blob/5dc539d92e3150a3593bde3668082753aa4fdebe/internal/data/network/kc_virtual_switch.go)。

# 9. 安全组声明与应用结果

SG 声明直接看自身 Ready。绑定结果直接看目标 `EIP.status.appliedSecurityGroups []string` 或 `VNic.status.appliedSecurityGroups map[string]int64`。ID 与 provider_name 的翻译来自 Res DB，不从反向成员重新证明。

| 文件/方法 | 固定修改 |
| --- | --- |
| `kc_security_groups.go: securityGroupReady` | 三态读取 Ready；删除 Valid 和 allowSameGroupTraffic 存在/false 的合取；原生 observedGeneration 只确认本 SG 当前更新，不改其他资源状态 |
| `applySecurityGroupResource` | 留自己 CR 的 GET/Create/Patch/Delete 与 UID；删删除前 appliedVNics/appliedEIPs 计数闸门；securityGroupSpecEqual 只决定自己字段需不需要 PATCH |
| `applyEIPSecurityGroups` | PATCH 目标 EIP.spec.securityGroups；直接读取该 EIP.status.appliedSecurityGroups；删除全 SG GET/LIST、Ready/spec 比较、反向 appliedEIPs、gateway/node/disabled 合取 |
| `eipReadyForSecurityGroups/eipSecurityGroupMembershipsRemoved` | 删除；EIP 原对象已不存在时结束自己的绑定关系 |
| `ObserveSecurityGroupConsumer/observeSecurityGroupConsumerPods` | 由当前 Attachment/manual binding 定位 namespace/name，只 GET 当前 VNic；UID 已知则使用原目标身份，UID 未知不阻止读取；无当前 VNic 返回 configured/AppliedIDs 空 |
| `observeRetiredSecurityGroupVNics/ObserveConsumerSecurityGroups` | 删除历史关系清场与反向成员核对 |
| `security_groups.go: resolveSecurityGroups` | 保留租户/namespace/存在/未删除/规则组合；删除 State==Available 前提 |
| `SyncSecurityGroupBinding` | 保存当前 desired IDs/revision/幂等与引用，不用历史 AppliedIDs 持有旧产品引用 |
| `ConfirmSecurityGroupBinding/confirmSecurityGroupBinding` | 删除事务内外部 Observe；只确认本消费者配置已提交，写 configured，释放不在 desired IDs 的旧引用，ScheduleSecurityGroup |
| `ReleaseSecurityGroupBinding` | 当前消费方结束就 released，释放本地引用并停任务；不等物理成员清空 |
| `kc_vnic.go: syncVNicSecurityGroups` | PATCH 后 configured+入队；删除 SG appliedVNics 反查；不能立刻填 AppliedIDs |
| `ClaimSecurityGroup/FinishSecurityGroup/ApplySecurityGroup` | 复用现有 worker 支持 security_group/eip/container/vm/vnic；hardcoded eip 改 work.TargetType；消费方分支只读当前目标状态 |

`SecurityGroupResult` 增加 Observed、ObservationError、OperationReason；保留 ProviderUID/State/Reason/AppliedIDs/Complete。读取错误不改旧 State/Reason/AppliedIDs/ObservedAt；completed operation 连 UpdatedAt 都不再写。Complete 只表达本次管理操作完成。

`network_security_group_targets.target_type` 已允许 vnic，不重复迁移；扩展的是 reconciliation 表 CHECK。使用准确查询 `ScheduleSecurityGroup/ClaimSecurityGroupTask/LeaseSecurityGroupTask/FinishSecurityGroupTask`，接入 ClaimedGeneration/observation_error/retry_not_before；删除 `ListSecurityGroupConsumerRelations`。

Attachment 的当前 VNic 变化通知对应 container/vm SG task；manual VNic 与 EIP watch 按现有管理映射通知对应 target。已 deleted/released 不再每 24 小时扫描。

Instance/VM 的安全组配置命令提交后只做管理 Confirm。`Notebook.SetSecurityGroups`、`Native.SetSecurityGroups` 保留自己明确负责的 Notebook/VM 模板与当前对象注解 PATCH、UID/RV；删 PATCH 后再遍历祖先和所有 Pod 的全链同一性复验，使用 Compute 已有当前引用。没有当前 VNic 不妨碍实例/VM 创建、启停、删除。

源码入口：[kc_security_groups.go](https://github.com/zhangzhe-ctrl/ani-resource-service/blob/5dc539d92e3150a3593bde3668082753aa4fdebe/internal/data/network/kc_security_groups.go)、[security_groups.go](https://github.com/zhangzhe-ctrl/ani-resource-service/blob/5dc539d92e3150a3593bde3668082753aa4fdebe/internal/data/network/security_groups.go)、[security_group_worker.go](https://github.com/zhangzhe-ctrl/ani-resource-service/blob/5dc539d92e3150a3593bde3668082753aa4fdebe/internal/data/network/security_group_worker.go)。

# 10. LB 只处理明确声明组件

## 原生组件条件

| 组件 kind | GVR | 本次采用的原生状态 |
| --- | --- | --- |
| envoy_proxy | gateway.envoyproxy.io/v1alpha1/envoyproxies | 本身没有 Ready/Accepted 契约；自己的声明已提交即可，不查看生成 deployment |
| gateway | gateway.networking.k8s.io/v1/gateways | Accepted、Programmed；仅自己的 listener 名下 Accepted、Programmed、ResolvedRefs |
| backend | gateway.envoyproxy.io/v1alpha1/backends | Accepted |
| route | gateway.networking.k8s.io/v1/httproutes | 自己 Gateway parent/controller 下 Accepted、ResolvedRefs |
| tcp_route / udp_route | gateway.networking.k8s.io/v1alpha2/tcproutes、udproutes | 同 route |
| policy / client_policy | gateway.envoyproxy.io/v1alpha1/backendtrafficpolicies、clienttrafficpolicies | 自己 Gateway ancestor/controller 下 Accepted |
| patch_policy | gateway.envoyproxy.io/v1alpha1/envoypatchpolicies | Accepted、Programmed |

Route/Policy 只取 `controllerName=gateway.envoyproxy.io/gatewayclass-controller`、本 Gateway 的 group/kind/name/namespace；Route 还匹配本 listener sectionName。省略可默认的 ref 字段按 API 默认值解释。多余 status 项不判失败。

`lbConditions/lbObjectReady` 返回 *bool。原生 observedGeneration 用于确认本次目标版本被处理：旧一代 True 不提前推进 applied_version，未报告继续当前 operation，但不把旧稳定状态改成 Failed。没有任何生成物条件参加合取。

## 方法与类型

| 位置 | 固定修改 |
| --- | --- |
| `resolveLB` | 只取 DB 管理引用/组件/后端；删除 VPC/Subnet/EIP health/pending 合取 |
| `lbDesiredObject/lbComponentGVR` 和 render helpers | 保留 Res 的 Gateway/Route/Policy/Backend/EnvoyProxy 声明 |
| `lbInspect/lbProxyComparableSpec` | 只比较本服务写入字段和 configuration-version，不比较整个默认 spec |
| `ObserveLoadBalancer/observeLoadBalancerOnce` | 精确 GET 已知组件；删除 audit 重试和全量 LIST |
| `loadBalancerCriticalObservation/errLBAuditInvalid` | 删除 |
| `kc_load_balancer_generated.go` | 整个删除：lbGenerated、lbServiceConditions/Matches/Targets、lbEIPBound、lbEIPBindingConflict、lbAddressConflict 等全部退出运行 |
| `kc_load_balancer_eip.go: readEIPLoadBalancer` | 删除反向 EIP/LB 证明 |
| `lbFind/lbOwnedBy/lbOwnerAPIVersion/lbIPInRange` | 删除已无运行消费者的物理验收 helper |
| `validateLBCertificate` | 留输入/引用权限校验；运行时证书有效性听 listener.ResolvedRefs，不反复读 Secret/生成证书结果 |
| `MutateLoadBalancer/stepLoadBalancer` | 留自身版本、UID/RV、lease、pending、未知命令恢复；删 DependenciesReady/GeneratedReady/GeneratedReleased/AddressReleased/GeneratedConflict 分支 |
| `hydrateLBWork/finishLB` | 不装载/保存 generated 表；直接组件配置完成时 BoundLBClaim；释放不等 VIP absence revision |
| `CanDeleteLBWithoutProvider/parkLBWaitingWork/reincarnateLB` | 只用自己是否已 dispatch、pending、原组件删除情况；删 generated/IPAM 条件 |

| 类型 | 删除字段 |
| --- | --- |
| lbResolved | objects 全量集合、dependencies |
| LoadBalancerWork | VIPOccupiedRevision、VIPAbsenceRevision |
| LoadBalancerMemberIdentity | VNicName、VNicUID、VNicIPName、VNicIPUID、只为再次查 Pod 的 PodName |
| LoadBalancerGeneratedResource | 整个类型 |
| LoadBalancerObservation | GeneratedConflict、VIPOccupiedRevision、VIPAbsenceRevision、Proof、Generated、DependenciesReady、GeneratedReady、GeneratedReleased、AddressReleased |
| LoadBalancer | 内部 DataPlaneState、DataPlaneObservedAt |

`LoadBalancerComponentObservation.Ready` 改 *bool，保留自己的 Conflict/ID/Identity/Exists/Matches/Deleting/ClearPending/Deleted/AppliedVersion；Matches 仅自己的字段。组件自己的 PendingAction/CreateDispatched/TargetVersion/AppliedVersion 全保留。

## 后端与删除

`lbBackendIdentity/lbMemberIdentity` 只用已保存的 Attachment 当前消费关系和 CurrentIPs；不再 GET Pod→VNic→VNicIP。DB 继续用现有 tenant_id+attachment_id+pod_uid+address，与 Attachment 当前 Submission/Generation 关联，不再新增一套 consumer 键列。

读取地址失败保留原后端；当前消费者停止/关闭、当前 Pod 替换或成功读取到的新地址才更新。Eligible 只表达当前业务成员资格，应用健康仍由用户配置的健康检查交给 controller。

配置完成由直接声明组件 owner 接受/编程后推进 configuration_state/applied_version。无 backend 的 waiting_for_backends 业务语义保留。更新移除后端：新 Route 被 owner 确认后删旧 Backend。

删除固定顺序：Route/Policy → Gateway 正常 DELETE → Backend/EnvoyProxy；逐个确认自己原对象不存在，清完自己的 pending 后释放 EIP/VIP/Subnet claim。Gateway finalizer 交原 controller 处理。retired binding 只保留自己旧 Gateway 的身份与删除记录，不再代表物理地址回收证明。

## capabilities 与安装配置

原 `KCObservation.collectLB` 随 audit 删除。显式查询固定调用链：GetPlatformNetworkCapabilities → Egress.GetPlatformCapabilities → Postgres.GetPlatformCapabilities(ctx) → KCProvider.ReadLoadBalancerCapability(ctx) → GET GatewayClass/lb-common → SaveLBCapability。

新增窄接口 `LoadBalancerCapabilityReader.ReadLoadBalancerCapability(context.Context)(*bool,Reason,error)`，用 `Postgres.UseLoadBalancerCapabilityReader` 注入。GET 在 DB 事务外；只读 Accepted。成功读取时保存当前诊断。`SaveLBCapability` 的 ready 参数可空：冲突更新用 COALESCE(本次ready,旧ready)，未报告时只更新成功读取时间与未报告原因；无旧行且未报告时，既有 bool 输出为 false 并带 PROVIDER_STATUS_UNREPORTED 表达未知。GET 错误不调用 Save，响应返回旧 Ready/ObservedAt 加本次读取错误；无旧行用未知诊断。这个查询不被 AcceptLoadBalancer 或 worker 调用。

删除 runtime ConfigureLoadBalancer、KCProvider.loadBalancerInstallation、以 installation 指针决定 watcher 启用的分支。五个安装 fingerprint/image 配置全部退出 runtime 必填校验；该 SHA 的 Envoy/Shutdown image 也没有用于声明渲染。显式 `VerifyLoadBalancerInstallation(ctx,client,expected)` 保留独立诊断，私有参数校验与临时收集不依赖 auditView，不回填业务 gate。

源码入口：[kc_load_balancer.go](https://github.com/zhangzhe-ctrl/ani-resource-service/blob/5dc539d92e3150a3593bde3668082753aa4fdebe/internal/data/network/kc_load_balancer.go)、[generated 文件](https://github.com/zhangzhe-ctrl/ani-resource-service/blob/5dc539d92e3150a3593bde3668082753aa4fdebe/internal/data/network/kc_load_balancer_generated.go)、[LB worker](https://github.com/zhangzhe-ctrl/ani-resource-service/blob/5dc539d92e3150a3593bde3668082753aa4fdebe/internal/biz/network/load_balancer_worker.go)、[capability](https://github.com/zhangzhe-ctrl/ani-resource-service/blob/5dc539d92e3150a3593bde3668082753aa4fdebe/internal/data/network/kc_load_balancer_capability.go)。

# 11. 数据库迁移与 SQL 清单

新增 `migrations/0032_network_management_simplification.sql`；不修改 0001–0031，不重写历史校验值。下列是同一次 Res 升级的完整 schema 目标。

## 删除与新增列

| 表 | DROP | ADD / 保留要点 |
| --- | --- | --- |
| network_reconciliations | evidence_hash、evidence_applied_at | observation_error；保留原通知计数、retry、lease、retired |
| network_platform_reconciliations | evidence_hash、evidence_applied_at | observation_error；保留原通知计数、retry、lease |
| network_attachments | provider_relations、protocol_blocked、evidence_hash、evidence_applied_at | observation_error；current_vnic_name/current_vnic_uid/current_mac_address/current_ips；保留原通知计数/epoch/业务字段 |
| network_attachment_consumer_snapshots | snapshot_hash、root_controller | 留当前 snapshot、snapshot_revision、confirmed_snapshot_revision、confirmed_pod_uid |
| network_provider_bindings | provider_ready、provider_observed_at、provider_observed_uid、provider_generation、provider_observed_generation | 留原 cluster/namespace/name/UID、binding、create_dispatched、pending_action/pending_since |
| network_virtual_switch_reconciliations | 无 | observation_error、requested_generation、processed_generation、retry_not_before |
| network_default_vpc_egresses | pool_provider_uid、admission_hash、admitted_at、target_generation | observation_error、requested_generation、processed_generation、retry_not_before；留 pool_revision/default VPC/自建 CR 身份/pending/lease/retired_at |
| network_security_group_reconciliations | 无 | observation_error、requested_generation、processed_generation、retry_not_before；requested_version 保留为用户配置版本 |
| network_platform_resources | provider_images | 原生资源投影与管理配置保留 |
| network_snat_bindings | target_generation | desired_enabled、nullable applied_enabled、地址和账本引用保留 |
| network_lb_generated_resources | 整表 | 不建替代 proof 表 |
| network_load_balancers | vip_occupied_revision、vip_absence_revision、data_plane_state、data_plane_observed_at | 留直接声明版本、VIP intent、claim、状态 |
| network_lb_retired_bindings | vip_occupied_revision、vip_absence_revision | 留旧原对象身份、cleanup_target_version、operation/completed_at；历史不可变约束保留 |
| network_lb_members | vnic_name、vnic_uid、vnicip_name、vnicip_uid | 留 tenant/attachment/subnet/pod_uid/address/port/weight；不新增重复消费身份列 |
| network_lb_capabilities | fingerprint、provider_images | 留 cluster_id/ready/reason/observed_at，来源改显式直接查询 |

六个任务承载表新增 `observation_error text NOT NULL DEFAULT ''`。三组新增通知字段统一为：

```sql
requested_generation bigint NOT NULL DEFAULT 1,
processed_generation bigint NOT NULL DEFAULT 0,
retry_not_before timestamptz NOT NULL DEFAULT '1970-01-01 UTC',
CHECK (processed_generation >= 0 AND requested_generation >= processed_generation)
```

四个地址列前三列 `text NOT NULL DEFAULT ''`，current_ips 为 `text[] NOT NULL DEFAULT '{}'`。不新增 KCN generation/hash/快照覆盖时间。

## 索引、约束与 JSON

- 重建 `network_vnics_manual_mac_unique`：`ON network_vnics(tenant_id,subnet_id,lower(requested_mac)) WHERE state<>'deleted' AND requested_mac<>''`。
- 重建 `network_attachment_vpc_active`，谓词仅 `state<>'released'`；现有 slot 业务唯一约束继续保留。
- 增加普通非唯一通知索引 `(cluster_id,namespace,pod_name)` 与 `(cluster_id,namespace,current_vnic_name)`，各自排除 released/空名称。
- 扩展 `network_security_group_reconciliations.target_type` CHECK 为 security_group/eip/container/vm/vnic；targets 表已在 0029 支持 vnic，不重复修改。Attachment consumer_type 的 container_instance/virtual_machine 映射为现有 SG target_type container/vm。
- `network_pool_scope_config` 只从 intranet 分支移除 `cardinality(intranet_networks)>0`；public 分支与管理引用要求保留。
- `network_public_pools.verification/verification_expires_at/intranet_networks` 保留存档，所有生命周期 SQL 不再使用其有效期作授权。
- 清理当前 snapshot JSON 的历史 RootController/Controllers/RetiredPods/ControllerUIDs/hash；不改 operation/idempotency 历史响应。
- `network_virtual_switches.nodes` 后续写入删除 LastObservedAt；保留自己的 PATCH 恢复字段和管理 fingerprint。

## Attachment 一次性数据回填

顺序固定为 ADD 四列 → 搬运旧当前输出 → DROP provider_relations/证明列 → 缩减当前 consumer snapshot。

只对仍 attached 且 pod_uid 非空的当前关系，按照旧 GetAttachment 的纯数据选择搬运：provider_relations 的实际 JSON 键为 `Kind/Namespace/Name/UID/OwnerUID/Address/MACAddress/Current/AddressEligible`。选 Current 的 VNic，OwnerUID 对应当前 pod_uid，或被 snapshot.ManualVNics 的 ProviderName/ProviderUID 明确选中。前三列使用旧数组最后匹配项的 Name/UID/MACAddress；current_ips 取这些 VNic UID 对应的 Current 且 AddressEligible 的 VNicIP.Address。

这只保存升级前已有输出，旧 flags 随列删除，不进入新运行逻辑。不调用 KCN，不重算证据，不修改 state/reason/observed_at/version/operation；inactive 清空，无可提取值为空。随后新 worker 直接读取当前源更新。已有 LB 成员表原样保留，避免凭迁移空值删除旧后端。

## SQL 逐文件修改

| 文件 | 必须同步改的查询 |
| --- | --- |
| observation.sql | 删 ObservationResources/ObservationAttachments/ObservationEgressEdges/ObservationLBEdges/WakeAuditedResource/Platform/Attachment；加 LookupObservationTargets、WakeManaged*、NotifyDefaultEgress/VirtualSwitch/SecurityGroupTarget；NotifyResource/Attachment 排除终态 |
| worker.sql | AcquireLease 返回 requested_generation；ReleaseLease 删 evidence/covered_generation，加 claimed_generation/observation_error/terminal；删 SaveVPCProviderReadiness/GetVPCProviderReadiness；BeginProviderMutation 允许同一未知创建恢复；CompleteAttempt 用 OperationReason |
| platform.sql | Acquire/ReleasePlatformLease 同共同规则；删 ObservationPlatformResources/SavePlatformImages；BeginPlatformMutation 同名创建恢复；AdvancePlatform 仅成功读记时；CompletePlatformAttempt 用 OperationReason；排除 deleted |
| attachments.sql | FinishAttachment 去 relations/protocol/proof 参数；CountAttachments/HasAttachmentSlot/CountAttachmentConsumers 去 OR protocol_blocked；ListAttachmentConsumers 读四列；released 优先停止调度 |
| subnets.sql / vnics.sql / egress.sql | AdvanceSubnet/VNic/EIP/Snat 仅成功读更新相应投影；AdvanceSnat 删 target_generation；错误不能清 MAC/IP/applied 值；SubnetOverlaps/CountBlockingSubnets 不保留 released+protocol_blocked 占用 |
| default_vpc_egress.sql | ClaimDefaultEgressLease 返回 lease_epoch/requested_generation；Insert/Finish 去准入证明/target_generation；FinishDefaultEgress 接共同排程；BeginDefaultEgressMutation 支持同一未知 create_eip/create_snat；操作 reason 独立 |
| virtual_switch.sql | LeaseVirtualSwitch 返回通知计数；ReleaseVirtualSwitchLease 接共同式；NotifyVirtualSwitch 不撤租约；AdvanceVirtualSwitch 不因读错写 degraded；deleted park |
| security_groups.sql | ScheduleSecurityGroup 保留 requested_version 并增加通知；Notify 只加通知；Claim/Lease/Finish 接共同字段；删 ListSecurityGroupConsumerRelations；所有 target lock/write 用 work.TargetType |
| load_balancers.sql | 删 SaveLBGenerated/ListLBGenerated/ResetLBRetiredVIPProof；AdvanceLB/UpdateLBIntent/AdmitLBDeletion 去 revision/dataplane；InsertLBMember/ListLBMemberIdentities 去 VNic/VNicIP；Save/GetLBCapability 去 fingerprint/images |
| load_balancers.sql 的取消/退役路径 | CanDeleteLBWithoutProvider/CompleteLBDatabaseDeletion/InsertLBRetiredBinding/CanParkLBWaitingWithoutProvider/CompleteLBRetiredDatabaseDeletion 全部去 generated/VIP 子句，保留自己的 dispatch/pending/原组件身份 |
| platform/egress pool 查询 | 删 PublicPoolTopology/PublicPoolVirtualSwitchTopology；删 addressPoolTopologyHash/addressPoolTopologyFor/sameProviderImages 调用；SavePoolVerification 只存输入，不触发状态任务 |
| base_connectivity.sql / base_backfill.sql | 删除 ProviderObservedAt/Proof/freshness 激活 gate；组合对象的自身关系变化仍可通知，不转成健康复核 |

GET/LIST 的 VPC/Subnet/VNic/EIP/Snat/平台/LB 查询 LEFT JOIN 对应任务 observation_error，由 data mapper 填既有 ObservationStale；避免每行再查，也不在 getter 发 KCN 请求。内部 Lock 查询保留自己的模型与锁序。VPC public_egress 的 flag 取对应 SNAT 任务；AppliedEnabled 不受 flag 清空。

`AdvanceVNic` 的 mac_address/primary_ip/ips 和 `AdvanceEIP.address` 改为仅按 observed 控制赋值：成功读取则保存实际值，包括空值；读取失败保留旧值。删除非空字符串/cardinality>0 才接受新输出的条件，避免 KCN 已报告撤回后仍黏住旧地址。

`sqlc.yaml` schema 加 0032；对 `network_default_vpc_egresses.next_run_at` 增加 `pgtype.Timestamptz` override，使 park=infinity 后整行 GET 可正常解码；保留 attachments.next_check_at 既有 override。重新生成 sqlc，所有 embed 查询调用者同步访问新返回结构。

源码入口：[migrations](https://github.com/zhangzhe-ctrl/ani-resource-service/tree/5dc539d92e3150a3593bde3668082753aa4fdebe/migrations)、[network queries](https://github.com/zhangzhe-ctrl/ani-resource-service/tree/5dc539d92e3150a3593bde3668082753aa4fdebe/internal/data/network/queries)、[sqlc.yaml](https://github.com/zhangzhe-ctrl/ani-resource-service/blob/5dc539d92e3150a3593bde3668082753aa4fdebe/sqlc.yaml)。

# 12. API、配置、组装与文档

## 对外 protobuf 保留字段号

本期保持客户端格式；不新增 proof 协议。没有运行来源的字段明确 deprecated/空值，不能为兼容它们继续执行旧验证。

| proto message/字段 | 固定输出或处理 |
| --- | --- |
| network.proto VPC/Subnet observed_at=11、observation_stale=12 | 保留；最近成功读取/无成功读取或最近读错 |
| VPCPublicEgress observed_at=10、observation_stale=11、applied_enabled=9 | 保留；不按 TTL 清 applied_enabled |
| BaseConnectivity observed_at=3、observation_stale=4 | 保留诊断；不作为激活前提 |
| VNic observed_at=14、observation_stale=15、reason=16 | 直接投影与读取诊断分离 |
| Attachment observed_at=21、root_controller=25 | 21 保留；25 deprecated、输出不再填历史链 |
| AttachmentConsumer observation_stale=11、observed_at=12 | 保留；不倒推占用或释放 |
| GetSubmissionResponse controller_uids=12、root_controller=16、controllers=17、retired_pods=19 | deprecated；Network adapter 忽略；current_pods=18 与当前业务身份保留 |
| egress.proto EIP/VPCSnatBinding/PlatformResource observed_at=11、observation_stale=12 | 保留无年龄语义的诊断 |
| VPCSnatBinding desired_enabled=15、optional applied_enabled=16 | 保留；Pending/未报告为 nil，读请求失败保持最后值 |
| CapabilityObservation observation_stale=4 | 读取错误诊断，不强制 ready=false |
| PublicAddressPool verification=11 | 管理员记录保留 |
| PublicAddressPool topology_fingerprint=12、observed_provider_images=13 | deprecated，返回空字符串/空列表 |
| IntranetAddressPool intranet_networks=6、verification=10 | 保留可选记录 |
| IntranetAddressPool topology_fingerprint=11、observed_provider_images=12 | deprecated，返回空 |
| CreateIntranetAddressPoolRequest intranet_networks=8 | 保留可选，不与基础设施覆盖比较 |
| GetIntranetPoolDefaultsResponse name=1、uid=2、intranet_networks=3 | 前两项取 status.default；3 固定空数组 |
| DefaultVPCEgress observed_at=25、target_generation=26、observation_stale=28 | 25/28 保留；26 deprecated/0；pending_action=27、applied_enabled=17 保留 |
| LoadBalancer desired_version=19、applied_version=20、configuration_state=21、provider_target_version=31 | 保留本服务配置语义 |
| LoadBalancer data_plane_state=22、data_plane_observed_at=29 | deprecated，固定 UNKNOWN/nil；不把 Gateway Ready 映射成包转发健康 |
| LoadBalancer observed_at=23、observation_stale=24；BackendMember observed_at=9、stale=10 | 保留读取诊断，不按年龄改业务状态 |

`RecordPublicPoolVerification/RecordIntranetPoolVerification` RPC 和 Request 的 pool_id=1/expected_version=2/verification=3/idempotency_key=4、Response resource=1 保留并真实存档。Verification 的 provider_source_revision=1/provider_image_digests=2/topology_fingerprint=3/evidence_reference=4/scope=5/verified_at=6/expires_at=7 原样保存；不称为 Res 的验证成功。

同步 Res 内部 wireEIP/wireSnat/wirePlatform、platform_intranet、Default Egress/LB/Attachment/SG wire mapper，重新生成必要 protobuf。历史字段不复用编号。

## 配置逐 tag

| internal/conf/v1/conf.proto | 处理 |
| --- | --- |
| Observation.audit_interval=1、audit_jitter=2、audit_timeout=3、flush_interval=4、queue_capacity=5 | 保留解析、deprecated；运行时完全不读 |
| Observation.workers_per_kind=6、request_qps=7、request_burst=8 | 保留实际并发和请求预算 |
| Worker.stale_after=4 | 保留解析、deprecated；删除所有 runtime 用途 |
| Worker.lease=1、request_timeout=2、observe_every=3、retry_min=5、retry_max=6、poll_interval=7 | 保留自己的执行/排程；observe_every 仅未完成操作 |
| LoadBalancer.enable_isolated_api=1 | 保留 |
| LoadBalancer.installation_fingerprint=2、controller_image_id=3、envoy_image_id=4、shutdown_image_id=5、kc_image_id=6 | 保留解析、deprecated；runtime 忽略，新部署不用填 |

`Bootstrap.Validate` 删除 audit/stale/容量 freshness budget 合取、StaleAfter>ObserveEvery、LB fingerprint/镜像必填与格式校验；保留 lease/request_timeout、retry、worker 并发、QPS/burst 等本服务参数约束。configs/config.yaml 去废弃配置示例及旧 env fallback；旧 YAML 留这些键仍可启动。显式安装诊断只校验它的 expected 参数。

## composition root 必改

- `openNetworkProvider/network_observation.go`：不组装 ObservationOptions，不调用 runtime ConfigureLoadBalancer；EnableObservation 只接 request_timeout；注入明确 capability reader。
- `app.go` 和 `governance.go` 两处 AttachmentWorker 构造一起删生命周期 provider verifier，接独立地址 reader。
- `NewNetwork(repository,key)`、`NewLoadBalancers(repository,authorization,key)`、`NewAttachments(repository)` 去 freshness；Egress 使用第 8 节窄接口。
- `UseBaseConnectivityFreshness/baseFreshnessLimit/Postgres.baseFreshness` 删除；default-egress、vpc_read、Instance/VM composition 不再传 StaleAfter。
- Instance 两处 NewInstanceNetwork 删除 freshness；VM 两处 Network 构造删除 native；同步 Release 的调用、接口与 mock。
- AcceptSubnet/AcceptEIP/AcceptSnat/AcceptPlatform/AcceptLoadBalancer/PrepareAttachment/ListAttachmentConsumers/GetPlatformCapabilities/NewDefaultVPCEgressRepository/SetVMNetworkResolver/ResolveVMNetworkSelection 上的 freshness 参数和 caller 全部删除。

## telemetry 和权限

删除 audit_collection/audit_failures/optional_audit_failures/audit_evidence_age/audit_index/关系候选与全局失效指标；删除 `maximum_applied_evidence_age_seconds`、server.evidenceAge、`ani_network_applied_evidence_age_seconds`。保留普通 API 请求/错误/字节、watch 错误、队列长度/重试/age、PG 事务/WAL。

`ObserveWork/ObserveAttachmentWork/ObserveVirtualSwitchWork` 不再读取 Proof/CollectedAt。读错分类为 read_error，providerReachable 由本次请求结果决定，不从 resource.Reason 推测；没请求不更新该 gauge。纯 telemetry 使用 30 秒采集、500ms DB timeout，不通知任务、不修改状态。数据库 readiness 心跳保留为服务自身健康。

按角色裁剪 `deployments/egress/network-rbac.yaml`、`deployments/ani-system/networking-rbac.yaml`、`deployments/load-balancer/network-rbac.yaml` 中只供审计的 NAT/ConfigMap/ServiceCIDR/生成 Service/Deployment/ReplicaSet/EndpointSlice 权限；保留直接声明 CR 的 get/list/watch、当前 Pod 地址读取、自己 Node 注解和明确 Secret 引用权限。删除 `ani-resource-vm-nic-cleanup` Role/RoleBinding 及 kcn-system nics 清场权限；Res 自管 NAD/ServiceAccount、Compute 自己管理对象权限保留。

## 文档合同

重写 `docs/specs/cr-observation.md` 的审计、Covers、TTL、全链释放要求；在 `docs/adr/0004-observe-cr-with-durable-reconciliation.md` 明确旧相关条款被当前状态消费合同替代。同步删除 API 注释中 independent provider proof、fresh closure、address eligibility、tombstone recovery 的验收含义。`docs/plans/network-management-simplification.md` 存本次实施规格；执行记录只记将来实际验证，不把方案写成已上线证据。

源码入口：[network.proto](https://github.com/zhangzhe-ctrl/ani-resource-service/blob/5dc539d92e3150a3593bde3668082753aa4fdebe/api/network/v1/network.proto)、[egress.proto](https://github.com/zhangzhe-ctrl/ani-resource-service/blob/5dc539d92e3150a3593bde3668082753aa4fdebe/api/network/v1/egress.proto)、[load_balancer.proto](https://github.com/zhangzhe-ctrl/ani-resource-service/blob/5dc539d92e3150a3593bde3668082753aa4fdebe/api/network/v1/load_balancer.proto)、[conf.proto](https://github.com/zhangzhe-ctrl/ani-resource-service/blob/5dc539d92e3150a3593bde3668082753aa4fdebe/internal/conf/v1/conf.proto)、[validate.go](https://github.com/zhangzhe-ctrl/ani-resource-service/blob/5dc539d92e3150a3593bde3668082753aa4fdebe/internal/conf/v1/validate.go)。

# 13. 实施顺序与有限验收

## 代码组织顺序

| 批次 | 必须一起交付的改动 | 完成条件 |
| --- | --- | --- |
| 1 | 共同类型/状态表、直接 adapter、observer 通知、六类任务 Finish 与 SQL | 无 Proof/Covers/TTL/父健康 gate；读错保留投影；通知不丢 |
| 2 | VPC/Subnet/VNic 准入与删除、MAC 索引、Attachment 当前关系/地址模型 | Ready 直接投影；原对象消失即完成；正常消费者关系可独立释放 |
| 3 | Instance/VM caller、SG 当前目标 worker | start/stop/release 不等地址或物理清场；当前快照只由本服务自己的成功观察发布 |
| 4 | Egress/Pool/Default/VSwitch、LB 组件/claim/后台路径 | 删除全部跨资源物理验收；只保留自己的声明、配置与引用 |
| 5 | 0032、生成代码、wire/config/telemetry/RBAC/规范 | schema 与所有调用方一致，旧客户端和旧配置可兼容运行 |

批次用于组织同一变更的提交；包含删字段的代码和迁移作为同一版本交付，不部署中间不兼容版本，不引入新旧 Proof 双写。

## 升级步骤

1. 停止旧 Res 中访问这些 schema 的网络 API/worker 及相关 Instance/VM 网络调用进程，避免旧代码与已删列混跑。
2. 应用追加 0032：先加新列、搬运当前地址输出，后删 proof/关系/生成资源列；修改索引与约束；保留用户配置、原 UID、pending、幂等响应、operation 历史与账本。
3. 对已停止旧执行者的任务清 lease 并增加本层执行 epoch。已完成 deleted/released/retired 的任务永久退出，其余已有任务安排一次直接读取；不通过 SQL 批量置 Available。
4. 为现有未 released 的 container/vm/vnic SG targets 补齐已有 reconciliation 类型的任务行：target_type/target_id/tenant_id 取当前目标，requested_version 取该目标 version，ON CONFLICT DO NOTHING；没有集群反向发现。
5. 启动新版。informer 只通知，worker 逐个消费当前原生状态。老 Failed 可随 Ready=True 恢复；过去已失败 operation 保持历史。

这是一轮协调升级；不要求 KCN、更底层 controller 或客户端先发布。真实实施时按仓库远端执行约定完成生成、构建和必要测试。本轮没有执行上述步骤。

## 必要验收用例

| 编号 | 验证行为 | 判定标准 |
| --- | --- | --- |
| T1 | VPC/Subnet/VNic/EIP/Pool Ready=True，缺 boundResources/Valid/init/generation/MAC/IP | Res 仍采信 Ready；fake client 证明无父/子/Node/镜像补证请求 |
| T2 | Ready True/False/nil 与旧 Failed | 按状态表更新；nil 不降级；旧 Failed 可恢复；已完成 operation 所有字段保持不变 |
| T3 | API 403/超时/连接错/未尝试读取 | 旧 State/Reason/地址/Applied/ObservedAt 不变；只记 observation_error/operation/retry；成功读才清错 |
| T4 | Claim(g) 后 Notify(g+1) 再 Finish | 新通知不会被 infinity 覆盖；重复通知/DB 重试不改用户 version/lease |
| T5 | 同名未知创建、已知 UID 缺失、删除404/异UID | 未知UID固定名重试；已知UID不重建；删除404完成；新UID对象不被删除；终态事件不复活 |
| T6 | 原 version/lease/epoch 与明确请求幂等 | 旧执行者提交被拒绝；自己产品占用仍生效；没有恢复 KCN 证明门槛 |
| T7 | Attachment open/stopped/closing/closed、同 FinalizationID 重放 | 管理状态表成立；closed 即 Released；无后代扫描；地址读错不阻止生命周期 |
| T8 | Instance/VM 当前关系与地址 | 无 Ready/TTL/Reason空值/IP非空 gate；VM GetSubmission 零 K8s 请求零写库；start/stop/delete 不等地址 |
| T9 | RequestedMAC 与自动 MAC；SG 应用 | 显式请求重复仍受自己约束；自动结果直接保存；SG 只读目标 appliedSecurityGroups，无反向 LIST |
| T10 | SNAT/Default/VLAN/VirtualSwitch/Pool | phase/Initialized/自己的注解直接映射；RecordVerification 真存档且不访问 provider；defaults 只读 status.default |
| T11 | LB 直接 owner 状态与删除 | Gateway/Route 等满足原生条件即推进配置；生成 Pod/Service 异常不参与判断；自己的组件 Gone 即释放账本 claim |
| T12 | 0031→0032、protobuf/sqlc/config/读接口 | 原意图/UID/幂等/历史完整；地址回填不清旧后端；Default Egress park=infinity 仍可 GET；旧 tag/YAML 可用；观察时间很旧不变 stale |

在现有测试中修改真实行为断言，删除只为 Covers/hash/TTL/全链消失编写的旧测试。执行与变更相关的 network、Instance/VM、migration/生成检查及仓库要求的 make verify；不为本整改新增全量物理网络审计或 KCN 故障注入验证。

## 合并前检索验收

运行代码中应没有：ObservationProof、ObservationRequirement、Covers、auditView、completeDependencies、sharedTombstone、ErrObservationPending、ProviderTarget.Direct/Tombstone、currentNetworkResource、requireLBVPCProviderReady、运行时 RejectionReason，以及由 observed_at 年龄拒绝管理命令的调用。

保留并验证：请求幂等 fingerprint、Res 配置 digest、lease/execution epoch、tenant/placement、原 UID 条件写、自己声明的 desired/applied 版本、产品引用与唯一 claim。这些字段处理 Res 自己的事务和声明，不重新裁决 KCN 网络结果。
