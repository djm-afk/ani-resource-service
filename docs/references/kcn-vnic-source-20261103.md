# KCN VNic 源码合同核对（2026-11-03）

本记录只说明固定 KCN 源码快照提供的合同，覆盖租户 namespace 内预建 VNic、分配地址、Pod 引用接网、运行端点退出及用户显式删除的 KCN 行为。产品范围以[手动 VNIC 管理方案](../plans/kcn-vnic-management-20261103.md)为准：默认实例仍自动分配，手动 VNIC 不随实例删除。未修改 KCN，未执行构建、测试、集群/API 操作，不代表已完成 Resource 接入或真实网络验收。

## 来源与新鲜度

- 源码目录：`/home/chabking/workspace/kc-networking`；已读该仓 `AGENTS.md`。
- 本地 `HEAD = main = origin/main = origin/HEAD`：`6132623baabee50f17e9e179c0f5e9c1f380db04`，提交时间 `2026-10-30T10:26:30+08:00`，标题 `Merge branch 'fix_unable_del_nic' into 'main'`。核对开始时 `git status --short` 为空。
- 2026-11-03 执行只读远端核对：`timeout 20s env GIT_SSH_COMMAND='ssh -o BatchMode=yes -o ConnectTimeout=8 -o ConnectionAttempts=1' git ls-remote --symref origin HEAD refs/heads/main refs/heads/master`。
- 结果 `FAIL`，exit 128：`Connection closed by 192.168.20.144 port 22` / `Could not read from remote repository`。因此只能确认**最新本地快照**，不能称已确认远端最新或部署版本；没有 pull/fetch/checkout。
- 下文所有 `[Sxx]` 都固定到上述 commit，标签后的路径与行号是可复核主源；GitLab 链接仅作源码定位，不表示远端页面已访问成功。

## 1. VNic 对象、引用与分配合同

| 项目 | 固定快照的行为 |
|---|---|
| GVK / scope | `networking.kubercloud.com/v1`, `kind: VNic`，plural `vnics`，Namespaced。[S01] |
| spec | 必填 `subnet`；`type` 默认 `VETH`，枚举 `UNMANAGED/VETH/INTERNAL`；可选 `macAddress/securityGroups/isolationGroup/qos`。`type/subnet/macAddress` 不可改；空 MAC 由 controller 生成。[S02][S03] |
| subnet 引用 | 可写 `namespace/name`；只写 name 时解析为 VNic 自己的 namespace。跨 namespace 仍需 Subnet 的 `allowedNamespaces` 允许；默认只允许相同 namespace。[S03][S04] |
| Pod 引用 | `networking.kubercloud.com/vnic: <name>` 按 **Pod namespace + 原样 name** 读取，不解析 `namespace/name`。优先级 `vnic > vnicip > subnet`；显式 VNic 不存在时返回错误，不自动补建该名称。[S05][S06] |
| VM 的类型 | 类型注释写“VM uses UNMANAGED”，但经过 Pod CNI 的入口明确拒绝非 `VETH`。KubeVirt launcher Pod 使用本 annotation 路径时必须按 Pod CNI 的 `VETH` 合同；`UNMANAGED` 是另一种直接 Nic 接口路径，不能从注释推导可用于 launcher Pod。[S02][S06][S07] |
| 地址分配开关 | **没有 `spec.createDefaultIP`**。`metadata.annotations[networking.kubercloud.com/allocate_ip_with_dhcp]` 存在才触发默认 VNicIP 创建；值精确为字符串 `"true"` 才启用该 VNicIP 的 DHCP，`"false"` 仍创建并分配地址。[S02][S08] |
| 自动子对象 | 按 Subnet 地址族，IPv4 VNicIP 名为 `<vnic-name>`，IPv6 名为 `<vnic-name>-v6`；同 namespace、同 subnet、`spec.vNic=<vnic-name>`，ownerReference 指向 VNic。完成后写 `allocated_vnicip`（逗号分隔）和 `ip_count` annotation。[S08] |
| 地址呈现 | VNicIP 的空 `spec.ipAddress` 经 IPAM 分配后被写回；VNic 汇总为 `status.primaryIP/ips`、`status.primaryIPv6/ipsV6`。因此正常预分配判断只需读取该 VNic，不必每轮展开子对象图。[S09][S10] |
| status | 有 `conditions`、`appliedSecurityGroups`、`gateway/nodeName/nic/dstNic`、上述地址字段；**没有 VNic 顶层 `status.observedGeneration`**。[S02] |

