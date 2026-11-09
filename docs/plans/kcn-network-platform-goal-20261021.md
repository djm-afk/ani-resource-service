# KCN 网络平台化执行 Goal 提示词

> 历史工作包：下文环境、连续执行、共享设施和拆建许可仅属于原任务，不自动授权后续工作。当前行为以[领域规格](../specs/kcn-v09-underlay-flow.md)和[全域原则](../specs/res-responsibility-and-simplification.md)为准；被替代的证明条款不作为新任务要求。

日期：2026-10-21。本文是可单独复制到新会话执行的完整提示词；文件交付本身不表示已开始执行、通过验证或完成部署。不给 Goal 设置 token 预算。

> main 发布授权修订（2026-10-22）：用户明确要求“所有代码推送到main分支，走pr,然后bypass合并”。据此允许本任务 Res/Gov 代码经现有 PR 锁定 head 后立即合入 main，不等待 CI 或人工评审，覆盖下文“不默认 merge”限制。仍不直接推 main、不修改 KCN、不混入其他任务工作区改动；源码合并不改变未运行/延期业务验收的结论。

> 原公网替换最新授权（2026-10-22）：用户回复维护范围确认“**你先完全删除，记下网段相关参数，然后再通过res创建一个一样的出来**”。据此记录当前原生公网五对象完整参数/依赖，正常删除限定原链并让旧 Device 正式退役，再以真实 Gov/Res 产品用例创建同参数的虚拟交换机/二层网络/公网池/默认 VPC 出口，最终核实 `.192 → kcn-system/kcn-cluster`。ens35/VLAN0、172.16.102.0/24、上游.1、OVN路由.2、可用.192–.207保持；ens34/ens36、默认VPC/Subnet身份及他人资源保护。正式网络不以原生kubectl创建代替Res；允许明确区分的临时出口见证取得新池实际验收证据，见证清理后再绑定正式产品出口。取消watchdog的决定仍有效，不扩大租户/LB验收；必要的本次重建/出口可用性验证不再受先前网关/池“后面再测”限制。

> 旧 Device 后续授权（2026-10-22）：用户在获知 ens35 原公网仍在用后明确回复“**那你删吧，管理网络不会断的**”，因此允许对原 Device 发起正式退役，不再以“仅无用时才清理”限制受理。该授权用于指定旧登记，不能把它扩展为强删原生公网 VLAN/EIP/Snat、伪造退役完成或修改 KCN；受理后的关闭/fence、真实最终状态及剩余引用分别记录。产品二层记录与真实 KCN VlanNetwork 的创建时点必须区分：产品原子受理在前，逐节点组标签完成在后，再创建 KCN 对象；标签写入仍不等于 KCN 已识别组。

> 最新范围修订（2026-10-22）：用户已明确恢复“10～12集群你可以使用了”，并要求合入/部署最新版 Gov 后继续任务，取代下文历史只读及暂停限制；禁止修改 KCN、KCN 问题记录报告的要求保留。用户随后明确“不要这个，不算阻塞”：不再开发、部署或以 `restore-egress` 自动执行器、独立 watchdog 的缺失阻塞任务，覆盖 P4 第2/5项及 P6 对独立看护接手的要求；原出口如被实际改动仍需按快照恢复，不把撤销自动工具理解为免除恢复义务。“网关和地址池验收开放”按用户要求延后，记为 `deferred_by_user`，不列为本轮必须完成项。旧 Device 先查使用/引用，无用时已获直接清理授权；不得把仍承载原公网的接口直接视为无用。租户 VPC 自动基础连接仅核查当前状态，不据问询自行启用。二层网络选择虚拟交换机使用已实现的新协议；选择字段不能冒充 KCN 组识别/修复后重驱证明。执行结果仍只见 `docs/execution/status.md`。

> 执行授权修订（2026-10-21）：用户随后明确要求“**不允许修改 kcn 代码**”。该指令覆盖下文所有 KCN 源码窄改授权；不得据原 P1 再修改、发布或部署本任务 KCN 源码候选。Res/Gov 的既有授权继续有效。固定版本 KCN 无法满足的恢复验收必须如实保留阻塞，不得以 Res 绕过、标签反复变更或共享组件重启替代。

