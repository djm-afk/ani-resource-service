# 2026-09-17 同事排查结论复核

2026-11-09 引用维护：本记录保留原时点的实施与验收结论。标为“历史定位”或“仓外定位”的原路径在当前 checkout 无法解析，仅保留追溯信息，证据可用性为 not_verified；不表示归档已恢复或旧规则仍适用。处理范围见[文档修复记录](../../res-doc-fix-20261109.md)。

本轮仅通过 ani-test-1 SSH 只读获取当前集群对象、对照安装的 CRD 和源码；未创建、修改或删除集群资源，未发送数据面流量。

| 结论 | 复核结果 |
|---|---|
| 复用现有 Public Subnet | 按用户要求保留。当前 `kcn-system/pubdebug-20260917-public` UID `39619c25-703e-4db2-a35a-d44ee2acb780`，Ready=True；用户报告可用及 CNI 后续版本修复，修复版本/提交未独立核实。Ready 不替代流量测试。 |
| 健康检查端口遗漏 | 当前 public-lb-policy generation=2 已含 `active.overrides.port: 8080`，Accepted=True。安装 CRD 明确省略时使用 endpoint 的 serving port，因此不能认定省略 override 就没有健康检查端口，根因 `not_verified`。 |
| Backend 未创建 | 当前手工 namespace `pubdebug-20260917` 中确实没有 Backend；public-lb HTTPRoute 的 ResolvedRefs=False/BackendNotFound，分别引用 backend-a、backend-b。当前对应 Pod IP 为 10.233.1.2、10.233.1.3，服务端口 8080。该现场引用缺失确认 `pass`，转发仍未复测。 |

原始快照：[Public Subnet](public-subnet.json)、[当前 LB 与相关对象](manual-lb.json)、[安装的健康检查 schema](policy-crd.json)。每份保存命令、时间与退出码。

历史产品 API 场景不能与当前手工场景混同：[历史 Provider 快照](../device-registration-20260915/public-private-configured-provider.json)存在四个 Backend，端点为 10.233.1.2:8080 和 10.233.1.3:8080；适配器源码（历史定位：`../../../../../internal/data/kc_load_balancer.go`）生成 Backend endpoint 及 Route 引用，健康检查使用默认 endpoint 端口。因此本轮证据不支持“Network 创建 LB 一直未创建 Backend”的判断。

手册原本将 Backend 的生成、创建拆为独立步骤；现补充缺失检查和停止条件，撤去平台创建及删除指令，保留当前池及其 EIPGateway/VlanNetwork。详见[更新手册](../../../../runbooks/public-forwarding-manual-20260917/README.md)。未改 Go 代码或重新运行完整门禁。

后续先按当前 Pod IP 补齐两个 Backend，再确认 Route ResolvedRefs=True 后复验入口。Public SNAT 当前尚未创建；Backend/健康检查问题不能解释独立 SNAT 出站失败。三项历史 Public fail 不在本轮改为 pass。

## 后续授权：补齐并复测

用户随后授权执行。04:17:59 UTC 在 ani-test-1 通过 kubectl create 创建 backend-a（UID 0ddeaffc-d506-473a-b30a-c77236036ede）和 backend-b（UID 852586dc-a121-4267-83db-9973b9b3b39d），分别指向当场确认的 10.233.1.2:8080、10.233.1.3:8080。两者 Accepted=True，Route ResolvedRefs=True。健康检查已有的显式 8080 保持，Public Subnet UID 保持，无删除重建。

最初 public-a 的 5 次入口请求返回 503，Envoy 报 cluster_not_found/端点配置初始获取超时；client 直连两个后端均 200。收敛后 public-a 入口请求返回 200，随后 4 次确认全部 200，覆盖 backend-a/backend-b 的身份及 nonce。public-b 的 4 次初测及 1 次复测仍超时，不将整项标为通过，也不把初期 503 当作持续故障。

证据：[收敛后请求和对象/日志](backend-fix-retest.txt)、[四次身份确认](backend-fix-confirm.txt)。本轮仅测试当前纯 Public LB，未创建双入口 LB 或 Public SNAT，未替代 Network API 验收。现场保留。

## 10:37 UTC 再次复测

用户报告恢复后，交替从 public-a/public-b 各发送 6 次请求至现有 EIP 172.16.102.194:8080。12/12 均 HTTP 200，各客户端均命中 backend-a 和 backend-b，响应 nonce 对应请求。public-b 的历史超时本轮未复现，当前手工纯 Public LB 有限流量检查 pass。Public Subnet UID 保持；本轮未更改集群配置，无法据此确定两轮之间的恢复原因。双入口及 Public SNAT 未测，不替代产品 API 验收。证据：[复测输出](public-b-recovery-retest.txt)。

## 其他类型复测（2026-09-17 10:39–10:43 UTC）

