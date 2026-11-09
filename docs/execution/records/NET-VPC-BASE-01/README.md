# NET-VPC-BASE-01 实施与受控验收记录

2026-11-09 引用维护：本记录保留原时点的实施与验收结论。标为“历史定位”或“仓外定位”的原路径在当前 checkout 无法解析，仅保留追溯信息，证据可用性为 not_verified；不表示归档已恢复或旧规则仍适用。处理范围见[文档修复记录](../res-doc-fix-20261109.md)。

日期：2026-09-14。当前执行结论只维护在[执行状态](../../status.md)。本记录固定本批实现、实际命令与分层证据；业务规则以[规格](../../../specs/vpc-connectivity-lb.md)和 [ADR-0005](../../../adr/0005-separate-vpc-connectivity-and-exclusive-eip-bindings.md)为准。

## 固定候选与权限范围

实现工作树为 `/home/chabking/workspace/.worktrees/network-vpc-base-01`，分支 `codex/net-vpc-base-01`，HEAD 为 `d8835a22d905e358b7f60756d3113baa97d7c762`。成果保持未提交。14 项设计输入和原始哈希见 [inputs.json](inputs.json)，任务范围见[当前 Goal](goal-objective-updated.md)；[原始 Goal](goal-objective.md)及[更新差异](goal-update.json)保留，新附件仅再次强调重任务在 ubuntu。三个权威设计文件保持用户固定哈希。

本批只实施 NET-U00/U01/U02/U03/U04/U08。在本任务工作树编辑，在 ubuntu 独立源码快照、临时 PostgreSQL 和受控 Provider API 中验证；没有真实存量补齐、共享 Kubernetes 操作、kc/ANI/Envoy 代码修改、提交、推送、合并、发布或部署。LB 只有完整 Proto 和最低数据库基础，服务未注册。

## 六张任务卡的实现落点

| 卡 | 实际交付 | 主要源码及验收依据 |
|---|---|---|
| U00 | 基础连接摘要、系统资源边界、binding_target 兼容投影、Intranet 管理接口、完整未来 LB 契约与 operation；固定生成和基线 breaking 检查 | [契约记录](contracts.md)、[network.proto](../../../../api/network/v1/network.proto)、[egress.proto](../../../../api/network/v1/egress.proto)、[load_balancer.proto](../../../../api/network/v1/load_balancer.proto)、[契约门禁](../../../../scripts/check-vpc-base-contracts) |
| U01 | 仅追加 0006；scope/双默认池、系统归属、SNAT purpose、typed EIP claim、LB 最低身份/父关系及 VIP 意图、基础步骤、补齐与开关；可靠旧 Public 迁移及失败回滚 | [0006](../../../../migrations/0006_vpc_base_connectivity.sql)、schema 真 PG 测试（历史定位：`../../../../internal/data/base_schema_integration_test.go`） |
| U02 | 管理员 Intranet 池 CRUD/验证/分配/默认；独立基础/Public/LB 能力；实际 kc Intranet adapter、默认 VPC UID、配置/ServiceCIDR/镜像合同、持续观察和 RBAC | 平台用例（历史定位：`../../../../internal/biz/platform_intranet.go`）、能力查询（历史定位：`../../../../internal/data/platform_capabilities.go`）、adapter（历史定位：`../../../../internal/data/kc_intranet.go`）、[配置前提](../../../../deployments/egress/intranet.md)、[只读 Provider 来源](platform-provider-inputs.json) |
| U03 | VPC 同事务固定池版本/子 ID/placement，VPC→EIP→SNAT→聚合；同一持久 worker 执行和恢复、持续退化/恢复、系统资源隐藏、终止/删除及未知写入 fencing | 基础生命周期（历史定位：`../../../../internal/data/base_connectivity.go`）、worker（历史定位：`../../../../internal/data/worker.go`）、实际 adapter 生命周期（历史定位：`../../../../internal/data/base_connectivity_integration_test.go`）、进程故障（历史定位：`../../../../internal/data/base_process_integration_test.go`）、双 worker（历史定位：`../../../../internal/data/base_concurrency_integration_test.go`） |
| U04 | Public purpose 筛选，公共 claim 原子受理，绑定/启用基础就绪门禁，停用/解绑可恢复；EIP claim 的新旧字段同源投影；猜系统 ID 拒绝 | Public 数据用例（历史定位：`../../../../internal/data/egress.go`）、Public 隔离回归（历史定位：`../../../../internal/data/egress_public_integration_test.go`）、wire 兼容测试（历史定位：`../../../../internal/service/egress_public_test.go`） |
| U08 | paused 审核计划及固定 SHA、候选/冲突/排除、固定池版本、独立 ensure、双 dispatcher 限速、暂停/恢复、删除互斥和单独新建/legacy 聚合开关 | CLI（历史定位：`../../../../cmd/ani-network-service/base_connectivity.go`）、持久实现（历史定位：`../../../../internal/data/base_backfill.go`）、[操作及失败处置](backfill.md)、PG/CLI 进程演练（历史定位：`../../../../internal/data/base_backfill_integration_test.go`） |

