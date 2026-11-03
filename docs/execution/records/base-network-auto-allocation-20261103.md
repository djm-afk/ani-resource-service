# 基础网络自动分配与 LB 产品主链（2026-11-03）

当前状态只见[执行状态](../status.md#base-network-auto-allocation-20261103)。业务合同见[基础连接规格](../../specs/vpc-connectivity-lb.md)与[平台运行前提](../../../deployments/egress/intranet.md)。

## 实施与固定输入

Resource 延续 PR #46 的 `cba0bcb`，本批业务源码提交 `1ddb5c6`；Console 延续 PR #10 的 `1e032e4`，新增只读摘要提交 `c0765b6`；BOSS 从实际最新 main `9f917a4` 创建独立分支，最终提交 `02dc75b`；Governance 从 main `316ee2e` 创建配套分支，提交 `566c3b2`。原有 dirty 主工作区及用户 43927 预览保留。

Resource 新增 `GetIntranetPoolDefaults` 和 `SetIntranetPoolAutoAllocationEnabled`；平台身份来自既有 mTLS 委托，池版本与永久幂等复用既有接受记录。自动开启在一个事务设置默认池、池开放及新 VPC rollout；关闭只允许当前默认源并保留池开放和现有资源。默认源切换在开启时校验并开放目标池，不移动已受理子资源。Intranet 池新增只读 `new_vpcs_enabled`，只有当前启用的默认源为 true。

迁移 0027 删除人工 Intranet verification 分配约束，并在既有平台操作类型 CHECK 中加入新命令；不改已应用迁移。准入与运行 adapter 均改为依赖真实 Provider 当前事实，不再以人工证据作为 Intranet EIP/SNAT 前置。创建表单默认身份及路由来自只读 Provider 查询，实际创建仍核对冻结的 UID、控制器默认 router、路由覆盖与 Ready。

## Fedora 检查

独占目录 `/home/chabking/tasks/base-network-20261103/resource`。初始输入 8542 文件，归档 SHA-256 `fb173cf7d799aae5db656efe0ef70bcc855c4f2ab08999c201b84ea1bb328a24`；后续精确差异保留于本 Git 提交及远端源码。Go 1.26.10，Buf 1.60.0，sqlc 1.31.1；GOMAXPROCS=2、GOFLAGS=-p=2。生成、格式化、数据库、自动化测试、镜像构建、API 驱动与集群操作均在 Fedora 执行；本机仅编码/Git/转运及正式页面轻量浏览器交互。

- `make config`：PASS；生成后网络 API 与正式输入同步。
- `go test -count=1 ./internal/biz/network ./internal/service/network ./internal/server`：PASS，包含所有平台方法的受信身份检查。
- `scripts/integration ./internal/data/network -run '^Test(Intranet|BaseConnectivity|BaseConcurrentWorkers)' -v -timeout=15m`：PASS，真实独占 PostgreSQL，运行 116.650 秒；覆盖原子默认/分配/开关、无人工 verification、永久重放/冲突/旧版本、切换旧源误关闭拒绝、真实默认身份/路由拒绝，以及新建 VPC→基础 EIP/SNAT→并发/未知结果恢复/删除。
- 最终 `make verify`：PASS，包括锁定生成一致性、分层故障检查、依赖 tidy diff、全 Go 单元检查、vet、build、模块校验与 diff 检查。

原始失败未覆盖：首次新增开关 PG 失败定位为平台操作类型 CHECK 未登记，已在 0027 修复；真实 VPC fixture 去除人工证据后发现 adapter 残余 verification gate，已移除并由同一真实生命周期复验通过。最初扩大至旧 backfill 测试时，它们在旧 VPC fixture 创建阶段因 `tenant namespace reader unavailable` 失败；该历史补齐路径不在本次实施范围，不修改断言或记为通过。具体日志为远端 `evidence/{postgres-tests-first-failed,postgres-tests-with-legacy-fixture-failures,postgres-tests-adapter-gate-failed,postgres-tests,make-verify}.log`。

配套仓库均在各自独占 Fedora 源码目录执行生成/必要检查和原生构建：Governance `make api`、新增平台方法/委托/错误与 VPC 摘要的定向行为测试、data/auth vet PASS，SDK 固定为 `v0.0.0-20261102171245-1ddb5c61eb17`；整个 Service vet 仍有未修改的 `vm_list_filter_test.go` mutex copy 及 tenant-space 测试 unkeyed literal 失败，未宣称全仓检查通过。BOSS/Console 的 lint、format、类型及 production build PASS；BOSS 真实 Hook/TanStack/API 的12场景受控 HTTP 边界验证、Console 摘要组件/既有 LB Hook 验证及最终依赖图检查 PASS。未为了末尾仅更新文档而重复已经通过的生成/构建/测试。

## R1 首次发布与业务验收

目标为 `kubernetes-admin@ani-lab`，API `https://172.16.101.10:6443`，kube-system UID `5277649d-e28d-4a0a-ae76-8354365c63bc`。依次完成 Resource→Governance→BOSS/Console 发布；四套 Deployment 均核对 UID、实际 imageID、observedGeneration 和 Ready，正式前端 HTTP/bundle 与 Pod 文件哈希一致。镜像仓库前缀均为 `172.16.101.10:30003/icn-01a15337-release/`：

| 服务 | 业务源码 | 实际镜像 digest |
| --- | --- | --- |
| Resource | `1ddb5c6` | `ani-resource-service@sha256:ab3d779d6abcf0259666f460a6ea26f93a1f9a500a81ae80db79377864e2e693` |
| Governance | `566c3b2` | `ani-governance@sha256:12e40190f94008fb6cad5119489e27bad4613a6376ba87d01d93cf8ebef49009` |
| BOSS | `02dc75b` | `eight-boss@sha256:ae1ae8a666760258ba3c2569914738e04908fdc0a606b250a62032f3270273fa` |
| Console | `c0765b6` | `eight-console@sha256:83acd3b15db32ca249abbd25402c19cd31b12e7e7c6515a6e38a5386aa1a9f72` |

Resource 原生构建二进制与镜像 `/app/bin/server` 字节一致，SHA-256 `567b5d29bbaebed2f0a16d865cb357a441c4c992820c88458ec567c074a29f76`。正式 Resource 配置仅扩充已验证不冲突的 VPC CIDR 预设，保留原 `10.73.0.0/16`，增加 `10.75.0.0/16`～`10.78.0.0/16`；未改 Provider 门禁。完整部署回执位于 Fedora `resource/deployment/evidence/publication-result.json`、`governance-artifacts/delivery-receipt.json` 和 `/home/chabking/tasks/base-network-frontend-publish-20261103/`。

### 数据库与发布修复

原生 Network 0026→0027 迁移 PASS；原 26 项 checksum 保留，新增迁移仅调整上述两项 CHECK。执行前 `pg_dump -Fc` 及 `pg_restore --list` 均 PASS，备份 SHA-256 `bd76ca2283efaa46eed66066afa057ec54db8b1a44dbcd8b67195380c20c1b28`。旧严格 schema 镜像不能直接作为 0027 数据库的回退方案，保留原部署与备份。

Governance 业务库同样完成完整 custom backup 及 TOC 校验，SHA-256 `53289bea4d202b9617437919ebb1acfd7538018c88cc1139aa3fb2dcd1b197cc`；原生 catalog 同步与本批最小权限增量已执行。首次发布遗漏主干已存在的乱序 Atlas 迁移 `20261030010000 tenant_space_intent`，导致 `sys_tenants.tenant_space_state` 缺失、正常租户登录 401 和租户查询 500；授权路径的旧 403 也是该查询错误，并非 Instance 权限不足。按仓库正式部署流程通过原生 Atlas `apply 1 --exec-order non-linear` 补齐唯一缺失迁移，最终 18 项已执行、pending 0。未改密码、账号或 Compute 权限；fresh 正常租户登录、平台租户列表、initial-context 和 VPC 列表均恢复 200。证据为 `governance-artifacts/{auth-schema-stage.log,auth-postmigration.json,delivery-receipt.json}`，保留原失败，不将初次发布记为成功。

### 已完成的真实验收

- 自动分配缺省关闭：正常租户新建 `vpc_bac2c2741de546f3be444ac2455f5ec4`（`10.78.0.0/16`），同键重放返回相同 VPC/operation，SQL 恰好一组受理，基础连接/EIP/SNAT 均 0。真实 KC VPC UID `4061e790-36fa-4eb0-a8a3-e544fc619bb6` 达到 Ready；正式 DELETE 后 operation succeeded、物理 VPC 消失、活动 CIDR/operation/pending binding 均 0。原 7 个 KC VPC UID 保留。证据 `vpc-off-acceptance/result.json`。
- 管理权限：正常租户向新增 defaults GET 和合法 auto-allocation PUT 均得到 Gov 403，两个 request ID 及真实拒绝日志保留；按 Gov 实际时间校正 Fedora 约61秒时差后，Resource 对应窗口两个 RPC 调用均0，前后 SQL 的目标池/operation/幂等记录均0，默认源与 rollout 未变。平台 root 的 list200/defaults503 已通过认证授权、后者仅在 Provider 门禁失败。证据 `platform-put-permission/result.json`；未创建测试账号、API Key 或扩大授权。
- 正式 Console 空后端创建：`lb_9d66811e2c954d23b4e34a5c9f354fd5`、private/small、VIP `10.73.0.101:8081`。页面显示等待后端；target/applied 均 0，标签选择器核查本 LB 的 Gateway/Proxy/Backend/Route/Policy 均 0。配置更新重放返回同一 operation，旧版本请求返回 412 `VERSION_CONFLICT`；浏览器旧表单保存后监听端口仍为 8081，未覆盖并发描述更新。
- 纯数据库 LB 删除与 VIP 产品占用释放：两个专门空后端样本依次使用同一 `10.73.0.102`，每次正式 DELETE 均 succeeded/deleted，第二次正常受理证明第一组占用已释放。该结果仅证明未下发分支，不冒充运行中数据面清理。证据 `live-evidence/lb-empty-delete-summary.json`。
- 正常 Image→Instance→Attachment 真实业务后端：首次现有官方 Nginx 在既有 UID1000/drop ALL 下因缓存目录权限失败；未降低安全上下文。通过正式租户镜像发布/注册兼容静态 HTTP 镜像 `img_07d36296f16b4fbc96af3cded04a6a4c`，digest `0a0041e80d18aabb8747ae0808440a7aeee04ab8d7d4106fee1cbe3125169919`；正常 Instance `41330fcd-f726-4c99-b5aa-3ac488c2da8e` running、operation completed，Attachment `att_b4905919fee8414882324f769521e92d` attached，真实 Pod UID/GID1000、Ready、restart0，同 VPC `10.73.0.15:8080` 返回 HTTP200/`base-lb-http-uid1000-20261103`。
- 正式 Console 添加后端并激活 private/small：member `848e5235-bb16-497e-bb52-bd9b41226261` 绑定上述 Attachment，专属 Proxy、Gateway、Backend、Route、Policy 正常下发，Gateway UID `5faed532-b96e-4b74-b99b-2c11f43cf068`、Programmed True、2/2 Envoy 可用；desired/target/applied=3、available/configured。同一正常业务实例访问 `10.73.0.101:8081` 实测 HTTP200 与正确 marker。此 VPC 使用此前保留的手工基础 SNAT，不能记为新 VPC 自动分配主链通过。
- 正式 Console 权重 1→3：desired/target/applied=4、available/configured，实际 Route configuration4、weight3、Accepted/ResolvedRefs 与当前代次一致；Gateway UID、member ID 和 Attachment 均保持，VIP 仍 HTTP200。随后在正式页面清空最后成员，VIP 不再转发、业务后端仍200；等待原生 360 秒 Envoy 退出宽限期后，desired/target/applied=5、等待后端，全部自有 CR 和生成 Deployment/RS/Pod/Service/EndpointSlice 匹配0。未强删 Pod 或改变 grace。
- 为顺序验证中型容量，经正式平台 API 新建本次专用 CPU preset `a66ffe3d-e5ec-49a2-93ee-560f019ed04f`（100m/128Mi），正常租户创建同镜像 HTTP Instance `fb7502b6-3acd-4e33-aeb4-416ba4096ebf`、Attachment `att_8422c9ba45e7477999fa0cfceeb7504b`，`10.73.0.18:8080` 实测200/正确 marker。沿正常产品删除本次失败 Nginx 和先前 1CPU HTTP 测试实例；不改旧 preset、平台请求量或 LB 固定规格。最终保留小规格后端用于主 LB 再激活，原1CPU后端仅作为上述初次流量验证历史。
- 私网 medium 专用样本 `lb_e7ab1ebb1af145b3855a85e2a02ce947`：同键创建重放同 LB/operation；真实4/4 Envoy Ready，每副本 requests2CPU/2Gi、limits4CPU/4Gi，额外 shutdown manager10m/32Mi；未改规格或调度配置。desired/target/applied=1、available，正式后端访问 `10.73.0.103:8083` HTTP200/正确 marker。通过正常 DELETE operation `e4602961-35c4-400a-b1b8-17712a48abeb` 删除后，operation succeeded、API deleted/reason空，旧 Gateway 及全部自有 CR、生成 Deployment/RS/4Pods/Service/EndpointSlice 匹配0，VIP103不再转发、业务后端仍200。SQL 核对 `vip_occupied_revision=89115246`、后续 `vip_absence_revision=89126006`，同 VIP 活动产品占用、active component/generated 均0，容量释放；完整证据见 `lb-backend/evidence/empty-medium/`。
- 正式 Console 再添加新后端后，主 small desired/target/applied=6、available/configured，真实2/2副本保持每 Envoy requests1CPU/1Gi、limits2CPU/2Gi；VIP101:8081再次返回 HTTP200/正确 marker。新 Gateway `lb-065406b03c474924ba3b1fff6e240476` UID `b6a4ea53-32e4-4dbd-a659-7cd5c2bd51e7` 不同于旧 UID，新 member `7a7349e1-08b6-47e2-929b-becc664896e4` 不复用旧 member，绑定当前 Attachment、weight3。正式页面刷新后可见 available/已配置与新成员。证据 `lb-backend/evidence/reactivated-small/result.json`；保留供用户复核。

### 环境阻塞与保护范围

（本节保留 R1 发布时点的失败与归因；后续重放已定位为 Resource 的两项取证缺陷，不能继续把它们单独归因于 ani-03 故障，修复证据见下节。解除这两项误拒绝不要求先恢复 ani-03。）

BOSS 正式 `http://172.16.101.10:30082/basic-networks` 浏览器已登录并填写新建表单，但 defaults 查询 503 `BASE_CONNECTIVITY_NOT_READY`，提交按钮禁用，未发池创建 POST。默认 VPC UID/Ready/代次、默认路由和 Service CIDR 覆盖都通过；现有 KCN `providerImages` 健康门禁因一个 Pending controller、一个自 10 月 30 日删除中的 ani-03 controller 返回缺失。ani-03 自该日 NotReady；既有 Fedora/本机 SSH key 均被拒，任务尚无可用节点登录方式。未更改 KCN 控制器、默认 VPC 或放宽门禁。开启自动分配、默认源切换、无人工 verification 的新 VPC 真实自动 EIP/SNAT，以及其删除仍待该环境前提恢复；真实 PG/adapter 行为测试与现场验收分别报告。

产品公网池 `pool_1c5c2bebf286440fa7ed5006c2256021` 仍 degraded/`PROVIDER_UNAVAILABLE`、观测过期，租户产品 EIP 列表为空。仅一次正常 CreateEIP 在 `poolReady` 受理检查返回 503 `PUBLIC_EGRESS_NOT_READY`，request ID `f228d641-dcbc-49ac-b0cc-a6e90eb22cf5`，未持久化 EIP 或调用 Provider 创建。产品公网/双模式 LB 真实流量未运行；原手工公网 `.194` 流量试验不替代本次产品闭环。证据 `public-lb-prerequisite/result.json`。

large 固定6副本，每 Envoy requests4CPU/4Gi、limits8CPU/8Gi，另含 shutdown manager10m/32Mi，合计24.06CPU/24.1875Gi。最终样本恢复时，ani-01 已请求14.660/15.600CPU、余0.940，ani-02 已请求7.740/15.600CPU、余7.860，Ready 节点总余8.800CPU/21,881,091,710B，分箱最多1/6副本；ani-03 NotReady。该时点因容量不足未运行，未创建六副本占用或缩小规格，历史证据保留于 `lb-backend/evidence/reactivated-small/large-capacity-final.json`。用户随后明确“跳过大型”，本轮 large 运行验收现为 **SKIPPED（用户明确跳过）**，不再列作待完成场景；小/中规格和固定映射保持原样。

跳过仅适用于大型规格。基础网络自动分配代码虽已实现并发布，真实 BOSS→新 VPC→基础 EIP/SNAT 主链仍卡在上述 defaults503，业务交付尚未完成；公网/双模式产品验收的前提阻塞也未解除。

用户授权的闲置样本清理释放旧 Inference 的 8 CPU/16Gi，以及 Notebook/KServe manual 样本的 1 CPU/1Gi；旧 Inference ani-03 上的 Pod 仍未物理删除，故其整体删除保持 pending，不强删 finalizer 或伪造完成。保留此前私网/公网 KCN 对照试验、基础 EIP `.2/.3`、公网 EIP `172.16.102.194`、平台默认 SNAT `.192`、用户 VM 和新正常 HTTP 后端/私网 LB 主样本；手工 CR 不采用为本次产品管理资源。43927 预览未修改或停止。

本轮主保留资源：LB `lb_9d66811e2c954d23b4e34a5c9f354fd5`（`base-flow-private-small-20261103`、`10.73.0.101:8081`）；Instance `fb7502b6-3acd-4e33-aeb4-416ba4096ebf`（`base-lb-http-small-20261103`、`10.73.0.18:8080`）；上文100m preset、兼容 HTTP Image/tag、既有 VPC/Subnet。完整清单及已删除专用样本见 Fedora `lb-backend/evidence/final-retained-resources.json`。正式页面为 `http://172.16.101.10:30082/basic-networks` 和 `http://172.16.101.10:30081/load-balancers/lb_9d66811e2c954d23b4e34a5c9f354fd5`。四仓 PR 均不自动合并。

## 后续诊断与 R2 修复、发布和真实验收（2026-11-03）

R1 把 defaults 503 与产品公网池过期统一停在“ani-03 NotReady，等待节点恢复”，该归因不完整。Fedora 对真实现场事实重放并沿正式观察路径定位到以下两项 Resource 缺陷。ani-03 的实际故障仍保留，但不能用它替代修复这两项误拒绝。下面先记录真实调用链回归，再分别记录 R2 实际发布与 BOSS→新 VPC→唯一基础 EIP/SNAT→三模式 LB 的真实流量；不以测试或 Deployment Ready 代替业务验收。

### H1：额外不可用副本抹掉健康角色镜像证据

使用原始 VPC、ConfigMap、ServiceCIDR、Pod 快照，经 Kubernetes dynamic fake 外部边界调用真实 `KCProvider.GetIntranetPoolDefaults`。旧实现下，身份、VPC Ready/当前代次、路由覆盖、控制器默认 router 与基础设施检查全部为 true，但 `providerImages=[]`，返回 503；仅保留快照中真实 Running、Ready、非删除的 Pod，其他事实不变，即返回原默认 VPC UID `d0235860-f51a-4e63-8f4f-1ca1b34bc04f`。再单独放回一个 Pending 或 Deleting controller，旧实现均再次 503。日志 `defaults-503-diagnosis/snapshot-replay.log` 的 PASS 表示成功复现旧错误，不是 defaults 通过。

修复后以每个完整健康实例收集摘要：必须非删除、Running、PodReady=True、全部容器 Ready 且有合法实际 SHA-256；四个既定角色仍须全部有证据。Pending、Deleting、PodReady=False/Unknown/缺失、容器不完整或摘要非法的副本既不提供证据，也不清除同角色健康实例的证据。未移除镜像门禁，未把缓存的 containerReady 当作 Pod 当前 Ready，未修改 KCN 部署或强删 Pod。

同一完整现场快照在 R2 中返回原 UID，取证日志保留真实摘要 `sha256:494432d2f7b896eb953647166c6aa18d6ea2113b133d40d82127cfcbe416712f`；完整快照、仅健康实例、额外 Pending、额外 Deleting 四组均通过，见 `defaults-503-diagnosis/snapshot-after-fix.log`。正式回归从真实 GetDefaults seam 先 RED 后 GREEN：额外 Pending 的 `red-scheduling.log` exit1 → `green-scheduling.log` exit0；每角色缺失/只有坏实例及健康实例旁存在坏副本的 `red-ready-role.log` exit1 → `green-ready-role.log` exit0（0.079 秒）。后者同时证实并修复旧实现误采 PodReady=False 的问题。日志位于 `resource/provider-evidence-fix/`；只替换 Kubernetes 外部边界，不模拟默认身份选择或业务门禁。

### H2：已有绑定观察错误套用全员在线要求

产品公网 EIP 受理先在持久池的 degraded/stale 状态返回 503。继续追踪为何无法刷新：现场 KC Public Subnet、Gateway、VLAN 的身份、规格、Ready、当前代次及关联条件通过，`underlayState.chassisNode=ani-02,ani-01`，两节点与各自 CNI 当前健康；完整冻结成员仍为 ani-01/02/03。`virtualSwitchInventoryMode(..., false)` 的末尾布尔参数在旧实现被忽略为 `_ bool`，导致已有绑定观察也要求所有冻结成员 Node/CNI 在线，ani-03 Unknown 即使未被选为当前出口，仍阻止池刷新。现场只读事实及调用链见 `defaults-503-diagnosis/public-gate/{result.json,recovery-analysis.md}`。

R2 明确区分新物理配置准备与已有绑定观察。已有绑定仍校验完整冻结成员数量、名称/UID、allocated、priority、binding owner、组及设备确认、Provider UID/spec 和当前收集证明；不删除或替换 ani-03，不释放其接口占用。对已有 Underlay 池另外逐一核实当前选中 chassis：名称非空且不重复、属于冻结组、当前 Node/CNI Ready、有 chassis 身份且设备确认完整。选中出口失联、UID 被替换、缺少任一冻结成员或其设备确认、外来/重复 chassis 均拒绝。EIP/SNAT 使用该已有绑定及当前出口证据，不能被当成重新接管全部物理口。

新 VlanNetwork 或新 Underlay Public Subnet 首次物理 POST 仍在持久受理后、发送前严格重查全部冻结成员健康；坏成员时不发送物理写，成员恢复后恰好发送一次。既有删除 fencing、UID/RV、未知写及 KCN 释放确认不改。真实 PostgreSQL + 正式 `KCProvider.Observe/EnsureEgress` HTTP 外部边界回归：`red-existing-pool.log` exit1 → `green-existing-pool.log` exit0；`green-complete-membership.log`、`green-boundaries.log`（30.074 秒）、`green-product-eip.log`（8.590 秒）均 PASS。覆盖完整冻结身份/当前出口反例、首次物理 POST 重查、产品 EIP 正常受理与创建，以及既有 VirtualSwitch 删除/租约边界；日志位于 `resource/public-chassis-fix/`。

本节证据根目录为 Fedora `/home/chabking/tasks/base-network-20261103/`。R2 正式镜像发布、BOSS defaults/创建基础池、新 VPC 自动 EIP/SNAT 和公网/双模式真实流量结果待后续回执补齐；代码验证结果见下文，不提前记为现场通过。用户明确跳过 large 的范围保持不变。

R2 首次完整 `make verify` 已 PASS。扩大到相关 54 项 PostgreSQL 测试时，52 项通过，两处旧测试未对齐当前输入：`public_pool_optional_verification_integration_test.go` 仍要求人工 Intranet verification 和已由 0027 删除的 CHECK；`public_eip_workflow_integration_test.go` 的外租户安全组未配置其外部 Namespace，尚未进入权限业务断言就失败。前者改为通过正式业务方法开放无人工证据的 Ready Intranet 池，并确认不会重新打开已关闭的 Public 分配；后者明确准备第二租户 Namespace、保留原租户 UID，继续验证越权拒绝、无部分持久化、初始 SG、幂等和替换。第二轮越权断言通过后还暴露旧整体 `reflect.DeepEqual` 对 PostgreSQL/JSON 时间内部表示的误判；修正为时间瞬间相等且其他业务字段全等，未跳过时间或幂等检查。原始失败保留于 `resource-r2/verification/{postgres,postgres-fixture-green}.log`，生产文件字节未因此改变；两项修正后的 PostgreSQL 补测均 PASS，其余52项未重跑；最终完整 `make verify` 于 Fedora 2026-11-02 21:48 UTC exit0（132.706秒）。全部生成差异为空，全部生产文件保持首次通过的字节；最终测试候选 manifest SHA-256 `e107effe843a185fcfdeb2cf9d6c0a20fe9e664bb947dd777d0f48e528470a3f`。正式命令、退出码和原失败分别保留于 `resource-r2/verification/{result-initial,fixture-attempt-1-result,result}.json` 及 `make-verify-final.log`，不因仅补文档而重复已通过门禁。

### R2 正式发布与自动分配现场闭环

Resource 修复提交 `3fab1f19d570055f4508ee014538a39af144c5b0` 已通过 SSH 推至原 PR #46；实际运行版本 `base-network-r2-20261103-3fab1f1`、镜像 `172.16.101.10:30003/icn-01a15337-release/ani-resource-service@sha256:953f356c9a87ad81ec0c34809fde2d9cad7bd68bd51bf750d635451d19d7932f`。native 与镜像 `/app/bin/server` SHA-256 均为 `e44828665a805808a8a7931bf9e5b9d899e3b58191d4fa7e7d1a19994ffa9f7b`。Deployment 原 UID 不变、generation/observed=64、1/1 Ready/restart0；Config Secret UID/数据未变，未改 Schema、KCN、Governance 或前端。完整回执 `resource-r2/deployment/evidence/publication-result.json`。

原始 HTTP 红循环正式复验 PASS：defaults GET 返回200，0.896秒，原默认 VPC UID `d0235860-f51a-4e63-8f4f-1ca1b34bc04f` 与真实三段路由保持，request ID `f60a1b2e-817b-43e3-bd02-4d3d6e558342`；证据 `defaults-503-diagnosis/defaults-http-latest.json`。已有产品公网池同样恢复 `available/fresh`、分配/默认开启，ani-03 仍 Unknown，未改变其成员或 Ready；见 `e2e-r2/r2-public-pool-readonly.json`。

正式 BOSS 页面创建 `base-auto-main-20261103`：`pool_0b59b1d2f0e04508a243c2b58f245eec`、CIDR `10.250.251.0/28`、网关 `.1`，真实创建 operation `87461edc-8524-4df8-982e-f9872fd731b6` succeeded。随后页面单次“开启自动分配”操作成功，GET 证实 default/allocation/new_vpcs_enabled 均 true、verification=null。正式 Console 普通表单创建 `base-auto-vpc-main-20261103`：`vpc_bd5d4a4e965f465ba26f001ab11f16e9`、`10.75.0.0/16`，未传租户基础 EIP/SNAT 意图。实际工作流先 VPC Ready、EIP available、SNAT provisioning，再正常收敛为 VPC available/base ready/fresh；不是仅凭202宣称完成。

最终只读 SQL 与原生复验：基础 EIP `eip_8c7e7b6fe6bd4ec7aaded2528c8f8c6b`、SNAT `snat_e6cbb53fcdde47feb56a397fc4a0e28a` 均 available，base/EIP/SNAT 恰好各1，create operation/幂等记录各1，三份原生对象和真实 UID 均存在；证据 `e2e-r2/r2-auto-ready-{db,native}`、`main-pool-ui-{created,enabled}.json`。LB 流量与开关/源切换/失败恢复/专用删除结果分别见下文，耗尽后的原对象自动恢复 FAIL 单独保留。

### 新自动分配 VPC 的三模式 LB 真实验收

本批正常业务后端为 Instance `f3e49e17-bfb5-4d75-a0e2-96dcbe3e1757`、Attachment `att_ebb20adc14b64c54a56067eec19299f2`，子网 `subnet_3545029eb11542e693eb75e4564a47eb`（`10.75.0.0/24`）。实际 Pod UID `3aea4057-1de7-47d1-b5af-01cc5a9b0b72`、`10.75.0.2:8080`、100m/128Mi，沿正式 Image→Instance→Attachment 接入，无原始 Pod 替代或安全上下文降级。

| 模式 | 产品 LB | 实际入口与结果 |
| --- | --- | --- |
| private/small（正式 Console 创建） | `lb_70c22e8f491e4649beeaf3525ce051d9` | `10.75.0.101:8081` HTTP200 |
| public/small | `lb_cac01579371949fe95c177ad5a72faeb` | `172.16.102.195:8082` HTTP200 |
| both/small | `lb_e072d3e19ebc49409980e0e4a61d4b2e` | `10.75.0.103:8083` 与 `172.16.102.196:8083` 均 HTTP200 |

私网请求来自上述真实业务 Pod（hostNetwork=false），公网来自 ani-01 上真实 Ready KCN hostNetwork Pod，记录 Pod/Node UID；四入口均返回 `base-lb-http-uid1000-20261103`。三个 LB 最终均 available/fresh、CONFIGURED、desired/target/applied=1，各自 operation succeeded；正式配置结束后再实测入口，而非仅凭 Gateway Ready 或前期HTTP200。三个实际 Deployment 均2/2 Envoy Ready，每副本 requests1CPU/1Gi、limits2CPU/2Gi、restart0，owner链 Gateway→Deployment→ReplicaSet→Pod 及专属 Proxy 证据完整；未缩小 LB 规格。中型固定四副本及完整删除结果仍沿用上文 R1 已完成样本，大型按用户要求 SKIPPED。

独立公网 EIP `eip_138a817e83bd40a0af6d93dfdcda1390`、双模式 EIP `eip_5bf9f87a005e413f92cf2a65073ab909` 仅作为 LB 入口；主 VPC 的基础 SNAT 恰好一组，未额外给其绑定公网 SNAT。全部主样本保留，旧 `.194` 公网试验、`.192` 平台 SNAT、原 VM 及旧实验未变。

原始过渡保留：新 Instance 曾 reserved/INSTANCE_NETWORK_CONFLICT，现有观察后自然 attached，三方 Pod UID 一致；首个公网 EIP 曾 context deadline，沿同一持久 operation 重试后 available；公网 LB 第一次300秒观察窗口结束时仍 applying，原 operation 后续正常 succeeded。未通过删除重建替代样本或改 DB/CR 伪造成功。主 VPC 在并发创建期也出现过真实 stale/degraded，后续原 EIP/SNAT 续报后恢复 Ready/fresh；未调宽现有 stale_after60s、observe_every10s、audit_interval30s、request_qps5/burst10。默认源切换只改变后续分配，主 VPC 的池/EIP/SNAT/Provider UID始终固定，期间私网和公网实际流量通过；不把 transient 状态掩盖为全程零错误。

完整组装回执 `e2e-r2/result.json` 已核对全部原始 API、operation、native 和 curl 证据及实际 R2 镜像 digest。私网/公网最终 operation 分别为 `64a1fca0-ad2b-4cbf-89f5-e80e1a20d329`、`aade8246-a8ca-4e8b-a395-7c9855e3c924`，双模式为 `aa2dcc75-1d36-4b40-980e-3eceb2688fdd`；最终 API 服务器时间为 UTC22:07:16/17 和22:11:17。

### 默认源切换、耗尽反馈与恢复边界

专用辅助池 `pool_21c658cc5460444bbe205162524d48f4`（`10.250.252.0/30`，网关`.1`，唯一可分配地址`.2`）通过正式 SetDefault 切换，默认/分配/新 VPC 自动分配全 true。随后创建76 VPC `vpc_61629e6b2836499389d57f595ec76ab1`，基础 EIP获得`.2`；77 VPC `vpc_bc8172d7cc6a41178055354918b8a21f` 的基础 EIP `eip_67cc52dc452a413587df94cc841d1029` 无地址。两组启用态请求均实际同 body/key 再 POST，返回相同 VPC/operation，SQL create/idempotency和子资源各恰1，主 VPC 固定池/EIP/SNAT/Provider UID不变。

耗尽反馈 PASS：77 EIP 原生 UID `018fc9d5-8b63-46d8-9487-dd4f82253bac`、generation1、Valid=False/Error/`Failed to allocate ip address`；同一辅助池 available0/using1/usingRange`.2`。正式 Console 显示 provisioning、基础连接“配置中”，原因是标准文本 `The requested configuration is not ready yet.`；没有地址不足专用前端文案，精确IPAM原因在原生证据中。早期仅看到 PROVIDER_UNAVAILABLE、EIP尚未创建时的结果明确保留为未分类，不提前冒充耗尽。固定原始0/1证据在 `auto-semantics/exhaustion-result.json` 的 state；辅助读取的同名pool文件后续会更新，不作为不可变原始状态。

正常 DELETE76 的 operation `ad089c22-579b-4656-8ff0-bd9013eb1d19` succeeded，原 VPC/EIP/SNAT 全物理消失、活动claim/VPC占用0；辅助池恢复 available1/using0/availableRange`.2`。但是77原 EIP UID/gen与失败 condition 在释放四分钟后仍未变化，Resource正常观察未触发重新分配，**原对象自动恢复 FAIL**。没有为了通过而改 CR、换 UID、调阈值、重启 KC 或把正常删除重建冒充同对象重试。

对应点时 KC 本地输入为 `/home/chabking/workspace/kc-networking`、main `6132623baabee50f17e9e179c0f5e9c1f380db04`，origin `git@gitlab.changqingyun.cn:kubercloud/sdn/kc-networking.git`。`internal/controller/networking/eip_controller.go:188-202` 分配失败只记录并返回nil，214-219写ValidFalse后返回nil，75-120成功结束无Requeue，670-687只监听EIP generation及Nat/Snat/Service、无Subnet变化；`internal/controller/handlers/misc.go:123-125` 空地址校验的invalid文本不带错误。现场与该逻辑吻合，未核对部署KC镜像的源码revision，不能把本地HEAD冒充部署证明。Resource已知UID分支沿正式观察，没有租户修改系统基础EIP或RetryEIP入口。本轮不扩大为KC代码/控制器发布；正常删除失败VPC后普通重建的恢复结果另行记录，与上述自动恢复FAIL分开。

正常产品删除重建恢复 PASS（与原对象自动恢复 FAIL 分开）：77原 create_vpc/create_eip/bind_snat 终止为 CREATE_TERMINATED，正常 DELETE `b8a931d7-2887-4c21-bcc0-b7150240ab7c` succeeded，原三份 CR/活动占用均消失。以新 key 正常创建77b `vpc_6de14bcc58294acda27db68cea6bd075`、同CIDR `10.77.0.0/16`，operation `a0125c25-9af5-4fab-856d-e4f1631f748e`；同 body/key 实际重放返回相同新ID/operation。新基础EIP `eip_be7c2e8284344d74b57d9c6bc15e5dce` 重新获得已释放的 `10.250.252.2`，新SNAT `snat_0f983d2c95964542936cc19683ad183f`，新VPC available/baseReady/fresh。旧77 tombstone与失败证据未改，未复用旧key或伪造同UID恢复。证据 `auto-semantics/manual-recreate-proof.json`。

关闭态 PASS：正式关闭后正常创建78 `vpc_89bfebfae1694c2bbe2c8a9a44eda218`（`10.78.0.0/16`），同body/key实际重放相同ID/operation，VPC available且base/EIP/SNAT均0、create/idempotency各1。77b provisioning期间一次正式 SetDefault 切回主池，GET证实主池 default/allocation/new_vpcs_enabled 全true；77b仍固定aux源/revision及其已受理子ID，随后Ready，主VPC的原池/子身份也保持。两方向切源、关闭只影响后续新建，不做历史补齐；证据 `off-result.json`、`main-restored-while-77b-inflight-result.json`。

专用资源删除与地址释放 PASS：77b 正常 DELETE `9d740ec0-29e4-4a7e-bb34-4f33922b5953`、78 正常 DELETE `51a4691b-4185-4af1-94eb-4b1a9b0eb5a6` 均 succeeded；76/77/77b/78 的原生 VPC/EIP/SNAT 均消失，相关活动 claim/VPC/CIDR 占用为0。确认无依赖后正常关闭辅助池地址分配，再由正式 BOSS 页面删除且仅删除该辅助池，operation `f417c272-88ea-454e-9a03-1f925894311b` succeeded/reason空；只读核对原 Provider Subnet absence、持久 platform_state=deleted、retired=true、active_eips=0、active_claims=0。无强制 finalizer、手工 DB/CR 清理或重复 DELETE。

最终主 VPC available/baseReady/fresh、reason空，基础池/EIP/SNAT/Provider UID 与初始冻结完全一致；主池 default/allocation/new_vpcs_enabled 仍全true。证据 `e2e-r2/auto-semantics/{restore-cleanup-result.json,verify-cleanup-result.json,aux-release-final.json,auto-main-final-ready-fresh.json,auto-main-final-pool.json}`；BOSS 正式列表已只保留主池且两项开关开启。原耗尽 EIP 自动恢复 FAIL、新对象正常恢复 PASS 的两份记录同时保留，不合并为一个绿色结论。

### R2 最终保留与观察波动

`e2e-r2/final-retained/final-retained-resources.json` 的结果为 `FINAL_CHECKS_PASS_WITH_KNOWN_LIMITATIONS`。主池三开关、主 VPC/基础连接、三 LB 配置版本及 fresh 均在最终收尾快照通过；服务器 UTC22:47:17～18 四入口各只做一次实际请求，均 HTTP200/正确 marker。原7份 KC VPC逐一保留同UID；手工公网`.194`、平台默认`.192`及其SNAT的UID/spec/原绑定未变。清单相对链接 R1 保留清单、R2 API/native/curl 与上述删除/失败恢复原证据，不重复跑已通过规格或编辑场景。

不能把阶段 PASS 宣称为持续全绿：收尾时公网 LB 同 operation 在 UTC22:42:00 epoch79 出现 degraded/PROVIDER_UNAVAILABLE，22:42:37 GET 的观测年龄约73秒且配置投影 UNKNOWN；同 Gateway/Deployment/两 Pod UID 均不变，原生当前代次条件与2/2 Ready持续有效。22:43:12 epoch80 未经配置修改或重建即恢复 available。22:40～22:44 的349条日志没有该 LB 底层cause或明确 limiter/deadline/源集合失效证据，不能据 stale 推断限流/超时；背景其他资源冲突告警也不作为该 LB 根因。原红回执、一次正常恢复 GET 和原生证明保留于最终清单及 `e2e-r2/final-observation-diagnosis/`。

### R3 补全正式 LB 错误诊断链

进一步定位到日志缺口：LB worker 的观察错误分支只设置标准 Reason/Backoff，未填既有 `Progress.ProviderError`；正式 Observability 只有该字段非nil才记录原始 Cause。源码 `c8072f1405e1757f690fc1ac51194fa8ce442953` 仅给该分支补一行 `p.ProviderError = err`，沿用现有日志机制。新增行为回归从真实 Worker.Step 经正式 Observability 写 JSON 日志，Provider为外部错误边界，验证原始Cause/资源/operation/binding/epoch与原降级退避、无观察证明/无新Provider突变。

Fedora 独占 `resource-r3/`：新测试在原3fab生产代码上 RED（Cause丢失，非编译失败），补上述一行后 GREEN；最终 `make verify` 于 UTC22:55:33～22:58:46 exit0，生成diff空、源码字节/模式与候选一致，manifest `bcf78fb4d4e7b64945ed037a620941c7a4d59fe4af028988a76b983a6ea705cc`。证据 `resource-r3/verification/{result.json,behavior-red-result.json,behavior-red.log,behavior-green.log,make-verify.log}`。纯错误日志转交不重复 PostgreSQL、其他服务、前端或真实流量；R2业务验收与已知失败保留。该源码已通过 SSH 推至原 PR #46。

R3 实际发布：版本 `base-network-r3-20261103-c8072f1`，Registry 与运行 Pod imageID 均为 `ani-resource-service@sha256:c8782128a464baa018af985fa407cbd637c4c4ec3f1e44dc911c30a001ce17ef`；native 与镜像 `/app/bin/server` SHA-256 均为 `75ef349aceeb68a7c31a49a4e43b392ec3a0f068b8a1685b5ea43d87b583392c`。真实 Git commit/tree 与8545份候选源字节/模式一致，最终 commit manifest `4cec1ef6bd97c6bbfade134774eec92fcd0faf7bdb4700a2a61848443c655200`。Deployment 原UID不变、generation/observed=65、1/1 Ready，新Pod `ani-resource-service-75f8588c7d-w7845` restart0且日志确认R3版本；仅更新Resource镜像，原Config Secret UID/data、Schema与其他组件保持。回执 `resource-r3/deployment/evidence/publication-result.json`。后续只观察已有LB正常轮次，不以日志补丁重复业务流量或规格验收。

R3 窄观察最终结果：原公网 `PROVIDER_UNAVAILABLE` 错误分支未复现，不能获得该次可信原Cause，R2底层cause仍 **UNKNOWN**。旧私网同operation在UTC23:05:43 epoch827短暂 PROVIDER_NOT_READY，23:05:55 epoch828自然恢复；新private70c22同期epoch128→129亦恢复，这些非错误返回没有原Cause，不能宣称日志补丁消除了波动。正式Observability仅对活动操作、失败或状态/Reason变化写INFO，稳定正常轮次无INFO不等于worker停顿。

最后各只读一次正式GET：服务器UTC23:07:24，公网 observed23:07:13、available/configured/fresh、versions1/1/1、原operation不变；旧私网 observed23:07:03、available/configured/fresh、versions6/6/6、原operation不变，证明正常观察续新。证据 `e2e-r3/observation-cause/{final-result.json,resource-final.log}` 及该目录两份 final-api JSON；原R2红回执保留。未发业务写、探针、原生读取或流量重验，未无限等待全绿，也不把启动期readyz503当作原失败原因。

### R4 同步最新 main、解决 PR 冲突与最终发布

PR #46 收尾检查发现与最新 main `8b31a8e30b4ea0228e9fb433849c5b1c09d2d710`（VM共享观测修复#45）有冲突。`git merge-tree` 定位仅 `docs/execution/status.md` 的新记录插入冲突，解法保留LB与VM两边原文；所有上游VM源码自动合并、无本任务改写。暂存tree `b61efe9418827d604b2b835323ffa189fc705761` 的8548份文件/模式与Fedora候选完全相同，候选manifest `e46073d2ec982676111ea30e7c63f019e9b9a3c442395eeab7c9773546f3710a`。合并候选本轮唯一 `make verify` 于UTC23:15:43～23:19:42 PASS，生成diff空、无verify后源变化；没有重复PG/前端/规格。上游VM的原PG/race证据继续使用其[原验证记录](vm-observation-scope-20261102.json)，不冒称本任务重跑或现场VM行为验收。

正式合并提交 `d6744abbca93f887bb679610e29d46877196ccc8`（父9bf7863与8b31a8e）已SSH推至原PR，GitHub实际检查mergeable=true、无合并冲突；不自动合并或绕过分支规则。真实commit tree与通过候选一致，最终commit manifest `6029d5c77e3a939b49e1256f71b4fffda1f8d35dbe8b1546cb1411cda17427b3`。检查及源证据见 `resource-r4/verification/{result.json,make-verify.log,candidate-source-check.json}`。

最终线上版本 `base-network-r4-20261103-d6744ab`，Registry与Pod实际imageID均为 `ani-resource-service@sha256:4c651e6e91d22bb093d660717638cf020f277b065b2563d11b83f16822a0e852`；native与镜像 `/app/bin/server` SHA-256均为 `7ea223326b06adaf9b775c6d442692e1dc95b45be8e7df3700a3c65595116e87`。Deployment原UID不变，generation/observed66、1/1 Ready；Pod `ani-resource-service-7c9b7f5c-pbdxh` restart0，日志确认R4版本。只改Resource镜像，ConfigSecret UID/data、Schema及其他组件未变，无新迁移；完整回执 `resource-r4/deployment/evidence/publication-result.json`。

R4 部署后的必要烟测仅运行一次，结果 `SMOKE_PASS_WITH_KNOWN_LIMITATIONS`、failures=[]。入口核对10集群UID、上述实际新源/immutable digest/native版本；主池default/allocation/new_vpcs_enabled全true、verification=null，主VPC/baseReady/fresh、原池/EIP/SNAT/VPC Provider UID固定；三LB均available/configured/fresh、原operation succeeded、desired/target/applied1/1/1。四入口各只发一次真实请求，均HTTP200及正确marker；原7KC VPC UID、manual`.194`/默认`.192`及其SNAT UID/spec/绑定均保持。未出现本轮红状态，未轮询、未业务写、未重测规格或CRUD。

最终保留清单 `/home/chabking/tasks/base-network-20261103/e2e-r4/final-smoke/final-retained-resources.json` 已相对链接R2业务证据/清理、R3诊断与R4正式部署回执。**原耗尽EIP自动恢复FAIL、原观察causeUNKNOWN、large用户SKIPPED、持续全绿NOT_ASSERTED** 均保留；一次烟测通过不消除这些限制。四仓PR保留原open/draft状态并更新证据，不自动合并；本段只补文档，不重复已经通过的生成/测试/构建。
