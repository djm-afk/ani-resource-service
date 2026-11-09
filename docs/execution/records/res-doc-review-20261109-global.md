# Res 全域原则复扫与待清理清单（2026-11-09）

本次按用户更新的[Res 全域管理职责与简化原则](../../specs/res-responsibility-and-simplification.md)重新审阅。原则正文在原路径替换，保持一份权威版本；本记录是第二轮审阅的时点结果，不是新规格或长期执行台账，后续处理只见[唯一状态](../status.md)。本次没有执行清单中的业务整改。

## 输入、变化与审阅范围

- 最新附件：《Res 全域管理职责与简化原则.md》，SHA-256 为 `c24043e20378a1ed214c7a012dd3e2d1e81cbe7b8239a028f6a5e6f6325bbc4a`，按原字节替换 `docs/specs/res-responsibility-and-simplification.md`。AGENTS 和索引同步说明全域适用及合同优先关系，没有保留第二份通用原则。
- 源码 HEAD 仍为 `813885cb8e6dfd11b4627d9a41ca8e3cd47c6afa`，分支 `codex/vm-phase1-remediation-20261106`；本轮开始时的未提交文件及 SHA-256 见[复扫输入和链接结果](res-doc-review-20261109-global-links.json)。本轮没有更改业务源码、迁移、运行配置、测试或部署清单。
- [初审](res-doc-review-20261109.md)保留旧附件的原时点结果。D01–D17 继续列入本清单；原 X01 根据实际调用链升级为 D18；本轮补充 D19–D25。共 25 组正文、职责或入口清理项，另列链接问题。D16 的全域原则阅读指引已补齐，历史执行入口等剩余问题仍待清理。
- 新版明确：全域原则约束存量与未来能力，旧合同、旧测试和未整改实现不构成豁免；具体字段、状态和完成条件仍由领域在此边界内定义。判断依赖对象归属和动作用途，不能按 PV/Node/Service 等类型一律删除，也不能给同步功能强加 worker、lease、operation 或统一 observer。
- 扫描全部 249 份审阅前 Markdown 的目录和本地链接，并定向复核现有 Network、VM、Instance、Sandbox、Compute、Image、Storage 七个模块；Storage 分别核对 PVC 与 S3 路径。历史执行记录主要核对来源、替代关系和链接，未逐句重新审计全部历史证据。
- 实际执行位置为本机 Windows PowerShell / WSL Ubuntu，仅文档读取、编辑及轻量静态核对；没有使用 SSH、集群或浏览器，没有查询远端 PR 现态，也没有取得附件引用的 VM/ACC Library 原包。Network/VM 的既有报告不用于证明其他模块或当前部署已整改。

## 各领域核对结果