运行实现继续使用既有 biz/data/service/server 分层、composition root、持久 worker 和 observation 框架。平台 scope 是内部现有平台资源的扩展；没有第二套执行框架。`go.mod` 仅将既有 `sigs.k8s.io/yaml v1.6.0` 从 indirect 移为 direct，供 Intranet 配置严格解析使用；没有依赖版本升级。`go.sum`、固定工具版本及历史迁移 0001—0005 保持基线。

## 生命周期与迁移中的具体保证

首次受理与固定子资源/claim/operation 同一事务提交；Provider IO 在事务外执行。`create_dispatched` 在发起 create 前持久化，历史 binding 迁移为 true，避免凭缺失 UID 推定从未发送。未知结果保留 mutation 和占用，按固定身份核验；只有明确从未发送的系统步骤可直接取消，并将其 reconciliation 标记 retired。普通墓碑的观察语义保持。

基础 SNAT 按父 VPC 的 Provider 就绪事实受理执行，不等待产品 available。父产品状态使用 EIP/SNAT 当前、同代、同 UID、启用及 bound claim 聚合；配置、身份和时效异常可持续退化并自动恢复。GET/List 只读取持久投影。系统子 operation 不向租户按 ID 暴露。创建中终止保留原创建失败结论；已成功的历史 operation 不被当前观察重写。

删除 VPC 先封闭新子步骤，再清理系统 SNAT、EIP，最后删除 VPC；用户 Subnet/Attachment/Public SNAT/LB 继续构成删除依赖。SNAT 的停止、删除中及结果未知都不释放用途槽位或 EIP claim。共享 claim 在 Provider 已确认解绑/删除后才释放。

0006 在事务中验证旧 Public provenance；来源不明会带资源身份报错并整体回滚，不按名称猜测归属。typed FK 同时保持 tenant、cluster、namespace 及父关系。真实两事务 claim 测试通过数据库等待证明竞争，分别验证 SNAT/LB 两种先到顺序；DB fixture 不是 LB 产品受理实现。

## 验收编号分层矩阵

以下是本批相关子项；实际结果以[门禁矩阵](gates.json)和命名测试为准，未执行层不计 pass。

