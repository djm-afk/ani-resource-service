# 2026-11-04：Res 网络状态消费简化实施记录

<a id="takeover-delivery-20261104"></a>

## 接手交付：生成、协调迁移及 Resource 现场主流程

本节为后续实际执行结果，取代下文恢复时点的“未生成、未部署”状态；历史记录保留。用户指定所有重任务在 `ubuntu@100.64.128.15`，集群操作在 `kubercloud@100.64.128.12`；随后明确要求后续改动不跑全量门禁，网络连接失败直接报告，不自行排查。

### 实际交付

- 延续 PR #57 的 `work/network-status-authority` 分支及 main 基线 `5dc539d92e3150a3593bde3668082753aa4fdebe`，未覆盖打包机原 checkout。按固定版本运行 protobuf、sqlc、typed config 生成与 gofmt，修正 sqlc 严格 ORDER BY、JSON SQL 参数类型及嵌入行字段引用的真实编译问题。
- 补修非终态 Subnet 的 VM NAD/ServiceAccount 准备及受原 lease/版本/绑定保护的延迟重试，不再用 Native Ready 阻挡准备；失败不覆盖已经成功的网络投影或历史操作。
- verification 归档使用独立 Succeeded operation 满足现有三列外键，资源继续指向原运行操作；VNIC 空原生 IP 输出使用非 NULL 数组。身份和 Namespace 负向约束保留，正向测试明确提供外部身份边界。
- 生成/构建源码为 `8ee93f14118deede6e9476c53ea6cd283077201b`，完整 tree 为 `ed0db2b0f7283ef82d25592f245b36aa456e865c`。Git SSH 推送失败后直接报告，通过 GitHub 连接器提交同一完整 tree 为 `9b8e81ecdd6316ee777ef13271f8f019a3c86d47`，以 expected-head 检查快进更新原分支；没有排查连接或 force-push。两份提交仅提交对象身份不同，源码 tree 完全相同。

### 镜像、迁移与配置

| 项目 | 实际结果 |
| --- | --- |
| 构建/推送 | `.15` 构建固定源码、linux/amd64；Harbor 推送及 manifest/config/source/snapshot 回读 PASS，临时认证材料删除。 |
| 最终镜像 | `10.10.1.66:30003/ani-gpu-platform/ani-resource-service@sha256:c6f095b28f87897ccdb43e5347d9855315ab69bcecaea2eebe86d2214f95b3e5` |
| 最终输入快照 | `sha256:29a1b3dd4cb535207dbc22f504c19d2ceeadb48f38ea6ccfd647f1be8e9c83a2`；版本 `network-authority-20261104-54aebd9`。 |
| 集群身份 | `kubernetes-admin@ani-kcn-full`，kube-system UID `3eeb5379-8748-4bf8-8f6d-d7a48ce3bc24`；Deployment UID `9b17ed7e-6521-40e8-8bf5-70a44237e501`；只发布 `ani-system/ani-resource-service` 的 `resource` 容器。 |
| 现场迁移 | 用户报告的故障时账本为 28；本轮首次复核时现场已到 canonical31、旧镜像 Ready，因此没有重复执行 0029～0031。逐项校验 1～31 checksum 后，只应用 0032。 |
| 停写与备份 | 旧 Resource 缩至 0，旧 Pod 消失，`resource_runtime` 连接为 0。备份 Deployment/RBAC/配套 Secret 和全库 custom dump，服务端 TOC 校验通过。最终切换备份 5,702,210 bytes，SHA256 `e4ee7acaff1e888f1aa22a1a61efc55850e41df2374ca9345ef6d535a8b70756`。此前同库备份已在 `.15` 独占 PG17.11 容器实际恢复并执行 28→32；最终备份未再次重跑恢复。 |
| 首次失败 | 0032 遇到 `network_security_group_reconciliations` owner 不匹配，整个迁移事务回滚，确认仍为31后恢复旧 Resource。保留失败日志和备份，没有将失败记作通过。 |
| 所有权修正 | 再次停写并备份后，仅将六张 `network_security_group_*` 表及 `network_vnics`、`network_vnic_bindings` 从 postgres 转给 `resource_migrate`，不改其他 schema；由该迁移身份执行成功。保留原始所有权记录。 |
| 数据保持 | 迁移前后 VPC、绑定原身份、幂等回执及 Network/Platform 操作历史逐项 JSON 比对一致；32 旧 lease 失效。未调高 QPS/Burst。 |
| RBAC | 应用匹配 egress ClusterRole、LB ClusterRole 及 Resource SA/Binding；未应用 LB 文件的租户占位 Role。移除退休清场/生成资源扫描权限，保留原声明写入和必要读取。 |
| 启动兼容问题 | 新镜像严格解码拒绝现场 `Service.ImageUpload`，错误被汇总成 managed compute configuration。核实 DB 路径和 ClusterID 一致后，保留 `ani-vm-resource-runtime-20261025` 原 Secret，为此次镜像创建 `ani-vm-resource-pr57-20261104`，仅移除该不支持的配置键。当前 main 和 PR 分支均不包含该字段对应能力，VM 镜像上传能力未验证。其他 VM 配置和密钥保持。 |
| 最终状态 | 新 Pod `ani-resource-service-7f8654cb88-8hn5p` Running/Ready，generation/observed173，restart0；schema32。最终镜像更新后正式认证 VM/VPC/Subnet/媒体/预设查询200。 |