> 集群使用确认修订（2026-10-21）：用户随后明确要求“**需要使用集群就找我确认**”。开展任何一组明确用途的 10–12 集群操作前，必须先说明用途、范围和影响并取得用户确认，包含只读 Kubernetes/API 核查、节点 SSH、部署、流量验收和恢复；确认仅覆盖约定范围。此要求覆盖下文原有集群预授权。Fedora 上的软件生成、编译、正式 Git/Go 模块传输和完全隔离的 loopback 测试可以继续，不能借“预检/dry-run”绕过真实集群确认。

> 本轮确认与 KCN 处理修订（2026-10-22）：用户已确认“**允许本次只读 API 与三节点 SSH 核查**”，范围是身份、镜像/CRD/NodeFacts/原公网依赖及 ens34 网卡/OVS socket 权限，不含部署、配置修改、内部镜像仓库读写、流量或拆公网。用户另明确“**碰到 kcn 问题，记录并报告即可**”；因此不再要求用户提供源码或恢复协议，也不修改 KCN。未证实的恢复能力作为限制记录，不视为公网维护门禁通过。当前证据仍只在 `docs/execution/status.md` 索引。

> 集群占用修订（2026-10-22）：用户随后明确“**集群正在使用，你不能部署服务，必须使用集群就暂停**”。上述已完成的只读核查不授权继续连接集群；不得部署服务或执行其他集群操作。仅收尾无需集群的 Fedora 隔离源码、验证与本任务临时资源清理；下一步必须使用集群时，按用户要求将 Goal 标记 `paused`，保留完整未完成范围，等待用户恢复。

---

请创建并执行以下完整 Goal，持续推进到业务验收、清理和原公网出口恢复完成，不设置 token 预算。先读取各目标仓库适用的 AGENTS.md，再读本文引用的正式方案；不要停在研究、代码提交、单测通过或 Pod Ready。每轮简述主链已到哪一步、下一处真实阻塞；授权范围内不要重复索要“继续”确认。仅当缺少真实身份、必要权限、受支持恢复能力或发现超出本授权的破坏性影响时，明确证据和最小缺口，继续不依赖它的工作。

## 目标、范围与既有授权

实现并交付 [KCN v0.9 虚拟交换机与二层网络流程](../specs/kcn-v09-underlay-flow.md) 的完整业务主链；把 [2026-10-21 关联源码审查](../execution/records/kcn-network-flow-review-20261021.md) 的必要修订一同落实：

- 管理员通过真实 Gov HTTP 入口先创建只需名称的虚拟交换机，无区域/AZ/简介；再一次提交集群、网卡、VLAN 和虚拟交换机来创建二层网络。后台原子受理交换机首次绑定、完整成员快照、设备登记/占用、operation 和持久 reconcile，由 KCN 真实接管；用户不额外 Adopt 网卡或打标签。
- 公网网关/池经真实控制面与流量验收后开放分配；基础内网合格池、VPC CIDR 候选及新 VPC 基础连接 rollout 明确配置。租户经 Gov 真实权限和持久用例创建 VPC/Subnet、接入工作负载、申请公网 EIP、绑定 SNAT 并出网。
- 回归 LB“先保存和预留 VIP/EIP、waiting_for_backends → 配置合法后端后激活”的主链，真实验证私网、公网/双入口及 EIP 独占，不把等待配置误报为已可转发。
- 实现方案中的独立平台默认 VPC 公网出口用例，不能把 `kcn-system/kcn-cluster` 伪装成租户 VPC 或复用租户身份绕过授权。测试完成后清理本 run 产品资源并恢复原生出口；旧原生资源不因此自动纳管。

允许修改 Res/Gov 的必要 API、业务、持久化、适配、权限、配置、部署和文档；KCN 仅允许为组识别/组暂缺及稳定配置错误重入补必要窄改，并在证明需要时受控升级 10–12 的对应组件；installer 仅调整目标物料锁定、CRD/RBAC/配置交付，不能把 release 全套搬到 10–12、重装集群或顺带升级无关组件。Model/Inference 等其他服务只作真实回归消费者，不改其业务代码；ModelScope pageSize=100 的上游最大 50 限制单独报告，不擅自改 Model 来扩大本 Goal。

