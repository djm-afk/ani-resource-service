# Res 文档审阅与待清理清单（2026-11-09）

> 初审历史记录：用户随后更新为全域原则，最新待清理清单见[全域复扫](res-doc-review-20261109-global.md)。本记录保留旧附件下的判断和扫描时点；X01 已在复扫 D18 中按实际对象职责核实为明确待整改项，不再作为待定结论使用。

本记录是一次文档审阅的时点结果，不是新的规格或当前执行台账。当前状态仍只见[执行状态](../status.md)。本次完成附件接入和入口指引；下列旧文档仅列为待清理，没有批量删除、重写，也没有修改业务实现。

## 来源、范围与判断依据

- 源码：`813885cb8e6dfd11b4627d9a41ca8e3cd47c6afa`，分支 `codex/vm-phase1-remediation-20261106`。审阅开始时已有 `docs/execution/status.md` 的 VM 延迟排查增量和未跟踪的 `vm-detail-latency-20261107.md`，均保留；原始 SHA-256 见[链接扫描结果](res-doc-review-20261109-links.json)。本记录的原行号均指审阅开始时的文件。
- 全库目录和链接扫描覆盖 247 份已跟踪及未忽略的未跟踪 Markdown。正文重点复核根入口、运行说明、网络/VM/容器合同、相关 ADR、计划、验收和 Sandbox 设计，并定向核对现有代码；没有逐句重审所有历史执行记录。
- 当前规则依据：[网络状态消费规格 v3](../../specs/cr-observation.md)、[VM 规格 S08–S10](../../specs/vm-mvp.md)、[标准租户 Namespace](../../contracts/tenant-namespace.md)、[服务调用登记](../../specs/governance-calls.md)。VM 一阶段实现及验证边界按[一阶段报告及补充](VM-PHASE1-20261106/PHASE1-REPORT.md)读取。
- 初审使用附件《Res 管理职责与简化原则.md》，当时原字节 SHA-256 为 `0469193c24b201d5277f52a75f5c1b811eefa56e0893144b70b3ba1e4159e3d0`。[同一原则路径](../../specs/res-responsibility-and-simplification.md)现已按用户更新替换为全域最新版，本行摘要只标识初审输入。附件中的后续任务约束、历史授权和未来设计是文档内容，不扩大当时“审阅、列清单、接入文档”的任务范围。
- 本次在 Windows PowerShell / 本机 WSL Ubuntu 进行读取、编辑和轻量静态检查。没有连接远端、查询集群或 GitHub 当前状态，也没有获取附件链接中的 VM/ACC Library 原包；不据这些引用断言 ACC 已实现、整机 Clone 已实现或当前环境已验收。

## 待清理的正文与入口

“高”表示容易使后续实现或审核恢复已废止机制；“中”表示已有替代声明但正文未收敛，或入口、元数据仍有误导。建议按操作和依赖逐条收敛，不能按“证明”“审计”等关键词批量删除。