执行证据分别保存在 `.15:/home/ubuntu/workspace/resource-pr57-20261104` 和 `.12:/home/kubercloud/resource-pr57-20261104`，备份/Secret 文件仅留在私有目录，不入 Git。32 已提交，不允许单独切回依赖旧列的二进制；故障优先前滚，恢复需配套备份和旧配置/镜像。

### 正式认证业务验收

通过现有租户的正常登录接口取得 token，退出登录均200；没有绕过鉴权、手工插入业务数据或操作原有用户 VM。临时 VM 名为 `codex-resource-pr57-20261104`，ID `c3dfc350-dc51-4d37-a796-c8e01d70a26e`。

| 项目 | 结果 | 实际后置状态 |
| --- | --- | --- |
| 读取 VM/VPC/Subnet/媒体/预设 | PASS | 公开接口200；子网 available。 |
| VM 创建并启动 | PASS | operation `661b185e-9033-4ca7-8297-39959f092ffd` succeeded，VM Running，主响应地址 `10.128.0.23`。 |
| VM 关机 | PASS | operation `44fca0f9-21a5-48b3-add1-0bada67fbdf3` succeeded，VM Stopped，当前地址清空。 |
| VM 再次启动 | PASS | operation `04208ba5-7910-454a-ab94-1c456198819b` succeeded，VM Running，主响应地址 `10.128.0.24`。 |
| VM 删除 | PASS | operation `c42367e2-bd0e-416f-b912-92e452634626` succeeded，管理状态 Deleted；未以202作为完成证据。 |
| VNIC 创建/删除 | PASS | 旧 Gov 入口400后，用户明确追加升级 Gov 授权；更新最新 main 及目录/权限后，VNIC `vnic_8384e24673bf4db59c8f41db0531096f` 实际 available，地址 `10.128.0.25`、MAC `fa:00:49:2e:21:ff`，删除后实际 Deleted、地址清空。创建 operation `779a5f04-d188-4e69-b0cd-14abd6408426`，删除 operation `51ae44e9-11ec-4be1-9772-fd87d523ee0e`；临时 VNIC 已清理。 |
| VM 独立网卡详情 | NOT_VERIFIED | Governance `/api/v1/virtual-machines/{id}/network-interfaces` 返回404；主 VM 响应的地址展示已验证，不能将两者混写。 |
| Guest 内关机/网络故障注入 | NOT_RUN | 本轮只验收平台启停；没有 KCN 故障注入或物理网络审计。 |

源码8ee93f1的稳态窗口605.561秒：Network provider传输309次、0.5103 QPS，LIST0；34个租户删除任务、4个平台删除任务、32条Released Attachment的epoch/停放记录均未改变，62个稳定任务停放、有限时刻待调度任务0。这里只统计带测量 transport 的 Network provider，不把它当成所有独立 Kubernetes 客户端的全局请求预算；没有调高QPS/Burst。当前 informer 可用 Watch 初始事件同步，不能要求必然出现初始 LIST 才认为计数有效。

