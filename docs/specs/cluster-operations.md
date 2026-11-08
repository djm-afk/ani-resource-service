# 管理员集群运维一期

本规范定义 ANI Res 的 clusterops 业务范围。接口字段、原生动作、错误与预算的权威说明见[调用合同](../contracts/cluster-operations-v1.md)，实施顺序和验收条件见[计划](../plans/cluster-operations-phase1.md)，实际结果只见[执行状态](../execution/status.md#cluster-operations-phase1)。输入为 ANI-Res-ClusterOps-Plan-v1.1.zip；方案状态不代表运行状态。

## 入口和职责

仅在 Res 单仓内新增 PlatformClusterOpsService，复用现有业务 mTLS gRPC 和 Gov platform 委托，在 full/governance 显式装配。admin 继续用于健康和指标；vpc-read、compute-only、image-admin 不注册本服务。精确方法及受信 metadata 见[治理调用登记](governance-calls.md#clusterops-platform)。

Kubernetes 负责 schema、默认值、不可变字段、admission、调度和控制器收敛。Res 负责允许范围、定位、Namespace 写限制和原生请求适配。一次请求只管理一个对象；成功只表示原生受理或修改完成，不表示 Ready、彻底删除或应用已热加载。

## 资源动作范围

“读取”表示原生 List/Get；“CRUD”表示单对象 Create/Update/Delete 加读取。每个写动作仍受 Namespace 规则、管理员身份与 Kubernetes 原生权限约束。

| group/version/resource | scope | 本期动作 |
|---|---|---|
| /v1/nodes | Cluster | 读取 |
| /v1/namespaces | Cluster | CRUD；已登记租户 Namespace 本体只读 |
| /v1/pods | Namespaced | CRUD、读取日志；没有“重启 Pod”动作 |
| apps/v1/deployments | Namespaced | CRUD、scale、restart |
| apps/v1/statefulsets | Namespaced | CRUD、scale、restart |
| apps/v1/daemonsets | Namespaced | CRUD、restart；没有 scale |
| apps/v1/replicasets | Namespaced | 读取，供原生关联与修订信息展示 |
| apps/v1/controllerrevisions | Namespaced | 读取，供原生修订信息展示 |
| batch/v1/jobs | Namespaced | CRUD |
| batch/v1/cronjobs | Namespaced | CRUD |
| /v1/configmaps | Namespaced | CRUD |
| /v1/secrets | Namespaced | CRUD；列表和默认详情不展开内容 |
| /v1/services | Namespaced | CRUD |
| networking.k8s.io/v1/ingresses | Namespaced | CRUD |
| discovery.k8s.io/v1/endpointslices | Namespaced | 读取，供 Service 排障 |
| /v1/persistentvolumeclaims | Namespaced | CRUD |
| /v1/persistentvolumes | Cluster | CRUD |
| storage.k8s.io/v1/storageclasses | Cluster | CRUD |
| /v1/serviceaccounts | Namespaced | CRUD |
| rbac.authorization.k8s.io/v1/roles | Namespaced | CRUD |
| rbac.authorization.k8s.io/v1/rolebindings | Namespaced | CRUD |
| rbac.authorization.k8s.io/v1/clusterroles | Cluster | CRUD |
| rbac.authorization.k8s.io/v1/clusterrolebindings | Cluster | CRUD |
| apiextensions.k8s.io/v1/customresourcedefinitions | Cluster | 读取、Update、Delete；本期不加 CRD 创建入口 |
| /v1/events | Namespaced | 读取 |
| 已安装 CRD 声明且 served 的 CR 实例类型 | 以 CRD 为准 | 通用 CRUD；Namespace 规则照常适用 |

表中 core group 是空字符串，不是字符串 core。resource 使用 Kubernetes plural 名称。动作表是服务端固定范围，不接受客户端提交 verbs 或 namespaced 作为授权依据。

不自动纳入聚合 API、任意子资源或未列出的内置类型。不能通过填写 resource="pods/exec"、"*/status" 等绕过范围。只为日志、scale 提供下文明确的专用入口。

## 租户 Namespace 写保护

只使用 Res 已登记的 `(cluster_id, namespace)`；不按前缀、标签、Owner、ownerReferences 或依赖猜测归属。Network 与 Sandbox 两个来源必须共同形成完整集合：

- Network 使用[现有登记 SQL](../../internal/data/network/queries/vpcs.sql)和[窄只读适配](../../internal/data/network/clusterops_namespaces.go)，按 cluster_id 过滤；NULL/空 UID 不解除保护。
- Sandbox 使用[独立登记 reader](../../internal/data/sandbox/clusterops_namespaces.go)，仅查询 `SELECT tenant_id::text FROM sandbox.tenant_spaces ORDER BY tenant_id`，通过[既有名称规则](../../internal/tenantnamespace/identity.go)转换登记行。open/ready/closing/closed 和无 UID 记录均保留。
- composition root 先确认 Compute 存在且 settings.ClusterID 与 Network.ClusterId 一致，再从既有 DatabaseDSNFile 构造 reader。缺配置、不匹配、凭据不可读或 DSN 非法注入 unavailable source。合法配置的 pool 不以首次 Ping 为构造前提，暂时查询故障由后续请求自然恢复。
- 不跨库 UNION，不启动 Sandbox worker、迁移、租户初始化、UID 回填或业务 Owner 流程来读取登记；不新增登记表、后台恢复服务或不完整集合缓存。

所有 namespaced 资源以及 Namespace 本体在登记命中时只读。Create/Update/Delete/Scale/Restart 和 dry-run 使用相同 guard，写入口每次重查；先按请求定位判断，正文目标必须与定位一致。Delete 不存在对象也先做保护。需要 Namespace 判定的写入要求两个来源均可读，故障返回 NAMESPACE_POLICY_UNAVAILABLE，不能当作零租户。

List/Get 的只读提示查询使用[合同规定的独立子预算](../contracts/cluster-operations-v1.md#请求预算)。依赖故障而父请求仍有效时继续原生读取，相关对象标记 read_only/NAMESPACE_POLICY_UNAVAILABLE；父请求取消或到期则透传。普通 cluster-scoped 对象不增加无关登记依赖，Namespace 本体和 namespaced CRD 删除例外。

Namespaced CRD 删除另执行[实例分页预检](../contracts/cluster-operations-v1.md#crd-删除预检)，禁止连带删除登记 Namespace 中的实例。本保护只适用于新增 clusterops 用例，原业务 Owner 链路不接 guard。

## 已接受的边界

未登记的历史共享 Namespace 不做资源级租户识别。ClusterRole、ClusterRoleBinding、PV、StorageClass、cluster-scoped CR 和 CRD 编辑等全局动作可能间接影响租户。CRD 检查与删除不是跨 Owner 原子事务，不承诺消除并发创建实例的窗口。

不新增 Owner 追溯、标签补齐、依赖图、锁/冻结协议、准入控制器、全量 informer、资源镜像库、控制器、操作任务表、账本、审批、审计数据库或签名证明。终端、文件、HPA、应用发布、Helm/GitOps 接管、Prometheus、集群凭据管理、新部署 mode、HTTP annotations 和 REST listener 均不在本期。外部 Gov/IAM/BOSS/KCN/Higress/installer 接入不修改、不作为 Res 交付前提。
