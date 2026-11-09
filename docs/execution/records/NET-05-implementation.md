# NET-05 实施与真实环境验收

2026-11-09 引用维护：本记录保留原时点的实施与验收结论。标为“历史定位”或“仓外定位”的原路径在当前 checkout 无法解析，仅保留追溯信息，证据可用性为 not_verified；不表示归档已恢复或旧规则仍适用。处理范围见[文档修复记录](res-doc-fix-20261109.md)。

本记录固定执行输入与用例，不替代 [当前执行状态](../status.md) 或 [规格](../../specs/vpc-subnet.md)。NET-05 已在用户调整后的范围内完成：实际产品主链、V-12 数据面、故障恢复、产品清理及临时环境撤销均通过。不得据此进入 NET-06。

发布说明：用户在验收完成后授权仅发布 Network main，ANI 继续保留本地。下文未提交描述对应原验收封存时点。公开证据副本仅对重复的非凭据元数据字段名作 [明确投影](NET-05/publication-projection.json)，原始与发布 hash 均保留，实际值/状态/UID/断言不变；未修改凭据扫描规则。

## 执行中范围调整

2026-09-10 用户告知并行任务正在调整 worker 资源持续观察，并允许本包不做这方面测试。因此停止扩展或重复持续观察专项，相关未重跑项不再作为 NET-05 完成阻断；保留此前 V-09 暂停/纯查询/stale 实测及其精确源码身份，不能把该结果冒称为并行任务实现的验收。创建/删除事务、租约栅栏与实例 owner 恢复仍继续。并行任务内容不导入本包。

本包在该调整前已复现新 Subnet 饥饿：已受理 Subnet 持续停留 version 1/lease epoch 0，同时发生 228 次 VPC Provider 观察，未发出其 Subnet 请求。最小修复统一按到期时间领取 VPC/Subnet，并保持先锁父 VPC 的顺序；真实 PG 回归先红后绿。该差异在未提交 diff 中单独保留，供持续观察任务协调，不扩建观察架构。

## 固定输入与执行位置

- Network：`5d4a53451beca0317bee1f577d8fb9cc189fc035`，tree `d4b45c908ba334c3c58f99dad0952e34aadfc588`，`/home/chabking/workspace/.worktrees/network-net-05`。
- ANI：`0363b6b5f906886c3ab2748ef68940c562b93382`，tree `113d664efc7baa96ab1e16bbd9a021ad612c999b`，`/home/chabking/workspace/.worktrees/ani-net-05`。
- 两个成果 worktree 均为 `codex/net-05`，保留未提交状态。原 worktree 的初始 dirty 内容、权限和摘要见 [initial-state.json](NET-05/initial-state.json)。
- 用户 Goal 文件 SHA-256：`e61f0b5377273648ed8873dc373e2e912f003efea3124fe11014a396414b05ed`。
- 远端 host `i-8yg2l7u8`，固定 `kind-kc062`，`kube-system` UID `a05787f7-fd36-482d-97ce-daef70e269c6`。三个节点和 12 个 kcn-system Pod 健康，见 [首次只读预检](NET-05/preflight.json)。
- kc 契约源码 `a2245883eb2b46a998f041feb3ad0ed3f6cf7c60`；v0.6.2 安装输入、导入镜像与实际 imageID 分别核对。源码到部署镜像构建来源仍为 `not_verified`。
- 原安装 hash `d1c69f5d980a0a81d5c71253db51537af08f9e369f88a0fece25c1caedfa0e97`；kind 适配安装 hash `c2c8454cdc589a49f7c5280ef4583fcc6c882129ca9d603e2eebf1ab699d2817`；实际 imageID `docker.io/library/import-2026-09-09@sha256:e2efcb27fd9b982ad6f49c0dc7b8d7ab9622bfa1172491c187152422cca27749`。

## 预先固定的执行约束