### 现场主流程之后的单点重试修复

两项 NAD PG 失败暴露一处实际问题：`Finish` 总会将 Subnet version 加一，随后 `RetryVMNetworkPreparation` 却用原 version 条件写，导致首次 NAD 创建响应丢失后的重试被误判为旧工作。改为精确匹配完成后的 `original_version+1`，保留 lease epoch、binding、删除状态和版本条件，不接受更新的计算/删除任务。`TestVMNetworkPreparationRetryIsFencedByLeaseAndDeletion` 在78ee8fd通过，继续验证新 lease 和 deleting 产品不能被旧任务重排。

另两项修正只涉及外部测试边界：启动 wake 可能先领取独立 VPC 工作，测试需先让该工作停放；第二租户 Namespace 必须由平台边界预置。Instance Notebook 用例复用已有真实 Image Runtime/PG 和 Secret writer 设施，避免未装配 Image Ensure 导致 `PROVIDER_MUTATION_FAILED`。未修改生产身份或存储守卫。只复验上述失败/未执行项，没有重复运行已通过门禁。

最终修复源码为 `54aebd9509cda0ca893eafab1c841208cbe35633`，GitHub同tree提交为 `ae6e430837253275dfb0a7a05b5f39d53efaf3de`，完整tree均为 `c78819fea2a4904cbe92b9d0692a0d18cf2df305`。镜像构建327.342秒，Harbor推送1.805秒并回读 source/platform/config/snapshot；只更新 Resource 镜像，没有再次迁移或变更其他服务。最终digest和快照见上表。

上述临时VM完整启停/删除在8ee93f1镜像执行；最终54aebd9的生产差异仅为 NAD 失败后的重试版本条件和注释，VM原生执行代码未改，复用该主流程证据，并执行对应真实PG回归。最终54aebd9已验证正式认证查询，未重复新建第二台临时VM。最终Pod的稳态窗口另行记录，不混用两个Pod的计数。

### 生成和回归的真实边界

| 检查 | 结果与来源 |
| --- | --- |
| 固定工具 `make generate` / gofmt | PASS，在 `.15` 执行，生成物正常入原 PR。 |
| 生产编译与镜像构建 | PASS，固定源码8ee93f1，完整服务真实编译，不是仅编译测试桩。 |
| Network 定向单元回归 | PASS，`unit-focused`，源码bfc198a；后续8ee93f1仅格式化。 |
| 28→32、31→32、canonical28 / released30 桥接 | PASS，`network-focused-172701.log`；保留身份、意图/幂等/历史、当前地址及旧 lease 失效断言。 |
| Network PG 定向回归 | PASS：fencing在78ee8fd通过；`TestVMAutoNetworkPGBackfillsExistingSubnetAndPinsAcrossRestart`在54aebd9通过。原 `network-focused-172701.log` 及中间失败日志保留；只复验相关失败，未重跑全部包。 |
| Instance PG 定向回归 | PASS：`TestInstanceNetworkVerticalPGNotebookTwoRestartsAndRecreation`、`TestInstanceNetworkVerticalConfirmRetryRequiresDurableAck`在54aebd9通过。原 `instance-focused-172758.log` 及未设置任务TMPDIR的启动失败保留；最终日志为 `instance-vertical-pg` 的54aebd9 receipt。 |
| 其他历史检查 | 八个相关包单元测试曾在7703bd0通过；tagged VM PG 曾在2505ee3通过。它们不是最终源码的全套重跑。初始全量相关 PG 失败日志保留，未重试至绿。 |
| 全量门禁 / CI | `make verify` NOT_RUN；按用户后续要求不再跑全量门禁，不重跑历史失败 CI，后续提交含 `[skip ci]`。 |

### 最终稳态与追加授权的 Governance 发布

最终54aebd9 Pod窗口340.238秒：Network provider传输154次、0.4526 QPS，LIST0；34个租户删除任务、4个平台删除任务、32条Released Attachment的epoch/停放记录均未改变，稳定任务62、有限时刻调度任务0。该窗口结束后才进行 VNIC 创建删除，不把正常用户变更流量混作静稳采集。