预建 VNic 的 `metadata.annotations` 与 `metadata.labels` 应分别含分配 annotation 和真实 Resource 管理标签。当前 `bindPodVNic` 对这两个 map 直接赋值，未初始化 nil map；通用 `SetMetadata` 对 VNic 也不会补空 map。这是源代码风险，样例保留正常管理标签，并不要求 Resource 写入 KCN 的内部 node/pod 标签。[S11][S12]

## 2. 创建顺序与最小预分配判断

可以先创建 VNic、分配 IP，再创建 Pod。VNic controller 在没有绑定 Nic 的情况下仍会配置逻辑交换机端口并设置 `Ready=True`，不依赖 Pod 存在。反过来，**Ready=True 也可能尚无 IP**；仅等 Ready 不足以表示预分配完成。[S08][S10][S13]

针对一期 IPv4 VETH，建议的最小读取判断为：

1. 确认当前 VNic 的 namespace/name/UID 与本次 Resource 意图一致、未进入删除，`spec.type/subnet` 匹配。
2. `Valid=True`、`Initialized=True`、`Ready=True`；`spec.macAddress` 非空；`status.primaryIP` 非空，且在 `status.ips` 中有匹配地址。
3. 若明确要求双栈，对 IPv6 同样检查 `primaryIPv6/ipsV6`；不能把仅 IPv4 就绪当成双栈完成。
4. 此阶段**不要求** `status.nic/nodeName` 或 Pod 已存在；它们是运行时绑定结果。上述规则是依据实际字段制定的 Resource 判断建议，并非 KCN 另有一个分配状态机。[S02][S08][S10][S13]

KCN 通用 condition helper 注释掉了 `ObservedGeneration: obj.GetGeneration()`，且相同状态/原因/消息时直接返回；不能等待 VNic condition 的 `observedGeneration == metadata.generation`。SG 的顶层 `status.observedGeneration` 是单独写入的字段，二者不可混用。[S14][S15]

Pod CNI 自己的入口只等 `Valid` 和 `len(ips)+len(ipsV6) >= ip_count`（缺省期望 1），并不等 VNic Ready；先由 Resource 等待预分配可减少这一异步窗口。CNI 从实际 Pod annotation 取名；daemon 的 VNic cache 按 node label 过滤，node label 由 Pod controller 建立。不要把 annotation 已写入当成 CNI 已接通。[S06][S11][S16]

## 3. 最小 YAML 与 VM annotation 边界

下面仅为合同示例，未执行。示例 namespace/Subnet/工作负载镜像需要换为真实输入；标签是示例管理标签，不定义 Resource 新的持久化合同。

```yaml
apiVersion: networking.kubercloud.com/v1
kind: VNic
metadata:
  namespace: tenant-a
  name: res-vnic-example-001
  labels:
    app.kubernetes.io/managed-by: ani-resource-service
  annotations:
    networking.kubercloud.com/allocate_ip_with_dhcp: "false"
spec:
  type: VETH
  subnet: tenant-a/subnet-a
  # securityGroups: [sg-a]  # 如使用，SG 必须在 tenant-a 中
---
apiVersion: v1
kind: Pod
metadata:
  namespace: tenant-a
  name: workload-example
  annotations:
    networking.kubercloud.com/vnic: res-vnic-example-001
spec:
  containers:
    - name: app
      image: registry.example.invalid/workload:replace-me
```

在 KCN 已作为默认 CNI 的前提下，这个 annotation 对应 `eth0`；若现场由 Multus 指定默认网络，还必须保留实际 NAD/default-network 选择。KCN 支持一个默认网络及多个附加网络；附加接口用 `<ifName>.networking.kubercloud.com/vnic`，例如 `net1.networking.kubercloud.com/vnic`，并需对应 Multus 附件配置。不能把多个 VNic 名称写进一个默认 annotation。[S17]

