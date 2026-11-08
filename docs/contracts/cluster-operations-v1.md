# Cluster operations v1 调用合同

服务为 `clusterops.v1.PlatformClusterOpsService`，字段号、类型、optional presence 和服务方法的唯一机器合同为[clusterops.proto](../../api/clusterops/v1/clusterops.proto)。共十个 unary 与一个 server-stream；精确方法授权及 platform metadata 见[治理调用登记](../specs/governance-calls.md#clusterops-platform)。资源动作矩阵与租户登记规则只在[业务规范](../specs/cluster-operations.md)定义；本合同不开放额外 GVR、子资源或管理员入口。

## 请求预算

| 位置 | 固定上限 |
|---|---|
| 十个已登记 unary 整体 | 30 秒 |
| 单次普通 Kubernetes 请求（含发现、版本和分页） | 15 秒 |
| 两个登记来源形成一次完整集合 | 共用 2 秒子预算 |
| 其他 unary | 原 c.Timeout；非正值不额外加期限 |

调用方更短 deadline 始终优先。共享 NewGRPCServer 使用框架 Timeout(0)，在构造的新 middleware slice 首位安装精确方法预算；未取得 transport context 或不在精确表中沿用原预算。所有装配模式保留旧方法语义，不能把 SERVER_GRPC_TIMEOUT 改为 30 秒。

发现与 /version 使用原生 REST Do(ctx)，DB、原生读取和每页均继承当前请求 context。概览与 CRD 预检不因翻页重置整体预算；任一必要页失败不返回部分完整结果，预检未完成或期限已到不得发 Delete。写调用发出后的超时可能有不确定结果，不自动重试，调用方刷新对象确认。

## 字段和查询

| 字段 | 精确语义 |
|---|---|
| cluster_id | 必填，必须匹配当前配置，不接受任意连接信息 |
| type.group | core/v1 使用空字符串 |
| type.version | 必填，必须在服务端当前实际提供的版本中 |
| type.resource | 必填，plural；不含斜杠，不允许任意子资源或路径 |
| ref.namespace | Namespaced 单对象必填；Cluster scope 必须为空 |
| ref.name | 单对象必填；Create 必须与 manifest metadata.name 相同 |
| uid / expected_uid | Kubernetes 原生 UID 字符串；不强制解析成平台 UUID |
| resource_version | Kubernetes 不透明字符串，不转整数、不自增 |
| expected_resource_version | Update/Scale/Restart 必填，来自用户读取的对象；冲突必须刷新 |
| object_json | JSON 对象文本，使用 native unstructured 往返；不是 protobuf Struct 数字转换后的重建对象 |
| manifest_yaml | 单个 YAML/JSON 对象；不是任意脚本；拒绝多文档与 kind: List |
| dry_run | 可选原生预校验能力，映射 metav1.DryRunAll；不是强制审批步骤，也不生成票据 |
| read_only | 仅表示本模块固定动作/Namespace 策略，不能承诺 Kubernetes RBAC 一定允许其他动作 |
| read_only_reason | TENANT_NAMESPACE_READ_ONLY、NAMESPACE_POLICY_UNAVAILABLE、RESOURCE_READ_ONLY 或空字符串 |
| actions | 固定字符串 list/get/create/update/delete/scale/restart/logs 的适用子集，不代表调用者已获 Kubernetes 权限 |

include_secret_data 仅对 Secret 有意义；非 Secret 传 true 不产生额外能力。所有请求仍是同一管理员委托，不新增“查看密码审批”或临时令牌。

默认 gRPC 消息大小沿用现有运行配置；超限返回明确错误，不静默裁剪原生对象，不另增业务容量预测门槛。

### 查询与展示

**ListClusters：**只读取已配置 cluster ID；display_name 本期等于 cluster_id。不能返回 kubeconfig 路径、token、证书、API server 私有连接细节，也不因 List 触发集群注册。

**GetClusterOverview：**

- Kubernetes version 读取原生 /version，data 层使用同一 discovery RESTClient 的 GET 并显式 Do(ctx)，得到与 ServerVersion 相同的版本信息；不通过内部使用 context.TODO 的无参便捷方法绕开 请求预算 的请求预算。
- Node、Namespace、Pod 使用原生分页 List；在内存里累计本次响应，不保存到数据库。
- nodes_ready 以 Node Ready condition 的 status=True 计算。
- pod_phase_counts 按 Pod.status.phase 原值统计；空值归入 Unknown，不把所有异常重建成平台状态机。
- capacity/allocatable 按 Node 原生资源键分别累加，值为 Quantity 的规范字符串。不能把不同 GPU 扩展资源键换算成“物理卡总数”。
- 不提供 CPU/GPU 使用率、历史趋势、请求配额消耗或“可分配实例数”推断。
- sampled_at 是查询完成时间；多次原生读取不是集群原子快照。任何必要查询失败都返回错误，不能用 0 伪装成功。

**ListResourceTypes：**

- 返回固定内置表中当前 API 可用的类型，以及 live CRD 中 served 的实例版本。
- CRD 本身是内置管理项，custom_resource=false；由 CRD 定义的实例类型 custom_resource=true，填写 crd_name。
- 使用 discovery/CRD 原生信息判断 kind、scope、served version；请求者不得指定并覆盖这些信息。
- 允许进程内仅缓存类型发现结果，最长 60s；CRD 更新/删除成功或发现 NoMatch 后失效。该缓存不是资源镜像。
- CRD 读取或 discovery 真正失败时返回错误；不能把失败解释为“没有自定义资源”。

**ListResources：**

- Namespaced：namespace 非空且 all_namespaces=false，或 namespace 为空且 all_namespaces=true，二选一。
- Cluster：namespace 为空且 all_namespaces=false。
- page_size=0 时使用 50；允许 1–200。映射 ListOptions.Limit。
- continue_token 原样映射 Continue；不签名、不解码、不存库。
- next_continue_token 为空才表示结束；不能用本页条数少于 page_size 判断结束。
- name 是精确匹配，使用 metadata.name selector；不提供子串匹配、全局排序、任意页码、精确 total。
- label_selector/field_selector 使用 Kubernetes 原生语法；传入无效或该类型不支持的 selector 返回原生类别错误。
- name 与 field_selector 同时指定 metadata.name 时必须一致，否则 InvalidArgument；其余过滤条件合取。
- 同一遍历的 GVR、Namespace、筛选、page_size 保持不变。410 ResourceExpired 返回 LIST_EXPIRED，客户端从头刷新；不偷偷拼接新快照。
- object_json 保留原生 spec/status/metadata，用于不同类型展示；可统一去掉 managedFields 以缩小响应。Secret 使用本合同的 Secret 规则投影。
- 不为截图中的“第几页/共几页”扫描全量资源或建立索引。

**GetResource：**原生 GET 后返回身份、JSON 与 YAML。YAML 用于配置编辑，省略 status、managedFields、服务端生成的 UID/RV/时间/generation；UID/RV 已单独返回。原生 status 在 object_json 中查看。只读对象也能查看配置 YAML。

**Events：**复用 ListResources，type 为 core/v1/events。详情事件过滤使用 field_selector=involvedObject.uid=<当前对象 UID>，可叠加 type=Warning 等原生支持条件。Node 等集群对象的事件查询使用 all_namespaces=true；不误把 Node 名称当 Namespace。不新增事件存储或 ListEvents 重复 RPC。

**Pod 按节点或状态筛选：**使用 spec.nodeName、status.phase 原生 field selector。工作负载页通过原生 selector 查询 Pod，再展示原生 ownerReferences；不重建全局依赖关系。

## 原生写方法

### 所有写方法的共同步骤

1. RequirePlatform。
2. 校验请求 envelope、配置 cluster ID、服务端 descriptor 和允许动作。
3. 校验 Namespaced/Cluster 寻址。manifest 必须与目标 apiVersion/kind/name/namespace 一致；正文中 namespace 省略时仅可填入已检查的目标 namespace。
4. 读取所需登记集合并执行 Namespace guard。租户 Namespace 的 dry_run 也按写入口拒绝。
5. 仅执行本次原生 API 调用及必要的对象读取/并发检查。
6. 返回 API server 结果或稳定错误，不写业务账本，不创建操作单，不调用 quota、退款、ACC、Owner action 或 reconcile。
7. 服务端不自动重试不确定的写入。网络超时不能改成“肯定未执行”；调用方刷新原生对象确认，再决定下一次操作。

平台请求的 request ID 不带来跨次幂等保证。Create 同名 AlreadyExists、原生 resourceVersion 冲突、Delete UID precondition 是本期采用的原生机制，不再加第二套幂等账本。

### CreateResource

- 允许动作必须含 create；CRD 定义、Node、Event、ReplicaSet 等不允许创建。
- 单个对象，metadata.name 必填；不接受 generateName 自动命名。
- 对象正文禁止携带服务端 UID/resourceVersion/creationTimestamp/deletionTimestamp/managedFields/status 等系统生成字段；返回 InvalidArgument，而不是偷偷承接历史导入。
- 使用原生 YAML/JSON 解码，保留 CR 未知结构和大整数，不能通过 map[string]any 的浮点路径丢精度。
- dynamic.Resource(gvr).Namespace(ns).Create，Cluster 类型不调用 Namespace(ns)。
- CreateOptions 使用 FieldManager=ani-res-clusterops、FieldValidation=Strict；dry_run 时设置 DryRunAll。
- Native AlreadyExists 不自动转 Update，不自动删除重建、改名或接管业务资源。

CreateResource 与 UpdateResource 共用 internal/data/clusterops/manifest.go 的 decodeManifestStrict(manifest string) (*unstructured.Unstructured, error)，固定解析顺序如下：

1. 确认输入恰为一个 YAML/JSON 对象；拒绝第二个 YAML 文档、第二个 JSON 值、顶层非对象及 kind: List。允许首尾空白和注释，不能仅解码首个对象后忽略剩余输入。
2. 在转换成 unstructured/map 前拒绝任意嵌套层级的重复键。使用现有 sigs.k8s.io/yaml.YAMLToJSONStrict 的严格转换能力（JSON 输入同样检查），不能先用普通 YAMLToJSON/Unmarshal 丢弃重复项，再寄希望于服务端 FieldValidation=Strict。
3. 严格转换后的 JSON 使用现有 apimachinery 的保留整数、区分字段大小写的原生 unstructured 解码路径；不经过默认 float64 的 interface{} 解码。对结构内未知业务字段保持原样，由 API server 的 CRD/schema 决定是否合法；不自建 schema 校验器。
4. Namespace、GVK、名称及系统字段的检查都使用这同一份解析结果。解析错误映射 INVALID_ARGUMENT，公共消息不包含原 manifest、重复字段的值或原始解析器错误正文。

FieldValidation=Strict 仍保留，用于校验 API server 实际收到的字段；它不能检测前置解析已丢失的重复项。A08 同时覆盖 YAML/JSON 重复键和大于 2^53 的有效 int64 往返精度。

### UpdateResource

- expected_uid、expected_resource_version 必填。
- 先 GET 当前对象；UID 不符返回 RESOURCE_UID_CHANGED；RV 不符返回 RESOURCE_VERSION_CONFLICT。
- manifest 身份与 ref 一致。若正文显式含 uid/RV，必须与 expected 值一致；不能通过正文覆盖它们。
- 使用请求携带的 expected_resource_version 发送 Update，不把 GET 到的新 RV 偷换进去。
- 服务端保存当前 status；若用户正文尝试不同的 status，返回 SYSTEM_FIELD_READ_ONLY。编辑正文未含 status 时复制当前 status；当前不存在则不新增。CR 未必有 status 子资源，不能因为编辑视图省略状态就清掉它。
- UID、creationTimestamp、generation、deletionTimestamp、deletionGracePeriodSeconds 等服务端字段，正文省略时复制当前值，显式提交相同值时接受，显式提交不同值时返回 SYSTEM_FIELD_READ_ONLY；managedFields 使用同一规则且不作为可编辑数据。正常 metadata 标签、注解、ownerReferences、finalizers 按原生语义提交。
- 不自动删除 finalizer、不自动修复 ownerReferences、不强制接管字段所有权。
- 走 dynamic Update/原生 PUT，FieldManager=ani-res-clusterops、FieldValidation=Strict，可选 DryRunAll。
- 409 返回刷新冲突。禁止 RetryOnConflict 把旧整份 YAML 套到最新对象；禁止 force apply。
- Kubernetes 的 immutable、Service 字段、PVC 字段、RoleBinding.roleRef 等限制直接交给 API server，Res 不复制一份验证引擎。

本模块统一不提供 status 编辑，Create 禁止提交、Update 保留原值的规则也适用于未启用 /status 子资源的 CR。这是本期产品范围，不声称所有 CR 的主资源原生 PUT 都天然禁止修改 status；不据此新增 status API。

### DeleteResource

- expected_uid 必填。
- 先按目标执行 Namespace guard；即使对象已不存在，也不能用此路径解除租户 Namespace 的限制。
- 使用原生 DeleteOptions.Preconditions.UID；不提供强制删除开关，不设置 gracePeriodSeconds=0，不自动清 finalizers。
- propagationPolicy 和 grace period 不赋新值，沿用 API server 默认行为。
- Native Delete 成功返回 deletion_requested=true、already_absent=false；不能当作资源已完全消失。
- NotFound 返回 deletion_requested=false、already_absent=true。
- UID precondition 冲突返回 RESOURCE_UID_CHANGED。
- 删除 Pod 就是删除，不承诺重建，不标记为“重启成功”。
- CRD 定义额外执行本合同的 CRD 删除预检检查。

### ScaleWorkload

仅 apps/v1 Deployment、StatefulSet 允许。

- replicas 字段必须出现且 >=0；0 合法。Res 不增加服务最低副本、计费、配额或容量预测限制。
- Namespace guard 后调用对应 typed AppsV1 GetScale。
- 核对 scale.metadata.uid/resourceVersion 与 expected 值；只修改 scale.spec.replicas。
- UpdateScale 使用原生版本条件，dry_run 按原生 UpdateOptions。
- desired_replicas 来自返回的 Scale.spec.replicas；observed_replicas 来自 Scale.status.replicas；它不表示全部 Pod 已 Ready。
- Pod、DaemonSet、Job、CronJob 返回 ACTION_NOT_SUPPORTED。
- 不暂停 HPA，不改 Helm values/GitOps 源，不因为 managed-by=Helm 标签就一律拒绝运维。已有控制器后续可能覆盖手工值，不由本模块接管。

### RestartWorkload

仅 apps/v1 Deployment、StatefulSet、DaemonSet 允许。

- 先 Namespace guard、GET、UID/RV 检查。
- Deployment.spec.paused=true：返回 RESTART_STRATEGY_UNSUPPORTED。
- DaemonSet/StatefulSet.spec.updateStrategy.type=OnDelete：同上。
- StatefulSet.spec.updateStrategy.rollingUpdate.partition>0：同上。
- 不自动改 paused、updateStrategy 或 partition；不循环 Delete Pod 兜底。
- 将 spec.template.metadata.annotations["kubectl.kubernetes.io/restartedAt"] 更新为服务端本次 UTC RFC3339Nano 时间。
- 使用包含 expected resourceVersion/UID 的最小 merge patch；注解 map 不存在时正常创建它，不覆盖其余注解。
- PatchOptions 使用固定 FieldManager，支持原生 dry_run；409 直接返回冲突。
- 沿用原生策略。Deployment Recreate 可能有中断；副本数为 0 时不创建 Pod。返回仅说明模板更新被接受。

## 配置、Secret 和 Kubernetes RBAC

### ConfigMap

完整保存原生 data/binaryData/immutable 等配置。只保存对象，不扫描消费者，不自动重启工作负载，不判断应用是否完成热加载。

环境变量、卷投射、subPath 与应用重载遵循 Kubernetes 和应用原有机制；保存成功只表示 API server 接受。immutable 的原生错误不通过删除重建绕过。

Higress 直接按类型、Namespace、名称查 ConfigMap/CR。不要新增“把所有 Higress 配置识别并聚合”的后台。

### Secret

固定两个读取行为，不增加第二套授权：

| 出口 | 行为 |
|---|---|
| ListResources | object_json 移除 data、stringData 和 metadata.annotations；secret_data_redacted=true，返回 key 名称列表 |
| GetResource(include_secret_data=false) | 同上；manifest_yaml 为空，不能把脱敏文档当编辑正文 |
| GetResource(include_secret_data=true) | 返回真实原生内容及可编辑 YAML；secret_data_redacted=false |
| MutationResponse | 使用脱敏投影，不从保存响应再次展开 Secret |
| 普通请求日志和错误日志 | 不记录 manifest、Secret 值、日志正文或 last-applied 配置 |
| Kubernetes HTTP Warning | 本模块 client 使用 rest.NoWarnings{}，不输出原文；不改变其他 Owner client |

secret_keys 取真实 data/stringData 的键并排序，不返回值。默认去掉注解，是因为 last-applied 等注解也可能含原始配置。

编辑端必须先请求真实内容。后端不生成含 ****** 的伪造可提交 YAML；Update Secret 正文必须显式包含 data 或 stringData，清空 data 要明确提交 data: {}。不靠猜测值是否“像密码”处理字段。

Secret data 保持 Kubernetes 原生 base64 表达，stringData 保持原生输入语义，不建加解密或凭证签发服务。

### RBAC 与 ServiceAccount

五类资源按原生对象 CRUD；租户 Namespace 内的 namespaced 对象仍只读。ClusterRole/ClusterRoleBinding 不做租户引用追溯。

roleRef 不可变时返回原生错误，后端不能偷偷删后重建。ServiceAccount 管理不包含 TokenRequest、长期 Token 自动创建、用户 kubeconfig 签发、Gov 角色同步或权限扩散计算。

## Pod 日志

### 目标与参数

一次请求一个明确 Pod 和一个容器，不聚合一个 Deployment 的所有 Pod，不跟随同名替换 Pod 自动续流。

- cluster_id、namespace、pod_name 必填。
- expected_pod_uid 可选；调用方已有详情 UID 时应携带。开流前 GET Pod，若期望 UID 不符返回 POD_CHANGED。
- container 为空且 Pod 只有一个普通容器时选择它；多普通容器时要求显式选择。显式名称可指向 Pod 中存在的普通/init/ephemeral 容器。
- follow、previous、timestamps 直接映射 PodLogOptions。
- tail_lines 未设置时保持 native nil；设置时 >=0，0 不等于未设置。
- since_seconds 与 since_time 最多设置一个；since_seconds >0；Timestamp 有效。
- limit_bytes 未设置时 native nil；设置时 >0。
- previous 与 follow 组合按 Kubernetes 原生行为处理；不额外发明参数兼容性限制。
- 不暴露 insecureSkipTLSVerifyBackend、任意路径、exec 或其他日志子资源能力。

### 方法与帧

Data adapter 固定调用：

~~~go
CoreV1().Pods(namespace).GetLogs(podName, podLogOptions).Stream(ctx)
~~~

- 普通 client 15s 整体超时不能用于 follow；日志 client 的整体 Timeout=0、HTTP 响应头等待 15s，应用层使用下表预算。
- 进程内同时最多 100 条 clusterops 日志流；取得并发槽后、调用 OpenPodLogs 进行 Pod 检查和建流前记录唯一开始时刻 t0。超出并发上限返回 ResourceExhausted，不建租户流配额表。

| 预算 | 固定值 | 作用 |
|---|---|---|
| 读取窗口 | 自 t0 起 10 分钟 | 以 RPC context 派生 readCtx；目标检查、建流及读取共用，到期取消并关闭 reader |
| READY/DATA 单次发送等待 | 最多 15s | 从该帧开始交给唯一发送者时计时，不因重试延长 |
| END/ERROR 终止帧发送等待 | 最多 5s | EOF、窗口结束或读取错误后的有限结束通知 |
| 整个转发处理函数 | 自 t0 起最多 10 分钟 5 秒 | 包含读取窗口及结束阶段，handler 必须有界返回 |

实际发送等待取该帧预算、整个处理函数剩余预算以及调用方剩余预算中的最短者。所有期限继承上游取消；30s unary 预算不作用于本流。内部 readCtx 到期不直接取消上游 RPC，保留发送正常结束通知的机会，但不得因此无限持有原 RPC。

帧与结果固定为：

- 单帧 DATA 最多 16KiB，按 bytes 转发；禁止 io.ReadAll、整段落盘或默认 Scanner 的长行截断。
- 开流失败在 READY 前返回 gRPC status；成功后先发 READY（pod_uid、container），再发 DATA。
- 正常 EOF：在终止帧预算内发送 END（reason=EOF）；成功后返回 nil。
- 读取窗口到期且 RPC 仍有效：关闭 reader，停止继续读取和组装 DATA；在剩余总预算内发送 END（reason=STREAM_LIMIT_REACHED），成功后返回 nil。已有一帧发送未完成时，只可等待该发送的原有截止时间与总截止时间中的较早者；无法完成则走发送超时退出，不能并发补发 END。
- READY 后其他读取错误：在终止帧预算内尝试一次 ERROR（reason=LOG_READ_FAILED，message=Pod 日志读取失败）；发送成功后仍返回 Unavailable 并附同一 reason，不能把故障包装成 EOF。公共错误不含内部 URL或原始响应正文。
- 上游已取消/到期：优先返回其 Canceled/DeadlineExceeded，关闭 reader，不保证终止帧。判断顺序为先检查 RPC context，再检查内部读取窗口，最后处理其他读取错误。
- 任意实际 Send 返回错误：立即关闭 reader 并返回该错误，不继续发送。
- READY/DATA/END/ERROR 的发送等待到期而 RPC context 仍有效：返回 DeadlineExceeded、ErrorInfo.reason=LOG_SEND_TIMEOUT；关闭 reader，停止产生帧，不再尝试 END/ERROR。正常窗口结束与发送受阻必须区分，后者不能返回 OK。
- END/ERROR 后不得再发 DATA。不自动重连、不聚合历史、不承诺日志永久保留。
- API 按名称建流，没有跨 API 全局事务保证；expected UID 仍是常规目标检查，不升级为租户或 Owner 证明协议。

StreamPodLogs 在租户 Namespace 内允许调用，因为是查看能力；管理员身份仍然必需。

### 单一发送者与有界退出的固定实现

gRPC SendMsg 可以等待下游流控而阻塞；只取消 readCtx、关闭 Kubernetes reader 或替换 ServerStream.Context()，都不能直接取消原 stream 上的 Send。固定采用以下同进程实现，不增加流管理服务：

1. UseCase.StreamPodLogs 取得并发槽后统一 defer 归还。reader 一旦取得，必须在 readCtx 取消及用例退出时关闭；沿用现有 response-header timeout、reader 关闭方式，不搬入 Owner UID 链或业务生命周期判断。
2. 每条流只建立一个实际调用 send(LogFrame) 的发送 goroutine。service 提供的 send 回调只调用原 gRPC stream.Send，不再启动第二个发送者。
3. 用无缓冲帧通道和容量为 1 的发送结果通道逐帧交接；上一帧结果未确认前，不读下一块数据、不提交下一帧。发送者还监听停止信号；停止后不接新帧。不得建立无界队列，不得按每帧启动无法回收的发送 goroutine。交给发送者的 frame/data 使用独立字节副本，发送后不再复用或修改，避免与 gRPC 的延后读取竞争。
4. 用例所在 goroutine 监督发送结果、RPC context 与上述发送/总期限，不能直接卡在底层 stream.Send。读取窗口到期时停止上游读取；已有发送按 本日志帧规则 的剩余期限处理，完成后才可发送结束帧。
5. 发送超时、发送失败、上游取消或总期限耗尽时，停止产生帧、关闭 reader、发出发送者停止信号，随后让用例和 gRPC handler 返回。结果通道有一个缓冲槽，使发送者迟到的返回结果不会再次阻塞。
6. handler 返回前不得无限等待发送者结束：grpc-go 在 handler 返回后关闭底层 stream，才会解除流控中的 Send 等待。不得形成“等待发送者退出才能返回、发送者又等待 stream 关闭”的循环。handler 返回后，发送者必须随底层 stream 关闭退出；A17 必须验证发送者实际回收，而非只验证计数器减少。

生产预算固定为上述常量；测试可以通过包内测试构造缩短时长，不能增加面向调用方的超时参数、生产配置开关或新的 Proto 字段。

## CRD 删除预检

删除 CRD 会删除该类型的实例，因此对 Namespaced CRD 保留一次 Namespace 预检。

流程固定：

1. GET CRD 并核对 expected_uid；仅此初始 GET 确认定义不存在时返回 already_absent=true；其他读取错误返回失败。
2. 读取保护 Namespace 集合；失败返回 NAMESPACE_POLICY_UNAVAILABLE。
3. 选择一个 served 的实例版本：优先 served 且 storage=true 的版本，否则使用声明顺序中的第一个 served 版本。
4. 用该 GVR 在所有 Namespace 分页 List，Limit=200；只读每个实例的 metadata.namespace，不读取或分析业务字段。
5. 任意实例 namespace 命中保护集合，立即返回 CRD_HAS_PROTECTED_NAMESPACE_INSTANCES。
6. List（包括任一页的 404）、转换 webhook 或版本选择失败都返回明确错误，不当成零实例或 CRD 已不存在，且不得执行 Delete；未找到 served version 时返回 CRD_INSPECTION_FAILED。
7. 全部页检查结束后执行原生 Delete，UID precondition 不变。初始 GET 已找到 CRD 后，原生 Delete 阶段的 404 也返回 RESOURCE_NOT_FOUND，不重放请求或改报 already_absent。
8. 成功后失效类型发现缓存。

scope=Cluster 的 CRD 不做租户归属检查；定义编辑、ClusterRole 等共享变更也不进行全局影响分析。

此检查与实际 Delete 不是跨 Owner 原子事务。其他业务仍可能在检查后并发创建实例。方案只承诺上述操作前检查，不增加锁、停发协议、摘要、审批票或证明系统，也不宣称绝对隔离。删除正在被其他业务并发使用的定义仍是管理员运维责任。

## 稳定错误和日志出口

Service 统一把 biz 错误映射到 gRPC status，并附 google.rpc.ErrorInfo：

- domain 固定 clusterops.ani.io。
- reason 使用下表稳定字符串。
- metadata 只允许 cluster_id、group、version、resource、namespace、name、kubernetes_reason、kubernetes_http_status、field_paths 等安全定位信息。
- 不放请求 body、Secret 值、日志内容、证书、凭据文件路径或内部连接 URL。

| reason | gRPC code | 行为 |
|---|---|---|
| INVALID_ARGUMENT | InvalidArgument | 缺字段、非法 selector、多个 YAML 对象、目标与正文不一致 |
| SYSTEM_FIELD_READ_ONLY | InvalidArgument | 尝试修改由服务器维护的状态/身份字段 |
| PLATFORM_SCOPE_REQUIRED | PermissionDenied | 非受信管理员范围 |
| CLUSTER_NOT_CONFIGURED | NotFound | 未配置的 cluster ID |
| RESOURCE_TYPE_UNAVAILABLE | FailedPrecondition | 未支持/未提供的 GVR 或版本 |
| RESOURCE_READ_ONLY | PermissionDenied | 固定表中的只读类型尝试写入 |
| TENANT_NAMESPACE_READ_ONLY | PermissionDenied | 登记租户 Namespace 或其内部资源禁写 |
| NAMESPACE_POLICY_UNAVAILABLE | Unavailable | 必需登记来源无法读取 |
| RESOURCE_NOT_FOUND | NotFound | 原生对象不存在 |
| RESOURCE_ALREADY_EXISTS | AlreadyExists | Create 同名对象 |
| RESOURCE_UID_CHANGED | Aborted | 对象同名重建或 UID precondition 失败 |
| RESOURCE_VERSION_CONFLICT | Aborted | resourceVersion 冲突 |
| ACTION_NOT_SUPPORTED | FailedPrecondition | 资源类型不支持 scale/restart 等 |
| RESTART_STRATEGY_UNSUPPORTED | FailedPrecondition | paused、OnDelete、partition 等已列情况 |
| CRD_HAS_PROTECTED_NAMESPACE_INSTANCES | FailedPrecondition | CRD 下存在租户 Namespace 的实例 |
| CRD_INSPECTION_FAILED | FailedPrecondition | 没有可读 served 版本等无法完成原生检查的情况 |
| LIST_EXPIRED | Aborted | Native 410；刷新列表，不续接新快照 |
| KUBERNETES_FORBIDDEN | PermissionDenied | 当前受管 Kubernetes 凭据没有原生权限 |
| KUBERNETES_UNAUTHORIZED | Unauthenticated | Kubernetes 凭据被拒绝 |
| KUBERNETES_INVALID | InvalidArgument | Native 400/422 等 schema/immutable 错误 |
| KUBERNETES_UNAVAILABLE | Unavailable | 原生 5xx/连接依赖失败 |
| RESOURCE_EXHAUSTED | ResourceExhausted | 原生 429、现有传输上限或日志并发上限 |
| POD_CHANGED | Aborted | 请求的期望 Pod UID 不符 |
| POD_CONTAINER_REQUIRED | InvalidArgument | 多容器未选定容器 |
| LOG_READ_FAILED | Unavailable | READY 后原生日志读取失败；错误帧之后仍以该非 OK 状态结束 |
| LOG_SEND_TIMEOUT | DeadlineExceeded | 日志发送或终止帧等待预算耗尽；关闭 reader 并返回，不继续补发帧 |
| INTERNAL_ERROR | Internal | 未分类内部错误；不输出原始敏感正文 |

上游 RPC 的 context.Canceled、DeadlineExceeded 保持对应 code，不包装成成功；本模块内部日志窗口到期，只有 END/STREAM_LIMIT_REACHED 在预算内发送成功才正常结束，发送受阻按 LOG_SEND_TIMEOUT 非 OK 结束，两者不混用。Kubernetes 的原生原因与字段路径可返回；包含提交值的完整错误字符串不写日志，也不直接复制成公共 message。无法安全拆分的错误使用上述稳定 reason 和通用说明。

在现有 requestLogging Redacter 分支加入 /clusterops.v1.。普通日志最多记录已有 trace/actor/request ID、方法和结果，不为本功能另建审计事件表、告警闭环或取证报告。

Redacter 与公共错误清理不覆盖 client-go 的默认 WarningLogger。按 客户端配置 对本域全部 client 副本设置 rest.NoWarnings{}，防止 admission 返回的 HTTP 299 Warning 原文进入日志；原生 HTTP 响应仍按成功/错误正常处理。这一设置不改全局 logger、Owner client 或集群 admission 策略。

## 实现边界与调用方接入

biz 使用纯 Go 端口和用例；data 承担 dynamic/typed/discovery、严格解析、原生对象投影和错误提取；service 只做 protobuf/domain/status 转换；cmd 显式构造和关闭连接。客户端复用 Network 集群配置，普通配置副本 Timeout=15s、QPS=5、Burst=10、UserAgent=ani-resource-service/clusterops。日志副本 Timeout=0，开流使用独立 15s 计时，reader 关闭时取消其上下文；保留 client-go 原有 TLS/auth transport。全部 clusterops client 副本设置 WarningHandlerWithContext=rest.NoWarnings{}，不改 Owner 配置或进程全局 Warning handler。

类型发现当前逐请求 live 读取，不缓存资源对象。CRD 编辑/删除后下一次发现立即反映 API server 状态。Kubernetes 实际可用动作仍受受管凭据及既有 admission 限制，Res 不扩大服务账号权限。

Gov 后续登记权限目录、路由/BFF 和正式 API module 版本；IAM/BOSS 后续完成管理员授权和界面。沿用单 module 发布，不复制生成代码、不用本地 replace，不在本任务修改外部仓库。日志正文不持久化，Secret 编辑先显式取真实内容；列表使用下一页游标，不伪造 total。