使用远端独立 `net05-<run-id>` 目录、两个独立空数据库及不同 migration owner/runtime 角色。运行服务监听 VM loopback 或本包 Docker internal 网络；每个请求显式传 `ANI_AUTH_MODE=dev` 的合法 tenant/actor UUID，属于未经验证的测试身份。业务对象只由实际产品 API 创建，不手工补网络注解、Provider 对象、submission 或终态。

小型拓扑：两个租户，租户 A 的 VPC A1 内两个子网、VPC A2 一个子网，租户 B 的 VPC B1 一个子网；A1/A2/B1 有意采用同 CIDR。候选 `10.205.0.0/16` 在写入前再次与实时 host/node/Pod/Service/kc 网段核对。产品 API 同时最多创建 10 个轻量 Pod，每个 50m CPU、64Mi 内存，根据实际节点选端点，不修改调度契约。

Network 凭据只能写两个专用 namespace 的 VPC/Subnet；只读跨 namespace 观测必要关联。ANI owner 可写本包 Deployment/Service/Secret，观测与删除本包控制器/Pod，不可写 Network CR/DB。runner exec 使用第三个身份。admin 仅用于 fixture 准备和核验。凭据、kubeconfig、Secret 正文仅保留远端私有目录。

## 验收矩阵与证据层

下表沿用执行前固定的用例断言。live 表示实际 main、真实 PostgreSQL、真实 Kubernetes/kc；overlay 表示真实装配加仅测试构建暂停点。PG/受控列不能替代 live。完整原始日志、对象 UID、版本/epoch、操作和永久 binding 在链接文件中；大文件以 `.gz` 无损保存，原始及压缩 hash 见 [归档索引](NET-05/export-index.json)。

| 规格 | 结果 | 实际断言与直接证据 |
|---|---|---|
| V-01 | pass | 固定生成、分层、编译、单元测试：最终 `make verify`；[门禁逐项退出码](NET-05/final-gates.json) |
| V-02 | pass | Network 正规 `-migrate`；独立两库和非 owner/no BYPASSRLS 角色；ANI 既有 RLS fixture；[迁移](NET-05/network-migrate.json)、[表约束](NET-05/network-tables.json)、[数据库权限](NET-05/database-permissions.json)及实际九路由两个租户读写 |
| V-03 | pass | 实际 Network runtime 跨租户 UPDATE 被复合 FK 以 23503 拒绝且事务前后不变；ANI runtime 跨租户 SELECT/UPDATE 均零行；[运行边界](NET-05/V-02-03-runtime-database-boundaries.json)，真实 PG tenant mutation 门禁检测遗漏谓词 |
| V-04 | pass | 实际 Gateway 并发单受理、永久重放、异意图 409、同键跨租户独立、删除墓碑后重放无新 CR；[永久受理](NET-05/api-permanent-acceptance.json)、[API 事件](NET-05/events.jsonl.gz) |
| V-05 | pass | 同 VPC 并发重叠仅一项 201，另一项 409；跨 VPC 同 CIDR 成功；错误父/字段/状态拒绝，保留 reserved/attached/releasing 时 Subnet 删除 409；live API 与 V-10 证据，PG 并发回归补足互斥 |
| V-06 | pass | T1 已持久受理，停止 Gateway 后 Network 无查询独立完成；[T1](NET-05/V-06-07-T1.json)。ANI 无 Network DB/CR 写权限，Network 无 Pod 写权限仍完成主链；[实际 SA 权限](NET-05/rbac.json)。实际产品清理见下文 |
| V-07 | pass | overlay 精确暂停在 T1 后、真实 Provider 201 后/T4 前及真实 DELETE 已发送/未确认；kill/restart 使用同库和 binding，UID/POST 计数不变，无伪终态；[T3/T4](NET-05/V-07-T3-T4.json)、[删除未确认](NET-05/V-07-delete-unconfirmed.json) |
| V-08 | pass | 两个真实 Network 进程持久 lease/epoch/version 竞争；旧 Work Finish 被真实 PostgreSQL 拒绝 `ErrLeaseLost`；[栅栏](NET-05/V-08-lease-fence.json)。旧真实 POST/DELETE 响应迟到不能重发/退回意图；[迟到请求](NET-05/V-08-11-late-real-requests.json) |
| V-09 | pass（执行时点）；后续专项豁免 | 原 overlay 暂停 worker，API GET/LIST/GetOperation 不改持久数据、不触发 Network Provider 调用；stale 可见且新 Prepare 拒绝；[纯查询](NET-05/V-09-pure-query.json)。用户随后将持续观察专项交并行任务，不追加或重跑此项，不评价对方实现 |
| V-10 | pass | 实际 owner reserved 保留、未知 Deployment POST 多次真实 404 不重发、丢 Confirm、错误 Pod UID/namespace 和跨租户拒绝；两端各自/同时重启；[reserved](NET-05/V-10-reserved-restarts.json)、[未知提交与 Confirm](NET-05/V-10-owner-unknown-confirm.json)。真实 Pod/VNic/IP 存在时维持 releasing；丢 Release/consumer 不可达后仍正确收敛，见下文 |
| V-11 | pass | 真实代理断连、真实 API 403、真实请求超时未知分别记录，不制造 Provider 结果；[失联](NET-05/V-11-unreachable.json)、[RBAC 拒绝](NET-05/V-11-real-rbac-denial.json)、[未知/迟到](NET-05/V-08-11-late-real-requests.json)。live 错 UID/namespace 不认领；PG/受控 actual adapter 的 foreign-owner/changed-UID/多关联对象负例仅列补充门禁证据 |
| V-12 | pass | 实际两个 worker、八 Pod 的带身份双向连通与重叠隔离；[拓扑](NET-05/topology.json)、[地址/路由](NET-05/pod-routes.json)、[完整请求与控制](NET-05/traffic.jsonl)，详见下表 |
| V-13 | pass（接口范围） | 实际 main 九路由、双页 cursor 及 tenant/kind/name/state/vpc 绑定、严格字段、跨租户统一拒绝；[分页](NET-05/api-pagination.json)。实际 `POST /api/v1/instances` 经过完整 bootstrap/identity/audit/dry-run/owner 链；[首次审计](NET-05/first-admission-audit.json)。无新增 fallback；Console 不在范围 |