| 编号 | 数据库 | 受控 Provider / 用例 | 实际进程恢复 | 真实数据面及延后项 |
|---|---|---|---|---|
| U-V01 | 默认池缺失零孤儿受理；池/子身份固定 | 实际 adapter 顺序、无 available 循环、平台能力独立 | VPC/EIP/SNAT 三步骤正常链及恢复 | 内网真实流量 not_verified |
| U-V02 | claim 与持久 mutation，未发送可取消 | UID/代次/引用合同、固定地址身份 | 九个故障/终止场景，见下表 | 真实控制器故障/网络恢复 not_verified |
| U-V03 | 同 VPC 一内一公、第二条同用途拒绝、租户 FK | Public API 只取 Public，系统 ID 不可见 | 本批服务恢复链覆盖，非独立全量隔离验收 | 实际流量隔离 not_verified |
| U-V04 | SNAT/LB 同 EIP 原子竞争仅一方成功，VIP 与 typed FK | 错误 namespace/UID/跨租户拒绝，LB claim 兼容投影 | 本批 SNAT 未知结果的持久保留 | LB 受理/生命周期/真实竞争 not_verified |
| U-V05 | Public 操作保持基础 ID/地址/UID/期望快照 | Public 绑定/启用检查新鲜基础依赖；停用可继续 | 既有 Public 恢复回归 | 实际内网不受影响、Public 出站/源地址 not_verified |
| U-V09 | 用户依赖阻止 VPC 删除；系统资源有序清理 | 实际 adapter 真实解绑事实后释放 claim | SNAT/EIP/VPC DELETE 响应丢失、部分创建终止 | LB 删除保留 EIP、真实回收流量 not_verified |
| U-V10 | 0005 升级保留 Public/operation/幂等；plan 固定 SHA、限速、暂停、删除竞争 | ensure 使用既有 worker，不收养变更身份 | CLI 独立进程重启，持久计划只受理一次 | 真实存量补齐及旧工作负载不中断 not_verified |
| U-V11 | 观察持久唤醒、freshness/claim 门禁、退休取消步骤 | 默认 VPC/池/子 UID/代次/配置变化退化与恢复，GET/List 纯读 | 本批重启恢复及既有 observation 回归 | LB 观察、真实 Informer/控制器故障组合 not_verified |

U-V06/07/08/12 的 LB 行为保持 not_verified。NET-U07 全卡和 U09 均未启动，本批必要故障与观察测试不将它们标为完成。

### 基础连接九个实际服务进程场景

| 场景 | 必须观察到的行为 |
|---|---|
| vpc_POST_success_response_lost | 原 VPC 写入成功后响应丢失，进程重启按原 identity 继续，无重复分配 |
| eip_POST_success_response_lost | 原 EIP UID/地址及 claim 保持，重启继续 SNAT |
| snat_POST_success_response_lost | 原 SNAT 不重复创建，bound 后基础聚合完成 |
| eip_POST_accepted_success_after_process_kill | 进程退出后原请求迟到成功；重启核验原结果 |
| snats_DELETE_success_response_lost | 未确认前保留 claim，重启核验解绑并继续 EIP |
| eips_DELETE_success_response_lost | 重启确认原 EIP 消失后继续父删除 |
| vpcs_DELETE_success_response_lost | 重启核验父 CR 已消失并完成原删除 operation |
| termination_before_any_CR_process_recovery | 父/子 CR 均未发送时终止，零 Provider 创建，取消步骤停止调度 |
| partial_unknown_EIP_termination_before_late_success | EIP 结果未知期间删除父，迟到成功后重启清理，SNAT 从未发送，claim 不提前释放 |

这些测试使用实际生产二进制、独立进程、真实持久 PG 和真实 kc adapter，对受控 HTTP API 施加失败。请求数、资源 ID/UID、claim 和 operation 结论均有断言；API fixture 不模拟 OVN 数据面。

## 远端门禁和证据

最终 run `20260914T135236Z-18dc5c44` 的 `make verify`、固定基线 breaking、定向两项 race 和全量真实 PG race 均为 **pass / exit 0**。全量完成 115 个顶层测试、259 个含子场景测试结果；9 个基础服务进程故障场景通过。唯一 skip 为既有可选 `TestNET05ACapacity`，不在本批必需范围。上述矩阵中本批数据库与受控 Provider 子项为 pass；进程层只以实际命名测试为准，U-V03/04 没有单独进程隔离验收，保持 not_verified。所有真实数据面/LB 延后项仍为 not_verified。

