---
status: superseded
date: 2026-09-09
---

# 首片暂缓服务间身份验证，先完成网络业务与测试链

> 历史决定。身份验证延期已由[Governance → Resource 调用合同](../specs/governance-calls.md)及受信 mTLS 入口承接；默认/full、governance、vpc-read 不提供明文回退。下文保留 2026-09 的原范围，不作为当前认证或权限豁免。

本决定来自 2026-09-09 用户讨论，明确将 Network 的服务间身份验证推迟到 IAM 就绪后接入。当前只落实设计文档，尚未实现网络业务、测试接线或 IAM 集成；IAM 的候选身份协议不成为当前 VPC/Subnet 业务开发和测试环境链路的硬阻断。

本期测试链允许显式传入非空 Tenant ID，作为一次操作的目标租户及资源归属上下文。Network 仍检查输入、同租户资源关系和持久化隔离；测试上下文不宣称已通过身份认证或权限验证，不能仅因包装成 context 或从调用方收到就称为可信身份。具体承载方式由[首片规格](../specs/vpc-subnet.md)规定。

资源租户、Actor 和 Direct Caller 分别表达。未验证身份时不伪造已认证 Principal、默认系统用户或 IAM 授权决定，也不把请求关联标识或幂等标识解释成身份凭据。这里暂缓的是服务间身份验证，不是取消租户数据边界；当前业务与隔离测试可以执行，其结果不作为真实鉴权已完成的证据。

IAM 就绪后，在 Network 入站接线处接入届时明确的服务身份与调用上下文契约，并单独验证真实调用方和租户授权链路。此阶段不在 Network 提前复制 IAM 的 Principal、Membership、Role、Token 或 Workload Grant 实现，也不预建一套临时服务间鉴权协议。

相关词汇见[领域词汇表](../../CONTEXT.md)，数据边界见[ADR-0002](0002-use-tenant-owned-data-without-rls.md)，实施状态见[执行状态](../execution/status.md)。

## 2026-09-22 只读接入补充

本批按用户授权增加隔离的 [Governance VPC 只读入口](../specs/governance-vpc-read.md)：验证 Governance mTLS 服务身份后，信任它委派的租户与 user/access-key actor，仅允许 GetVPC。原 full 入口及完整 IAM 接入仍不属于本批完成声明。
