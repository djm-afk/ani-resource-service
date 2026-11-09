# Res 文档清理修复记录（2026-11-09）

## 范围与输入

按[全域复扫清单](res-doc-review-20261109-global.md)修复文档。用户澄清本次为文档任务，因此本轮只改 Markdown 与文档校验详单；没有修改业务代码、迁移、配置、RBAC 或测试，没有运行构建、业务测试、PostgreSQL 或部署。文档修复验收时尚未提交或推送；随后用户授权创建 PR 和缺陷 issue，提交准备见本文末节。

源码基线为 813885cb8e6dfd11b4627d9a41ca8e3cd47c6afa，分支 codex/vm-phase1-remediation-20261106。实际操作由 Windows PowerShell 访问本机 WSL Ubuntu 源码目录；使用 WSL git、Python 和 rg 进行文件检查，不使用远程构建或集群入口。个人环境连接信息不写入项目。

[最新版原则](../../specs/res-responsibility-and-simplification.md)保持与用户更新附件逐字节一致，SHA256 为 c24043e20378a1ed214c7a012dd3e2d1e81cbe7b8239a028f6a5e6f6325bbc4a。用户已有 VM 详情延迟排查记录及其状态段保留，未改写历史 pass/fail/not_verified 结论。

## 清单处理结果

| 原项 | 文档修复 |
| --- | --- |
| D01–D03 | 更新 README、运行装配和验证说明；覆盖原审阅基线的七个模块，撤回旧审计/证明时效/GeneratedReady 运行要求；业务实现检查与文档静态检查分开。 |
| D04–D07 | VM/Instance 的 Network 消费收敛到当前管理快照与地址；保留 owner 自己的退出、未知写、版本、引用和输出责任；统一标准租户 Namespace，区分 Sandbox 开通者与只读消费者。 |
| D08–D12 | 调整 VPC/Subnet、SNAT、Underlay、LB、安全组及相关手册的目标边界，保留本服务声明、真实 applied 字段、原 UID/并发、用户配置及产品占用。初次清理仍遗漏终态恢复、LB 健康字段和完成示例中的冲突，已在下方 F02/F04 修订中补正；不据文档调整宣称全域代码整改完成。 |
| D13–D14 | 修正 VM 快照删除计划和验收矩阵的 Content/CSI 等待要求；撤回“全部待实现”的过期标题。未来整机 Clone 不写成已完成，实际状态仍以唯一执行状态为准。 |
| D15–D17 | ADR 身份延期标为历史替代，修正 ADR-0007 元数据与索引；AGENTS、执行约定和旧 Goal 明确当前任务与历史授权边界。 |
| D18–D19 | 修正 Instance/Sandbox 目标合同；代码整改仍待处理，见下节，不将文档修改计为实现完成。 |
| D20–D21 | 补齐[直接 PVC 管理合同](../../specs/storage-volumes.md)和[对象存储合同](../../specs/object-storage.md)，说明直接对象、身份、正常查询、并发/错误、输出交接及删除边界。保留合法 PV 详情读取、S3 ETag、分页和短期 URL。 |
| D22–D23 | Instance 已接入统一 Secret writer，独立 mTLS 入口只允许 Resolve + Ensure；初次说明遗漏 Sandbox 仍进程内取材并自行写 Secret 的现状，已在 F06 更正，不能称全部已替代。Compute 摘要包含 SANDBOX 用途；本次未实施 Sandbox writer 迁移。 |
| D24 | 提取[Sandbox 正式合同](../../specs/sandbox.md)；[原计划](../../plans/sandbox-runtime-design-20261025.md)保留阶段顺序、未来能力和历史来源，增加替代指引。 |
| D25 | 同步 Intranet/Egress、LB、公网操作说明，区分产品动作、显式底层诊断和历史现场结果；删除失效 Go 包测试命令与旧运行准入。 |