KCN 仓库此快照没有 KubeVirt VM/VMI controller 或其 annotation 传播实现；源码只证明读取**最终 Pod metadata.annotations**。Resource 在 VM template/VMI 何处放置 annotation、KubeVirt 是否传播到 launcher Pod，须由 Resource/KubeVirt 集成源码与实际 Pod 单独确认。KCN 侧不存在“只在 VM metadata 写一次就一定可用”的保证。[S05][S06]

`allocate_ip_with_dhcp: "false"` 只使自动子对象 `VNicIP.spec.dhcp=false`，不关闭地址分配。KCN CNI 把地址/MAC/网关作为 CNI 结果返回；这与 KubeVirt bridge binding 向 guest 提供 DHCP 是不同层面的行为。KCN 源码不能单独证明 guest DHCP、guest 路由或真实数据面成功。[S08][S18]

## 4. Pod 删除、重接、迁移与最终删除

- **预建 VNic**：`getSpecificPodVNic` 只读取；`bindPodVNic` 只改 Pod 对应 annotation/node label，不设置或覆盖 Pod owner。Pod 删除后 `releasePodVNic` 清除其关联信息，并保留普通命名 VNic。其自动 VNicIP owner 是 VNic，因此保留 VNic 即可保留地址分配，适合作为停机/重建后的稳定身份。[S05][S08][S11][S19]
- **Pod 自动创建 VNic**：`autoCreateVNic` 使用随机 `auto-...-vnic-xxxxx` 名并添加 Pod ownerReference；释放时会按名称正则识别自动对象、在无使用 node label 时显式删除。Resource 自有稳定名称不得模仿该保留模式。没有 `deleteWithPod` spec 字段；生命周期由创建路径、owner 与名称识别共同决定。[S19][S20][S21]
- **重接**：新 Pod 引用同一预建 VNic，KCN 新建运行时 Nic 并更新 `status.nic/nodeName`；稳定 VNic、MAC、IP 与运行时 Nic 身份不同。已获得 PodIP 的 Pod 不再走常规网络重设，不能靠运行中修改 vnic annotation 当作热插拔接口。[S06][S10][S19][S22]
- **迁移边界**：VNic controller 保留当前主 Nic，并把其余候选放进 `status.dstNic`（实际逗号列表，类型注释说单个目标）。原 Nic 不在列表时才提升其余候选；daemon 只对选中主 Nic执行实际 attach。此为源级候选/切换机制，不足以宣称 KubeVirt 热迁移通过。全仓未找到 `allowLiveMigration`，CRD/VNicSpec 也没有此字段，不能把它设计成当前 KCN 开关。[S02][S07][S23]

以下仅适用于用户已显式请求删除独立 VNIC，不由实例删除自动触发：

1. 先删除/停止实例实际运行时 Pod，使 CNI DEL 与 KCN Nic 控制器正常释放绑定；不要先等待 Resource 自有 VNic 自动消失。
2. 对该 VNic 确认 `status.nic` 与 `status.dstNic` 已空、该 Pod 的 `pod.networking.kubercloud.com/<pod-name>` 关联已移除，再按记录的 namespace/name/UID 发起显式 VNic 删除。`status.nic` 非空时 KCN finalizer 会阻止删除。[S19][S23][S24]
3. 定点确认该 VNic 以及创建时记录的自动 VNicIP 名称均 NotFound；自动 VNicIP 通过 VNic owner 触发 GC，VNicIP 自己在 finalizer 中释放 IPAM 后才移除 finalizer。这是两个异步完成点，VNic NotFound 本身不证明地址已释放。不需要把所有网络资源扫描一遍。[S08][S24][S25]

源码还暴露一个删除风险：`handleDeleteVNic` 调用 `deleteVNicRules` 时忽略返回错误，随后直接清 finalizer；因此即使 VNic CR 已消失，也不能从这个信号严格证明 OVN 逻辑端口清理成功。应作为 KCN 外部缺陷报告，不能在 Resource 内伪造 Provider 清理通过；本轮未修改或复现。[S24][S26]

