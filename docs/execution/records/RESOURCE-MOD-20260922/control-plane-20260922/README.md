# 真实控制面续测：2026-09-22

2026-11-09 引用维护：本记录保留原时点的实施与验收结论。标为“历史定位”或“仓外定位”的原路径在当前 checkout 无法解析，仅保留追溯信息，证据可用性为 not_verified；不表示归档已恢复或旧规则仍适用。处理范围见[文档修复记录](../../res-doc-fix-20261109.md)。

用户授权继续测试并如实记录结果。本轮不要求 Public 流量成功后才开始可独立执行的控制面测试，
但没有取消原计划的必需数据面和发布条件。唯一当前状态在 [status](../../../status.md)。

## 输入与边界

执行主机 Fedora，隔离 run 为
`/home/chabking/workspace/ani-resource-service-runs/rsmod-20260921T1640Z/control-20260922T0400Z`。
候选完整源码 `9e491faaf621b3394635f5aa22fd58efd20177b1`，原版
`66f787bd30134141726c596612501a83cf75bdb7`；候选主程序复用已通过安全修复门禁的相同运行源码产物，
构建版本文本仍是 `7b39e53-dirty`，不改写成此次文档提交。旧客户端来自原版，候选辅助 CLI 从 9e491fa 构建。
[源码 manifest](evidence/candidate-source-manifest.json)、[产物 SHA](evidence/binaries.sha256)、
[构建信息](evidence/candidate-build-info.txt)、[268 项运行源码与实际产物一致性](evidence/runtime-source-match.json)、[前次精确 head 门禁](prior-exact-head/final-assessment.json)关联输入。
Go 1.26.7-X:nodwarf5、GOWORK=off、任务缓存、共享重任务锁、CPU 200%/内存 2300M 保持。

写入前重新核对 ani-test-1→ani-01（172.16.101.10）、现有三节点集群，
cluster UID `be57b911-892c-4e75-aa9d-4a05d819c59e`，节点 Ready、权限及安装身份。
[preflight](evidence/preflight.json)、[installation](evidence/installation.json)为本轮实采，未直接沿用历史身份。
新建任务 DB `net_vpc_lb_02_c92201`，所有切换使用同库、同配置、同地址与同签名密钥。
配置 SHA `d9084a38e07aaff152cf08ea341cf02397b4f610744e71e5cd5f84a77cc5a617`；配置和凭据留在 Fedora 私有目录。

## 实测结果

| 检查 | 结果 | 证据与限制 |
|---|---|---|
| 原版创建 VPC、两个 Subnet、两个 Attachment/业务 Pod、基础 Intranet EIP/SNAT、private LB | pass | [baseline 就绪](evidence/ready-baseline.jsonl)；LB configured/fresh，基础连接 ready；不代表租户数据面通过 |
| Intranet 平台资格检查 | pass | 原有驱动实际执行 DNS/HTTP 检查，再通过产品 API 记录、启用、设置默认池；[执行回执](evidence/step-baseline-intranet-proof.json) |
| 候选同库接管 | pass | [21 个原对象](evidence/compare-baseline-candidate-takeover.json) UID/spec/labels/owner/FieldManager 完全相同；[旧回执/cursor/schema](compatibility/check-candidate-takeover.json)保持 |
| 候选正常操作 | pass | 新增 Subnet，LB 健康检查间隔由 5 改为 7，desired/applied 最终为 2/2；[最终检查](evidence/lb-check-candidate-applied.json)；[预期 CR 变化](evidence/normal-mutation-delta.json)单列 |
| 候选强杀后的在途任务恢复 | pass | 冻结本 run worker 后受理新 Subnet，确认原 operation 为 QUEUED，再 SIGKILL；重启后同一 operation 完成；[故障注入](compatibility/pending-crash.json)、[恢复检查](compatibility/check-candidate-recovery-complete.json) |
| 原版同库回退 | pass | [23 个对象保持](evidence/compare-candidate-restarted-rollback.json)；[原版回执/schema 检查](compatibility/check-rollback.json)、[LB 更新保留](evidence/lb-check-rollback.json)；原版成功删除候选创建的额外 Subnet |
| 候选再次启动 | pass | [回退删除后的 22 个对象保持](evidence/compare-rollback-after-delete-candidate-final.json)；[旧客户端最终检查](compatibility/check-candidate-final.json) |
| 产品 API 清理 | pass | [终态/占用核验](evidence/cleanup-database.json)、[Provider 对象为 0](checkpoints/products-cleaned/summary.json) |
| Public 前置拒绝的一致性 | pass（拒绝行为） | baseline/candidate/rollback 均为设备记录不存在、Public pool 列表为空、CreateEIP 返回 PUBLIC_EGRESS_NOT_READY；不等于 Public 创建成功 |
| Public SNAT、Public/双入口 LB 的创建、变更、接管、回退 | not_verified | 当前隔离库缺少合法平台产品记录及可用 Public 池，创建 EIP 已被拒绝，不能跳过前置伪造成功 |
| 本轮 Pod 隔离/通信、租户 Intranet、Public SNAT、三类 LB 六次数据面对照 | not_verified | 本轮按用户指示推进控制面，未补跑该矩阵；此前严格计数 fail、Public 流量 fail 原样保留 |