V-14/V-15、Console/前端、外网可达性、性能/容量、物理多机 HA、生产升级与切换均 `not_verified`。固定 ANI 全历史 Atlas 与八条 authentication/branding compatibility 原门禁仍 `fail`；无新增兼容回归检查是独立 `pass`，不把原失败改成通过。

## 实际 main 修复及其范围

ANI 专项变更记录（仓外定位：`../../../../ani-net-05/repo/development-records/net-05-kind-network-validation.md`）列出精确回归。四项真实阻断均先复现再修复：明确 VPC/Subnet 的普通容器被旧默认 Attachment 拒绝；显式 APIHost 的 tokenFile 未加载导致 anonymous 403；实际 Deployment 省略空列表导致错误不一致；实际 typed LIST item 缺少 TypeMeta 导致 owner graph 无法确认。修复保持非空字段/错误类型/UID/租户校验，不改产品契约或认证模型。

Network 只修复已经在 live 阻塞创建的到期工作饥饿，改动为 `internal/data/resource_work.go`、两份现有 query 及 pinned sqlc 输出。先锁父 VPC，按 VPC/Subnet 最早 next_run_at 选父，再领取对应资源；没有引入 watch/informer 或新状态/迁移。真实 PG 回归 `TestNET05OldestSubnetWorkIsNotStarvedByDueVPCObservation` 先红后绿；证据 [饥饿现场](NET-05/V-06-subnet-starvation-before.json)和 `regressions/fairness-{red,green}.log`。该修复在用户豁免持续观察专项之前完成，未提交 diff 中独立可见。

仅测试 handler fixture 的九路由 × 五种 TenantContext 异常共 45 个用例返回 400/TENANT_REQUIRED 且零 Network RPC；现有 explicit instance 异常上下文测试验证零写入。它们与未改 main 的 dev 身份证据明确分开，不新增生产 auth 开关。

