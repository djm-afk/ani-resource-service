# VPC SNAT 本仓实现与验证记录

2026-11-09 引用维护：本记录保留原时点的实施与验收结论。标为“历史定位”或“仓外定位”的原路径在当前 checkout 无法解析，仅保留追溯信息，证据可用性为 not_verified；不表示归档已恢复或旧规则仍适用。处理范围见[文档修复记录](../res-doc-fix-20261109.md)。

记录日期：2026-09-10；当前状态只在 [执行状态](../../status.md) 维护。本记录冻结本次实现与实际运行证据。

Network 的平台出口、租户 EIP/SNAT、持久事务和持续 worker 已实现，最终远端 `make verify` 与真实 PostgreSQL 全量 race 测试均 `pass`。原生 Overlay 数据面为 `not_verified`：固定 kc 源码仍有已知缺陷，修复版本及源码到运行镜像的关联没有合格证据。按用户“你不用修复kc-networking的bug,标注就行了”的要求，仅标注，不修改或升级 kc；本记录不宣称完整 Goal 完成。

## 1. 固定输入与交付位置

- [完整 Goal 及用户补充](goal-objective.md)。新 worktree：`/home/chabking/workspace/.worktrees/network-vpc-snat-implementation`，分支 `codex/vpc-snat-implementation`，HEAD 保持 `e481e968d3cc2f17bc4c6a736c438428519b09a0`，全部成果未提交。
- 设计输入来自未提交的独立 design worktree；[595 项文件清单和 SHA-256](design-input-manifest.json)明确记录来源。正式规格、计划和手册在实现 worktree 中演进，原设计树及导入的历史执行证据保持原字节。
- kc 固定源码 `a2245883eb2b46a998f041feb3ad0ed3f6cf7c60` 只读。[初始预检](provider-preflight.json)与[结束预检](provider-final-preflight.json)分别记录实际节点、命名空间、provider Pod UID 和 imageID，不能把镜像标签当作修复证明。
- 没有修改 ANI、共享 Network checkout、kc、CNI、宿主防火墙或现有 VM；没有提交、推送、PR、tag、镜像发布或真实网卡接管。

## 2. 已交付实现