| 领域 / 能力 | 本轮核对的对象、权威事实和边界 | 结果与待清理入口 |
| --- | --- | --- |
| Network | 产品配置、引用及声明原 UID；按原生 Ready/phase/Initialized 和当前消费者管理快照推进，非生成物清场 | 当前 v3 已明确边界，多份旧规格、运行手册和部署说明未收敛，见 D02–D12、D25。自己的节点标签、配置占用及声明身份继续保留。 |
| VM / VM Image | VM/VMI/相关运行对象、DV/PVC、Snapshot/Restore、具名输出与引用；复核 S08–S10 和一阶段报告 | 不重新认定已完成试点未实现；清理跨域旧证明及 CSI 完成条件的文字，见 D04、D13–D14。Required 卷、原生可用性、内容 SHA 和输出交接仍有效。 |
| Instance | Notebook、受控 STS/Pod、自己的工作 PVC、borrowed 引用、Network 与原账 outbox | 原 PV 等待/补扫延长消费领域的删除和退额边界，见 D18；Namespace 和网络闭包旧文档见 D05–D06。停止、未知写与 borrowed 名称 guard 不按 proof 删除。 |
| Sandbox | 自建 Pod/PVC、租户开通对象、数据面目标与在途 mutation；Pod Ready / PVC 自身状态和原对象身份 | 非直接管理的工作盘 PV 被用于删除完成、运行状态和全群补扫，见 D18–D19。暂停路径已保留独立完成语义，不把正常 Pause 当作待重新实现的整改。合同归档和旧阶段说明见 D24。 |
| Compute | 平台规格目录、可信用途、单次只读解析、冻结 selection 内容；无原生控制器结果可供重复证明 | 未在所查 CRUD/解析路径发现应按越权证明删除的依赖。用途说明有未更新文字，见 D23；平台范围、GPU 精确引用及 selection_digest 是业务合同，应保留。 |
| Image / Harbor | Project/Robot/登记 Digest、命令回执、Secret 写入、可信调用方与凭据代次 | 旧工作负载接线说明仍描述材料出站，已落后于统一 Secret writer，见 D22。Harbor 身份、内容和未知 POST 的业务恢复记录不因简化删除，不把旧文档误报成当前已暴露明文 RPC。 |
| Storage / PVC | 自己直接创建、扩容和删除的 PVC、借用引用/名称删除 ticket；只读详情可查 PV 策略 | 直接删除按原 PVC 消失或同名替换完成，未在所查删除链路发现等待 PV/CSI 的门槛。应补齐正式管理合同，见 D20；名称 guard、使用方校验、UID/RV 和在途删除恢复保留。 |
| Storage / S3 | 受管/租户桶映射、单 key 内容与 ETag、预签名请求和 S3 公开回执 | 所查路径为直接对象管理，无需照搬 Kubernetes UID、lease 或持久 worker。正式合同缺少完整边界说明，见 D21；单 key 读写、ETag 条件和对象统计的正常 LIST 不列为下游证明。 |

上述“未发现”只覆盖本次所查路径，不是整个模块已经完成全域迁移的认证。没有为本次审阅建立运行时注册表、证明表或全局验收控制器。

## 继续保留的待清理项 D01–D17

这些项已按新版原则和现有文件复核，原定位文件主体未改变；详细初审依据仍可追溯，但本表为本次复扫的结论。行号指本轮编辑前的文件，涉及 AGENTS/索引的行号以章节为准。

