# VPC / Public 出站 / LB 统一方案交付

2026-11-09 引用维护：本记录保留原时点的实施与验收结论。标为“历史定位”或“仓外定位”的原路径在当前 checkout 无法解析，仅保留追溯信息，证据可用性为 not_verified；不表示归档已恢复或旧规则仍适用。处理范围见[文档修复记录](res-doc-fix-20261109.md)。

2026-09-14。用户在确认两条规则后要求新方案及可执行任务：一个 EIP 独占一个目标（SNAT 或 LB）；SNAT 按内网/公网用途限制，并补齐 VPC 基础内网创建、恢复、删除和就绪判断。当前用户要求是方案，未授权本轮实施或部署。

## 文档成果

- [统一方案](../../specs/vpc-connectivity-lb.md)：平台、VPC、Public EIP/SNAT、LB、并发占用、观察、旧资源补齐和验收合同。
- [NET-U00—U12 任务卡](../../plans/vpc-connectivity-lb.md)：每项给出输入、依赖、修改范围、工作和退出条件，含可直接交给执行者的模板。
- [ADR-0005](../../adr/0005-separate-vpc-connectivity-and-exclusive-eip-bindings.md)：仅标记用户明确确认的规则；API、第一批 HTTP 范围及具体模型为工程提案。
- [领域词汇](../../../CONTEXT.md)：区分基础内网连接、两类地址池、EIP 绑定和 LB。
- 旧 VPC/Subnet、Public SNAT 规格及计划增加局部替代链接；历史验证不重写。

## 固定来源与只读核对

独立文档工作树 `/home/chabking/workspace/.worktrees/network-vpc-lb-plan-20260914`，分支 `codex/vpc-lb-plan-20260914`，起点 `d8835a22d905e358b7f60756d3113baa97d7c762`。原 main 的在途平台文档改动未动；原 EIP/SNAT 实现和 LB 实验工作树均未修改。ANI 只读参考 HEAD 为 `50aa9fe2099b7ff4c276f883939a8d26c9d9eff8`，后续实际接线需重新固定输入。

选定源文件和证据的 SHA-256 见 [inputs.json](VPC-LB-PLAN-20260914/inputs.json)。主要代码事实：

| 依据 | 已确认现状 | 方案落点 |
|---|---|---|
| AcceptVPC（历史定位：`../../../internal/data/postgres.go`） | 首次受理没有基础EIP/SNAT意图 | 稳定子资源及固定池版本随首次事务持久化 |
| VPC worker（历史定位：`../../../internal/biz/worker.go`）、SNAT准入（历史定位：`../../../internal/data/egress.go`） | VPC CR Ready即可available；公开Snat又依赖VPC available | 区分ProviderReady与聚合Ready，内部SNAT避免循环等待 |
| [旧迁移](../../../migrations/0005_vpc_egress.sql) | 每VPC只有一个SNAT位置，EIP来自Public池 | scope/managed_by/purpose、双用途位置及统一claim |
| EIP查询（历史定位：`../../../internal/data/queries/egress.sql`） | binding_id是SNAT ID，只从SNAT表判占用 | 新target字段；旧ID只投影SNAT，LB占用仍reserved/bound |
| Public池渲染（历史定位：`../../../internal/data/kc_egress.go`） | 固定Public类型/出口前提 | 基础Intranet平台能力单独定义，不能绕过公网验证 |
| EIP观察（历史定位：`../../../internal/data/kc_egress.go`） | Service引用被当作冲突，未识别产品LB目标 | 验证合法生成Service归属，外来引用仍冲突 |
| VPC删除（历史定位：`../../../internal/data/worker.go`） | 任何未删除SNAT及未完成创建都会阻止删除 | 系统基础资源进入删除编排，支持失败创建终止 |

只读审查子任务独立核对上述基线，并对新方案复核。发现并补齐两处：从未发送Provider请求的计划步骤可直接取消，未知请求仍保留身份清理；旧EIP binding字段在LB情况下有明确投影及客户端开放门槛。未使用审查来扩展到kc修复、IAM或配额。

## 引用的实测范围

保存了先前独立 LB 实验的[六条业务请求](VPC-LB-PLAN-20260914/lb-traffic.json)与[30项检查](VPC-LB-PLAN-20260914/lb-verification.json)。二者是 2026-09-14 既有手工 CR 场景的 pass；本轮没有重跑集群测试。

这些证据覆盖 kind 中三种 LB 的 HTTP 入口，不能证明新方案已实现，不能抵消 Public SNAT 分支的历史外部Provider阻塞，也不等于真实互联网、Underlay、HA或生产验收。输入只作为时点证据，不构成跨工作树构建依赖。

## 本轮检查与实际边界

文档本地链接/锚点、任务编号和依赖、JSON证据语法、git diff空白检查均 pass，详见 [document-checks.json](VPC-LB-PLAN-20260914/document-checks.json)。没有业务代码改动，因此未运行编译或make verify；后续实施任务仍需按卡片运行必要门禁。

本轮未执行数据库迁移、资源补齐、集群写入、服务发布、Git提交或推送。当前状态以[执行状态](../status.md)为准，所有实施任务尚未开始。