2026-11-03 用户明确：由用户与 KCN 沟通解决方法，Res 不为此增加防御逻辑。以上是源码问题记录，不是要求 Res 增加 OVN 检查、额外释放门禁或补偿分支；正常管理删除继续消费 KCN 的资源状态。

CNI DEL 当前实现通过 containerID 派生 peerName 过滤其他 sandbox 的 Nic 删除，并支持 Pod 已不在 API 的情况。这里只确认此防护源码存在，不把旧版本删除故障或此快照代码当成实测结论。[S27]

## 5. SG 与 get-or-create 的操作边界

预建 VNic 的安全组以 `spec.securityGroups` 为权威，引用同 namespace 的 SG 名。Pod 的 `networking.kubercloud.com/security_groups` 只同步名称符合自动 VNic 正则的默认 VNic，不会替代 Resource 对预建 VNic 的 SG 设置。[S02][S28]

VNic 的 `Ready` 不检查所需 SG 是否全部应用；`status.appliedSecurityGroups` 仅在 SG 的 `status.appliedVNics` 包含本 VNic 时记录其已观察 generation。需要 SG 生效时，定点检查这一 map，并对所需 SG 核对 Ready/顶层 observedGeneration/成员；不要把“VNic Ready + spec 里有 SG 名”误认作策略已生效。当前 SG 成员索引依赖 `status.ips`（IPv4），纯 IPv6 SG 覆盖不能由此源码宣称。[S10][S15][S29]

该快照没有名为 `GetOrCreateVNic` 的 Provider API；Pod 显式引用是 GET-only。默认 VNicIP 的创建虽然接受 AlreadyExists，却不在该分支核对现有对象的 subnet、vNic 或 owner，然后就写分配完成 annotation。Resource 的 get-or-create 必须属于自己的幂等操作：使用已记录的稳定名称，已有对象须校验本方身份与不可变意图；冲突不接管，禁止仅凭同名成功或盲目重复 CREATE 认定绑定正确。[S05][S08]

首次正常分配无需常态读取 VNicIP 全图；只在同名冲突、分配异常恢复或最终删除阶段，对已知子对象名称读取 UID/owner/绑定进行定点确认。分配 marker 非空后 KCN 不再自动补建子对象，Subnet 从单栈改双栈也不会据此自动补齐；删除子 VNicIP 后不能期望原 annotation 自愈重建。[S08]

## 主源索引

所有路径均属 KCN commit `6132623baabee50f17e9e179c0f5e9c1f380db04`。