本 Goal 授权任务分支提交、通过 Git SSH 推送、发布正式可拉取 Res API 版本、创建 draft PR；授权核实身份后的必要 Res/Gov/KCN 目标部署和本节限定的公网验收维护。没有授权直接推 main、默认 merge PR、广泛部署到其他集群或修改 20–22。所有 PR 创建后附加到当前任务；提交/PR/发布/部署/业务验收分别报告。

## 固定输入与执行纪律

仓库根目录均在 `/home/chabking/workspace/`：`ani-resource-service`、`ani-governance`、`kc-networking`、`ani-installer`。正式方案、审查是权威输入，不复制第二套状态机；执行进度只更新 Res 的 `docs/execution/status.md`，本 run 的必要证据集中到一个 `docs/execution/records/KCN-NETWORK-PLATFORM-<run-id>/`。

新会话直接读取以下本地输入；不依赖上一个会话的隐藏上下文：

- 本提示词：`/home/chabking/workspace/ani-resource-service/docs/plans/kcn-network-platform-goal-20261021.md`。
- 正式方案：`/home/chabking/workspace/ani-resource-service/docs/specs/kcn-v09-underlay-flow.md`。
- 固定源码审查：`/home/chabking/workspace/ani-resource-service/docs/execution/records/kcn-network-flow-review-20261021.md`。
- 原公网恢复参考：`/home/chabking/workspace/ani-model-service/docs/deploy/kcn-cluster-public-eip-20261020/` 内 `README.md`、`receipt.json`、`vlan.json`、`gateway.json`、`pool.json`、`eip.json`、`snat.json`；必须用执行前真实快照校正。

批准输入的 SHA256（先核实，再转入任务工作树；代码 ref 刷新不改变已批准产品边界）：

| 输入 | SHA256 |
|---|---|
| `docs/specs/kcn-v09-underlay-flow.md` | `48e017691ada3904e63bc483afa006da16d124743b307737cefd73003759bc01` |
| `docs/execution/records/kcn-network-flow-review-20261021.md` | `b646e3a2b6bf049670fd1fedd23ec6536ef76a61407a0bed4542de2ef89c66de` |

输入缺失或 hash 不符先定位实际差异；不得用旧版本推断用户最新批准内容。执行中的必要方案更新保留初始 hash 与变更理由，不要求后续文件永远维持该 hash。

| 来源 | 2026-10-21 已审查 ref，执行 P0 必须通过 SSH 刷新并重新固定 |
|---|---|
| Res main | `6bff99f1747a495fe4b278844c20ee72b1ce2ffc`；旧草案基线 `d32c4f438b43b8cb7534486c73e116e4d26c890a` |
| Gov main | `323eb749b8a3d607b26687797b372919052895c7` |
| 先前选用的 Gov 分支 | `codex/gov-model-inference-latest-20261020`，`05bdb9d890b7e89645dd67ed1c697af2ad7c3ed0`；历史选择，不代表当前运行镜像 |
| KCN | 缓存 `origin/main=2b9467c66dc9024fbf321a4b3a98a9e5f3fef377`；**GitLab 远端最新未确认**。相对旧部署基线 `ca06a4e06c902b9f9cdc6a6a6a1879792e390fd3` 仅文档变化，不能称恢复问题已修复 |
| installer main / release | `78d8418307b6156954feb10aee8d6972d2ff1bbd` / `8df08cd5c13f0ce5156a2df3f1ce0ed8cf56f166`；分列物料与真实运行版本 |
| 历史运行回执 | `/home/chabking/workspace/ani-model-service/docs/deploy/kcn-cluster-public-eip-20261020/README.md` 与 `receipt.json`；仅作恢复参考，不能代替执行时快照 |

只用 Git SSH 获取、发布源码；GitHub 可使用已配置的 SSH 443 通道，KCN 使用既有 GitLab SSH 身份。不得复制凭据、改 remote 来掩盖访问问题，或将不可达时的缓存 SHA 冒充最新。以 P0 固定的新 main 为实现基线，单列实际部署及先前选择分支的差异，确保目标 Gov 部署不无关回退现有功能。

本地仅源码编辑、Git、静态阅读和轻量文档/差异检查。所有依赖下载、生成、编译、测试、PostgreSQL、API 驱动、镜像、部署脚本和集群操作都从 `ssh fedora` 的任务独占目录执行；**Fedora 不可用时没有本地重任务回退**。沿用可核实的远程锁、工具版本和资源限制，不无界并行重任务。KCN 专项测试在 Fedora 的 envtest 等隔离环境进行；不得把仓库通用破坏性 e2e 跑在 10–12。只运行相关行为测试和仓库必需门禁；无新变更、失败或风险，不反复扩大验证。

