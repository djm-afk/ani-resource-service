# ani-01 untagged 配置与 IP 实测

2026-11-09 引用维护：本记录保留原时点的实施与验收结论。标为“历史定位”或“仓外定位”的原路径在当前 checkout 无法解析，仅保留追溯信息，证据可用性为 not_verified；不表示归档已恢复或旧规则仍适用。处理范围见[文档修复记录](../../res-doc-fix-20261109.md)。

2026-09-15 01:02–01:05 UTC。用户明确要求使用 untagged 配置 ani-01 ens35 并继续测试，取代此前来宾侧 tagged 102 的要求；逻辑上游仍为 VLAN 102、172.16.102.0/24、网关 172.16.102.1。

后续修正说明：本文保留诊断时点。`managedDevices` 是 kc 接管及创建二层网络的正常前提，不能将“已接管”本身判为环境阻塞；下文所述缺口属于当时 Network 的设备登记实现。用户随后授权修正，现由[设备登记修正记录](../device-registration-20260915/README.md)承接，方案以[Public 规格 4.1](../../../../specs/vpc-snat.md#41-underlay-的网卡发现与二层网络)为准。

## 实际配置及结果

ens35 已从属于 OVS，`br-ens35` 是对应的内部接口。两个 OVS Port 的 tag/trunks/vlan_mode 均为空，桥已有 NORMAL 转发规则。本次没有添加 VLAN 子接口，也没有给受管物理成员口直接配置 IP。

在固定 ani-01 名称、节点管理集群 UID、ens35 MAC 和原接口/路由状态后，先发送三次针对 `.200` 的地址冲突探测，未见冲突回应；这不构成 DHCP/IPAM 保留。随后临时给 `br-ens35` 加 `172.16.102.200/24` 并启用其主机接口。测试仅使用该连接路由，未修改默认路由、netplan、OVS 配置、kc 配置或防火墙。

| 检查 | 结果 | 证据边界 |
|---|---|---|
| ani-01 → 网关 172.16.102.1 | `pass`，3 发 3 收 | ens35 实抓 3 个请求与 3 个回复，均无 VLAN 标签；不是只有 ARP |
| ani-01 → 172.16.102.30 ping | `fail`，3 发 0 收 | 邻居状态 FAILED，不能据此判定整个 untagged 网络不通 |
| ani-01 → 172.16.102.30:22 | `fail`，No route to host | 邻居解析失败，未建立 TCP |
| 本机 → 172.16.102.30:22 | `fail`，SSH 连接超时 | 与先前正常 VM 快照不同；用户随后确认 VM 已关机 |
| 随后同源 ARP 复核 | 网关 untagged `pass`；网关 tagged 与 .30 无回复 | `.30` 在两种标签方式下均无回复，正常对照目标的当前可用性已变化 |
| Public EIP/SNAT、三类 LB 的 U09 产品流量 | `not_verified` | 本次是宿主接口的临时网络诊断，未生成产品验收结论 |

实际脚本见 [untagged-ip-test.py](untagged-ip-test.py)，完整命令和配置前后状态见[第二次执行](untagged-ip-result-02.json)。[第一次执行](untagged-ip-result.json)因脚本从 addr JSON 读取仅存在于 link JSON 的字段而退出；未配置任何地址，现场一致。修正读取来源后再执行，保留该次失败，不将它写成成功。

[追加 ARP 对照](arp-after-ip-test.json)与[本机 SSH 复查](working-vm-followup.json)分别保留当前目标不可达证据。用户随后明确回复“已关机，先保留测试结果”。因此停止对 `.30` 的重测，记录对照目标已关机；不把这组 ping/TCP 失败用于判定 untagged 链路故障，也不把尚未完成的 TCP 验证改写成通过。电源状态来源是用户确认，未读取 vCenter。

## 清理及具体差异

脚本 finally 已删除 `.200/24`，移除随地址产生的连接路由，将内部接口恢复 DOWN，并恢复 IPv6 地址生成模式；没有添加默认网关。ens35 的原 MAC、UP 状态、OVS master、空地址保持，OVS Port 配置逐字段一致。之后[独立读取](product-prerequisites-and-cleanup.json)确认 br-ens35 无地址、无邻居、路由恢复原值。

完整快照比较的 `restored=false` 必须保留：br-ens35 在此次启用后报告的 qdisc 从 `noop` 变为 `noqueue`，其余记录字段一致。未执行 tc 写操作，也没有为消除此内核报告差异重建共享桥。不能把这个结果写成所有字段完全恢复；业务相关地址、路由、管理链路和 OVS 配置的清理检查通过。

本次没有持久化一个宿主 `.200` 地址。后续 Public EIP 应由产品池分配，测试地址也不因此被预留。

## 产品 API 的独立接入缺口

[当前平台对象](product-prerequisites-and-cleanup.json)显示：kcn-config 已由安装器配置 `managedDevices=ens35`，没有 Network 的 device-adoption annotation；没有 VlanNetwork、EIPGateway 或 Network node-facts ConfigMap。kcn-config UID `dcca6e9e-db3f-4ebe-8b27-8898e3a2939e`、resourceVersion `39661` 保持。

现有源码存在以下限制，属于源码核查结论，未伪称已经调用一次实际失败的 RPC：

- FilterNodeInterfaces（历史定位：`../../../../../internal/biz/egress.go`）将 OVS master、OVSManaged、KCManaged 分别标为占用/已管理，不能用于新的空闲设备接管。
- observeDevice / ensureDevice（历史定位：`../../../../../internal/data/kc_node_facts.go`）拒绝已有 managedDevices 成员但缺少本资源 binding marker 的情况。
- CreateVlanNetwork 需要可用的产品 NetworkDevice 记录；直接写一个 `vlanID: 0` 的历史手工 CR 不能替代产品前置流程。

因此，网关链路已经可用，完整 Public API 测试仍缺少“使用安装器预先管理的物理设备”的正式入口，以及实际节点事实采集部署。删除现有桥再接管、手写 adoption marker、伪造节点事实或直接插入产品数据库，都不能作为本次验收步骤。

最小后续工作应先明确已受管设备的登记/引用语义：固定 kcn-config UID、节点 UID/MAC、真实 OVS mapping 与已有 VLAN 占用；已有配置不归测试实例所有，退役不能释放共享物理设备；处理不同 Network 实例的并发占用。再实现受控入口和对应门禁，部署真实只读采集器，通过产品 API 创建 `vlan_id=0` 的实验网络/池。它是设备接入语义的增量，未在本次临时主机配置中静默加入。

原 Goal 的业务进程和 API 转发未因本次诊断重新启动；保留此前数据库、池及 fixture。未重复运行源码未变化的受控重门禁。完整 U09 未完成。