## 真实拓扑和数据面

本次 run `net05-1717d354`，CIDR `10.205.0.0/16`。租户 A `292e225a-1f85-4912-88e0-590dd5fa953d`，租户 B `2fc974a1-6930-4c24-b55f-86b2b142b510`；actor、namespace prefix、cluster_id、端口在 [固定 run contract](NET-05/run-contract.json)。两个 worker 的实际放置由产品调度取得，没有给 Pod 或节点打补丁。

| 域/Pod 名后缀 | worker | IP | 监听端口 | 产品 Subnet |
|---|---|---|---|---|
| A1 v2-p2 | worker2 | 10.205.1.5 | 18080 | A1s1 /24 |
| A1 v2-a1s1-p3 | worker | 10.205.1.6 | 18080 | A1s1 /24 |
| A1 v2-a1s1-p4 | worker | 10.205.1.7 | 18080 | A1s1 /24 |
| A1 v2-a1s2-p1 | worker2 | 10.205.2.3 | 18080 | A1s2 /24 |
| A2 v2-a2-p1 | worker | 10.205.1.4 | 18081 | A2s1 /24 |
| A2 v2-a2-p2 | worker2 | 10.205.1.5 | 18081 | A2s1 /24 |
| B1 v2-b1-p1 | worker | 10.205.1.4 | 18082 | B1s1 /24 |
| B1 v2-b1-p2 | worker2 | 10.205.1.5 | 18082 | B1s1 /24 |

A1/A2/B1 都使用 `10.205.0.0/16`，A1s1/A2s1/B1s1 都是 `10.205.1.0/24`。相同 IP 在不同 VPC 中真实存在。网关分别为产品子网 `.1`，Pod 内地址、路由与产品配置核对通过。完整 instance/Attachment/Pod UID、namespace、node、imageID 在 topology 和 traffic 的 source/target 中。

最终完整 V-12 在 `2026-09-10T02:39:31Z` 结束：A1 同子网同节点、同子网跨节点、同 VPC 跨子网均双向 HTTP 通过；A2、B1 各自正向控制通过。40 个跨域拒绝包括同租户跨 VPC 16、跨租户 A1/B1 16、跨租户 A2/B1 8，每个负例都紧邻两个域的实际正向控制，并记录目标端口和 nonce。每个成功响应必须匹配 fixture、预期 instance、hostname 和 nonce，不能把错误端点或自身响应算通过。不同域用不同端口，排除了重叠 IP 命中自身监听的假通过。

探测 v2 镜像 [固定构建与节点导入](NET-05/probe-image-b97b9f5889fe2977.json)：binary SHA-256 `b97b9f5889fe297727f9910cbe3c55cb821f4fbe6a12fccca5c2e18e31f8082f`，archive `d75acf21a7c5dc4079a3a1c667a51f383ec659e600bd44cf73186a3ff981f2ac`。后续 owner 故障 fixture 的 SIGTERM 等待镜像另有 [独立 hash](NET-05/probe-image-ffcf0c98acaedd82.json)，不覆盖 V-12 镜像身份。首次探测 fixture 使用了错误实例身份 env 名；第一批八 Pod 经产品删除完整回收后，使用正确 `ANI_WORKLOAD_ID` 的 v2 新逻辑实例重建。日志保留部分早期重复请求，最终 40 个拒绝不能简单按所有日志行计数推算。

## 事务、实例恢复和真实清理

故障构建来自 Go `-overlay`，原源码文件不改；[构建 manifest](NET-05/fault-build-manifest.json)包含命令、原文件/overlay/binary hash。T1/T3-T4、worker pause、owner prepared、Confirm/Release 和 consumer 窗口均为测试构建边界。真实 Provider 成功来自原 API；stale Work 用例仅延长 Go 调用 context 以让旧 Work 进入真实数据库 epoch/version 判定，不改持久 lease。