先查看 dirty 与已附加 worktree；复用本任务合适的隔离 worktree，否则从固定 SHA 建 `codex/` 任务分支/worktree。保留所有无关修改，不 reset/clean/stash/强推或整树覆盖。远程 source manifest/hash 排除凭据、kubeconfig、私有配置及无关产物；生成物回传临时目录，核对期间漂移后选择性合入。禁止以 mock 绕过权限、持久化、worker 或待实现逻辑制造成功；外部边界替身验证和真实验收分开。

这些输入文档可能尚未提交，新的 main worktree 不保证含有它们。P0 对原输入校验 hash 后，仅将上述方案、审查和本提示词原样转入任务 Res worktree 的同路径，再作必要索引/后续实现更新；不能整体复制旧工作树的 dirty 文件或用新 main 中旧稿替代用户已批准的方案。转运保留来源/hash；遇到同路径差异先比较，不能盲覆盖。登录使用合法平台账号及受保护的现有凭据来源，旧 session 不作为有效输入；如缺凭据仅请求必要登录信息，不把密码或令牌写入提示词、源码或命令输出。

## P0：冻结来源、集群身份与恢复边界

1. SSH 刷新上表各仓库 ref，记录 URL、branch、commit/tree SHA、适用 AGENTS、实际差异；不能沿用工作树 HEAD 猜远端状态。KCN 若仍不可达，保留明确阻塞，不以其他缓存/installer 锁定值替代远端核实；继续可独立的 Res/Gov 实现，KCN 候选发布前必须解决来源问题。
2. Fedora 只读核对唯一业务目标：context `kubernetes-admin@ani-lab`，节点 `ani-01/02/03`，InternalIP `172.16.101.10/11/12`，`kube-system` UID **`5277649d-e28d-4a0a-ae76-8354365c63bc`**。默认 VPC `kcn-system/kcn-cluster` 历史预期 UID **`d0235860-f51a-4e63-8f4f-1ca1b34bc04f`**。这些是历史 guard，不是已复验事实；任何不符先解释，不向不明集群写入。禁止新增/修改 20–22 或其他业务集群。
3. 冻结真实 Res/Gov/KCN imageID/digest、CRD/API、配置/迁移版本、证书与直接调用身份、NodeFacts 新鲜度、Node patch RBAC、权限/API 目录、默认池和 rollout 当前状态；记录 source → module/API → image → deployment 矩阵。installer main/release 和旧回执分别列出，不因更新锁文件就宣称集群升级。
4. 读取原公网现状和全部消费者依赖图，检查 Model/CoreDNS 基线。旧 Device `device_94d6ebc1e5bb466d9effad72e9fb055d`、Gateway `egw_d6d865fccf9946bb90a8a171975be566` 及其 pending/adoption/ConfigMap/worker 记录保留原身份。仅删原生公网 CR 不会释放旧 Device 的 cluster/device 唯一占用；必须核对其真实 UID、marker、活动引用、未知结果及 worker。若阻塞本 Goal 的受控测试，按 P2 实现受限的旧登记关闭/退役管理路径；不得清库、直接改 available、换 ID 或假收养。原生删除授权不自动赋予其他资源迁移所有权。

P0 输出一份可复现来源和运行输入清单、保护对象清单、所需运行权限与具体阻塞。尚未接通的主链写清；源码/文档审查不能替代后续实现。

## P1：先解决 KCN 必要恢复协议，再固定 Provider profile

对 P0 固定 KCN 源码验证两项问题是否仍在：组暂缺导致未初始化 VLAN 无可靠重入；网卡缺失/已有 IP 的稳定错误修复后无受支持重驱。若已修复，引用固定源码和行为测试删去旧阻塞；否则做最小控制器/daemon 修复，保留安全检查和有界重试，避免忙循环。证明“标签先后/缓存乱序、未初始化 VLAN、物理条件修复、进程重启”均能安全恢复，不用 sleep、删重建 VLAN、反复改标签或重启共享 daemon 当产品协议。

