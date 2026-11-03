# Intranet 地址池运行前提

本文件说明 NET-U02 的配置接口和只读 Provider 前提。业务规则以
[基础连接规格](../../docs/specs/vpc-connectivity-lb.md)为准；当前实现及测试结论以
[执行状态](../../docs/execution/status.md)及本批正式证据为准。

BOSS 创建表单先通过 `GetIntranetPoolDefaults` 读取真实默认 VPC 身份与现有内网目标范围，再通过 `CreateIntranetAddressPool` 一并提交地址段、OVN 网关和保留地址。管理员无需手工填写 Provider UID；创建仍重新校验冻结的身份和配置，不能因两次请求间默认 VPC 被替换而误接。池 scope 固定为 `intranet`；创建后的默认网关身份和目标声明不可变。Network 在 `kcn-system` 创建 `Subnet(type: Intranet)`，`spec.gateway` 为 `kcn-system/<default_vpc_name>`。该流程不创建 Public EIPGateway、不修改默认 VPC，也不修改 `kcn-config` 或集群路由。

受理及持久 worker 会检查：

- 默认 VPC 位于 `kcn-system`，UID 与管理员固定值相等；实际控制器启动参数中的 `--cluster-router` 与短名相符，缺省值是固定 Provider 合同的 `kcn-cluster`。
- 默认 VPC 的 Valid/Initialized/Ready 及当前 observedGeneration；Intranet 池和 EIP 使用各自的类型、namespace、UID与代次合同。
- `kcn-system/kcn-config` 的 `data.intranetNetworks` 是有效 IPv4 CIDR 列表，不能为 `0.0.0.0/0`；现有配置覆盖管理员声明的目标范围。声明和实际配置均须覆盖 Kubernetes `ServiceCIDR` 对象中的 IPv4 Service 网段。管理员还应在声明中包括实际 DNS、xDS 和其他平台依赖的目标网段。
- 池 CIDR 不与已存在的地址池、节点、Pod 或 Service 基础设施地址重叠；本批基础内网池使用 Overlay，无 Underlay 配置。
- 控制器、CNI、OVS 和 OVN Central 四种角色各至少有一个当前 Running、Pod Ready、非删除中的实例，且该实例全部容器 Ready、实际镜像摘要合法。Pending、删除中或不就绪的副本不提供证据，也不抹掉同角色健康实例的证据；任一角色没有有效实例时仍拒绝。现有 [RBAC](network-rbac.yaml) 已含所需 VPC、Subnet、ConfigMap、Pod 和 ServiceCIDR 读取；不新增配置写权限。

池最初关闭新分配。人工 `RecordIntranetPoolVerification` 为可选诊断记录，不作为开放分配或 VPC 创建的门禁，不以虚构证据替代真实 Provider 就绪。迁移 0027 移除人工证据数据库约束，现有记录保留。

`SetIntranetPoolAutoAllocationEnabled` 使用当前池 `expected_version` 与永久幂等键，在一个事务中设置默认池、开放池分配并开启 `new_vpcs_enabled`。关闭仅允许操作当前默认源，且只关闭后续新 VPC 自动分配，保留默认池、池分配状态和既有资源。开启期间 `SetDefaultIntranetPool` 会校验并开放新默认源；切换仅影响后续申请，已有 VPC 保留原池及子资源身份。各池只读 `new_vpcs_enabled` 仅在该池为默认源且集群开关启用时为 true。

`GetPlatformNetworkCapabilities` 独立投影基础内网、Public 地址及 LB 能力。资源 GET/List 不访问 Provider 或安排任务（创建表单 defaults 是明确的 Provider 只读事实查询）；持久观察提供事实和唤醒。未配置的 LB 返回 `ready=false` 和 `CAPABILITY_UNKNOWN`，不会阻止基础内网可用。配置、默认 VPC身份或观测失效使基础内网能力退化；恢复原合同后由现有持久 worker 重新核验。

删除池前先关闭新分配。仍被任何用途 EIP 占用的池不能删除。Public RPC 无法查询或修改 Intranet 池；两个默认池与验证证据分别管理。

第一批交付时未将上述管理员操作部署到共享集群，也未执行真实存量补齐；后续隔离验收须依据对应任务授权执行。实际内网流量、Public 出站和 LB 数据面结论统一见[执行状态](../../docs/execution/status.md)。
