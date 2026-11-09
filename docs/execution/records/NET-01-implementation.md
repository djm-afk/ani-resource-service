# NET-01 实施记录

2026-11-09 引用维护：本记录保留原时点的实施与验收结论。标为“历史定位”或“仓外定位”的原路径在当前 checkout 无法解析，仅保留追溯信息，证据可用性为 not_verified；不表示归档已恢复或旧规则仍适用。处理范围见[文档修复记录](res-doc-fix-20261109.md)。

## 固定输入与授权

Goal 于 2026-09-09 启动；授权只实施 VPC、真实 PostgreSQL、持久操作、kc VPC adapter 与受控环境验证，不包括 NET-02、kind、IAM、兄弟仓库写入或发布。

- 初始 HEAD：`f8d44daaff6dc1bbe2ed9960ce43b99045550583`。
- 初始 52 文件清单 SHA-256：`b590f9163f08ca63921d463c8897ce214e16d890bb81f8ba4a61783adc243916`，与 Goal 一致；完整输入保存在 `.work/net01-baseline/`。
- kc API：`a2245883eb2b46a998f041feb3ad0ed3f6cf7c60`，只读核对。
- 远程短检查：Go 1.26.7、Buf 1.60.0、sqlc 1.31.1、固定 PostgreSQL 镜像 amd64 均通过。
- 重任务位置：ubuntu；尚无本地回退。

## 测试接口

Goal 已明确授权的接口：VPC 用例及 gRPC、真实 PostgreSQL 持久适配、实际 kc adapter 对受控 API server、真实服务进程与持久恢复。数据库结构/权限/租户谓词的直接负向检查是 Goal 指定的数据库约束验证，不代替用例行为测试。

实施采用逐条行为的 red/green 验证；领域输入规则先行，随后完成原子受理和查询，再串接外部执行与恢复。

## 当前证据

业务实现及完整代码门禁已经通过。下文区分受控环境证据与尚未进行的真实数据面验收。

## 实施细节

- 业务与 Proto 已接通 Create/Get/List/Delete/GetOperation；operation 使用具体 VPC 的闭合持久引用，RPC 以 resource_type/resource_id 表达目标。字段、幂等、租户和状态规则由 biz 实现。
- 六张 VPC 租户表由同一 owner migration 建立，运行角色使用 sqlc/pgx 事务，不启用 RLS。受理、lease/epoch、Provider pending marker 和条件完成构成 T1–T4。
- 正常组合根装配真实 kc dynamic client；namespace/产品映射、ownership labels、UID、实际 VPC spec 和条件 DELETE 位于 data。没有运行时 fake 技术开关。
- 持续观察、未知结果、删除确认、墓碑清理和失败恢复均由 Network worker 负责；没有 Core/Gateway/中央任务兜底。
- GET/LIST/GetOperation 纯读；worker 生命周期、数据库/schema/角色检查贡献 readiness。Provider 当前可达性单独呈现。新增稳定 reason_message、worker 日志和有界 labels 的指标。
- 正常进程只使用 runtime 凭据；`-migrate` 显式读取 migration DSN 与 runtime role。测试容器内每个测试使用独立 owner/runtime 角色，避免并行测试之间权限变化干扰。

## 已发现并修复的问题

1. `SET ROLE` 提权没有被原始 readiness 检查发现：真实 PG 负向测试先失败，补充角色可达性检查，并验证运行角色不能建表、临时建表或 ALTER。
2. Protobuf JSON Duration 不接受 `100ms`：实际进程启动失败后，修正配置和进程 fixture 为 `0.1s` 等秒格式；保持类型化配置流程。
3. 已知归属冲突仍短暂显示 available：真实 adapter 测试先失败，修改为立即 degraded，UID 不被替换也不误删。
4. failed 创建经过持续观察丢失原因：测试先失败，观察时保留历史失败 operation 的原因；不重开 operation。
5. 测试固定 Base64 签名样例触发 Gitleaks：改为执行时生成随机测试密钥，没有加检测豁免。重启/副本 fixture 共享同一测试密钥。
6. 双进程测试以 0.15s 预算连续请求默认限流的 client-go，导致 DELETE 一直重试：使用 1s 单次请求/3s lease 的可执行 fixture，保留故障点和原断言；没有增加生产 API QPS 或跳过条件。

所有编译、生成、完整测试、数据库与扫描器构建在 ubuntu 完成。原仓库仅进行读取、编辑、格式/范围和轻量 Git 历史扫描；没有本地重任务回退。原有两个 Git 提交保留，工作区设计输入未丢弃。

