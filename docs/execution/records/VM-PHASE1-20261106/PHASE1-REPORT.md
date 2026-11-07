# VM 一阶段实施与验收报告

2026-11-07 PR #66 两项局部修复及本地验证见[本次补充记录](#pr66-local-fixes-20261107)。下列原一阶段验收时间、源码、镜像和 LIVE 记录保持历史含义，不代表补充代码已部署。

一阶段结论：**PASS**。报告时间 2026-11-07T08:05:24.404233+08:00。仅 R01–R09；二阶段 **NOT_STARTED**，克隆专属字段为空。

R01–R09、P1A01–P1A10、P1L01–P1L04 及登记存量处置均满足本次范围。

机器可读结果见 [PHASE1-RESULT.json](PHASE1-RESULT.json)。本报告遵循用户提供的 goal-objective.md；任务包中的验收要求是技术依据，旧集群地址、远端构建规则和二阶段内容没有被当成执行授权。

## 代码、环境与制品

- 本机 WSL2 发行版 Ubuntu，仓库 `/home/ubuntu/workspace/ani-resource-service`，分支 `codex/vm-phase1-remediation-20261106`。
- 初始 main/HEAD、实际 base 与审核基线均为 `1498c223d902f24b2786ac017514087a476da091`，初始工作树干净。验收时交付为未提交差异，原 HEAD 只标识差异基底，不能代表完整测试版本。后续按用户要求整理为代码提交 991eb61a8acf245662a9cea70074835121415439，并提交 [PR #66](https://github.com/zhangzhe-ctrl/ani-resource-service/pull/66)；尚未合并。已逐文件核对提交的产品/测试源码与验收快照一致，发布记录见 [PR-HANDOFF.json](PR-HANDOFF.json)。
- 最终测试与构建的源码清单 SHA-256 为 `19cdb8e29a5a4757031aadfd478e1ee72c01260c4b9bb97fcde508c1d1dd863f`；标准镜像工作流的 dirty source snapshot 为 `sha256:b3f13212578885a391c6e9a6e1253d23f6b0642e4328f1dc323b546c0448001c`。见 [evidence/final-source-provenance.json](evidence/final-source-provenance.json)、[evidence/verify-06.manifest.json](evidence/verify-06.manifest.json)、[evidence/service-image-r4-build-result.json](evidence/service-image-r4-build-result.json)。
- Go 1.26.10、buf 1.60.0、sqlc 1.31.1；所有编辑、生成、编译、自动化、镜像构建及验收驱动在本机 WSL。未建立远端开发工作区。Gov 仓库仅按 `61cb1b6c74bd16d32f327aef42182566a87fe024f` 检视，没有修改。
- 既有 SSH config 在 WSL 和 Windows 均缺失，用户随后指定 `ssh kubercloud@10.10.1.66`；本次直接使用该入口，无 Host 别名。context `kubernetes-admin@ani-kcn-full`，API `https://lb.kubesphere.local:6443`；服务 namespace `ani-system`，fixture namespace `tenant-64c5c35a-f554-432c-b4bc-7b4f36f08420`。
- 实际 Kubernetes v1.35.8、KubeVirt v1.9.0、CDI v1.66.1；CSI driver 及实际组件镜像见 [evidence/cluster-version-scoped.json](evidence/cluster-version-scoped.json)。没有升级组件、修改共享 runtime/安全配置、StorageClass/reclaimPolicy，或直接写数据库。
- 最终服务镜像：`10.10.1.66:30003/ani-gpu-platform/ani-resource-service@sha256:96cf48e8b82400ee533b6944da836062759a549e0968de2e8449cd8c02d6dc78`。verifier：`10.10.1.66:30003/ani-gpu-platform/ani-vm-image-verify@sha256:c98767e78dfd6babe2527b7eac4068a4592d0e93b80514bffd343293ae2aea55`。基于原运行镜像构建，未夹带运行时升级；候选仍部署在集群中。
- 先部署兼容 reader，再切 verifier writer；随后 R3 补初始 PVC inventory，R4 修复轻量介质投影。各次实际镜像、部署时间和回执见 [evidence/deploy-reader.json](evidence/deploy-reader.json)、[evidence/deploy-writer.json](evidence/deploy-writer.json)、[evidence/deploy-final.json](evidence/deploy-final.json)、[evidence/deploy-final-r4.json](evidence/deploy-final-r4.json)。原构建结果中的 pushed=false 是构建结束时快照，后续 push/rollout 已单独保留回执。

## R01–R09 结果

| ID | 状态 | 实现与保留约束 | 检查 |
|---|---|---|---|
| R01 | PASS | 电源与根删除：Native.ObserveLifecycle/controlVM; Worker.power/remove; observeRuntimePods。tenant/VM/VMI UID、owner、Restart 新旧代次、非终态关联 Pod 和真正完成判断；介质投影复用 VM/VMI，不读存储 | P1A01/P1L01 |
| R02 | PASS | 原生 Snapshot 状态：ObserveSnapshot/EnsureSnapshot; SnapshotComplete; PreviewRestore/previewDigest。Required/Inventory 与 included/excluded 覆盖、原生错误、首次 Class 支持策略、停止态/版本/确认；预览摘要仅含业务身份及清理责任 | P1A02/P1L02 |
| R03 | PASS | Restore 具名输出：ObserveRestore; observeRestoreCleanup; RestoreOutputsRecorded; durable legacy checkpoint reuse。具名输出登记 actual PVC UID/容量；DiskID/volumeName/serial、旧盘和取消清理责任；Pending/WFFC；保留结果载体至持久交接 | P1A03/P1L02 |
| R04 | PASS | 标准状态观察：StateObservation informer/index/trackingWatch; Postgres.SaveObservation。初次同步、已知断连反馈、正常重连；有界队列、独立预算、lease/version/generation/closing 和只更新投影；无 audit/proof/续签/专用恢复 | P1A04/P1L01 |
| R05 | PASS | 业务删除边界：DeleteVolume; DeleteBackendState; DeleteSnapshot; ImageNative.DeleteImageObjects。精确业务 DV/PVC/Secret/Job/Snapshot/Restore 身份与真实消失；当前引用保护；PV/CSI/Artifacts 仅诊断；正常 Network/SG 关闭 | P1A05/P1L01/P1L02/P1L03 |
| R06 | PASS | 既有身份与默认配置解耦：Start/Restart service/native preflight; registered image/media/disk identity。新建支持范围仍有效；既有 UID/owner/mode/capacity 和真实功能合同保留；不让新默认 SC/Class/全局版本否定旧对象 | P1A06/P1L01/P1L03 |
| R07 | PASS | CDI 完成结算引用：ObserveImageClone; existing reference reconciliation/retirement。目标 DV 身份与来源请求匹配且 Succeeded 即 Completed；inventory 独立；Pending/Completed/Absent 和 PR65 退休 CloneAbsent 释放不变 | P1A07/P1L03 |
| R08 | PASS | 镜像内容校验：vm-image-verify main; VerificationResult reader/validation; restricted Job。O_RDONLY、最小长度/ISO 签名/CDI 处理后 SHA、结果归属与 READY 顺序；ReadOnlyProof 兼容但非必填；部署只读一次验收 | P1A08/P1L03 |
| R09 | PASS | CDI 运维接替：ObserveManagedSources/DeleteImageObjects; ImageCleanupObjects; scoped operator runbook。自有 source DV/PVC/Job/token 精确清理与 producer 先退出；旧 CDI/PV 引用仍可读；常规 worker 不接管 prime/scratch、不改 uploader finalizer；保留 PR65 | P1A09/P1L04 |

实现位置和具体测试名称列于 JSON 每项；当前正式合同为 [VM spec](../../../specs/vm-mvp.md) 与 [VM v1 contract](../../../contracts/vm-v1.md)，运维边界见 [CDI 接替入口](../../../runbooks/vm-image-cdi-handoff.md)。未增加 VM_CLONE、整机来源表、Clone worker、配额、第二套 action/owner、证明库或常驻 repair/orphan 服务。

## 自动化与现场检查

最终 `make verify`：2026-11-06T17:29:35.573354+00:00 至 2026-11-06T17:31:49.777848+00:00，exit 0。包含固定工具/生成物、递归边界、已有 fault 检查、全量单测、vet、build、module verification 和 diff 检查。

最终 `./scripts/integration -tags=instanceintegration -timeout=20m -v ./internal/data/vm`：2026-11-06T17:29:35.580571+00:00 至 2026-11-06T17:31:18.924714+00:00，exit 0，使用本机任务 PostgreSQL，结束后容器正常移除。观察器 race 定向检查也通过；该部分生产代码随后未变。证据为 [evidence/verify-06.json](evidence/verify-06.json)、[evidence/verify-06.log](evidence/verify-06.log)、[evidence/postgres-04.json](evidence/postgres-04.json)、[evidence/postgres-04.log](evidence/postgres-04.log)、[evidence/race-01.json](evidence/race-01.json)、[evidence/race-01.log](evidence/race-01.log)。

文档链接检查覆盖修改后的 Markdown：新增/变更链接全部通过；全文件扫描发现 45 条未改动的旧历史链接在基线即缺失，未扩大范围改写历史台账。完整诊断与基线归属见 [evidence/document-link-check.json](evidence/document-link-check.json)。

下表每项的完整开始/结束时间、实际命令/请求、退出码、测试名称、候选覆盖范围均在 [PHASE1-RESULT.json](PHASE1-RESULT.json) 的 checks 中，现场请求 ID、原受理 operation 和最终完成态在 [evidence/live-actions.jsonl](evidence/live-actions.jsonl) 中。

| ID | 实际层级 | 状态 | 实际断言/结果 |
|---|---|---|---|
| P1A01 | AUTOMATED | PASS | 电源/根删除不读存储；UID/owner、Restart 代次、提交与完成分离；关联 Pod 歧义不否定电源；轻量介质投影回归通过。 |
| P1A02 | AUTOMATED | PASS | 原生 Snapshot Ready/失败及必要卷覆盖；重放不重选 Class；运行态 Preview、停止态执行与版本/摘要；底层诊断不入摘要。 |
| P1A03 | AUTOMATED | PASS | Restore 具名输出、Pending 与未知输出区分、取消交接、持久 checkpoint、丢 ACK/进程退出恢复、旧 Plan 不改写。 |
| P1A04 | AUTOMATED | PASS | 标准 informer 同步/重连、健康缓存不因旧 proof 窗口失效；真实不可用、队列/并发栅栏/故障退避仍受保护。 |
| P1A05 | AUTOMATED | PASS | 精确业务对象生命周期、UID 替换与当前引用保护；PV/CSI 诊断不挡完成；业务清理与空探测保留。 |
| P1A06 | AUTOMATED | PASS | 新默认 SC/Class/配置不否定既有身份；新建能力门槛、真实功能、UID/容量和 Pending/WFFC 保留。 |
| P1A07 | AUTOMATED | PASS | 匹配 DV Succeeded 直接 Completed；不读目标 PVC 证明复制；Pending/Completed/Absent 与 PR65 退休释放不变。 |
| P1A08 | AUTOMATED | PASS | 新结果无 ReadOnlyProof 可通过；只读访问、内容校验、身份与摘要发布顺序保留；错误输入拒绝。 |
| P1A09 | AUTOMATED | PASS | 常规 worker 不读取/接管 CDI 派生对象、不修改 uploader finalizer；producer 顺序、自有业务对象和 PR65 保留。 |
| P1A10 | AUTOMATED | PASS | 生成物、边界、全量单测/vet/build/module、PostgreSQL 事务/租约/版本/关闭/重放及旧记录兼容均通过；未新增克隆协议或表。 |
| P1L01 | LIVE | PASS | Gov→Res Create/Start/Stop/Start/Restart/ForceStop；原生完成及 Restart VMI UID 更换；R4 再跑 Restart/Stop/Start/ForceStop/Start、Console RFB 101、稳定观察跨 60 秒；两台 Delete succeeded，根/盘清理及 Attachment consumer closed。未分配 SG，最终 SG IDs 为空；未宣称物理擦除。 |
| P1L02 | LIVE | PASS | 系统盘与数据盘小样本写入→Snapshot Ready Complete（含两业务卷，NoGuestAgent）→运行态 Preview v2→更改样本→停机后 Preview v8→确认 Restore v9；公开 complete 与具名输出、PVC UID/容量/serial 映射一致，checkpoint durable 且 CR 保留至登记之后；恢复及热插拔后均读回快照前样本。Snapshot/Restore/VM 正常删除。 |
| P1L03 | LIVE | PASS | 合法 qcow2 Block 与 ISO 经新 verifier READY，CDI 处理后 SHA 可用；无效 ISO FAILED/SOURCE_VALIDATION_FAILED，未 READY。正式 consumer 两个目标 DV Succeeded、来源引用 RELEASED/CLONE_COMPLETED，目标盘 inventory 完整；一次适用模板的 Block/Filesystem 写打开均拒绝且零数据写入。来源删除后 R4 consumer 重启与介质 effective 正常，全部正常清理。 |
| P1L04 | LIVE | PASS | 已按用户单 Pod 授权执行 UID/resourceVersion/完整 finalizer JSON Patch，只移除 foregroundDeletion；原生 GC 精确清理已知五个对象，平台镜像 DELETED；用户接收现有运维路径责任。 |

R2/R3 执行了正常 Linux VM、Snapshot/Restore 和输入校验的完整现场路径。R4 与 R3 的实际差异及针对性补测见 [evidence/final-source-provenance.json](evidence/final-source-provenance.json)：快照/恢复实现、镜像内容与 CDI 清理职责没有变化；轻量电源介质投影及其调用点已在 R4 重新做完整自动化和现场电源、介质、Console、持续观察、删除复核。没有把早期请求写成它们在 R4 上重新执行过。

### 必要现场身份与结果

| 对象 | 平台 ID / 原生身份 | 结果 |
|---|---|---|
| Linux VM | 58c38dbc-c4c7-43eb-9eb6-5cf6fa42ac76 / VM UID eae4252d-e42e-4d4e-8e22-902bedcb3ae9 | Delete operation 855827f6-6831-46da-9a78-4680e241cd66 succeeded；Network consumer closed |
| 镜像 consumer | ed05c03b-7e95-4622-b36d-0c5fcb88c977 / VM UID cd63e3f1-132a-4043-a919-64342f46b385 | R4 生命周期、介质投影、Console 通过；Delete operation 5e772557-376c-4c9b-a700-f3ce10be1932 succeeded；Network consumer closed |
| Snapshot | 42196e14-179f-5209-acd7-177ee0407d1d / UID a1d232fd-5fa9-4ffc-b9da-4509c18e665e | Ready/Complete，覆盖 system/data；NoGuestAgent，未新增 Agent 准入；正常删除 |
| Restore | vm-restore-c0b7be73-87ac-4e76-a5f7-308ab11ee4aa / UID 18854de8-0c4e-46c3-86d0-26a6b2d36a85 | public complete；Plan/mapping/cleanup checkpoint durable；CR 在登记完成后仍存在，随后随正常 Snapshot Delete 清理 |
| Block 镜像 | d6cbecd0-8248-4fd0-9586-10e8d1a43007 | READY → 正常 DELETED |
| ISO | 785372ad-19de-4124-b338-15b35f94eb7f | READY → 正常 DELETED |
| 无效 ISO | 516572a2-ed34-41f4-aadf-c18927521934 | FAILED / SOURCE_VALIDATION_FAILED，cleanup complete；随后正常 DELETED |
| failed uploader | 797b73ec-80d3-4dc1-b6e8-0134c762031c | 接替完成、DELETED |

恢复后两盘映射如下，具名 PVC 均与公开 Restore 输出及目标 VM 实际卷引用一致：

| DiskID = serial | volumeName | 恢复后 PVC UID | 观测容量 bytes |
|---|---|---|---|
| c0101713-cbda-591b-b922-ab466cff637b | disk-c0101713cbda591bb922ab466cff637b | af2fcf57-10ba-4d5f-9650-b00c570e5ef7 | 8589934592 |
| 0ecb19fe-45cc-5d58-864a-8b4cf7f045cd | disk-0ecb19fe45cc5d58864a8b4cf7f045cd | f32f409d-4a22-4131-a2ff-9ae5178a52df | 1000341504 |

系统盘样本 `phase1-before-restore` 和数据盘 128 bytes 小样本 `phase1-before-restore-data` 在快照后被改写，Restore 后通过既有 SSH 通道读回原值；数据盘按原 serial 在线卸载/挂载后再次读回同值。只读预览从运行态 v2 摘要变为停机后 v8 新摘要，执行用后者并明确确认。未做全盘 hash 或下层 CSI 来源证明。见 [evidence/live-before-cleanup-exact.json](evidence/live-before-cleanup-exact.json) 和 live-actions 的 guest-before / guest-changed / guest-read。

consumer 系统盘 DV `vd-5a1341bd-85da-590b-b861-d71820e9b1ce`（UID 61f7d290-119e-45d7-9af6-5c7606763476）与 ISO 副本 DV `vm-media-0a1468f6-49a4-56cb-ab93-23652f1d789b`（UID 4a644d58-c9fd-46fc-bd1c-99b0c1a23327）均实测 Succeeded，两个来源引用为 RELEASED/CLONE_COMPLETED。系统盘 PVC UID 1c401da7-9c89-4baf-bf23-db22fa61a028，台账容量 2147483648 bytes，原逻辑 DiskID/serial 保留。中途删除竞态由自动化覆盖，没有声称现场刻意卡住控制器。两台 fixture 均未配置 SG，最终 SG 列表为空；Attachment 的当前 VMI/launcher/revision 和关闭状态有实际记录。

Block/Filesystem 环境检查复用实际 verifier 的受限模板、uid107、无 SA token 与只读 PVC 声明，仅将本次环境探针替换进模板；O_RDONLY 正常，O_WRONLY/O_RDWR 分别返回 EPERM / EROFS，全程零数据写入，四个任务 Job 已删除。最初 BLKROGET 探针显示 Block flag=0；它与实际写打开 EPERM 并存，不能单凭该标志断言容器可写。完整原始诊断和后续正确权限检查均保留。此检查没有回塞到每镜像 Job 或业务合同。见 [evidence/readonly-environment.json](evidence/readonly-environment.json)、[evidence/readonly-environment-opens.json](evidence/readonly-environment-opens.json)、[evidence/environment-cleanup.json](evidence/environment-cleanup.json)。

## R09 实际接替与授权状态

已用受管上传入口提交可解析 header、转换时失败的受控 qcow2。CDI 上传服务实际退出码 1，原始消息包含 qemu-img 在 byte 0 读取 EIO；重启后经正式 Gov→Res Delete，source DV/PVC 和实际 prime PVC 均进入删除，uploader Pod 最终 Failed 且留下 foregroundDeletion。未手改组件 status、暂停控制器或修改共享配置来制造故障。

已核对并处置的对象为 `cdi-upload-prime-2f4f19ae-fbfc-4344-bd1d-4e87df2ecbeb`，UID `56cff51e-3e65-45aa-a849-9306d91039dd`，namespace 同 fixture。已准备一次性 JSON Patch：test UID、当前 resourceVersion、完整 finalizer 列表，再仅移除 foregroundDeletion；任何身份/终态/列表变化均停止。不会剥 PVC protection 或操作 PV/CSI。具体可审阅结果见 [evidence/cdi-takeover-plan.json](evidence/cdi-takeover-plan.json)；一次性脚本在 [evidence/drivers/cdi_takeover_once.py](evidence/drivers/cdi_takeover_once.py)，尚未授权时不执行。

现有 kubercloud 运维入口已按用户明确授权完成接管，逐个已知对象消失，平台镜像 DELETED。实际执行及收尾为 2026-11-07 北京时间 08:01:19 至 08:01:26。实际授权及接收责任见 evidence/cdi-takeover-authorization.json；动作/时间/回执及已知五个对象的精确 NotFound 结果见 evidence/cdi-takeover-executed.json。

Res 常规 worker 已移出 CDI prime/scratch 接管与 uploader finalizer 补偿，未搬到常驻 helper；PR65 退休 CloneAbsent 释放的单元/PostgreSQL 回归通过。初始已登记旧 CDI Cleanup 为 0，有只读查询依据，未通过删记录制造 0。

## 已登记存量

初始查询 2026-11-06T16:10:14.834418+00:00，最终定向重读 2026-11-06T17:38:46.524117+00:00。仅平台登记受影响记录；SQL 均 BEGIN READ ONLY，没有全集群 PVC/PV/CSI 扫描，也没有数据写入或修改无关用户 VM/image。

| 类别 | 实际数量 | 处置与结果 |
|---|---|---|
| 进行中 Restore（已有 Plan / 尚无 Plan） | 0 / 0 | 无需切换迁移 |
| 已登记 completed Restore carrier | 1 | 实际 CR 仍在，原 Plan、NativeCompleted、MappingCommitted、CleanupCompleted 和输出保留；丢失 carrier 数 0 |
| 旧 VM Cleanup | 32 VM rows，47 个 PV 诊断引用 | 9 Running / 1 Stopped / 22 Deleted；诊断保留可读，不再阻塞业务完成；82 个已登记业务 DV/PVC UID 字段保留，不等于声明物理对象仍存在 |
| 活跃 Snapshot 下层诊断 | 2 records；2 Content、5 VS、5 VSC | 原字段保留，不再是 Ready/删除门槛 |
| 旧受影响镜像 | 1 IMPORTING；0 UNAVAILABLE | 无关既有上传不处置；ReadOnlyProof 为瞬态结果字段，兼容读取由自动化验证，SHA 与引用语义保留 |
| 旧 source pin | 5 RELEASED | 保留，无重新 pin |
| 旧内部 CDI Cleanup | 0 | 查询涵盖有 Cleanup 或清理中的登记镜像；没有待移交的旧条目 |

完整 SQL、范围、时间、数量和精确已登记身份见 [evidence/existing-records-initial.json](evidence/existing-records-initial.json)、[evidence/existing-records-followup.json](evidence/existing-records-followup.json)、[evidence/existing-records-final.json](evidence/existing-records-final.json)。本次合成旧记录回归与上述真实现场数量分开。

## 原始失败、修复和证据边界

- 最初 verify 的日志写进被检查源码范围，造成 source snapshot 漂移；移至 .work 后重跑。基线自身有 gofmt 空行问题，已修正。
- 初次 PostgreSQL 回归的 JSONB 空白比较与 Network publication 步数假设，在独立基线 worktree 上也能复现。改为规范 JSON 比较，并驱动真实 Network 发布步骤，未改生产 Network 合同；保留 [evidence/baseline-postgres.log](evidence/baseline-postgres.log) 和历史检查。
- Console 初次缺正确 Origin / subprotocol、ForceStop 缺明确确认、若干删除/Console 驱动缺必填 ID/version，均是被拒绝的驱动请求，修正后按正式合同验证；未将 HTTP 202 等同于完成。上传尚未 Ready 的 503、恢复启动后 SSH 尚未就绪的拒绝均保留，随后实测成功。
- R3 在 CDI 复制/重启成功后介质 pendingRestart 投影未刷新，是实际回归。R4 将观察纯投影提取到已读 VM/VMI 上，新增“无存储读取仍刷新、投影错误不挡电源”测试；最终完整检查和 R4 LIVE 已验证修复，旧失败证据不覆盖删除。
- 全部必要原始检查历史封装在 [evidence/checks-history.tar.gz](evidence/checks-history.tar.gz)；最终常用结果另行展开。公开驱动在 evidence/drivers，登录材料、upload token、console URL、registry 凭据和 Secret 备份未交付至证据目录；已做已知密码/token/auth 值泄漏检查。[evidence/SHA256SUMS](evidence/SHA256SUMS) 只用于交付文件完整性。

本次登录逻辑会话已正常登出并确认 session_revoked；临时登录/curl/token/Secret 备份已清理，两个任务 SSH 转发已关闭，原始 SSH 密钥未动。见 [会话清理回执](evidence/local-session-cleanup.json)。最终源码、14 项检查、9 项整改、实际部署、已知对象新一轮精确读取及平台终态审核见 [最终交付核对](evidence/final-delivery-check.json)。

提交 PR 前重新执行 make verify；门禁发现独立证据驱动的格式及归档方式问题，规范格式并以源码文本归档，避免其参与服务 Go module，最终门禁通过。原始失败和重跑记录保留于 evidence/pr-verify-01.*、evidence/pr-verify-02.*、evidence/pr-verify-03.*，服务与测试源码没有新增差异。

## 交接与停止点

最终一阶段 **PASS**。所有本阶段必需项完成，交付已验证 dirty snapshot 及可审阅差异；后续集成应核对该快照，而非仅凭基底 HEAD。

报告 WSL 路径：`/home/ubuntu/workspace/ani-resource-service/docs/execution/records/VM-PHASE1-20261106/PHASE1-REPORT.md`。JSON：`/home/ubuntu/workspace/ani-resource-service/docs/execution/records/VM-PHASE1-20261106/PHASE1-RESULT.json`。证据目录：`/home/ubuntu/workspace/ani-resource-service/docs/execution/records/VM-PHASE1-20261106/evidence`。

二阶段始终 **NOT_STARTED**。未实现 VM_CLONE、未创建整机克隆 source/target/Clone 或开展二阶段测试。本次停止于一阶段交付；任何后续克隆任务均需另行安排。


<a id="pr66-local-fixes-20261107"></a>

## 2026-11-07：PR #66 两项局部修复

审核基准与执行前再次核对的 PR 最新提交均为 `241bacd8cd001f4d0075707311c225a36876656b`；工作分支继续使用 `codex/vm-phase1-remediation-20261106`，初始工作区干净。两项问题在该提交仍存在，未重复修复已完成事项。本次只修复以下两点，不改变 R01–R09 原 LIVE 证据，不进入二阶段。 修复代码提交为 `4cc64e028957d716976d85e62404f64ddd401592`，之后仅补充文档证据。

1. **具名恢复输出正常消失。** `recordRestoreVolumes` 对同 kind/namespace/name 的记录合并身份：观察 UID 为空时保留已知 UID，退休磁盘/介质保留原记录和 cleanup，backend 保留原身份及诊断/容量并标记不再存在；两个非空 UID 不同仍返回 `ErrOwnership`。当前磁盘/介质也按同一规则识别，避免缺失观察造成重复退休记录。保留 namespace、kind、VM owner 校验，没有吞掉身份冲突或清空旧 UID。适配层已正确把实际 NotFound 返回为具名空 UID 引用，因此本次不修改其既有行为。
2. **异步磁盘准备最终刷新。** 复用现有 `Observer` / `StateObservation` / `vm.observation_work`，只对已登记且尚未完成准备的盘定向读取 DV/PVC，复用 `observeDisk` 的 UID、owner、容量检查。运行事实和磁盘读取分别限时，磁盘错误不作为运行观察错误落库；成功的准备事实在现有租约、generation/version/closing 及当前物理映射校验下，原子更新 current configuration 与 `vm.disks`。只合并 UID、Preparation 和观察容量，保留当前挂载、命名、cleanup 等意图字段。已完成准备的盘不持续审计。Start/Stop/Restart 仍使用 `ObserveLifecycle`，没有改回全量 `Observe`，没有新增 controller、常驻轮询体系或存储前置门槛。

### 本次实际执行

本机 Windows 宿主、WSL2 Ubuntu，主机 `WIN-HUFRHKJT097`，目录 `/home/ubuntu/workspace/ani-resource-service`，Go 1.26.10、buf 1.60.0、sqlc 1.31.1。按本轮用户明确要求本地执行，不沿用历史远端构建约定。最终受测 Go 文件清单 SHA-256 为 `98d3914bdb5d69f48af25335ea31612f98eeab701b37c3ca0e21fa1627e2e718`；[源文件清单](evidence/pr66-followup-20261107/source-manifest.json)与审核基准组合标识最终受测源码。最终检查后逐文件核对无源码变化。

- `GOMAXPROCS=4 GOFLAGS=-p=4 ./scripts/integration -tags=instanceintegration -timeout=20m -v ./internal/data/vm`：**PASS**，111 个顶层测试通过，95.232s。[完整日志](evidence/pr66-followup-20261107/vm-pg-full.log)。另有 12 项 Image 专用数据库测试因未设置 `VM_IMAGE_TEST_ADMIN_DSN` 跳过，不属于本轮两项修复；进程子测试入口单独运行时跳过，实际父进程退出恢复测试通过，不能把 13 个 skip 计为通过。
- `GOWORK=off GOMAXPROCS=4 GOFLAGS=-p=4 GOMEMLIMIT=4GiB make verify`：**PASS**，2026-11-07 14:02:20–14:04:40 +08:00；固定工具生成一致性、边界/故障编译、tidy、全包普通测试、vet、build、module verify、diff check 均完成。[日志](evidence/pr66-followup-20261107/make-verify.log)、[命令及时间回执](evidence/pr66-followup-20261107/make-verify.json)。
- 相同四个环境变量下执行 `./scripts/integration -race -tags=instanceintegration -timeout=10m -run 'TestStoppedCreateInventoryRefreshAfterIndependentStart|TestObservationPreparationFencesRemap|TestRestoreMissingRegisteredOutputs' -v ./internal/data/vm`：**PASS**，三项新增顶层集成回归均通过；2026-11-07 14:03:49–14:04:32 +08:00。[日志](evidence/pr66-followup-20261107/vm-pg-race-focused.log)、[命令及时间回执](evidence/pr66-followup-20261107/vm-pg-race-focused.json)。

新增回归覆盖退休磁盘、介质、backend 以及当前盘/介质的“已知 UID → 正常缺失”、同名不同非空 UID 拒绝及原归属边界；真实 Native Restore 输出读取、PG 台账和 Worker 快照删除共同证明旧输出消失不阻断删除。磁盘回归使用实现了 `ObserveLifecycle` 的真实 Native、受控 KubeVirt Start HTTP 接口和实际 PostgreSQL：停机创建时 PVC 缺失/Pending，独立 Start 在存储 GET 失败时仍完成；后台运行投影保持 Running，存储恢复/PVC Bound 后自动补齐 UID、Preparation、容量及数据库镜像，GetVM/ListDisks 直接显示新台账且不额外读取存储。另验证旧观察不能覆盖同版本新物理映射，也不覆盖当前挂载/命名/cleanup。

### 基线、原始失败和边界

在独立 detached 工作树 `ani-resource-service-pr66-review-base` 的原审核提交，只加入相同三份回归测试：
- `GOMAXPROCS=4 go test -p=4 ./internal/biz/vm -run '^TestRestoreOutputMerge' -count=1` **EXPECTED_FAIL**，复现缺失 UID 合并误报身份冲突；[原始日志](evidence/pr66-followup-20261107/baseline-restore-unit.log)。
- 同上定点 PG 命令（不带 race，timeout=5m）**EXPECTED_FAIL**：旧 observer 没有执行磁盘刷新读取/持久化，快照删除以 `VM_OBJECT_OWNERSHIP_CONFLICT` 失败；[原始日志](evidence/pr66-followup-20261107/baseline-targeted-pg.log)。这与最新实现的通过结果分开记录。

早期新测试先因未用 import 编译失败，随后因 Native Start 夹具缺少现有 tablet/resourceVersion、删除动作断言包含夹具初始化动作而失败；已修正测试夹具及动作范围，未放宽产品校验。中间失败、定点重跑和最终结果均保存在[结构化记录](evidence/pr66-followup-20261107/results.json)与同目录日志；完整源快照未捕获的中间尝试不冒充最终源码证据。[本次校验和](evidence/pr66-followup-20261107/SHA256SUMS)独立于原验收校验和。

**本轮没有连接或使用集群**，没有发布镜像/部署，没有重跑 LIVE 恢复矩阵、R09 finalizer 接替或二阶段。原 2026-11-06～07 的 R4 LIVE 证据仍只对应原代码提交 `991eb61a8acf245662a9cea70074835121415439`；不能据此声明本次修复已在真实集群验收。各次集成测试均使用脚本独占的固定 PostgreSQL 镜像并在退出时删除自己的临时容器，既有无关容器保留；本次基线临时工作树已清理，原工作区及既有 audit 工作树未重置或覆盖。

按用户后续“ci暂不处理，解决其他问题”，本轮没有修复或继续追踪 GitHub CI，没有关闭/放宽门禁，也不宣称新提交 CI 通过；最新 CI 状态未复查，失败根因未继续归因。PR #66 原分支更新，暂不合并；停止于这两项修复和本地验证，不扩展其他模块。
