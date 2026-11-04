# 租户安全组

> 2026-11-04 网络合同更新：本文件中的网络状态、Ready/时效准入、Attachment 释放、SG 应用及 LB 生成物验收条款以[网络状态消费规格 v3](cr-observation.md)为准；方法/字段变更见[实施方案](../plans/network-management-simplification.md)。实际验证只见执行状态。

安全组由 Resource Network 拥有产品身份、规则、引用及持久操作；Governance
负责受信租户、权限和 HTTP 适配，实例 owner 负责模板及当前运行对象。
KCN v1 负责实际规则下发。来源为本地 kc-networking `6132623` 的 usage/API；
不修改 KCN，也不通过兼容逻辑掩盖 Provider 缺陷。

## 规则

安全组属于租户，落在已有租户/集群 namespace 映射，不强制属于 VPC。
产品仅接 IPv4；协议为 tcp、udp、icmp、ipip、gre、all，action 为 allow/drop。
同向 priority 数值越大越优先；用户范围 2–4000。每组由服务端原子创建并保留
`sgr_default_ingress_drop`：ingress、all、drop、priority 1，无地址/端口限制。
默认行可查询，但不可改删。出方向不添加默认 drop。
`allowSameGroupTraffic` 显式为 false，避免组内互信越过用户入方向规则。

每方向最多 100 条（含默认规则）；单条最多 10 个规范 IPv4 CIDR、10 个不重叠
目的端口闭区间。仅 tcp/udp 配端口。多个安全组合用时，同向同优先级的相反
action 被拒绝；已有绑定的规则修改同样检查该组合约束。

## 主链与当前配置

创建容器（CPU/GPU）或 VM 的正式请求携带产品 `security_group_ids`。
业务校验及解析 Provider 名后保存初始创建意图；worker 在首次下发前登记引用。
当前选择独立于不可变 CREATE 保存。更换使用独立的版本化、幂等 operation，
不会靠 Stop/Restart 代替网络配置变更。

容器写 Notebook、StatefulSet 模板和当前 owner Pod；VM 写 VM 模板、VMI 与
当前 virt-launcher。Pod annotation `networking.kubercloud.com/security_groups`
使用逗号分隔的 KCN 名，清空使用显式空字符串。所有原生变更以 UID/RV 条件写。
创建和重建必须使用持久当前配置；用户不可直接注入 Provider 注解绕过产品 ID。

EIP 使用 `spec.securityGroups`。CreateEIP 的 `security_group_ids` 与地址、初始配置及引用原子受理，初始配置 version 1；不传安全组时，首次 GET 绑定配置返回 version 1 的空配置；
PUT 使用该配置版本，而非地址资源版本。未绑定 EIP 可以保存配置，但保持
待应用状态；具备绑定目标、VPC、节点及网关后才能报告实际应用。

## 应用与释放

SG CR 的 Valid/Ready、observedGeneration 追平及目标应用成员共同构成
Provider 应用证据；它们不替代真实流量测试。EIP 自身 appliedSecurityGroups
仅证明引用存在，不作 ACL 下发证据。

更换前先保留新旧组引用，核实新配置后再清理旧引用。consumer revision 来自
持久 worker 资源版本；旧 Sync/Confirm、不同意图和退休后的重放不能改动新引用。
停止态无当前 Pod 只能报告 configured，不能伪报 applied；历史 Attachment
中仍存在的 VNic UID 也阻止释放旧引用。

任何未释放引用（含未绑定 EIP、在途更换）都阻止删除安全组；Provider 仍有
应用成员或 finalizer 时等待正常清理。删除以原对象 UID 的真实消失完成。

PG 保存意图、不可变幂等回执、操作和身份；持久 Stepper 推进，GET/List 无副作用。
旧操作被新版本明确替代时保留原身份并标记 OPERATION_SUPERSEDED，不能永久悬空。
实际执行结果只记录在 [执行状态](../execution/status.md)。

## 部署权限

Network 身份需要 [Network RBAC](../../deployments/egress/network-rbac.yaml) 中
SecurityGroup CRUD/Patch 和 EIP Patch；Instance/VM 运行身份另需要
[消费者安全组变更权限](../../deployments/egress/security-group-consumers-rbac.yaml)
以修改 Notebook、VM/VMI 和当前 Pod 的注解。按部署实际 ServiceAccount 绑定，
沿用已有 owner 读写和诊断权限；提交清单不表示已在共享服务部署应用。