用户随后明确授权 Governance 升级。固定最新 main `3a30c697b5c812226e3d13ea7bb24c6cfdea2c9a`（已合入PR #88，tree `9c46fb7586595b0ecd39417d69e1b188f7c79878`），在 `.15` 使用固定Go1.26.10和国内GOPROXY编译 server/admin，再以缓存的固定distroless运行时打包；没有改 Gov 源码或跑全量测试。源码归档快照 `sha256:1e0a965e726e5f6902183364f4b39a64e4079fb02647a21f16bdb18845ea0ac6`。

- 服务镜像：`10.10.1.66:30003/ani-gpu-platform/ani-governance@sha256:a035f93883068ee85998410863ae2d9f56e3d3f5782db608510d1769eebffed7`，Harbor回读校验通过，替换原 `dataset-dev-20261104` 镜像；保留原Deployment/配置和回退digest。只更新主Gov，未更新其独立sandbox Deployment、Console或其他服务。
- 配套admin镜像：`sha256:c9474bf06c633ab4e7c344a399273db8b3e6349c33ee0ea3ce607fb51171191c`，只用于一次性目录任务。先成功执行 `sync-apis --dry-run`，保存预览，再显式 `sync-apis`；旧API不删除、不重新编号，权限关系不重建。同步前备份三张目录/权限表及角色权限关系，TOC验证通过。未执行首次播种或业务schema迁移。
- 按固定源码的 `register-vnic-permissions.sql` 登记9个新接口/权限。仅对验收租户2（原Resource租户64c5c35a…）的现有 `tenant:manager` 角色5追加 VNIC create/get/list/delete 和 VM network-interfaces get 共5项。事务校验原租户/角色和既有 NETWORK/VIRTUAL_MACHINE 套餐模块，拒绝覆盖已有禁用/deny；其他角色、模板、套餐保持。没有伪造主体或绕过公开接口鉴权。
- 重载Gov以刷新Casbin策略，最终generation243、Ready1；正式登录后的 VNIC list/create/get/delete 成功，logout200。旧400入口阻塞已解除。一次性DSN Secret已删除；配套日志/备份保存在 `.12` 的任务私有目录。

PR交待评审，不自动合并。不声称未执行项通过：VM独立网卡详情和Guest内部关机未在最终组合重新验证；现场额外ImageUpload能力不属于此main分支，未验证；未跑全量门禁。下面是原始实施/恢复时点的历史记录。

## 输入和实际执行范围

用户授权按已审的方法/字段级方案，直接在最新 Res 代码执行简化：完整采信 KCN 状态，Res 负责自己的管理工作。此次没有 KCN 改造前置条件。

- 仓库：`zhangzhe-ctrl/ani-resource-service`。
- 基线：main `5dc539d92e3150a3593bde3668082753aa4fdebe`，`fix(network): 隔离观测异常，恢复 VM/VNIC 正常准入并收敛终态扫描 (#54)`。
- 工作分支：`work/network-status-authority`。
- 设计输入：[已审实施方案](../../plans/network-management-simplification.md)。当前合同：[网络状态消费规格 v3](../../specs/cr-observation.md)。
- 实际执行位置：本地工作区，承担源码编辑、Git 和轻量静态检查。重任务仍限定在 Fedora；真实集群范围仍为 10–12。

本记录中的源码改动尚未形成可编译发布的完成态：sqlc、protobuf 和 typed config 生成物仍为基线版本，必须完成仓库固定工具链的生成，再编译和运行测试。没有以手工编辑生成物替代生成流程。

## 已落到源码的改动