V-11 的 403 由临时撤销本包 Network Role 的 create 权限产生，UID 围栏恢复原 Role。固定 adapter 把它作为可重试 ProviderUnavailable，已知拒绝可清除 Pending；未知超时保留 Pending、禁止重发。此证据证明三类实际传输结果及持久行为，不宣称 403 是 terminal ProviderRejected。真实旧 POST 被延迟至多次 GET 404 后才送到 API，原资源 UID 被采纳，POST 次数仍一；旧 DELETE 响应迟到不能把新墓碑退回 deleting。

reserved 用例从真实 Prepare/owner prepared 开始；停止两端，再仅启动 Network 且 consumer 实际停止，未封闭 Attachment 保持 reserved/ConsumerUnavailable。证据中的 `wait_seconds:35` 是初始采样等待下限，不是总持续时间；后续重启及真实等待跨过多轮 lease，按前后时间戳核对。后期 owner fixture 使用既有 `ANI_WORKER_OBSERVE_EVERY=30s`，前期故障用例为 10s，freshness 仍 60s；配置预算变化没有改生产默认或实现新观察策略。

实际 Deployment POST 延迟产生 owner 未知提交，多次真实 404 后仍无第二个 POST；丢 Confirm 时 Network 自动发现同 Pod 并 attached，恢复 Confirm 后 owner 同步。错误 Pod UID 返回 AlreadyExists、错误 namespace 返回 FailedPrecondition、其他租户 GetAttachment 返回 NotFound，未写错误绑定。

删除恢复在 `03:18:48Z` 通过：[实际残留 Pod/NIC/IP](NET-05/V-10-real-residual-nic-ip.json)、[Release/consumer 重启](NET-05/V-10-release-consumer-restarts.json)、[最终相同 finalization identity](NET-05/V-10-final-owner-recovery.json)。最初两次只延迟 Pod 退出的尝试未捕获真实 NIC/IP 残留窗口，记 fixture 未触发，实例正常删除；最终改为延迟真实 owner Deployment DELETE 的转发，使 naturally created Pod/VNic/VNicIP 实际仍在。此时 owner closing、Network releasing、Subnet DELETE 409。放行真实 DELETE 后由 Kubernetes/kc 自然回收；即使丢 Release，consumer closed 与实际关联全消失仍由 Network 自行确认 released。没有剥 finalizer、删除 CR/网卡或写业务终态。

本基线产品实例删除路由实际是 `POST /api/v1/instances/{id}/lifecycle`，body `action=delete`，不是 HTTP DELETE。测试走这个既有真实产品接口，不新增路由。最后八个 v2 实例 `03:19:47Z` 完整 closed/released/identity revoked；随后所有测试意图（含前期失败/重放 fixture）按 Subnet→VPC 产品 DELETE 清理，`03:20:20Z` 通过：12 VPC、6 Subnet 墓碑、subnet_count 全零、无 pending binding/active operation，实际 CR/Deployment/RS/Pod/Service/Secret/VNic/VNicIP 全空。见 [最终产品所有权与清理](NET-05/product-resource-ownership-and-cleanup.json)、[八实例清理](NET-05/product-cleanup-20260910T031947Z.json)及 [完整数据库终态](NET-05/database-20260910T032017Z.json.gz)。

## 可重复入口、配置和执行证据

所有重构建/门禁均在 SSH `ubuntu`（实际 host `i-8yg2l7u8`），通过 `/home/ubuntu/.local/share/ani-network-service/env.sh`，Go 并发上限 2；未回退本地重测试。固定 kind 工具目录 `/home/ubuntu/workspace/ani-network-service-runs/kc-kind-20260909T144827Z`，每个 kubectl 命令显式 kubeconfig/context，写入前检查 kube-system UID。三个节点和 CNI 不重建/重启。