唯一文档索引为 [START-HERE](../../START-HERE.md)，当前进度只保存在[执行状态](../status.md)。初审和全域复扫记录、各自链接 JSON 保留原时点结果；本文记录本次文档处理，不是第二份持续执行账本。

## 尚未实施的代码整改

- **D18**：Instance/Sandbox 在原 PVC 已退出后仍等待底层 PV 消失，部分 adapter 还扫描 PV 补找绑定。后续需同步收敛 worker/adapter、cleanup receipt 消费、仅服务该证明用途的配置/权限与测试；保留自己的 Pod/Notebook/StatefulSet、原 PVC、未知写、Network、borrowed 引用及原账 outbox 责任。
- **D19**：Sandbox 的运行投影仍被 PV 存在、回收策略和辅助读取结果合取。后续需按动作分开原 Pod/PVC 事实与辅助诊断，调整错误投影及相关测试。原 PVC 身份/缺失/真实未就绪仍须处理。

以上两项只更新了目标文档；没有改实现或部署 RBAC。现有部署手册已提示不能仅据新文档删除权限。详细代码定位保留在原全域复扫清单。

## 链接与证据位置处理

原全域复扫报告为 249 份 Markdown、3437 个本地链接、179 个路径/锚点问题。其中 12 项过期引用已替换为有效目标或随旧合同删除；167 项当前不可用的历史源码、日志、仓外记录保留原位置并标为 not_verified。修复后不存在可点击的本地失效路径或锚点。此处理不恢复归档、不补造原始日志，也不表示相关业务结果重新验证通过。

Source22 原 verify.log 未归档，状态段明确说明，现存 safe.json 收据与 rc 链接保留；Underlay 跨仓回执保留原定位并标 not_verified。其他引用维护的逐项记录和最终检查结果见[链接处理与静态校验详单](res-doc-fix-20261109-links.json)。

## 实际检查

| 检查 | 实际命令/范围 | 结果 |
| --- | --- | --- |
| 本地路径与 Markdown 锚点 | WSL Ubuntu：python3 .work/res-doc-fix-20261109/check_docs.py；扫描 git 已跟踪及未忽略的 254 份 Markdown、3440 个本地链接，跳过代码围栏，检查标题锚点及目标存在 | pass，0 个失效链接 |
| 修改文档格式 | 同一轻量脚本检查 63 份变更 Markdown 的 UTF-8、新增行尾空白、代码围栏及文件末尾；最新附件保持原始字节 | pass，无新增问题；首次发现 Sandbox 末尾空行后已修正 |
| 差异空白 | WSL git diff --check | pass，exit 0 |
| 改动范围 | WSL git diff --name-only HEAD 与 git ls-files --others --exclude-standard | pass；仅 Markdown 和文档链接 JSON，无业务代码/迁移/配置/RBAC 变化 |
| 内容保护 | SHA256 及状态段原文比对；Windows Get-FileHash 复核最新附件 | pass，最新原则与附件同 hash，原 VM 延迟记录及状态段未变 |

输入源码 HEAD 与上节一致；链接详单保留原审阅问题及逐项处置，不覆盖初审/复扫的原始 JSON。检查脚本和临时输出位于任务私有 .work/res-doc-fix-20261109，不作为正式规格或第二份执行账本。

业务测试、构建、数据库及现场验证均为 not_run，本轮文档任务不需要这些验证。

## PR 提交准备（2026-11-09）

用户授权将文档修复提交 PR，并将 D18、D19 分别登记 GitHub issue。独立分支 codex/res-docs-cleanup-20261109 从最新 main 511b11194dfc1e49a6291a21fa19b0a2c43af3df 创建，只移入本任务文档差异；保留 main 已有 ClusterOps 规范、调用登记、Sandbox 部署说明及完整执行状态，并在 README、运行契约和领域索引补齐入口。