- [最终门禁、逐项矩阵和源码覆盖](gates.json)：212 个运行/测试/迁移/契约/配置/脚本文件与最终远端快照逐文件相同。
- [最终源码快照](runs/20260914T135236Z-18dc5c44/snapshot.json)、[命令日志](runs/20260914T135236Z-18dc5c44/command.txt)、[测试逐项结果](runs/20260914T135236Z-18dc5c44/test-results.json)、[退出结果](runs/20260914T135236Z-18dc5c44/result.json)。
- [全部 run 记录](runs.json)：保留早期 SQL/编译/fixture/依赖声明失败、修复后的重复验证和每个 archive/manifest SHA。早期 run 未完整打印版本的记录限制单独注明；最终 run 完整记录工具及 pinned binary 来源检查。
- [未提交成果完整 manifest](source-manifest.json)：最终本地候选，包括正式证据；仅为避免自引用而排除本 manifest 本身。远端源码归档保留在对应 run，未将临时数据库/凭据或 kubeconfig 归档。
- [输入、历史迁移和其他工作树末态检查](candidate-audit.json)、[文档链接与 anchor](document-checks.json)、[任务环境清理复核](cleanup.json)。

收尾发现并修复 Intranet operation 缺少枚举映射及池锁等待后的时效校验；追加真实服务查询和 PG 锁等待回归。锁等待测试先因跨角色 `pg_stat_activity` 可见性失败，改用真实阻塞锁关系，最终通过。首次 SNAT Pending 的组合测试同时断言原 create 不结束、claim 不释放、同 UID/代次恢复成功。不能用早期全量 pass 替代包含这些修复的最终门禁。

所有重任务都在 ubuntu 串行执行，显式加载 `/home/ubuntu/.local/share/ani-network-service/env.sh`，共享 `net05a-heavy.lock`，CPUQuota=200%、MemoryMax=2300M、MemorySwapMax=0、GOMAXPROCS=2、GOFLAGS=-p=2。Go 1.26.7、Buf 1.60.0、sqlc 1.31.1；临时 PostgreSQL 18.6 使用固定镜像 digest、768 MiB/1 CPU。不存在本地重任务回退。

生成物通过远端 pinned Buf/sqlc 回传任务临时目录，按原快照逐文件核验本地未漂移后应用。最终 verify-source 再次生成并要求整棵源码一致。本批生成与测试支持变更只涉及增加 ServiceCIDR 受控观察端点、新的合同基线检查和远端工具版本记录，没有修改共享测试服务或外部环境。

## 第二批直接消费的输入与阻塞条件

1. 在本候选成果上继续 U05/U06/U07，不仅检出 HEAD；未提交文件也是必要输入。使用最终源码 manifest 对齐后，消费 U00 LB Proto、0006 typed claim/LB/VIP 表、父依赖锁和 observation/worker 机制。后续 migration 继续追加，不重写 0006。
2. LB 产品受理需要实现新增 service 注册、权限/租户映射、幂等/版本、三类曝光与同 VPC 后端真实身份；不能只凭 CIDR 接受后端。U06 消费 claim 时必须识别合法 LB Service，继续拒绝外来 Nat/Snat/Service；受理、执行、删除共用原有占用与父删除互斥。
3. [Intranet 前提](../../../../deployments/egress/intranet.md)要求固定 default VPC UID、Service/DNS/xDS 目标范围、kcn-config、控制器参数、运行镜像及适用验证证据。新建和 legacy 开关默认关闭，开启顺序与审核流程见[补齐手册](backfill.md)。本候选没有为真实环境写入这些事实或开关。
4. Envoy Gateway、GatewayClass 私网 noeip 映射、附件 install 参数和后端策略先重新固定当前来源/镜像/拓扑。历史手工实验只作输入，不能证明未来 LB 产品契约或真实入口可用。没有 LB 健康事实来源时 data_plane_state 必须 unknown。
5. U09 需要独立授权的真实测试环境、合格 kc 固定源码与实际运行镜像关联、原生 Overlay 已知缺陷闭环、两 worker/两租户与真正内网/Public 探针；Public 外部源地址和 LB 外部入口不能用 port-forward/NodePort 替代。历史 kc 缺陷未在本批修复或重新证明消除。
6. 调用方必须从 binding_state/binding_target 判断占用；旧 binding_id 在 LB 场景可空。ANI/OpenAPI/客户端及真实 IAM 接入仍需独立范围。正式升级、切换、部署和真实补齐均保持独立授权与验收。