可重复入口是 [net05-remote](../../../scripts/net05-remote) 的两仓快照，加 [kind runner](../../../scripts/net05-kind.py)、[API 矩阵](../../../scripts/net05-api.py)、[故障构建](../../../scripts/net05-build-faults.py)、[故障阶段](../../../scripts/net05-faults.py)、[清理](../../../scripts/net05-cleanup.py)、[门禁](../../../scripts/net05-gates.py)和[证据导出](../../../scripts/net05-export.py)。它们是本包固定版本夹具，不是通用部署工具。以下说明复现顺序，本次已执行的各轮真实 argv/源码文件权限与 hash 以 `sources/` receipt、[执行命令索引](NET-05/execution-index.json)为准；不要对已清理 run 重放 prepare 或用新库冒充恢复。

```sh
# 通过 net05-remote 的独立远端快照执行；命令之间保持同一个 NET05_RUN_DIR。
cd "$NETWORK_SOURCE"
make build
(cd "$ANI_SOURCE/repo" && CGO_ENABLED=0 go build -trimpath -o ../ani-gateway-net05 ./services/ani-gateway)
python3 -B scripts/net05-kind.py prepare
python3 -B scripts/net05-kind.py start
python3 -B scripts/net05-kind.py first-network
python3 -B scripts/net05-kind.py first-subnet
python3 -B scripts/net05-kind.py image
python3 -B scripts/net05-kind.py first-instance
python3 -B scripts/net05-kind.py topology
python3 -B scripts/net05-kind.py traffic
python3 -B scripts/net05-api.py
python3 -B scripts/net05-build-faults.py
CGO_ENABLED=0 go build -trimpath -o "$NETWORK_SOURCE/../fault-build/rpc" ./tests/net05/rpc
python3 -B scripts/net05-faults.py setup --build "$NETWORK_SOURCE/../fault-build"
# 顺序执行 transactions、lease、unknown、provider、owner、owner-delete。
# queries 是已有 V-09 可复现入口；本次用户调整后没有再次执行。
python3 -B scripts/net05-cleanup.py permissions
python3 -B scripts/net05-cleanup.py products
python3 -B scripts/net05-gates.py
python3 -B scripts/net05-export.py
python3 -B scripts/net05-cleanup.py fixtures
```

运行目录 `/home/ubuntu/workspace/ani-network-service-runs/net05-20260910T015345Z-665860d8` 与后续源码快照目录分开：重启始终沿用此私有 state 的数据库、角色、cluster/prefix、cursor key 与永久标识。每个快照先校验文件 bytes/mode，再创建用户授权的远端临时验证提交；两个本地成果 worktree HEAD 始终保持固定基线。`NET-05/` 原始记录与并行 KC 输入不装入业务源码 snapshot，防止递归证据混入源码身份。

Network 使用 `ANI_NETWORK_DATABASE_DSN`、`ANI_NETWORK_CURSOR_SIGNING_KEY`、`ANI_NETWORK_INSTANCE_CONSUMER_ENDPOINT`、`ANI_NETWORK_CLUSTER_ID/NAMESPACE_PREFIX`、`ANI_NETWORK_KUBECONFIG`；迁移通过 `ANI_NETWORK_MIGRATION_DSN` 与 `ANI_NETWORK_RUNTIME_ROLE`。Gateway 使用 `NETWORK_RPC_ENDPOINT/TIMEOUT`、`NETWORK_INSTANCE_CLUSTER_ID/NAMESPACE_PREFIX`、`NETWORK_CONSUMER_LISTEN`、`DATABASE_URL`、`WORKLOAD_PROVIDER=kubernetes_rest`、`WORKLOAD_PROVIDER_APPLY_ENABLED=true`、APIHost/tokenFile/CA、真实 main 所需 Redis。配置值中的凭据只在 VM 私有目录，未进入源码/证据。

PostgreSQL 18.6 镜像固定 `sha256:4ef4dbc939d61acea57712655ddb4b4ab27419c913f94cca0cd57cb3ea3c2280`，内存 768Mi；main 各 512Mi/1CPU，共享数据库容器的隔离 network namespace，测试第二个 Network 最多同时一份。运行角色不能跨库 CONNECT 或 DDL；ANI 不以 admin 运行。独立 TLS relay 保留原 API CA/SAN 和每个 runtime 原 SA，runner exec 不授给 Network。