原 worktree 的 VM 延迟排查文件、状态段以及全部未提交改动保留原样，不纳入本 PR。前述 254/3440 是原审阅 worktree 的检查结果；PR 分支按新基线单独复核，实际结果另记于本节与链接详单的 pr_preparation，不覆盖原审阅输入或结果。

- [D18 #68：Instance/Sandbox 删除被底层 PV 回收等待和补扫阻塞](https://github.com/zhangzhe-ctrl/ani-resource-service/issues/68)
- [D19 #69：Sandbox 运行状态被 PV 辅助诊断错误降为 Unknown](https://github.com/zhangzhe-ctrl/ani-resource-service/issues/69)

两个 issue 均包含固定源码位置、可触发条件、影响、修复范围和验收项；本 PR 只关联问题，不关闭问题或宣称实现已修复。

PR 分支最终静态复核：256 份 Markdown、3463 个本地链接，0 个失效目标；62 份修改 Markdown 格式与 git diff --check HEAD 均 pass。相对最新 main 共 65 个文档/详单路径变化，无业务代码、迁移、运行配置或 RBAC 变化。最新原则 SHA256 与附件一致，main 的 ClusterOps 状态段逐字保留，原 worktree 的 66 个既有改动文件 hash、HEAD 与 index 均未变。

实际检查命令为本机 WSL Ubuntu 的 python3 .work/res-doc-pr-20261109/check_docs.py 和 git diff --check HEAD；没有运行业务测试、构建、PostgreSQL 或部署。GitHub 原 SSH 拉取本次连接关闭，main 通过公共 HTTPS 获取，发布使用已连接的 GitHub 插件，没有更改 SSH 配置或将凭据复制到项目。

<a id="pr70-review-f01-f06-20261109"></a>

## PR #70 审核修订 F01–F06（2026-11-09）

开始时核对 [PR #70](https://github.com/zhangzhe-ctrl/ani-resource-service/pull/70) 为 open、未合并，当前 head 仍为 221c1f1a32d2246184b15aaf32a871ea917a9a45，base 为 511b11194dfc1e49a6291a21fa19b0a2c43af3df，分支 codex/res-docs-cleanup-20261109；未发生 head 前移，六项冲突均仍存在。使用该 head 的领域合同及实际源码逐项核对，在已有干净 PR worktree 追加文档修订，没有覆盖原仓库的未提交改动。

| 审核项 | 修改位置 | 修正结果与源码依据 |
| --- | --- | --- |
| F01 | [Instance §8.3/§13（已核对 §7.2）](../../specs/instance-mvp.md#13-最小验收清单) | 验收等待自身计算对象、原 PVC、Network 管理关系、borrowed 引用及未解决外部写；匹配 DELETE/版本/租约后保存 owner 完成事实和原 GPU outbox。PV/CSI/物理擦除及可选诊断不作为门槛，Gov 故障不回退已完成清理。核对 OwnerCleanupReady、worker 与 repository 的 sealQuotaRelease 消费；#68 的旧 PV 等待实现仍未修复。 |
| F02-A | [VPC/Subnet §5.3](../../specs/vpc-subnet.md#53-恢复与并发) | 只恢复本集群已持久的未完成管理工作；终态任务退休，历史身份/回执保留；未知提交继续原任务，不伪造完成、不恢复墓碑审计。依据网络 v3、WakeManagedTasks 及终态排程条件。 |
| F02-B | [LB §3.1/§3.2](../../specs/lb-deferred-activation.md#32-状态与-operation) | 旧字段编号保留，data_plane_state=UNKNOWN、data_plane_observed_at 为空；配置回执不是流量健康。同步去掉版本示例中生成资源/实际地址清理的额外完成条件，显式流量诊断独立记录。 |
| F03 | [VM V10](../../verification/vm-mvp-acceptance.md) | 同一当前消费者无新事件/读错不按年龄撤址，成功读取按字段合同更新；stopped/closing/closed 或消费者更换撤回旧输出。保留 Stop 的 Attachment 预留、最终 Release、真实 guest 网络/跨节点/隔离验收及 VM 来源断连 Unknown 语义。 |
| F04 | [LB 操作第4步](../../../deployments/load-balancer/README.md#产品操作顺序)、[§5.1/§5.2](../../specs/lb-deferred-activation.md#51-清空部分监听器) | 区分减少 Backend、仅清空某监听器和清空最后运行投影三种范围。按 load_balancer_worker 的 ProjectLoadBalancerListeners、retired component 与 draining 分支核对，保留其他监听器及必要共享声明，不增加生成物或物理地址等待。 |
| F05 | [Storage 删除结果](../../specs/storage-volumes.md#删除引用与恢复) | GONE 仅表示明确 absent 的成功结果；前台或恢复遇同名新 UID 返回 ErrIdentityChanged，经 service 映射为 AlreadyExists / VOLUME_IDENTITY_CHANGED。旧 ticket 可结束，但公开调用不返回成功 State，不碰替换对象。核对 deleteVolumeGuarded / advanceDelete / completeDelete 和 rpcError / DeleteVolume。 |
| F06 | [Image 方法 §7](../../specs/image-methods.md#7-工作负载创建方)、[规格 §8](../../specs/image-mvp.md#8-工作负载使用)、[API §4](../../specs/image-api.md#4-内部-imageruntimeservice) | 区分 Instance 的 Ensure writer、独立 mTLS 两方法限制、Sandbox 进程内 GetTenantPullMaterial 及自行写 ani-sandbox-pull-&lt;tenant&gt; Secret。核对 sandbox.go、Sandbox image/provider、Instance adapter 和 image_runtime 拦截器；统一 writer 尚未覆盖 Sandbox，本轮没有代码迁移。 |

此前处理表中“清除”“已替代”等概括已按上述范围收窄。原始审阅 JSON、历史执行结果及既有检查计数保留原时点含义，本轮不另建审计报告或持续账本。#68、#69 继续为未完成的代码事项；Sandbox Secret writer 接线纠正仅是现状说明。

本轮实际静态检查仅覆盖上述修改文件及直接引用，不重新扫描全仓：

| 检查 | 实际命令/范围 | 结果 |
| --- | --- | --- |
| 范围、格式、链接与锚点 | python3 .work/pr70-review-fixes-20261109/check_docs.py；11 份修改 Markdown，1170 个相对链接；检查新增行尾空白、代码围栏、表格列数及文件末尾 | pass，0 个格式问题、0 个失效路径/锚点；无范围外变更 |
| 空白差异 | git diff --check；另由轻量脚本执行 git diff --check 221c1f1a32d2246184b15aaf32a871ea917a9a45 | pass，exit 0 |
| 原冲突句检索 | rg -n -F 的八个精确原句，范围为本次九份合同/手册/验收文档，排除历史审阅快照 | 无匹配，exit 1 表示目标句均已移除；仅用于定位，不代替语义复核 |
| 六项语义与源码对照 | 按上表核对正文、手册、验收表及固定 head 的对应 worker/adapter/service；区分目标与现存代码 | F01–F06 均已处理；#68/#69 与 Sandbox writer 迁移仍未完成 |
| 内容保护 | 仅允许本轮 11 个 Markdown 路径；原仓库文件 hash/HEAD/index 与开始快照比对；原则 SHA256 核对 | pass，原有 66 个改动文件与原工作区 HEAD/index 未变，原则原文未变 |

没有运行 Go 测试、make verify、数据库、镜像、SSH、Kubernetes 或真实业务验收。开始时读取的旧 head 自动 CI run 42557612121 为 failure（source/image 通过，network 三分片、supply-chain 和汇总 verify 失败）；这是该 head 已观察到的状态，不据此推断新 head 结果或把未定位失败称为历史问题。追加提交后的自动 CI 沿原配置运行，最终状态在原 PR 和交付中按实际 head 报告。