| 编号 / 优先级 | 定位 | 本次结论与建议处理 |
| --- | --- | --- |
| D01 / 高 | [README](../../../README.md) L9、51、57–58 | 仍把 Resource 描述成不含 Compute/Storage、VM/IAM 待后续的 Network 批次，且称关系索引/完整审计为运行事实。按当前[组合根](../../../cmd/ani-resource-service/app.go)更新入口范围，历史改名限制归原任务。 |
| D02 / 高 | [运行说明](../../runtime.md) L47–51、68 | 完整分页审计、证明续鲜/失效及废弃观察参数仍被描述成运行机制。与[网络 v3](../../specs/cr-observation.md)及[观察器](../../../internal/data/network/kc_observation.go)不符；改为精确通知、目标 GET、持久任务与读错重试，废弃参数注明忽略。 |
| D03 / 高 | [运行验证](../../runtime-verification.md) L21–27、测试入口表、L86 | 仍要求墓碑恢复、GeneratedReady 和证据过期拒绝准入。撤回被废止的完成条件；保留真正未知写恢复、租户/UID/租约测试，历史 NET-05A 验收明确归档。 |
| D04 / 高 | [VM 合同](../../contracts/vm-v1.md) C05 L179–181、L234；[VM 规格](../../specs/vm-mvp.md) L30、135、144、146 | Network 仍被要求重新核验整条关系和证明 VNic/VNicIP/Nic 退出。按当前消费者管理关系释放；VM 先完成自己的根/运行对象退出后 closed，普通 Stop 保留预留，地址独立。保留 owner 对正确目标及非终态 Pod 的检查。 |
| D05 / 高 | [容器配置与网络](../../specs/instance-configuration-network.md) L188–232、B4 | 仍要求 Network 完整 owner 链、fresh proof、历史网卡清场，并链接已删除 adapter。收敛至 v3 的当前快照及地址输出；保留 Instance 自己的停止、未知写、revision/finalization 等适用保护。 |
| D06 / 高 | [容器规格](../../specs/instance-mvp.md) §3、L421 | 任意可信 namespace/可配置前缀及“命名统一不在本轮”已被[标准 Namespace 合同](../../contracts/tenant-namespace.md)和实际装配替代。收回旧规则，保留冻结位置和原身份。此规则只用于对应 Kubernetes 租户资源，不推广到 S3/平台目录。 |
| D07 / 高 | [运行说明](../../runtime.md) L37 | “Resource 各 owner 不创建 Namespace”遗漏 Sandbox 租户空间开通的合法直接管理责任。按标准合同区分开通者与只读消费者；不能将 Sandbox 已有职责推广成所有 owner 的创建权。 |
| D08 / 中 | [VPC/Subnet](../../specs/vpc-subnet.md) L7、96、203 等 | v3 替代声明之下，仍存在父 Ready/freshness、60 秒 TTL 准入及 v2 锚点。逐条收敛，保留产品父子关系、CIDR、占用和并发。 |
| D09 / 中 | [VPC SNAT](../../specs/vpc-snat.md) L5、106–136、156–157、221–224 | 仍联合 Valid/Initialized/Ready、节点健康、chassis 和双向代次证明。按[当前逐类映射](../../../internal/data/network/kc_egress.go)与 v3 处理，安装支持诊断与运行门槛分开；不删除自己的配置、身份或引用约束。 |
| D10 / 高 | [Underlay 流程](../../specs/kcn-v09-underlay-flow.md) L11、94–96、202–227、248–257 | namespace_prefix、组识别证明、节点新鲜度、fingerprint 有效期仍写成当前动作前提，且缺少 v3 替代入口。按动作归属收回额外证明，节点 UID/本服务标签和占用责任继续保留。 |
| D11 / 中 | [VPC/LB](../../specs/vpc-connectivity-lb.md) L7、61、94、117、154、205–243；[延迟激活](../../specs/lb-deferred-activation.md) L28、99、109–127 | 仍等待生成物和 VIP/EIP 物理释放，保留 absence/occupied proof 与镜像/freshness 门槛。收口至自己声明的组件、原生配置回执和产品引用释放；不将 Service 类型本身列为禁用。 |
| D12 / 中 | [安全组](../../specs/security-groups.md) L41–50 | EIP applied 字段被否定，另要求联合成员证明和历史 VNic 清场。按 v3 §6 与[当前 adapter](../../../internal/data/network/kc_security_groups.go)修正；保留 configured/applied 区别及真实未释放引用。 |
| D13 / 高 | [VM 计划](../../plans/vm-mvp.md) L324、[VM-04A](../../plans/vm-mvp-goals/VM-04A.md) L20、[验收矩阵](../../verification/vm-mvp-acceptance.md) V18/V22 | content/CSI 回收仍写入删除完成任务。与 VM 一阶段 R05/S10 对齐：组件诊断不成为消费领域终态门槛；自己的 DV/PVC、producer、输出与引用仍须完成。历史 CSI 实测保留原时间。 |
| D14 / 中 | [VM 合同](../../contracts/vm-v1.md) L3、[VM 规格](../../specs/vm-mvp.md) L5 | “新增设计，待实现”“均为拟实施”全称状态已过期。规格只定义目标行为，实际实现指向唯一状态与一阶段补充，不把未来整机 Clone 写成完成。 |
| D15 / 中 | [ADR-0003](../../adr/0003-defer-workload-authentication.md)、[ADR-0001](../../adr/0001-own-network-lifecycle.md)及索引 ADR 表 | 暂缓服务身份验证仍呈为当前决定。标注历史部分已由[调用登记](../../specs/governance-calls.md)及现有 full mTLS 承接，不能据旧测试上下文恢复认证旁路。 |
| D16 / 中 | [索引](../../START-HERE.md)阅读顺序/ADR-0004/旧 Goal；[AGENTS](../../../AGENTS.md) Execution location | 本轮已强化全域原则指引；旧 NET-05A 当前化摘要、Storage“仍待后续切片”、R0–R5/VM Goal 的“本轮”及旧环境授权语气仍须整理。明确现行规格、历史方案及当前任务权限的区别，不凭历史入口执行。 |
| D17 / 中 | [ADR-0007](../../adr/0007-image-domain-and-private-registry.md)标题、索引 ADR 表 | 已编号 0007 却仍写“待按下一可用 ADR 编号落库”，总 ADR 表遗漏。整理元数据与索引即可，不改变 Image 业务责任。 |

