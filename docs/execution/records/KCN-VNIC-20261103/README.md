# 手动 VNIC 实施证据

实现输入为[计划](../../../plans/kcn-vnic-management-20261103.md)，三仓固定基线分别为 Resource `46a2b8e`、Gov `b6262869`、Console `87b75acd`。当前状态只在[执行状态](../../status.md#kcn-vnic-management-20261103)维护。

## 源码与定向验证

- 主机：`ssh fedora`；Resource 独立源快照 `/home/chabking/tasks/vnic-resource-20261103/source`；Network PG 快照 `/home/chabking/tasks/vnic-network-20261103/source`。本地仅源码、Git 和传输。
- 工具：Go `1.26.10`、Buf `1.60.0`、sqlc `1.31.1`，生成物从正式 Proto/SQL 输入生成并回传。未手改生成物。
- `go test`：Network/VM/Instance 的领域、API codec、data、service、server 和组合根通过；新行为包括保存不修改运行端点、取消预留、模板版本与运行确认分离。
- 真实 PG：`TestManualVNicPGReplayReservationRetentionAndDelete`、`TestManualVNicPGExclusiveReservationAndStableRead`、`TestManualVNicPGServiceTrustedCaller` 全部通过。仅 KCN 分配回执使用外部边界替身；持久化、幂等、租户、预留、worker 与删除为真实业务代码。过程中修复删除观察未带分配事实时将地址写 NULL 的缺陷。任务 PG 容器已清理。
- 按仓库要求执行 `make verify`：生成一致性、领域边界、故障构建、mod tidy 及业务测试通过；旧 `scripts/lb-api` socket 用例因长 TMPDIR 失败。使用短临时目录单独复验该包通过，不重跑已通过测试。后续 `go vet` 指出新测试按值复制 protobuf 锁，改用 `proto.Clone` 后 `go vet ./...`、`go build -trimpath ./...`、`go mod verify`、`git diff --check` 通过。保留原失败，不将首次命令记为全绿。
- 原始日志：Fedora `/home/chabking/tasks/vnic-resource-20261103/` 对应本地采集的 `vnic-fourth-compile.log`、`vnic-verify.log`、`vnic-verify-remaining.log`、`vnic-verify-final-steps.log`；Network PG 日志在独立 Network run。Console `935321e` 的 Fedora 源码检查已通过并 SSH 推送。

## 真实环境

Fedora 经受控 SSH 登录 `ubuntu@172.16.101.10`，kube-system UID `5277649d-e28d-4a0a-ae76-8354365c63bc`；初次探测节点 10/11 Ready、12 NotReady，后续测试 Notebook 已实际在 ani-03 运行。此前 BatchMode 密钥认证失败不是节点不可访问；旧 loopback kubeconfig 隧道不作为本轮集群凭据。

Resource 业务源最终为 `b70271b0d936f59c91e99f3ae9e9e4f3ff1e287a`，镜像 `sha256:4db32668803eb8d1f7b675f0d0eba2856912f4720295be5d360a007c40a349f9`，generation85/observed85/Ready1/ani-02，原500m/256Mi请求保留、Restart=true。ani-01 CPU 不足时仅调整了 Resource 的 nodeSelector。Gov `aed3378a` 镜像 `sha256:524bfd5eb0c76b51529457fc289333e8d81b84f7522072d98ef4177a307a82eb`，正式 SDK `v0.0.0-20261103092013-f4711fd8f7e0`；Console `e741b25` 正式分支独占预览完成真实页面，未改用户开发服务或发布生产 Console。最终回执为 Fedora `deployment/retained-ip-b70271b/release-result.json`。未修改 KCN，未新建数据库备份或额外发布门禁。

`7185724` 已落成用户确认的 SG 成员充分条件：对应真实 PG 单例 PASS，Ready=false/旧 observedGeneration 但成员包含仍成功，缺成员或旧组残留仍等待，最终清空不依赖这些额外字段。原 UID/租户/位置、消费预留和 owner 最新版本仍核验。

## 页面到运行事实与历史阻塞

2026-11-03 正式 Console 分支的独占预览完成 VM `22207203-060b-4f86-8666-186574150b04` 与 Notebook `fb41b4ac-d185-41b8-8aca-01f8cceafab8` 的默认自动创建。实际 Notebook→VM guest SSH，以及 guest→Notebook HTTP 均通过；guest 观察 `10.75.0.9 / fa:00:97:6b:b3:f5`，Notebook 为 `10.75.0.10`，另验证 Notebook→guest HTTP。未为默认模式新增手动产品记录。

页面申请 A `vnic_9609943037864545aa447e0ba8e02056`（10.75.0.11）及 B `vnic_f090bf4ede0347969c5598096f4288c9`（10.75.0.12）；A 原 UID `b4922de6-a321-492c-85af-944adb05f4db`。原创建请求同键重放保持产品 ID、operation ID、原 UID 和唯一 CR；重放仍返回原创建快照，地址以随后 GET 为准。请求与实际流量证据位于 Fedora task private 的 `create-replay-proof.json`、`default-network-proof.json`，页面原请求由 Console task 保存，不含 Authorization。

17:00:57 抓取时，VM1 保存 operation `8d2bc3dd-dc2c-4c2a-a14e-e8ad22b28ca1` 为 running（network provider temporary）。目标 VNIC spec 已引用 allow SG；SG Ready/observedGeneration=1，appliedVNics 同时包含目标 A 和当前自动 VNIC，但两张 VNIC 的 status 均无 appliedSecurityGroups。固定 KCN `6132623` 的 `vnic_controller.go:770` securityGroupUpdateHandler 为空。用户随后转达 KCN 团队确认：SG.status.appliedVNics 含目标即可认定绑定成功，无需 VNIC.status.appliedSecurityGroups。Resource 按此修正确认条件；旧抓取保留为历史证据，不能再将缺少该字段列为外部阻塞。目标 A 的 nic/dstNic/nodeName 均空，当前 Nic Attached 的仍是原自动 VNIC；保护现场不删除、解绑或重启，原保存允许自然收敛。

本批定向修正接正式纯读 Attachment 事实显示当前网卡/地址和产品名称；配置保存未完成时独立显示 configuring/operation reason，不能提前宣告待重启就绪。重核同一配置复用绑定，避免 Restart 的新 operation ID 生成重复有效关联。上述字段和读取/复用行为按其针对性用例验证，不重跑无关全量。SG 写入版本围栏 `1323796` 此前单独暂存；按 KCN 新确认口径修正后，一并部署以恢复保存主链。

SG 确认修正 `cd4fb81` 的 Fedora PG 用例先复现缺少 VNIC 回执的误阻塞，随后成员与版本围栏用例通过；该版仍保留的 SG Ready/observedGeneration 完成条件随后按用户“只要成员包含即成功”的口径移除。最终应用回执只确认当前成员/旧组移除，不与原资源身份、准入或 owner 版本围栏混为一谈。VM/容器 `TestNetworkView` 定向通过：当前网卡视图只使用网络自身的观察原因，不继承 owner 先前诊断；真实来源仍有错误时保留该错误。日志为 `/home/chabking/tasks/vnic-resource-20261103/owner-reason-test.log`，未重跑无关检查。

容器普通观察的 `confirmNetworkInterface` 曾把合法待重启配置判成依赖不可用：保存 B 时原 Pod/网络不变但主状态 pending，取消后同 Pod 恢复 Running。`069fea8` 只在普通观察待重启期间保留当前确认，生命周期路径仍严格验证新模板/端点；Fedora RED 后四个相关用例 GREEN。仅测试提交 `10646d0` 补计划明确要求的创建响应丢失恢复与 Reserve/Delete 竞态，真实 PG 两例通过（`vnic-recovery-pg-01.log`）；前者仅在外部 HTTP 创建成功后断连接，确认原 name/UID/operation 且 POST=1，后者确认删除与预留只有一方成功。没有新增集群故障或镜像门禁。

按新口径，A 保存 operation 已 succeeded；原 VMI `f206a79a-98ce-4d9c-87f6-a6d5aed94581`、launcher `7bd2bb6c-82fc-43e5-83cf-cdc4ce112ab2` 未变，当前仍自动地址 .9，A 为待重启目标。证据 `private/sg-membership-a-proof.json`。

独立 VM2 `c3bd9fef-e81e-42a4-9ca0-342cca09d474` 保存 C 后原 VMI/launcher/template 与 .14 guest 通信保持；页面 Restart operation `fcb4f6b3` succeeded，新 VMI `686f8298-943c-4104-8654-f1da367ab6dd`、launcher `0bf198fb-75fc-48ae-9669-1d932420f8d3` 实际使用 C 的 .13/fa:00:b1:45:21:c8。guest SSH 与双向 HTTP PASS，正式视图 effective_now=true/revision2/2；C 原 UID `da2d9ba9-8a22-4f1d-99c3-310fef8ea075` 且无 ownerReferences。旧自动 VNIC/launcher 正常消失。随后 Stop succeeded，C/IP/逻辑绑定保留，停止态当前网卡地址为空。证据 `private/pending-c-proof.json`、`manual-c-proof.json`、`manual-c-stopped-proof.json`。

Notebook 页面 Restart 初次在 Gov 接受前返回 503 INSTANCE_ACTION_UNAVAILABLE，未生成新 operation；确认为 Gov/Resource 两端缺少 ANI_INSTANCE_RESTART_ENABLED。只给各 server 增加 true，保留原镜像/节点/其余配置；两端 generation167/82 均 observed 且 Ready1。原 UI 幂等键保留后继续，不将这次拒绝记为重启成功。

## Notebook 真实模板与共享观察收口

v1.10 CRD 不支持 `spec.template.metadata`，controller 仅在生成 STS 时复制 root metadata，更新已有 STS 仅复制 replicas/PodSpec。`6930be3` 在 SafeStopped、无受控 Pod、root/受控 STS 原 UID/RV 再核对后正常删除旧受控 STS，由原 Notebook controller 重建；不写被裁剪的字段、不 patch STS/Pod，Notebook/PVC/VNIC/绑定保留。独立 writer Role 仅在目标 tenant namespace 允许 statefulsets delete。新 STS `ec17c5a6-a502-406f-a0c6-3161fbee74fb` 生成正确 B/revision4，旧 STS `376cf0cc-996d-41ed-879d-29a3bdceff14` 退出。Network `c5dce3f` 仅接受快照已明确登记、同原 Notebook owner 的重建 controller 为现存依赖，foreign UID 仍拒绝；两项针对回归 RED→GREEN。

原 b55 新 Pod .12 已实际 Running，但共享观察候选缺 ManualVNics，`kc_attachment.go:290` 将 B 误判缺失；同一 revision9 正式快照、PG/direct Provider 成功而 shared Provider Conflict 的用例 RED→GREEN 后，`c710a54` 将已接受 B 原 UID/名称加入现有索引。`e1e4c49` 仅允许 waiting_network 的 protocol reason 继续原 owner Confirm；Confirm 失败继续 blocked，成功后下一轮仍须正确 Pod/snapshot/无错误观察才完成，不清 SQL flag 或历史。Fedora 两个对应回归 RED→GREEN。

原 b55 在修复期间触发既有 deadline，终态 failed/INSTANCE_OPERATION_DEADLINE_EXCEEDED 并安全停止，未复活或延长时间。该批 `e1e4c49` 发布后，正常新 Start `5ec174e4-4b1b-4fe2-8694-0fe5e95f1b8f` completed，新 Pod `38e255e9-890e-4c95-99c0-cf1e0d947fe9` .12、原 B UID/MAC、正式视图 effective/revision4/4，证据 `private/b-recovery-{op,network}.json`。新的真实普通菜单 UI Restart `291f0488-4078-4c75-8a5a-58c0d3be1dff` completed/done/runtime_verified，新 Pod `4489dd7e-87c7-4a92-b207-8155ea816472`；页面“已生效”，截图 Console task `ui-notebook-b-restart-effective.png`。旧失败与新成功分别留存。

## VM 保留地址与 SG 实际策略

VM2 Stop 后 C 原 UID/IP/逻辑绑定保留，正常 Start `6949921d` succeeded，新 VMI `58fe4d96-676c-4018-b2e7-6444fb81347b` 仍使用 C .13/MAC fa:00:b1:45:21:c8。SG op `0ba75636` 新增 drop 组成功，即使 C.status.appliedSecurityGroups 为空，SG.status.appliedVNics 已包含 C；实际 B 客户端到 C TCP8080 超时，而 SSH22、guest 本地 HTTP、guest→B HTTP 成功。替换独立 allow 组 op `f6968a35` 后 8080 恢复，旧组成员移除；清空 op `d8b2c6b9` 后 spec.securityGroups=[]、旧组成员消失且 8080 仍通。全程同 VMI/C UID，没有用重启代替热更新。证据 `private/manual-c-sg-{application,drop-proof,replace-proof,clear-proof}.json`；流量脚本临时 HTTP 进程由前台 SSH 会话持有，已结束该客户端会话。

## 换绑、退出、跨实例复用与显式释放

所有下述请求均为正式 Gov→Resource→持久化/worker→实际 KCN/KubeVirt/Notebook 路径；只有前述 PG 故障用例替换外部 KCN HTTP。以下 `private/` 均位于 Fedora `/home/chabking/tasks/vnic-resource-20261103/`。

| 实际结果 | 证据 |
|---|---|
| VM2 保存自动目标时当前 C 保持 .13/Running；取消恢复 C，预留集合收敛为一条当前/目标关系。Notebook 正常 Stop→Start 保留原 B UID/MAC/.12，停止态无当前地址 | `private/vm2-pending-auto-proof.json`、`cancel-auto-and-b-reuse-proof.json`、`manual-b-stopped-proof.json` |
| Notebook 平台重启恢复自动模式，当前自动 .15，新 Pod，B 原 .12 变空闲而不删除 | `private/notebook-auto-restore-proof.json` |
| VM2 current C/target B 同时继承 allow，再共同跟随 drop/allow；原 VMI 不变。预留 B 的 DELETE 返回412 RESOURCE_BUSY，取消后 B 空闲且保留最后 SG | `private/pending-pair-sg-and-cancel-proof.json` |
| VM2 平台重启 C→B，新 VMI/guest 实际 .12/MAC B，当前和目标均 B；C 原 UID/.13 空闲保留 | `private/vm2-manual-swap-b-proof.json`、`vm2-b-guest-proof.json` |
| VM2 Delete 原生/卷/媒体清理完成后，独立手动 IP 错被记为 Current 导致 closed Finish 拒绝。`b70271b` 仅修其消费者 Current 语义；真实 PG `TestVMBlockedAttachmentPGReleasesRetainedManualIP` RED→GREEN，闭合屏障正常恢复、历史关系/IP对象不删除。原3aa09f76自然 succeeded，B/C空闲且原UID/IP保留，SG仍因独立引用返回412 RESOURCE_IN_USE | `deployment/retained-ip-b70271b/release-result.json`、`private/vm-delete-retains-manual-vnics-proof.json` |
| VM3以等价正式 builder 请求创建（明确 reconstructed，非原VM2请求重放），关闭自动启动；停止态选 C，无当前在线地址且清空上个实例 SG；正常 Start 后同 C UID/.13/MAC，guest SSH/HTTP 实通 | Console task `vm3-create-reconstructed.json`；`private/vm3-create-stopped-reconstructed.json`、`vm3-stopped-config-clears-prior-sg.json`、`vm3-reuses-c-proof.json`、`vm3-c-guest-proof.json` |
| VM3 平台重启恢复自动 .16，guest MAC/IP与正式当前事实一致，实际访问自动 Notebook .15；C原 .13空闲保留 | `private/vm3-auto-restore-proof.json`、`vm3-auto-guest-proof.json` |
| Notebook 接回 C，原 UID/.13/MAC再次复用，VM3自动 guest .16↔Notebook C .13真实 SSH/HTTP；Notebook正常Delete completed而C available/free、原UID/IP保留 | `private/notebook-reuses-c-proof.json`、`notebook-c-vm3-auto-traffic-proof.json`、`notebook-delete-retains-c-proof.json` |
| B/C正常显式DELETE后产品Deleted、原CR/各自原VNicIP均消失、Subnet .12/.13分配记录空。仅两独立测试SG正常Deleted；瞬时worker lease忙按原路径正常重试，无改引用/lease、无force/finalizer。测试VM3正常Delete succeeded | `private/cleanup-b-sg-proof.json`、`cleanup-c-proof.json`、`vm3-deleted-proof.json` |
| 保护VM1原VMI/launcher、A、原auto VNIC、原allow SG、原Nic共六个UID全部未变；VM1仍自动 .9/目标A待重启。原两Notebook仍version1/Running/attached；父网络未删除 | `private/protected-scene-final.json` |

跨租户/可信caller、foreign controller身份边界、创建响应丢失、Reserve/Delete竞态、旧SG写入版本围栏和闭合恢复由对应真实PG/边界用例验证；未将其写成集群故障注入验收。无adapter的其他实例类型仍明确不可绑定，纯IPv6未验，不声明支持范围扩大。测试源码与新权限均已按本次关联行为验证，后续文档收口不重复全量检查。

## 自定义 IPv4/MAC 增量

2026-11-03 用户授权增加创建时可选地址；只约束同子网手动产品网卡 MAC 唯一，KCN 其他冲突如实报错。正式规则见[资源规格](../../../specs/vpc-subnet.md#手动-vnic-与实例选择)。源码 Resource `48badda` + `a854948` + `3e1ef97`（正式 SDK `8c3da37`），Gov `67e6c8e0`，Console `f2e517d` 均 SSH 推送。预览 `http://172.16.101.31:43928/vnics/` 已有输入与申请/实际分栏，生产 Console 未发布。

- Fedora run：`/home/chabking/tasks/vnic-addresses-20261103/`，Go1.26.10/Buf1.60.0/sqlc1.31.1；本地仅编辑、Git和传输。`go test ./internal/biz/network`、`TestKCVNicRequested*`、真实PG的请求持久化/规范化重放/并发MAC占用/自动MAC保护及原保留删除/可信service用例 PASS；只替换外部KCN控制器，真实业务和PG未替换。现场隐式IPv4及父版本并发修正只复验相关provider组；版本冲突回归先RED、修复后GREEN，原父UID被替换仍拒绝。未追加全量make verify、数据库备份或额外发布门禁。
- Gov正式SDK `v0.0.0-20261103134829-8c3da37dd6e0`（无replace），API生成及三个相关契约测试 PASS。Console三文件定向lint/format/typecheck与真实字段错误阻止提交 PASS。Gov已发布 `sha256:53bfd498aaf8db40bbc12f1f3cc55382c55997cd5d2e0ad6e8fb1de08a844329`。
- Network迁移29、vnicips-create最小权限已应用；Resource初版48badda镜像 `sha256:8c377e2484f4edc707835226e1a34c8ed13c097aa54f15c13a99eec5dfd65123` Ready1/generation86。页面唯一请求key `86ef853c-e333-42f3-aa6a-1a0385c6bb7c`，产品 `vnic_3b55300002864620b1abf0e601ef5ef8`，operation `2595cf2b-2d3e-4b9c-ad1e-bd3c37d3f4e1`；POST200/provisioning时实际地址为空，未冒充分配成功。
- KCN原VNic UID `985be981-822f-4f87-9642-06d6eb992130`、子IP UID `791bca0a-8ff7-4906-8df7-62fbaa354319`，实际IP/MAC为 `10.75.0.21 / 02:75:00:00:00:21`。现场CRD.spec只有dhcp/ipAddress/subnet/vNic，会裁掉ipVersion，导致原适配器 ownership-conflict；a854948接受省略为implicit IPv4，同时保留精确请求/实际、原parent UID/child UID及归属核验。a854948修复发布后，原申请/同operation自然available/free，申请值与实际值一致、child UID固定，未重提/重建；原JSON保留初始冲突和最终页面结果。
- 首个IP冲突样本 `vnic_f6a77c90ea2e476d86d6e31d5396ff37` 为failed/PROVIDER_REJECTED，KCN子IP实际Valid=False/AddressConflict，保留该失败历史并正常删除。子IP UID条件写回存在父RV变化被422误判永久拒绝的边界，3e1ef97修复为重新核实同原UID且RV已变化后重试；不以版本冲突误报地址永久无效。该边界RED→GREEN，未放松原UID核验。
- 最终Resource镜像 `sha256:34114dedc4c764e63d30063841e70b68572346fe91b92d89e4eeb4addb42d312`，generation88/observed88/Ready1。新鲜创建 `vnic_68c26deeaebd47878ab46b6c94dee776` 仅指定 `.22`、requested_mac为空，实际MAC自动为 `fa:00:17:5c:a5:42`，available；真实冲突 `vnic_3e9bd6cc1dce42399f0bdf4cdbd51515` 请求已占 `.21`，failed/ADDRESS_CONFLICT。二者均经正常产品DELETE到deleted，原 `.21` 成功样本仍available。手动MAC重复的实际409/ADDRESS_CONFLICT也PASS。
- 镜像上传500已确认Harbor `/storage` 10Gi满；用户明确批准仅该卷在线扩到20Gi，CSI/文件系统完成（20G/已用10G/可用10G），随后同镜像推送与发布成功，没有删除共享镜像。未重建Harbor或KCN。
- 原IP冲突测试先因父VPC基础聚合短降级返回412/PARENT_NOT_READY，未受理；自然恢复后沿同key正常受理。用户要求将此支线交另一个agent，已交API原回执、history时间线和源码入口；没有绕过父准入或修改该聚合。只读证据位于本地 `.work/vnic-addresses-20261103/base-observation-handoff.md`，具体底层观察失败原因未确定。
- 最小原始记录：run内`{biz-final,pg-test,provider-test,provider-implicit-ipv4,service-pg-test}.log`、`cluster-release.log`、`private/{custom-vnic-cr-final,mac-conflict-proof,final-address-checks,ip-conflict-cleanup}.json`，`provider-version-race-{red,green}.log`与`final-live.log`；Console run的`ui-custom-vnic-create.json`及原冲突截图。原A/VM1/原SG等排查场景没有写入或删除。