## 原始记录索引

[run-ledger.json](NET-01/run-ledger.json) 列出各轮实际命令、主机、目录、源码清单与 archive 摘要和 SSH 返回码。完整源码归档、逐文件权限/摘要与命令日志保留在 `.work/net01-runs/<run>/`；生成物先进入 returned 目录，核对本地文件仍等于发送时摘要后才回填。

早期 `20260909T101915Z-adbc3842` 在远端执行前 SSH 断开，核对 command.exit/shell.pid 和源码目录后仅执行原 run.sh 一次，结果为 pass；不能把 SSH 255 当成业务检查失败。`20260909T102310Z-a4aaa74c` 为 SSH ControlPath 过长，已改用工作区固定短路径。其余编译红灯、fixture/门禁错误均保留各自记录，不改写为成功。

## 完整门禁结果

2026-09-09 在 ubuntu 的 `net01-20260909T112705Z-a21b8620` 独立目录执行：

```bash
make verify && make integration && make race && make tenant-mutations && make audit
```

整体退出码 **0**。该次 93 文件源码清单 SHA-256 为 `c25bcdcb60a1f7942c6d1f497bd88369a303d6858ebeb1514649b19d2a586d49`。最终运行源码、配置、迁移、生成物、测试和检查脚本已逐文件与该快照核对相同；此后仅整理交付文档/证据及重新生成对应 SBOM。最终包含这些交付材料的完整文件清单、权限、摘要与初始输入差异保存在 最终源码清单（历史定位：`../../../.work/net01-final/manifest.json`），不将清单文件自身作为其输入。

- [完整命令输出](NET-01/verification.txt)：生成无漂移、verify、真实 PG/进程、全包 race、三个 tenant 查询变异、audit 全部 pass。
- [原仓库历史扫描](NET-01/local-history-secrets.txt)：本地轻量扫描原有 **2 commits**，无泄露；远程 audit 扫描包含全部未提交源文件的 **1 个临时快照提交**，无泄露，两者范围不混同。
- [远程工具补齐记录](NET-01/remote-tools.txt)：用户目录安装 jq 1.7.1、rg 14.1.0、govulncheck 1.7.0、cyclonedx-gomod 1.12.0、Gitleaks 8.30.1；无 sudo、无本地编译回退。
- govulncheck 检查 60 modules 与 Go 1.26.7，数据库时点 `2026-09-02T19:12:04Z`，无已发现漏洞。
- SBOM 记录 **59 个非 main 组件**，许可证据完整；冻结 upstream notice SHA-256 保持 `3b44929bdd575a570c42165d4fd4eac57a5055745d2ecf3f121aa6a084cf9230`。本仓库尚未选择项目 LICENSE 是既有状态，不修改 notice 或编造项目授权；本包没有发布。

PostgreSQL 镜像固定为 `docker.io/library/postgres@sha256:4ef4dbc939d61acea57712655ddb4b4ab27419c913f94cca0cd57cb3ea3c2280`。测试以 `network_test_<UUID>` 数据库、`network_owner_<UUID>` / `network_runtime_<UUID>` 角色运行，实际 DDL 被运行角色以 SQLSTATE 42501 拒绝，跨租户 FK 以 23503 拒绝。fixture 使用 owner，业务只使用 runtime。

## 故障和恢复轨迹

| 场景 | 实际证据与结果 |
|---|---|
| T1 原子回滚 | owner 注入最后一段 history INSERT 故障；用例受理失败后六张业务表均无半份记录，相同键可再次正确受理 |
| 幂等和租户 | 12 个同键并发请求仅一个 VPC/operation/receipt/history；跨租户同键独立；重放不覆盖最初未验证 Actor；删除后返回最初受理快照 |
| T1 后进程退出 | 实际服务在首个 Provider GET 处被 kill；同一数据库/Provider 保留，重启完成原 VPC 与 operation，创建次数由 0 变为 1 |
| Provider 成功而 T4 未完成 | HTTP server 已保存 VPC、尚未返回响应时 kill；重启观察原 UID，创建次数始终 1 |
| 多进程与 lease | 两个实际服务进程共享 PG 争抢；独立 lease 测试验证单调 epoch，过期 epoch 的开始 mutation 和完成回写均被拒绝 |
| 外部迟到 POST | HTTP 请求在超时后仍执行；另一 worker 先看到缺失，但保持 blocked，不发第二个 POST；迟到对象出现后以原身份 available |
| 未知删除与墓碑 | DELETE 已在 HTTP server 生效但响应未达进程时 kill；重启确认缺失并完成原 delete operation；再次重启清理受控的同 UID 迟到对象，保留已成功 operation 的完成时间 |
| 归属/UID/子资源 | 错误归属、同名换 UID、status 子资源和未进入 status 的真实 Subnet 引用均不触发危险 DELETE |
| 纯查询和观测 | 停 worker 后反复查询不改 version/history、不调用 Provider；stale 可见；恢复 worker 才持久推进 degraded，原创建 operation 保持 succeeded |
| 生命周期 | 数据库健康失败与恢复改变 readiness；worker panic 使 readiness 不健康并退出；实际二进制收到信号后关闭监听器 |