| 引用 | 精确路径与行号 |
|---|---|
| [S01] | `config/crd/bases/networking.kubercloud.com_vnics.yaml:7–44` |
| [S02] | `api/networking/v1/vnic_types.go:24–129`；spec 全字段与 status 全字段 |
| [S03] | `internal/controller/networking/vnic_controller.go:200–270`；MAC 与 subnet 验证 |
| [S04] | `internal/controller/handlers/allowed_namespaces.go:13–40` |
| [S05] | `internal/controller/core/pod_controller.go:445–493`；引用优先级与同 namespace GET |
| [S06] | `internal/daemon/cniserver/handler.go:177–291`；实际 Pod 注解、VETH 限制与 IP 等待 |
| [S07] | `internal/daemon/networking/nic_controller.go:151–224`；UNMANAGED 与 active Nic |
| [S08] | `internal/controller/networking/vnic_controller.go:274–336`；默认 VNicIP 创建 |
| [S09] | `internal/controller/networking/vnicip_controller.go:186–209`；地址写回 |
| [S10] | `internal/controller/networking/vnic_controller.go:691–720`；地址、绑定、已应用 SG status |
| [S11] | `internal/controller/core/pod_controller.go:546–605`；绑定元信息写入 |
| [S12] | `internal/ctrlutils/metadata.go:75–108,139–144`；map 合并与 VNic 空元信息 |
| [S13] | `internal/controller/networking/vnic_controller.go:534–609`；OVN 应用与 Ready |
| [S14] | `api/networking/v1/conditions.go:123–144`；未写 observedGeneration |
| [S15] | `internal/controller/networking/securitygroup_controller.go:236–273`；应用成员与 generation |
| [S16] | `cmd/daemon/daemon.go:183–213`；按 node label 缓存 VNic |
| [S17] | `internal/controller/core/pod_controller.go:280–357,382–442`；默认及附加网络 |
| [S18] | `internal/daemon/cniserver/handler.go:79–119`；CNI 地址返回 |
| [S19] | `internal/controller/core/pod_controller.go:591–639`；普通 VNic 解绑与自动对象删除 |
| [S20] | `internal/controller/core/pod_controller.go:505–534`；Pod owner 只在自动创建路径 |
| [S21] | `internal/ctrlutils/naming.go:61–79`；自动名称正则 |
| [S22] | `internal/controller/core/pod_controller.go:123–140,642–660`；PodIP 后不重设网络 |
| [S23] | `internal/controller/networking/vnic_controller.go:358–418`；主/候选 Nic 选择 |
| [S24] | `internal/controller/networking/vnic_controller.go:102–142`；删除阻塞与 finalizer |
| [S25] | `internal/controller/networking/vnicip_controller.go:89–129`；异步 IPAM 释放 |
| [S26] | `internal/controller/networking/vnic_controller.go:612–620`；OVN 删除错误返回 |
| [S27] | `internal/daemon/cniserver/handler.go:143–174,329–362`；CNI DEL sandbox 过滤 |
| [S28] | `internal/controller/core/pod_controller.go:162–192`；Pod SG 更新范围 |
| [S29] | `internal/controller/handlers/indexer.go:211–218`；SG 成员的 IPv4 条件 |

