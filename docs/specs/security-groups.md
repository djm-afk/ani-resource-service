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
configured 状态；实际 applied 仅按约定原生输出登记，不加 VPC/节点/网关健康合取。

## 应用与释放

安全组自身使用原 CR Ready。目标应用结果直接读取精确 EIP/VNic 的 appliedSecurityGroups，使用 Res 租户/namespace/名称映射为产品 ID；不通过 SG 反向成员、完整历史 VNic、额外 Valid/代次或 ACL 检查拼出第二套应用证明。特定配置请求的原生回执仍按自己的合同处理，不使旧稳定状态因代次而失效。

配置 PATCH 成功仅记 configured 并通知现有任务，不能把 desired IDs 复制成 applied IDs。无当前 VNic 时保留 configured，消费者变化后由现有任务读取实际输出。原生状态或 applied 不是流量测试的替代品。

更换和删除保留真实产品引用、原对象身份、配置版本、consumer revision 与迟到回执保护；仍被当前资源或在途变更引用时不能删除。旧事件不能覆盖新选择或复活 released/deleted 任务。删除仅正常结束自己的原 SG 声明，不等待反向成员闭包，不处理第三方 finalizer。

PG 保存意图、幂等、操作和身份，现有 Stepper 推进，Get/List 无副作用；已完成 operation 保持历史结果。实际实现、验证与部署只见[执行状态](../execution/status.md)。

## 部署权限

Network 身份需要 [Network RBAC](../../deployments/egress/network-rbac.yaml) 中
SecurityGroup CRUD/Patch 和 EIP Patch；Instance/VM 运行身份另需要
[消费者安全组变更权限](../../deployments/egress/security-group-consumers-rbac.yaml)
以修改 Notebook、VM/VMI 和当前 Pod 的注解。按部署实际 ServiceAccount 绑定，
沿用已有 owner 读写和诊断权限；提交清单不表示已在共享服务部署应用。