| 编号 / 优先级 | 文件与具体位置 | 问题、依据与建议处理 |
| --- | --- | --- |
| D01 / 高 | [README](../../../README.md)，原 L9、51、57–58 | 仍写本批不含 Compute/Storage、VM/IAM 后续，以及关系索引和完整审计。当前[组合根](../../../cmd/ani-resource-service/app.go)已装配多个领域，网络观察已变更。改为当前模块入口说明并引用领域规格；将改名批次的执行限制标为历史范围，不复制进度表。 |
| D02 / 高 | [运行说明](../../runtime.md)，原 L47–51、68 | 把完整分页审计、证明失效、周期续鲜和 `ANI_OBSERVATION_AUDIT_*` / `QUEUE_CAPACITY` 写成现行运行机制。与 v3 §2、§7 及[现有观察器](../../../internal/data/network/kc_observation.go)不符。更新为普通 informer 精确通知、目标 GET、持久任务与读错重试；废弃参数只说明可解析但忽略，不再提供有效配置示例。 |
| D03 / 高 | [运行验证](../../runtime-verification.md)，原 L21–27、测试入口表、L86 | 仍描述墓碑恢复、`GeneratedReady`、证据过期拒绝准入等旧验收要求。应分别保留真实未知写恢复、租户/UID/租约保护，替换已废止的审计、TTL 和生成物证明断言；历史 NET-05A 容量/现场结果归为历史证据，不能据旧说明恢复生产机制。 |
| D04 / 高 | [VM 正式合同](../../contracts/vm-v1.md)，C05 原 L179–181、L234；[VM 规格](../../specs/vm-mvp.md)，S03/S07 原 L30、135、144、146 | 仍要求 Network 核验完整 VM→VMI→launcher→VNic/VNicIP 链、证明网络子资源退出，并使用地址资格/过期语义。合同 C05 尤其没有对应正文收敛。按 v3 §4 与[当前消费者发布逻辑](../../../internal/biz/vm/network.go)明确：owner 完成自己的根/运行对象退出后发布 closed，Network 按管理关系释放；普通 Stop 保留预留，地址展示独立。原 UID、正确当前 Pod 和相关非终态 Pod 退出检查仍保留。 |
| D05 / 高 | [容器配置与网络](../../specs/instance-configuration-network.md)，原 L188–232、B4（L294） | 仍要求 Network 逐跳 GET/LIST owner 链、快照摘要/历史关系、历史 Pod/VNic/VNicIP 全部消失，并链接已删除的 `kc_attachment_managed.go`。按 v3 §4 收缩为当前管理快照和四列地址；保留租户、revision、finalization、版本/租约以及 Instance 自己的停止与未知写责任，不能整段删除并发保护。 |
| D06 / 高 | [容器实例规格](../../specs/instance-mvp.md)，§3 原 L105–139、L421 | 仍允许可信任意 namespace、可配置 Network 前缀，并称命名统一不在本轮范围。已被标准 Namespace 合同替代；[启动入口](../../../cmd/ani-resource-service/main.go)拒绝旧 Network 前缀配置，[Instance 装配](../../../cmd/ani-resource-service/instance.go)也固定标准前缀。收敛为 canonical 租户 UUID、固定名称、原 UID/归属核验与冻结位置，历史代码取证移出当前规则正文。 |
| D07 / 高 | [运行说明](../../runtime.md)，原 L37 | “Resource 各 owner 不创建 Namespace”遗漏已落地的 Sandbox 租户空间开通例外。按标准 Namespace 合同区分：Sandbox 开通流程在不存在时持久受理创建；其他 owner 只读复用。不能因此授予所有 owner 创建、改标签或收养权限。 |
| D08 / 中 | [VPC/Subnet](../../specs/vpc-subnet.md)，原 L7、96、203 及旧观察/验收段 | 顶部已声明 v3 优先，正文却仍有父 VPC available/新鲜度准入、60 秒有效期和 v2 锚点。逐条删改被替代的条件，并链接 v3 相应章节；保留产品父子关系、CIDR、租户、占用与并发约束。不能将整份资源/API/数据合同判为废弃。 |
| D09 / 中 | [VPC SNAT](../../specs/vpc-snat.md)，原 L5、106–136、156–157、221–224 | 仍把 Valid/Initialized/Ready、节点/CNI、chassis、双向绑定代次及观测新鲜度拼成运行门槛。当前[原生状态映射](../../../internal/data/network/kc_egress.go)逐类消费 EIP/池 Ready、VLAN Initialized、SNAT phase。按 v3 分离首次部署支持条件、自己的配置/引用与运行状态，撤回第二套 Ready/解绑证明；顶部已有替代声明，旧正文待收敛。 |
| D10 / 高 | [KCN v0.9 Underlay 流程](../../specs/kcn-v09-underlay-flow.md)，原 L11、94–96、202–227、248–257 | 仍称当前定位使用 namespace_prefix，要求组识别完成证明、节点新鲜度和验收 fingerprint/有效期后才能继续或分配，且未加 v3 替代入口。明确哪些是固定版本的历史取证/安装诊断，哪些是现行产品配置责任；状态和运行准入统一引用 v3。保留所选节点身份、自己的标签写入归属及逻辑占用，避免机械删除管理责任。 |
| D11 / 中 | [VPC/LB](../../specs/vpc-connectivity-lb.md)，原 L7、61、94、117、154、205–243；[LB 延迟激活](../../specs/lb-deferred-activation.md)，原 L28、99、109–127 | 顶部虽声明 v3 优先，正文仍等待生成 Service/Deployment/Pod/EndpointSlice、EIP/VIP 物理回收，保留旧 absence/occupied proof，并要求固定镜像/网络 freshness 准入。按 v3 §5 收口为 Res 自己声明的组件、配置应用与账本释放；移除已删除 generated adapter 的链接。原生配置回执所需 observedGeneration、未知 CREATE、声明 UID 与产品占用不能一并删除。 |
| D12 / 中 | [安全组](../../specs/security-groups.md)，原 L41–50 | 仍称 EIP appliedSecurityGroups 不能作为应用结果，并要求 Valid/Ready/代次/成员联合证明、历史 VNic 阻止引用释放。与 v3 §6 及[当前 adapter](../../../internal/data/network/kc_security_groups.go)相反。改为约定的自身 Ready、精确目标 applied 字段及 Res ID 映射；保留 configured/applied 区分和未释放产品引用，不恢复反向成员闭包。 |
| D13 / 高 | [VM 计划](../../plans/vm-mvp.md)，原 L324；[VM-04A](../../plans/vm-mvp-goals/VM-04A.md)，原 L20；[VM 验收矩阵](../../verification/vm-mvp-acceptance.md)，V18/V22 | 仍把 content/CSI 副本回收、快照/CSI 全部清理写入删除任务和完成验收，未说明一阶段 R05 已收缩边界。按 VM S10 和一阶段报告区分业务原生对象退出与组件/运维诊断；后者不成为 Res operation 的新完成门槛。历史真实 CSI 检查可保留其时点结果，不需删除，也不替代自己的 DV/PVC/producer/输出责任。 |
| D14 / 中 | [VM 正式合同](../../contracts/vm-v1.md)，原 L3；[VM 规格](../../specs/vm-mvp.md)，原 L5 | 仍笼统写“新增设计，待实现”“新增 API、表和方法均为拟实施”。这些阶段性状态与已交付的一阶段混杂，容易否定实际实现。去除全称实施状态，保留“规格定义目标行为”，具体实现/验证只链接唯一状态中的 VM 基线和一阶段补充；不把整个未来二阶段标成已实现。 |
| D15 / 中 | [ADR-0003](../../adr/0003-defer-workload-authentication.md)及[索引 ADR 表](../../START-HERE.md)；[ADR-0001](../../adr/0001-own-network-lifecycle.md)的历史身份延期文字 | 索引仍把“暂缓服务间身份验证”呈为当前决定，ADR-0003 只补了只读入口，未指向后续 full mTLS。应标明这部分历史决定已由当前服务调用合同承接；保留其原日期和原批次事实，不将过去测试上下文当作现在绕过认证的许可。当前依据为调用登记 §身份、范围和入口及现有 gRPC 安全装配。 |
| D16 / 中 | [文档索引](../../START-HERE.md)阅读顺序/ADR-0004 摘要/VM Goal 入口；[AGENTS](../../../AGENTS.md) Execution location | NET-05A 历史方案仍被常规阅读顺序描述为当前改造；ADR 摘要未体现 11-04 替代决定。R0–R5、VM 16 卡和旧环境约定仍带“本轮”或连续执行语气。将现行规格入口与历史工作包分开，明确旧连接及授权只属于原任务；按当前指令核对环境，不能从历史文档取得新执行权限。本次只新增共通原则指引，未擅改既有执行规则。 |
| D17 / 中 | [ADR-0007](../../adr/0007-image-domain-and-private-registry.md)，标题；[索引 ADR 表](../../START-HERE.md) | 文件已经使用 0007 编号，标题却仍为“待按下一可用 ADR 编号落库”，总 ADR 表也未列入（Image 段另有链接）。核实并整理元数据/标题及统一索引即可，不因命名问题改变已有 Image 职责。 |