用户授权继续测试且禁止删除已有 Public Subnet。检查三节点 CPU requests 为 35–43%，内存 requests 为 31–37%，可容纳标准两副本；因此保留 public-lb，使用手册 07-dual-lb.yaml 创建 dual-lb/Gateway、HTTPRoute、BackendTrafficPolicy，使用既有 dual-eip .195、VIP 10.233.0.201 和已 Accepted 的 Backend。未删除任何现有对象，未改变共享配置。双入口策略沿用 endpoint 服务端口默认健康检查行为。

| 用例 | 结果 | 证据与限制 |
|---|---|---|
| 双入口私网 VIP | pass | 两轮 8/8 HTTP 200，覆盖两个后端 |
| 双入口 EIP / public-b | pass | 两轮 8/8 HTTP 200，覆盖两个后端 |
| 双入口 EIP / public-a | fail | 两轮 8/8 请求超时；第一轮无 Public SNAT，第二轮启用 Public SNAT，方向差异仍存在；根因未确定 |
| Public SNAT 实际出站 | pass | 创建前 client 超时；按 08-public-snat.yaml 创建后，client、backend-a、backend-b 均 HTTP 200 |
| SNAT 出口源地址 | pass | public-b 接收端 ESTABLISHED peer 为 172.16.102.196，与 snat-eip 一致 |
| SNAT 停用/恢复 | pass | disable=true 后 phase=Disabled，新请求超时；恢复 false 后新请求 HTTP 200 |
| 原纯 Public LB 回归 | pass | 两个客户端各 1 次 HTTP 200 |

证据：[双入口第一轮](dual-before-snat.txt)、[SNAT 创建前](snat-before-create.txt)、[启用状态与三个来源请求](snat-enabled.txt)、[真实出口地址](snat-peer-live.json)、[停用](snat-disabled.txt)、[恢复及双入口第二轮](dual-final-snat-reenabled.txt)、[最终对象](other-cases-final-objects.txt)、[原 LB 回归与池 UID](public-after-other-cases.txt)。

最终保留 public-lb、dual-lb、已启用的 public-snat 和所有测试资源；Public Subnet UID 仍为 39619c25-703e-4db2-a35a-d44ee2acb780，未删除或重建其关联基础网络。上述是当前手工 CR 数据面测试，不是产品 API 重验；不将历史 U09 矩阵整体改为通过。出站目的地为实验 Public 网络接收端，不声称真实互联网验收。

## 10:48 UTC 双入口 EIP 再复测

用户询问当前是否恢复。未改配置，交替从 public-a/public-b 各请求 6 次双入口 EIP .195。public-a 首次超时、随后 5 次均 200，覆盖两个后端；public-b 6/6 为 200（本轮均 backend-a）。说明当前已有成功流量，但本轮仍出现超时，不能认定已稳定恢复。恢复原因未确定，Public Subnet UID 保持。证据：[原始输出](dual-recovery-retest.txt)。

## 用户结论：功能按正常处理，残留超时交接 kcn

用户明确决定当前功能按正常处理，偶发超时留给 kcn 团队后续解决。本任务停止追加该超时的循环复测。该决定属于功能交接处置，不覆盖上面的原始失败次数，也不证明 kcn 根因已经确认。

实际处理顺序：

1. 固定复用现有 Public Subnet pubdebug-20260917-public 及其关联基础网络，不删除重建。用户报告 CNI 的删除重建缺陷在后续版本修复，本任务未升级插件或核实修复提交。
2. 复用已有 Public EIP：纯 Public .194、双入口 .195、SNAT .196，分别绑定对应对象。
3. 补齐当前手工场景缺失的 backend-a/backend-b CR，指向实际 Pod 10.233.1.2:8080、10.233.1.3:8080，HTTPRoute 的 BackendNotFound 消失。配置下发收敛后能转发到两个后端。
4. 同事已将纯 Public 策略的 healthCheck.active.overrides.port 设为 8080。本任务保留该设置；安装 CRD 的默认行为为 endpoint 服务端口，新建双入口策略未设置 override 也有成功流量。因此不将“未显式写健康检查端口”作为已确认根因。
5. 创建双入口 Gateway/Route/Policy，验证 VIP 与 EIP；创建 Public SNAT，验证三来源 HTTP、接收端真实源地址 .196、停用失败与重新启用成功。全部实验资源保留。

没有通过本轮修改 Network Go 代码、升级 kcn/Envoy 或更改物理网络。历史产品 API 场景已存在 Backend，本轮修复的是当前手工场景的缺失，不能追溯归因为原产品代码从未生成 Backend。

当前结论：纯 Public LB、双入口 LB、Public SNAT 功能按用户决定可用；双入口残留偶发超时委托 kcn 后续排查。产品 API 重验、完整 Goal 与独立观察稳定性问题不因本次手工交接自动完成。