## 本轮升级与新增项 D18–D25

| 编号 / 优先级 / 类型 | 文档和实际调用定位 | 问题、依据及待清理范围 |
| --- | --- | --- |
| D18 / 高 / 存量职责整改（原 X01） | [容器规格](../../specs/instance-mvp.md) L283、292；[Sandbox 设计](../../plans/sandbox-runtime-design-20261025.md) L293、348–368；[Instance worker](../../../internal/biz/instance/worker.go) L259–295、[Notebook adapter](../../../internal/data/instance/notebook.go) L241–325；[Sandbox worker](../../../internal/biz/sandbox/worker.go) L161–168、[Sandbox provider](../../../internal/data/sandbox/provider.go) L533–599、678–695 | **已核实为消费领域延长删除边界，不是直接管理 PV。** 两个 worker 在自有 PVC 已消失后仍等 VolumeExists=false；缺 PV 名时 adapter 还会完整 LIST PV，按 claim UID 补找底层绑定。Sandbox 设计本身 L360 已说明 PV/后端通常不由它直接创建或操作。按全域原则第 3/7/12 条撤回此类回收证明及补扫依赖，不能以旧合同作豁免。需同步收敛合同、worker/adapter、cleanup receipt 消费、仅为此用途的配置/权限和测试；完成自己的 Pod/STS/Notebook、PVC、未知写、Network、borrowed 引用及原账 outbox 责任，不能直接批量标成功或删除历史身份。Storage 正常详情查询和直接管理 PVC 的删除保护不在此项删除范围。 |
| D19 / 高 / 辅助诊断耦合 | [Sandbox 设计](../../plans/sandbox-runtime-design-20261025.md) L46、293、362–368、493；[Sandbox provider](../../../internal/data/sandbox/provider.go) L676、703–723 | `WorkspaceBound` 除 PVC 自身 Bound 外还合取底层 PV 存在、策略和 PV 查询成功；PV 读取失败/策略诊断先于 Pod Ready 将整体状态改为 Unknown，形成另一层运行状态裁决。按第 2/4/5/9 条明确各动作最小依赖，分开原 Pod/PVC 事实、必要业务输入和底层诊断；保留原 PVC 缺失、身份冲突、真实未就绪和存储诊断本身，不机械照搬 VM 字段。现有 Pause 在无原 Pod且未知写已收敛时可独立完成（L730 起），该行为保留。同步调整相关合同和错误投影测试，不能只删文档或清空错误。 |
| D20 / 中 / 正式合同缺口：Storage PVC | [Storage 部署说明](../../../deployments/storage/README.md) L1–76、[调用登记](../../specs/governance-calls.md) §存储；[用例](../../../internal/biz/storage/usecase.go)、[删除恢复](../../../internal/biz/storage/delete_recovery.go) L112–194、[Kubernetes adapter](../../../internal/data/storage/k8s.go) | 当前资料主要说明 RBAC、租户开通和 RPC 名称，缺少一个可供全域审核的完整 PVC 管理合同入口。应将已实现的直接管理对象、namespace/tenant/UID/RV、借用引用与名称 guard、DELETE 受理/等待/原 UID 退出的不同结果，以及查询失败与替换对象的语义收敛到正式合同并加入唯一索引。已有删除恢复只 GET 原 PVC、确认原对象退出，不等 PV；这是应保留的业务机制，不能为全域推广另加通用 operation/observer 或删掉合法使用方检查。属于补齐合同，不能据此声称运行逻辑违规或已完成迁移。 |
| D21 / 中 / 正式合同缺口：对象存储 | [Storage 部署说明](../../../deployments/storage/README.md) §对象存储配置、[调用登记](../../specs/governance-calls.md) L159–167；[对象用例](../../../internal/biz/storage/usecase.go) L359–648、[S3 adapter](../../../internal/data/storage/s3.go) | 现有说明偏向桶配置、权限和预签名 endpoint，未完整说明桶/key 的实际作用域与身份、受管/租户桶授权、上传 URL 交付与真实上传的区别、ETag 条件、正常分页/统计的部分结果、单 key 删除和错误分类。应按第 1/2/5/6/7 条补齐领域合同并索引。S3 直接结果与相应身份适用，不能强套 Kubernetes UID 或异步框架；内容/ETag 校验、业务统计 LIST 和短期签名 URL 不属于应删除的越权 proof。对尚未核定的错误或并发语义如实列为后续合同核对，不以文档缺口推断代码已通过所有场景。 |
| D22 / 高 / 文档落后于安全接线 | [Image 规格](../../specs/image-mvp.md) L113–121、[方法规格](../../specs/image-methods.md) L37、99–108；对照[当前 API 合同](../../specs/image-api.md) L90–98、[runtime 拦截器](../../../internal/server/image_runtime.go) L81–84 及[Secret writer](../../../internal/biz/image/pullsecret.go) L63–85 | 旧说明仍让创建方获取明文 pull material 并自行写 Secret，普通容器 owner 还标“待绑定”；当前 API 已约定 Resolve + Ensure，Resource 统一 writer 返回非敏感回执，[Instance adapter](../../../internal/data/instance/image.go)也已调用 Ensure。同步更新职责、方法和调用方接线说明，区分同进程内部取材与受信独立服务权限。拦截器明确拒绝旧 GetTenantPullMaterial，不能为满足旧文档重新开放；本次没有据旧文字断言当前存在明文出站漏洞，也不删除 writer 必需的内部取材。 |
| D23 / 中 / Compute 用途描述未收敛 | [Compute 合同](../../specs/compute-presets.md) L21、37、41；[用途定义与解析](../../../internal/biz/compute/resolution.go) L11–20 | 合法枚举已含 SANDBOX=5，摘要段仍写“四种合法用途”。统一说明五种用途的内容摘要语义，并区分外部目录入口与 Sandbox 固定生命周期用途，避免误解为 Sandbox 复用普通容器授权。保留用途/平台范围校验、GPU 精确引用、只读解析及 selection_digest；未发现需要为“全域统一”新增的 worker/租约。 |
| D24 / 中 / Sandbox 合同与计划混放 | [Sandbox 唯一设计入口](../../plans/sandbox-runtime-design-20261025.md) L3–9、§5–8、P1–P5 和来源段 | 当前稳定业务规则、未来能力、逐阶段实施要求及“新接线尚未验证”的旧时点文字混在 plans 中，唯一索引又把它作为全部 Sandbox 行为入口。将当前正式对象/事实/动作/完成合同收敛到正式规格位置，plans 保留顺序、未来目标和历史来源并标替代链接；具体完成状态仅链接唯一状态。尤其不能把 P5 的将来 checkpoint/clone 或旧未验证说明当成已实现能力/当前验收结论，也不能用旧 PV 条款豁免 D18–D19。 |
| D25 / 高 / 操作手册与当前合同脱节 | [Intranet 前提](../../../deployments/egress/intranet.md) L9–16、21；[LB 手册](../../../deployments/load-balancer/README.md) L11、25–38；[公网操作手册](../../kc-public-egress-manual.md) L17、23、209、271、616 | Intranet 仍写 controller/CNI/OVS/OVN 镜像联合门槛，LB 仍要求运行共享观察复核安装指纹和 VPC/Subnet freshness，公网手册仍有“新鲜验收/就绪证据”和已取消的候选网卡语义；L23 的历史测试命令还指向无 Go 包文件的 `./internal/data`。明确历史复现、显式安装诊断和现行产品动作各自范围，按 v3 收回运行门槛并更新当前命令路径。安装验证、管理员真实物理出网诊断和带日期的流量证据按用途保留，不因为出现 Node/Service 或 fingerprint 就整份删除；不借手册授权操作共享环境。 |

