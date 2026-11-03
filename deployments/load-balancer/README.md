# 隔离 LB 产品 API 操作

本手册保留 NET-VPC-LB-02 历史 ubuntu 独立 run 的 fixture 命令与请求，不代表本轮已执行。当前任务的重操作仅经 `ssh fedora`，真实 Kubernetes 仅使用用户指定的 172.16.101.10–12 集群，并以本次实际核验的 context、API Server 和 `kube-system` UID 为准。产品契约以 [统一规格](../../docs/specs/vpc-connectivity-lb.md) 为准，当前完成度见 [执行状态](../../docs/execution/status.md)。下面的操作步骤不是已经取得的流量证据。

## 启动前提

1. 先核验当前 172.16.101.10–12 集群身份。历史[用户恢复输入](../../docs/execution/records/NET-VPC-LB-02/resume-20260915.md)记录的 `kubernetes-admin@ani-platform`、`https://api.ani.internal:6443` 和 UID `be57b911-892c-4e75-aa9d-4a05d819c59e` 仅用于追溯。运行 [lb-preflight](../../scripts/lb-preflight) 时显式传入本次核验的 context 和本 run 的 kubeconfig，保存实际集群 UID、只读对象、节点、镜像和拓扑快照。
2. 保留原 kind 的 `lb-strict-0911` 和新集群的 `lb-validation`；至少两个 worker 必须有足够 requests 预算安排完整 small 的两副本 Envoy（每副本 Envoy 1 CPU/1 GiB，另计 shutdown-manager、客户端与业务 Pod），不靠改副本、requests 或控制平面污点通过检查。三种 exposure 可串行运行。
3. 安装须具备 GatewayNamespace、Backend 和 EnvoyPatchPolicy 扩展、所需跨 namespace 和 TokenReview 权限，以及经过确认的 kc/EG/Envoy/shutdown imageID。平台维护一组公共 `GatewayClass/lb-common` 与 `envoy-gateway-system` 内的 `EnvoyProxy/lb-common`；每个 LB 的 Gateway 引用同租户 namespace 内的专属 EnvoyProxy。专属 Proxy 设置 `mergeType: StrategicMerge`，small/medium/large 三档均按[固定容量合同](../../docs/specs/vpc-connectivity-lb.md#62-provider-映射)显式填写副本、CPU/内存 requests/limits 和 Service 类型；公共 Proxy 提供镜像与调度等公共配置，不设置容量或 Service 类型。完整 imageID 按运行值逐字匹配；裸 sha256 可能是配置摘要，不能冒充可拉取的镜像 manifest。源码到运行 digest 的构建关联另行留证；版本标签不代表合格。
4. 按本轮 `ssh fedora` 要求和[远端执行约定](../../docs/remote-execution.md)，使用独立源码归档、精确 manifest、共享锁和资源限额；本手册中的 ubuntu 路径及 `scripts/snat-remote` 历史用法仅保留复现用途。数据库与 Kubernetes 调度分别核算。运行数据库名使用 `net_vpc_lb_02_<唯一后缀>`，独立 owner/runtime 角色；凭据仅留在 run 的私有文件/环境中，不纳入源码归档。需要导入集群的本 run 产物从 Fedora 导出、经本机中转；每次传递核对哈希并记录实际导入 imageID。
5. LB 激活核验 VPC/Subnet 自身 Provider 的新鲜状态、UID 与当前代次；不要求基础 Intranet EIP/SNAT 或等待其就绪。公网入口按 [Public 操作手册](../../docs/kc-public-egress-manual.md)准备本 run 所需地址池/网关和已分配的独立 Public EIP。实测验证记录与分配开关只作用于独立数据库。不得复制历史验证记录充当本次实测。

必要额外权限见 [network-rbac.yaml](network-rbac.yaml)。渲染唯一名称并绑定本 run 的 ServiceAccount；仍需原有 Egress/Attachment 观察权限。专属 EnvoyProxy 的写权限通过租户 namespace 内的 Role/RoleBinding 授予；公共 `lb-common` Class/Proxy 保持只读。Network 写自己的专属 Proxy、Backend、Gateway、Route、Policy，生成 Service、Deployment 等仅授权读取。模板不自动修改任何共享绑定。

## 构建与安装输入冻结

下面保留历史 **ubuntu 独立 run/source** 的命令及 `/home/ubuntu/.local/share/ani-network-service/env.sh` 环境来源。本轮在 Fedora 的独立 source 中使用已核验的固定工具链；本地仅编辑与回传审查。路径变量均指本 run 的私有目录。

```bash
go build -trimpath -o "$LB_RUN/lb-api" ./scripts/lb-api
go build -trimpath -o "$LB_RUN/network" ./cmd/ani-resource-service
"$LB_RUN/lb-api" installation -kubeconfig "$KUBECONFIG" -output "$LB_RUN/installation.json"
```

本轮调用 `installation` 时显式传入本次核验的 `-expected-context` 与 `-expected-cluster-uid`。该命令只读取公共 `lb-common` 等安装对象和集群 UID，保存完整 spec/data、UID、状态和 canonical SHA-256；不写集群，也不宣布 ready。输出文件不覆盖。审阅该内容与原 install 的差异，再把 `fingerprint` 和只读 Pod 状态中固定的四个 imageID 写入独立实例配置：

```yaml
network:
  load_balancer:
    enable_isolated_api: true
    installation_fingerprint: <installation.json 的 fingerprint>
    controller_image_id: <实际 Envoy Gateway 完整 imageID，sha256 或仓库名@sha256>
    envoy_image_id: <实际 Envoy 完整 imageID，sha256 或仓库名@sha256>
    shutdown_image_id: <实际 shutdown-manager 完整 imageID，sha256 或仓库名@sha256>
    kc_image_id: <实际 kc 完整 imageID，sha256 或仓库名@sha256>
```

此片段追加到该 run 的完整有效配置中。默认配置不开放 LB。能力由共享观察器重新核验，手填期望指纹不绕过检查；mutable Envoy tag 还要求现有 owner 链的运行 imageID 证明。若安装对象变化，先保存差异并确认其来源，不自动刷新期望值掩盖漂移。

## 固定调用上下文与实例 owner

先运行正常 Network 服务的迁移及标准进程，再启动显式 fixture。标准服务缺少可信上下文时仍拒绝 Egress/LB。fixture 不建立第二个 worker，不替代事务或生成产品 CR。

```bash
mkdir -m 700 "$LB_RUN/sockets"
"$LB_RUN/lb-api" owner -socket "$LB_RUN/sockets/owner.sock" -registry "$LB_RUN/owner.json"
"$LB_RUN/lb-api" serve -conf "$LB_RUN/config.yaml" -database "$LB_DATABASE" -tenant "$TENANT_A" -socket "$LB_RUN/sockets/tenant-a.sock"
"$LB_RUN/lb-api" serve -conf "$LB_RUN/config.yaml" -database "$LB_DATABASE" -tenant "$TENANT_B" -socket "$LB_RUN/sockets/tenant-b.sock"
"$LB_RUN/lb-api" serve -conf "$LB_RUN/config.yaml" -database "$LB_DATABASE" -platform -socket "$LB_RUN/sockets/platform.sock"
```

四个命令分别作为本 run 的受限进程运行并记录 PID；socket 为 0600，父目录 0700。已存在的 socket 不覆盖。管理员 socket 只注册 PlatformNetworkService，不隐式委托租户；租户 socket 固定一个 UUID，不从 header/body 生成管理员身份，并约束旧 NetworkService 的显式 tenant_id。`target_tenant_id` 仍经过真实领域授权。客户端支持这五个固定服务的 unary RPC，JSON 未知字段会拒绝，非 OK 返回非零退出码并在 stdout 保留结构化 status/details。

`owner.json` 是 0600 的 JSON 数组，每项使用真实 `GetSubmissionResponse` proto JSON。初始可为空数组。业务 fixture 调用 `PrepareAttachment` 后，按返回 plan 创建自己的 Pod/VM，在读到实际 Pod UID 后原子更新对应提交记录。配置 `instance_consumer_endpoint: unix:///.../owner.sock` 指向该 endpoint；填写租户、实例、Attachment、submission、generation、cluster、namespace 和实际 Pod/controller UID。封闭时填写本次 finalization ID，按原 [Attachment 协议](../../docs/specs/vpc-subnet.md) 进入 closing/closed，不能提前伪造 owner closed。owner fixture 不写 Network 表、不创建 Backend/Gateway 等产品 CR。

## VPC、后端和三类入口

通用调用方式：

```bash
"$LB_RUN/lb-api" call -target "unix://$LB_RUN/sockets/tenant-a.sock" -service NetworkService -method CreateVPC < "$LB_RUN/requests/vpc.json" > "$LB_RUN/responses/vpc.json"
"$LB_RUN/lb-api" call -target "unix://$LB_RUN/sockets/tenant-a.sock" -method CreateLoadBalancer < "$LB_RUN/requests/private.json" > "$LB_RUN/responses/private.json"
```

先经 NetworkService 创建 VPC、入口 Subnet、后端 Subnet，取得这些父对象自身 Provider 的新鲜事实；LB 不等待基础 Intranet EIP/SNAT。至少两个实际后端放在两个 worker；经 Prepare/owner 协议接入，再将经过 Attachment/VNicIP UID 核验的地址作为 LB 成员。客户端也通过 Attachment 接入目标 VPC。业务响应包含 run 标识、后端名称和节点。

private 请求模板（ID/地址须替换为本 run 实际值）：

```json
{
  "name": "run-private",
  "vpc_id": "<vpc_id>",
  "subnet_id": "<entry_subnet_id>",
  "exposure": "LOAD_BALANCER_EXPOSURE_PRIVATE",
  "private_ip": "<无冲突的入口 VIP>",
  "idempotency_key": "<run>-private",
  "health_check": {"port": 8080},
  "backends": [
    {"subnet_id": "<backend_subnet_id>", "address": "<node-a 后端实际 IP>", "port": 8080},
    {"subnet_id": "<backend_subnet_id>", "address": "<node-b 后端实际 IP>", "port": 8080}
  ]
}
```

| 类型 | 请求差异 | 必需入口验证 |
|---|---|---|
| private | 不填 public_eip_id | 同 VPC 客户端直接访问 VIP:8080，识别两个节点的后端 |
| public | exposure 为 PUBLIC，移除 private_ip，public_eip_id 为经 CreateEIP 获得的独立 Public EIP | VPC 外客户端直接访问 EIP:8080；先证明 VPC 没有 Public SNAT binding |
| public_private | exposure 为 PUBLIC_PRIVATE，同时填写两种地址 | EIP、VIP 同时有效，均识别两个后端 |

公开请求的 enum 全名分别为 `LOAD_BALANCER_EXPOSURE_PUBLIC`、`LOAD_BALANCER_EXPOSURE_PUBLIC_PRIVATE`。默认 HTTP/8080、权重 1 和健康参数来自产品契约；显式权重 0 保留成员。公网 LB 与 Public SNAT 必须使用不同 EIP。

查询用 `GetLoadBalancer {"load_balancer_id":"..."}`、`ListLoadBalancers {"limit":20}`、`GetLoadBalancerOperation {"operation_id":"..."}`。保存每次原始请求/响应、operation 和版本；GET/List 纯读。`configured`/operation 成功不是 HTTP 健康，产品 `data_plane_state=unknown` 不能手改。

## 更新、故障和组合验收

更新使用最新资源 `version` 作为 `expected_version`，附新幂等键、名称/描述、**完整**后端集合和支持的健康参数。保留成员 ID 时地址身份、Subnet、端口不能变化；需要新身份时使用新成员。删除/版本冲突必须返回稳定错误，原键重放返回原收据。权重 0 示例：

```json
{
  "load_balancer_id": "<lb_id>",
  "expected_version": "<最新 version>",
  "name": "run-weight-zero",
  "idempotency_key": "<run>-update-1",
  "backends": [
    {"id":"<member-a>","subnet_id":"<subnet>","address":"<ip-a>","port":8080,"weight":0},
    {"id":"<member-b>","subnet_id":"<subnet>","address":"<ip-b>","port":8080,"weight":1}
  ]
}
```

逐项记录并核验：Public SNAT 启用/停用/重启/解绑期间基础 EIP/SNAT 身份和私网流量不变；真实出站连接及远端看到的源地址；后端替换、权重 0、TCP 监听失败/恢复的真实转发变化；同 EIP 竞争、租户/namespace/UID 错误及合法/外来 Service 占用；本 run 的旧形态 VPC 补齐、暂停恢复和删除竞争。仅对本 run 的服务/代理/对象注入故障。流量采用直接 VIP/EIP，不用 NodePort、port-forward、Pod IP 或手工产品 CR 替代。kind 实验 EIP、实验出站和互联网分别记账。

U08 复用 [第一批补齐手册](../../docs/execution/records/NET-VPC-BASE-01/backfill.md)，只处理本 run 创建的旧形态 fixture。每个验证项同时保存 cluster UID、安装指纹、imageID、源码 manifest、API 收据、可识别业务响应及时间；无法执行的条目标明 not_verified 和缺失前提。

## 删除与清理

先调用 `DeleteLoadBalancer {"load_balancer_id":"..."}` 并等待持久 delete operation。实际顺序是撤 Route/Policy、删 Gateway、确认生成资源与地址释放、清自有 Backend，最后删除专属 EnvoyProxy；公共 `lb-common` GatewayClass/EnvoyProxy 保留。部分创建/未知写入仍保留同一占用。finalizer 卡住应保存精确对象 UID/pending 状态和阻塞，不去掉 finalizer。

确认原 Public EIP 仍存在且可再次绑定、VIP 意图和 LB Subnet 引用释放、业务后端保留；原有基础 Intranet 资源也应保留，未配置基础连接的 LB 无须补建。再经 Public SNAT 解绑、EIP 删除、owner 封闭/Attachment 释放、Subnet/VPC 删除清理其余产品资源。核验本 run 存在的系统基础资源自动消失和登记记录匹配后，才能停止 run 进程、删除独立 DB 和基础 fixture。不能先删 namespace/DB 掩盖残留。

## 第三批接入门槛

U10/U11/U12 消费当前 Proto、错误和异步 operation；客户端以 `binding_state`/`binding_target` 判断 EIP 占用，LB 场景旧 `binding_id` 为空。完成该兼容适配前不向现有租户入口开放 LB。真实 IAM/ANI Gateway/Console、生产部署和正式存量迁移分别独立验证。此 fixture 的固定身份不是它们的认证方案。