固定本轮按对象的观察条件：VLAN 的 Valid/Initialized、组成员、notReadyNodes 和真实 wiring；Gateway 的实际 Ready；Subnet 的 Ready/observedGeneration/underlay/chassis 与真实依赖；Snat 的 Valid/phase 和 EIP boundResource 类型、名称、ObservedGeneration 对应 Snat generation、VPC、启用状态。不能等待旧版本未发布的 VLAN/Snat Ready/Healthy，不能把 underlay.ready 摘要当独立流量验收。

必要 KCN 候选先在 Fedora 隔离专项测试并产生可追溯镜像/CRD/RBAC。将其精确版本交付到 installer 的目标物料；10–12 的受控组件升级须有当前 CRD 兼容性、配置/镜像回退和正常收敛证据，保护既有流量，不套用面向 20–22 的整套安装流程。

## P2：Res 一次打通“交换机 → 二层网络 → 真正接管”

按正式方案实现最小纵向切片，不能只先交付虚拟交换机 CRUD：

- 平台交换机创建用户业务字段仅 name，使用合法、唯一、不可变的组名；产品 ID/幂等/operation/历史真实持久化。未绑定时不塞假 cluster_id、Provider UID 或旧 PlatformResource kind；首次二层网络才绑定真实可达目标集群。
- 首次冻结全部 `allocated=true` 节点的具体 Node UID、priority=100、网卡 MAC 和完整 fingerprint，核验 Ready/chassis/daemon、网卡、物理二层条件；不按“有该网卡”静默过滤。后续复用同一组，第一版无跨集群复用和动态成员编辑；漂移退化、阻止新准入，不擅自改现有连接。
- 一个受理事务固定交换机绑定、内部设备活动占用、VLAN、operation、reconcile、历史和幂等 v2。保留 Proto 旧字段号/v1 hash/旧精确重放、pending marker、稳定 Provider 名、UID/RV、lease/epoch、BindingID；旧结果未知不能因新请求放行。增量 migration/sqlc，不能改已执行 migration 字节。
- worker 逐节点条件合并本交换机拥有的组标签，持久原值/Node UID/RV/已应用及未知结果；不因同键同值收养外部组，不代写设备成功注解。组标签归交换机生命周期，不随单 VLAN 失败删除。SQL、worker 依赖观察、Provider POST 前三处 gate 同步解除旧“先物理接管”前提；Pool 门禁仍等全部节点真实 wiring。
- 活动 `(cluster, device_name)` 跨交换机唯一；多个 VLAN 可正确共享内部登记。最后 VLAN/未知结果/worker 引用都核清、Provider 最后引用清理后，受锁且 fenced 地退役内部登记、保留 ID/历史/墓碑并释放活动槽位。交换机 DELETE 阻止并发新引用，异步清自己的标签，最终清理后释放名称；不能形成设备与交换机相互等待的永久锁。
- 若旧 v1 Device 阻塞 ens35 验收，先实现并在 Fedora 验证平台授权的受限“旧登记关闭/退役”管理路径：锁定原 ID/版本，核清无活动依赖和 unknown，按真实 owner/UID 对自己的 ConfigMap adoption marker 做 CAS 清理，关闭旧 reconcile、推进版本/租约 fence，保留历史后释放活动槽位。不能 raw SQL 删行、盲清标记或借新交换机收养旧 native。若发现他人引用或未知外部副作用、无法证明处置范围，先恢复已触碰的原出口，再报告具体阻塞；不能跳过真实测试宣告完整。

Fedora 跑真实 PostgreSQL/worker 和相关行为测试：并发首次绑定、跨租户/平台边界、旧/新幂等、部分 Node patch、未知创建、身份漂移、稳定错误重入、迟到 worker、共享引用删除、无引用交换机删除。节点写入 mock 只可位于 K8s 外部边界，不能替代上述业务持久用例。

## P3：正式发布 Res API，接通 Gov 与完整业务依赖