[S01]: https://gitlab.changqingyun.cn/kubercloud/sdn/kc-networking/-/blob/6132623baabee50f17e9e179c0f5e9c1f380db04/config/crd/bases/networking.kubercloud.com_vnics.yaml#L7-44
[S02]: https://gitlab.changqingyun.cn/kubercloud/sdn/kc-networking/-/blob/6132623baabee50f17e9e179c0f5e9c1f380db04/api/networking/v1/vnic_types.go#L24-129
[S03]: https://gitlab.changqingyun.cn/kubercloud/sdn/kc-networking/-/blob/6132623baabee50f17e9e179c0f5e9c1f380db04/internal/controller/networking/vnic_controller.go#L200-270
[S04]: https://gitlab.changqingyun.cn/kubercloud/sdn/kc-networking/-/blob/6132623baabee50f17e9e179c0f5e9c1f380db04/internal/controller/handlers/allowed_namespaces.go#L13-40
[S05]: https://gitlab.changqingyun.cn/kubercloud/sdn/kc-networking/-/blob/6132623baabee50f17e9e179c0f5e9c1f380db04/internal/controller/core/pod_controller.go#L445-493
[S06]: https://gitlab.changqingyun.cn/kubercloud/sdn/kc-networking/-/blob/6132623baabee50f17e9e179c0f5e9c1f380db04/internal/daemon/cniserver/handler.go#L177-291
[S07]: https://gitlab.changqingyun.cn/kubercloud/sdn/kc-networking/-/blob/6132623baabee50f17e9e179c0f5e9c1f380db04/internal/daemon/networking/nic_controller.go#L151-224
[S08]: https://gitlab.changqingyun.cn/kubercloud/sdn/kc-networking/-/blob/6132623baabee50f17e9e179c0f5e9c1f380db04/internal/controller/networking/vnic_controller.go#L274-336
[S09]: https://gitlab.changqingyun.cn/kubercloud/sdn/kc-networking/-/blob/6132623baabee50f17e9e179c0f5e9c1f380db04/internal/controller/networking/vnicip_controller.go#L186-209
[S10]: https://gitlab.changqingyun.cn/kubercloud/sdn/kc-networking/-/blob/6132623baabee50f17e9e179c0f5e9c1f380db04/internal/controller/networking/vnic_controller.go#L691-720
[S11]: https://gitlab.changqingyun.cn/kubercloud/sdn/kc-networking/-/blob/6132623baabee50f17e9e179c0f5e9c1f380db04/internal/controller/core/pod_controller.go#L546-605
[S12]: https://gitlab.changqingyun.cn/kubercloud/sdn/kc-networking/-/blob/6132623baabee50f17e9e179c0f5e9c1f380db04/internal/ctrlutils/metadata.go#L75-144
[S13]: https://gitlab.changqingyun.cn/kubercloud/sdn/kc-networking/-/blob/6132623baabee50f17e9e179c0f5e9c1f380db04/internal/controller/networking/vnic_controller.go#L534-609
[S14]: https://gitlab.changqingyun.cn/kubercloud/sdn/kc-networking/-/blob/6132623baabee50f17e9e179c0f5e9c1f380db04/api/networking/v1/conditions.go#L123-144
[S15]: https://gitlab.changqingyun.cn/kubercloud/sdn/kc-networking/-/blob/6132623baabee50f17e9e179c0f5e9c1f380db04/internal/controller/networking/securitygroup_controller.go#L236-273
[S16]: https://gitlab.changqingyun.cn/kubercloud/sdn/kc-networking/-/blob/6132623baabee50f17e9e179c0f5e9c1f380db04/cmd/daemon/daemon.go#L183-213
[S17]: https://gitlab.changqingyun.cn/kubercloud/sdn/kc-networking/-/blob/6132623baabee50f17e9e179c0f5e9c1f380db04/internal/controller/core/pod_controller.go#L280-442
[S18]: https://gitlab.changqingyun.cn/kubercloud/sdn/kc-networking/-/blob/6132623baabee50f17e9e179c0f5e9c1f380db04/internal/daemon/cniserver/handler.go#L79-119
[S19]: https://gitlab.changqingyun.cn/kubercloud/sdn/kc-networking/-/blob/6132623baabee50f17e9e179c0f5e9c1f380db04/internal/controller/core/pod_controller.go#L591-639
[S20]: https://gitlab.changqingyun.cn/kubercloud/sdn/kc-networking/-/blob/6132623baabee50f17e9e179c0f5e9c1f380db04/internal/controller/core/pod_controller.go#L505-534
[S21]: https://gitlab.changqingyun.cn/kubercloud/sdn/kc-networking/-/blob/6132623baabee50f17e9e179c0f5e9c1f380db04/internal/ctrlutils/naming.go#L61-79
[S22]: https://gitlab.changqingyun.cn/kubercloud/sdn/kc-networking/-/blob/6132623baabee50f17e9e179c0f5e9c1f380db04/internal/controller/core/pod_controller.go#L123-660
[S23]: https://gitlab.changqingyun.cn/kubercloud/sdn/kc-networking/-/blob/6132623baabee50f17e9e179c0f5e9c1f380db04/internal/controller/networking/vnic_controller.go#L358-418
[S24]: https://gitlab.changqingyun.cn/kubercloud/sdn/kc-networking/-/blob/6132623baabee50f17e9e179c0f5e9c1f380db04/internal/controller/networking/vnic_controller.go#L102-142
[S25]: https://gitlab.changqingyun.cn/kubercloud/sdn/kc-networking/-/blob/6132623baabee50f17e9e179c0f5e9c1f380db04/internal/controller/networking/vnicip_controller.go#L89-129
[S26]: https://gitlab.changqingyun.cn/kubercloud/sdn/kc-networking/-/blob/6132623baabee50f17e9e179c0f5e9c1f380db04/internal/controller/networking/vnic_controller.go#L612-620
[S27]: https://gitlab.changqingyun.cn/kubercloud/sdn/kc-networking/-/blob/6132623baabee50f17e9e179c0f5e9c1f380db04/internal/daemon/cniserver/handler.go#L143-362
[S28]: https://gitlab.changqingyun.cn/kubercloud/sdn/kc-networking/-/blob/6132623baabee50f17e9e179c0f5e9c1f380db04/internal/controller/core/pod_controller.go#L162-192
[S29]: https://gitlab.changqingyun.cn/kubercloud/sdn/kc-networking/-/blob/6132623baabee50f17e9e179c0f5e9c1f380db04/internal/controller/handlers/indexer.go#L211-218