| 范围 | 方法和字段落点 | 当前行为 |
| --- | --- | --- |
| 共同状态 | `ProviderObservation.Ready/StatusReason`、`Progress.ObservationError`、`Worker.Step/ensure/delete` | 直接映射原对象状态；读错保留成功投影；历史已完成 operation 不再更新。 |
| 事件与排程 | `KCProvider.EnableObservation`、`KCObservation.changed`、`Postgres.NotifyObservedObject/WakeManagedTasks`、六类任务 Finish | informer 只通知精确对象键；直接 GET 当前声明；通知计数、失败退避和原有事务租约负责本服务执行。旧证据索引、readset、审计、tombstone 和证明覆盖逻辑删除。 |
| 租户准入 | `AcceptSubnet/AcceptVNic/ReserveVNic`、Attachment Prepare、Egress/LB 受理和 VM/Instance 网络选择 | 依据 Res 租户、placement、用户意图、引用和版本；移除 Provider Ready、时效、父对象已回 UID、地址已经生成等条件。 |
| Attachment | `AttachmentWorker.Step`、`AttachmentAddressObservation`、`current_vnic_name/current_vnic_uid/current_mac_address/current_ips` | 管理生命周期直接消费当前计算侧快照；地址为独立展示输出。删除历史控制器链、VNicIP/Nic 关系与清场证明。 |
| 计算侧通知 | Instance `NetworkRuntime.NotifiedSnapshotRevision`、VM `NetworkPlan.NotifiedSnapshotRevision`、`UseNetworkNotifier` | 现有计算快照发生变化后通知 Network；跨库通知确认存在原 JSON，失败复用既有工作重试，不引入网络状态判断或新任务表。 |
| Egress / Pool / Switch | `observeEgress`、defaults 查询、管理员 verification、VirtualSwitch 原节点声明 | 消费原生 Ready/phase；管理员材料只存档；移除节点/镜像/物理拓扑的运行验收。 |
| LB | `ObserveLoadBalancer`、组件状态映射、`finishLB`、明确的 capability reader | 只处理自己声明的组件及原生配置确认；删除生成资源库存和 VIP/数据面回收证明。公开旧数据面字段返回 UNKNOWN。 |
| 安全组 | `SecurityGroupWorker`、`observeTargetSecurityGroups`、当前 consumer VNic 查询 | 现有 worker 处理五类目标；applied IDs 只来自原生状态，PATCH 成功只记 configured；结束目标同事务退休任务。 |
| SQL / schema | 查询源、`sqlc.yaml`、迁移 `0032_network_management_simplification.sql` | 移除证明列、关系 JSON、生成资源表；增加六类任务的读错字段及缺失的通知字段；搬运旧 Attachment 当前地址输出。 |
| 装配与合同 | cmd、service、observability、proto/config、RBAC | 取消旧证据依赖注入和权限；保留旧 API 字段号及废弃配置解析，删除运行门槛和证明指标。 |

0032 采用协调升级，旧写入者需要先停止；迁移保留状态、用户意图、原 UID、幂等回执和 operation 历史，重新安排现存非终态任务。该迁移删除旧代码依赖的列和表，不能把旧二进制回切作为回滚方案。本次未执行迁移。

## 验证事实

`pass` 只表示表中列出的轻量检查通过，不代表 Go 编译或数据库执行通过。

| 项目 | 结果 | 证据边界 |
| --- | --- | --- |
| 固定源码身份 | pass | 原提交及 tree 身份已核对；实施所需源文件从该版本取得，逐 blob 校验。没有用重建的合成提交替代 main。 |
| YAML 解析 | pass | `sqlc.yaml`、`configs/config.yaml` 及三个变更 RBAC 文件使用现有 Python YAML 解析器检查。 |
| 文档与退休路径 | pass | 新规格、方案、记录的本地链接目标及执行状态锚点存在；已明确退休的生产文件实际删除，不只是停止引用。 |
| Git 差异格式 | pass | `git diff --check` 无输出；此检查不能替代 gofmt、编译或测试。 |
| 旧接口和字段静态检索 | pass | 按范围检查已移除网络 proof/audit/TTL/关系字段的生产调用；历史迁移、兼容 protobuf 和其它领域的自有生命周期不作为残留网络证明。 |
| SQL 参数与调用方人工/文本核对 | pass | 已检查查询命名参数、返回行嵌入结构、exec/execrows 调用形式以及读错保留分支；不是 sqlc 或 PostgreSQL 的解析结果。 |
| 新旧测试调整 | not_verified | 已修改或新增状态三态、读错保留、UID/404、管理生命周期、通知并发、SG applied、LB 声明及 0031→0032 迁移回归；没有执行测试，删除旧证明合同的用例不算测试通过。 |
| `make generate` / gofmt | not_verified | Fedora 连接不可用；生成物尚未刷新。 |
| `make verify` | not_verified | 依赖正式生成及 Fedora；未执行 Go test/vet/build 或仓库完整门禁。 |
| PostgreSQL 集成与升级 | not_verified | 未创建数据库、运行迁移或执行 PG 用例。 |
| 真实集群与业务验证 | not_verified | 本次未对 10–12 或其它集群执行操作。 |
| 提交、推送、PR、部署 | not_verified | 没有执行。`AGENTS.md` 要求提交前运行 `make verify`；本次不将未生成、未验证源码提交为已完成变更。 |

