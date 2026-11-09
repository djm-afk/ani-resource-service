# 虚拟机一期验收矩阵 v2

日期：2026-10-23。此文件定义待执行验收，**不是执行结果**。本文件保留V01–V25及双系统全部必需项，当前产品结果仅写[Resource唯一状态](../execution/status.md#vm-mvp)及证据索引，不在本文件维护结果。

本轮按[完整 Goal 的后续范围](../plans/vm-mvp-complete-goal-20261023.md)执行：ANI 业务前端对接由专人负责，BROWSER 使用最薄临时 VNC 测试页；其真实画面、登录、键鼠和会话拒绝场景仍为必需项。

V10 的历史范围修订按[完整 Goal 后续用户修订](../plans/vm-mvp-complete-goal-20261023.md)执行：仅另行授权公网子任务，办公内网因用户明确不做且无环境而排除，旧五对象 Intranet 方案未执行并取消本轮应用；不据此将 DNS 或其它格子记为 PASS，该轮公网子任务本身未恢复整体Goal；之后用户已明确RESUMED。

WFFC 当前环境范围按[完整 Goal 最新用户指示](../plans/vm-mvp-complete-goal-20261023.md)执行：保持现有 `ani-block` / Immediate，不新增 WFFC SC/Profile/组件、不改 binding 或暗启 VM，错误如实返回/展示；专用 LIVE 记为当前环境不适用/未覆盖，不计 PASS、不声明已实测支持。下表 V07 独立 DV/Block 身份、V16 多盘/冷挂卸及 V20 恢复映射/清理等其它要求仍保留，既有 PG/BOUNDARY 结果不删；旧新增五对象 Snapshot 方案本轮不可执行，替代原地 VSC policy 的范围已由用户明确为整个10～12集群后续新快照、RBD＋CephFS，配置已达 CONFIG_APPLIED_VERIFIED，具体回执仅见[唯一状态](../execution/status.md#vm-mvp)。该永久集群策略不随Goal清理回退；后续规范类名为 `ani-rbd-delete` / `ani-cephfs-delete`，既有Profile spec/status显式新名、旧类已正常退休，历史Retain快照/Content及其历史类名保持。配置核验不能替代真实CSI新快照删除/restore，不能单独计为V20/V21或VM-04全面通过；实际结果仅见唯一执行状态。

当前整体Goal已RESUMED；公网问题DEFERRED_BY_USER，原FAIL/NOT_MEASURED保留且不再出网探测/网络修复，不因暂缓计PASS。其余验收继续；NoCloud账户初始化仍限已实测verified介质、指定UID/hash及用户输入凭据/公钥，不生成用户密钥。

## 1 证据层级与通过规则

- CODE：生成、编译、规则与受影响单元/契约检查。
- PG：真实隔离PostgreSQL中的事务、幂等、版本与恢复验证。
- BOUNDARY：受控原生API替身/故障注入，仅证明协议处理。
- LIVE：真实KubeVirt/CDI/CSI/KCN及存储结果。
- GUEST：真实Linux/Windows系统读写、安装、网络结果。
- BROWSER：真实noVNC画面及键鼠操作。

PASS只适用于实际执行的层级；NOT_RUN/FAIL不能被另一层PASS覆盖。源码证明某分支存在，不能证明现场已经通过。代码有变化、关联失败或剩余风险时补必要测试；未失效证据可复用，不为整理文档全量重跑。

每项记录：源码/镜像精确版本、环境及对象UID、输入/步骤/期望/实测、命令退出码、脱敏证据、清理。首次现场执行还需对应授权引用；票据、口令、私钥、kubeconfig内容不得进公开材料。

## 2 必需验收项

| ID | 场景与必须观察的断言 | 层级 | 归属卡 |
|---|---|---|---|
| V01 | 先只读固定172.16.101.10～12集群/节点身份及Block前置；旧环境缺项不直接套用。本轮runtime配置无具体批准不得写入；确需VM-00R时缺“用户确认空闲+具体后续授权”任一条件无写入/重启/探针。无需变更则保留当前事实并验证原生Block；独立代码工作继续。 | 文档/授权范围 | VM-00、VM-00R |
| V02 | 获授权后选定runtime配置在批准候选节点生效；CDI非root import/clone/blank到Block/RWO可用，最终PVC mode=Block；实际VM识盘读写，精确清理成功。不能只看配置文件或DV Accepted。 | LIVE/GUEST | VM-00R |
| V03 | VM业务Proto/窄console生成编译；module=17、usage、权限/scope一致，console无Resource运行时依赖。01A冻结Network/SG编号，不等后续实现；Network/SG实际生成与接线分别在01D/01E验证。旧tag/模块依赖不破坏。 | CODE | VM-01A/D/E |
| V04 | CPU容器、GPU容器、VM create/delete统一action表；同owner错action/schema/version拒绝；仅一个Resource adapter。已有支持的容器canonical原字节可重放，不开放停用版本；VM只认canonical.vm_contract_version，旧列占位不当其版本。 | CODE/PG | VM-01B |
| V05 | VM创建/删除真实PG持久受理，空items/charges无CPU/mem/disk账户变化；没有这些资源账户时，具备功能资格/权限且规格合法仍可受理，不保留余额准入；同键原意图同operation，修改expected_version等意图冲突；当前DB version变化不破坏原请求重放。进程退出/丢ACK重放无第二个VM/盘；Gov待投递可查且不误报404。按C03.3暂停DELETE投递后Gov持久受理即阻止新电源/盘/介质/快照恢复/console意图；原查询和合法同意图重放仍指向原operation/binding，恢复投递原DELETE继续且无新副作用；在途请求的expected_version冲突明确可查，不覆盖命令或长期排队冒充成功。 | PG/BOUNDARY，核心重试加LIVE | VM-01B/C/F |
| V06 | compute只解析一次并冻结；目录修改/禁用后旧Start/重放不重解析。1500m映射2vCPU但保留1500m request/limit；guest内存与launcher开销区分。盘仅正整数GB，20GB请求写20G，不写20Gi；实际容量允许向上分配。可选账户初始化遵守C03.1：仅实测NoCloud且绑定source UID/hash的verified Linux DISK_IMAGE；ISO/Windows/未验证目录拒绝，不能按OS名字标verified；用户名/密码/单行用户公钥边界及同键改变凭据冲突通过。 | CODE/PG/BOUNDARY/LIVE观察 | VM-01A/B/C/F |
| V07 | 根VM UID先登记，再独立DV owner；没有dataVolumeTemplates。共享源不被写入/加owner；来源同名换UID拒绝。WFFC停止态创建可结束、盘待消费者；首次Start不等待DV先Succeeded；目标仍Block。 | PG/BOUNDARY/LIVE | VM-01C/F、VM-02A |
| V08 | 真实公开API完成Linux系统镜像创建→临时VNC页浏览器登录键鼠→租户网→写根盘marker→Stop/Start保留→产品删除。已验证NoCloud镜像用公开账户初始化输入完成实际控制台密码登录，公钥正确落入目标账户，默认未启SSH密码登录；源UID/hash不变、无派生镜像。修改来宾账户状态后Stop/Start/Restart保持NoCloud身份且不重置；同VM恢复保持快照内账户状态，不重新用初始凭据覆盖；AEAD受理/ACK原子性和重放另以PG验证，读API/operation/日志/证据不回显密码或密文，最终专属Secret按UID清理。原生探针只可作为该介质能力证据，不能替代产品链。202/Ready/101均不能单独判PASS。 | PG/LIVE/GUEST/BROWSER | VM-01B/C/F、VM-04B |
| V09 | vm_managed/v3严格VM→VMI→launcher；旧v1/v2保持。错tenant/namespace/UID/owner/NAD摘要/Subnet拒绝；新合法但未登记launcher待观察；CDI/hotplug helper非当前端点。NAD依赖损坏仍可按冻结UID清理Stop/Delete。 | CODE/PG/BOUNDARY/LIVE | VM-01D/F、VM-03B |
| V10 | 两真实租户、至少两节点guest DHCP/DNS/出站/跨节点唯一marker HTTP；跨租户按现有规则拒绝。同一当前消费者没有新事件或读取失败时，不得仅按观察年龄或 TTL 撤回最后成功地址；成功读取的当前输出按字段合同更新，不因旧地址非空而永久保留。明确 stopped、closing、closed 或消费者更换时撤回旧输出；Stop 保留同一 Attachment 预留，Start 使用原子网并登记当前消费者，IP 可变；最终删除按消费者终结与 closed→Release→released 合同推进，无循环。地址是否存在不追加为受理、电源操作或释放门槛；本项仅约定 Network 地址，不改变 VM 对真实观察源断连的既有 Unknown 语义。 | LIVE/GUEST | VM-01F、VM-05A |
| V11 | Start/Stop/显式ForceStop原生语义；普通Stop宽限期后允许原生终止；ForceStop缺失/false确认拒绝且不得发原生强制调用。无VMI/launcher运行且旧地址资格撤销后停止完成，盘/Attachment保留；失联未知不伪造物理释放。 | BOUNDARY/LIVE/GUEST | VM-01E/F |
| V12 | Restart保存old_vmi_uid；丢回执/worker重启后观察同一次请求，不因旧UID暂存或stateChangeRequests已空盲重发。新UID属于原VM并达目标后完成；未知时保持原operation可查，无额外运行代次系统。 | PG/BOUNDARY/LIVE | VM-01E/F |
| V13 | noVNC真实画面/键鼠。无vm:console权限拒绝，重试也先当前授权；SG允许重放状态内同键同binding同会话、异binding冲突；不新建VM业务operation或递增version。篡改binding任一身份/version拒绝；开流前后VMI变化关闭错误流，不转发。旧票据在停开后拒绝，旧连接随VMI结束且名额释放。未有IP/Agent/安装完成或create仍待观测时仍可开可信console。无需Resource等待SG回执。 | CODE/BOUNDARY/LIVE/BROWSER | VM-01E/F |
| V14 | Linux从空白Block根盘经ISO/VNC安装；driver/ISO副本只读、租户内；SetMedia同键重放使用首次冻结来源、执行前源换UID拒绝；弹出/下次启动生效显示准确，模板更新不占住后续Start。切系统盘启动后仍可登录/读marker，最后副本清理；没有Installed伪状态。 | LIVE/GUEST/BROWSER | VM-02A |
| V15 | 固定Windows ISO/驱动版本完成识Block盘、NetKVM、键鼠、Agent与停开保盘。若OS要求TPM/UEFI，原生持久backend为Filesystem，停开及后续恢复状态通过；不关闭系统要求。安装/停开结果可先通过，恢复子项等04B。 | LIVE/GUEST/BROWSER | VM-02B、VM-04B |
| V16 | 至少两数据盘Block/RWO、稳定ID/serial；CreateDisk未挂载且WFFC可完成，后续冷Attach/Start不死锁。冷挂卸无VMI时只核持久配置；再挂原盘marker保留，单盘删除精确，跨VM引用拒绝。 | PG/BOUNDARY/LIVE/GUEST | VM-03A |
| V17 | Linux/Windows各两数据盘在线原生热挂卸，SCSI/hotpluggable与有效gate。持久模板及VMI volumeStatus共同满足；guest离线确认明确，marker重挂/重启保留；旧HotplugVolumes不会抢占声明式路径。 | LIVE/GUEST | VM-03B |
| V18 | 双系统在线/离线快照覆盖root+至少两块挂载持久data，保存逻辑disk inventory；无Agent/冻结不成功/必要卷排除如实展示，必要root/data及所需backend不完整或原生不可用时不能Restore，其余included/excluded如实展示。Snapshot 删除按自身原生对象和引用收尾；Content/CSI 结果可留作组件诊断，不作为 Res 终态门槛。 | CODE/BOUNDARY/LIVE/GUEST | VM-04A |
| V19 | 同根VM UID且VMI不存在恢复；运行态拒绝。预览绑定version+摘要，变化必须重预览。五种盘集合逐一覆盖：快照内仍挂、已卸、已删；快照外挂载新增；快照外已卸。恢复/重挂/删除/保留准确，不靠当前活盘表猜。 | PG/BOUNDARY/LIVE/GUEST | VM-04B |
| V20 | 原生Restore完成→新UID映射落库→旧root/data精确清理；任一checkpoint进程退出恢复仍是同一operation。cleanup失败保新映射及旧refs、不重复Restore/回写旧DV。WFFC PVC Pending可结束，无暗中Start；backend native清理不承诺失败回滚。 | PG/BOUNDARY/LIVE/GUEST | VM-04B |
| V21 | Linux/Windows在线与离线快照各至少一次restore→显式Start→各盘marker回到快照值；随后运行态数据盘摘挂仍可用，hotpluggable/serial未丢，不重建旧盘。必要TPM状态纳入同一用例。 | LIVE/GUEST | VM-04B |
| V22 | 导入中、停止态、热插拔后、恢复后/cleanup中五类删除：未知结果查明，根/运行对象、当前/卸载/被替换盘、介质、backend、Snapshot/Restore 及 Network 管理关系完成；不等 PV/CSI/Content 物理清场。共享源/NAD/其他租户不变；无finalizer强删、无空charge退款前置。 | PG/BOUNDARY/LIVE | VM-01F、VM-02/03/04、VM-05A |
| V23 | 当前授权、跨租户VM/盘/快照/会话、同名换UID、伪造namespace/owner拒绝；恢复破坏确认不能覆盖版本冲突；通用权限/规格校验不削弱、不新增VM专属GPU禁止策略。业务前端的GPU筛选交其owner，本轮提供正式API合同。 | CODE/PG/BOUNDARY，关键路径LIVE | 随功能、VM-05A |
| V24 | 受影响旧compute三用途、容器CPU/GPU受理及配额、Inference分派、Network v1/v2、SG Instance terminal回归。新增字段空值不改旧指纹，旧消费者无需迁移。不要求无关全量回归。 | CODE/PG/BOUNDARY，受影响路径LIVE | VM-01B/D/E、VM-05A |
| V25 | 正式依赖ref可获取，无临时replace/go.work；增量迁移/兼容部署/worker启动/开放入口顺序有证据。发布前准备与实际发布分开；回退版本理解新VM合同/数据，未获授权不实施发布。 | CODE/发布记录（获授权后） | VM-05B |

## 3 双系统最小现场矩阵

同一已固定候选与物料执行；保持原一期范围，不要求额外发行版扩展。

| 路径 | Linux | Windows |
|---|---|---|
| 预置系统镜像启动 | 必须 | 不额外强制制作Windows模板 |
| ISO安装、弹出后磁盘启动 | 必须 | 必须 |
| 浏览器VNC与guest租户网络 | 必须 | 必须 |
| Start/Stop/Restart/ForceStop、根盘保留 | 必须 | 必须 |
| 两块数据盘冷/热挂卸与再挂 | 必须 | 必须 |
| 在线快照→停机恢复→启动读回 | 必须 | 必须 |
| 离线快照→恢复→启动读回 | 必须 | 必须 |
| 恢复后的数据盘在线摘挂 | 必须 | 必须 |
| TPM/UEFI持久状态 | 仅所选OS要求时 | 仅所选OS要求时 |

“无Agent”与必要卷被排除等负面展示可用最小代表用例验证；Windows有Agent的实际安装/工作仍须独立验证。不因场景共享而把未执行OS状态写为PASS。

## 4 已知环境缺项登记

- ENV-BLOCK：任务包中的缺项来自此前环境；10～12目标当前有效配置/能力须重新核验，确有缺项才阻塞依赖路径并等待具体维护批准。当前结果只记唯一状态。
- ENV-MEDIA：真实Linux/Windows源、校验值及驱动版本待VM-00冻结；不在制包时随意下载替换。
- ENV-NETWORK：参与租户受管NAD及真实子网身份需VM-00核验，缺项只阻断现场依赖。
- ENV-SNAPSHOT / ENV-HOTPLUG / ENV-TPM：实际capability待核验；不能因缺一项阻断不依赖它的代码或Windows基本安装。
- INPUT-FRONTEND：当前材料未可靠定位要修改的前端仓库/目录，VM-00从实际工作区和owner输入固定；不假设不存在、不新建替代项目。

本期完成条件为必需矩阵通过、清理闭环和兼容结果齐备。任何必需格子FAIL/NOT_RUN均保留实际断点，不能以文档复核通过抵消。