1. Res 合同定稿并按 pinned workflow 在 Fedora 生成、验证；通过 SSH 正式发布可供拉取的 module/API commit/tag/version，记录 module sum 和 SHA，再更新 Gov 固定依赖。禁止 go.work、本地 replace、私有临时副本或前端串多次请求绕过 Provider API 发布和一次受理。
2. Gov 新增 VirtualSwitch DTO/validator，支持 unbound 状态，明确 data 包装、ID/错误/operation 映射；新交换机列表采用 `page/pageSize → items/total` 的直接跳页合同，HTTP total 按公共合同输出十进制字符串。旧 limit/cursor 接口是否迁移按正式方案兼容窗口，不顺带破坏所有旧列表；完整网卡库存不能用伪分页隐藏缺员。
3. 新 RPC 经既有 mTLS trusted workload allowlist、平台 scope/真实 actor/request ID；核对服务端注册、客户端、HTTP method/path 白名单及独立 create/read/list/delete 等权限与 API 关联。只增量更新必要权限，不宽放所有 admin，不伪造 JWT，不借租户 UUID 访问平台资源。用真实平台账号登录、最小 grant、刷新 Gov 策略并做拒绝路径；凭据/令牌只留受保护临时文件，不写本文、Git、命令日志或公开证据。
4. 接通已存在的合格 Intranet Pool、VPC CIDR 候选读取/首次精确匹配及与平台池双向不重叠；候选配置发布按实际合同重启 Resource，旧 key 精确重放保持不变。显式验证 `new_vpcs_enabled` rollout、缺候选/池/过期事实 fail closed；不擅自迁移历史 VPC。
5. LB 回归既有“无后端先保存、父引用及 EIP/VIP 独占仍保留、无运行投影时停放 reconcile”，合法后端激活时才核验基础连接、runtime capability、Subnet/EIP 和后端归属。完成平台默认 VPC 的独立授权、持久平台 EIP/Snat、精确默认 VPC UID 限定、观察与删除用例，不能把通用管理员任意指定 VPC 当作边界。

分别运行 Res/Gov 相关行为/合同/授权测试、必要生成和 `make verify` 等仓库门禁；失败定位到真实断点，不靠重复全量测试代替修复。镜像、远程可拉取 module、部署配置与证书/RBAC 全部准备好后，才进入维护验收。

## P4：先准备可执行恢复，再打开唯一公网维护窗口

用户已经授权**为了本 Goal 的公网创建验收暂时拆掉现有公网网段，使用原参数测试，测试后必须恢复**。这不是清空网络或自动纳管的授权。仅限：

| 项 | 许可参数/历史恢复对象 |
|---|---|
| 物理口/组 | 三节点 `ens35`、VLAN 0；原组 `ani-public`、priority=100。候选交换机使用本 run 专属合法组名，清理后恢复原组，不同名自动收养 |
| 公网 | `172.16.102.0/24`；物理上游 `172.16.102.1`；OVN 网关 `172.16.102.2` |
| EIP 池 | `172.16.102.192–172.16.102.207`；最终恢复 `172.16.102.192` 到 `kcn-system/kcn-cluster` |
| 原生对象 | VLAN `cluster-public-ens35-20261020`；Gateway `cluster-public-egress-20261020`；Subnet `kcn-system/cluster-public-102-20261020`；EIP/Snat `kcn-system/cluster-public-egress-20261020` |
| 额外必须恢复 | 执行前真实 natOutgoing、原组标签和全部先前消费者/配置；历史 natOutgoing=true、egress ani-01 只能作核查参考 |

**拆出口前的全部门禁：**

1. 当前完整快照及依赖图：精确 UID/RV/spec/owner/labels/annotations、默认 VPC/Subnet、natOutgoing、全部同 CIDR/接口/组/池的 EIP/Snat/LB/路由/消费者、平台默认池/分配开关/rollout、相关 operation/reconcile/lease、必要数据库恢复点、实际镜像/CRD。公开回执脱敏；恢复必需私有项留远端任务目录 0700/0600，不入 Git、不输出密码或令牌。共享数据库快照是恢复依据，不授权覆盖其他用户后续写入；采用可兼容增量迁移和经验证的前向修复/目标镜像回退，禁止为回退全库覆盖或 truncate 共享表。
2. 在 Fedora 保存并审查可执行的 `restore-egress` 与状态 journal，校验 hash，完成 dry-run/参数与 UID guards、恢复对象合法性检查、产品清理和新旧 UID 映射演练。恢复脚本不回填旧 status/UID/RV；删除重建必然得到新 UID，要在恢复后重新建立引用和证明。备份成功不等于恢复能力已通过。
3. 验收 `ens34` 管理 SSH、cluster API 的独立恢复通道；全部依赖/工具/镜像/候选配置、恢复物料及可用于恢复的合法登录/身份都提前准备好。不能拆完出口才下载依赖、拉镜像或发现登录已过期。
4. 建立单操作串行维护窗口和排他 fence；只用已支持的对象级维护/删除机制阻止**本次待拆对象或本 run**的后台复活，持久记录 fence/恢复点。不得停整个共享 Res/KCN controller，不得给无 Res owner 的原生对象伪造数据库归属。若现有恢复/维护协议不足，先实现并验证，不能带缺口拆网。
5. 准备不依赖当前会话存活的恢复看护/超时退出方案；中断或失败也要恢复。无人值守恢复不能安全驱动产品清理、核清 pending、释放同 CIDR 占用时，不得进入破坏阶段。