操作、请求、返回、退出码在 [product-driver](product-driver/state.json)、
[platform-driver](platform-driver/state.json)、[兼容检查](compatibility/freeze-baseline.json)与 evidence 下完整保留。
真实 Public 拒绝回执：[原版](evidence/public-precondition-baseline.json)、
[候选](evidence/public-precondition-candidate.json)、[回退原版](evidence/public-precondition-rollback.json)。
这里的“平台产品记录”指 Network 自己数据库里的设备/地址池记录及其 CR 身份绑定；不是额外平台，
也不能把已有 `kcn-system/public` 手工 CR 的 Ready 状态当作隔离库内产品记录。

接管和回退没有清库、恢复旧备份、重建既有 CR、改归属标签或更换资源 ID。
正常 LB 更新按既有语义生成新 Backend 并更新 HTTPRoute，属于显式业务操作的预期差异；
其余对象身份检查不忽略 UID/spec/owner 等字段。status/resourceVersion 是可变观察字段，单独保存而不要求字节不变。
没有改变产品 timeout、租约、epoch 或 worker 实现。

## 保留的失败与等待

[运行器事件](evidence/runner-events.json)区分初始失败和最终结果：
首次 VPC 创建前漏开 enable-new，以已有管理员 CLI 审核并回填本任务唯一 VPC；
LB 更新与故障恢复后的早期断言先于异步收敛，原失败保留，随后加强等待条件再检查实际结果；
故障后 transient unit 已卸载和保护检查 nullable namespace 的运行器错误均已修正，原脚本保留。
没有放宽产品断言、修改数据库状态或循环发送流量直到成功。

LB 清理首次 180 秒控制面等待未完成，记为该窗口 fail；后续只读检查发现其自然进入 DELETED，
再继续依赖清理。所有 pending 读和退出码保留；最终清理 pass 不抹掉初次等待失败。
本轮未证明无中断、持续观察稳定性或数据面恢复。

## 清理与恢复入口

产品资源全部终态：1 VPC、4 Subnet、1 LB、1 EIP、1 SNAT、1 Intranet pool、
1 基础连接 deleted，2 Attachment released；claim/VIP intent/Subnet ref/generated resource 未释放计数均为 0。
19 个任务 namespace/RBAC 等支持对象按创建 UID 删除，并复查 404；进程退出、PG 容器停止并删除：
[清理回执](evidence/cleanup-runtime.json)、[支持资源](evidence/support-cleanup.json)。
[17 个保护对象](evidence/protected-after.json) UID/spec/labels 均与本轮开始相同，三节点仍 Ready。
共享 `kcn-system/public`、原 gateway/VLAN、已有 EIP/探针与他人资源保留。
[三个临时代理/隧道端口](evidence/transport-cleanup.json)已关闭。

原版/候选产物、源码、任务缓存与私有备份留在上述 Fedora run。
最终备份 SHA 及路径见清理回执；备份只作恢复入口，本轮没有恢复旧快照来完成回退。
凭据、kubeconfig、数据库备份未进入仓库；导出前逐文件扫描实际凭据值，见 [导出 manifest](export-manifest.json)。
[脚本](scripts/driver.py)提供冻结驱动入口；已清理的 run 不得直接重放创建命令，下一次运行必须重新冻结环境、DB 和资源身份。

完整 R4/R5 仍未满足：保持草稿 PR、原默认分支和仓库名称，未进行正式目录改名或发布后 module 消费验证。
[9e491fa 两条 CI](prior-exact-head/final-ci-assessment.json)成功仅覆盖其精确提交，不代替尚缺的真实 Public 和数据面验收。

提交前 secret 扫描初次命中 22 项，逐项确认为公开产物 SHA、Go module checksum 或测试幂等键，见 [核对记录](evidence/secret-findings-review.json)。仅增加精确路径与完整值同时匹配的例外，默认 secret 规则保留；[18 个正反例](evidence/secret-fixtures.json)通过，改路径或改值仍被检测。运行源码和依赖没有新增修改。

Fedora 提交前 `make verify` [退出 0](evidence/verify.exit)，日志（历史定位：`evidence/verify.log`）保留；工作树 secret 扫描通过（历史定位：`evidence/working-tree-secrets.log`）。普通测试的 PG skip 不被当作本轮新增 PG 门禁；真实 PG/CI 证据仍分别对应此前已通过的提交，本轮真实同库操作独立记录。

最终归档增量以显式 `.gitleaks.toml` 再扫描[退出 0](evidence/final-records-secrets-explicit.exit)。仅扫描子目录而未指定配置的一次调用未加载根规则，重复报告原 22 项公开元数据；该调用记录仍留在 Fedora review/evidence。Git 属性仅对 11 个原始命令输出允许其捕获的行尾空白/尾部空行，以保留导出哈希，其他文件不豁免空白检查。