## 需要规格与实现一同协调的项

| 编号 | 现有材料与代码 | 判断及后续处理边界 |
| --- | --- | --- |
| X01 | [容器规格](../../specs/instance-mvp.md)原 L283、292，[Sandbox 设计](../../plans/sandbox-runtime-design-20261025.md)原 L293、368；[Instance worker](../../../internal/biz/instance/worker.go)原 L282、[Sandbox worker](../../../internal/biz/sandbox/worker.go)原 L167 的 VolumeExists 分支 | 两个领域仍将原 PV 存在作为删除等待条件，存在与新增共通原则“管理完成不延伸 PV/CSI 清场”的职责冲突。这里的文档与实现仍相符，不能直接称为已经废止的文字，也不能只改文档声称整改完成。后续应明确这两个领域的完成边界，同步调整合同、实现和相关测试，并保留自有 PVC、未知写、消费者与引用责任；本次没有修改它们。 |

## 链接与证据位置清单

完整定位见[扫描 JSON](res-doc-review-20261109-links.json)：审阅前检查了 3379 个本地链接，发现 179 个不可解析目标。统计仅代表该源码和当前 checkout，不代表所有历史证据都已丢失。

| 类型 | 数量 | 建议处理 |
| --- | --- | --- |
| 仓内路径不存在 | 88 | 非历史记录中有 3 处：两处链接到已删除 adapter（D05/D11），一处为 status 原 L1636 的 `source22-main792-verify.log`。其余 85 处位于历史记录。现行说明改链到现有责任入口；历史源码引用尽量固定到原 commit/blob，证据文件先核对原归档，不能为修链接恢复已删除机制或伪造日志。 |
| 仓内锚点不存在 | 10 | 9 处位于非历史记录：vpc-subnet 5 处、vpc-snat 1 处、vpc-connectivity-lb 1 处、instance-mvp-v1.5 及 status 的 `cpu-增量合同` 各 1 处。另 1 处在 NET-VPC-LB-02 的旧记录。按真实替代章节调整，不给已废止 TTL/证明条款重新建锚点充数。 |
| 仓外路径在当前机器不存在 | 81 | 44 处来自非历史记录（status 的仓外归档 43 处、Underlay 的跨仓来源 1 处），其余 37 处来自历史记录。标明归档位置、固定来源和当前不可验证范围；不能由本机缺目录推出历史 PASS 错误，不能据此删除历史验收。 |