通过上述门禁后，从 Fedora 单步核对身份并拆除限定旧原生链：**Snat → 确认解绑/正常终结 → EIP → Public Subnet → 无引用 VLAN/Gateway → 必要的原组标签**。每步用当前 UID 和依赖条件、等待正常 finalizer；后两者无依赖时按实际拓扑顺序。KCN 物理清理及旧登记受限退役门禁确认后才能转入新产品验收，不保留原池与候选池重叠并存。候选产品交换机用 run 专属合法名称，不创建同名 ani-public 来自动取得旧组所有权。

禁止删除默认 VPC/default Subnet，禁止修改 `ens34/ens36`、刷 iptables、直改 OVN、强去 finalizer、手工改库跳状态机或伪造 owner。发现未登记消费者、身份变化、恢复 guard 不符，停止拆除并执行恢复；新影响范围确实超授权才请求具体补充。

## P5：在 10–12 通过真实 HTTP 和工作负载验收

按 P0 冻结版本部署必要目标 Res/Gov/KCN/最小配置和 RBAC，记录实际镜像 ID/rollout；不宣称 installer 整体或其他集群已部署。通过真实 Gov 平台管理员 HTTP 完成“两步”创建，验证后端登记、全部三节点组标签和 wiring、持久 operation、网关/池控制面与真实出口证据，再开放验收所需分配；不能直接 kubectl 创建候选 VLAN/Pool 替代管理员产品主链。

使用真实租户入口、测试所属 VPC/Subnet/实例、EIP/Snat/LB 完成以下矩阵；平台基础出口预验收可用明确记录的独立运维对象，但不能冒充产品租户用例。产品默认 VPC 出口用例与租户用例均须有真实授权、持久记录、独占绑定及清理，不给系统 VPC 塞假租户行。运行现有 Model 搜索只用 pageSize=50，校验 TLS，不使用 `--insecure` 将证书问题变成成功。

| 验收项 | 必须留下的真实证据 |
|---|---|
| HTTP 两步与 unbound | 仅 name 创建成功、未绑定详情/直接跳页 items/total；一次创建 L2 后后台完整成员登记/标签/VLAN/wiring；无额外 Adopt |
| 授权与一次受理 | 平台管理员经 mTLS 到 Res；无 grant、租户冒用平台、跨租户/伪 VPC 被拒绝；幂等重放与冲突、未知创建重启恢复 |
| 接管与删除 | 组识别乱序及稳定错误修复重入；部分节点/错误 mapping/缺组不能 available；共享 VLAN/其他组保护，迟到 worker 不复活、无引用交换机可最终删除 |
| VPC 候选/rollout | 候选过滤与首次精确匹配、配置为空/过期拒绝；合格基础池与新 VPC rollout 生效；基础资源真实应用，无历史 VPC 自动迁移假结论 |
| 租户出站 | 合法 VPC/Subnet/真实消费者、独立公网 EIP 与 SNAT exclusive claim、地址/代次/启用状态一致；DNS、校验证书 HTTPS、物理出口源地址和回程 |
| LB 保存/激活 | 空后端保存 waiting_for_backends、预留 EIP/VIP；与 SNAT 抢同 EIP 被拒绝；合法后端激活后私网及公网/双入口真实流量，非法后端拒绝 |
| 平台默认出口 | 平台独立持久用例绑定精确默认 VPC，无租户身份绕过；真实系统工作负载出网及正常退役 |
| 恢复验收 | 下节全部完成，原配置/消费者恢复、候选无活跃引用、无重叠池或旧 worker 复活；缺这一行不能完成 Goal |