契约以 [egress.proto](../../../../api/network/v1/egress.proto) 为准，字段及生命周期以 [正式规格](../../../specs/vpc-snat.md) 为准，使用顺序见 [操作手册第 0 节](../../../kc-public-egress-manual.md#0-network-产品接口操作顺序)。此处只记录实现落点，不复制第二份契约表。

| 层次 | 实现落点与已验证行为 |
|---|---|
| 领域与入口 | 领域用例（历史定位：`../../../../internal/biz/egress.go`）、gRPC 适配（历史定位：`../../../../internal/service/egress.go`）、组合根（历史定位：`../../../../cmd/ani-network-service/app.go`）：平台管理、租户申请/绑定/启停/解绑/释放、查询与 operation；可信调用上下文及显式代操作授权。没有可信上下文时新增 RPC 拒绝访问。 |
| 平台资源 | 平台事务（历史定位：`../../../../internal/data/platform.go`）：节点事实与设备接管、VLAN 0–4094、Public/Host EIPGateway、Overlay/Underlay 池、分配开关、默认池、版本条件和删除依赖；验收记录绑定拓扑 fingerprint、Provider UID、实际镜像 digest 及有效期。 |
| 租户持久化 | 租户事务（历史定位：`../../../../internal/data/egress.go`）、[增量迁移 0005](../../../../migrations/0005_vpc_egress.sql)：显式 tenant、持久 namespace、同 tenant/cluster/namespace 复合 FK，永久幂等、固定选池和未删除 binding 唯一占用。先本地事务受理，再执行外部动作。既有迁移不改写。 |
| Provider | kc adapter（历史定位：`../../../../internal/data/kc_egress.go`）：实际 CR 字段、稳定产品 ID 名称、UID/RV 保护、同 namespace 引用、kc 分配的 spec.ipAddress、地址/排除区间、boundResource/generation/disabled/nodeName 和 Nat/Service 冲突。Overlay 不写 underlayConfig，Underlay 明确 VLAN/物理网关。 |
| 持久执行与观察 | 原 worker（历史定位：`../../../../internal/biz/worker.go`）、出网 work（历史定位：`../../../../internal/biz/egress_worker.go`）、资源执行（历史定位：`../../../../internal/data/resource_work.go`）、平台执行（历史定位：`../../../../internal/data/platform_worker.go`）、观察调度（历史定位：`../../../../internal/data/kc_observation.go`）：沿用 Shared Informer→关系索引→PG 唤醒→领域 worker；新增资源、依赖扇出、周期审计、租约/fencing、公平处理、未知结果恢复及 tombstone。 |
| 事实与运行接线 | 节点事实（历史定位：`../../../../internal/data/kc_node_facts.go`）、只读采集进程（历史定位：`../../../../internal/data/node_facts_collector.go`）、[部署前置](../../../../deployments/egress/README.md)：真实接口命令来源，原始采集时间与完整读取证明分开；部分节点进度持久化，旧事实退化传播至 VLAN/池/EIP/SNAT。追加最小所需 RBAC、关闭流程及现有有界指标/结构化日志。 |

`desired_enabled` 与 nullable `applied_enabled` 分开；应用代次不足、旧事实或依赖失败不会制造已应用状态。控制面 available/Bound 不等于互联网可达。物理事实进程的编译、只读命令和 UID fencing 有自动测试，实际部署及 OVS socket 环境仍 `not_verified`。

## 3. 最终必要门禁

所有编译、生成、完整测试及 PG 均在 SSH `ubuntu` 实际主机 `i-8yg2l7u8` 串行执行；本机只编辑、读源码、gofmt 和轻量静态检查。工具为 Go 1.26.7、Buf 1.60.0、sqlc 1.31.1。PostgreSQL 使用固定 18.6 镜像 `docker.io/library/postgres@sha256:4ef4dbc939d61acea57712655ddb4b4ab27419c913f94cca0cd57cb3ea3c2280`，每次独占容器、每个测试独立数据库。

| 检查 | 结果 | 精确运行证据 |
|---|---|---|
| 全量 PostgreSQL、race、既有 VPC/Subnet/Attachment 回归 | `pass` | `20260910T144842Z-acf48246`，`scripts/integration -p 2 -race ./...`；[输出](runs/20260910T144842Z-acf48246/command.txt)、[输入清单](runs/20260910T144842Z-acf48246/snapshot.json)、[退出码](runs/20260910T144842Z-acf48246/command.exit)。data 包 106.017s，exit 0。 |
| 最终生成一致性/分层、tidy、单元测试、vet、build、module 校验 | `pass` | `20260910T145133Z-96a527b1`，`make verify`；[输出](runs/20260910T145133Z-96a527b1/command.txt)、[输入清单](runs/20260910T145133Z-96a527b1/snapshot.json)、[退出码](runs/20260910T145133Z-96a527b1/command.exit)。此命令自身不提供 PG 证明，PG 见上一行。 |
| 新增真实 PG、受控 gRPC、实际 kc adapter 与设备部分进度 | `pass` | `20260910T144134Z-71f8a5eb` 的 [定向输出](runs/20260910T144134Z-71f8a5eb/command.txt)，后由全量 race 门禁覆盖。 |
| 实际 Network 进程 kill/restart、已提交 POST/DELETE 丢回复、迟到 tombstone | `pass` | 进程测试源码（历史定位：`../../../../internal/data/egress_process_integration_test.go`），最终全量 PG/race 运行；编译并启动实际 main，通过受控 Kubernetes HTTP 依赖施加故障，恢复同一持久 DB。不是实际 kc 数据面。 |
| 历史迁移升级与永久事实保留 | `pass` | 最终全量 PG/race 包含原 NET-01/NET-05A checksum 升级断言，仅从历史 JSON 投影排除新增 nullable eip_id/snat_id；未减弱原租约、历史、幂等及 checksum 断言。 |
| 原生 Overlay、双 worker 内网/DNS/HTTPS、EIP 源转换和回程 | `not_verified` | Provider 外部前提不满足，本轮没有 live EIP/Snat 或手工 OVN 修补。 |
| Underlay 合同及受控生命周期 | `pass` | VLAN 0、非零 VLAN 合同与非法组合、排除物理网关、部分节点接管、共享 ConfigMap CAS、资源占用与退役、陈旧事实退化；最终全量 PG/race 覆盖。 |
| Underlay 真实物理口、OVS/VLAN/交换机及出网 | `not_verified` | 按约定延期；[后续现场输入](../../../plans/vpc-snat.md#4-underlay-后续输入)。 |
| ANI Gateway/OpenAPI、真实 IAM 入口、部署/生产切换 | `not_verified` | 不在本轮；新增 RPC 当前默认拒绝未提供可信调用上下文的访问，受控 interceptor 不算 IAM 证明。 |

最终全量 PG/race 的 source manifest SHA-256：`c6646abb302e1793f5df2d2e394f4858db42c764a1cc1e72a8b15fa70ab40f01`；最终 make verify：`7f42813c2dded9547cbc1d6bfc029ab2ac1dd48f35529fab5ae9a820f3529e57`。二者业务源码、生成物和测试一致；后续只增加记录/静态审计脚本。最终静态审计核对该对应关系，不用固定 HEAD 冒充这些未提交修改的身份。

运行器使用 GOMAXPROCS=2、GOFLAGS=-p=2、CPUQuota=200%、MemoryMax=2300M、禁用任务 swap、独占重任务锁及资源 guard。两次最终门禁的峰值 host load1 分别约 3.06 / 2.40，最低可用内存分别约 3696 / 3976 MiB，未触发 guard。原始采样在各 run 的 `resources.jsonl`；这些数值只说明本次门禁资源记录，不作容量结论。

## 4. 自动测试覆盖与证据范围

| 验收合同 | 自动证据 | 限制 |
|---|---|---|
| SNAT-V01/V02 | 设备实际 adapter 测试（历史定位：`../../../../internal/data/egress_device_integration_test.go`）、领域校验（历史定位：`../../../../internal/biz/egress_test.go`）、gRPC 授权（历史定位：`../../../../internal/data/egress_grpc_integration_test.go`）：不可选原因、节点 UID、部分失败、共享配置保护、普通租户/管理员代操作 | 受控事实与可信上下文注入；真实物理/IAM 不包含在 pass 中。 |
| SNAT-V03/V04/V05 | 真实 PG 生命周期与并发（历史定位：`../../../../internal/data/egress_integration_test.go`）、实际 kc 字段（历史定位：`../../../../internal/data/kc_egress_test.go`）、共享观察生命周期（历史定位：`../../../../internal/data/egress_kc_integration_test.go`）：固定池/关分配、持久重放、跨 tenant/namespace FK、并发争用、失败/未知/停用占用、旧 binding ID、完整启停解绑释放 | kc 依赖为受控 HTTP/动态 client；真实地址池耗尽与数据面不由该结果代替。 |
| SNAT-V10/V11 | 同一 worker 的失联/旧事实/状态代次、依赖退化、UID 替换、未知 create/update/delete、租约过期、实际进程重启、删除确认/tombstone；原观察公平调度、旧缓存、风暴/backoff 和 VPC/Subnet/Attachment 测试同时回归 | 本轮没有 live 清理工作流；仅清理受控测试实例，外部环境作只读复核。 |
| SNAT-V06/V07/V08 | 无本轮实际数据面执行 | `not_verified`，需要合格 Provider 后另行恢复该阶段。历史手改路由对照不采作 pass。 |
| SNAT-V09 | Underlay 自动合同与设备/平台受控生命周期通过 | 物理验收 `not_verified`，按计划延后。 |

## 5. 保留失败与修正

[全部 26 次运行索引](runs/index.json)保留 18 次 pass、8 次开发过程中 fail，所有原始退出码均保留。最后两项必要门禁均通过；没有将早期失败日志改成通过。

| 失败 run | 原因及处理 |
|---|---|
| `134228Z-5bde6bb5` / `140102Z-248668b4` / `140858Z-56a24cd4` | 分别是缺少 netip import、服务文件插入语法和 sqlc GetVPCRow 字段路径编译错误；修正后生成与编译通过。 |
| `141326Z-24ea957e` | 测试期待了错误的 VPC 删除保护 reason；实现按 SNAT 依赖返回 VPC_SNAT_EXISTS，修正断言。 |
| `141632Z-319b1c10` | client-go dynamic fake 引入测试依赖后需 tidy；在 ubuntu 执行并返回 go.mod/go.sum，最终 tidy-diff 通过。 |
| `142241Z-72b3358f` | 实际 kc 的空 boundResource 可编码为 JSON null；修正 adapter 将 absent/null/empty map 一致识别为空。 |
| `142417Z-756edd96` | 停用后 EIP 持续观察尚未恢复 available，测试过早重新启用；等待真实观测条件，不放宽 enable 的新鲜就绪约束。 |
| `143025Z-c3699866` | 原进程测试的 controlledKC fixture 未列出新增观察 GVR 的空 LIST；补齐该 fixture，保留原 403 Watch 负例；定向及最终全量 race 回归通过。 |

结束预检脚本首次使用可选输出名时发生局部变量覆盖，未生成结果文件；修正后重放只读查询并保存 `provider-final-preflight.json`。该脚本问题不涉及运行时代码或集群写入。

## 6. kc 外部阻塞

以下均由固定本地源码直接核对；[预检](provider-final-preflight.json)保存三文件哈希。没有修改 kc、重新构建 kc 或升级运行镜像。

| 缺陷 | 固定源码位置 | 影响与放行所缺证据 |
|---|---|---|
| 新网关缓存遗漏 serviceIP | `internal/controller/handlers/eip_handler.go:353–371` 的 LoadGatewaySubnet；`:1305–1321` 的 addEIPRoutesOnER | 热加载创建 gwState 未填 serviceIP，后续拿该值作 nexthop。需要新建网关、启动加载、disable→enable 的修复回归及无手改 OVN 的真实链路。 |
| EIP 候选按裸名称跨 namespace 选择 | `internal/controller/networking/eip_controller.go:296` 的 selectBoundObj | Nat/Snat/Service 候选检索缺少同 namespace 限制；需要跨 namespace 同名负例。Network 自身的引用/UID/租户防护不能替代 kc 修复证明。 |
| Snat→VPC 同 namespace 限制不足 | `internal/controller/networking/snat_controller.go:163–171` | ParseNameSplit 接受 namespace/name 后读取 VPC，没有约束与 Snat namespace 相等；需要越 namespace 引用拒绝回归。 |

结束预检中的运行 kc 镜像为 v0.6.2，实际 imageID digest `sha256:e2efcb27fd9b982ad6f49c0dc7b8d7ab9622bfa1172491c187152422cca27749`。修复验收及此镜像到修复源码的关联均 `not_verified`。原 [2026-09-10 Overlay 失败/手改对照](../KC-OVERLAY-20260910T114200Z/README.md)原样保留。后续只恢复 [Overlay 产品复测入口](../../../plans/vpc-snat.md#3-overlay-复测入口)，不由本记录授权 kc 修改、升级或发布。

## 7. 清理、环境与复现

[结束环境核对](final-environment.json)记录全部本任务 PG 容器已不存在、历史 13 类资源 UID 清单保持、现有 VM Ready/VMI Running，以及 kcn-config 与节点网络只读比较。[节点网络逐项差异](node-network-differences.json)保留原始比较：节点路由相同，NAT 差异仅为 KUBE-SERVICES 的相同规则重新排序，未发现增删规则；不能把字节差异隐藏成环境完全一致。没有创建 live Pod/EIP/Snat/平台资源、临时 NAT 规则或物理接管；没有需要撤销的本轮集群写入。共享 OVN 设施未删除，也没有在本轮重新做完整 OVN 数据面审计。

[进程清理复核](process-cleanup.json)未发现遗留的本轮测试服务进程。

每次远端源码快照、测试构建物和日志保留在该 run 的 `/home/ubuntu/workspace/ani-network-service-runs/snat-<run>/`，用于复核；共享镜像/依赖缓存保留。正式记录只归档源码清单、命令、输出、退出码和资源采样，不复制凭据、数据库数据或生成的服务可执行文件。

在实现 worktree 中复现自动门禁（串行）：

```bash
scripts/snat-remote -- scripts/integration -p 2 -race ./...
scripts/snat-remote -- make verify
python3 docs/execution/records/VPC-SNAT-IMPLEMENTATION/collect-evidence.py
```

只读 Provider 复核须用新的输出文件名，旧证据不覆盖：

```bash
python3 docs/execution/records/VPC-SNAT-IMPLEMENTATION/preflight.py --output provider-recheck-NEW-RUN.json
```

实际远程命令由各 run 的 `run.sh` 记录。[采集脚本](collect-evidence.py)只读取本任务目录并拒绝改写已有单次记录；[环境脚本](final-environment.py)只读取资源，拒绝覆盖已有结果。源码、原设计/迁移保留及文档链接的轻量检查见 [静态审计](static-audit.json)。