## 实际阻塞

当前执行环境中，`ssh -o BatchMode=yes -o ConnectTimeout=8 fedora true` 返回 `Could not resolve hostname fedora: Temporary failure in name resolution`。历史指定的 `/home/chabking/.ssh/config` 及当前环境 `/root/.ssh/config` 均不存在，`SSH_AUTH_SOCK` 未配置。本地没有使用替代工具链开展生成、编译、测试或 PostgreSQL 工作。

需恢复当前任务对指定 Fedora 的 SSH 访问，才可继续隔离源码传输、固定工具链生成及必要验证。用户已授权继续实施，不需要再次确认简化方案。

## 尚待完成的执行步骤

1. 将包含本轮未提交修改的精确源码快照传至 Fedora 的独立任务目录，核对文件清单与 SHA-256，沿用既有重任务资源限制。
2. 在 Fedora 运行仓库固定版本的 `make generate`，取回并检查生成差异；解决正式生成/编译发现的问题。
3. 运行 `make verify`，以及本次状态消费、管理事务、通知恢复与迁移所需的 Network/Instance/VM PostgreSQL 回归；如发现失败，保存原始输出并修复后复验。
4. 将实际通过、失败和未验证分别补入本记录；通过提交门禁后再形成正式提交。部署及真实环境验收另记实际执行事实。


<a id="recovery-pr-20261104"></a>

## 2026-11-04：中断任务恢复与草稿交付

用户在知晓上文未生成、未验证状态后，另行要求“把改动都提 PR”。原对话出错后，本次按该授权恢复已有源码，不把恢复交付记为生成或验收通过。上文“尚未提交”等内容保留原执行时点事实，当前状态以本节及执行状态页为准。

- 草稿 PR：[Resource #57](https://github.com/zhangzhe-ctrl/ani-resource-service/pull/57)；分支 `work/network-status-authority`，目标 `main`。
- 原始基线：`5dc539d92e3150a3593bde3668082753aa4fdebe`。
- 原始源码提交：`39a63cd6aa3abccc98a2669be633f3a313cca3d3`，298 个变更路径（14 新增、245 修改、39 删除），完整保存原工作区与补丁中的源码。
- 原补丁 SHA-256：`56789d87fa0a6087e7e7539c65d29b664412efe542934f7df3b155604fae98a9`；逐文件 blob 与原 manifest 一致。
- 远端源码 tree：`720d1b3fba1f6ffd2dc786c67203f18e494e89f0`，与原补丁在固定基线上应用所得 tree 完全一致；`git diff --cached --check` 通过。
- 终端 SSH 无法解析 GitHub/Fedora，本次通过已连接的 GitHub 接口保存提交与分支并创建草稿。没有覆盖原工作区、修改 AGENTS、合并或部署。
- 本次恢复仅做完整性与差异格式检查，没有运行生成、gofmt、编译、测试、PostgreSQL 迁移或集群验证。`make verify` 门禁仍未满足，PR 保持 Draft。

后续从同一分支在 Fedora 使用固定工具链刷新 sqlc/protobuf/typed config 并完成 gofmt，解决生成与编译发现的问题，再执行仓库门禁、相关 PostgreSQL 回归及 0031→0032 升级验证。0032 仍须协调停止旧写入者后升级，不能在迁移后直接回切旧二进制。