详细身份轨迹另见 `20260909T112456Z-f30122f3` 日志；两种故障分别使用五个独立服务进程（其中两个同时运行）连接原有数据库/Provider。最终全量 integration 和 race 又覆盖相同场景。所有 Provider server 都是受控 HTTP 服务，不是假 runtime 后端，也不是真实 Kubernetes/kc 的证明。

## 验收映射

| 验收项 | 本包结果 | 剩余范围 |
|---|---|---|
| V-01 | pass | 后续每个代码包继续运行门禁 |
| V-02 / V-03 | VPC 六表、角色、无 RLS、FK、读取与变异部分 pass | Subnet/Attachment 及其扩展关系尚未实现 |
| V-04 | VPC 幂等部分 pass | 后续 Subnet/Attachment 重放 |
| V-06 | 独立 Network 进程、无调用方继续执行部分 pass | 真实集群禁用 Core 网络权限与 Gateway 停机验证 |
| V-07 / V-08 | 真实 PG、实际服务进程和受控 HTTP 故障/竞争部分 pass | 真实 Kubernetes/kc 外部执行保证 |
| V-09 | VPC 纯查询、stale、恢复观察部分 pass | Subnet/Attachment stale 准入和真实实例接入 |
| V-11 | VPC adapter、拒绝/失联/未知/归属/删除部分 pass | Subnet 和真实依赖故障验收 |
| V-05 / V-10 | not_verified | NET-02 / NET-03 |
| V-12 / V-13 / V-14 / V-15 | not_verified | kind/容器、Gateway/Console、VM、IAM 分别后续实施 |

## 已知限制与 kc 对接记录

- `subnet_count=0` 是 NET-01 的范围事实；没有 Subnet/Attachment 表和业务 API，不声称具备后续父子锁定和真实子资源统计。
- REST 的 JSON/HTTP 适配及公开身份接入属于后续包；RPC 的显式 UUID tenant 不是可信认证上下文。
- 当前已知 UID 消失会 degraded，不自动以新 UID 重建。未知创建请求持续观察/blocked；无法确认是否发送的 pending marker 不按超时清除。产品 CIDR、namespace、绑定标识首片不可迁移。
- **kc 请求记录，未发送外部团队**：固定 `a2245883eb2b46a998f041feb3ad0ed3f6cf7c60` 的普通 Kubernetes POST 没有请求执行结果查询接口。最小场景是 pending marker 提交后、POST 是否到达不明，GET 暂时 404（见迟到 POST 和 lease 恢复测试）；预期获得可持久查询的创建执行身份/结果，以安全区分“确定未执行”与“迟到中”。当前保留 blocked；不改 kc、不强制取消、不过早成功或删除。
- **后续 kc 验收请求，未发送外部团队**：用该版本的真实 API/OVN 验证 Ready/Initialized/Valid 与 observedGeneration 确实对应成功应用；并发外部 Subnet 引用与条件 VPC 删除不会留下错误清理。受控测试能核验发出的 UID/resourceVersion/Orphan 条件，不能建立跨对象原子性或真实 finalizer 保证。
- 测试的同 UID 墓碑迟到对象是故障模型，不宣称 Kubernetes 会正常复用 UID。真实同名新 UID 只报告冲突，不误删。

测试结束已核对：任务标签下残留 integration 容器 **0**，无未结束的验证命令。数据库、角色随独占容器清理，Go 测试清理其进程/临时凭据；远程源码快照、日志、工具和镜像缓存保留用于复核。只处理本任务资源。原仓库无提交、推送、PR 或部署；NET-02 未开始。


交付文档检查通过：21 份 Markdown、118 个本地链接/锚点、35 处源码行号引用；检查包含新增和未提交文件。最终工作区范围与空白检查、SBOM 同步日志保存在 `.work/net01-final/`；该目录是可再生成的交付快照附件，不是另一个当前规格或状态入口。最终 SBOM 按完成后的文档重新生成，原有代码门禁的逐文件证据不因此改写。