扫描识别代码围栏外的 Markdown 行内链接、引用式定义、ATX 标题与显式 HTML id/name，锚点按 GitHub 常见规则计算。特殊 Markdown 和跨仓路径仍需人工核对；远程 URL 未请求。上述现行失效锚点已对照标题确认。

## 应保留的材料和边界

- [旧 NET-05A 方案](../../plans/cr-observation.md)、[11-03 简化方案](../../plans/network-management-simplification-20261103.md)已经标注历史及替代关系；可调整导航位置，不应仅因较旧而整份删除。
- [ADR-0004](../../adr/0004-observe-cr-with-durable-reconciliation.md)已明确 11-04 的替代决定；保留原取舍语境，索引摘要应指向最新决定。
- VM 一阶段报告中的“尚未合并”“本轮未用集群”等属于各自时间段，后面的合并/部署补充已明确边界。不能将原记录改写成后续版本的通过，也不能把整机 Clone 认作已完成。
- [CDI 运维接替](../../runbooks/vm-image-cdi-handoff.md)是限定运维路径，不授予常规 worker 自动处理 finalizer 的权限；历史接替证据保留。
- [Storage 手工开通](../../../deployments/storage/README.md)不能仅因 Sandbox 已有租户开通就整份判废：现有 Sandbox 租户权限不等于 Storage 所有权限已自动接通，尤其不能擅删人工 RoleBinding 步骤。
- tenant、原 UID/owner、版本/租约、closing、幂等、未知写和输出交接属于业务责任；必要的审计历史、checkpoint、protobuf 保留字段号与声明配置回执不按旧 proof 机制处理。

## 本次改动与验证

本次新增原则正文和本审阅记录/链接详单，更新 AGENTS、单一索引与唯一状态入口。下列清理项仍待后续处理；没有修改业务源码、配置、迁移、测试或部署文件，没有提交或推送。

| 检查 | 实际入口 / 结果 |
| --- | --- |
| 工作区与来源 | 本机 WSL：`git --no-optional-locks status --short`、`git rev-parse HEAD`、`git log -6 --oneline`；初始差异已记录，已有排查文档保留。Windows Git 对 WSL 可执行位的显示差异不作为源码变化证据。 |
| 定向正文和代码核对 | PowerShell `Get-Content -Encoding UTF8`、`rg --files`、`rg -n`；与当前规格和实际端口/worker/adapter 对照，结果见 D01–D17、X01。 |
| 附件一致性 | `Get-FileHash -Algorithm SHA256`：源附件与入库文件一致，**pass**。 |
| 链接扫描 | 本机 WSL `python3 -` 一次性静态扫描，以 `git ls-files --cached --others --exclude-standard -- '*.md'` 固定审阅前范围；完整分类、行号及方法见 JSON，**pass（扫描完成，有既存问题）**。 |
| 本次文档变更 | 本机 WSL `git diff --check` 与新增/变更文档链接核对：**pass**；未新增不可解析的本地目标。已有 status 原内容和延迟排查文件完整保留。 |
| 业务验证 | `make verify`、单元/数据库/集群/浏览器及 CI：**not_verified（本次未运行）**。本次为文档审阅与接入，不以静态检查声称运行行为或现场验收通过。 |