## 链接复扫

本轮替换前扫描 249 份 Markdown、3437 个本地链接，仍有 179 个不可解析目标，与初审相比没有新增问题。逐条文件、行号、目标和方法见[本轮 JSON](res-doc-review-20261109-global-links.json)，初审 JSON 保留原样。

| 分类 | 数量 | 处理方式 |
| --- | --- | --- |
| 仓内路径不存在 | 88 | 其中非历史记录 3 处：两个已删除 adapter 和 status 的 Source22 日志。当前说明改指向实际入口；历史源码引用固定到原 commit，日志先核对归档，不恢复废止机制或伪造证据。 |
| 仓内锚点不存在 | 10 | 非历史记录 9 处，主要为 v2 时效章节和 CPU 增量标题；历史记录 1 处。按实际替代章节修链接，不重建旧规则充数。 |
| 仓外路径当前不可访问 | 81 | 其中非历史记录 44 处、历史记录 37 处。单列归档/跨仓来源缺口；不由本机缺目录判断历史 PASS 错误。 |

范围、解析规则及特殊 Markdown 的限制与初审一致；远程 URL 未请求。本次新增和修改文档的本地链接单独校验，没有新增不可解析目标。

## 应保留与尚未验证

- 按实际作用域保留管理身份和适用保护：平台 Compute 不添加虚构 tenant；S3 不强加 Kubernetes 身份；同步 Ensure/解析不强加 worker；异步删除和输出交接复用本领域持久机制。
- Storage 的原 PVC 删除、借用引用/名称 guard、详情中的 PV 策略只读查询属于自己的直接管理或展示；D18 针对消费领域的底层清场依赖，不构成 PV 类型黑名单。Sandbox 自建 Pod/PVC 与未来确实自管的 Service/NetworkPolicy 也不因类型被禁止。
- 内容 SHA、selection_digest、ETag、合法身份签名和业务 mutation journal 不等于越权证明。其他模块尚未整改不成为本批读取/编辑文档的全局验收前置，也不被认定合规。
- [CDI 限定运维](../../runbooks/vm-image-cdi-handoff.md)、旧 NET-05A/11-03 的已标替代方案、VM 一阶段报告及后续补充均按原日期保留，不把历史 finalizer 接替变成常规授权。
- [Storage 手工绑定](../../../deployments/storage/README.md)和[Sandbox 部署说明](../../../deployments/sandbox/README.md)不能因租户开通能力存在就整体删除；现有开通权限与每类资源操作权限分别核实，未在本次访问集群验证它们。
- 没有运行完整测试、数据库或真实业务验收；此清单没有认定全 Res 已完成推广，也没有把源代码存在当成部署通过。