ANI [schema 提取来源](NET-05/schema/ani-schema-sources.json)与 [合成 fixture SQL](NET-05/schema/ani-schema.sql)均来自固定 ANI 已有 DDL；原 owner migration SHA-256 `a34f6d5ee844b46d0edea270fc501c72ba35e3b75b212e00d19685839030af51` 原样应用。只初始化 tenant/actor，api_keys/metadata audit 通过真实实例调用产生并正常撤销。该证明不替代全历史 Atlas 重放。

## 最终门禁、源码和收尾

最终门禁 [逐项命令/时间/退出码](NET-05/final-gates.json)：Network verify、真实 PG race、tenant mutations、audit；ANI NET-05 回归、make test、architecture/doc entrypoints、Network/OpenAPI/services route 契约检查、既有无新增兼容检查全部 `pass`。audit 首次在生成 SBOM 前因只读核验导入 Python 产生的 `scripts/__pycache__` 触发 clean-source 拒绝；仅删除该核验缓存后在新干净验证快照重跑通过，首次日志与退出码仍保留于 previous_attempt。不是依赖漏洞或隐藏测试失败。

原八条 compatibility 仍实际 exit 2 / `fail`，独立 checker 证明仅这些既有失败。Atlas 固定基线 checksum/重复版本仍 `fail`；本次远端 env 未提供固定 Atlas，发现的共享工具摘要与历史记录不同，未执行该候选工具、未改迁移。该项是继承的已接受失败，不冒称本次已重跑，详见 [Atlas 边界补充](NET-05/final-gates-with-atlas-boundary.json)。

[运行 binary 与两轮 overlay 身份](NET-05/runtime-build-identities.json)区分普通 main、前期 T1/T4/lease/V-09 构建、后期包含最小公平性修复的 owner 构建。V-12 和 V-09 是其执行时点证据；后期公平性修复经过真实 PG/race/变异及实际 owner/删除恢复，依用户调整不重复持续观察专项。所有生产代码与最终门禁快照相同；最终文档/导出器变化经轻量复核，最后源文件列表/权限/hash、两仓 diff、SBOM 和链接检查见 [最终交付审计](NET-05/final-source-audit.json)和[收尾门禁](NET-05/finalization-gates.json)。

`03:34:02Z` [fixture 清理](NET-05/fixture-cleanup.json)通过：50 项本包容器/进程/RBAC/SA/空 namespace 已按身份台账和 UID 前置条件移除，最后删除专用数据库、internal Docker 网络和私有 recovery 目录。产品资源事先已 released/closed，未强拆未知提交。三节点、12 个 CNI Pod 的 UID/imageID/restartCount 与前测一致；[环境前后比较](NET-05/environment-comparison.json)显示原有 250 项 inventory 的名称/UID/标签完整保留，无新增或丢失，固定安装输入 hash 和健康均通过。

[证据导出检查](NET-05/export-scan.json)核对实际数据库密码、cursor key、三个 SA token 及 19 份真实持久 workload Secret 的原值/base64，并拒绝 Secret 正文/私钥。凭据和原始进程日志未复制；大量 JSON/JSONL 无损压缩，其原值/压缩 hash 在 export-index。清理后新增收尾记录只含公开源码/环境/退出码。

[原 worktree 对照](NET-05/original-worktrees-preserved.json)：ANI NET-02–04 HEAD、初始 dirty 内容与权限/hash 均不变；Network 原 checkout 的三份文档在用户所述并行任务期间变化，精确末态 hash 单列，NET-05 未导入、覆盖或撤回这些变化。两个成果 worktree 均保持固定 base HEAD 和未提交 diff，不提交、推送、PR、打标签或发布镜像；远端临时验证提交仅用于可复现门禁。

保留原 kind/CNI、工具、镜像缓存和源码/脱敏证据快照。NET-06、NET-AUTH、前端、VM/IAM S2S 与整体切换没有启动。
