# 虚拟机一期执行计划 v2

日期：2026-10-23。本计划只规定执行顺序与完成条件；业务以[规格](../specs/vm-mvp.md)，字段以[合同](../contracts/vm-v1.md)为准。验收ID见[矩阵](../verification/vm-mvp-acceptance.md)。本文不记录执行状态；当前结果仅见[唯一状态](../execution/status.md#vm-mvp)。此前 ENV-BLOCK 线索须在本轮目标重新核验。

## 1 执行方式

先执行VM-00，只读核验172.16.101.10～12集群身份和Block前置。确有runtime缺项时才准备VM-00R具体变更并等待明确批准；已满足前置时不强行变更，验证原生Block路径后继续。VM-01A～VM-01E可独立推进代码与隔离验证；VM-01F需目标Block能力、现场对象范围及物料就绪，形成首条真实Linux纵向后再扩ISO/Windows/多盘/快照。

本轮按[完整Goal](vm-mvp-complete-goal-20261023.md)与用户执行指令连续完成16张[Goal](vm-mvp-goals/VM-00.md)，覆盖制包时的逐卡停止限制；完成一张后更新唯一状态并按依赖继续，不逐卡询问。依赖中的“代码完成”和“现场通过”分开记录，不用测试替身替代真实链路。已写代码但现场缺前提，报告CODE_PASS/LIVE_NOT_RUN，不能把整项功能标PASS。

环境权限按S03；以后授权某张开发Goal不自动授权共享runtime、KCN/gate/CSI改动。产品/探针创建使用已批准的namespace、对象前缀、节点和清理范围。此前探测授权不得挪用。

本轮生成、编译、测试、PostgreSQL、镜像、服务运行、API/集群驱动全部仅ssh fedora；禁止Ubuntu或本地重任务回退。本地仅阅读、编辑、轻量检查、Git/转运及现有浏览器。Git传输全部用SSH。记录真实host、源码快照及dirty摘要，复用既有资源锁，不无界并行重任务。集群仅172.16.101.10～12；旧100.64.128.12 / 10.10.1.66～68结果不作为本轮验收。

开发阶段跨仓新协议可用任务独占go.work/临时module覆盖验证，必须明确记录并从交付候选剔除；它只证明隔离组合可工作，正式发布必须使用可获取精确ref。

## 2 依赖与阶段

| 卡片 | 工作 | 主要依赖 | 现场前置 |
|---|---|---|---|
| VM-00 | 固定源码、正式文档和实施输入 | 无。只读环境检查限已有获准入口；没有入口不探测新凭据。 | 本卡不要求真实集群写入 |
| VM-00R | 获授权维护窗口内完成 Block 运行时前置 | VM-00 具体变更单；用户届时确认集群无人使用并明确授权该变更、节点、重启及探针。缺一项都不得写入。 | 未来维护窗口+单独授权 |
| VM-01A | 固定 VM 协议、权限与 compute 解析 | VM-00；不依赖VM-00R。 | 本卡不要求真实集群写入 |
| VM-01B | 容器与 VM 统一 action 受理和投递 | VM-01A；不依赖VM-00R。 | 本卡不要求真实集群写入 |
| VM-01C | Resource 持久创建、独立 DV 与删除基础链 | VM-01A；可与VM-01B并行，现场依赖VM-00R。 | 代码可先做；真实路径需相应批准与能力 |
| VM-01D | 接通 VM Network consumer、NAD 和当前地址 | VM-01A、VM-01C的数据/端口；真实网络依赖VM-00R和已批准NAD准备。 | 代码可先做；真实路径需相应批准与能力 |
| VM-01E | 电源最小适配与可信 VNC | VM-01B、VM-01C、VM-01D；代码/隔离测试不依赖VM-00R。 | 代码可先做；真实路径需相应批准与能力 |
| VM-01F | 最小前端与 Linux Block 首条真实纵向 | VM-01B–VM-01E代码通过；目标Block能力已验证（需变更时VM-00R PASS）；明确本卡环境/对象写入授权；已定位现有前端与租户NAD/源。 | 代码可先做；真实路径需相应批准与能力 |
| VM-02A | 预置介质目录与 Linux ISO 安装 | VM-01F；已批准且固定的Linux ISO/驱动物料；代码可在现场依赖满足前准备。 | 代码可先做；真实路径需相应批准与能力 |
| VM-02B | Windows 驱动、设备与必要持久状态 | VM-02A；固定Windows版本/ISO/驱动；若所选OS要求TPM/UEFI，则其原生持久状态存储可用；快照组件/存储能力缺项只阻断后续快照恢复现场验收。 | 代码可先做；真实路径需相应批准与能力 |
| VM-03A | 多数据盘、冷挂卸与精确删除 | VM-01F；相关API已在VM-01A冻结。 | 代码可先做；真实路径需相应批准与能力 |
| VM-03B | 原生声明式在线热插拔 | VM-03A；现场需DeclarativeHotplugVolumes有效且旧HotplugVolumes关闭；Windows现场依赖VM-02B。 | 代码可先做；真实路径需相应批准与能力 |
| VM-04A | 在线和离线快照及完整性展示 | VM-03A/VM-03B的持久盘合同，VM-02B的Windows；Snapshot API/controller/driver/gate前置满足。 | 代码可先做；真实路径需相应批准与能力 |
| VM-04B | 原 VM 恢复、映射更新与旧盘清理 | VM-04A、VM-03B；已停止且VMI对象不存在；原生恢复与存储前置满足。 | 代码可先做；真实路径需相应批准与能力 |
| VM-05A | 全链清理、兼容与剩余风险验收 | VM-01F、VM-02B、VM-03B、VM-04B；沿用同一固定候选。 | 代码可先做；真实路径需相应批准与能力 |
| VM-05B | 精确依赖、发布准备与交接 | VM-05A；正式发布/部署按届时具体授权；准备工作可先完成。 | 按具体发布授权 |

VM-01F是首条纵向里程碑；VM-02B、VM-03B、VM-04B补齐一期范围。VM-05A只补剩余风险与受影响回归。发布准备与实际发布分开，VM-05B未获发布授权时仍能交付完整待执行材料。

## 3 逐卡要求

### VM-00 固定源码、正式文档和实施输入

- 主责：Resource 主责；Gov、SG、前端 owner 配合
- 前置：无。只读环境检查限已有获准入口；没有入口不探测新凭据。
- 规则：S01–S05、S12；C01–C03。
- 修改范围：Resource docs/START-HERE.md；docs/START-HERE.md#vm-mvp“文档接入与唯一状态”映射表中的正式文档、Goal、模板及复核记录；docs/execution/status.md的VM章节及records/VM-MVP下本卡记录。只读遵循各仓 AGENTS 与适用 remote-execution，不借本卡修改授权或执行规则。

1. 严格按docs/START-HERE.md#vm-mvp映射表接入正式文件，替换旧版相反规则并登记索引；不覆盖其他任务内容或仓库README。改正所有相对链接及Goal文字路径并校验；模板保持空白，实际输入/维护单另存records/VM-MVP；单一实施状态留在docs/execution/status.md#vm-mvp。
2. 重新固定 Resource/Gov/SG HEAD、dirty清单与文件摘要，核对本包基线差异；定位现有前端仓库/框架/VM页面应接入的位置。前端未定位只阻断前端卡，不自建新项目。
3. 填写 docs/plans/vm-mvp-templates/environment-inputs.yaml 的真实 host、cluster/namespace UID、目标节点、runtime版本/配置来源、StorageClass/StorageProfile、SnapshotClass、gate、受管NAD、正式Linux/Windows物料和可用VNC路径；凭据只记引用。
4. 按 docs/plans/vm-mvp-templates/runtime-change-plan.md 准备具体配置diff、备份/恢复方案、节点顺序、限定探针与清理清单。ENV-BLOCK按10～12目标的当前只读事实登记；确有缺项才保持BLOCKED_ENV，准备具体批准单；缺NAD/物料/gate分别登记，不擅自修补。
5. 核对协议新枚举/tag未被后续代码占用；冻结未确定的实际材料与执行位置，不发明新产品规则。

产出：基线/差异清单、已接入文档、环境与物料输入、具体维护变更单、按能力拆分的阻塞清单。

验收：V01、V03 的基线/编号检查。

退出：所有可获取输入已固定；缺项具体到对象/原因/受影响卡。此卡完成不要求执行runtime变更或现场产品验收。

禁止扩展：不改runtime、不重启/清空集群、不部署产品、不导入介质、不改KCN/CSI/gate；不将历史探测写成新验收。

Goal：[VM-00.md](vm-mvp-goals/VM-00.md)

### VM-00R 获授权维护窗口内完成 Block 运行时前置

- 主责：受授权的集群实施 agent
- 前置：VM-00 具体变更单；用户届时确认集群无人使用并明确授权该变更、节点、重启及探针。缺一项都不得写入。
- 规则：S03、S04、S06；U01/U02；维护模板。
- 修改范围：仅批准的 runtime 配置输入与对应服务；批准namespace中的限定DV/PVC/探针VM；环境证据。

1. 读取并复核批准窗口、节点、配置来源和配置diff；备份实际配置。发现仍有不在批准中可中断的业务负载，保存事实并暂停写入，不自行关停它们。
2. 按实际containerd v1/v2或CRI-O及发行版方式只设置device_ownership_from_security_context；k3s/RKE2使用可持久化生成输入，不能盲改生成文件。
3. 按批准顺序逐节点重启对应服务，核验节点和既有基础组件；任何失败按批准恢复方案处理，不并发重启全部节点。
4. 在批准范围新建/重建受影响CDI工作负载，覆盖选定源import/clone/blank到Block和RWO；核对非root设备访问、所有候选VM节点的有效配置及一条真实VM读写。
5. 删除本次精确探针资源并核对回收，保留原错误/配置摘要/对象UID/结果。仅能力验收PASS后解除ENV-BLOCK。

产出：实际配置差异/备份位置、节点逐项结果、Block探针与清理证据、ENV-BLOCK结果。

验收：V01、V02。

退出：批准节点配置生效，选定Block路径及清理验证通过；不得据此标记ANI VM产品链路通过。

禁止扩展：当前没有后续授权时只补准备材料并报告BLOCKED_ENV；不改Filesystem兜底、不提高Pod权限、不升级组件、不改KCN/共享gate、不修改installer。

Goal：[VM-00R.md](vm-mvp-goals/VM-00R.md)

### VM-01A 固定 VM 协议、权限与 compute 解析

- 主责：Resource + Gov API owner
- 前置：VM-00；不依赖VM-00R。
- 规则：S02、S04；C01–C04、C06。
- 修改范围：新增Resource api/vm/v1、api/vm/console、internal/service/vm、internal/biz/vm必要类型；compute usage与scope；Gov Module/PlanModule/Api/permcode及公开DTO。

1. 按C01/C03/C06生成正式业务Proto和纯console子模块；固定FullMethodName常量、字段/tag、请求/响应/error reason。只生成本期合同，不实现空provider框架。
2. 接入VIRTUAL_MACHINE usage与功能module=17，权限和精确方法scope；Resource ResolveCreateSpec内部一次调用compute，组合媒体/磁盘/网络只读校验。
3. 实现VM v1 canonical与创建摘要：vm_contract_version唯一、冻结原生来源/namespace UID、十进制GB/CPU拓扑/内存映射；拒绝公开输入可信原生身份。
4. 冻结网络期望和console binding字段；保持已有Instance/compute API字节及纯console依赖边界。

产出：可生成编译的协议、字段/错误/权限映射、Resolver最小实现与规则检查。

验收：V03、V06；租户/scope错误用例。

退出：协议/生成/影响范围检查通过，Resolver仅只读且冻结结果稳定；console module不依赖Resource运行时。

禁止扩展：不新增CPU/mem/disk配额、preset版本、VM专属GPU禁令、全量API拆包；不以临时本地replace宣称正式发布。

Goal：[VM-01A.md](vm-mvp-goals/VM-01A.md)

### VM-01B 容器与 VM 统一 action 受理和投递

- 主责：Gov
- 前置：VM-01A；不依赖VM-00R。
- 规则：S02；C02–C03。
- 修改范围：Gov internal/commandcontract、service/quota_adapter.go、command_contract.go、gpu_acceptance.go、managed_admission.go、instance_cpu_contract.go、quota_ledger_repo.go调用点、Resource client/composition、Gov VM基础BFF service/路由。

1. 保留一owner一adapter registry，建立唯一静态action定义；容器create/delete与VM create/delete同路查表，去除共用层owner→Instance的usage/version/hash/类型断言。
2. VM canonical走自己的codec；历史InstanceContractVersion列按C02占位兼容，原Instance CPU/GPU与Inference合同不改字节、不放开旧禁用版本。
3. 沿既有Gov事务/租约/worker保存VM create/delete，空items/charges不写扣退额；空charge合法性按各自已注册合同判断。
4. 实现当前授权后幂等、严格durable ACK、尚未送到Res时的operation查询；未知投递结果复用原ID/命令。
5. 完成C03的VM列表、详情、创建、删除及operation查询BFF；列表/详情经当前授权调用Resource，不进配额投递。既有compute目录透传VIRTUAL_MACHINE用途和对应读取权限。
6. 按C03.3复用canOperate实现Gov删除持久受理栅栏；暂停DELETE投递时拒绝新直达变更/会话，保留原查询与合法重放；处理在途请求及expected_version冲突，不覆盖旧命令。

产出：统一action路由、VM持久投递接线、真实隔离PG和兼容用例结果。

验收：V04、V05（含C03.3删除栅栏）、V24中受影响受理/配额合同。

退出：CPU容器、GPU容器、VM同一选择规则；VM无资源账户变化，旧合同重放不退化。

禁止扩展：不注册第二个Resource adapter，不创建通用动作引擎，不把电源/盘/快照塞入Gov配额ledger，不重命名全部旧GPU表/类。

Goal：[VM-01B.md](vm-mvp-goals/VM-01B.md)

### VM-01C Resource 持久创建、独立 DV 与删除基础链

- 主责：Resource vm
- 前置：VM-01A；可与VM-01B并行，现场依赖VM-00R。
- 规则：S02、S04–S06、S10–S11；C03–C04。
- 修改范围：internal/biz/vm、internal/data/vm、internal/service/vm、migrations/vm（新增）、api/vm/v1、cmd/ani-resource-service/vm.go；受信listener。

1. 建立租户隔离的VM/disks/operations数据与显式迁移，冻结原始创建和当前配置；沿现有模式持久受理、工作领取、pending mutation与恢复扫描。
2. 实现AcceptCreate/AcceptDelete/Get/List/GetOperation和原生adapter：确定名称→Network Prepare端口→Manual根VM→UID登记→独立DV(owner=VM)→按意图Start。
3. root/data显式Block/RWO；DV/PVC未知create用同一对象查证；WFFC不阻止消费者创建，停止态创建允许盘等待消费者。
4. 只实现首条Linux系统镜像链所需根盘、媒体来源校验、停止保盘、最终删除及cleanup refs；data_disks接口保留，未实现阶段不可默默忽略非空输入。
5. 删除可接管关闭但不丢未知mutation；保持共享源/NAD不变，网络Release经端口，异域SQL禁止。

产出：首条VM核心代码、迁移、真实PG受理恢复和原生adapter契约验证。

验收：V05、V07、V22首链相关用例；真实Block证据在VM-01F完成。

退出：隔离验证证明幂等/重启恢复/归属/UID删除成立，核心可与网络与SG拼接；现场未跑保持NOT_RUN。

禁止扩展：不使用dataVolumeTemplates，不自写CDI转换器/虚拟化controller，不写Instance表，不复制通用工作流；不先做全部CRUD再拖延纵向链。

Goal：[VM-01C.md](vm-mvp-goals/VM-01C.md)

### VM-01D 接通 VM Network consumer、NAD 和当前地址

- 主责：Resource network + vm
- 前置：VM-01A、VM-01C的数据/端口；真实网络依赖VM-00R和已批准NAD准备。
- 规则：S07；C05；S08的VNC解耦。
- 修改范围：C05列出的biz/data/service network路径、api/network/v1、VMNetworkConsumer/NetworkPort、两个composition。

1. 新增vm_managed/v3严格VM→VMI→launcher链，保留v1/v2格式、摘要和owner校验；VM走VMReader，不查InstanceReader。
2. 实现Prepare.vm_expectation=12、VMPrimaryPlan=26、当前地址=27；NAD身份/config摘要与KCN Subnet独立，Validate/Prepare冻结一致。
3. 复用现有Attachment worker和快照修订，当前端点只有launcher；地址从Network正式DTO输出，过期/停止/UID冲突清空资格。
4. Stop保留Attachment，Start/Restart观察新UID；删除按根/运行对象退出→consumer closed→Release→Network独立released，不引入循环等待。
5. 缺NAD拒绝新建/Start；依赖消失不阻止按冻结UID Stop/Delete。两个装配入口都生效。

产出：新模式协议/adapter/投影/地址和现有worker接线、兼容与身份测试。

验收：V09、V10的隔离部分、V24旧Network路径；真实来宾部分在VM-01F。

退出：新旧模式严格分派、无地址跨域私读、网络关闭顺序成立；现场条件缺失不冒充通过。

禁止扩展：不新增NAD产品生命周期、固定IP、多NIC、任意GVK注册、binding插件；不改KCN或用共享默认网代替租户隔离。

Goal：[VM-01D.md](vm-mvp-goals/VM-01D.md)

### VM-01E 电源最小适配与可信 VNC

- 主责：Resource + Gov + SG
- 前置：VM-01B、VM-01C、VM-01D；代码/隔离测试不依赖VM-00R。
- 规则：S08；C03、C06。
- 修改范围：vm power/native/console用例；Gov VM power/console入口；api/vm/console；SG proto、session store/fingerprint、mTLS、kubevirt adapter与现有WS路径。

1. 实现Start/Stop/ForceStop/Restart持久operation及当前version互斥；Stop采用原生宽限期终止语义，停止保盘/Attachment。
2. Restart提交前保存old_vmi_uid；超时先观察原请求/新旧UID，未知不盲重发；不用运行代次计数器。
3. Gov创建console会话使用稳定幂等键和可信ResolveConsoleTarget；SG新增vm_binding及专用mTLS校验，存储/fingerprint包含全部字段。
4. 连接前、原生开流后转发前核验VM/VMI身份与关闭意图；不依赖guest IP/Agent/安装完成或create操作结束。
5. 旧流随原VMI结束关闭，旧绑定失效；Resource不等待SG关闭回执，原Instance terminal路径保持。

产出：电源与console完整接线、未知结果/同名换UID/旧票据用例。

验收：V11–V13隔离部分、V24 SG兼容；真实键鼠与停开在VM-01F。

退出：接入现有运行/会话机制；只新增必要绑定和operation上下文，不引入新服务。

禁止扩展：不自建关机/运行代次/全局会话撤销系统，不让旧票据连接最新VMI，不暴露QEMU端口或让浏览器传原生目标。

Goal：[VM-01E.md](vm-mvp-goals/VM-01E.md)

### VM-01F 最小前端与 Linux Block 首条真实纵向

- 主责：前端 + Gov/Resource/SG 联调
- 前置：VM-01B–VM-01E代码通过；目标Block能力已验证（需变更时VM-00R PASS）；明确本卡环境/对象写入授权；已定位现有前端与租户NAD/源。
- 规则：S01、S04、S06–S08、S10；C03。
- 修改范围：已固定的现有前端VM菜单/页面/API client/noVNC接入；上述服务必要最小修复；受控验收记录。

1. 接现有UI风格/认证/API client，完成VM列表/创建/详情/操作状态/console/电源/删除；筛选无GPU规格，输入GB，不展示任意硬件/YAML。
2. 通过真实Gov→真实PG→Resource产品入口创建Linux qcow/raw的Block根盘VM；验证真实浏览器画面、键鼠登录。
3. 在真实租户子网验证DHCP、DNS、出站、两节点guest唯一marker HTTP与跨租户拒绝；停止/再启动盘marker保留、Attachment不变、IP可变化。
4. 覆盖同键重放、受理重启、Restart丢回执、旧票据拒绝；缺guest IP时控制台仍能用于排障。
5. 通过产品删除核对VM/盘/网络及旧binding；记录API受理、原生状态、guest/browser证据，不用202/Ready代替。

产出：首条真实纵向产品用例、最小可用UI与精确清理证据。

验收：V02结果引用；V08、V10–V13、V22首链、V23关键拒绝。

退出：Linux创建→浏览器→真实租户网→启停→删除闭环全部通过；此后再扩ISO/Windows/多盘/快照。

禁止扩展：不新建前端项目，不把未授权环境变更夹带进联调，不用Filesystem、共享默认网、Pod ping或WebSocket握手替代验收。

Goal：[VM-01F.md](vm-mvp-goals/VM-01F.md)

### VM-02A 预置介质目录与 Linux ISO 安装

- 主责：Resource + Gov + 前端
- 前置：VM-01F；已批准且固定的Linux ISO/驱动物料；代码可在现场依赖满足前准备。
- 规则：S05–S06；C03。
- 修改范围：vm media catalog/native adapter、ListMedia/SetMedia/EjectMedia/SetBootDevice、Gov/UI介质与启动设置。

1. 完成只读受管目录、来源UID/内容校验、租户专属ISO副本；SetMedia首次受理冻结本次兼容来源及副本身份，重放不重解析，执行前重核，不回写初始create_spec。共享源不可写且不归用户VM。
2. 空白Block系统盘、预定义安装/驱动光驱与有限启动顺序；通过VNC手动安装Linux。
3. 介质更新区分持久意图与运行态实际结果；仅模板生效时配置operation可结束，返回pending_restart，不占住互斥等Start、不静默重启；替换/弹出副本仍保留到最终删除。
4. 去除安装介质、切系统盘优先，重启后从原系统盘登录并读marker；介质副本纳入最终清理。

产出：只读介质入口、Linux ISO安装/摘盘/磁盘启动产品链。

验收：V14、V07来源隔离、V22媒体清理。

退出：Linux ISO与系统镜像两条链各有独立证据，不假造Installed状态。

禁止扩展：不做镜像管理、自动安装、qemu-img转换、自动驱动注入；不扩大CDI跨namespace授权。

Goal：[VM-02A.md](vm-mvp-goals/VM-02A.md)

### VM-02B Windows 驱动、设备与必要持久状态

- 主责：Resource + 前端 + 验收owner
- 前置：VM-02A；固定Windows版本/ISO/驱动；若所选OS要求TPM/UEFI，则其原生持久状态存储可用；快照组件/存储能力缺项只阻断后续快照恢复现场验收。
- 规则：S05、S08–S09；C03。
- 修改范围：受管Windows物料/设备默认值、vm native映射、既有介质/console UI；必要场景验收。

1. 按已冻结Windows版本使用只读SATA VirtIO驱动光盘，Block根盘与匹配VirtIO网卡/磁盘驱动。
2. 浏览器完成安装、键鼠交互、识盘、NetKVM联网、Agent实际连接；去ISO后从根盘启动。
3. 若系统要求TPM/UEFI，使用KubeVirt原生持久状态，backend PVC保留Filesystem，不强套Block；停开验证必要状态。
4. 补足Windows当前功能UI表现与原生错误；后续快照/热插拔的Windows现场结果由对应卡补齐。

产出：固定版本Windows可重复安装/启停/网络/VNC证据及物料说明。

验收：V15中的安装/停开部分；TPM快照恢复在VM-04B补全。

退出：至少一套正式Windows版本按真实要求通过；材料缺失则明确NOT_RUN，不用Linux结果替代。

禁止扩展：不关闭OS要求、不自动激活、不自动注入驱动、不扩为任意Windows支持矩阵。

Goal：[VM-02B.md](vm-mvp-goals/VM-02B.md)

### VM-03A 多数据盘、冷挂卸与精确删除

- 主责：Resource + Gov + 前端
- 前置：VM-01F；相关API已在VM-01A冻结。
- 规则：S06、S10–S11；C03–C04。
- 修改范围：vm disks/operation/repository、CreateDisk/AttachDisk/DetachDisk/DeleteDisk、Gov/UI磁盘列表和确认。

1. 完成创建时data_disks数组与后续blank自有盘；每盘稳定disk_id/volume_name/serial，Block/RWO/整数GB。
2. CreateDisk声明/身份登记后结束，允许WFFC；冷挂卸以持久配置/逻辑关系完成，不能等不存在VMI或盘Succeeded占住Start。
3. 至少两块数据盘各写marker，卸载保留、再挂原盘，关开机数据保留；拒绝跨VM/跨租户引用。
4. 单盘删除要求原生已卸载，带UID清理DV/PVC；保留必要tombstone与cleanup refs，为快照还原旧逻辑盘提供身份。

产出：完整多盘冷态管理与删除UI/API、PG/原生语义和guest验证。

验收：V16、V22多盘清理、V23磁盘身份。

退出：两块数据盘冷挂卸/再挂与精确删除通过，盘准备状态不冒充可用。

禁止扩展：不格式化/分区/扩容guest，不借用任意已有PVC，不做独立云盘、共享盘或单Pod排他检查。

Goal：[VM-03A.md](vm-mvp-goals/VM-03A.md)

### VM-03B 原生声明式在线热插拔

- 主责：Resource + 前端 + 验收owner
- 前置：VM-03A；现场需DeclarativeHotplugVolumes有效且旧HotplugVolumes关闭；Windows现场依赖VM-02B。
- 规则：S06；C03。
- 修改范围：vm原生卷/设备更新与观察、UI运行态盘结果；仅必要错误/回执适配。

1. 数据盘从首次配置具有SCSI/serial/hotpluggable；在线挂卸只更新原生声明，用既有operation观测。
2. 成功同时核对VM模板与VMI volumeStatus；显示原生等待/失败，不能模板写入即成功。
3. Linux/Windows各在真实VM至少两块盘在线挂卸与marker读写；guest先离线/卸载后确认，helper不参与Network当前端点。
4. 检查卸载保盘、重挂同盘、Stop/Start声明和数据保留；重复请求不创建新盘。

产出：在线挂卸产品能力及双系统真实证据。

验收：V17、V09 helper身份；恢复后热插拔另由V21验证。

退出：两种OS运行态验证完成且底层gate/驱动证据齐备；gate缺项保持该现场阻塞。

禁止扩展：不自建挂盘controller、不改旧热插拔API混用、不擅改共享gate、不把在线失败改成自动关机后成功。

Goal：[VM-03B.md](vm-mvp-goals/VM-03B.md)

### VM-04A 在线和离线快照及完整性展示

- 主责：Resource + Gov + 前端
- 前置：VM-03A/VM-03B的持久盘合同，VM-02B的Windows；Snapshot API/controller/driver/gate前置满足。
- 规则：S09–S10；C03。
- 修改范围：vm snapshot记录/native adapter、Create/Get/List/DeleteSnapshot、Gov/UI快照与一致性展示。

1. 只创建原生VirtualMachineSnapshot，冻结source VM UID与当前持久root/data逻辑盘清单；等待原生结果，核对included/excluded。
2. 完整性、Agent连接/quiesce与原生indications分别展示，必要盘缺失不得标完整可恢复。
3. Linux/Windows各覆盖在线/离线快照，系统盘+至少两块持久数据盘；无Agent路径明确不保证冻结，不暗中关机。
4. 删除快照完成本服务原生对象与引用的管理责任；Content/CSI 回收按组件策略处理，不作 operation 完成前置，不逐盘编排或删除其他快照数据。

产出：快照API/UI、逻辑盘inventory与完整性合同、双系统快照结果。

验收：V18、V23快照归属、V22快照清理。

退出：在线/离线覆盖和失败展示通过；能恢复的数据证明在VM-04B闭合，不能提前宣布全功能快照验收完成。

禁止扩展：不提供RAM恢复、备份调度、group snapshot保证、数据库事务一致保证或自有CSI快照引擎。

Goal：[VM-04A.md](vm-mvp-goals/VM-04A.md)

### VM-04B 原 VM 恢复、映射更新与旧盘清理

- 主责：Resource + Gov + 前端
- 前置：VM-04A、VM-03B；已停止且VMI对象不存在；原生恢复与存储前置满足。
- 规则：S09；C03–C04。
- 修改范围：PreviewRestore/RestoreSnapshot、vm当前配置/盘映射/cleanup上下文、UI影响预览；原生Restore adapter。

1. 固定FailImmediate/RandomizeNames/Vm与同根VM UID；实现五种盘集合预览，version+preview_digest绑定确认，版本变化重预览。
2. 保存snapshot disk inventory以恢复当前已卸载/已删除的快照内盘；快照外挂载新增盘成功后删除，快照外已卸载盘保留。
3. 先原生完成，再核对新PVC UID并持久映射，停止态补回数据盘hotpluggable/SCSI/serial，再精确清理已冻结旧root/data；backend由native管理，不保证失败回滚。
4. operation在cleanup确认前保持处理中；重启/超时复用进展点，不重复Restore，不覆盖回初始DV；WFFC PVC Pending不阻止结束，不隐式Start。
5. 双系统在线/离线快照都完成restore→显式Start→marker读回；恢复后在线摘挂和必要TPM/UEFI状态一并验证。

产出：有确认的原VM恢复、稳定逻辑盘映射、可恢复清理及双系统真实数据证据。

验收：V19–V21、V15 TPM恢复、V23恢复权限/冲突。

退出：五种盘集合、未知结果/cleanup恢复、恢复后热插拔及不回写旧盘均通过。

禁止扩展：不跨VM克隆、不逐盘人工CSI恢复、不自动备份补偿、不静默关开机、不在native未知时主动删旧root/data。

Goal：[VM-04B.md](vm-mvp-goals/VM-04B.md)

### VM-05A 全链清理、兼容与剩余风险验收

- 主责：跨服务联调owner
- 前置：VM-01F、VM-02B、VM-03B、VM-04B；沿用同一固定候选。
- 规则：全部规格；验收矩阵V01–V24。
- 修改范围：受影响服务/前端的必要修复与docs/execution/records/VM-MVP；不新建测试平台。

1. 用同一候选补齐未执行格子，已通过且未变更的证据可复用，不无理由全量重跑。
2. 覆盖导入中、停止态、在线热插拔后、Restore映射/cleanup中删除；目标未知先查证，所有当前/卸载/旧引用资源完整回收且共享源/NAD不变。
3. 定向回归compute原用途、容器CPU/GPU action与账项、Inference受影响分派、Network v1/v2、SG Instance terminal。
4. 核验跨租户、同名换UID、当前授权重放、观察过期、预览冲突、旧票据与cleanup中进程重启；没有CPU/mem/disk账户变化。
5. 分开报告代码/真实PG、边界替身、真实集群、guest/browser；未跑保持NOT_RUN，失败不降范围。

产出：完整V01–V24覆盖索引、残留扫描、兼容结果、必要最小修复。

验收：V22–V24及此前尚未闭合项。

退出：全部一期必需验收已通过或明确保持阻塞；有必需NOT_RUN/FAIL不得标一期完成。

禁止扩展：不强删finalizer、不清库清namespace制造清理成功、不修无关项目、不增加库存/配额/工作流系统。

Goal：[VM-05A.md](vm-mvp-goals/VM-05A.md)

### VM-05B 精确依赖、发布准备与交接

- 主责：各仓owner与部署owner
- 前置：VM-05A；正式发布/部署按届时具体授权；准备工作可先完成。
- 规则：S12；C01/C06；V25。
- 修改范围：正式module依赖、迁移/部署配置、版本矩阵、docs/execution/status.md与交接材料。

1. 先形成可审的API/module版本、服务镜像digest、迁移清单、部署顺序和回退范围；正式依赖必须可获取，删除临时验证go.work/replace。
2. 发布兼容协议后更新精确Gov/SG依赖；显式增量迁移，部署理解新合同的Resource/Gov/SG，核验写入/恢复/清理worker后再开放VM入口。
3. 写明回退先关闭新受理，仍保留理解新VM数据/命令的处理能力；不能退到会误解析VM action或丢新盘映射的旧版本。
4. 仅在具体发布/部署获得授权后执行对应写入；未获授权交付完整可审发布单并保持NOT_RUN，不阻塞文档/验证收尾。
5. 汇总实际来源SHA、正式依赖、证据、操作说明与未完成条件；更新唯一状态。

产出：精确版本/迁移/部署与回退单、前端交接、唯一状态；获授权时附实际发布结果。

验收：V25。

退出：所有可审发布准备完成；正式部署结果与准备状态分别陈述，不因任务包交付声称已发布。

禁止扩展：不自动merge/tag/release/推送部署，不降级到不理解新合同的版本，不清理历史数据作回退捷径。

Goal：[VM-05B.md](vm-mvp-goals/VM-05B.md)

## 4 状态与停止规则

所有结果写入现有唯一状态，不另建总台账。报告包含源码/工具/host、当前授权范围、实际命令及退出码、变化文件、资源UID、验收ID及PASS/FAIL/NOT_RUN、cleanup与剩余依赖，按[模板](vm-mvp-templates/task-report.md)。

未知原生结果继续查证同一目标，身份冲突停止该对象的副作用；共享缺项只阻断依赖卡，其他已授权工作继续。新风险只补必要验证，文档整理不触发全量重跑。不得为通过验收改默认模式、降低身份校验、强删finalizer或修改配额账目。

包内验收文件在接入仓库时落为 docs/verification/vm-mvp-acceptance.md；本包其余引用随实际目标同步修正。原计划VM-00～VM-05已细分为本表，不继续单独执行旧粗粒度卡。