## 本轮实际改动和静态校验

本轮原位替换原则正文、强化 AGENTS 和索引指引；新增全域复扫记录/链接详单；初审记录增加替代说明，保留其原始扫描结果和旧附件摘要的历史含义。唯一状态追加本轮结果，之前的文档审阅、VM 延迟排查和其他历史内容保留。

| 检查 | 实际命令/入口及结果 |
| --- | --- |
| 源码与差异 | 本机 WSL `git --no-optional-locks status --short`、`git rev-parse HEAD`、`git diff --name-only`；业务源码没有更改。输入文件摘要见本轮 JSON。 |
| 全域静态复核 | PowerShell `Get-Content -Encoding UTF8`、`rg --files`、`rg -n`，定向读取上述领域的用例、adapter、worker 和合同；D18–D19 已对照实际依赖用途，其他域没有按关键词直接判违规。 |
| 附件替换 | WSL `python3 -` 读取附件并 `write_bytes` 原位写入，同源文件逐字节比较和 SHA-256 核对：**pass**。 |
| 链接与原内容 | WSL `python3 - baseline` / `python3 - verify` 一次性 Markdown 扫描，复用同一解析规则比较问题多重集：**pass（既存 179 项，新问题 0）**；初审链接 JSON、原 VM 排查文件和本轮之前的 status 原内容摘要保持。 |
| 文档差异 | 本机 WSL `git diff --check`：**pass**。 |
| 构建/业务验证 | `make verify`、单元/数据库/集群/浏览器/CI：**not_verified（本轮未运行）**；没有提交、推送或部署。 |