源地址证明应在 ens35/物理出口观察 SNAT 源 `172.16.102.192`；若上游还有 NAT，外网回显可以不同，记录转换边界与实际回程，不能要求公网回显该私有地址。失败用精确 request ID、RPC/Provider/节点证据定位，不以 HTTP 200、Pod Ready 或空列表代替链路成功。

## P6：无论测试成功、失败或中断，先恢复原出口，再交付

1. 立即停止本 run 新建/激活，持久维护阶段和恢复原因；恢复属于工作本身，不以任务中断、测试失败或提交完成省略。需要暂停或退出当前会话时，先确认独立恢复看护接手并报告其实际结果；恢复未完成不得称 Goal complete。
2. 先经产品生命周期逆序释放本 run 的工作负载占用、LB/Snat/EIP、Pool、VLAN、Gateway、虚拟交换机及内部登记。核清所有同 CIDR 引用、pending/未知结果和 worker fence，确认真实对象最终消失、组标签和物理引用可安全归还；历史/墓碑保留，不删库去伪造已清理。旧 native owner 与候选产品 owner 不能互换。
3. **候选池/消费者全部清理且地址、接口、组占用释放后**，按执行前快照正常重建原生组/VLAN/Gateway/Public Subnet/EIP/Snat，恢复原名称、参数、`172.16.102.192 → kcn-system/kcn-cluster`、组 priority=100、natOutgoing 和先前消费者/临时验收开关/配置。不得让两套相同网段池同时存在。正常创建得到新 UID，记录 old→new 映射，不伪注入旧 UID/RV/status，也不把重建对象自动收养为产品。受控关闭/退役的旧失败 Device 保留修复后历史/fence，不为还原网络参数重启其失败 worker；正式交付的代码/迁移/权限与临时验收配置分别记录。
4. 确认恢复后的逐节点 annotation/OVS/mapping、真实 subnet/gateway/EIP/Snat 状态与绑定代次，默认 VPC/Subnet UID 未变、ens34/ens36 未改；恢复与原状态不同的实际项逐项解释。撤销本 run 的维护 fence 后再次验证无候选对象复活。
5. **最终必过恢复流量门禁**：两个 CoreDNS 副本各自查询外部源成功；Model 实际 Pod 网络命名空间 DNS 成功、使用其 CA 校验 HTTPS；真实登录的模型搜索 `Qwen/Qwen3-0.6B&page=1&pageSize=50` 成功；物理出口 SNAT 源 .192 与回程明确；此前消费者逐一恢复。不能仅用节点宿主机 curl 代表 Pod，不能把 pageSize=100 上游限制误算为本网络恢复失败/顺便修 Model。
6. 清本 run 临时 Pod/验收对象/登录 session/凭据、任务独占的远端测试数据库和进程；不删除共享业务库或他人测试库，保留必要工具共享缓存及可恢复证据，任务 worktree 按需要保留/归档，不清理其他任务。若失败，先完成恢复和精确 FAIL/NOT_RUN，再报告尚未解决的业务阻塞，不能把失败后的“计划恢复”写成已恢复。
7. 在任务分支选择性提交/SSH 推送并创建每仓 draft PR，描述最终问题、行为、真实测试、部署和恢复边界；附加 PR。正式 Res API module、source SHA、生成工具/源码 manifest、迁移、权限增量、镜像 digest、部署 revision、业务请求及新旧 UID 映射可追溯。按仓库门禁完成必要检查，不默认 merge。

## 完成与退出条件

只有实现主链、Res 正式 API 与 Gov 固定依赖、必要目标部署、矩阵业务验收、全部测试资源正常清理和**原公网出口真实恢复**都完成，才能标记 Goal complete。提交数、测试数、方案完成、替身闭环、镜像构建或单个 HTTP 200 都不足以替代。

最终给一份简洁交付：实际业务跑到哪一步；各仓分支/提交/API 版本/PR；部署镜像 digest 与集群身份；矩阵逐项 `PASS / FAIL / NOT_RUN`；恢复的旧/新 UID 和消费者证据；仍存阻塞、限制及回退入口。从未执行的检查不能补写 PASS。若受必要外部条件阻塞，保留可续跑 journal/输入/下一步，优先完成已发生副作用的恢复，不把 KCN 远端未核实或真实验收未做隐藏掉。
