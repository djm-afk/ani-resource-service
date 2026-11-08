# 集群运维一期实施与验收

输入：ANI-Res-ClusterOps-Plan-v1.1.zip，SHA-256 `45f8853076fa58b4b606dbfcc30e9e570daeeb6eb263c0b6100317a45433749e`。设计核对基线 `ab189b99a5bcfc482a0208102ee3328c9a60ae27`。本计划仅安排工作，实际实现、命令与 pass/fail/not_verified 只记录在[执行状态](../execution/status.md#cluster-operations-phase1)。

正式规则见[业务规范](../specs/cluster-operations.md)，字段与原生方法语义见[调用合同](../contracts/cluster-operations-v1.md)，平台委托见[治理登记](../specs/governance-calls.md#clusterops-platform)。任务包两份输入按职责接入这些权威落点，不保留第二套现行规范。

## 工作包

| 工作项 | 主要落点 | 完成条件 |
|---|---|---|
| CO-01 合同与管理端接入 | proto、service骨架、clusterops_identity、现有 unary/stream allowlist、首个请求预算middleware、日志脱敏 | 新包生成正确，管理员委托可达，tenant/mixed/未知方法拒绝；本域30s，旧方法预算和授权保持 |
| CO-02 配置与 Namespace 规则 | clusterops composition、两个登记 reader、NamespacePolicy、client Warning配置 | 不启动Owner流程也可查询登记；Network-only、Sandbox-only、closed/NULL UID 均保护；登记2s子预算，暂时故障可恢复，读故障不变可写 |
| CO-03 原生读取 | catalog、List/Get、Overview、Secret投影、Events使用说明 | 范围固定，原生分页/selector正确，数据不落库 |
| CO-04 原生写与工作负载动作 | 严格manifest解析、CRUD、scale、restart、CRD删除预检 | 租户NS禁写，重复键拒绝，整数无损，目标/UID/RV校验，原生错误和异步结果语义正确 |
| CO-05 管理员Pod日志 | 平台流授权、typed logs、帧/取消/预算、单一发送者 | 可读租户NS日志，长行无截断，取消释放，慢客户端发送有界；handler与发送者均能退出 |
| CO-06 必要验证与交付 | 对应测试、仓内合同/状态、简短交付说明 | 通过必要检查，失败与未验证项如实记录，无越界修改 |

顺序是实施依赖，不是逐卡申请审批。任务范围内连续推进；出现具体代码冲突再解决，不为补齐模板而新增机制。

## A01–A19

| 编号 | 检查 | 通过标准 |
|---|---|---|
| A01 | 管理端身份 | 合法 Gov platform 接受；tenant、mixed scope、API Key平台actor、未知方法拒绝 |
| A02 | 装配 | full/governance注册；admin健康端口无业务路由；vpc-read/compute-only不扩权 |
| A03 | Namespace 两来源 | 仅Network登记、仅Sandbox登记、两边登记的NS都只读；Compute/Network cluster配置不匹配不得混用登记 |
| A04 | 登记状态 | Sandbox closed、尚无UID、Network NULL UID仍只读；不触发生命周期写 |
| A05 | 写入口覆盖 | Create/Update/Delete/Scale/Restart、Namespace本体、dry_run均受同一guard |
| A06 | 读取与原业务 | 租户NS的List/Get/Events/Logs可用；现有Owner创建/管理不经过新guard |
| A07 | 数据来源故障与恢复 | 两来源共享2s子预算；父请求仍有效时列表可查并给只读原因，写入拒绝；父请求取消正确透传；依赖恢复后下一次查询自然恢复，不永久冻结source、不默认为空登记 |
| A08 | 范围、寻址和解析 | 未配置集群、路径注入、子资源、正文换NS/类型/名称、多对象YAML/JSON被拒绝；YAML和JSON的嵌套重复键在转换前拒绝；有效int64如9007199254740993往返不失真 |
| A09 | 原生列表 | limit/continue、精确名称、Pod node/phase筛选和410处理正确；无伪造total |
| A10 | 并发条件 | 旧RV更新冲突；旧UID不能删同名新对象；无强制重试覆盖 |
| A11 | 状态保留 | 无status子资源的测试CR更新配置后status仍保留；不允许通用status写入口 |
| A12 | ConfigMap、Secret与日志出口 | CM保存不重启Pod；Secret列表/默认详情/保存响应无值；显式详情可编辑；含sentinel的manifest、原始错误及HTTP Warning均不进入普通日志；其他Owner Warning配置不变 |
| A13 | 扩缩容 | Deployment/StatefulSet可设0；其他类型明确拒绝；不修改HPA或业务配额 |
| A14 | 重启 | 仅改模板注解；paused/OnDelete/partition>0拒绝；不删Pod、不改更新策略 |
| A15 | RBAC | SA/Role/Binding与ClusterRole/Binding走原生CRUD；roleRef错误不自动重建 |
| A16 | CRD | 编辑成功刷新发现；tenantNS中有实例时拒绝删除；分页/查询失败不漏检；cluster scope按本期规则处理 |
| A17 | 日志 | READY/DATA/END/ERROR顺序、长行完整、非平台流拒绝；窗口正常END与发送超时非OK区分；客户端不接收但连接保持时，handler按期退出、reader关闭、槽归还，底层stream关闭后唯一发送者退出；不并发Send、不无限等终止帧 |
| A18 | 工程边界 | 生成文件一致，分层检查通过，无新业务表/controller/审计证明体系，无兄弟仓库修改 |
| A19 | 真实gRPC入口预算 | 本域请求耗时超过原默认1s且低于30s时成功；旧域仍按原c.Timeout，调用方更短deadline优先；共享构造函数的其他模式不丢旧预算；概览/CRD多页不重置总预算，未完成预检或ctx已到期不再发Delete |

## 验证与退出

受控测试覆盖纯 Go policy/usecase、fake/httptest 原生传输、TLS fixture 的真实 gRPC listener；数据库使用既有 scripts/integration 的独立 PostgreSQL。真实 API server 验证使用当次授权的 10.10.1.66 与任务临时 Namespace/RBAC/CRD；租户保护只用受控登记 fixture。不得修改真实租户登记、真实服务或 admission。

实际入口预算需要超过原 1 秒的请求成功、旧方法原预算及客户端更短期限；慢客户端保持连接并停止 Recv，验证 handler、reader、槽和底层 stream 关闭后的唯一发送者回收。日志内部规则测试用短预算，不做十分钟长跑。重复键、int64 与 Warning/manifest/错误脱敏只作有限反例验证。

沿用 pinned Buf/sqlc 和 make verify，分层与 AGENTS 不绕过；只修复本次引入的失败。完成规定能力、必要检查、清理和可审查代码后交付分支/提交/PR，不自动合并或正式部署，不追加外部仓库接入、长期压测或审计工程。
